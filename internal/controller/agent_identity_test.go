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

func releasingAgent(agent *corev1alpha1.Agent) *corev1alpha1.Agent {
	agent.Status.IdentityClaim = &corev1alpha1.AgentIdentityClaim{Issuer: "entra", ID: testAgentIdentityID, Releasing: true}
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
	r = setupAgentReconciler(renamed)
	if claim, err := r.reconcileIdentityClaim(context.Background(), renamed); err != nil || claim == nil || claim.Releasing {
		t.Fatalf("naming the held identity again cancels the release: claim=%+v err=%v", claim, err)
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
	root.CreationTimestamp = rootCreated
	child := boundTask("child", agent, corev1alpha1.TaskPhaseRunning)
	child.Labels = map[string]string{labels.LabelParentTask: "root"}
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
	if binding, err := agentIdentityBindingFromTask(context.Background(), reader, orphan); err != nil || !binding.DelegationStart.IsZero() {
		t.Fatalf("a broken chain has no delegation start: %+v %v", binding, err)
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
	} {
		if err := sessions.CreateSession(context.Background(), session); err != nil {
			t.Fatal(err)
		}
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
	deleting := metav1.Now()
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
		Sessions: sessions,
	}.Check
	binding := func(name string) outboundaccess.AgentIdentityBinding {
		return outboundaccess.AgentIdentityBinding{TaskNamespace: testNS, TaskName: name, TaskUID: name + "-uid"}
	}
	for name, want := range map[string]string{
		"running":                   "",
		"in-open-session":           "",
		"cancelled":                 "Cancelled",
		"in-deleted-session":        "deleted",
		"finished":                  "Succeeded",
		"cancel-requested":          "cancellation",
		"deleting":                  "being deleted",
		"gone":                      "gone",
		"child-of-running":          "",
		"child-of-succeeded":        "",
		"child-of-cancelled":        "was cancelled",
		"child-of-cancel-requested": "cancellation of a Task it was delegated from",
		"child-of-gone":             "is gone",
		"grandchild-of-cancelled":   "was cancelled",
		"child-of-replaced":         "was replaced",
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
