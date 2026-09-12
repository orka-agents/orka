/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/outboundaccess"
)

// OutboundAccessPolicyReconciler validates policy structure and references.
type OutboundAccessPolicyReconciler struct {
	client.Client
	APIReader                  client.Reader
	Scheme                     *runtime.Scheme
	Trust                      outboundaccess.TrustConfig
	AIWorkerServiceAccountName string
}

const (
	outboundAccessPolicyRefreshInterval = 5 * time.Minute
	outboundTokenRequestRequeueInterval = 100 * time.Millisecond

	outboundTokenRequestRBACFinalizer          = "core.orka.ai/outbound-tokenrequest-rbac"
	outboundTokenRequestRBACPrefix             = "orka-oap-tokenrequest-"
	outboundTokenRequestPolicyAnnotationKey    = "orka.ai/outbound-tokenrequest-policy"
	outboundTokenRequestPolicyUIDAnnotationKey = "orka.ai/outbound-tokenrequest-policy-uid"
	outboundTokenRequestManagedByLabelKey      = "orka.ai/managed-by"
	outboundTokenRequestManagedByLabelValue    = "orka"
	maxOutboundTokenRequestRBACNameLength      = 253
	outboundTokenRequestRBACHashLength         = 16
)

// +kubebuilder:rbac:groups=core.orka.ai,resources=outboundaccesspolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups=core.orka.ai,resources=outboundaccesspolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core.orka.ai,resources=outboundaccesspolicies/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets;services;serviceaccounts,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=serviceaccounts/token,verbs=create
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;delete

func (r *OutboundAccessPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	policy := &corev1alpha1.OutboundAccessPolicy{}
	if err := r.Get(ctx, req.NamespacedName, policy); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !policy.DeletionTimestamp.IsZero() {
		return r.finalizeOutboundTokenRequestGrant(ctx, policy)
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
		return r.finishWithoutOutboundTokenRequestGrant(ctx, policy, accepted, resolved, nil)
	}

	issue, err := outboundaccess.ResolveReferences(ctx, r.referenceReader(), policy, r.Trust)
	if err != nil {
		resolved.Status = metav1.ConditionUnknown
		resolved.Reason = outboundaccess.ReasonResolutionFailed
		resolved.Message = "Reference resolution could not be completed"
		return r.finishWithoutOutboundTokenRequestGrant(ctx, policy, accepted, resolved, err)
	}
	if issue != nil {
		resolved.Status = metav1.ConditionFalse
		resolved.Reason = issue.Reason
		resolved.Message = issue.Message
		return r.finishWithoutOutboundTokenRequestGrant(ctx, policy, accepted, resolved, nil)
	}

	serviceAccounts := outboundTokenRequestServiceAccountNames(policy)
	if len(serviceAccounts) == 0 {
		return r.finishWithoutOutboundTokenRequestGrant(ctx, policy, accepted, resolved, nil)
	}
	if !controllerutil.ContainsFinalizer(policy, outboundTokenRequestRBACFinalizer) {
		controllerutil.AddFinalizer(policy, outboundTokenRequestRBACFinalizer)
		if err := r.Update(ctx, policy); err != nil {
			return ctrl.Result{}, fmt.Errorf("add outbound TokenRequest RBAC finalizer: %w", err)
		}
		return ctrl.Result{RequeueAfter: outboundTokenRequestRequeueInterval}, nil
	}

	ready, err := r.reconcileOutboundTokenRequestGrant(ctx, policy, serviceAccounts)
	if err != nil {
		resolved.Status = metav1.ConditionUnknown
		resolved.Reason = outboundaccess.ReasonResolutionFailed
		resolved.Message = "ServiceAccount TokenRequest authorization could not be reconciled"
		return r.updateOutboundAccessPolicyStatus(ctx, policy, accepted, resolved, err, outboundTokenRequestRequeueInterval)
	}
	if !ready {
		resolved.Status = metav1.ConditionUnknown
		resolved.Reason = outboundaccess.ReasonResolutionFailed
		resolved.Message = "ServiceAccount TokenRequest authorization is being reconciled"
		return r.updateOutboundAccessPolicyStatus(ctx, policy, accepted, resolved, nil, outboundTokenRequestRequeueInterval)
	}
	return r.updateOutboundAccessPolicyStatus(ctx, policy, accepted, resolved, nil, outboundAccessPolicyRefreshInterval)
}

func (r *OutboundAccessPolicyReconciler) finishWithoutOutboundTokenRequestGrant(
	ctx context.Context,
	policy *corev1alpha1.OutboundAccessPolicy,
	accepted metav1.Condition,
	resolved metav1.Condition,
	reconcileErr error,
) (ctrl.Result, error) {
	done, cleanupErr := r.revokeOutboundTokenRequestGrant(ctx, policy)
	reconcileErr = errors.Join(reconcileErr, cleanupErr)
	if cleanupErr != nil || !done {
		if resolved.Status == metav1.ConditionTrue {
			resolved.Status = metav1.ConditionUnknown
			resolved.Reason = outboundaccess.ReasonResolutionFailed
			resolved.Message = "Obsolete ServiceAccount TokenRequest authorization is being revoked"
		}
		return r.updateOutboundAccessPolicyStatus(ctx, policy, accepted, resolved, reconcileErr, outboundTokenRequestRequeueInterval)
	}
	if controllerutil.ContainsFinalizer(policy, outboundTokenRequestRBACFinalizer) {
		controllerutil.RemoveFinalizer(policy, outboundTokenRequestRBACFinalizer)
		if err := r.Update(ctx, policy); err != nil {
			return ctrl.Result{}, errors.Join(reconcileErr, fmt.Errorf("remove outbound TokenRequest RBAC finalizer: %w", err))
		}
		return ctrl.Result{RequeueAfter: outboundTokenRequestRequeueInterval}, reconcileErr
	}
	return r.updateOutboundAccessPolicyStatus(ctx, policy, accepted, resolved, reconcileErr, outboundAccessPolicyRefreshInterval)
}

func (r *OutboundAccessPolicyReconciler) finalizeOutboundTokenRequestGrant(
	ctx context.Context,
	policy *corev1alpha1.OutboundAccessPolicy,
) (ctrl.Result, error) {
	done, err := r.revokeOutboundTokenRequestGrant(ctx, policy)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !done {
		return ctrl.Result{RequeueAfter: outboundTokenRequestRequeueInterval}, nil
	}
	if !controllerutil.ContainsFinalizer(policy, outboundTokenRequestRBACFinalizer) {
		return ctrl.Result{}, nil
	}
	controllerutil.RemoveFinalizer(policy, outboundTokenRequestRBACFinalizer)
	if err := r.Update(ctx, policy); err != nil {
		return ctrl.Result{}, fmt.Errorf("remove outbound TokenRequest RBAC finalizer during deletion: %w", err)
	}
	return ctrl.Result{}, nil
}

func (r *OutboundAccessPolicyReconciler) updateOutboundAccessPolicyStatus(
	ctx context.Context,
	policy *corev1alpha1.OutboundAccessPolicy,
	accepted metav1.Condition,
	resolved metav1.Condition,
	reconcileErr error,
	requeueAfter time.Duration,
) (ctrl.Result, error) {
	before := policy.Status.DeepCopy()
	policy.Status.ObservedGeneration = policy.Generation
	meta.SetStatusCondition(&policy.Status.Conditions, accepted)
	meta.SetStatusCondition(&policy.Status.Conditions, resolved)
	if requeueAfter <= 0 {
		requeueAfter = outboundAccessPolicyRefreshInterval
	}
	if reflect.DeepEqual(before, &policy.Status) {
		return ctrl.Result{RequeueAfter: requeueAfter}, reconcileErr
	}
	if err := r.Status().Update(ctx, policy); err != nil {
		if reconcileErr != nil {
			return ctrl.Result{}, errors.Join(reconcileErr, err)
		}
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, reconcileErr
}

func (r *OutboundAccessPolicyReconciler) referenceReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *OutboundAccessPolicyReconciler) tokenRequestAuthorizationReader() (client.Reader, error) {
	if r.APIReader == nil {
		return nil, errors.New("uncached APIReader is required for TokenRequest RBAC reconciliation")
	}
	return r.APIReader, nil
}

func outboundTokenRequestServiceAccountNames(policy *corev1alpha1.OutboundAccessPolicy) []string {
	if policy == nil || policy.Spec.Direct == nil {
		return nil
	}
	names := map[string]struct{}{}
	add := func(source *corev1alpha1.OutboundTokenSource) {
		if source == nil || source.Source != corev1alpha1.OutboundTokenSourceServiceAccount || source.ServiceAccountRef == nil {
			return
		}
		if name := source.ServiceAccountRef.Name; name != "" {
			names[name] = struct{}{}
		}
	}
	add(&policy.Spec.Direct.Subject)
	add(policy.Spec.Direct.Actor)
	result := make([]string, 0, len(names))
	for name := range names {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

func outboundTokenRequestRBACName(
	policy *corev1alpha1.OutboundAccessPolicy,
	serviceAccounts []string,
	aiWorkerServiceAccountName string,
) (string, error) {
	if policy == nil || strings.TrimSpace(string(policy.UID)) == "" {
		return "", errors.New("OutboundAccessPolicy UID is required for TokenRequest RBAC")
	}
	workerName := workerServiceAccountName(aiWorkerServiceAccountName, AIWorkerServiceAccount)
	digestInput := string(policy.UID) + "\x00" + workerName + "\x00" + strings.Join(serviceAccounts, "\x00")
	sum := sha256.Sum256([]byte(digestInput))
	suffix := hex.EncodeToString(sum[:])[:outboundTokenRequestRBACHashLength]
	prefixLength := maxOutboundTokenRequestRBACNameLength - len(suffix) - 1
	prefix := strings.TrimRight((outboundTokenRequestRBACPrefix + policy.Name)[:min(len(outboundTokenRequestRBACPrefix+policy.Name), prefixLength)], "-.")
	if prefix == "" {
		prefix = strings.TrimSuffix(outboundTokenRequestRBACPrefix, "-")
	}
	return prefix + "-" + suffix, nil
}

func outboundTokenRequestObjectMetadata(policy *corev1alpha1.OutboundAccessPolicy) (map[string]string, map[string]string) {
	return map[string]string{
		outboundTokenRequestManagedByLabelKey: outboundTokenRequestManagedByLabelValue,
	}, map[string]string{
		outboundTokenRequestPolicyAnnotationKey:    policy.Name,
		outboundTokenRequestPolicyUIDAnnotationKey: string(policy.UID),
	}
}

func outboundTokenRequestControlledBy(policy *corev1alpha1.OutboundAccessPolicy, object metav1.Object) bool {
	return policy != nil && object != nil && metav1.IsControlledBy(object, policy)
}

func outboundTokenRequestRoleEqual(
	policy *corev1alpha1.OutboundAccessPolicy,
	current *rbacv1.Role,
	desired *rbacv1.Role,
) bool {
	return current != nil && desired != nil && outboundTokenRequestControlledBy(policy, current) &&
		reflect.DeepEqual(current.Rules, desired.Rules) &&
		reflect.DeepEqual(current.Labels, desired.Labels) &&
		reflect.DeepEqual(current.Annotations, desired.Annotations) &&
		reflect.DeepEqual(current.OwnerReferences, desired.OwnerReferences)
}

func outboundTokenRequestRoleBindingEqual(
	policy *corev1alpha1.OutboundAccessPolicy,
	current *rbacv1.RoleBinding,
	desired *rbacv1.RoleBinding,
) bool {
	return current != nil && desired != nil && outboundTokenRequestControlledBy(policy, current) &&
		current.RoleRef == desired.RoleRef &&
		reflect.DeepEqual(current.Subjects, desired.Subjects) &&
		reflect.DeepEqual(current.Labels, desired.Labels) &&
		reflect.DeepEqual(current.Annotations, desired.Annotations) &&
		reflect.DeepEqual(current.OwnerReferences, desired.OwnerReferences)
}

func (r *OutboundAccessPolicyReconciler) desiredOutboundTokenRequestGrant(
	policy *corev1alpha1.OutboundAccessPolicy,
	serviceAccounts []string,
) (*rbacv1.Role, *rbacv1.RoleBinding, error) {
	if r.Scheme == nil {
		return nil, nil, errors.New("outbound access policy reconciler scheme is required for TokenRequest RBAC")
	}
	name, err := outboundTokenRequestRBACName(policy, serviceAccounts, r.AIWorkerServiceAccountName)
	if err != nil {
		return nil, nil, err
	}
	roleLabels, roleAnnotations := outboundTokenRequestObjectMetadata(policy)
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   policy.Namespace,
			Labels:      roleLabels,
			Annotations: roleAnnotations,
		},
		Rules: []rbacv1.PolicyRule{{
			APIGroups:     []string{""},
			Resources:     []string{"serviceaccounts/token"},
			ResourceNames: append([]string(nil), serviceAccounts...),
			Verbs:         []string{"create"},
		}},
	}
	if err := controllerutil.SetControllerReference(policy, role, r.Scheme); err != nil {
		return nil, nil, fmt.Errorf("set TokenRequest Role owner: %w", err)
	}

	bindingLabels, bindingAnnotations := outboundTokenRequestObjectMetadata(policy)
	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   policy.Namespace,
			Labels:      bindingLabels,
			Annotations: bindingAnnotations,
		},
		RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: name},
		Subjects: []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Name:      workerServiceAccountName(r.AIWorkerServiceAccountName, AIWorkerServiceAccount),
			Namespace: policy.Namespace,
		}},
	}
	if err := controllerutil.SetControllerReference(policy, binding, r.Scheme); err != nil {
		return nil, nil, fmt.Errorf("set TokenRequest RoleBinding owner: %w", err)
	}
	return role, binding, nil
}

func (r *OutboundAccessPolicyReconciler) reconcileOutboundTokenRequestGrant(
	ctx context.Context,
	policy *corev1alpha1.OutboundAccessPolicy,
	serviceAccounts []string,
) (bool, error) {
	desiredRole, desiredBinding, err := r.desiredOutboundTokenRequestGrant(policy, serviceAccounts)
	if err != nil {
		return false, err
	}
	return r.reconcileOutboundTokenRequestObjects(ctx, policy, desiredRole, desiredBinding)
}

func (r *OutboundAccessPolicyReconciler) revokeOutboundTokenRequestGrant(
	ctx context.Context,
	policy *corev1alpha1.OutboundAccessPolicy,
) (bool, error) {
	return r.reconcileOutboundTokenRequestObjects(ctx, policy, nil, nil)
}

func (r *OutboundAccessPolicyReconciler) reconcileOutboundTokenRequestObjects(
	ctx context.Context,
	policy *corev1alpha1.OutboundAccessPolicy,
	desiredRole *rbacv1.Role,
	desiredBinding *rbacv1.RoleBinding,
) (bool, error) {
	roles, bindings, err := r.listOutboundTokenRequestObjects(ctx, policy.Namespace)
	if err != nil {
		return false, err
	}
	desiredCurrentRole, desiredCurrentBinding, collisionErr := currentOutboundTokenRequestObjects(
		policy,
		roles,
		bindings,
		desiredRole,
	)
	roleReady := desiredRole != nil && collisionErr == nil && outboundTokenRequestRoleEqual(policy, desiredCurrentRole, desiredRole)
	bindingReady := desiredBinding != nil && collisionErr == nil && roleReady && outboundTokenRequestRoleBindingEqual(policy, desiredCurrentBinding, desiredBinding)

	deleted, err := r.revokeStaleOutboundTokenRequestBindings(ctx, policy, bindings, desiredBinding, bindingReady)
	if err != nil || deleted {
		return false, err
	}
	deleted, err = r.revokeStaleOutboundTokenRequestRoles(ctx, policy, roles, desiredRole, roleReady)
	if err != nil || deleted {
		return false, err
	}
	if collisionErr != nil {
		return false, collisionErr
	}
	if desiredRole == nil || desiredBinding == nil {
		return true, nil
	}
	created := false
	if desiredCurrentRole == nil {
		if err := r.Create(ctx, desiredRole); err != nil {
			return false, fmt.Errorf("create TokenRequest Role %s/%s: %w", desiredRole.Namespace, desiredRole.Name, err)
		}
		roleReady = true
		created = true
	}
	if desiredCurrentBinding == nil {
		if !roleReady {
			return false, errors.New("TokenRequest Role is not ready for binding")
		}
		if err := r.Create(ctx, desiredBinding); err != nil {
			return false, fmt.Errorf("create TokenRequest RoleBinding %s/%s: %w", desiredBinding.Namespace, desiredBinding.Name, err)
		}
		created = true
	}
	if created {
		return false, nil
	}
	return true, nil
}

func (r *OutboundAccessPolicyReconciler) listOutboundTokenRequestObjects(
	ctx context.Context,
	namespace string,
) ([]rbacv1.Role, []rbacv1.RoleBinding, error) {
	reader, err := r.tokenRequestAuthorizationReader()
	if err != nil {
		return nil, nil, err
	}
	bindings := &rbacv1.RoleBindingList{}
	if err := reader.List(ctx, bindings, client.InNamespace(namespace)); err != nil {
		return nil, nil, fmt.Errorf("list TokenRequest RoleBindings in namespace %s: %w", namespace, err)
	}
	roles := &rbacv1.RoleList{}
	if err := reader.List(ctx, roles, client.InNamespace(namespace)); err != nil {
		return nil, nil, fmt.Errorf("list TokenRequest Roles in namespace %s: %w", namespace, err)
	}
	return roles.Items, bindings.Items, nil
}

func currentOutboundTokenRequestObjects(
	policy *corev1alpha1.OutboundAccessPolicy,
	roles []rbacv1.Role,
	bindings []rbacv1.RoleBinding,
	desiredRole *rbacv1.Role,
) (*rbacv1.Role, *rbacv1.RoleBinding, error) {
	if desiredRole == nil {
		return nil, nil, nil
	}
	var currentRole *rbacv1.Role
	for i := range roles {
		if roles[i].Name == desiredRole.Name {
			currentRole = &roles[i]
			break
		}
	}
	var currentBinding *rbacv1.RoleBinding
	for i := range bindings {
		if bindings[i].Name == desiredRole.Name {
			currentBinding = &bindings[i]
			break
		}
	}
	var collisionErr error
	if currentRole != nil && !outboundTokenRequestControlledBy(policy, currentRole) {
		collisionErr = fmt.Errorf("TokenRequest Role %s/%s exists but is not controlled by OutboundAccessPolicy UID %s", currentRole.Namespace, currentRole.Name, policy.UID)
	}
	if currentBinding != nil && !outboundTokenRequestControlledBy(policy, currentBinding) {
		collisionErr = errors.Join(collisionErr, fmt.Errorf("TokenRequest RoleBinding %s/%s exists but is not controlled by OutboundAccessPolicy UID %s", currentBinding.Namespace, currentBinding.Name, policy.UID))
	}
	return currentRole, currentBinding, collisionErr
}

func (r *OutboundAccessPolicyReconciler) revokeStaleOutboundTokenRequestBindings(
	ctx context.Context,
	policy *corev1alpha1.OutboundAccessPolicy,
	bindings []rbacv1.RoleBinding,
	desired *rbacv1.RoleBinding,
	desiredReady bool,
) (bool, error) {
	deleted := false
	for i := range bindings {
		binding := &bindings[i]
		if !outboundTokenRequestControlledBy(policy, binding) {
			continue
		}
		if desired != nil && binding.Name == desired.Name && desiredReady {
			continue
		}
		if err := r.Delete(ctx, binding); err != nil && !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("revoke TokenRequest RoleBinding %s/%s: %w", binding.Namespace, binding.Name, err)
		}
		deleted = true
	}
	return deleted, nil
}

func (r *OutboundAccessPolicyReconciler) revokeStaleOutboundTokenRequestRoles(
	ctx context.Context,
	policy *corev1alpha1.OutboundAccessPolicy,
	roles []rbacv1.Role,
	desired *rbacv1.Role,
	desiredReady bool,
) (bool, error) {
	deleted := false
	for i := range roles {
		role := &roles[i]
		if !outboundTokenRequestControlledBy(policy, role) {
			continue
		}
		if desired != nil && role.Name == desired.Name && desiredReady {
			continue
		}
		if err := r.Delete(ctx, role); err != nil && !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("delete TokenRequest Role %s/%s: %w", role.Namespace, role.Name, err)
		}
		deleted = true
	}
	return deleted, nil
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

// SetupWithManager registers policy, referenced-object, and managed-RBAC watches.
func (r *OutboundAccessPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}
	mapReferenced := handler.EnqueueRequestsFromMapFunc(r.requestsForReferencedObject)
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.OutboundAccessPolicy{}).
		Owns(&rbacv1.Role{}).
		Owns(&rbacv1.RoleBinding{}).
		Watches(&corev1.Secret{}, mapReferenced).
		Watches(&corev1.Service{}, mapReferenced).
		Watches(&corev1.ServiceAccount{}, mapReferenced).
		Named("outboundaccesspolicy").
		Complete(r)
}
