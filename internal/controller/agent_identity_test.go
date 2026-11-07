// Copyright (c) 2026. MIT License - see LICENSE file for details.

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
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
	"github.com/orka-agents/orka/internal/outboundaccess"
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
			verifier := &countingIdentityVerifier{err: tc.err}
			r.IdentityVerifier = verifier
			r.Client = apiServerClockClient(fake.NewClientBuilder().WithScheme(r.Scheme).WithObjects(agent).WithStatusSubresource(agent).Build())
			request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(agent)}
			// The first pass records the claim; its grant time comes from the
			// API server's record of that write on the next pass. The issuer
			// is asked only once the claim is granted.
			if _, err := r.Reconcile(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			if verifier.calls != 0 {
				t.Fatalf("Verify() called %d times while the claim was pending", verifier.calls)
			}
			result, err := r.Reconcile(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			if verifier.calls != 1 {
				t.Fatalf("Verify() called %d times, want once the claim is granted", verifier.calls)
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
			// The Agent's own status writes requeue it; those passes reuse
			// the issuer's answer instead of asking again.
			for range 3 {
				if _, err := r.Reconcile(context.Background(), request); err != nil {
					t.Fatal(err)
				}
			}
			if verifier.calls != 1 {
				t.Fatalf("Verify() called %d times across repeated passes, want the answer reused", verifier.calls)
			}
			// A reused answer requeues the Agent for when it expires, so a
			// late pass cannot push verification past the interval.
			r.verifications.Lock()
			entry := r.verifications.byAgent[request.NamespacedName]
			entry.until = time.Now().Add(10 * time.Minute)
			r.verifications.byAgent[request.NamespacedName] = entry
			r.verifications.Unlock()
			reused, err := r.Reconcile(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			if reused.RequeueAfter <= 0 || reused.RequeueAfter > 10*time.Minute {
				t.Fatalf("RequeueAfter = %v, want at most the 10m the answer has left", reused.RequeueAfter)
			}
			// An edit asks the issuer again at once.
			if err := r.Get(context.Background(), client.ObjectKeyFromObject(agent), stored); err != nil {
				t.Fatal(err)
			}
			stored.Generation++
			if err := r.Update(context.Background(), stored); err != nil {
				t.Fatal(err)
			}
			if _, err := r.Reconcile(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			if verifier.calls != 2 {
				t.Fatalf("Verify() called %d times after an edit, want the issuer asked again", verifier.calls)
			}
		})
	}
}

type countingIdentityVerifier struct {
	err   error
	calls int
}

func (v *countingIdentityVerifier) Verify(context.Context, *corev1alpha1.Agent) error {
	v.calls++
	return v.err
}

// apiServerClockClient stamps every Agent it reads with a managedFields write
// time, as the API server records one for each write.
func apiServerClockClient(base client.WithWatch) client.WithWatch {
	written := metav1.NewTime(time.Now().Truncate(time.Second))
	return interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if err := c.Get(ctx, key, obj, opts...); err != nil {
				return err
			}
			if _, ok := obj.(*corev1alpha1.Agent); ok {
				obj.SetManagedFields([]metav1.ManagedFieldsEntry{{
					Manager: "manager", Operation: metav1.ManagedFieldsOperationUpdate, Time: &written,
				}})
			}
			return nil
		},
	})
}

type identityVerifierFunc func(context.Context, *corev1alpha1.Agent) error

func (f identityVerifierFunc) Verify(ctx context.Context, agent *corev1alpha1.Agent) error {
	return f(ctx, agent)
}

func TestAgentReconcileNeverRecordsAVerificationOnAnEditedAgent(t *testing.T) {
	// The claim is already granted, so this pass asks the issuer.
	agent := claimedAgent(identityBoundTestAgent("reviewer", time.Now()))
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
	if !reflect.DeepEqual(stored.Status.IdentityClaim, agent.Status.IdentityClaim) {
		t.Fatalf("IdentityClaim = %+v, want the claim left as it was (%+v)", stored.Status.IdentityClaim, agent.Status.IdentityClaim)
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

	// A Self-mode binding never needs the delegation start, so it reads no
	// ancestor.
	reads := 0
	counting := interceptor.NewClient(fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(root, child).Build(), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			reads++
			return c.Get(ctx, key, obj, opts...)
		},
	})
	selfOnly, err := agentIdentityBindingFromTask(context.Background(), counting, child)
	if err != nil || selfOnly == nil || !selfOnly.DelegationStart.IsZero() || reads != 0 {
		t.Fatalf("Self-mode binding = %+v, err = %v, reads = %d; want no delegation start and no reads", selfOnly, err, reads)
	}
	child.Status.AgentExecutionBinding.Agent.Identity.ActsForRequester = true

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
		{Namespace: testNS, Name: "deleting", SessionType: "task"},
		{Namespace: testNS, Name: "chat-deleting", SessionType: store.SessionTypeChat},
		{Namespace: "chat-ns", Name: "chat-elsewhere", SessionType: store.SessionTypeChat},
	} {
		if err := sessions.CreateSession(context.Background(), session); err != nil {
			t.Fatal(err)
		}
	}
	// The requester deleted these Sessions; their records stay until
	// runtime cleanup finishes, but delegated calls must stop at once.
	for _, name := range []string{"deleting", "chat-deleting"} {
		digest := sha256.Sum256([]byte("delete-" + name))
		if _, err := sessions.PrepareSessionCleanup(context.Background(), store.SessionCleanupIntent{
			Namespace: testNS, SessionName: name, OperationID: "delete-" + name,
			OperationDigest: "sha256:" + hex.EncodeToString(digest[:]), PreparedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	epochIn := func(namespace, name string) string {
		createdAt, err := sessions.GetSessionCreatedAt(context.Background(), namespace, name)
		if err != nil {
			t.Fatal(err)
		}
		return labels.SessionEpoch(createdAt)
	}
	epoch := func(name string) string { return epochIn(testNS, name) }
	elsewhereEpoch := epochIn("chat-ns", "chat-elsewhere")
	openEpoch, deletedEpoch, deletingEpoch := epoch("chat-open"), epoch("chat-reused"), epoch("chat-deleting")
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
		task("in-deleting-session", withSession("deleting")),
		// A chat in another namespace created this Task; its Session is
		// found in the chat's namespace the chat API recorded.
		task("in-chat-elsewhere", func(t *corev1alpha1.Task) {
			inChat("chat-elsewhere", elsewhereEpoch)(t)
			t.Labels[labels.LabelChatSessionNamespace] = "chat-ns"
		}),
		task("in-chat-elsewhere-unrecorded", inChat("chat-elsewhere", elsewhereEpoch)),
		task("in-deleting-chat", inChat("chat-deleting", deletingEpoch)),
		task("parent-in-deleting-session", withSession("deleting")),
		task("child-of-parent-in-deleting-session", childOf("parent-in-deleting-session")),
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
		"running":                             "",
		"in-open-session":                     "",
		"cancelled":                           "Cancelled",
		"in-open-chat":                        "",
		"in-deleted-chat":                     "Session was deleted",
		"in-recreated-chat":                   "Session was deleted and replaced",
		"in-unpinned-chat":                    "no pinned incarnation",
		"in-deleting-session":                 "Session is being deleted",
		"in-chat-elsewhere":                   "",
		"in-chat-elsewhere-unrecorded":        "Session was deleted",
		"in-deleting-chat":                    "Session is being deleted",
		"child-of-parent-in-deleting-session": "Session of a Task it was delegated from is being deleted",
		"child-of-parent-in-recreated-chat":   "Session of a Task it was delegated from was deleted and replaced",
		"child-of-parent-in-deleted-session":  "Session of a Task it was delegated from was deleted",
		"child-of-parent-in-deleted-chat":     "Session of a Task it was delegated from was deleted",
		"child-in-open-session-of-running":    "",
		"orphan-of-running":                   "no UID pins",
		"pinned-orphan-of-running":            "",
		"fork-of-running":                     "no UID pins",
		"in-deleted-session":                  "deleted",
		"finished":                            "Succeeded",
		"cancel-requested":                    "cancellation",
		"deleting":                            "being deleted",
		"gone":                                "gone",
		"child-of-running":                    "",
		"child-of-succeeded":                  "",
		"child-of-cancelled":                  "was cancelled",
		"child-of-cancel-requested":           "cancellation of a Task it was delegated from",
		"child-of-gone":                       "is gone",
		"grandchild-of-cancelled":             "was cancelled",
		"child-of-replaced":                   "was replaced",
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

// With the issuer down, the first Agent's verification starts a shared
// back-off: the rest of the queue is not each held for a timeout, and every
// Agent reports the issuer unavailable rather than its identity refused.
func TestAgentReconcileBacksOffAnUnavailableIssuer(t *testing.T) {
	agents := make([]client.Object, 0, 5)
	for i := range 5 {
		agent := claimedAgent(identityBoundTestAgent(fmt.Sprintf("agent-%d", i), time.Now()))
		agent.Spec.Identity.Entra.AgentIdentityID = fmt.Sprintf("33333333-3333-3333-3333-%012d", i)
		agent.Status.IdentityClaim.ID = agent.Spec.Identity.Entra.AgentIdentityID
		agents = append(agents, agent)
	}
	r := setupAgentReconciler()
	r.Identity = testIdentityConfig()
	r.Client = fake.NewClientBuilder().WithScheme(r.Scheme).WithObjects(agents...).WithStatusSubresource(agents...).Build()
	calls := 0
	r.IdentityVerifier = identityVerifierFunc(func(ctx context.Context, _ *corev1alpha1.Agent) error {
		calls++
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > agentIdentityVerifyTimeout {
			t.Errorf("verification deadline = %v, %v; want at most %s", deadline, ok, agentIdentityVerifyTimeout)
		}
		return fmt.Errorf("the Entra token endpoint is unreachable: %w", agentidentity.ErrIssuerUnavailable)
	})
	for _, agent := range agents {
		result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(agent)})
		if err != nil {
			t.Fatal(err)
		}
		if result.RequeueAfter <= 0 || result.RequeueAfter > agentIdentityIssuerBackoff {
			t.Fatalf("%s: RequeueAfter = %v, want a retry within the back-off", agent.GetName(), result.RequeueAfter)
		}
		stored := &corev1alpha1.Agent{}
		if err := r.Get(context.Background(), client.ObjectKeyFromObject(agent), stored); err != nil {
			t.Fatal(err)
		}
		condition := meta.FindStatusCondition(stored.Status.Conditions, conditionTypeIdentityVerified)
		if condition == nil || condition.Status != metav1.ConditionUnknown || condition.Reason != reasonIdentityIssuerUnavailable {
			t.Fatalf("%s: IdentityVerified = %+v, want Unknown/%s", agent.GetName(), condition, reasonIdentityIssuerUnavailable)
		}
		if stored.Status.Ready {
			t.Fatalf("%s: an unverified identity must not make the Agent Ready", agent.GetName())
		}
		if !reflect.DeepEqual(stored.Status.IdentityClaim, agent.(*corev1alpha1.Agent).Status.IdentityClaim) {
			t.Fatalf("%s: IdentityClaim = %+v, want the claim kept while the issuer is down", agent.GetName(), stored.Status.IdentityClaim)
		}
	}
	if calls != 1 {
		t.Fatalf("issuer asked %d times, want once for the whole queue", calls)
	}

	// A definitive refusal is the identity's own answer and starts no back-off.
	r.issuerBackoff.until = time.Time{}
	calls = 0
	r.IdentityVerifier = identityVerifierFunc(func(context.Context, *corev1alpha1.Agent) error {
		calls++
		return errors.New("Entra refused the token request: unauthorized_client (AADSTS7000112, HTTP 400)")
	})
	for _, agent := range agents[:2] {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(agent)}); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 {
		t.Fatalf("issuer asked %d times after refusals, want each Agent verified", calls)
	}
}

// TestACPDispatcherRechecksIdentityBeforeSubmittingAPrompt covers the time
// between admission and a prompt's submission, the RuntimeSession start or a
// wait for capacity: the Agent can lose the identity meanwhile, so the
// dispatcher checks the claim again before every submission and settles the
// Task as IdentityUnavailable instead.
func TestACPDispatcherRechecksIdentityBeforeSubmittingAPrompt(t *testing.T) {
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

	unavailable := dispatcher.identityUnavailableBeforeSubmission(ctx, task)
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

// TestIdentityDecisionsWaitForTheEnforcementPeriod covers a controller that
// has not yet led long enough to start its period of Agent use enforcement:
// a binding and a pre-start check both wait and retry rather than decide.
func TestIdentityDecisionsWaitForTheEnforcementPeriod(t *testing.T) {
	agent := claimedAgent(identityBoundTestAgent("reviewer", time.Now()))
	config := testIdentityConfig()
	config.Enforcement = &agentidentity.EnforcementPeriod{}
	r := &TaskReconciler{Client: fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(agent).Build(), Identity: config}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "work", Namespace: testNS, CreationTimestamp: metav1.NewTime(testClaimGrantedAt.Add(time.Minute))},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent, AgentRef: &corev1alpha1.AgentReference{Name: agent.Name}},
	}
	if _, err := r.agentExecutionIdentity(context.Background(), task, agent); !errors.Is(err, errEnforcementPeriodPending) || isPermanentACPAgentConfigurationError(err) {
		t.Fatalf("binding before the period starts: err = %v, want a retryable wait", err)
	}
	bound := boundTask("bound", agent, corev1alpha1.TaskPhasePending)
	reader := fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(agent).Build()
	if _, err := boundIdentityUnavailable(context.Background(), reader, config, bound, bound.Status.AgentExecutionBinding); !errors.Is(err, errEnforcementPeriodPending) {
		t.Fatalf("pre-start check before the period starts: err = %v, want a retryable wait", err)
	}
	config.Enforcement.Start(testClaimGrantedAt.Add(-time.Hour))
	if ref, err := r.agentExecutionIdentity(context.Background(), task, agent); err != nil || ref == nil {
		t.Fatalf("binding once the period started: ref = %+v, err = %v", ref, err)
	}
}
