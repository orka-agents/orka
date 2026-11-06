// Copyright (c) 2026. MIT License - see LICENSE file for details.

package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/agentidentity"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/outboundaccess"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
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

// testClaimGrantedAt is when test claims were granted, before any test Task.
var testClaimGrantedAt = metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second))

func claimedAgent(agent *corev1alpha1.Agent) *corev1alpha1.Agent {
	granted := testClaimGrantedAt
	agent.Status.IdentityClaim = &corev1alpha1.AgentIdentityClaim{Issuer: "entra", ID: testAgentIdentityID, GrantedAt: &granted}
	return agent
}

// identityAgentReconciler is an AgentReconciler with Agent use authorization
// enforced, so it grants identity claims.
func identityAgentReconciler(objs ...runtime.Object) *AgentReconciler {
	r := setupAgentReconciler(objs...)
	r.Identity = testIdentityConfig()
	return r
}

func boundTask(name string, agent *corev1alpha1.Agent, phase corev1alpha1.TaskPhase) *corev1alpha1.Task {
	return &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS, CreationTimestamp: metav1.Now()},
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
	r = identityAgentReconciler(fresh)
	claim, err = r.reconcileIdentityClaim(context.Background(), fresh)
	if err != nil || claim == nil {
		t.Fatalf("an unheld identity is claimed: claim=%v err=%v", claim, err)
	}
}

func releasingAgent(agent *corev1alpha1.Agent) *corev1alpha1.Agent {
	granted := testClaimGrantedAt
	agent.Status.IdentityClaim = &corev1alpha1.AgentIdentityClaim{Issuer: "entra", ID: testAgentIdentityID, Releasing: true, GrantedAt: &granted}
	return agent
}

func TestIdentityClaimIsReleasedInTwoPasses(t *testing.T) {
	agent := claimedAgent(identityBoundTestAgent("holder", time.Now()))
	agent.Spec.Identity = nil
	r := setupAgentReconciler(agent)
	claim, err := r.reconcileIdentityClaim(context.Background(), agent)
	if err != nil || claim == nil || !claim.Releasing {
		t.Fatalf("the first pass only marks the claim releasing: claim=%+v err=%v", claim, err)
	}

	releasing := releasingAgent(identityBoundTestAgent("holder", time.Now()))
	releasing.Spec.Identity = nil
	r = setupAgentReconciler(releasing, boundTask("running", releasing, corev1alpha1.TaskPhaseRunning))
	if claim, err := r.reconcileIdentityClaim(context.Background(), releasing); err != nil || claim == nil {
		t.Fatalf("a releasing claim stays while a bound Task runs: claim=%+v err=%v", claim, err)
	}
	r = setupAgentReconciler(releasing, boundTask("finished", releasing, corev1alpha1.TaskPhaseSucceeded))
	if claim, err := r.reconcileIdentityClaim(context.Background(), releasing); err != nil || claim != nil {
		t.Fatalf("the second pass releases once bound Tasks finish: claim=%+v err=%v", claim, err)
	}

	renamed := releasingAgent(identityBoundTestAgent("holder", time.Now()))
	r = identityAgentReconciler(renamed)
	if claim, err := r.reconcileIdentityClaim(context.Background(), renamed); err != nil || claim == nil || claim.Releasing ||
		claim.GrantedAt == nil || !claim.GrantedAt.After(testClaimGrantedAt.Time) {
		t.Fatalf("naming the held identity again cancels the release and grants it anew: claim=%+v err=%v", claim, err)
	}

	switched := releasingAgent(identityBoundTestAgent("holder", time.Now()))
	switched.Spec.Identity.Entra.AgentIdentityID = "44444444-4444-4444-4444-444444444444"
	r = setupAgentReconciler(switched, boundTask("running", switched, corev1alpha1.TaskPhaseRunning))
	r.Identity = testIdentityConfig()
	if _, err := r.reconcileIdentityClaim(context.Background(), switched); err == nil || !strings.Contains(err.Error(), "until 1 Tasks") {
		t.Fatalf("switching identities waits for bound Tasks: err=%v", err)
	}
}

func TestReleasingClaimsAcceptNoNewBindings(t *testing.T) {
	agent := releasingAgent(identityBoundTestAgent("reviewer", time.Now()))
	kube := fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(agent).Build()
	r := &TaskReconciler{Client: kube, Identity: testIdentityConfig()}
	root := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "root", Namespace: testNS}}
	if _, err := r.agentExecutionIdentity(context.Background(), root, agent); !errors.Is(err, errIdentityClaimPending) {
		t.Fatalf("binding against a releasing claim: error = %v, want a retryable wait", err)
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
	instance := "entra:11111111-1111-1111-1111-111111111111/22222222-2222-2222-2222-222222222222"
	parentOwn := boundTask("parent-own", agent, corev1alpha1.TaskPhaseRunning)
	parentOwn.Status.AgentExecutionBinding.Agent.Identity.Instance = instance
	recreated := agent.DeepCopy()
	recreated.UID = "earlier-reviewer-uid"
	parentRecreated := boundTask("parent-recreated", recreated, corev1alpha1.TaskPhaseRunning)
	parentRecreated.Status.AgentExecutionBinding.Agent.Identity.Instance = instance
	parentOtherInstance := boundTask("parent-other-instance", agent, corev1alpha1.TaskPhaseRunning)
	parentOtherInstance.Status.AgentExecutionBinding.Agent.Identity.Instance = "entra:99999999-9999-9999-9999-999999999999/22222222-2222-2222-2222-222222222222"
	parentUnbound := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "parent-unbound", Namespace: testNS},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent, AgentRef: &corev1alpha1.AgentReference{Name: "reviewer"}},
	}
	scheduled := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: testNS, CreationTimestamp: metav1.NewTime(testClaimGrantedAt.Add(time.Second))},
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent, Schedule: "0 2 * * *",
			AgentRef: &corev1alpha1.AgentReference{Name: "reviewer"}},
	}
	// A scheduled Task created before the grant was never checked for the
	// identity, though its runs are new.
	unchecked := scheduled.DeepCopy()
	unchecked.Name = "nightly-unchecked"
	unchecked.CreationTimestamp = testClaimGrantedAt
	parents := []*corev1alpha1.Task{parentOwn, parentRecreated, parentOtherInstance, parentUnbound, scheduled, unchecked}
	builder := fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(agent, unclaimed)
	for _, parent := range parents {
		parent.UID = types.UID(parent.Name + "-uid")
		builder = builder.WithObjects(parent)
	}
	r := &TaskReconciler{Client: builder.Build(), Identity: testIdentityConfig()}

	// task returns a child controlled by the named parent, as delegate_task
	// and scheduled runs create them.
	task := func(parent string) *corev1alpha1.Task {
		task := &corev1alpha1.Task{
			ObjectMeta: metav1.ObjectMeta{Name: "child", Namespace: testNS, Labels: map[string]string{}, CreationTimestamp: metav1.Now()},
			Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent, AgentRef: &corev1alpha1.AgentReference{Name: "reviewer"}},
		}
		if parent != "" {
			task.Labels[labels.LabelParentTask] = parent
			controller := true
			task.OwnerReferences = []metav1.OwnerReference{{
				APIVersion: corev1alpha1.GroupVersion.String(), Kind: taskResourceKind, Name: parent,
				UID: types.UID(parent + "-uid"), Controller: &controller,
			}}
		}
		return task
	}
	ref, err := r.agentExecutionIdentity(context.Background(), task(""), agent)
	if err != nil || ref == nil || ref.Issuer != "entra" || ref.ID != testAgentIdentityID || ref.Instance != instance {
		t.Fatalf("root Task identity = %+v, %v", ref, err)
	}
	if _, err := r.agentExecutionIdentity(context.Background(), task("parent-own"), agent); err != nil {
		t.Fatalf("child of the same bound Agent: %v", err)
	}
	if _, err := r.agentExecutionIdentity(context.Background(), task("nightly"), agent); err != nil {
		t.Fatalf("scheduled run of the scheduled Task's own Agent: %v", err)
	}
	for _, parent := range []string{"parent-recreated", "parent-unbound", "parent-other-instance"} {
		_, err = r.agentExecutionIdentity(context.Background(), task(parent), agent)
		if err == nil || !isPermanentACPAgentConfigurationError(err) || !strings.Contains(err.Error(), "parent's own bound Agent") {
			t.Fatalf("child of %s: error = %v, want a permanent refusal", parent, err)
		}
	}

	if _, err := r.agentExecutionIdentity(context.Background(), task("nightly-unchecked"), agent); err == nil ||
		!isPermanentACPAgentConfigurationError(err) || !strings.Contains(err.Error(), "re-create the scheduled Task") {
		t.Fatalf("run of a scheduled Task created before the grant: error = %v, want a permanent refusal", err)
	}
	otherAgentRun := task("nightly")
	otherAgentRun.Spec.AgentRef.Name = "other"
	if _, err := r.agentExecutionIdentity(context.Background(), otherAgentRun, agent); err == nil ||
		!isPermanentACPAgentConfigurationError(err) || !strings.Contains(err.Error(), "scheduled Task names it") {
		t.Fatalf("scheduled run naming another Agent: error = %v, want a permanent refusal", err)
	}
	orphan := task("parent-own")
	orphan.OwnerReferences = nil
	replaced := task("parent-own")
	replaced.OwnerReferences[0].UID = "deleted-parent-uid"
	misrecorded := task("parent-own")
	misrecorded.Annotations = map[string]string{labels.AnnotationParentTaskUID: "deleted-parent-uid"}
	for name, child := range map[string]*corev1alpha1.Task{"orphan": orphan, "replaced parent": replaced, "recorded parent": misrecorded} {
		_, err = r.agentExecutionIdentity(context.Background(), child, agent)
		if err == nil || !isPermanentACPAgentConfigurationError(err) || !strings.Contains(err.Error(), "still controls it") {
			t.Fatalf("%s: error = %v, want a permanent refusal", name, err)
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

// An API fork names its source as parent but is not controlled by it, so it
// never binds the source's Agent identity: the requester creates a new Task.
func TestAgentExecutionIdentityRefusesAFork(t *testing.T) {
	agent := claimedAgent(identityBoundTestAgent("reviewer", time.Now()))
	source := boundTask("source", agent, corev1alpha1.TaskPhaseSucceeded)
	source.UID = "source-uid"
	source.Status.AgentExecutionBinding.Agent.Identity.Instance = "entra:11111111-1111-1111-1111-111111111111/22222222-2222-2222-2222-222222222222"
	r := &TaskReconciler{
		Client:   fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(agent, source).Build(),
		Identity: testIdentityConfig(),
	}
	fork := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: "source-fork", Namespace: testNS, CreationTimestamp: metav1.Now(),
			Labels: map[string]string{labels.LabelParentTask: "source"},
			Annotations: map[string]string{
				labels.AnnotationParentTaskName: "source", labels.AnnotationForkSourceTask: "source",
			},
		},
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent, AgentRef: &corev1alpha1.AgentReference{Name: "reviewer"}},
	}
	_, err := r.agentExecutionIdentity(context.Background(), fork, agent)
	if err == nil || !isPermanentACPAgentConfigurationError(err) || !strings.Contains(err.Error(), "a fork does not carry") {
		t.Fatalf("fork of an identity-bound Task: error = %v, want a permanent refusal naming the fork", err)
	}
}

type fakeIdentityVerifier struct{ err error }

func (f fakeIdentityVerifier) Verify(context.Context, *corev1alpha1.Agent) error { return f.err }

func TestAgentReconcileVerifiesIdentityWithTheIssuer(t *testing.T) {
	for name, tc := range map[string]struct {
		err        error
		wantStatus metav1.ConditionStatus
		wantReason string
		wantReady  bool
	}{
		"verified": {wantStatus: metav1.ConditionTrue, wantReason: reasonIdentityVerified, wantReady: true},
		"refused":  {err: errors.New("AADSTS700016"), wantStatus: metav1.ConditionFalse, wantReason: reasonIdentityVerificationFailed},
	} {
		t.Run(name, func(t *testing.T) {
			agent := identityBoundTestAgent("reviewer", time.Now())
			r := setupAgentReconciler()
			r.Identity = testIdentityConfig()
			r.IdentityVerifier = fakeIdentityVerifier{err: tc.err}
			r.Client = fake.NewClientBuilder().WithScheme(r.Scheme).WithObjects(agent).WithStatusSubresource(agent).Build()
			result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(agent)})
			if err != nil {
				t.Fatal(err)
			}
			if result.RequeueAfter <= 0 || result.RequeueAfter > agentIdentityReverifyInterval {
				t.Fatalf("RequeueAfter = %v, want re-verification within an hour", result.RequeueAfter)
			}
			stored := &corev1alpha1.Agent{}
			if err := r.Get(context.Background(), client.ObjectKeyFromObject(agent), stored); err != nil {
				t.Fatal(err)
			}
			condition := meta.FindStatusCondition(stored.Status.Conditions, conditionTypeIdentityVerified)
			if condition == nil || condition.Status != tc.wantStatus || condition.Reason != tc.wantReason {
				t.Fatalf("IdentityVerified = %+v", condition)
			}
			// The runtime config here is minimal, so readiness is judged only
			// by whether verification turned it false.
			if !tc.wantReady && stored.Status.Ready {
				t.Fatal("a refused identity must make the Agent not Ready")
			}
		})
	}
}

type identityVerifierFunc func(context.Context, *corev1alpha1.Agent) error

func (f identityVerifierFunc) Verify(ctx context.Context, agent *corev1alpha1.Agent) error {
	return f(ctx, agent)
}

func TestAgentReconcileNeverRecordsAVerificationOnAnEditedAgent(t *testing.T) {
	agent := identityBoundTestAgent("reviewer", time.Now())
	r := setupAgentReconciler()
	r.Identity = testIdentityConfig()
	r.Client = fake.NewClientBuilder().WithScheme(r.Scheme).WithObjects(agent).WithStatusSubresource(agent).Build()
	// While the issuer proves the original identity, an editor names another.
	r.IdentityVerifier = identityVerifierFunc(func(ctx context.Context, _ *corev1alpha1.Agent) error {
		edited := &corev1alpha1.Agent{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(agent), edited); err != nil {
			return err
		}
		edited.Spec.Identity.Entra.AgentIdentityID = "44444444-4444-4444-4444-444444444444"
		edited.Generation = 2
		return r.Update(ctx, edited)
	})
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(agent)}); err == nil {
		t.Fatal("Reconcile() = nil error, want the pass retried for the edited Agent")
	}
	stored := &corev1alpha1.Agent{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(agent), stored); err != nil {
		t.Fatal(err)
	}
	if stored.Generation != 2 {
		t.Fatalf("Generation = %d, want the edit stored", stored.Generation)
	}
	if condition := meta.FindStatusCondition(stored.Status.Conditions, conditionTypeIdentityVerified); condition != nil {
		t.Fatalf("IdentityVerified = %+v, want nothing recorded for the unverified identity", condition)
	}
	if stored.Status.IdentityClaim != nil {
		t.Fatalf("IdentityClaim = %+v, want no claim decided from the old spec", stored.Status.IdentityClaim)
	}
}

func TestIdentityClaimWaitsForADeletedHoldersTasks(t *testing.T) {
	now := time.Now()
	deleted := claimedAgent(identityBoundTestAgent("deleted", now)) // only its Tasks remain
	claimant := identityBoundTestAgent("claimant", now)
	r := identityAgentReconciler(claimant, boundTask("still-running", deleted, corev1alpha1.TaskPhaseRunning))
	claim, err := r.reconcileIdentityClaim(context.Background(), claimant)
	if claim != nil || err == nil || !strings.Contains(err.Error(), "still held by Task") {
		t.Fatalf("a deleted holder's running Task keeps the identity taken: claim=%v err=%v", claim, err)
	}

	r = identityAgentReconciler(claimant, boundTask("done", deleted, corev1alpha1.TaskPhaseSucceeded),
		boundTask("own", claimant, corev1alpha1.TaskPhaseRunning))
	claim, err = r.reconcileIdentityClaim(context.Background(), claimant)
	if err != nil || claim == nil || claim.ID != testAgentIdentityID {
		t.Fatalf("finished Tasks and the claimant's own Tasks do not hold it: claim=%v err=%v", claim, err)
	}
}

func TestIdentityAgentFailsEveryTaskTypeItCannotHonor(t *testing.T) {
	native := identityBoundTestAgent("native", time.Now())
	native.Spec.Runtime = nil
	r := &TaskReconciler{Identity: testIdentityConfig()}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "ai", Namespace: testNS},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI, AgentRef: &corev1alpha1.AgentReference{Name: "native"}},
	}
	if err := r.validateTaskAgentCompatibility(task, native); err == nil || !strings.Contains(err.Error(), "only on runtime Agents") {
		t.Fatalf("native AI Task with an identity-bound Agent: error = %v, want a refusal", err)
	}
	native.Spec.Identity = nil
	if err := r.validateTaskAgentCompatibility(task, native); err != nil {
		t.Fatalf("native AI Task without an identity: %v", err)
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
		"claim releasing":           {agent: releasingAgent(identityBoundTestAgent("reviewer", time.Now())), wantFail: true},
		"Agent deleted":             {wantFail: true},
		"already executing is left": {agent: released, executing: true},
	} {
		t.Run(name, func(t *testing.T) {
			task := boundTask("work", holder, corev1alpha1.TaskPhasePending)
			task.UID = "work-uid"
			if tc.executing {
				task.Status.Execution = &corev1alpha1.TaskExecutionStatus{State: corev1alpha1.TaskExecutionStateRunning}
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

func TestIdentityClaimIsDecidedFromTheLiveAgentNotTheCache(t *testing.T) {
	now := time.Now()
	released := identityBoundTestAgent("released", now)
	released.Spec.Identity = nil // live: no identity and no claim
	newHolder := claimedAgent(identityBoundTestAgent("new-holder", now))
	staleCopy := claimedAgent(released.DeepCopy()) // the cache still shows the old claim
	inFlight := boundTask("in-flight", released, corev1alpha1.TaskPhaseRunning)
	r := setupAgentReconciler(released, newHolder, inFlight)
	claim, err := r.reconcileIdentityClaim(context.Background(), staleCopy)
	if err != nil || claim != nil {
		t.Fatalf("a stale cached claim must not be kept: claim=%v err=%v", claim, err)
	}
}

func TestAgentIdentityBindingCarriesDelegationStartAndEgress(t *testing.T) {
	agent := claimedAgent(identityBoundTestAgent("reviewer", time.Now()))
	rootCreated := metav1.NewTime(time.Now().Add(-30 * time.Minute).Truncate(time.Second))
	root := boundTask("root", agent, corev1alpha1.TaskPhaseRunning)
	root.UID = "root-uid"
	root.CreationTimestamp = rootCreated
	child := boundTask("child", agent, corev1alpha1.TaskPhaseRunning)
	child.Labels = map[string]string{labels.LabelParentTask: "root"}
	child.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: corev1alpha1.GroupVersion.String(), Kind: "Task", Name: "root", UID: root.UID, Controller: new(true),
	}}
	child.CreationTimestamp = metav1.Now()
	reader := fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(root, child).Build()

	binding, err := agentIdentityBindingFromTask(context.Background(), reader, child)
	if err != nil || binding == nil {
		t.Fatalf("binding = %+v, err = %v", binding, err)
	}
	if !binding.DelegationStart.Equal(rootCreated.Time) {
		t.Fatalf("DelegationStart = %v, want the root Task's creation %v", binding.DelegationStart, rootCreated.Time)
	}
	if !binding.ManagedEgress || binding.AgentName != "reviewer" || binding.AgentUID != string(agent.UID) {
		t.Fatalf("binding = %+v", binding)
	}

	workspace := child.DeepCopy()
	workspace.Spec.Execution = &corev1alpha1.ExecutionSpec{Workspace: &corev1alpha1.ExecutionWorkspaceSpec{}}
	if binding, _ := agentIdentityBindingFromTask(context.Background(), reader, workspace); binding.ManagedEgress {
		t.Fatal("a workspace-backed Task is not on a network Orka controls")
	}
	orphan := child.DeepCopy()
	orphan.Labels[labels.LabelParentTask] = "gone"
	orphan.OwnerReferences[0].Name = "gone"
	if binding, err := agentIdentityBindingFromTask(context.Background(), reader, orphan); err != nil || !binding.DelegationStart.IsZero() {
		t.Fatalf("a broken chain has no delegation start: %+v %v", binding, err)
	}
}

// delegationTestTask is a Task the requester's chain is built from: stamped
// for alice, and created at created.
func delegationTestTask(name string, created time.Time, mutate func(*corev1alpha1.Task)) *corev1alpha1.Task {
	requester := &corev1alpha1.RequestedBy{Issuer: "https://issuer.example.test", Subject: "alice"}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: testNS, UID: types.UID(name + "-uid"), CreationTimestamp: metav1.NewTime(created),
			Annotations: map[string]string{labels.AnnotationRequestedBySource: labels.RequestedBySourceAPI},
		},
		Spec:   corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent, RequestedBy: requester},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning},
	}
	if mutate != nil {
		mutate(task)
	}
	return task
}

func delegatedFrom(parent string, pinned bool) func(*corev1alpha1.Task) {
	return func(task *corev1alpha1.Task) {
		task.Labels = map[string]string{labels.LabelParentTask: parent}
		task.Annotations[labels.AnnotationParentTaskName] = parent
		if pinned {
			task.OwnerReferences = []metav1.OwnerReference{{
				APIVersion: corev1alpha1.GroupVersion.String(), Kind: "Task", Name: parent,
				UID: types.UID(parent + "-uid"), Controller: new(true),
			}}
		}
	}
}

func TestDelegationStartFollowsOnlyProvenLineage(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	rootCreated, childCreated, forkCreated := now.Add(-3*time.Hour), now.Add(-2*time.Hour), now.Add(-time.Minute)
	root := delegationTestTask("root", rootCreated, nil)
	child := delegationTestTask("child", childCreated, delegatedFrom("root", true))
	recreated := delegationTestTask("recreated", now.Add(-time.Hour), nil)
	reader := fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(root, child, recreated).Build()

	tests := map[string]struct {
		task *corev1alpha1.Task
		want time.Time
	}{
		"an inherited child keeps its root's start": {
			task: delegationTestTask("grandchild", now, delegatedFrom("child", true)),
			want: rootCreated,
		},
		"a parent pinned by the brokered parent UID": {
			task: delegationTestTask("brokered", now, func(task *corev1alpha1.Task) {
				delegatedFrom("child", false)(task)
				task.Annotations[labels.AnnotationParentTaskUID] = "child-uid"
			}),
			want: rootCreated,
		},
		"a fork names its source without a UID pin and cannot be traced": {
			task: delegationTestTask("fork", forkCreated, func(task *corev1alpha1.Task) {
				delegatedFrom("child", false)(task)
				task.Annotations[labels.AnnotationForkSourceTask] = "child"
			}),
		},
		"an orphaned child cannot be traced": {
			task: delegationTestTask("orphan", now, delegatedFrom("child", false)),
		},
		"a parent recreated under its name": {
			task: delegationTestTask("stale", now, func(task *corev1alpha1.Task) {
				delegatedFrom("recreated", true)(task)
				task.OwnerReferences[0].UID = "an-older-uid"
			}),
		},
		"a parent acting for another requester": {
			task: delegationTestTask("borrowed", now, func(task *corev1alpha1.Task) {
				delegatedFrom("child", true)(task)
				task.Spec.RequestedBy = &corev1alpha1.RequestedBy{Issuer: "https://issuer.example.test", Subject: "mallory"}
			}),
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := delegationStart(context.Background(), reader, tc.task)
			if err != nil {
				t.Fatal(err)
			}
			if !got.Equal(tc.want) {
				t.Fatalf("delegationStart() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestClassificationActsForRequester(t *testing.T) {
	if classificationActsForRequester(map[string]connectorToolInfo{"gh": {PolicyName: "github-as-me"}}) {
		t.Fatal("a connection-mode Tool does not act through the Agent's identity")
	}
	if !classificationActsForRequester(map[string]connectorToolInfo{"gh": {}, "write_blob": {Delegated: true}}) {
		t.Fatal("a Delegated Tool makes the binding act for the requester")
	}
}

func TestDelegationLivenessStopsWhenTheTaskOrSessionEnds(t *testing.T) {
	db, err := sqlite.NewDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	sessions := sqlite.NewStore(db, ":memory:")
	for _, session := range []*store.SessionRecord{
		{Namespace: testNS, Name: "open", SessionType: "task"},
		{Namespace: testNS, Name: "chat-open", SessionType: store.SessionTypeChat},
		{Namespace: testNS, Name: "chat-reused", SessionType: store.SessionTypeChat, CreatedAt: time.Now().Add(-time.Hour)},
	} {
		if err := sessions.CreateSession(context.Background(), session); err != nil {
			t.Fatal(err)
		}
	}
	epoch := func(name string) string {
		createdAt, err := sessions.GetSessionCreatedAt(context.Background(), testNS, name)
		if err != nil {
			t.Fatal(err)
		}
		return labels.SessionEpoch(createdAt)
	}
	openEpoch, deletedEpoch := epoch("chat-open"), epoch("chat-reused")
	// The chat is deleted and a new one created under its name: Tasks of
	// the first must not act for the requester through the second.
	if err := sessions.DeleteSession(context.Background(), testNS, "chat-reused"); err != nil {
		t.Fatal(err)
	}
	if err := sessions.CreateSession(context.Background(), &store.SessionRecord{
		Namespace: testNS, Name: "chat-reused", SessionType: store.SessionTypeChat, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if epoch("chat-reused") == deletedEpoch {
		t.Fatal("a recreated chat Session must have a new incarnation")
	}
	task := func(name string, mutate func(*corev1alpha1.Task)) *corev1alpha1.Task {
		task := &corev1alpha1.Task{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS, UID: types.UID(name + "-uid")},
			Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent, AgentRef: &corev1alpha1.AgentReference{Name: "reviewer"}},
			Status:     corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning},
		}
		if mutate != nil {
			mutate(task)
		}
		return task
	}
	withSession := func(name string) func(*corev1alpha1.Task) {
		return func(task *corev1alpha1.Task) { task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: name} }
	}
	inChat := func(name, epoch string) func(*corev1alpha1.Task) {
		return func(task *corev1alpha1.Task) {
			task.Labels = map[string]string{labels.LabelChatSession: name, labels.LabelChatSessionEpoch: epoch}
		}
	}
	deleting := metav1.Now()
	fork := delegationTestTask("fork-of-running", time.Now(), delegatedFrom("parent-running", false))
	fork.Annotations[labels.AnnotationForkSourceTask] = "parent-running"
	childOf := func(parent string) func(*corev1alpha1.Task) {
		return func(task *corev1alpha1.Task) {
			task.Labels = map[string]string{labels.LabelParentTask: parent}
			task.OwnerReferences = []metav1.OwnerReference{{
				APIVersion: corev1alpha1.GroupVersion.String(), Kind: "Task", Name: parent,
				UID: types.UID(parent + "-uid"), Controller: new(true),
			}}
		}
	}
	objects := []client.Object{
		task("parent-running", nil),
		task("parent-succeeded", func(t *corev1alpha1.Task) { t.Status.Phase = corev1alpha1.TaskPhaseSucceeded }),
		task("parent-cancelled", func(t *corev1alpha1.Task) { t.Status.Phase = corev1alpha1.TaskPhaseCancelled }),
		task("parent-cancel-requested", func(t *corev1alpha1.Task) {
			t.Annotations = map[string]string{labels.AnnotationCancelRequested: "stop"}
		}),
		task("child-of-running", childOf("parent-running")),
		task("child-of-succeeded", childOf("parent-succeeded")),
		task("child-of-cancelled", childOf("parent-cancelled")),
		task("child-of-cancel-requested", childOf("parent-cancel-requested")),
		task("child-of-gone", childOf("parent-gone")),
		task("grandchild-of-cancelled", childOf("child-of-cancelled")),
		task("child-of-replaced", func(t *corev1alpha1.Task) {
			childOf("parent-running")(t)
			t.OwnerReferences[0].UID = "an-older-parent-uid"
		}),
		task("running", nil),
		task("in-open-session", withSession("open")),
		task("cancelled", func(t *corev1alpha1.Task) { t.Status.Phase = corev1alpha1.TaskPhaseCancelled }),
		task("in-open-chat", inChat("chat-open", openEpoch)),
		task("in-deleted-chat", inChat("chat-deleted", openEpoch)),
		task("in-recreated-chat", inChat("chat-reused", deletedEpoch)),
		task("in-unpinned-chat", inChat("chat-open", "")),
		task("parent-in-recreated-chat", inChat("chat-reused", deletedEpoch)),
		task("child-of-parent-in-recreated-chat", childOf("parent-in-recreated-chat")),
		task("parent-in-deleted-session", withSession("deleted")),
		task("parent-in-deleted-chat", inChat("chat-deleted", openEpoch)),
		task("child-of-parent-in-deleted-session", childOf("parent-in-deleted-session")),
		task("child-of-parent-in-deleted-chat", childOf("parent-in-deleted-chat")),
		task("child-in-open-session-of-running", func(t *corev1alpha1.Task) {
			childOf("parent-running")(t)
			withSession("open")(t)
		}),
		task("orphan-of-running", func(t *corev1alpha1.Task) {
			childOf("parent-running")(t)
			t.OwnerReferences = nil
		}),
		task("pinned-orphan-of-running", func(t *corev1alpha1.Task) {
			childOf("parent-running")(t)
			t.OwnerReferences = nil
			t.Annotations = map[string]string{labels.AnnotationParentTaskUID: "parent-running-uid"}
		}),
		fork,
		task("in-deleted-session", withSession("deleted")),
		task("finished", func(t *corev1alpha1.Task) { t.Status.Phase = corev1alpha1.TaskPhaseSucceeded }),
		task("cancel-requested", func(t *corev1alpha1.Task) {
			t.Annotations = map[string]string{labels.AnnotationCancelRequested: "stop"}
		}),
		task("deleting", func(t *corev1alpha1.Task) {
			t.DeletionTimestamp = &deleting
			t.Finalizers = []string{"orka.ai/cleanup"}
		}),
	}
	check := DelegationLiveness{
		Reader:   fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(objects...).Build(),
		Sessions: metadataOnlySessions{sessions},
	}.Check
	binding := func(name string) outboundaccess.AgentIdentityBinding {
		return outboundaccess.AgentIdentityBinding{TaskNamespace: testNS, TaskName: name, TaskUID: name + "-uid"}
	}
	for name, want := range map[string]string{
		"running":                            "",
		"in-open-session":                    "",
		"cancelled":                          "Cancelled",
		"in-open-chat":                       "",
		"in-deleted-chat":                    "Session was deleted",
		"in-recreated-chat":                  "Session was deleted and replaced",
		"in-unpinned-chat":                   "no pinned incarnation",
		"child-of-parent-in-recreated-chat":  "Session of a Task it was delegated from was deleted and replaced",
		"child-of-parent-in-deleted-session": "Session of a Task it was delegated from was deleted",
		"child-of-parent-in-deleted-chat":    "Session of a Task it was delegated from was deleted",
		"child-in-open-session-of-running":   "",
		"orphan-of-running":                  "no UID pins",
		"pinned-orphan-of-running":           "",
		"fork-of-running":                    "no UID pins",
		"in-deleted-session":                 "deleted",
		"finished":                           "Succeeded",
		"cancel-requested":                   "cancellation",
		"deleting":                           "being deleted",
		"gone":                               "gone",
		"child-of-running":                   "",
		"child-of-succeeded":                 "",
		"child-of-cancelled":                 "was cancelled",
		"child-of-cancel-requested":          "cancellation of a Task it was delegated from",
		"child-of-gone":                      "is gone",
		"grandchild-of-cancelled":            "was cancelled",
		"child-of-replaced":                  "was replaced",
	} {
		t.Run(name, func(t *testing.T) {
			err := check(context.Background(), binding(name))
			if want == "" {
				if err != nil {
					t.Fatalf("Check() = %v, want live", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("Check() = %v, want %q", err, want)
			}
		})
	}
	replaced := binding("running")
	replaced.TaskUID = "earlier-uid"
	if err := check(context.Background(), replaced); err == nil || !strings.Contains(err.Error(), "replaced") {
		t.Fatalf("a recreated Task: %v", err)
	}
}

// metadataOnlySessions refuses to load a Session with its transcript, so the
// liveness check that runs before every Delegated call must read metadata only.
type metadataOnlySessions struct{ *sqlite.Store }

func (metadataOnlySessions) GetSession(context.Context, string, string) (*store.SessionRecord, error) {
	return nil, errors.New("the liveness check loaded the Session transcript")
}

func TestIdentityClaimListsOnlyWithinTheAgentNamespace(t *testing.T) {
	agent := identityBoundTestAgent("reviewer", time.Now())
	base := fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(agent).Build()
	// The Helm install grants the controller Agent and Task RBAC only in its
	// watch namespace, so a cluster-scoped list is forbidden there.
	namespaced := interceptor.NewClient(base, interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			listOpts := &client.ListOptions{}
			listOpts.ApplyOptions(opts)
			if listOpts.Namespace == "" {
				return apierrors.NewForbidden(schema.GroupResource{Group: "core.orka.ai", Resource: "agents"}, "", errors.New("cluster scope"))
			}
			return c.List(ctx, list, opts...)
		},
	})
	r := &AgentReconciler{Client: namespaced, APIReader: namespaced, Identity: testIdentityConfig()}
	claim, err := r.reconcileIdentityClaim(context.Background(), agent)
	if err != nil || claim == nil {
		t.Fatalf("claim = %+v, err = %v; identity lists must stay in the Agent's namespace", claim, err)
	}
	released := claimedAgent(identityBoundTestAgent("reviewer", time.Now()))
	released.Spec.Identity = nil
	releasedClient := interceptor.NewClient(fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(releasingAgent(released)).Build(),
		interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			listOpts := &client.ListOptions{}
			listOpts.ApplyOptions(opts)
			if listOpts.Namespace == "" {
				return errors.New("cluster-scoped list")
			}
			return c.List(ctx, list, opts...)
		}})
	r = &AgentReconciler{Client: releasedClient, APIReader: releasedClient}
	if _, err := r.reconcileIdentityClaim(context.Background(), releasingAgent(released)); err != nil {
		t.Fatalf("the bound-Task scan must stay in the Agent's namespace: %v", err)
	}
	if requests := r.agentsSharingIdentity(context.Background(), agent); requests != nil {
		t.Fatalf("requests = %v", requests)
	}
}

func TestRequireBoundIdentityClaimRechecksAQueuedTask(t *testing.T) {
	fixture := newACPQueuePlanningFailureFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	queued, attempt := fixture.queueValidTask(t, ctx)
	if queued.Status.Execution == nil || queued.Status.Execution.State != corev1alpha1.TaskExecutionStateQueued {
		t.Fatalf("execution = %+v, want Queued for capacity", queued.Status.Execution)
	}
	// The binding froze an identity its Agent no longer holds: the Agent is gone.
	binding := queued.Status.AgentExecutionBinding.DeepCopy()
	binding.Agent.Identity = &corev1alpha1.AgentExecutionIdentityRef{Issuer: "entra", ID: testAgentIdentityID, Instance: "entra:x/y"}
	fixture.reconciler.Identity = testIdentityConfig()
	if _, err, handled := fixture.reconciler.requireBoundIdentityClaim(ctx, queued, binding); err != nil || !handled {
		t.Fatalf("queued Task: handled=%v err=%v, want it failed before dispatch", handled, err)
	}
	current := &corev1alpha1.Task{}
	if err := fixture.kubeClient.Get(ctx, client.ObjectKeyFromObject(queued), current); err != nil {
		t.Fatal(err)
	}
	if current.Status.Phase != corev1alpha1.TaskPhaseFailed || current.Status.Execution == nil ||
		current.Status.Execution.Reason != corev1alpha1.TaskExecutionReason("IdentityUnavailable") {
		t.Fatalf("Task status = %+v, want Failed/IdentityUnavailable", current.Status)
	}
	persisted, err := fixture.controlStore.GetPromptAttempt(ctx, attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.ExecutionState != store.PromptExecutionFailed {
		t.Fatalf("PromptAttempt state = %s, want Failed so it is never dispatched", persisted.ExecutionState)
	}
}

func TestIdentityClaimsAreGrantedOnlyWhileAgentUseIsEnforced(t *testing.T) {
	declared := identityBoundTestAgent("reviewer", time.Now())
	r := setupAgentReconciler(declared) // --agent-use-authorization-enabled is off
	if claim, err := r.reconcileIdentityClaim(context.Background(), declared); err != nil || claim != nil {
		t.Fatalf("no claim is granted without the use check: claim=%+v err=%v", claim, err)
	}
	held := claimedAgent(identityBoundTestAgent("reviewer", time.Now()))
	r = setupAgentReconciler(held)
	if claim, err := r.reconcileIdentityClaim(context.Background(), held); err != nil || claim == nil || !claim.Releasing {
		t.Fatalf("a held claim is released without the use check: claim=%+v err=%v", claim, err)
	}

	before := time.Now().Truncate(time.Second)
	r = identityAgentReconciler(declared)
	claim, err := r.reconcileIdentityClaim(context.Background(), declared)
	if err != nil || claim == nil || claim.GrantedAt == nil || claim.GrantedAt.Time.Before(before) {
		t.Fatalf("a granted claim records when: claim=%+v err=%v", claim, err)
	}
}

func TestOnlyTasksCreatedAfterTheGrantBindTheIdentity(t *testing.T) {
	agent := claimedAgent(identityBoundTestAgent("reviewer", time.Now()))
	kube := fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(agent).Build()
	r := &TaskReconciler{Client: kube, Identity: testIdentityConfig()}
	task := func(created time.Time) *corev1alpha1.Task {
		return &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "work", Namespace: testNS, CreationTimestamp: metav1.NewTime(created)}}
	}
	if _, err := r.agentExecutionIdentity(context.Background(), task(testClaimGrantedAt.Add(time.Second)), agent); err != nil {
		t.Fatalf("Task created the second after the grant: %v", err)
	}
	// Times have one-second precision, so a Task recorded in the grant's own
	// second may predate it.
	for name, created := range map[string]time.Time{
		"in the grant's second": testClaimGrantedAt.Time, "before the grant": testClaimGrantedAt.Add(-time.Second),
	} {
		_, err := r.agentExecutionIdentity(context.Background(), task(created), agent)
		if err == nil || !isPermanentACPAgentConfigurationError(err) || !strings.Contains(err.Error(), "create the Task again") {
			t.Fatalf("Task created %s: error = %v, want a permanent refusal", name, err)
		}
	}

	// A claim released and granted again after a bound Task was created no
	// longer covers that Task.
	regranted := claimedAgent(identityBoundTestAgent("reviewer", time.Now()))
	later := metav1.NewTime(time.Now().Add(time.Hour).Truncate(time.Second))
	regranted.Status.IdentityClaim.GrantedAt = &later
	bound := boundTask("work", claimedAgent(identityBoundTestAgent("reviewer", time.Now())), corev1alpha1.TaskPhasePending)
	bound.UID = "work-uid"
	r = &TaskReconciler{
		Client:   fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(bound, regranted).WithStatusSubresource(bound).Build(),
		Identity: testIdentityConfig(), Recorder: record.NewFakeRecorder(8),
	}
	if _, err, handled := r.requireBoundIdentityClaim(context.Background(), bound, bound.Status.AgentExecutionBinding); err != nil || !handled {
		t.Fatalf("Task bound before a re-grant: handled=%v err=%v, want it failed", handled, err)
	}
}

func TestAgentIdentityBindingFromTaskCarriesTheTaskCreationTime(t *testing.T) {
	agent := claimedAgent(identityBoundTestAgent("reviewer", time.Now()))
	task := boundTask("bound", agent, corev1alpha1.TaskPhaseRunning)
	task.UID = "bound-uid"
	binding, err := agentIdentityBindingFromTask(context.Background(), nil, task)
	if err != nil || binding == nil || binding.TaskUID != "bound-uid" || !binding.TaskCreated.Equal(task.CreationTimestamp.Time) {
		t.Fatalf("binding = %+v, want the Task's UID and creation time for the grant check", binding)
	}
	if none, err := agentIdentityBindingFromTask(context.Background(), nil, &corev1alpha1.Task{}); err != nil || none != nil {
		t.Fatal("a Task without an identity binding must yield none")
	}
}

// The issuer instance names a non-default authority, so a parent bound before
// the controller moved to another authority cannot hand its identity to a
// child bound after.
func TestAgentExecutionIdentityRefusesAChildAfterAnAuthorityChange(t *testing.T) {
	agent := claimedAgent(identityBoundTestAgent("reviewer", time.Now()))
	instance, _ := testIdentityConfig().Instance("entra")
	parent := boundTask("parent", agent, corev1alpha1.TaskPhaseRunning)
	parent.UID = "parent-uid"
	parent.Status.AgentExecutionBinding.Agent.Identity.Instance = instance
	sovereign := testIdentityConfig()
	sovereign.Entra.Authority = "https://login.microsoftonline.us"
	r := &TaskReconciler{
		Client:   fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(agent, parent).Build(),
		Identity: sovereign,
	}
	controller := true
	child := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: "child", Namespace: testNS, CreationTimestamp: metav1.Now(),
			Labels: map[string]string{labels.LabelParentTask: "parent"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: corev1alpha1.GroupVersion.String(), Kind: taskResourceKind, Name: "parent",
				UID: "parent-uid", Controller: &controller,
			}},
		},
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent, AgentRef: &corev1alpha1.AgentReference{Name: "reviewer"}},
	}
	root := child.DeepCopy()
	root.Labels, root.OwnerReferences = nil, nil
	ref, err := r.agentExecutionIdentity(context.Background(), root, agent)
	if err != nil || ref == nil || ref.Instance != instance+"@https://login.microsoftonline.us" {
		t.Fatalf("root Task identity under another authority = %+v, %v", ref, err)
	}
	_, err = r.agentExecutionIdentity(context.Background(), child, agent)
	if err == nil || !isPermanentACPAgentConfigurationError(err) || !strings.Contains(err.Error(), "parent's own bound Agent") {
		t.Fatalf("child after an authority change: error = %v, want a permanent refusal", err)
	}
}
