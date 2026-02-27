/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
)

const (
	reasonValidationSucceeded = "ValidationSucceeded"
	reasonValidationFailed    = "ValidationFailed"
)

// ProviderReconciler reconciles a Provider object
type ProviderReconciler struct {
	client.Client
	Scheme *runtime.Scheme
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
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	logger.Info("Reconciling Provider", "provider", provider.Name, "type", provider.Spec.Type)

	// Validate the provider configuration
	if err := r.validateProvider(ctx, provider); err != nil {
		logger.Error(err, "Provider validation failed")
		return r.updateStatus(ctx, provider, false, err.Error())
	}

	// Provider is valid
	return r.updateStatus(ctx, provider, true, "Provider configuration is valid")
}

// validateProvider validates the provider configuration
func (r *ProviderReconciler) validateProvider(ctx context.Context, provider *corev1alpha1.Provider) error {
	// Check that the referenced secret exists
	secret := &corev1.Secret{}
	secretKey := client.ObjectKey{
		Namespace: provider.Namespace,
		Name:      provider.Spec.SecretRef.Name,
	}

	if err := r.Get(ctx, secretKey, secret); err != nil {
		if errors.IsNotFound(err) {
			return &ValidationError{Message: "referenced secret not found: " + provider.Spec.SecretRef.Name}
		}
		return err
	}

	// Check that the key exists in the secret
	key := provider.Spec.SecretRef.Key
	if key == "" {
		key = "api-key"
	}

	if _, ok := secret.Data[key]; !ok {
		return &ValidationError{Message: "key '" + key + "' not found in secret"}
	}

	// Validate Azure-specific configuration
	if provider.Spec.Type == corev1alpha1.ProviderTypeAzureOpenAI {
		if provider.Spec.Azure == nil || provider.Spec.Azure.DeploymentName == "" {
			return &ValidationError{Message: "azure.deploymentName is required for azure-openai provider"}
		}
		if provider.Spec.BaseURL == "" {
			return &ValidationError{Message: "baseURL is required for azure-openai provider"}
		}
	}

	return nil
}

// ValidationError represents a validation error
type ValidationError struct {
	Message string
}

func (e *ValidationError) Error() string {
	return e.Message
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
		Type:               "Ready",
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
