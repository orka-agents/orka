/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/llm"
)

// ProviderResolver resolves LLM providers from Kubernetes CRDs and secrets.
// It consolidates provider lookup, API key resolution, and provider construction
// logic shared by the chat, OpenAI-compat, and Anthropic-compat handlers.
type ProviderResolver struct {
	client client.Client
	config ChatConfig
}

// ResolveOpts configures how a provider is resolved.
type ResolveOpts struct {
	ProviderName string // explicit provider name (chat handler)
	ModelStr     string // "provider/model" format or plain model (compat handlers)
	Model        string // explicit model override (chat handler)
	AgentRef     string // agent reference for agent-based resolution (chat handler)
	Namespace    string
	RequireModel bool // return error if model is empty after resolution
}

// ProviderResolutionInfo contains the Provider CRD metadata selected for a request.
type ProviderResolutionInfo struct {
	Name      string
	Namespace string
	Type      string
}

// NewProviderResolver creates a new ProviderResolver.
func NewProviderResolver(c client.Client, config ChatConfig) *ProviderResolver {
	return &ProviderResolver{client: c, config: config}
}

// Resolve finds the appropriate LLM provider and model for a request.
// It handles provider/model string parsing, Kubernetes secret resolution,
// and provider construction.
//
// When AgentRef or ProviderName is set, provider lookups are fatal (errors are returned).
// Otherwise, intermediate lookups are non-fatal and fall through to defaults.
func (r *ProviderResolver) Resolve(ctx context.Context, opts ResolveOpts) (llm.Provider, string, error) {
	provider, model, _, err := r.ResolveWithInfo(ctx, opts)
	return provider, model, err
}

// ResolveWithInfo finds the appropriate LLM provider and model for a request,
// and also returns the selected Provider CRD metadata.
func (r *ProviderResolver) ResolveWithInfo(ctx context.Context, opts ResolveOpts) (llm.Provider, string, ProviderResolutionInfo, error) {
	if opts.AgentRef != "" || opts.ProviderName != "" {
		return r.resolveFromExplicit(ctx, opts)
	}
	return r.resolveFromModelStr(ctx, opts)
}

// resolveFromExplicit handles the chat handler path with explicit provider names
// and agent refs. Provider lookups by name are fatal.
func (r *ProviderResolver) resolveFromExplicit(ctx context.Context, opts ResolveOpts) (llm.Provider, string, ProviderResolutionInfo, error) {
	var model string
	var providerCRD *corev1alpha1.Provider

	if opts.AgentRef != "" {
		agent := &corev1alpha1.Agent{}
		if err := r.client.Get(ctx, types.NamespacedName{Name: opts.AgentRef, Namespace: opts.Namespace}, agent); err != nil {
			return nil, "", ProviderResolutionInfo{}, fmt.Errorf("agent %q not found: %w", opts.AgentRef, err)
		}
		if agent.Spec.Model != nil && agent.Spec.Model.Name != "" {
			model = agent.Spec.Model.Name
		}
		if agent.Spec.ProviderRef != nil {
			p, err := r.LookupProvider(ctx, agent.Spec.ProviderRef.Name, opts.Namespace)
			if err != nil {
				return nil, "", ProviderResolutionInfo{}, err
			}
			providerCRD = p
		}
	}

	if providerCRD == nil && opts.ProviderName != "" {
		p, err := r.LookupProvider(ctx, opts.ProviderName, opts.Namespace)
		if err != nil {
			return nil, "", ProviderResolutionInfo{}, err
		}
		providerCRD = p
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
			return nil, "", ProviderResolutionInfo{}, fmt.Errorf("no provider configured and no 'default' Provider CRD found: %w", err)
		}
		providerCRD = p
	}

	apiKey, err := r.ResolveAPIKey(ctx, providerCRD)
	if err != nil {
		return nil, "", ProviderResolutionInfo{}, err
	}

	// Model resolution priority: opts.Model > agent model > provider default > config model
	if opts.Model != "" {
		model = opts.Model
	}
	if model == "" {
		model = providerCRD.Spec.DefaultModel
	}
	if model == "" {
		model = r.config.Model
	}

	provider, resolvedModel, err := r.buildProvider(providerCRD, apiKey, model)
	if err != nil {
		return nil, "", ProviderResolutionInfo{}, err
	}

	return provider, resolvedModel, providerResolutionInfo(providerCRD), nil
}

// resolveFromModelStr handles the compat handler path with "provider/model" format
// strings. Intermediate provider lookups are non-fatal (silently fall through).
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
		p := &corev1alpha1.Provider{}
		if err := r.client.Get(ctx, types.NamespacedName{Name: providerName, Namespace: opts.Namespace}, p); err == nil {
			providerCRD = p
		}
	}

	if providerCRD == nil && r.config.Provider != "" {
		p := &corev1alpha1.Provider{}
		if err := r.client.Get(ctx, types.NamespacedName{Name: r.config.Provider, Namespace: opts.Namespace}, p); err == nil {
			providerCRD = p
		}
	}

	if providerCRD == nil {
		p := &corev1alpha1.Provider{}
		if err := r.client.Get(ctx, types.NamespacedName{Name: "default", Namespace: opts.Namespace}, p); err != nil {
			return nil, "", ProviderResolutionInfo{}, fmt.Errorf("no provider %q found and no 'default' Provider CRD exists", providerName)
		}
		providerCRD = p
	}

	apiKey, err := r.ResolveAPIKey(ctx, providerCRD)
	if err != nil {
		return nil, "", ProviderResolutionInfo{}, err
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

	provider, resolvedModel, err := r.buildProvider(providerCRD, apiKey, model)
	if err != nil {
		return nil, "", ProviderResolutionInfo{}, err
	}

	return provider, resolvedModel, providerResolutionInfo(providerCRD), nil
}

// providerResolutionInfo extracts stable metadata from a Provider CRD.
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

// buildProvider constructs an LLM provider from a Provider CRD, API key, and model.
func (r *ProviderResolver) buildProvider(providerCRD *corev1alpha1.Provider, apiKey, model string) (llm.Provider, string, error) {
	providerConfig := llm.ProviderConfig{
		APIKey:       apiKey,
		BaseURL:      providerCRD.Spec.BaseURL,
		ProviderType: string(providerCRD.Spec.Type),
	}
	if providerCRD.Spec.Azure != nil {
		providerConfig.AzureAPIVersion = providerCRD.Spec.Azure.APIVersion
	}

	provider, err := llm.NewProvider(string(providerCRD.Spec.Type), providerConfig)
	if err != nil {
		return nil, "", fmt.Errorf("failed to create LLM provider: %w", err)
	}

	return provider, model, nil
}

// LookupProvider fetches a Provider CRD by name and namespace.
func (r *ProviderResolver) LookupProvider(ctx context.Context, name, namespace string) (*corev1alpha1.Provider, error) {
	p := &corev1alpha1.Provider{}
	if err := r.client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, p); err != nil {
		return nil, fmt.Errorf("provider %q not found in namespace %q: %w", name, namespace, err)
	}
	return p, nil
}

// ResolveAPIKey extracts the API key from a Provider CRD's secret reference.
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
