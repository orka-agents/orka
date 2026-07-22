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

	corev1 "k8s.io/api/core/v1"
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
	"github.com/orka-agents/orka/internal/outboundaccess"
)

// OutboundAccessPolicyReconciler validates policy structure and references.
type OutboundAccessPolicyReconciler struct {
	client.Client
	APIReader client.Reader
	Scheme    *runtime.Scheme
	Trust     outboundaccess.TrustConfig
}

const outboundAccessPolicyRefreshInterval = 5 * time.Minute

// +kubebuilder:rbac:groups=core.orka.ai,resources=outboundaccesspolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups=core.orka.ai,resources=outboundaccesspolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=secrets;services;serviceaccounts,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=serviceaccounts/token,verbs=create

func (r *OutboundAccessPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	policy := &corev1alpha1.OutboundAccessPolicy{}
	if err := r.Get(ctx, req.NamespacedName, policy); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	now := metav1.Now()
	accepted := metav1.Condition{
		Type:               corev1alpha1.OutboundAccessPolicyConditionAccepted,
		Status:             metav1.ConditionTrue,
		Reason:             outboundaccess.ReasonAccepted,
		Message:            "Policy structure is valid",
		ObservedGeneration: policy.Generation,
		LastTransitionTime: now,
	}
	resolved := metav1.Condition{
		Type:               corev1alpha1.OutboundAccessPolicyConditionResolvedRefs,
		Status:             metav1.ConditionTrue,
		Reason:             outboundaccess.ReasonResolvedRefs,
		Message:            "All policy references are resolved",
		ObservedGeneration: policy.Generation,
		LastTransitionTime: now,
	}

	if issue := outboundaccess.ValidateSpec(policy); issue != nil {
		accepted.Status = metav1.ConditionFalse
		accepted.Reason = issue.Reason
		accepted.Message = issue.Message
		resolved.Status = metav1.ConditionFalse
		resolved.Reason = outboundaccess.ReasonInvalidPolicy
		resolved.Message = "References were not resolved because the policy is invalid"
		return r.updateOutboundAccessPolicyStatus(ctx, policy, accepted, resolved, nil)
	}

	issue, err := outboundaccess.ResolveReferences(ctx, r.referenceReader(), policy, r.Trust)
	if err != nil {
		resolved.Status = metav1.ConditionUnknown
		resolved.Reason = outboundaccess.ReasonResolutionFailed
		resolved.Message = "Reference resolution could not be completed"
		return r.updateOutboundAccessPolicyStatus(ctx, policy, accepted, resolved, err)
	}
	if issue != nil {
		resolved.Status = metav1.ConditionFalse
		resolved.Reason = issue.Reason
		resolved.Message = issue.Message
	}
	return r.updateOutboundAccessPolicyStatus(ctx, policy, accepted, resolved, nil)
}

func (r *OutboundAccessPolicyReconciler) updateOutboundAccessPolicyStatus(
	ctx context.Context,
	policy *corev1alpha1.OutboundAccessPolicy,
	accepted metav1.Condition,
	resolved metav1.Condition,
	reconcileErr error,
) (ctrl.Result, error) {
	before := policy.Status.DeepCopy()
	policy.Status.ObservedGeneration = policy.Generation
	meta.SetStatusCondition(&policy.Status.Conditions, accepted)
	meta.SetStatusCondition(&policy.Status.Conditions, resolved)
	if reflect.DeepEqual(before, &policy.Status) {
		return ctrl.Result{RequeueAfter: outboundAccessPolicyRefreshInterval}, reconcileErr
	}
	if err := r.Status().Update(ctx, policy); err != nil {
		if reconcileErr != nil {
			return ctrl.Result{}, errors.Join(reconcileErr, err)
		}
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: outboundAccessPolicyRefreshInterval}, reconcileErr
}

func (r *OutboundAccessPolicyReconciler) referenceReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *OutboundAccessPolicyReconciler) requestsForReferencedObject(ctx context.Context, object client.Object) []reconcile.Request {
	policies := &corev1alpha1.OutboundAccessPolicyList{}
	if err := r.List(ctx, policies); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0)
	for i := range policies.Items {
		policy := &policies.Items[i]
		if outboundPolicyReferencesObject(policy, object) {
			requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: policy.Namespace, Name: policy.Name}})
		}
	}
	return requests
}

//nolint:gocyclo // Reference matching intentionally covers each supported adapter reference type.
func outboundPolicyReferencesObject(policy *corev1alpha1.OutboundAccessPolicy, object client.Object) bool {
	if policy == nil || object == nil {
		return false
	}
	namespace, name := object.GetNamespace(), object.GetName()
	matchSecret := func(ref *corev1alpha1.NamespacedSecretKeySelector) bool {
		if ref == nil || ref.Name != name {
			return false
		}
		refNamespace := ref.Namespace
		if refNamespace == "" {
			refNamespace = policy.Namespace
		}
		return refNamespace == namespace
	}
	matchService := func(ref *corev1alpha1.OutboundServiceReference) bool {
		if ref == nil || ref.Name != name {
			return false
		}
		refNamespace := ref.Namespace
		if refNamespace == "" {
			refNamespace = policy.Namespace
		}
		return refNamespace == namespace
	}
	switch object.(type) {
	case *corev1.Secret:
		if direct := policy.Spec.Direct; direct != nil {
			if matchSecret(direct.Subject.SecretRef) || (direct.Actor != nil && matchSecret(direct.Actor.SecretRef)) {
				return true
			}
			if auth := direct.ClientAuthentication; auth != nil && (matchSecret(auth.ClientSecretRef) || matchSecret(auth.PrivateKeyRef)) {
				return true
			}
			if direct.TokenEndpoint.TLS != nil && matchSecret(direct.TokenEndpoint.TLS.CASecretRef) {
				return true
			}
		}
		return policy.Spec.Gateway != nil && policy.Spec.Gateway.TLS != nil && matchSecret(policy.Spec.Gateway.TLS.CASecretRef)
	case *corev1.Service:
		if policy.Spec.Direct != nil && matchService(policy.Spec.Direct.TokenEndpoint.ServiceRef) {
			return true
		}
		if policy.Spec.Gateway != nil {
			return matchService(&policy.Spec.Gateway.ServiceRef)
		}
	case *corev1.ServiceAccount:
		if namespace != policy.Namespace {
			return false
		}
		if direct := policy.Spec.Direct; direct != nil {
			if direct.Subject.ServiceAccountRef != nil && direct.Subject.ServiceAccountRef.Name == name {
				return true
			}
			return direct.Actor != nil && direct.Actor.ServiceAccountRef != nil && direct.Actor.ServiceAccountRef.Name == name
		}
	}
	return false
}

// SetupWithManager registers policy and referenced-object watches.
func (r *OutboundAccessPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	mapReferenced := handler.EnqueueRequestsFromMapFunc(r.requestsForReferencedObject)
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.OutboundAccessPolicy{}).
		Watches(&corev1.Secret{}, mapReferenced).
		Watches(&corev1.Service{}, mapReferenced).
		Watches(&corev1.ServiceAccount{}, mapReferenced).
		Named("outboundaccesspolicy").
		Complete(r)
}
