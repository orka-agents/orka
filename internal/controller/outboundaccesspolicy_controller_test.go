/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/approvals"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/outboundaccess"
	"github.com/orka-agents/orka/internal/tokenexchange"
)

func TestOutboundAccessPolicyReconcilerConditions(t *testing.T) {
	tests := []struct {
		name         string
		policy       *corev1alpha1.OutboundAccessPolicy
		objects      []runtime.Object
		wantAccepted metav1.ConditionStatus
		wantResolved metav1.ConditionStatus
	}{
		{
			name: "invalid adapters",
			policy: &corev1alpha1.OutboundAccessPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: "policy", Namespace: "tenant", Generation: 1},
			},
			wantAccepted: metav1.ConditionFalse,
			wantResolved: metav1.ConditionFalse,
		},
		{
			name:         "missing secret",
			policy:       controllerDirectPolicy("tenant", "policy", "subject"),
			wantAccepted: metav1.ConditionTrue,
			wantResolved: metav1.ConditionFalse,
		},
		{
			name:   "resolved",
			policy: controllerDirectPolicy("tenant", "policy", "subject"),
			objects: []runtime.Object{&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "subject", Namespace: "tenant"},
				Data:       map[string][]byte{"token": []byte("assertion")},
			}},
			wantAccepted: metav1.ConditionTrue,
			wantResolved: metav1.ConditionTrue,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			if err := corev1alpha1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			if err := rbacv1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			objects := []runtime.Object{tt.policy}
			objects = append(objects, tt.objects...)
			client := ctrlfake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objects...).WithStatusSubresource(&corev1alpha1.OutboundAccessPolicy{}).Build()
			reconciler := &OutboundAccessPolicyReconciler{Client: client, APIReader: client, Scheme: scheme}
			if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "tenant", Name: "policy"}}); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			updated := &corev1alpha1.OutboundAccessPolicy{}
			if err := client.Get(context.Background(), types.NamespacedName{Namespace: "tenant", Name: "policy"}, updated); err != nil {
				t.Fatal(err)
			}
			if updated.Status.ObservedGeneration != updated.Generation {
				t.Fatalf("observedGeneration = %d, want %d", updated.Status.ObservedGeneration, updated.Generation)
			}
			accepted := meta.FindStatusCondition(updated.Status.Conditions, corev1alpha1.OutboundAccessPolicyConditionAccepted)
			resolved := meta.FindStatusCondition(updated.Status.Conditions, corev1alpha1.OutboundAccessPolicyConditionResolvedRefs)
			if accepted == nil || accepted.Status != tt.wantAccepted {
				t.Fatalf("Accepted = %#v, want %s", accepted, tt.wantAccepted)
			}
			if resolved == nil || resolved.Status != tt.wantResolved {
				t.Fatalf("ResolvedRefs = %#v, want %s", resolved, tt.wantResolved)
			}
			if len(updated.Status.Conditions) != 2 {
				t.Fatalf("conditions = %#v, want only Accepted and ResolvedRefs", updated.Status.Conditions)
			}
			statusJSON, err := json.Marshal(updated.Status)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(statusJSON), "assertion") || strings.Contains(string(statusJSON), "https://issuer.example.test") {
				t.Fatalf("status leaked credential or endpoint: %s", statusJSON)
			}
		})
	}
}

func controllerDirectPolicy(namespace, name, secretName string) *corev1alpha1.OutboundAccessPolicy {
	return &corev1alpha1.OutboundAccessPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Generation: 1},
		Spec: corev1alpha1.OutboundAccessPolicySpec{Direct: &corev1alpha1.DirectOutboundAccess{
			Grant:         corev1alpha1.OutboundGrantTokenExchange,
			TokenEndpoint: corev1alpha1.OutboundTokenEndpoint{URL: "https://issuer.example.test/token"},
			Subject: corev1alpha1.OutboundTokenSource{
				Source:    corev1alpha1.OutboundTokenSourceSecretRef,
				TokenType: tokenexchange.TokenTypeAccessToken,
				SecretRef: &corev1alpha1.NamespacedSecretKeySelector{Name: secretName, Key: "token"},
			},
			ExpectedIssuedTokenType: tokenexchange.TokenTypeAccessToken,
		}},
	}
}

func controllerServiceAccountPolicy(name string, serviceAccounts ...string) *corev1alpha1.OutboundAccessPolicy {
	policy := &corev1alpha1.OutboundAccessPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  "tenant",
			UID:        types.UID(name + "-uid"),
			Generation: 1,
		},
		Spec: corev1alpha1.OutboundAccessPolicySpec{Direct: &corev1alpha1.DirectOutboundAccess{
			Grant:                   corev1alpha1.OutboundGrantTokenExchange,
			TokenEndpoint:           corev1alpha1.OutboundTokenEndpoint{URL: "https://issuer.example.test/oauth/token"},
			ExpectedIssuedTokenType: tokenexchange.TokenTypeAccessToken,
		}},
	}
	if len(serviceAccounts) > 0 {
		policy.Spec.Direct.Subject = corev1alpha1.OutboundTokenSource{
			Source:            corev1alpha1.OutboundTokenSourceServiceAccount,
			ServiceAccountRef: &corev1alpha1.OutboundServiceAccountReference{Name: serviceAccounts[0]},
		}
	}
	if len(serviceAccounts) > 1 {
		policy.Spec.Direct.Actor = &corev1alpha1.OutboundTokenSource{
			Source:            corev1alpha1.OutboundTokenSourceServiceAccount,
			ServiceAccountRef: &corev1alpha1.OutboundServiceAccountReference{Name: serviceAccounts[1]},
		}
	}
	return policy
}

func outboundPolicyControllerTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := rbacv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func outboundTokenRequestTestKey(
	t *testing.T,
	policy *corev1alpha1.OutboundAccessPolicy,
	aiWorkerServiceAccountName string,
) types.NamespacedName {
	t.Helper()
	name, err := outboundTokenRequestRBACName(
		policy,
		outboundTokenRequestServiceAccountNames(policy),
		aiWorkerServiceAccountName,
	)
	if err != nil {
		t.Fatal(err)
	}
	return types.NamespacedName{Namespace: policy.Namespace, Name: name}
}

func reconcileOutboundPolicy(t *testing.T, reconciler *OutboundAccessPolicyReconciler, request ctrl.Request, attempts int) {
	t.Helper()
	for range attempts {
		if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
	}
}

func TestOutboundAccessPolicyReconcilerRestrictsTokenRequestsToReferencedServiceAccounts(t *testing.T) {
	scheme := outboundPolicyControllerTestScheme(t)
	policy := controllerServiceAccountPolicy("workload-token", "subject-sa", "actor-sa")
	subject := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "subject-sa", Namespace: policy.Namespace}}
	actor := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "actor-sa", Namespace: policy.Namespace}}
	client := ctrlfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(policy, subject, actor).
		WithStatusSubresource(&corev1alpha1.OutboundAccessPolicy{}).
		Build()
	reconciler := &OutboundAccessPolicyReconciler{
		Client:                     client,
		APIReader:                  client,
		Scheme:                     scheme,
		AIWorkerServiceAccountName: testAIWorkerServiceAccountName,
	}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: policy.Namespace, Name: policy.Name}}
	reconcileOutboundPolicy(t, reconciler, request, 1)
	interim := &corev1alpha1.OutboundAccessPolicy{}
	if err := client.Get(context.Background(), request.NamespacedName, interim); err != nil {
		t.Fatal(err)
	}
	if !controllerutil.ContainsFinalizer(interim, outboundTokenRequestRBACFinalizer) {
		t.Fatalf("policy finalizers = %#v", interim.Finalizers)
	}
	if condition := meta.FindStatusCondition(interim.Status.Conditions, corev1alpha1.OutboundAccessPolicyConditionResolvedRefs); condition != nil && condition.Status == metav1.ConditionTrue {
		t.Fatalf("ResolvedRefs became true before TokenRequest binding existed: %#v", condition)
	}
	reconcileOutboundPolicy(t, reconciler, request, 1)

	key := outboundTokenRequestTestKey(t, policy, reconciler.AIWorkerServiceAccountName)
	role := &rbacv1.Role{}
	if err := client.Get(context.Background(), key, role); err != nil {
		t.Fatalf("get TokenRequest Role: %v", err)
	}
	if len(role.Rules) != 1 || !slices.Equal(role.Rules[0].Resources, []string{"serviceaccounts/token"}) ||
		!slices.Equal(role.Rules[0].ResourceNames, []string{"actor-sa", "subject-sa"}) ||
		!slices.Equal(role.Rules[0].Verbs, []string{"create"}) {
		t.Fatalf("TokenRequest Role rules = %#v", role.Rules)
	}
	if !metav1.IsControlledBy(role, policy) {
		t.Fatalf("TokenRequest Role ownerReferences = %#v", role.OwnerReferences)
	}

	binding := &rbacv1.RoleBinding{}
	if err := client.Get(context.Background(), key, binding); err != nil {
		t.Fatalf("get TokenRequest RoleBinding: %v", err)
	}
	if binding.RoleRef != (rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: key.Name}) {
		t.Fatalf("TokenRequest RoleBinding roleRef = %#v", binding.RoleRef)
	}
	if len(binding.Subjects) != 1 || binding.Subjects[0].Name != testAIWorkerServiceAccountName || binding.Subjects[0].Namespace != policy.Namespace {
		t.Fatalf("TokenRequest RoleBinding subjects = %#v", binding.Subjects)
	}
	if !metav1.IsControlledBy(binding, policy) {
		t.Fatalf("TokenRequest RoleBinding ownerReferences = %#v", binding.OwnerReferences)
	}
	reconcileOutboundPolicy(t, reconciler, request, 1)
	ready := &corev1alpha1.OutboundAccessPolicy{}
	if err := client.Get(context.Background(), request.NamespacedName, ready); err != nil {
		t.Fatal(err)
	}
	resolved := meta.FindStatusCondition(ready.Status.Conditions, corev1alpha1.OutboundAccessPolicyConditionResolvedRefs)
	if resolved == nil || resolved.Status != metav1.ConditionTrue || resolved.ObservedGeneration != ready.Generation {
		t.Fatalf("ResolvedRefs = %#v, want current True after uncached grant inventory", resolved)
	}
}

func TestOutboundAccessPolicyReconcilerRevokesTokenRequestsWhenReferenceBreaks(t *testing.T) {
	scheme := outboundPolicyControllerTestScheme(t)
	policy := controllerServiceAccountPolicy("workload-token", "subject-sa")
	subject := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "subject-sa", Namespace: policy.Namespace}}
	client := ctrlfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(policy, subject).
		WithStatusSubresource(&corev1alpha1.OutboundAccessPolicy{}).
		Build()
	reconciler := &OutboundAccessPolicyReconciler{Client: client, APIReader: client, Scheme: scheme}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: policy.Namespace, Name: policy.Name}}
	reconcileOutboundPolicy(t, reconciler, request, 2)
	if err := client.Delete(context.Background(), subject); err != nil {
		t.Fatal(err)
	}
	reconcileOutboundPolicy(t, reconciler, request, 3)

	key := outboundTokenRequestTestKey(t, policy, reconciler.AIWorkerServiceAccountName)
	if err := client.Get(context.Background(), key, &rbacv1.RoleBinding{}); !apierrors.IsNotFound(err) {
		t.Fatalf("TokenRequest RoleBinding remained after reference failure: %v", err)
	}
	if err := client.Get(context.Background(), key, &rbacv1.Role{}); !apierrors.IsNotFound(err) {
		t.Fatalf("TokenRequest Role remained after reference failure: %v", err)
	}
	updated := &corev1alpha1.OutboundAccessPolicy{}
	if err := client.Get(context.Background(), request.NamespacedName, updated); err != nil {
		t.Fatal(err)
	}
	resolved := meta.FindStatusCondition(updated.Status.Conditions, corev1alpha1.OutboundAccessPolicyConditionResolvedRefs)
	if resolved == nil || resolved.Status != metav1.ConditionFalse {
		t.Fatalf("ResolvedRefs = %#v, want false", resolved)
	}
}

func TestOutboundAccessPolicyReconcilerDoesNotOverwriteUnmanagedTokenRequestRole(t *testing.T) {
	scheme := outboundPolicyControllerTestScheme(t)
	policy := controllerServiceAccountPolicy("workload-token", "subject-sa")
	subject := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "subject-sa", Namespace: policy.Namespace}}
	key := outboundTokenRequestTestKey(t, policy, "")
	unmanaged := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
		Rules:      []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get"}}},
	}
	client := ctrlfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(policy, subject, unmanaged).
		WithStatusSubresource(&corev1alpha1.OutboundAccessPolicy{}).
		Build()
	reconciler := &OutboundAccessPolicyReconciler{Client: client, APIReader: client, Scheme: scheme}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: policy.Namespace, Name: policy.Name}}
	reconcileOutboundPolicy(t, reconciler, request, 1)
	if _, err := reconciler.Reconcile(context.Background(), request); err == nil || !strings.Contains(err.Error(), "not controlled") {
		t.Fatalf("Reconcile() error = %v, want unmanaged collision", err)
	}
	current := &rbacv1.Role{}
	if err := client.Get(context.Background(), key, current); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(current.Rules, unmanaged.Rules) {
		t.Fatalf("unmanaged Role was overwritten: %#v", current.Rules)
	}
	if err := client.Get(context.Background(), key, &rbacv1.RoleBinding{}); !apierrors.IsNotFound(err) {
		t.Fatalf("RoleBinding created for unmanaged Role collision: %v", err)
	}
}

func TestOutboundAccessPolicyReconcilerKeepsResolvedRefsUnknownUntilObsoleteGrantIsRevoked(t *testing.T) {
	scheme := outboundPolicyControllerTestScheme(t)
	policy := controllerServiceAccountPolicy("workload-token", "subject-sa")
	subject := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "subject-sa", Namespace: policy.Namespace}}
	baseClient := ctrlfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(policy, subject).
		WithStatusSubresource(&corev1alpha1.OutboundAccessPolicy{}).
		Build()
	reconciler := &OutboundAccessPolicyReconciler{Client: baseClient, APIReader: baseClient, Scheme: scheme}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: policy.Namespace, Name: policy.Name}}
	reconcileOutboundPolicy(t, reconciler, request, 2)
	oldKey := outboundTokenRequestTestKey(t, policy, reconciler.AIWorkerServiceAccountName)

	updated := &corev1alpha1.OutboundAccessPolicy{}
	if err := baseClient.Get(context.Background(), request.NamespacedName, updated); err != nil {
		t.Fatal(err)
	}
	updated.Spec.Direct.Subject = corev1alpha1.OutboundTokenSource{Source: corev1alpha1.OutboundTokenSourceTransactionToken}
	updated.Generation++
	if err := baseClient.Update(context.Background(), updated); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if err := baseClient.Get(context.Background(), request.NamespacedName, updated); err != nil {
		t.Fatal(err)
	}
	resolved := meta.FindStatusCondition(updated.Status.Conditions, corev1alpha1.OutboundAccessPolicyConditionResolvedRefs)
	if resolved == nil || resolved.Status != metav1.ConditionUnknown || resolved.ObservedGeneration != updated.Generation {
		t.Fatalf("ResolvedRefs = %#v, want current Unknown during revocation", resolved)
	}
	if err := baseClient.Get(context.Background(), oldKey, &rbacv1.RoleBinding{}); !apierrors.IsNotFound(err) {
		t.Fatalf("obsolete RoleBinding remained while policy was marked unready: %v", err)
	}
	if err := baseClient.Get(context.Background(), oldKey, &rbacv1.Role{}); err != nil {
		t.Fatalf("obsolete Role should remain until binding deletion is observed: %v", err)
	}
}

func TestOutboundAccessPolicyReconcilerUsesAPIReaderForGrantInventory(t *testing.T) {
	scheme := outboundPolicyControllerTestScheme(t)
	policy := controllerServiceAccountPolicy("workload-token", "old-sa")
	oldServiceAccount := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "old-sa", Namespace: policy.Namespace}}
	newServiceAccount := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "new-sa", Namespace: policy.Namespace}}
	baseClient := ctrlfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(policy, oldServiceAccount, newServiceAccount).
		WithStatusSubresource(&corev1alpha1.OutboundAccessPolicy{}).
		Build()
	cachedRBACList := false
	cachedClient := interceptor.NewClient(baseClient, interceptor.Funcs{
		List: func(ctx context.Context, c ctrlclient.WithWatch, list ctrlclient.ObjectList, opts ...ctrlclient.ListOption) error {
			switch list.(type) {
			case *rbacv1.RoleList, *rbacv1.RoleBindingList:
				cachedRBACList = true
				return nil
			default:
				return c.List(ctx, list, opts...)
			}
		},
	})
	reconciler := &OutboundAccessPolicyReconciler{Client: cachedClient, APIReader: baseClient, Scheme: scheme}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: policy.Namespace, Name: policy.Name}}
	reconcileOutboundPolicy(t, reconciler, request, 2)
	oldKey := outboundTokenRequestTestKey(t, policy, reconciler.AIWorkerServiceAccountName)

	updated := &corev1alpha1.OutboundAccessPolicy{}
	if err := baseClient.Get(context.Background(), request.NamespacedName, updated); err != nil {
		t.Fatal(err)
	}
	updated.Spec.Direct.Subject.ServiceAccountRef.Name = newServiceAccount.Name
	updated.Generation++
	if err := baseClient.Update(context.Background(), updated); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if cachedRBACList {
		t.Fatal("TokenRequest grant inventory used the cached controller client")
	}
	if err := baseClient.Get(context.Background(), oldKey, &rbacv1.RoleBinding{}); !apierrors.IsNotFound(err) {
		t.Fatalf("obsolete RoleBinding was missed by grant inventory: %v", err)
	}
}

func TestOutboundAccessPolicyReconcilerRevokesBeforeChangingServiceAccountGrant(t *testing.T) {
	scheme := outboundPolicyControllerTestScheme(t)
	policy := controllerServiceAccountPolicy("workload-token", "old-sa")
	oldServiceAccount := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "old-sa", Namespace: policy.Namespace}}
	newServiceAccount := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "new-sa", Namespace: policy.Namespace}}
	client := ctrlfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(policy, oldServiceAccount, newServiceAccount).
		WithStatusSubresource(&corev1alpha1.OutboundAccessPolicy{}).
		Build()
	reconciler := &OutboundAccessPolicyReconciler{Client: client, APIReader: client, Scheme: scheme}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: policy.Namespace, Name: policy.Name}}
	reconcileOutboundPolicy(t, reconciler, request, 2)
	oldKey := outboundTokenRequestTestKey(t, policy, reconciler.AIWorkerServiceAccountName)

	updated := &corev1alpha1.OutboundAccessPolicy{}
	if err := client.Get(context.Background(), request.NamespacedName, updated); err != nil {
		t.Fatal(err)
	}
	updated.Spec.Direct.Subject.ServiceAccountRef.Name = newServiceAccount.Name
	updated.Generation++
	if err := client.Update(context.Background(), updated); err != nil {
		t.Fatal(err)
	}
	newKey := outboundTokenRequestTestKey(t, updated, reconciler.AIWorkerServiceAccountName)
	if newKey == oldKey {
		t.Fatalf("TokenRequest RBAC name did not change: %s", newKey)
	}

	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("Reconcile() during grant change error = %v", err)
	}
	if err := client.Get(context.Background(), oldKey, &rbacv1.RoleBinding{}); !apierrors.IsNotFound(err) {
		t.Fatalf("old RoleBinding remained during grant change: %v", err)
	}
	if err := client.Get(context.Background(), oldKey, &rbacv1.Role{}); err != nil {
		t.Fatalf("old Role should remain until its binding is revoked: %v", err)
	}
	if err := client.Get(context.Background(), newKey, &rbacv1.RoleBinding{}); !apierrors.IsNotFound(err) {
		t.Fatalf("new RoleBinding was created before old Role cleanup: %v", err)
	}

	reconcileOutboundPolicy(t, reconciler, request, 2)
	if err := client.Get(context.Background(), oldKey, &rbacv1.Role{}); !apierrors.IsNotFound(err) {
		t.Fatalf("old Role remained after grant change: %v", err)
	}
	newRole := &rbacv1.Role{}
	if err := client.Get(context.Background(), newKey, newRole); err != nil {
		t.Fatalf("new Role missing: %v", err)
	}
	if len(newRole.Rules) != 1 || !slices.Equal(newRole.Rules[0].ResourceNames, []string{"new-sa"}) {
		t.Fatalf("new Role rules = %#v", newRole.Rules)
	}
	if err := client.Get(context.Background(), newKey, &rbacv1.RoleBinding{}); err != nil {
		t.Fatalf("new RoleBinding missing: %v", err)
	}
}

func TestOutboundAccessPolicyReconcilerFinalizerRevokesGrantBeforeDeletion(t *testing.T) {
	scheme := outboundPolicyControllerTestScheme(t)
	policy := controllerServiceAccountPolicy("workload-token", "subject-sa")
	subject := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "subject-sa", Namespace: policy.Namespace}}
	client := ctrlfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(policy, subject).
		WithStatusSubresource(&corev1alpha1.OutboundAccessPolicy{}).
		Build()
	reconciler := &OutboundAccessPolicyReconciler{Client: client, APIReader: client, Scheme: scheme}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: policy.Namespace, Name: policy.Name}}
	reconcileOutboundPolicy(t, reconciler, request, 2)
	key := outboundTokenRequestTestKey(t, policy, reconciler.AIWorkerServiceAccountName)

	current := &corev1alpha1.OutboundAccessPolicy{}
	if err := client.Get(context.Background(), request.NamespacedName, current); err != nil {
		t.Fatal(err)
	}
	if !controllerutil.ContainsFinalizer(current, outboundTokenRequestRBACFinalizer) {
		t.Fatalf("policy finalizers = %#v", current.Finalizers)
	}
	if err := client.Delete(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	if err := client.Get(context.Background(), request.NamespacedName, current); err != nil {
		t.Fatalf("policy disappeared before finalization: %v", err)
	}

	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if err := client.Get(context.Background(), key, &rbacv1.RoleBinding{}); !apierrors.IsNotFound(err) {
		t.Fatalf("RoleBinding remained after deletion reconciliation: %v", err)
	}
	if err := client.Get(context.Background(), key, &rbacv1.Role{}); err != nil {
		t.Fatalf("Role should remain until binding revocation is observed: %v", err)
	}
	if err := client.Get(context.Background(), request.NamespacedName, current); err != nil {
		t.Fatalf("policy disappeared before Role cleanup: %v", err)
	}

	reconcileOutboundPolicy(t, reconciler, request, 2)
	if err := client.Get(context.Background(), key, &rbacv1.Role{}); !apierrors.IsNotFound(err) {
		t.Fatalf("Role remained after deletion finalization: %v", err)
	}
	if err := client.Get(context.Background(), request.NamespacedName, &corev1alpha1.OutboundAccessPolicy{}); !apierrors.IsNotFound(err) {
		t.Fatalf("policy remained after finalizer cleanup: %v", err)
	}
}

func TestOutboundAccessPolicyReconcilerRepairsDriftByRevokingBindingFirst(t *testing.T) {
	scheme := outboundPolicyControllerTestScheme(t)
	policy := controllerServiceAccountPolicy("workload-token", "subject-sa")
	subject := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "subject-sa", Namespace: policy.Namespace}}
	baseClient := ctrlfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(policy, subject).
		WithStatusSubresource(&corev1alpha1.OutboundAccessPolicy{}).
		Build()
	reconciler := &OutboundAccessPolicyReconciler{Client: baseClient, APIReader: baseClient, Scheme: scheme}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: policy.Namespace, Name: policy.Name}}
	reconcileOutboundPolicy(t, reconciler, request, 2)
	key := outboundTokenRequestTestKey(t, policy, reconciler.AIWorkerServiceAccountName)

	role := &rbacv1.Role{}
	if err := baseClient.Get(context.Background(), key, role); err != nil {
		t.Fatal(err)
	}
	role.Rules = append(role.Rules, rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get"}})
	if err := baseClient.Update(context.Background(), role); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if err := baseClient.Get(context.Background(), key, &rbacv1.RoleBinding{}); !apierrors.IsNotFound(err) {
		t.Fatalf("drifted RoleBinding was not revoked first: %v", err)
	}
	if err := baseClient.Get(context.Background(), key, &rbacv1.Role{}); err != nil {
		t.Fatalf("drifted Role should remain until binding revocation is observed: %v", err)
	}

	reconcileOutboundPolicy(t, reconciler, request, 2)
	repaired := &rbacv1.Role{}
	if err := baseClient.Get(context.Background(), key, repaired); err != nil {
		t.Fatal(err)
	}
	if len(repaired.Rules) != 1 || !slices.Equal(repaired.Rules[0].ResourceNames, []string{"subject-sa"}) {
		t.Fatalf("repaired Role rules = %#v", repaired.Rules)
	}
	if err := baseClient.Get(context.Background(), key, &rbacv1.RoleBinding{}); err != nil {
		t.Fatalf("repaired RoleBinding missing: %v", err)
	}
}

func TestOutboundAccessPolicyReconcilerKeepsFinalizerWhenRevocationFails(t *testing.T) {
	scheme := outboundPolicyControllerTestScheme(t)
	policy := controllerServiceAccountPolicy("workload-token", "subject-sa")
	subject := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "subject-sa", Namespace: policy.Namespace}}
	baseClient := ctrlfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(policy, subject).
		WithStatusSubresource(&corev1alpha1.OutboundAccessPolicy{}).
		Build()
	reconciler := &OutboundAccessPolicyReconciler{Client: baseClient, APIReader: baseClient, Scheme: scheme}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: policy.Namespace, Name: policy.Name}}
	reconcileOutboundPolicy(t, reconciler, request, 2)

	current := &corev1alpha1.OutboundAccessPolicy{}
	if err := baseClient.Get(context.Background(), request.NamespacedName, current); err != nil {
		t.Fatal(err)
	}
	if err := baseClient.Delete(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	deleteErr := errors.New("injected RoleBinding delete failure")
	restrictedClient := interceptor.NewClient(baseClient, interceptor.Funcs{
		Delete: func(ctx context.Context, c ctrlclient.WithWatch, obj ctrlclient.Object, opts ...ctrlclient.DeleteOption) error {
			if _, ok := obj.(*rbacv1.RoleBinding); ok {
				return deleteErr
			}
			return c.Delete(ctx, obj, opts...)
		},
	})
	reconciler.Client = restrictedClient
	if _, err := reconciler.Reconcile(context.Background(), request); !errors.Is(err, deleteErr) {
		t.Fatalf("Reconcile() error = %v, want injected delete failure", err)
	}
	if err := baseClient.Get(context.Background(), request.NamespacedName, current); err != nil {
		t.Fatalf("policy disappeared after failed revocation: %v", err)
	}
	if !controllerutil.ContainsFinalizer(current, outboundTokenRequestRBACFinalizer) {
		t.Fatalf("policy finalizer was removed after failed revocation: %#v", current.Finalizers)
	}
}

func TestOutboundTokenRequestServiceAccountNamesDeduplicate(t *testing.T) {
	policy := controllerServiceAccountPolicy("workload-token", "same-sa", "same-sa")
	if got := outboundTokenRequestServiceAccountNames(policy); !slices.Equal(got, []string{"same-sa"}) {
		t.Fatalf("service account names = %#v", got)
	}
}

func TestOutboundTokenRequestRBACNameUsesPolicyUIDAndValidLength(t *testing.T) {
	policy := controllerServiceAccountPolicy(strings.Repeat("a", 253), "subject-sa")
	name, err := outboundTokenRequestRBACName(policy, []string{"subject-sa"}, testAIWorkerServiceAccountName)
	if err != nil {
		t.Fatal(err)
	}
	if len(name) > maxOutboundTokenRequestRBACNameLength || len(validation.IsDNS1123Subdomain(name)) != 0 {
		t.Fatalf("RBAC name = %q len=%d", name, len(name))
	}
	repeated, err := outboundTokenRequestRBACName(policy, []string{"subject-sa"}, testAIWorkerServiceAccountName)
	if err != nil || repeated != name {
		t.Fatalf("repeated name = %q, %v", repeated, err)
	}
	recreated := policy.DeepCopy()
	recreated.UID = "different-uid"
	recreatedName, err := outboundTokenRequestRBACName(recreated, []string{"subject-sa"}, testAIWorkerServiceAccountName)
	if err != nil {
		t.Fatal(err)
	}
	if recreatedName == name {
		t.Fatalf("same-name policy recreation reused RBAC name %q", name)
	}
}

func TestOutboundAccessPolicyReferenceMapping(t *testing.T) {
	policy := controllerDirectPolicy("tenant", "policy", "subject")
	if !outboundPolicyReferencesObject(policy, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "subject", Namespace: "tenant"}}) {
		t.Fatal("policy did not map its subject Secret")
	}
	if outboundPolicyReferencesObject(policy, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "subject", Namespace: "other"}}) {
		t.Fatal("policy mapped a cross-namespace Secret")
	}

	gateway := &corev1alpha1.OutboundAccessPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "gateway", Namespace: "tenant"},
		Spec: corev1alpha1.OutboundAccessPolicySpec{Gateway: &corev1alpha1.GatewayOutboundAccess{
			ServiceRef: corev1alpha1.OutboundServiceReference{Name: "agentgateway", Namespace: "infra", Port: 8080},
		}},
	}
	if !outboundPolicyReferencesObject(gateway, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "agentgateway", Namespace: "infra"}}) {
		t.Fatal("gateway policy did not map cross-namespace Service")
	}

	trusted, err := outboundaccess.ParseTrustedServiceReferences("infra/agentgateway:8080")
	if err != nil || !trusted.Allows(gateway.Spec.Gateway.ServiceRef, gateway.Namespace) {
		t.Fatalf("trusted gateway parse = %#v, %v", trusted, err)
	}
}

type outboundPolicyRoundTripFunc func(*http.Request) (*http.Response, error)

func (f outboundPolicyRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestToolReconcilerDirectPolicyHealthCheckAcceptsAuthenticationRequiredResponse(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	policy := readyControllerPolicy("tenant", "direct", corev1alpha1.OutboundAccessPolicySpec{Direct: &corev1alpha1.DirectOutboundAccess{}})
	client := ctrlfake.NewClientBuilder().WithScheme(scheme).WithObjects(policy).Build()
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		http.Error(w, "authentication required", http.StatusUnauthorized)
	}))
	defer server.Close()
	reconciler := &ToolReconciler{Client: client, HTTPClient: server.Client()}
	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "direct-tool", Namespace: "tenant"},
		Spec: corev1alpha1.ToolSpec{HTTP: &corev1alpha1.HTTPExecution{
			URL:                     server.URL,
			OutboundAccessPolicyRef: &corev1alpha1.LocalObjectReference{Name: policy.Name},
		}},
	}
	if err := reconciler.healthCheck(context.Background(), tool); err != nil {
		t.Fatalf("healthCheck() error = %v", err)
	}
	if !called {
		t.Fatal("direct-policy health check did not probe the endpoint")
	}
}

func TestToolReconcilerGatewayPolicySkipsDirectHealthCheck(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	gateway := readyControllerPolicy("tenant", "gateway", corev1alpha1.OutboundAccessPolicySpec{
		Gateway: &corev1alpha1.GatewayOutboundAccess{},
	})
	client := ctrlfake.NewClientBuilder().WithScheme(scheme).WithObjects(gateway).Build()
	called := false
	reconciler := &ToolReconciler{
		Client: client,
		HTTPClient: &http.Client{Transport: outboundPolicyRoundTripFunc(func(*http.Request) (*http.Response, error) {
			called = true
			return nil, context.Canceled
		})},
	}
	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "gateway-tool", Namespace: "tenant"},
		Spec: corev1alpha1.ToolSpec{HTTP: &corev1alpha1.HTTPExecution{
			URL:                     "https://downstream.example.test/resource",
			OutboundAccessPolicyRef: &corev1alpha1.LocalObjectReference{Name: gateway.Name},
		}},
	}
	if err := reconciler.healthCheck(context.Background(), tool); err != nil {
		t.Fatalf("healthCheck() error = %v", err)
	}
	if called {
		t.Fatal("gateway-backed Tool health check called the original downstream URL")
	}
}

func TestToolReconcilerRejectsTerminatingOutboundAccessPolicy(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	policy := readyControllerPolicy("tenant", "direct", corev1alpha1.OutboundAccessPolicySpec{Direct: &corev1alpha1.DirectOutboundAccess{}})
	now := metav1.Now()
	policy.DeletionTimestamp = &now
	policy.Finalizers = []string{outboundTokenRequestRBACFinalizer}
	client := ctrlfake.NewClientBuilder().WithScheme(scheme).WithObjects(policy).Build()
	reconciler := &ToolReconciler{Client: client, Scheme: scheme}
	tool := &corev1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{Name: "tool", Namespace: "tenant"}, Spec: corev1alpha1.ToolSpec{HTTP: &corev1alpha1.HTTPExecution{
		URL:                     "https://api.example.test/resource",
		OutboundAccessPolicyRef: &corev1alpha1.LocalObjectReference{Name: policy.Name},
	}}}
	if err := reconciler.validateToolHTTPAuth(context.Background(), tool); err == nil || !strings.Contains(err.Error(), "terminating") {
		t.Fatalf("validateToolHTTPAuth() error = %v, want terminating policy rejection", err)
	}
}

func TestToolReconcilerOutboundAccessPolicyAuthCompatibility(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	direct := readyControllerPolicy("tenant", "direct", corev1alpha1.OutboundAccessPolicySpec{Direct: &corev1alpha1.DirectOutboundAccess{}})
	gateway := readyControllerPolicy("tenant", "gateway", corev1alpha1.OutboundAccessPolicySpec{Gateway: &corev1alpha1.GatewayOutboundAccess{}})
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "auth", Namespace: "tenant"}, Data: map[string][]byte{"token": []byte("credential")}}
	client := ctrlfake.NewClientBuilder().WithScheme(scheme).WithObjects(direct, gateway, secret).Build()
	reconciler := &ToolReconciler{Client: client, Scheme: scheme}

	tool := &corev1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{Name: "tool", Namespace: "tenant"}, Spec: corev1alpha1.ToolSpec{HTTP: &corev1alpha1.HTTPExecution{
		AuthSecretRef:           &corev1alpha1.SecretKeySelector{Name: "auth", Key: "token"},
		OutboundAccessPolicyRef: &corev1alpha1.LocalObjectReference{Name: "direct"},
	}}}
	if err := reconciler.validateToolHTTPAuth(context.Background(), tool); err == nil || !strings.Contains(strings.ToLower(err.Error()), "cannot coexist") {
		t.Fatalf("direct validateToolHTTPAuth() error = %v", err)
	}
	tool.Spec.HTTP.OutboundAccessPolicyRef.Name = "gateway"
	if err := reconciler.validateToolHTTPAuth(context.Background(), tool); err != nil {
		t.Fatalf("gateway validateToolHTTPAuth() error = %v", err)
	}
}

func TestToolReconcilerRejectsPlaintextDirectToolURL(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	direct := readyControllerPolicy("tenant", "direct", corev1alpha1.OutboundAccessPolicySpec{Direct: &corev1alpha1.DirectOutboundAccess{}})
	client := ctrlfake.NewClientBuilder().WithScheme(scheme).WithObjects(direct).Build()
	reconciler := &ToolReconciler{Client: client}
	tool := &corev1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{Name: "tool", Namespace: "tenant"}, Spec: corev1alpha1.ToolSpec{HTTP: &corev1alpha1.HTTPExecution{URL: "http://api.example.test", OutboundAccessPolicyRef: &corev1alpha1.LocalObjectReference{Name: "direct"}}}}
	if err := reconciler.validateToolHTTPAuth(context.Background(), tool); err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("error = %v", err)
	}
}

//nolint:unparam // Namespace is explicit to mirror the namespaced API contract.
func readyControllerPolicy(namespace, name string, spec corev1alpha1.OutboundAccessPolicySpec) *corev1alpha1.OutboundAccessPolicy {
	generation := int64(2)
	return &corev1alpha1.OutboundAccessPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Generation: generation},
		Spec:       spec,
		Status: corev1alpha1.OutboundAccessPolicyStatus{
			ObservedGeneration: generation,
			Conditions: []metav1.Condition{
				{Type: corev1alpha1.OutboundAccessPolicyConditionAccepted, Status: metav1.ConditionTrue, ObservedGeneration: generation},
				{Type: corev1alpha1.OutboundAccessPolicyConditionResolvedRefs, Status: metav1.ConditionTrue, ObservedGeneration: generation},
			},
		},
	}
}

func TestHarnessBrokeredTransactionAuthorityReadsTaskOwnedSecret(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "remote-task",
			Namespace: "tenant",
			UID:       types.UID("task-uid"),
			Annotations: map[string]string{
				labels.AnnotationTransactionTokenSecret: "task-transaction",
			},
		},
		Spec: corev1alpha1.TaskSpec{Transaction: &corev1alpha1.TaskTransaction{
			Scope:  "api.read api.write",
			Scopes: []string{"api.read", "api.write"},
		}},
	}
	controller := true
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "task-transaction",
			Namespace: "tenant",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: corev1alpha1.GroupVersion.String(),
				Kind:       "Task",
				Name:       task.Name,
				UID:        task.UID,
				Controller: &controller,
			}},
		},
		Data: map[string][]byte{"token": []byte("task-scoped-token")},
	}
	client := ctrlfake.NewClientBuilder().WithScheme(scheme).WithObjects(task, secret).Build()
	reconciler := &TaskReconciler{Client: client, APIReader: client}
	token, scopes, err := reconciler.harnessBrokeredTransactionAuthority(context.Background(), task)
	if err != nil {
		t.Fatalf("harnessBrokeredTransactionAuthority() error = %v", err)
	}
	if token != "task-scoped-token" || len(scopes) != 2 {
		t.Fatalf("authority = token %q scopes %#v", token, scopes)
	}

	secret.OwnerReferences[0].UID = types.UID("other-task")
	if err := client.Update(context.Background(), secret); err != nil {
		t.Fatal(err)
	}
	if _, _, err := reconciler.harnessBrokeredTransactionAuthority(context.Background(), task); err == nil {
		t.Fatal("unowned transaction authority Secret was accepted")
	}
}

func TestHarnessBrokeredTransactionAuthorityIdentityChangesOnSecretRotation(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "remote-task",
			Namespace: "tenant",
			UID:       types.UID("task-uid"),
			Annotations: map[string]string{
				labels.AnnotationTransactionTokenSecret: "task-transaction",
			},
		},
		Spec: corev1alpha1.TaskSpec{Transaction: &corev1alpha1.TaskTransaction{
			ID:            "txn-1",
			Scope:         "api.read",
			Scopes:        []string{"api.read"},
			ContextDigest: "sha256:context",
		}},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "task-transaction",
			Namespace: "tenant",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: corev1alpha1.GroupVersion.String(), Kind: "Task", Name: task.Name, UID: task.UID,
			}},
		},
		Data: map[string][]byte{"token": []byte("token-a")},
	}
	client := ctrlfake.NewClientBuilder().WithScheme(scheme).WithObjects(task, secret).Build()
	reconciler := &TaskReconciler{Client: client, APIReader: client}
	first, err := reconciler.harnessBrokeredTransactionAuthorityIdentity(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	firstDigest, err := approvals.TargetSpecDigest(first)
	if err != nil {
		t.Fatal(err)
	}
	secret.Data["token"] = []byte("token-b")
	if err := client.Update(context.Background(), secret); err != nil {
		t.Fatal(err)
	}
	second, err := reconciler.harnessBrokeredTransactionAuthorityIdentity(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	secondDigest, err := approvals.TargetSpecDigest(second)
	if err != nil {
		t.Fatal(err)
	}
	if firstDigest == secondDigest {
		t.Fatal("transaction authority Secret rotation did not change brokered approval identity")
	}
}

func TestHarnessBrokeredOutboundPolicyIdentityChangesOnServiceAccountRecreation(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	policy := &corev1alpha1.OutboundAccessPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name: "resource-api", Namespace: "tenant", UID: types.UID("policy-uid"), ResourceVersion: "10", Generation: 2,
		},
		Spec: corev1alpha1.OutboundAccessPolicySpec{Direct: &corev1alpha1.DirectOutboundAccess{
			Subject: corev1alpha1.OutboundTokenSource{
				Source:            corev1alpha1.OutboundTokenSourceServiceAccount,
				ServiceAccountRef: &corev1alpha1.OutboundServiceAccountReference{Name: "workload"},
			},
		}},
	}
	serviceAccount := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: "workload", Namespace: "tenant", UID: types.UID("service-account-a"), ResourceVersion: "1"},
	}
	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "dispatch", Namespace: "tenant"},
		Spec: corev1alpha1.ToolSpec{HTTP: &corev1alpha1.HTTPExecution{
			OutboundAccessPolicyRef: &corev1alpha1.LocalObjectReference{Name: policy.Name},
		}},
	}
	client := ctrlfake.NewClientBuilder().WithScheme(scheme).WithObjects(policy, serviceAccount).Build()
	reconciler := &TaskReconciler{Client: client, APIReader: client}
	first, err := reconciler.harnessBrokeredOutboundPolicyIdentity(context.Background(), tool)
	if err != nil {
		t.Fatal(err)
	}
	firstDigest, err := approvals.TargetSpecDigest(first)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Delete(context.Background(), serviceAccount); err != nil {
		t.Fatal(err)
	}
	replacement := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: "workload", Namespace: "tenant", UID: types.UID("service-account-b")},
	}
	if err := client.Create(context.Background(), replacement); err != nil {
		t.Fatal(err)
	}
	second, err := reconciler.harnessBrokeredOutboundPolicyIdentity(context.Background(), tool)
	if err != nil {
		t.Fatal(err)
	}
	secondDigest, err := approvals.TargetSpecDigest(second)
	if err != nil {
		t.Fatal(err)
	}
	if firstDigest == secondDigest {
		t.Fatal("ServiceAccount recreation did not change brokered approval identity")
	}
}

func TestToolReconcilerDirectPolicyDefersMCPResolvedEndpointValidation(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	policy := readyControllerPolicy("tenant", "direct-mcp", corev1alpha1.OutboundAccessPolicySpec{Direct: &corev1alpha1.DirectOutboundAccess{}})
	client := ctrlfake.NewClientBuilder().WithScheme(scheme).WithObjects(policy).Build()
	reconciler := &ToolReconciler{Client: client}
	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "mcp", Namespace: "tenant"},
		Spec: corev1alpha1.ToolSpec{
			HTTP: &corev1alpha1.HTTPExecution{OutboundAccessPolicyRef: &corev1alpha1.LocalObjectReference{Name: policy.Name}},
			MCP:  &corev1alpha1.MCPToolServer{SubstrateActor: &corev1alpha1.SubstrateMCPActor{}},
		},
	}
	if err := reconciler.validateToolHTTPAuth(context.Background(), tool); err != nil {
		t.Fatalf("unresolved endpoint error = %v", err)
	}
	tool.Status.Endpoint = "https://actor.example.test/mcp"
	if err := reconciler.validateToolHTTPAuth(context.Background(), tool); err != nil {
		t.Fatalf("HTTPS endpoint error = %v", err)
	}
	tool.Status.Endpoint = "http://actor.example.test/mcp"
	if err := reconciler.validateToolHTTPAuth(context.Background(), tool); err == nil {
		t.Fatal("plaintext resolved endpoint accepted")
	}
}
