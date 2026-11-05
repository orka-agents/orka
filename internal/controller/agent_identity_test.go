// Copyright (c) 2026. MIT License - see LICENSE file for details.

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/agentidentity"
	"github.com/orka-agents/orka/internal/labels"
)

const testAgentIdentityID = "33333333-3333-3333-3333-33333333aaaa"

func testIdentityConfig() agentidentity.Config {
	return agentidentity.Config{
		Entra: &agentidentity.EntraConfig{
			TenantID: "11111111-1111-1111-1111-111111111111", BlueprintClientID: "22222222-2222-2222-2222-222222222222",
		},
		AgentUseAuthorization: true,
	}
}

func identityBoundTestAgent(name string, created time.Time) *corev1alpha1.Agent {
	return &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: testNS, UID: types.UID(name + "-uid"), Generation: 1,
			CreationTimestamp: metav1.NewTime(created),
		},
		Spec: corev1alpha1.AgentSpec{
			Runtime: &corev1alpha1.AgentCLIRuntime{
				Type:    corev1alpha1.AgentRuntimeClaude,
				Ceiling: &corev1alpha1.AgentRuntimeCeiling{AllowedTools: []string{"Read"}},
			},
			Identity: &corev1alpha1.AgentIdentity{Entra: &corev1alpha1.EntraAgentIdentity{AgentIdentityID: testAgentIdentityID}},
		},
	}
}

func TestAgentReconcileReportsDeclaredIdentity(t *testing.T) {
	agent := identityBoundTestAgent("reviewer", time.Now())
	r := setupAgentReconciler()
	r.Identity = testIdentityConfig()
	r.Client = fake.NewClientBuilder().WithScheme(r.Scheme).WithObjects(agent).WithStatusSubresource(agent).Build()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(agent)}); err != nil {
		t.Fatal(err)
	}
	stored := &corev1alpha1.Agent{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(agent), stored); err != nil {
		t.Fatal(err)
	}
	condition := meta.FindStatusCondition(stored.Status.Conditions, conditionTypeIdentityVerified)
	if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != reasonIdentityDeclared {
		t.Fatalf("IdentityVerified condition = %+v, want False/Declared until stage 3 verifies it", condition)
	}
}

func TestAgentValidationRefusesUnusableIdentities(t *testing.T) {
	now := time.Now()
	older, newer := identityBoundTestAgent("older", now.Add(-time.Hour)), identityBoundTestAgent("newer", now)

	r := setupAgentReconciler(older, newer)
	r.Identity = testIdentityConfig()
	if err := r.validateAgent(context.Background(), older); err != nil && strings.Contains(err.Error(), "identity") {
		t.Fatalf("the first claimant must keep its identity: %v", err)
	}
	if err := r.validateAgent(context.Background(), newer); err == nil || !strings.Contains(err.Error(), "already bound to Agent") {
		t.Fatalf("second claimant: error = %v, want a duplicate-identity rejection", err)
	}

	unconfigured := setupAgentReconciler(older)
	if err := unconfigured.validateAgent(context.Background(), older); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("controller without an issuer: error = %v", err)
	}
}

func TestAgentsSharingIdentityRequeuesOtherClaimants(t *testing.T) {
	now := time.Now()
	older, newer := identityBoundTestAgent("older", now.Add(-time.Hour)), identityBoundTestAgent("newer", now)
	plain := baseAgent("plain")
	r := setupAgentReconciler(older, newer, plain)
	requests := r.agentsSharingIdentity(context.Background(), older)
	if len(requests) != 1 || requests[0].Name != "newer" {
		t.Fatalf("requests = %v, want only the other claimant", requests)
	}
}

func TestAgentExecutionIdentityFreezesAndRestrictsChildren(t *testing.T) {
	now := time.Now()
	agent := identityBoundTestAgent("reviewer", now)
	other := identityBoundTestAgent("other", now)
	other.Spec.Identity = nil
	parentOwn := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "parent-own", Namespace: testNS},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent, AgentRef: &corev1alpha1.AgentReference{Name: "reviewer"}},
	}
	parentOther := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "parent-other", Namespace: testNS},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent, AgentRef: &corev1alpha1.AgentReference{Name: "other"}},
	}
	kube := fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(agent, other, parentOwn, parentOther).Build()
	r := &TaskReconciler{Client: kube, Identity: testIdentityConfig()}

	task := func(parent string) *corev1alpha1.Task {
		task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "child", Namespace: testNS, Labels: map[string]string{}}}
		if parent != "" {
			task.Labels[labels.LabelParentTask] = parent
		}
		return task
	}
	ref, err := r.agentExecutionIdentity(context.Background(), task(""), agent)
	if err != nil || ref == nil || ref.Issuer != "entra" || ref.ID != testAgentIdentityID ||
		ref.Instance != "entra:11111111-1111-1111-1111-111111111111/22222222-2222-2222-2222-222222222222" {
		t.Fatalf("root Task identity = %+v, %v", ref, err)
	}
	if _, err := r.agentExecutionIdentity(context.Background(), task("parent-own"), agent); err != nil {
		t.Fatalf("child of the same Agent: %v", err)
	}
	_, err = r.agentExecutionIdentity(context.Background(), task("parent-other"), agent)
	if err == nil || !isPermanentACPAgentConfigurationError(err) || !strings.Contains(err.Error(), "parent's own Agent") {
		t.Fatalf("child of another Agent: error = %v, want a permanent refusal", err)
	}
	if ref, err := r.agentExecutionIdentity(context.Background(), task("parent-other"), other); err != nil || ref != nil {
		t.Fatalf("Agent without an identity: %+v, %v", ref, err)
	}

	r.Identity.AgentUseAuthorization = false
	if _, err := r.agentExecutionIdentity(context.Background(), task(""), agent); err == nil || !isPermanentACPAgentConfigurationError(err) {
		t.Fatalf("use authorization off: error = %v, want a permanent refusal", err)
	}
}
