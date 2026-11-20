/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/outboundaccess"
)

// A policy naming the controller's own ServiceAccount is not accepted and
// gets no TokenRequest grant.
func TestOutboundAccessPolicyReconcilerRefusesTheControllerServiceAccount(t *testing.T) {
	scheme := outboundPolicyControllerTestScheme(t)
	policy := controllerServiceAccountPolicy("controller-token", "orka-controller")
	account := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "orka-controller", Namespace: policy.Namespace}}
	client := ctrlfake.NewClientBuilder().WithScheme(scheme).WithObjects(policy, account).
		WithStatusSubresource(&corev1alpha1.OutboundAccessPolicy{}).Build()
	reconciler := &OutboundAccessPolicyReconciler{
		Client: client, APIReader: client, Scheme: scheme, AIWorkerServiceAccountName: testAIWorkerServiceAccountName,
		ReservedServiceAccount: outboundaccess.ReservedServiceAccount{Namespace: policy.Namespace, Name: "orka-controller"},
	}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: policy.Namespace, Name: policy.Name}}
	reconcileOutboundPolicy(t, reconciler, request, 3)
	updated := &corev1alpha1.OutboundAccessPolicy{}
	if err := client.Get(context.Background(), request.NamespacedName, updated); err != nil {
		t.Fatal(err)
	}
	accepted := meta.FindStatusCondition(updated.Status.Conditions, corev1alpha1.OutboundAccessPolicyConditionAccepted)
	if accepted == nil || accepted.Status != metav1.ConditionFalse || !strings.Contains(accepted.Message, "controller's own ServiceAccount") {
		t.Fatalf("Accepted = %+v, want the controller's ServiceAccount refused", accepted)
	}
	roles := &rbacv1.RoleList{}
	if err := client.List(context.Background(), roles); err != nil {
		t.Fatal(err)
	}
	if len(roles.Items) != 0 {
		t.Fatalf("Roles = %d, want no TokenRequest grant", len(roles.Items))
	}
}
