/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"reflect"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/connectors"
)

const connectionRefreshInterval = 5 * time.Minute

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
	err := r.Get(ctx, types.NamespacedName{Namespace: connection.Namespace, Name: connection.Spec.ProviderRef.Name}, provider)
	switch {
	case apierrors.IsNotFound(err):
		providerResolved.Status = metav1.ConditionFalse
		providerResolved.Reason = corev1alpha1.ConnectionReasonProviderMissing
		providerResolved.Message = "ConnectorProvider was not found"
	case err != nil:
		providerResolved.Status = metav1.ConditionUnknown
		providerResolved.Reason = connectors.ReasonResolutionFailed
		providerResolved.Message = "ConnectorProvider could not be read"
		return r.updateStatus(ctx, connection, providerResolved, err)
	case !connectors.ProviderAccepted(provider):
		providerResolved.Status = metav1.ConditionFalse
		providerResolved.Reason = corev1alpha1.ConnectionReasonProviderInvalid
		providerResolved.Message = "ConnectorProvider is not accepted for its current generation"
	}
	return r.updateStatus(ctx, connection, providerResolved, nil)
}

func (r *ConnectionReconciler) updateStatus(
	ctx context.Context,
	connection *corev1alpha1.Connection,
	providerResolved metav1.Condition,
	reconcileErr error,
) (ctrl.Result, error) {
	before := connection.Status.DeepCopy()
	connection.Status.ObservedGeneration = connection.Generation
	meta.SetStatusCondition(&connection.Status.Conditions, providerResolved)
	demoteStaleReadiness(connection)
	connection.Status.State = projectConnectionState(connection, providerResolved)
	if reflect.DeepEqual(before, &connection.Status) {
		return ctrl.Result{RequeueAfter: connectionRefreshInterval}, reconcileErr
	}
	if err := r.Status().Update(ctx, connection); err != nil {
		return ctrl.Result{}, errors.Join(reconcileErr, err)
	}
	return ctrl.Result{RequeueAfter: connectionRefreshInterval}, reconcileErr
}

// demoteStaleReadiness withdraws Ready when the spec widened to readWrite
// after the last consent. Consent granted only the read scopes for the older
// generation, so advertising Ready for the new generation would expose write
// tools without the person's approval. Narrowing needs no new consent.
func demoteStaleReadiness(connection *corev1alpha1.Connection) {
	ready := meta.FindStatusCondition(connection.Status.Conditions, corev1alpha1.ConnectionConditionReady)
	if ready == nil || ready.Status != metav1.ConditionTrue {
		return
	}
	if connection.Spec.Mode != corev1alpha1.ConnectionModeReadWrite || ready.ObservedGeneration >= connection.Generation {
		return
	}
	meta.SetStatusCondition(&connection.Status.Conditions, metav1.Condition{
		Type:               corev1alpha1.ConnectionConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             corev1alpha1.ConnectionReasonConsentRequired,
		Message:            "Mode widened to readWrite; consent for write scopes is required",
		ObservedGeneration: connection.Generation,
		LastTransitionTime: metav1.Now(),
	})
}

// projectConnectionState derives the coarse state from the conditions. An
// unresolved provider is an Error; otherwise the Ready condition owned by the
// consent and refresh paths decides. Its reason, not the previously stored
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
