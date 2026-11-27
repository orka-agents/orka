/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"fmt"

	"strings"

	fiber "github.com/gofiber/fiber/v3"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/llm"
)

type ProviderResolver struct {
	client client.Client
	config ChatConfig
}

type ResolveOpts struct {
	ProviderName               string
	ModelStr                   string
	Model                      string
	AgentRef                   string
	Namespace                  string
	RequireModel               bool
	RequireExplicitProvider    bool
	AuthorizeProviderReference func(ProviderResolutionInfo) error
	AuthorizeProviderUse       func(ProviderResolutionInfo, string) error
}

type ProviderResolutionInfo struct {
	Name      string
	Namespace string
	Type      string
}

func NewProviderResolver(c client.Client, config ChatConfig) *ProviderResolver {
	return &ProviderResolver{client: c, config: config}
}

func (r *ProviderResolver) Resolve(ctx context.Context, opts ResolveOpts) (llm.Provider, string, error) {
	provider, model, _, err := r.ResolveWithInfo(ctx, opts)
	return provider, model, err
}

func (r *ProviderResolver) ResolveWithInfo(ctx context.Context, opts ResolveOpts) (llm.Provider, string, ProviderResolutionInfo, error) {
	if opts.AgentRef != "" || opts.ProviderName != "" {
		return r.resolveFromExplicit(ctx, opts)
	}
	return r.resolveFromModelStr(ctx, opts)
}

func (r *ProviderResolver) resolveFromExplicit(ctx context.Context, opts ResolveOpts) (llm.Provider, string, ProviderResolutionInfo, error) {
	var model string
	var providerCRD *corev1alpha1.Provider
	var providerName string

	if opts.AgentRef != "" {
		agent := &corev1alpha1.Agent{}
		if err := r.client.Get(ctx, types.NamespacedName{Name: opts.AgentRef, Namespace: opts.Namespace}, agent); err != nil {
			return nil, "", ProviderResolutionInfo{}, fmt.Errorf("agent %q not found: %w", opts.AgentRef, err)
		}
		if agent.Spec.Model != nil && agent.Spec.Model.Name != "" {
			model = agent.Spec.Model.Name
		}
		if agent.Spec.ProviderRef != nil {
			providerName = agent.Spec.ProviderRef.Name
		}
	}

	if opts.ProviderName != "" {
		if providerName != "" && providerName != opts.ProviderName {
			return nil, "", ProviderResolutionInfo{}, fmt.Errorf("agent %q is bound to a different provider; omit the provider or choose an agent without a providerRef", opts.AgentRef)
		}
		providerName = opts.ProviderName
	}

	if providerName != "" {
		p, err := r.LookupProvider(ctx, providerName, opts.Namespace)
		if err != nil {
			if apierrors.IsNotFound(err) {
				if authErr := authorizeProviderReference(opts, ProviderResolutionInfo{Name: providerName, Namespace: opts.Namespace}); authErr != nil {
					return nil, "", ProviderResolutionInfo{}, authErr
				}
			}
			return nil, "", ProviderResolutionInfo{}, err
		}
		if err := authorizeProviderReference(opts, providerResolutionInfo(p)); err != nil {
			return nil, "", ProviderResolutionInfo{}, err
		}
		providerCRD = p
	}
	if providerCRD == nil && opts.RequireExplicitProvider {
		return nil, "", ProviderResolutionInfo{}, explicitProviderRequiredError()
	}

	if providerCRD == nil && r.config.Provider != "" {
		p, err := r.LookupProvider(ctx, r.config.Provider, opts.Namespace)
		if err != nil {
			return nil, "", ProviderResolutionInfo{}, err
		}
		providerCRD = p
	}

	if providerCRD == nil {
		p, err := r.LookupProvider(ctx, "default", opts.Namespace)
		if err != nil {
			fallback, fallbackErr := r.soleReadyProvider(ctx, opts.Namespace, "", opts.RequireExplicitProvider)
			if fallbackErr != nil {
				return nil, "", ProviderResolutionInfo{}, fallbackErr
			}
			p = fallback
		}
		providerCRD = p
	}

	if opts.Model != "" {
		model = opts.Model
	}
	if model == "" {
		model = providerCRD.Spec.DefaultModel
	}
	if model == "" {
		model = r.config.Model
	}
	providerInfo := providerResolutionInfo(providerCRD)
	if err := authorizeProviderUse(opts, providerInfo, model); err != nil {
		return nil, "", ProviderResolutionInfo{}, err
	}

	apiKey, err := r.ResolveAPIKey(ctx, providerCRD)
	if err != nil {
		return nil, "", ProviderResolutionInfo{}, err
	}

	provider, resolvedModel, err := r.buildProvider(providerCRD, apiKey, model)
	if err != nil {
		return nil, "", ProviderResolutionInfo{}, err
	}

	return provider, resolvedModel, providerInfo, nil
}

func (r *ProviderResolver) resolveFromModelStr(ctx context.Context, opts ResolveOpts) (llm.Provider, string, ProviderResolutionInfo, error) {
	var providerName, model string

	if idx := strings.Index(opts.ModelStr, "/"); idx > 0 {
		providerName = opts.ModelStr[:idx]
		model = opts.ModelStr[idx+1:]
	} else {
		model = opts.ModelStr
	}

	var providerCRD *corev1alpha1.Provider

	if providerName != "" {
		p, err := r.LookupProvider(ctx, providerName, opts.Namespace)
		if err != nil {
			if apierrors.IsNotFound(err) {
				if authErr := authorizeProviderReference(opts, ProviderResolutionInfo{Name: providerName, Namespace: opts.Namespace}); authErr != nil {
					return nil, "", ProviderResolutionInfo{}, authErr
				}
			}
			return nil, "", ProviderResolutionInfo{}, err
		}
		if err := authorizeProviderReference(opts, providerResolutionInfo(p)); err != nil {
			return nil, "", ProviderResolutionInfo{}, err
		}
		providerCRD = p
	}
	if providerCRD == nil && opts.RequireExplicitProvider {
		return nil, "", ProviderResolutionInfo{}, explicitProviderRequiredError()
	}

	if providerCRD == nil && r.config.Provider != "" {
		p := &corev1alpha1.Provider{}
		if err := r.client.Get(ctx, types.NamespacedName{Name: r.config.Provider, Namespace: opts.Namespace}, p); err == nil {
			providerCRD = p
		}
	}

	if providerCRD == nil {
		p := &corev1alpha1.Provider{}
		if err := r.client.Get(ctx, types.NamespacedName{Name: "default", Namespace: opts.Namespace}, p); err == nil {
			providerCRD = p
		}
	}
	if providerCRD == nil {
		fallback, err := r.soleReadyProvider(ctx, opts.Namespace, providerName, opts.RequireExplicitProvider)
		if err != nil {
			return nil, "", ProviderResolutionInfo{}, err
		}
		providerCRD = fallback
	}

	if model == "" {
		model = providerCRD.Spec.DefaultModel
	}
	if model == "" {
		model = r.config.Model
	}
	if opts.RequireModel && model == "" {
		return nil, "", ProviderResolutionInfo{}, fmt.Errorf("no model specified and no default model configured")
	}
	providerInfo := providerResolutionInfo(providerCRD)
	if err := authorizeProviderUse(opts, providerInfo, model); err != nil {
		return nil, "", ProviderResolutionInfo{}, err
	}

	apiKey, err := r.ResolveAPIKey(ctx, providerCRD)
	if err != nil {
		return nil, "", ProviderResolutionInfo{}, err
	}

	provider, resolvedModel, err := r.buildProvider(providerCRD, apiKey, model)
	if err != nil {
		return nil, "", ProviderResolutionInfo{}, err
	}

	return provider, resolvedModel, providerInfo, nil
}

func authorizeProviderReference(opts ResolveOpts, provider ProviderResolutionInfo) error {
	if opts.AuthorizeProviderReference == nil {
		return nil
	}
	return opts.AuthorizeProviderReference(provider)
}

func authorizeProviderUse(opts ResolveOpts, provider ProviderResolutionInfo, model string) error {
	if opts.AuthorizeProviderUse == nil {
		return nil
	}
	return opts.AuthorizeProviderUse(provider, model)
}

func explicitProviderRequiredError() error {
	return fmt.Errorf("no provider selected and no default Provider is configured; pass an explicit provider")
}

func providerResolutionInfo(providerCRD *corev1alpha1.Provider) ProviderResolutionInfo {
	if providerCRD == nil {
		return ProviderResolutionInfo{}
	}
	return ProviderResolutionInfo{
		Name:      providerCRD.Name,
		Namespace: providerCRD.Namespace,
		Type:      string(providerCRD.Spec.Type),
	}
}

func (r *ProviderResolver) buildProvider(providerCRD *corev1alpha1.Provider, apiKey, model string) (llm.Provider, string, error) {
	providerConfig := llm.ProviderConfig{
		APIKey:       apiKey,
		BaseURL:      providerCRD.Spec.BaseURL,
		ProviderType: string(providerCRD.Spec.Type),
	}
	if providerCRD.Spec.Azure != nil {
		providerConfig.AzureAPIVersion = providerCRD.Spec.Azure.APIVersion
		providerConfig.AzureDeploymentName = providerCRD.Spec.Azure.DeploymentName
	}

	provider, err := llm.NewProvider(string(providerCRD.Spec.Type), providerConfig)
	if err != nil {
		return nil, "", fmt.Errorf("failed to create LLM provider: %w", err)
	}

	return provider, model, nil
}

func (r *ProviderResolver) LookupProvider(ctx context.Context, name, namespace string) (*corev1alpha1.Provider, error) {
	p := &corev1alpha1.Provider{}
	if err := r.client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, p); err != nil {
		return nil, fmt.Errorf("provider %q not found in namespace %q: %w", name, namespace, err)
	}
	return p, nil
}

func (r *ProviderResolver) ResolveAPIKey(ctx context.Context, providerCRD *corev1alpha1.Provider) (string, error) {
	secretName := providerCRD.Spec.SecretRef.Name
	secretKey := providerCRD.Spec.SecretRef.Key
	if secretKey == "" {
		secretKey = "api-key"
	}
	secret := &corev1.Secret{}
	if err := r.client.Get(ctx, types.NamespacedName{Name: secretName, Namespace: providerCRD.Namespace}, secret); err != nil {
		return "", fmt.Errorf("failed to get provider secret %q: %w", secretName, err)
	}
	apiKeyBytes, ok := secret.Data[secretKey]
	if !ok {
		return "", fmt.Errorf("secret %q has no key %q", secretName, secretKey)
	}
	return string(apiKeyBytes), nil
}

func (r *ProviderResolver) soleReadyProvider(ctx context.Context, namespace, requestedName string, requireExplicitProvider bool) (*corev1alpha1.Provider, error) {
	list := &corev1alpha1.ProviderList{}
	if err := r.client.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("no provider %q found and no default Provider is configured (listing Providers failed: %w)", requestedName, err)
	}
	var ready []*corev1alpha1.Provider
	for i := range list.Items {
		item := &list.Items[i]
		if item.Status.Ready {
			ready = append(ready, item)
		}
	}
	if len(ready) == 1 && !requireExplicitProvider {
		return ready[0], nil
	}
	if requireExplicitProvider {
		return nil, explicitProviderRequiredError()
	}
	prefix := "no provider selected"
	if requestedName != "" {
		prefix = fmt.Sprintf("no provider %q found", requestedName)
	}
	if len(list.Items) == 0 {
		return nil, fmt.Errorf("%s and namespace %q has no Providers; create a Provider (or one named \"default\") or configure the chat default provider", prefix, namespace)
	}
	return nil, fmt.Errorf("%s and no default Provider is configured; list the Providers you can use with the providers API and pass one, create a Provider named \"default\", or configure the chat default provider", prefix)
}

func requestUsesContextToken(c fiber.Ctx) bool {
	ui := GetUserInfo(c)
	return ui != nil && ui.AuthType == AuthTypeContextToken && ui.ContextToken != nil
}

func requestRequiresExplicitProvider(c fiber.Ctx, authorization ContextTokenAuthorizationConfig) bool {
	return authorization.enforcing() && requestUsesContextToken(c)
}
