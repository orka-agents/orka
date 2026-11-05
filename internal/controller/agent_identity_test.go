// Copyright (c) 2026. MIT License - see LICENSE file for details.

package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
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
	if claim := stored.Status.IdentityClaim; claim == nil || claim.Issuer != "entra" || claim.ID != testAgentIdentityID {
		t.Fatalf("IdentityClaim = %+v, want the declared identity recorded", claim)
	}
}

func claimedAgent(agent *corev1alpha1.Agent) *corev1alpha1.Agent {
	agent.Status.IdentityClaim = &corev1alpha1.AgentIdentityClaim{Issuer: "entra", ID: testAgentIdentityID}
	return agent
}

func boundTask(name string, agent *corev1alpha1.Agent, phase corev1alpha1.TaskPhase) *corev1alpha1.Task {
	return &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent, AgentRef: &corev1alpha1.AgentReference{Name: agent.Name}},
		Status: corev1alpha1.TaskStatus{Phase: phase, AgentExecutionBinding: &corev1alpha1.AgentExecutionBinding{
			ContractVersion: corev1alpha1.AgentRuntimeContractHarnessV2, Backend: corev1alpha1.AgentExecutionBackendRuntimePool,
			Agent: &corev1alpha1.AgentExecutionAgentRef{
				Namespace: agent.Namespace, Name: agent.Name, UID: agent.UID, Generation: 1,
				Identity: &corev1alpha1.AgentExecutionIdentityRef{Issuer: "entra", ID: testAgentIdentityID, Instance: "entra:x/y"},
			},
		}},
	}
}

func TestIdentityClaimIsFirstComeAndExclusive(t *testing.T) {
	now := time.Now()
	holder := claimedAgent(identityBoundTestAgent("holder", now))
	older := identityBoundTestAgent("older", now.Add(-time.Hour))
	r := setupAgentReconciler(holder, older)
	r.Identity = testIdentityConfig()

	claim, err := r.reconcileIdentityClaim(context.Background(), older)
	if claim != nil || err == nil || !strings.Contains(err.Error(), "already bound to Agent") {
		t.Fatalf("an older Agent must not take over a held identity: claim=%v err=%v", claim, err)
	}
	claim, err = r.reconcileIdentityClaim(context.Background(), holder)
	if err != nil || claim == nil || claim.ID != testAgentIdentityID {
		t.Fatalf("the holder keeps its claim: claim=%v err=%v", claim, err)
	}

	fresh := identityBoundTestAgent("fresh", now)
	r = setupAgentReconciler(fresh)
	claim, err = r.reconcileIdentityClaim(context.Background(), fresh)
	if err != nil || claim == nil {
		t.Fatalf("an unheld identity is claimed: claim=%v err=%v", claim, err)
	}
}

func TestIdentityClaimOutlivesTheSpecWhileTasksAreBound(t *testing.T) {
	agent := claimedAgent(identityBoundTestAgent("holder", time.Now()))
	agent.Spec.Identity = nil
	running := boundTask("running", agent, corev1alpha1.TaskPhaseRunning)
	r := setupAgentReconciler(agent, running)
	claim, err := r.reconcileIdentityClaim(context.Background(), agent)
	if err != nil || claim == nil {
		t.Fatalf("a removed identity stays claimed while a bound Task runs: claim=%v err=%v", claim, err)
	}

	switched := claimedAgent(identityBoundTestAgent("holder", time.Now()))
	switched.Spec.Identity.Entra.AgentIdentityID = "44444444-4444-4444-4444-444444444444"
	r = setupAgentReconciler(switched, boundTask("running", switched, corev1alpha1.TaskPhaseRunning))
	r.Identity = testIdentityConfig()
	if _, err := r.reconcileIdentityClaim(context.Background(), switched); err == nil || !strings.Contains(err.Error(), "until 1 Tasks") {
		t.Fatalf("switching identities waits for bound Tasks: err=%v", err)
	}

	finished := boundTask("finished", agent, corev1alpha1.TaskPhaseSucceeded)
	r = setupAgentReconciler(agent, finished)
	claim, err = r.reconcileIdentityClaim(context.Background(), agent)
	if err != nil || claim != nil {
		t.Fatalf("the claim is released once bound Tasks finish: claim=%v err=%v", claim, err)
	}
}

func TestAgentsSharingIdentityRequeuesOtherClaimants(t *testing.T) {
	now := time.Now()
	holder := claimedAgent(identityBoundTestAgent("holder", now))
	holder.Spec.Identity = nil
	waiting := identityBoundTestAgent("waiting", now)
	plain := baseAgent("plain")
	r := setupAgentReconciler(holder, waiting, plain)
	requests := r.agentsSharingIdentity(context.Background(), holder)
	if len(requests) != 1 || requests[0].Name != "waiting" {
		t.Fatalf("requests = %v, want the Agent waiting for the held identity", requests)
	}
}

func TestAgentExecutionIdentityFreezesAndRestrictsChildren(t *testing.T) {
	now := time.Now()
	agent := claimedAgent(identityBoundTestAgent("reviewer", now))
	unclaimed := identityBoundTestAgent("unclaimed", now)
	unclaimed.Spec.Identity.Entra.AgentIdentityID = "44444444-4444-4444-4444-444444444444"
	parentOwn := boundTask("parent-own", agent, corev1alpha1.TaskPhaseRunning)
	recreated := agent.DeepCopy()
	recreated.UID = "earlier-reviewer-uid"
	parentRecreated := boundTask("parent-recreated", recreated, corev1alpha1.TaskPhaseRunning)
	parentUnbound := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "parent-unbound", Namespace: testNS},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent, AgentRef: &corev1alpha1.AgentReference{Name: "reviewer"}},
	}
	kube := fake.NewClientBuilder().WithScheme(newTestScheme()).
		WithObjects(agent, unclaimed, parentOwn, parentRecreated, parentUnbound).Build()
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
		t.Fatalf("child of the same bound Agent: %v", err)
	}
	for _, parent := range []string{"parent-recreated", "parent-unbound"} {
		_, err = r.agentExecutionIdentity(context.Background(), task(parent), agent)
		if err == nil || !isPermanentACPAgentConfigurationError(err) || !strings.Contains(err.Error(), "parent's own bound Agent") {
			t.Fatalf("child of %s: error = %v, want a permanent refusal", parent, err)
		}
	}
	_, err = r.agentExecutionIdentity(context.Background(), task(""), unclaimed)
	if err == nil || isPermanentACPAgentConfigurationError(err) || !errors.Is(err, errIdentityClaimPending) {
		t.Fatalf("Agent without its claim yet: error = %v, want a retryable wait", err)
	}

	r.Identity.AgentUseAuthorization = false
	if _, err := r.agentExecutionIdentity(context.Background(), task(""), agent); err == nil || !isPermanentACPAgentConfigurationError(err) {
		t.Fatalf("use authorization off: error = %v, want a permanent refusal", err)
	}
}

func TestRequireBoundIdentityClaimFailsATaskWhoseAgentLostTheIdentity(t *testing.T) {
	holder := claimedAgent(identityBoundTestAgent("reviewer", time.Now()))
	released := identityBoundTestAgent("reviewer", time.Now())
	for name, tc := range map[string]struct {
		agent     *corev1alpha1.Agent
		executing bool
		wantFail  bool
	}{
		"claim held":                {agent: holder},
		"claim released":            {agent: released, wantFail: true},
		"Agent deleted":             {wantFail: true},
		"already executing is left": {agent: released, executing: true},
	} {
		t.Run(name, func(t *testing.T) {
			task := boundTask("work", holder, corev1alpha1.TaskPhasePending)
			task.UID = "work-uid"
			if tc.executing {
				task.Status.Execution = &corev1alpha1.TaskExecutionStatus{}
			}
			builder := fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(task).WithStatusSubresource(task)
			if tc.agent != nil {
				builder = builder.WithObjects(tc.agent)
			}
			r := &TaskReconciler{Client: builder.Build(), Identity: testIdentityConfig(), Recorder: record.NewFakeRecorder(8)}
			_, err, handled := r.requireBoundIdentityClaim(context.Background(), task, task.Status.AgentExecutionBinding)
			if err != nil {
				t.Fatal(err)
			}
			if handled != tc.wantFail {
				t.Fatalf("handled = %v, want %v", handled, tc.wantFail)
			}
			if tc.wantFail {
				stored := &corev1alpha1.Task{}
				if err := r.Get(context.Background(), client.ObjectKeyFromObject(task), stored); err != nil {
					t.Fatal(err)
				}
				if stored.Status.Phase != corev1alpha1.TaskPhaseFailed {
					t.Fatalf("phase = %q, want Failed before any identity use", stored.Status.Phase)
				}
			}
		})
	}
}
