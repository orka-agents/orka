/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/connectors"
)

const connectorProviderRefreshInterval = 5 * time.Minute

// ConnectorProviderReconciler validates provider OAuth settings, tool
// declarations, and the client secret reference. It never reads the client
// secret value into status or logs.
type ConnectorProviderReconciler struct {
	client.Client
	APIReader client.Reader
	Scheme    *runtime.Scheme
	// KnownBuiltinTool reports whether a Builtin tool name exists. Nil accepts
	// any well-formed name.
	KnownBuiltinTool connectors.BuiltinToolCheck
}

// +kubebuilder:rbac:groups=core.orka.ai,resources=connectorproviders,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=core.orka.ai,resources=connectorproviders/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core.orka.ai,resources=connectorproviders/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// ConnectorProviderConnectionsFinalizer holds a ConnectorProvider while
// Connections still reference it, so their tokens can be revoked against the
// client that issued them before the provider disappears.
const ConnectorProviderConnectionsFinalizer = "core.orka.ai/connector-connections"

// connectorProviderDeletionRequeue is how often a provider blocked by live
// Connections re-checks for their removal.
const connectorProviderDeletionRequeue = 30 * time.Second

// finalize releases a deleting provider only once no Connection in its
// namespace references it; until then the deletion is held and the
// remaining Connections are named in the Accepted condition.
func (r *ConnectorProviderReconciler) finalize(ctx context.Context, provider *corev1alpha1.ConnectorProvider) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(provider, ConnectorProviderConnectionsFinalizer) {
		return ctrl.Result{}, nil
	}
	// The decision below is irreversible (a released provider cannot revoke
	// its Connections' tokens), so the reference check reads the API server
	// rather than the cache, which can trail a Connection created moments ago.
	var reader client.Reader = r.Client
	if r.APIReader != nil {
		reader = r.APIReader
	}
	connections := &corev1alpha1.ConnectionList{}
	if err := reader.List(ctx, connections, client.InNamespace(provider.Namespace)); err != nil {
		return ctrl.Result{}, err
	}
	remaining := 0
	for i := range connections.Items {
		if connections.Items[i].Spec.ProviderRef.Name == provider.Name {
			remaining++
		}
	}
	if remaining > 0 {
		meta.SetStatusCondition(&provider.Status.Conditions, metav1.Condition{
			Type:               corev1alpha1.ConnectorProviderConditionAccepted,
			Status:             metav1.ConditionFalse,
			Reason:             connectors.ReasonConnectionsRemain,
			Message:            fmt.Sprintf("Deletion is held until the %d Connection(s) that reference this provider are removed", remaining),
			ObservedGeneration: provider.Generation,
			LastTransitionTime: metav1.Now(),
		})
		if err := r.Status().Update(ctx, provider); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: connectorProviderDeletionRequeue}, nil
	}
	controllerutil.RemoveFinalizer(provider, ConnectorProviderConnectionsFinalizer)
	if err := r.Update(ctx, provider); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *ConnectorProviderReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	provider := &corev1alpha1.ConnectorProvider{}
	if err := r.Get(ctx, req.NamespacedName, provider); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if !provider.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, provider)
	}
	// The provider's OAuth client is what revokes the tokens its Connections
	// hold; it must outlive them, or disconnect would delete the only sealed
	// copies without revoking anything.
	if !controllerutil.ContainsFinalizer(provider, ConnectorProviderConnectionsFinalizer) {
		controllerutil.AddFinalizer(provider, ConnectorProviderConnectionsFinalizer)
		if err := r.Update(ctx, provider); err != nil {
			return ctrl.Result{}, err
		}
	}

	now := metav1.Now()
	accepted := metav1.Condition{
		Type:               corev1alpha1.ConnectorProviderConditionAccepted,
		Status:             metav1.ConditionTrue,
		Reason:             connectors.ReasonAccepted,
		Message:            "Provider structure is valid",
		ObservedGeneration: provider.Generation,
		LastTransitionTime: now,
	}
	resolved := metav1.Condition{
		Type:               corev1alpha1.ConnectorProviderConditionResolvedRefs,
		Status:             metav1.ConditionTrue,
		Reason:             connectors.ReasonResolvedRefs,
		Message:            "Client secret reference is resolved",
		ObservedGeneration: provider.Generation,
		LastTransitionTime: now,
	}

	if issue := connectors.ValidateProviderSpec(provider, r.KnownBuiltinTool); issue != nil {
		accepted.Status = metav1.ConditionFalse
		accepted.Reason = issue.Reason
		accepted.Message = issue.Message
		resolved.Status = metav1.ConditionFalse
		resolved.Reason = connectors.ReasonInvalidProvider
		resolved.Message = "References were not resolved because the provider is invalid"
		return r.updateStatus(ctx, provider, accepted, resolved, nil)
	}

	issue, err := connectors.ResolveProviderReferences(ctx, r.referenceReader(), provider)
	if err != nil {
		resolved.Status = metav1.ConditionUnknown
		resolved.Reason = connectors.ReasonResolutionFailed
		resolved.Message = "Reference resolution could not be completed"
		return r.updateStatus(ctx, provider, accepted, resolved, err)
	}
	if issue != nil {
		resolved.Status = metav1.ConditionFalse
		resolved.Reason = issue.Reason
		resolved.Message = issue.Message
	}
	return r.updateStatus(ctx, provider, accepted, resolved, nil)
}

func (r *ConnectorProviderReconciler) updateStatus(
	ctx context.Context,
	provider *corev1alpha1.ConnectorProvider,
	accepted metav1.Condition,
	resolved metav1.Condition,
	reconcileErr error,
) (ctrl.Result, error) {
	before := provider.Status.DeepCopy()
	provider.Status.ObservedGeneration = provider.Generation
	meta.SetStatusCondition(&provider.Status.Conditions, accepted)
	meta.SetStatusCondition(&provider.Status.Conditions, resolved)
	if reflect.DeepEqual(before, &provider.Status) {
		return ctrl.Result{RequeueAfter: connectorProviderRefreshInterval}, reconcileErr
	}
	if err := r.Status().Update(ctx, provider); err != nil {
		return ctrl.Result{}, errors.Join(reconcileErr, err)
	}
	return ctrl.Result{RequeueAfter: connectorProviderRefreshInterval}, reconcileErr
}

func (r *ConnectorProviderReconciler) referenceReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *ConnectorProviderReconciler) requestsForSecret(ctx context.Context, object client.Object) []reconcile.Request {
	providers := &corev1alpha1.ConnectorProviderList{}
	if err := r.List(ctx, providers, client.InNamespace(object.GetNamespace())); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0)
	for i := range providers.Items {
		provider := &providers.Items[i]
		if connectors.ProviderReferencesSecret(provider, object) {
			requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: provider.Namespace, Name: provider.Name}})
		}
	}
	return requests
}

// SetupWithManager registers provider and client-secret watches.
func (r *ConnectorProviderReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.ConnectorProvider{}).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.requestsForSecret)).
		Named("connectorprovider").
		Complete(r)
}
