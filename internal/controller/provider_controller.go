/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/provideridentity"
)

const (
	reasonValidationSucceeded = "ValidationSucceeded"
	reasonValidationFailed    = "ValidationFailed"

	// ConditionProviderCredentialReady reports whether a keyless Provider's
	// identity can obtain a token. Providers with an API key Secret do not
	// carry it.
	ConditionProviderCredentialReady = "CredentialReady"
	reasonTokenIssued                = "TokenIssued"
	reasonTokenRefused               = "TokenRefused"
	reasonIssuerUnavailable          = "IssuerUnavailable"

	// providerCredentialRecheck is how often a keyless Provider's identity
	// is verified with its issuer while its generation is unchanged.
	providerCredentialRecheck = time.Hour
	// providerCredentialRetry is how soon an unanswered verification is
	// retried.
	providerCredentialRetry = time.Minute
	providerVerifyTimeout   = 30 * time.Second
)

// ProviderIdentityVerifier checks keyless Provider identities.
type ProviderIdentityVerifier interface {
	// Check reports, without asking the issuer, whether the identity is
	// configured and may be used for baseURL.
	Check(identity, baseURL string) error
	// Verify obtains a fresh token for the identity and discards it.
	Verify(ctx context.Context, identity, baseURL string) error
}

// ProviderReconciler reconciles a Provider object
type ProviderReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Identities verifies keyless Providers; nil refuses them.
	Identities ProviderIdentityVerifier
	// Now returns the current time; time.Now when nil.
	Now func() time.Time

	// verifications remembers the last verification per Provider
	// namespace/name, so a status-only reconcile never asks the issuer again.
	// A record for a replaced Provider (another UID) is ignored, and a
	// deleted Provider's record is removed.
	verifications sync.Map
	// issuerBackoff records, per Provider identity, until when no Provider
	// asks the issuer again after it did not answer, so Providers sharing an
	// identity do not each wait out a timeout during an outage.
	issuerBackoff sync.Map
}

type providerVerification struct {
	uid        types.UID
	generation int64
	identity   string
	baseURL    string
	at         time.Time
	condition  metav1.Condition
}

// +kubebuilder:rbac:groups=core.orka.ai,resources=providers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core.orka.ai,resources=providers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core.orka.ai,resources=providers/finalizers,verbs=update

// Reconcile handles Provider reconciliation
func (r *ProviderReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the Provider
	provider := &corev1alpha1.Provider{}
	if err := r.Get(ctx, req.NamespacedName, provider); err != nil {
		if apierrors.IsNotFound(err) {
			r.verifications.Delete(req.NamespacedName)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	logger.Info("Reconciling Provider", "provider", provider.Name, "type", provider.Spec.Type)

	// A Provider that no longer uses keyless auth keeps no credential state,
	// whether or not its Secret is valid.
	if !provider.UsesKeylessAuth() {
		r.verifications.Delete(client.ObjectKeyFromObject(provider))
		meta.RemoveStatusCondition(&provider.Status.Conditions, ConditionProviderCredentialReady)
	}

	// Validate the provider configuration
	if err := r.validateProvider(ctx, provider); err != nil {
		logger.Error(err, "Provider validation failed")
		if provider.UsesKeylessAuth() {
			r.verifications.Delete(client.ObjectKeyFromObject(provider))
			meta.SetStatusCondition(&provider.Status.Conditions, metav1.Condition{
				Type: ConditionProviderCredentialReady, Status: metav1.ConditionFalse,
				Reason: reasonValidationFailed, Message: "the Provider configuration is invalid",
				ObservedGeneration: provider.Generation,
			})
		}
		return r.updateStatus(ctx, provider, false, err.Error())
	}

	if !provider.UsesKeylessAuth() {
		return r.updateStatus(ctx, provider, true, "Provider configuration is valid")
	}

	credential, requeue := r.verifyCredential(ctx, provider)
	meta.SetStatusCondition(&provider.Status.Conditions, credential)
	ready, message := true, "Provider configuration is valid"
	switch credential.Status {
	case metav1.ConditionFalse:
		ready, message = false, credential.Message
	case metav1.ConditionUnknown:
		message = "Provider configuration is valid; " + credential.Message
	}
	result, err := r.updateStatus(ctx, provider, ready, message)
	if err != nil {
		return result, err
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

// verifyCredential returns the CredentialReady condition of a keyless
// Provider and when to verify it again. The issuer is asked at most once per
// providerCredentialRecheck for an unchanged generation, identity and
// endpoint, and again after providerCredentialRetry when it did not answer.
// Only the issuer's error code reaches the condition.
func (r *ProviderReconciler) verifyCredential(ctx context.Context, provider *corev1alpha1.Provider) (metav1.Condition, time.Duration) {
	identity, baseURL := provider.EntraIdentity(), provider.Spec.BaseURL
	now := r.now()
	key := client.ObjectKeyFromObject(provider)
	if cached, ok := r.verifications.Load(key); ok {
		last := cached.(providerVerification)
		interval := providerCredentialRecheck
		if last.condition.Status == metav1.ConditionUnknown {
			interval = providerCredentialRetry
		}
		if last.uid == provider.UID && last.generation == provider.Generation && last.identity == identity && last.baseURL == baseURL {
			if wait := last.at.Add(interval).Sub(now); wait > 0 {
				return last.condition, wait
			}
		}
	}
	condition := metav1.Condition{Type: ConditionProviderCredentialReady, ObservedGeneration: provider.Generation}
	unavailable := func() {
		condition.Status, condition.Reason = metav1.ConditionUnknown, reasonIssuerUnavailable
		condition.Message = fmt.Sprintf("the token issuer for Provider identity %q did not answer; retrying", identity)
	}
	if until, ok := r.issuerBackoff.Load(identity); ok {
		if wait := until.(time.Time).Sub(now); wait > 0 {
			unavailable()
			return condition, wait
		}
	}
	requeue := providerCredentialRecheck
	verifyCtx, cancel := context.WithTimeout(ctx, providerVerifyTimeout)
	defer cancel()
	switch err := r.Identities.Verify(verifyCtx, identity, baseURL); {
	case err == nil:
		condition.Status, condition.Reason = metav1.ConditionTrue, reasonTokenIssued
		condition.Message = fmt.Sprintf("Provider identity %q obtained a token", identity)
	case errors.Is(err, provideridentity.ErrIssuerUnavailable) || errors.Is(err, context.DeadlineExceeded):
		unavailable()
		requeue = providerCredentialRetry
		r.issuerBackoff.Store(identity, now.Add(providerCredentialRetry))
	default:
		condition.Status, condition.Reason = metav1.ConditionFalse, reasonTokenRefused
		condition.Message = fmt.Sprintf("Provider identity %q could not obtain a token: %v", identity, err)
	}
	if condition.Status != metav1.ConditionUnknown {
		r.issuerBackoff.Delete(identity)
	}
	r.verifications.Store(key, providerVerification{
		uid: provider.UID, generation: provider.Generation, identity: identity, baseURL: baseURL, at: now, condition: condition,
	})
	return condition, requeue
}

func (r *ProviderReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// validateProvider validates the provider configuration
func (r *ProviderReconciler) validateProvider(ctx context.Context, provider *corev1alpha1.Provider) error {
	if provider.UsesKeylessAuth() {
		return r.validateKeylessProvider(provider)
	}
	if provider.Spec.SecretRef == nil {
		return fmt.Errorf("spec.secretRef is required unless spec.auth is set")
	}

	// Check that the referenced secret exists
	secret := &corev1.Secret{}
	secretKey := client.ObjectKey{
		Namespace: provider.Namespace,
		Name:      provider.Spec.SecretRef.Name,
	}

	if err := r.Get(ctx, secretKey, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("referenced secret not found: %s", provider.Spec.SecretRef.Name)
		}
		return err
	}

	// Check that the key exists in the secret
	key := provider.Spec.SecretRef.Key
	if key == "" {
		key = "api-key"
	}

	if _, ok := secret.Data[key]; !ok {
		return fmt.Errorf("key '%s' not found in secret", key)
	}

	// Validate Azure-specific configuration
	if provider.Spec.Type == corev1alpha1.ProviderTypeAzureOpenAI {
		if provider.Spec.Azure == nil || provider.Spec.Azure.DeploymentName == "" {
			return fmt.Errorf("azure.deploymentName is required for azure-openai provider")
		}
		if provider.Spec.BaseURL == "" {
			return fmt.Errorf("baseURL is required for azure-openai provider")
		}
	}

	return nil
}

// validateKeylessProvider checks a keyless Provider against the identities
// configured on this controller. Admission enforces the same shape; this
// covers objects stored before it, or by an older CRD.
func (r *ProviderReconciler) validateKeylessProvider(provider *corev1alpha1.Provider) error {
	if provider.Spec.SecretRef != nil {
		return fmt.Errorf("set exactly one of spec.secretRef or spec.auth")
	}
	identity := provider.EntraIdentity()
	if identity == "" {
		return fmt.Errorf("spec.auth needs an issuer; set spec.auth.entra")
	}
	if provider.Spec.Type != corev1alpha1.ProviderTypeAzureOpenAI {
		return fmt.Errorf("spec.auth is supported only for azure-openai Providers")
	}
	if provider.Spec.Azure == nil || provider.Spec.Azure.DeploymentName == "" {
		return fmt.Errorf("azure.deploymentName is required for azure-openai provider")
	}
	// The Azure OpenAI SDK places a baseURL path after /openai, so a prefixed
	// baseURL never reaches valid Azure paths; only the resource's origin does.
	if base, err := url.Parse(provider.Spec.BaseURL); err != nil || strings.Trim(base.Path, "/") != "" ||
		base.RawQuery != "" || base.Fragment != "" {
		return fmt.Errorf("a keyless Provider's baseURL must be the resource's origin, such as https://example.openai.azure.com, without a path or query")
	}
	if r.Identities == nil {
		return fmt.Errorf("Provider identity %q is not configured: this controller has no Provider identities", identity) //nolint:staticcheck // Resource kind begins the message.
	}
	switch err := r.Identities.Check(identity, provider.Spec.BaseURL); {
	case err == nil:
		return nil
	case errors.Is(err, provideridentity.ErrNotConfigured):
		return fmt.Errorf("Provider identity %q is not configured on this controller", identity) //nolint:staticcheck // Resource kind begins the message.
	case errors.Is(err, provideridentity.ErrEndpointNotAllowed):
		return fmt.Errorf("the baseURL origin is not in the allowedEndpoints of Provider identity %q", identity)
	default:
		return err
	}
}

// updateStatus updates the provider status
func (r *ProviderReconciler) updateStatus(ctx context.Context, provider *corev1alpha1.Provider, ready bool, message string) (ctrl.Result, error) {
	now := metav1.Now()

	provider.Status.Ready = ready
	provider.Status.Message = message

	if ready {
		provider.Status.LastValidated = &now
	}

	// Update condition
	condition := metav1.Condition{
		Type:               conditionReasonReady,
		LastTransitionTime: now,
		ObservedGeneration: provider.Generation,
	}

	if ready {
		condition.Status = metav1.ConditionTrue
		condition.Reason = reasonValidationSucceeded
		condition.Message = message
	} else {
		condition.Status = metav1.ConditionFalse
		condition.Reason = reasonValidationFailed
		condition.Message = message
	}

	meta.SetStatusCondition(&provider.Status.Conditions, condition)

	if err := r.Status().Update(ctx, provider); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager
func (r *ProviderReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.Provider{}).
		Named("provider").
		Complete(r)
}
