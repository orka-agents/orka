// Copyright (c) 2026. MIT License - see LICENSE file for details.

package controller

import (
	"context"
	"errors"
	"path/filepath"
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
	"github.com/orka-agents/orka/internal/gateway"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

const testAgentIdentityID = "33333333-3333-3333-3333-33333333aaaa"

// testIdentityInstance is the issuer instance testIdentityConfig configures.
const testIdentityInstance = "entra:11111111-1111-1111-1111-111111111111/22222222-2222-2222-2222-222222222222"

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
				Identity: &corev1alpha1.AgentExecutionIdentityRef{Issuer: "entra", ID: testAgentIdentityID, Instance: testIdentityInstance},
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
	requireGrantTimePending(t, "an unheld identity is claimed", claim, err)
}

// requireGrantTimePending fails unless the claim was granted and waits for the
// grant time the API server records.
func requireGrantTimePending(t *testing.T, name string, claim *corev1alpha1.AgentIdentityClaim, err error) {
	t.Helper()
	if claim == nil || claim.ID != testAgentIdentityID || claim.Releasing || claim.GrantedAt != nil ||
		err == nil || !strings.Contains(err.Error(), "waits for the API server to record the grant time") {
		t.Fatalf("%s: claim=%+v err=%v, want a granted claim waiting for its grant time", name, claim, err)
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
	claim, err = r.reconcileIdentityClaim(context.Background(), renamed)
	requireGrantTimePending(t, "naming the held identity again cancels the release and grants it anew", claim, err)

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
	ctx := context.Background()
	ref, err := r.agentExecutionIdentity(ctx, task(""), agent)
	if err != nil || ref == nil || *ref != (corev1alpha1.AgentExecutionIdentityRef{Issuer: "entra", ID: testAgentIdentityID, Instance: instance}) {
		t.Fatalf("root Task identity = %+v, %v", ref, err)
	}
	for name, child := range map[string]*corev1alpha1.Task{
		"child of the same bound Agent":                   task("parent-own"),
		"scheduled run of the scheduled Task's own Agent": task("nightly"),
	} {
		if _, err := r.agentExecutionIdentity(ctx, child, agent); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}

	otherAgentRun := task("nightly")
	otherAgentRun.Spec.AgentRef.Name = "other"
	orphan := task("parent-own")
	orphan.OwnerReferences = nil
	replaced := task("parent-own")
	replaced.OwnerReferences[0].UID = "deleted-parent-uid"
	misrecorded := task("parent-own")
	misrecorded.Annotations = map[string]string{labels.AnnotationParentTaskUID: "deleted-parent-uid"}
	for _, tc := range []struct {
		name string
		task *corev1alpha1.Task
		want string
	}{
		{"child of a recreated Agent's Task", task("parent-recreated"), "parent's own bound Agent"},
		{"child of an unbound parent", task("parent-unbound"), "parent's own bound Agent"},
		{"child of a parent frozen under another instance", task("parent-other-instance"), "parent's own bound Agent"},
		{"run of a scheduled Task created before the grant", task("nightly-unchecked"), "re-create the scheduled Task"},
		{"scheduled run naming another Agent", otherAgentRun, "scheduled Task names it"},
		{"orphan", orphan, "still controls it"},
		{"replaced parent", replaced, "still controls it"},
		{"recorded parent", misrecorded, "still controls it"},
	} {
		_, err := r.agentExecutionIdentity(ctx, tc.task, agent)
		requirePermanentIdentityRefusal(t, tc.name, err, tc.want)
	}

	if _, err := r.agentExecutionIdentity(ctx, task(""), unclaimed); !errors.Is(err, errIdentityClaimPending) || isPermanentACPAgentConfigurationError(err) {
		t.Fatalf("Agent without its claim yet: error = %v, want a retryable wait", err)
	}
	r.Identity.AgentUseAuthorization = false
	_, err = r.agentExecutionIdentity(ctx, task(""), agent)
	requirePermanentIdentityRefusal(t, "use authorization off", err, "agent-use-authorization-enabled")
}

// requirePermanentIdentityRefusal fails unless err permanently refuses a
// binding with a message containing want.
func requirePermanentIdentityRefusal(t *testing.T, name string, err error, want string) {
	t.Helper()
	if err == nil || !isPermanentACPAgentConfigurationError(err) || !strings.Contains(err.Error(), want) {
		t.Fatalf("%s: error = %v, want a permanent refusal mentioning %q", name, err, want)
	}
}

func TestAgentExecutionIdentityRequiresCheckedTaskSources(t *testing.T) {
	agent := claimedAgent(identityBoundTestAgent("reviewer", time.Now()))
	after := metav1.NewTime(testClaimGrantedAt.Add(time.Second))
	newScan := &corev1alpha1.RepositoryScan{ObjectMeta: metav1.ObjectMeta{Name: "new-scan", Namespace: testNS, UID: "new-scan-uid", CreationTimestamp: after}}
	oldScan := &corev1alpha1.RepositoryScan{ObjectMeta: metav1.ObjectMeta{Name: "old-scan", Namespace: testNS, UID: "old-scan-uid", CreationTimestamp: testClaimGrantedAt}}
	newMonitor := &corev1alpha1.RepositoryMonitor{ObjectMeta: metav1.ObjectMeta{Name: "new-monitor", Namespace: testNS, UID: "new-monitor-uid", CreationTimestamp: after}}
	oldMonitor := &corev1alpha1.RepositoryMonitor{ObjectMeta: metav1.ObjectMeta{Name: "old-monitor", Namespace: testNS, UID: "old-monitor-uid", CreationTimestamp: testClaimGrantedAt}}
	kube := fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(agent, newScan, oldScan, newMonitor, oldMonitor).Build()
	r := &TaskReconciler{Client: kube, Identity: testIdentityConfig()}
	ctx := context.Background()

	// producedBy returns a fresh Task controlled by the named source object.
	producedBy := func(kind, name, uid string) *corev1alpha1.Task {
		controller := true
		return &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{
			Name: "produced", Namespace: testNS, CreationTimestamp: metav1.Now(),
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: corev1alpha1.GroupVersion.String(), Kind: kind, Name: name, UID: types.UID(uid), Controller: &controller,
			}},
		}}
	}
	for name, task := range map[string]*corev1alpha1.Task{
		"scan created after the grant":    producedBy("RepositoryScan", "new-scan", "new-scan-uid"),
		"monitor created after the grant": producedBy("RepositoryMonitor", "new-monitor", "new-monitor-uid"),
	} {
		if _, err := r.agentExecutionIdentity(ctx, task, agent); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	gatewayTask := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{
		Name: "from-gateway", Namespace: testNS, CreationTimestamp: metav1.Now(),
		Labels: map[string]string{gateway.TaskGatewayBindingLabel: "support"},
	}}
	for _, tc := range []struct {
		name string
		task *corev1alpha1.Task
		want string
	}{
		{"scan created in the grant's second", producedBy("RepositoryScan", "old-scan", "old-scan-uid"), "re-create the RepositoryScan"},
		{"monitor created in the grant's second", producedBy("RepositoryMonitor", "old-monitor", "old-monitor-uid"), "re-create the RepositoryMonitor"},
		{"replaced scan", producedBy("RepositoryScan", "new-scan", "deleted-scan-uid"), "was replaced"},
		{"deleted monitor", producedBy("RepositoryMonitor", "gone", "gone-uid"), "is gone"},
		{"GatewayBinding Task", gatewayTask, "GatewayBinding"},
	} {
		_, err := r.agentExecutionIdentity(ctx, tc.task, agent)
		requirePermanentIdentityRefusal(t, tc.name, err, tc.want)
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
	requireGrantTimePending(t, "finished Tasks and the claimant's own Tasks do not hold it", claim, err)
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
		"claim held":                       {agent: holder},
		"claim released":                   {agent: released, wantFail: true},
		"claim releasing":                  {agent: releasingAgent(identityBoundTestAgent("reviewer", time.Now())), wantFail: true},
		"Agent deleted":                    {wantFail: true},
		"queued is left to the dispatcher": {agent: released, executing: true},
	} {
		t.Run(name, func(t *testing.T) {
			task := boundTask("work", holder, corev1alpha1.TaskPhasePending)
			task.UID = "work-uid"
			if tc.executing {
				task.Status.Execution = &corev1alpha1.TaskExecutionStatus{State: corev1alpha1.TaskExecutionStateQueued}
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
	requireGrantTimePending(t, "identity lists stay in the Agent's namespace", claim, err)
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

func TestACPDispatcherFailsQueuedPoolTaskThatLostItsIdentity(t *testing.T) {
	// No Task reconcile runs between the claim loss and the dispatcher scan.
	runACPDispatcherIdentityLossTest(t, false)
}

func TestACPDispatcherFailsReservedExternalTaskThatLostItsIdentity(t *testing.T) {
	// An external Reserved attempt holds no RuntimePool reservation, so it
	// settles without one.
	runACPDispatcherIdentityLossTest(t, true)
}

// runACPDispatcherIdentityLossTest queues (or, for external, reserves) a
// Task bound to an identity whose Agent is gone, runs one dispatcher scan with
// no Task reconcile in between, and requires the Task and its durable attempt
// to fail with IdentityUnavailable before any runtime admission.
func runACPDispatcherIdentityLossTest(t *testing.T, external bool) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	task := runtimePoolReservationTestTask("identity-lost", "identity-lost-uid", "pool-uid")
	task.CreationTimestamp = metav1.Now()
	task.Status.AgentExecutionBinding = testACPExecuteBindingForDispatcher()
	task.Status.AgentExecutionBinding.Agent = &corev1alpha1.AgentExecutionAgentRef{
		Namespace: task.Namespace, Name: "reviewer", UID: "reviewer-uid", Generation: 1,
		Identity: &corev1alpha1.AgentExecutionIdentityRef{Issuer: "entra", ID: testAgentIdentityID, Instance: testIdentityInstance},
	}
	if external {
		task.Status.AgentExecutionBinding.Backend = corev1alpha1.AgentExecutionBackendExternalEndpoint
		task.Status.Execution.RuntimePoolName, task.Status.Execution.RuntimePoolUID = "", ""
		task.Status.Execution.AgentRuntimeName = "external"
		task.Status.Execution.State = corev1alpha1.TaskExecutionStateReserved
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.Task{}).WithObjects(task.DeepCopy()).Build()
	db, err := sqlite.NewDB(filepath.Join(t.TempDir(), "identity-lost.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	controlStore := sqlite.NewStore(db, "identity-lost-test")
	epochs := NewControllerEpochManager(controlStore, "identity-lost-controller")
	epochCtx, cancelEpoch := context.WithCancel(context.Background())
	epochDone := make(chan error, 1)
	go func() { epochDone <- epochs.Start(epochCtx) }()
	t.Cleanup(func() {
		cancelEpoch()
		if err := <-epochDone; err != nil {
			t.Errorf("controller epoch manager shutdown: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	fence, err := epochs.CurrentFence(ctx)
	if err != nil {
		t.Fatal(err)
	}
	key := store.PromptAttemptKey{Namespace: task.Namespace, TaskUID: string(task.UID), Attempt: 1, PromptID: task.Status.Execution.PromptID}
	attemptID, err := key.CanonicalID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controlStore.CreatePromptAttempt(ctx, boundPromptAttemptForTest(&store.PromptAttempt{
		ID: attemptID, Key: key, RequestDigest: task.Status.Execution.RequestDigest,
		ExecutionState: store.PromptExecutionQueued, DeliveryState: store.PromptDeliveryNotRequested,
	}), fence); err != nil {
		t.Fatal(err)
	}
	dispatcher := &ACPDispatcher{
		Identity: testIdentityConfig(),
		Client:   kubeClient, APIReader: kubeClient, Store: controlStore, ResultStore: controlStore, Epochs: epochs,
		AdmissionGate: NewACPAdmissionGate(), sem: make(chan struct{}, 1), active: make(map[types.UID]struct{}),
	}
	if external {
		if err := dispatcher.transitionAttempt(ctx, attemptID, fence, store.PromptExecutionQueued, store.PromptExecutionReserved, "reserve", nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := dispatcher.dispatchOnce(ctx); err != nil {
		t.Fatal(err)
	}
	attempt, err := controlStore.GetPromptAttempt(ctx, attemptID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.ExecutionState != store.PromptExecutionFailed || attempt.TerminalReason != string(taskExecutionReasonIdentityUnavailable) {
		t.Fatalf("attempt = %s/%s, want Failed/IdentityUnavailable", attempt.ExecutionState, attempt.TerminalReason)
	}
	updated := &corev1alpha1.Task{}
	if err := kubeClient.Get(ctx, client.ObjectKeyFromObject(task), updated); err != nil {
		t.Fatal(err)
	}
	if updated.Status.Phase != corev1alpha1.TaskPhaseFailed || updated.Status.Execution == nil ||
		updated.Status.Execution.State != corev1alpha1.TaskExecutionStateFailed ||
		updated.Status.Execution.Reason != taskExecutionReasonIdentityUnavailable ||
		!strings.Contains(updated.Status.Execution.Message, "no longer holds identity") {
		t.Fatalf("Task status = %#v, want Failed/IdentityUnavailable", updated.Status)
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

	r = identityAgentReconciler(declared)
	claim, err := r.reconcileIdentityClaim(context.Background(), declared)
	requireGrantTimePending(t, "a claim is granted while the use check is enforced", claim, err)
}

func TestIdentityClaimGrantTimeComesFromTheAPIServer(t *testing.T) {
	// The API server's clock runs an hour ahead of the controller's here, and
	// its latest recorded write is the one that granted the claim.
	serverWrite := metav1.NewTime(time.Now().Add(time.Hour).Truncate(time.Second))
	earlier := metav1.NewTime(serverWrite.Add(-time.Minute))
	pending := identityBoundTestAgent("reviewer", time.Now())
	pending.Status.IdentityClaim = &corev1alpha1.AgentIdentityClaim{Issuer: "entra", ID: testAgentIdentityID}
	recorded := []metav1.ManagedFieldsEntry{
		{Manager: "kubectl", Operation: metav1.ManagedFieldsOperationUpdate, Time: &earlier},
		{Manager: "manager", Operation: metav1.ManagedFieldsOperationUpdate, Subresource: "status", Time: &serverWrite},
	}
	// serverReader returns the Agent with the managedFields the API server
	// recorded.
	serverReader := func(entries []metav1.ManagedFieldsEntry) client.Reader {
		base := fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(pending).Build()
		return interceptor.NewClient(base, interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				err := c.Get(ctx, key, obj, opts...)
				obj.SetManagedFields(entries)
				return err
			},
		})
	}
	r := identityAgentReconciler(pending)
	r.APIReader = serverReader(recorded)
	claim, err := r.reconcileIdentityClaim(context.Background(), pending)
	if err != nil || claim == nil || claim.GrantedAt == nil || !claim.GrantedAt.Equal(&serverWrite) {
		t.Fatalf("claim=%+v err=%v, want the grant time the API server recorded (%s), not the controller's clock", claim, err, serverWrite)
	}

	// With no recorded write time the claim stays pending, so nothing binds.
	r.APIReader = serverReader(nil)
	claim, err = r.reconcileIdentityClaim(context.Background(), pending)
	requireGrantTimePending(t, "no server write time", claim, err)

	kube := fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(claimedAgent(identityBoundTestAgent("waiting", time.Now()))).Build()
	waiting := &corev1alpha1.Agent{}
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: testNS, Name: "waiting"}, waiting); err != nil {
		t.Fatal(err)
	}
	waiting.Status.IdentityClaim.GrantedAt = nil
	if err := kube.Update(context.Background(), waiting); err != nil {
		t.Fatal(err)
	}
	tr := &TaskReconciler{Client: kube, Identity: testIdentityConfig()}
	work := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "work", Namespace: testNS, CreationTimestamp: metav1.Now()}}
	if _, err := tr.agentExecutionIdentity(context.Background(), work, waiting); !errors.Is(err, errIdentityClaimPending) {
		t.Fatalf("binding to a claim without its grant time: error = %v, want a retryable wait", err)
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

// TestACPDispatcherRechecksIdentityBeforeResubmittingAPrompt covers a prompt
// a RuntimePool turned away for capacity: while the Task waits, its Agent can
// lose the identity, so the dispatcher checks the claim again before it sends
// the prompt once more and settles the Task as IdentityUnavailable instead.
func TestACPDispatcherRechecksIdentityBeforeResubmittingAPrompt(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	task := runtimePoolReservationTestTask("identity-resubmit", "identity-resubmit-uid", "pool-uid")
	task.CreationTimestamp = metav1.Now()
	task.Status.Execution.State = corev1alpha1.TaskExecutionStateSubmitting
	task.Status.AgentExecutionBinding = testACPExecuteBindingForDispatcher()
	task.Status.AgentExecutionBinding.Agent = &corev1alpha1.AgentExecutionAgentRef{
		Namespace: task.Namespace, Name: "reviewer", UID: "reviewer-uid", Generation: 1,
		Identity: &corev1alpha1.AgentExecutionIdentityRef{Issuer: "entra", ID: testAgentIdentityID, Instance: testIdentityInstance},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.Task{}).WithObjects(task.DeepCopy()).Build()
	db, err := sqlite.NewDB(filepath.Join(t.TempDir(), "identity-resubmit.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	controlStore := sqlite.NewStore(db, "identity-resubmit-test")
	epochs := NewControllerEpochManager(controlStore, "identity-resubmit-controller")
	epochCtx, cancelEpoch := context.WithCancel(context.Background())
	epochDone := make(chan error, 1)
	go func() { epochDone <- epochs.Start(epochCtx) }()
	t.Cleanup(func() {
		cancelEpoch()
		if err := <-epochDone; err != nil {
			t.Errorf("controller epoch manager shutdown: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	fence, err := epochs.CurrentFence(ctx)
	if err != nil {
		t.Fatal(err)
	}
	key := store.PromptAttemptKey{Namespace: task.Namespace, TaskUID: string(task.UID), Attempt: 1, PromptID: task.Status.Execution.PromptID}
	attemptID, err := key.CanonicalID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controlStore.CreatePromptAttempt(ctx, boundPromptAttemptForTest(&store.PromptAttempt{
		ID: attemptID, Key: key, RequestDigest: task.Status.Execution.RequestDigest,
		ExecutionState: store.PromptExecutionQueued, DeliveryState: store.PromptDeliveryNotRequested,
	}), fence); err != nil {
		t.Fatal(err)
	}
	dispatcher := &ACPDispatcher{Identity: testIdentityConfig(), Client: kubeClient, APIReader: kubeClient, Store: controlStore, ResultStore: controlStore, Epochs: epochs}
	states := []store.PromptExecutionState{
		store.PromptExecutionQueued, store.PromptExecutionReserved, store.PromptExecutionSessionStarting,
		store.PromptExecutionPlanned, store.PromptExecutionSubmitting,
	}
	for i := 1; i < len(states); i++ {
		if err := dispatcher.transitionAttempt(ctx, attemptID, fence, states[i-1], states[i], "test-"+string(states[i]), nil); err != nil {
			t.Fatal(err)
		}
	}

	unavailable := dispatcher.identityUnavailableBeforeResubmission(ctx, task)
	if !strings.Contains(unavailable, "no longer holds identity") {
		t.Fatalf("unavailable = %q, want the lost identity named", unavailable)
	}
	if err := dispatcher.failPromptForUnavailableIdentity(ctx, task, attemptID, fence, nil, unavailable); err != nil {
		t.Fatal(err)
	}
	attempt, err := controlStore.GetPromptAttempt(ctx, attemptID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.ExecutionState != store.PromptExecutionFailed || attempt.TerminalReason != string(taskExecutionReasonIdentityUnavailable) {
		t.Fatalf("attempt = %s/%s, want Failed/IdentityUnavailable", attempt.ExecutionState, attempt.TerminalReason)
	}
	updated := &corev1alpha1.Task{}
	if err := kubeClient.Get(ctx, client.ObjectKeyFromObject(task), updated); err != nil {
		t.Fatal(err)
	}
	if updated.Status.Phase != corev1alpha1.TaskPhaseFailed || updated.Status.Execution == nil ||
		updated.Status.Execution.Reason != taskExecutionReasonIdentityUnavailable {
		t.Fatalf("Task status = %#v, want Failed/IdentityUnavailable", updated.Status)
	}
}

// TestHeldIdentityClaimIsReleasedOnceItsIssuerIsUnconfigured covers a
// restart without the Entra issuer while Agent use is still enforced: the
// claim is released, as it is when enforcement is off.
func TestHeldIdentityClaimIsReleasedOnceItsIssuerIsUnconfigured(t *testing.T) {
	held := claimedAgent(identityBoundTestAgent("reviewer", time.Now()))
	r := identityAgentReconciler(held)
	r.Identity.Entra = nil
	if claim, err := r.reconcileIdentityClaim(context.Background(), held); err != nil || claim == nil || !claim.Releasing {
		t.Fatalf("a held claim is released once its issuer is unconfigured: claim=%+v err=%v", claim, err)
	}
}

// TestBoundIdentityRequiresTheIssuerConfiguredAsBound covers a controller
// restarted after a Task bound to an identity: the Task may start only while
// Agent use is enforced and the issuer instance it froze is still configured.
func TestBoundIdentityRequiresTheIssuerConfiguredAsBound(t *testing.T) {
	agent := identityBoundTestAgent("reviewer", time.Now())
	task := boundTask("work", agent, corev1alpha1.TaskPhasePending)
	reader := fake.NewClientBuilder().WithScheme(newTestScheme()).Build()
	otherTenant := testIdentityConfig()
	otherTenant.Entra.TenantID = "44444444-4444-4444-4444-444444444444"
	for name, tc := range map[string]struct {
		config agentidentity.Config
		want   string
	}{
		"use authorization off": {agentidentity.Config{Entra: testIdentityConfig().Entra}, "agent-use-authorization-enabled"},
		"issuer removed":        {agentidentity.Config{AgentUseAuthorization: true}, "no longer configured"},
		"issuer moved":          {otherTenant, "no longer configured"},
	} {
		unavailable, err := boundIdentityUnavailable(context.Background(), reader, tc.config, task, task.Status.AgentExecutionBinding)
		if err != nil || !strings.Contains(unavailable, tc.want) {
			t.Errorf("%s: unavailable = %q, %v; want it to mention %q", name, unavailable, err, tc.want)
		}
	}
}

// TestIdentityClaimDoesNotCoverWorkFromBeforeTheEnforcementPeriod covers a
// controller run with Agent use authorization off that never reconciled an
// identity-bound Agent: its claim kept the old grant time, but a Task created
// during that run predates the current enforcement period and cannot bind.
func TestIdentityClaimDoesNotCoverWorkFromBeforeTheEnforcementPeriod(t *testing.T) {
	agent := claimedAgent(identityBoundTestAgent("reviewer", time.Now()))
	reenabled := testClaimGrantedAt.Add(time.Hour)
	config := testIdentityConfig()
	config.EnforcedSince = reenabled
	r := &TaskReconciler{Client: fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(agent).Build(), Identity: config}
	task := func(name string, created time.Time) *corev1alpha1.Task {
		return &corev1alpha1.Task{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS, CreationTimestamp: metav1.NewTime(created)},
			Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent, AgentRef: &corev1alpha1.AgentReference{Name: agent.Name}},
		}
	}
	_, err := r.agentExecutionIdentity(context.Background(), task("unchecked", reenabled.Add(-time.Minute)), agent)
	requirePermanentIdentityRefusal(t, "a Task created while the check was off", err, "create the Task again")
	if ref, err := r.agentExecutionIdentity(context.Background(), task("checked", reenabled.Add(time.Minute)), agent); err != nil || ref == nil {
		t.Fatalf("a Task created in the current period: ref = %+v, err = %v", ref, err)
	}
	bound := boundTask("bound-unchecked", agent, corev1alpha1.TaskPhasePending)
	bound.CreationTimestamp = metav1.NewTime(reenabled.Add(-time.Minute))
	reader := fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(agent).Build()
	unavailable, err := boundIdentityUnavailable(context.Background(), reader, config, bound, bound.Status.AgentExecutionBinding)
	if err != nil || unavailable == "" {
		t.Fatalf("a Task from before the period must not start: unavailable = %q, %v", unavailable, err)
	}
}
