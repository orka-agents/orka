/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"reflect"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8svalidation "k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/connectors"
)

// ConnectionReconciler resolves a Connection's provider and projects link
// state. Token custody is handled by the API server's consent flow; this
// reconciler never reads or writes token material.
type ConnectionReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=core.orka.ai,resources=connections,verbs=get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups=core.orka.ai,resources=connections/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core.orka.ai,resources=connections/finalizers,verbs=update

func (r *ConnectionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	connection := &corev1alpha1.Connection{}
	if err := r.Get(ctx, req.NamespacedName, connection); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	// The status as read, before any pass mutates it: the write decision
	// compares against this, so a change made only to ScopesGranted is
	// persisted rather than mistaken for no change.
	before := connection.Status.DeepCopy()
	if !connection.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	now := metav1.Now()
	providerResolved := metav1.Condition{
		Type:               corev1alpha1.ConnectionConditionProviderResolved,
		Status:             metav1.ConditionTrue,
		Reason:             corev1alpha1.ConnectionReasonProviderResolved,
		Message:            "ConnectorProvider is accepted",
		ObservedGeneration: connection.Generation,
		LastTransitionTime: now,
	}

	provider := &corev1alpha1.ConnectorProvider{}
	var err error
	// A name no provider could ever have is a stable invalid reference, not
	// a read failure to retry forever.
	if len(k8svalidation.IsDNS1123Subdomain(connection.Spec.ProviderRef.Name)) > 0 {
		err = apierrors.NewNotFound(corev1alpha1.GroupVersion.WithResource("connectorproviders").GroupResource(), connection.Spec.ProviderRef.Name)
	} else {
		err = r.Get(ctx, types.NamespacedName{Namespace: connection.Namespace, Name: connection.Spec.ProviderRef.Name}, provider)
	}
	switch {
	case apierrors.IsNotFound(err):
		providerResolved.Status = metav1.ConditionFalse
		providerResolved.Reason = corev1alpha1.ConnectionReasonProviderMissing
		providerResolved.Message = "ConnectorProvider was not found"
	case err != nil:
		providerResolved.Status = metav1.ConditionUnknown
		providerResolved.Reason = corev1alpha1.ConnectionReasonProviderReadFailed
		providerResolved.Message = "ConnectorProvider could not be read"
		meta.SetStatusCondition(&connection.Status.Conditions, scopesUnknownCondition(connection, now))
		return r.updateStatus(ctx, connection, before, providerResolved, err)
	case !connectors.ProviderAccepted(provider):
		providerResolved.Status = metav1.ConditionFalse
		providerResolved.Reason = corev1alpha1.ConnectionReasonProviderInvalid
		providerResolved.Message = "ConnectorProvider is not accepted for its current generation"
	}
	if providerResolved.Status == metav1.ConditionTrue {
		meta.SetStatusCondition(&connection.Status.Conditions, scopesGrantedCondition(connection, provider, now))
	} else {
		meta.SetStatusCondition(&connection.Status.Conditions, scopesUnknownCondition(connection, now))
	}
	return r.updateStatus(ctx, connection, before, providerResolved, nil)
}

// scopesUnknownCondition replaces a ScopesGranted verdict while the provider
// is not resolved, so the conditions never pair ProviderResolved=False with
// a stale ScopesGranted=True.
func scopesUnknownCondition(connection *corev1alpha1.Connection, now metav1.Time) metav1.Condition {
	return metav1.Condition{
		Type:               corev1alpha1.ConnectionConditionScopesGranted,
		Status:             metav1.ConditionUnknown,
		Reason:             corev1alpha1.ConnectionReasonProviderUnavailable,
		Message:            "Granted scopes cannot be judged while the provider is unavailable",
		ObservedGeneration: connection.Generation,
		LastTransitionTime: now,
	}
}

// scopesGrantedCondition compares the scopes granted at the last consent with
// the scopes the current mode and provider require. It never touches the
// consent-owned Ready condition, so widening projects Pending while the
// read-only consent stays valid, narrowing restores readiness, and a provider
// that starts requiring more scopes asks for consent again.
func scopesGrantedCondition(connection *corev1alpha1.Connection, provider *corev1alpha1.ConnectorProvider, now metav1.Time) metav1.Condition {
	condition := metav1.Condition{
		Type:               corev1alpha1.ConnectionConditionScopesGranted,
		ObservedGeneration: connection.Generation,
		LastTransitionTime: now,
	}
	ready := meta.FindStatusCondition(connection.Status.Conditions, corev1alpha1.ConnectionConditionReady)
	if ready == nil || ready.Status != metav1.ConditionTrue {
		condition.Status = metav1.ConditionFalse
		condition.Reason = corev1alpha1.ConnectionReasonPendingConsent
		condition.Message = "Consent has not completed"
		return condition
	}
	if !connectors.ConsentMatchesProvider(connection, provider) {
		// The held token belongs to another OAuth client (the provider was
		// replaced, or its client or endpoints changed); it must never be
		// refreshed against the new authority.
		condition.Status = metav1.ConditionFalse
		condition.Reason = corev1alpha1.ConnectionReasonConsentRequired
		condition.Message = "The provider's OAuth client changed since consent; consent again"
		return condition
	}
	mode := connection.Spec.Mode
	if mode == "" {
		mode = corev1alpha1.ConnectionModeReadOnly
	}
	if connectors.ScopesCover(connection.Status.GrantedScopes, connectors.ScopesForMode(provider, mode)) {
		condition.Status = metav1.ConditionTrue
		condition.Reason = corev1alpha1.ConnectionReasonScopesGranted
		condition.Message = "Granted scopes cover the " + mode + " mode"
		return condition
	}
	condition.Status = metav1.ConditionFalse
	condition.Reason = corev1alpha1.ConnectionReasonConsentRequired
	condition.Message = "Granted scopes do not cover the " + mode + " mode; consent again"
	return condition
}

func (r *ConnectionReconciler) updateStatus(
	ctx context.Context,
	connection *corev1alpha1.Connection,
	before *corev1alpha1.ConnectionStatus,
	providerResolved metav1.Condition,
	reconcileErr error,
) (ctrl.Result, error) {
	connection.Status.ObservedGeneration = connection.Generation
	meta.SetStatusCondition(&connection.Status.Conditions, providerResolved)
	connection.Status.State = projectConnectionState(connection, providerResolved)
	// Connection and provider changes are watched; nothing here depends on
	// the passage of time, so there is no periodic requeue to multiply by
	// the number of linked accounts.
	if reflect.DeepEqual(before, &connection.Status) {
		return ctrl.Result{}, reconcileErr
	}
	if err := r.Status().Update(ctx, connection); err != nil {
		return ctrl.Result{}, errors.Join(reconcileErr, err)
	}
	return ctrl.Result{}, reconcileErr
}

// projectConnectionState derives the coarse state from the conditions. An
// unresolved provider is an Error; otherwise the Ready condition owned by the
// consent and refresh paths decides, qualified by the controller-owned
// ScopesGranted condition. Its reason, not the previously stored
// state, carries Expired and Revoked, so a provider outage that briefly
// projects Error cannot erase them.
func projectConnectionState(connection *corev1alpha1.Connection, providerResolved metav1.Condition) string {
	if providerResolved.Status != metav1.ConditionTrue {
		return corev1alpha1.ConnectionStateError
	}
	ready := meta.FindStatusCondition(connection.Status.Conditions, corev1alpha1.ConnectionConditionReady)
	if ready == nil {
		return corev1alpha1.ConnectionStatePending
	}
	if ready.Status == metav1.ConditionTrue {
		granted := meta.FindStatusCondition(connection.Status.Conditions, corev1alpha1.ConnectionConditionScopesGranted)
		if granted != nil && granted.Status != metav1.ConditionTrue {
			// Consent is valid but does not cover the current mode.
			return corev1alpha1.ConnectionStatePending
		}
		return corev1alpha1.ConnectionStateReady
	}
	switch ready.Reason {
	case corev1alpha1.ConnectionReasonExpired:
		return corev1alpha1.ConnectionStateExpired
	case corev1alpha1.ConnectionReasonRevoked:
		return corev1alpha1.ConnectionStateRevoked
	}
	return corev1alpha1.ConnectionStatePending
}

func (r *ConnectionReconciler) requestsForProvider(ctx context.Context, object client.Object) []reconcile.Request {
	connections := &corev1alpha1.ConnectionList{}
	if err := r.List(ctx, connections, client.InNamespace(object.GetNamespace())); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0)
	for i := range connections.Items {
		connection := &connections.Items[i]
		if connection.Spec.ProviderRef.Name == object.GetName() {
			requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: connection.Namespace, Name: connection.Name}})
		}
	}
	return requests
}

// SetupWithManager registers Connection and provider watches.
func (r *ConnectionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.Connection{}).
		Watches(&corev1alpha1.ConnectorProvider{}, handler.EnqueueRequestsFromMapFunc(r.requestsForProvider)).
		Named("connection").
		Complete(r)
}
