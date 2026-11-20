/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
)

// The live kind run's RuntimePool, while the Session's own session was
// resident on it, reported Serving with admission Closed at capacity. Keeping
// and reusing that very session must be allowed there, and only there.
func TestACPRuntimePoolServesItsResidentOnlyForItsOwnCapacityClosure(t *testing.T) {
	fence := persistentTestFence()
	live := func() *corev1alpha1.RuntimePool {
		pool := persistentTestPool()
		setRuntimePoolAtOwnCapacity(pool)
		return pool
	}
	if reason := acpRuntimePoolResidentRefusal(live(), "instance-1", "boot-1", fence); reason != "" {
		t.Fatalf("a pool closed only by its own resident session refused reuse: %q", reason)
	}
	accepting := persistentTestPool()
	if reason := acpRuntimePoolResidentRefusal(accepting, "instance-1", "boot-1", fence); reason != "" {
		t.Fatalf("an accepting pool refused reuse: %q", reason)
	}
	for name, mutate := range map[string]func(*corev1alpha1.RuntimePool){
		"draining": func(p *corev1alpha1.RuntimePool) {
			p.Status.Lifecycle, p.Status.AdmissionState = corev1alpha1.RuntimePoolLifecycleDraining, corev1alpha1.RuntimePoolAdmissionDraining
		},
		"degraded":      func(p *corev1alpha1.RuntimePool) { p.Status.Lifecycle = corev1alpha1.RuntimePoolLifecycleDegraded },
		"stopping":      func(p *corev1alpha1.RuntimePool) { p.Status.Lifecycle = corev1alpha1.RuntimePoolLifecycleStopping },
		"admission off": func(p *corev1alpha1.RuntimePool) { p.Status.AdmissionState = corev1alpha1.RuntimePoolAdmissionDraining },
		"closed for another reason": func(p *corev1alpha1.RuntimePool) {
			p.Status.Conditions[0].Reason = corev1alpha1.RuntimePoolReasonAdmissionClosed
		},
		"capacity condition is stale": func(p *corev1alpha1.RuntimePool) { p.Status.Conditions[0].ObservedGeneration = p.Generation - 1 },
		"status is stale":             func(p *corev1alpha1.RuntimePool) { p.Status.ObservedGeneration = p.Generation - 1 },
		"no condition":                func(p *corev1alpha1.RuntimePool) { p.Status.Conditions = nil },
		"not at capacity": func(p *corev1alpha1.RuntimePool) {
			p.Status.Capacity.ResidentSessions = 0
		},
		"two resident sessions": func(p *corev1alpha1.RuntimePool) {
			p.Status.Capacity.MaxResidentSessions, p.Status.Capacity.ResidentSessions = 2, 2
		},
		"not a workspace pool": func(p *corev1alpha1.RuntimePool) { p.Spec.ExecutionWorkspace = nil },
	} {
		t.Run(name, func(t *testing.T) {
			pool := live()
			mutate(pool)
			if reason := acpRuntimePoolResidentRefusal(pool, "instance-1", "boot-1", fence); reason != acpPersistentReasonPoolNotAccepting {
				t.Fatalf("refusal = %q, want %q", reason, acpPersistentReasonPoolNotAccepting)
			}
		})
	}
	retiring := live()
	retiring.Annotations[externalRuntimeRetirementRequestedAnnotation] = booleanTrueValue
	if reason := acpRuntimePoolResidentRefusal(retiring, "instance-1", "boot-1", fence); reason != acpPersistentReasonWorkspaceNotWarm {
		t.Fatalf("a retiring at-capacity pool refusal = %q", reason)
	}
	if reason := acpRuntimePoolResidentRefusal(live(), "instance-2", "boot-1", fence); reason != acpPersistentReasonRuntimeReplaced {
		t.Fatalf("a replaced instance on an at-capacity pool refusal = %q", reason)
	}
}

func liveDefectWorkspace(attachedTask *corev1alpha1.Task, epoch int64) *workspacev1alpha1.ExecutionWorkspace {
	return &workspacev1alpha1.ExecutionWorkspace{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: persistentCompositeNamespace, Name: persistentTestWorkspace, UID: "workspace-uid",
			Annotations: map[string]string{
				acpWorkspaceSuspendAfterDetachAnnotation:      "5m",
				acpWorkspaceDetachActionAnnotation:            string(workspacev1alpha1.WorkspaceOnDetachSuspend),
				acpWorkspaceDurableSessionCommittedAnnotation: "1",
			},
		},
		Spec: workspacev1alpha1.ExecutionWorkspaceSpec{
			DesiredState:    workspacev1alpha1.ExecutionWorkspaceDesiredReady,
			SessionRef:      &workspacev1alpha1.ObjectIdentityReference{Name: persistentCompositeSession, UID: "persistent-session-uid"},
			AttachmentEpoch: epoch,
			Attachment: &workspacev1alpha1.ExecutionWorkspaceAttachment{
				TaskRef: workspacev1alpha1.ObjectIdentityReference{Name: attachedTask.Name, UID: attachedTask.UID}, Epoch: epoch,
			},
		},
	}
}

func liveDefectTask(name, uid string, attachmentEpoch string) *corev1alpha1.Task {
	task := persistentCompositeTask(name, uid)
	task.Labels = map[string]string{acpExecutionWorkspaceLinkLabel: persistentTestWorkspace}
	task.Annotations = map[string]string{acpExecutionWorkspaceUIDAnnotation: "workspace-uid", acpTaskAttachmentEpochAnnotation: attachmentEpoch}
	return task
}

// A Claude Session turn 1 settles and turn 2 runs with the flag on, while the
// Session's single-session pool goes through the live transitions: Accepting
// when turn 1 starts, then Closed at its own capacity from the moment turn 1's
// session is resident, through turn 1's settlement and turn 2's gate. Turn 1
// keeps its session and turn 2 claims and reuses it without a new session.
func TestPersistentSessionClaudeKeepsAndReusesThroughTheLiveAtCapacityPool(t *testing.T) {
	ctx := context.Background()
	turn1 := liveDefectTask("claude-turn-1", "33333333-0000-0000-0000-000000000001", "1")
	turn2 := liveDefectTask("claude-turn-2", "33333333-0000-0000-0000-000000000002", "2")
	pool := persistentTestPool()
	pool.Namespace = persistentCompositeNamespace
	workspace := liveDefectWorkspace(turn1, 1)
	h := newPersistentCompositeHarnessWithObjects(t, []client.Object{pool, workspace}, turn1, turn2)
	fence := h.fence()
	setPool := func(mutate func(*corev1alpha1.RuntimePool)) *corev1alpha1.RuntimePool {
		t.Helper()
		current := &corev1alpha1.RuntimePool{}
		require.NoError(t, h.kube.Get(ctx, client.ObjectKeyFromObject(pool), current))
		mutate(current)
		current.Status.ActiveInstance = &corev1alpha1.RuntimePoolActiveInstanceStatus{
			RuntimeInstanceID: "runtime-instance", BootID: "supervisor-boot", ControllerEpoch: fence.Epoch,
		}
		require.NoError(t, h.kube.Status().Update(ctx, current))
		return current
	}
	// Admission is open when turn 1 starts.
	setPool(setRuntimePoolAccepting)
	bound := &verifiedAgentExecution{plan: persistentTestPlan()}

	// Turn 1: no record yet, so it plans and creates generation 1.
	attempt1 := h.reserve(turn1, fence)
	reuse1 := &acpPersistentSessionReuse{attempted: true}
	session1, err := h.prepare(turn1, fence, h.claimInput(fence, ""), reuse1)
	require.NoError(t, err)
	require.False(t, session1.Reused)
	require.Equal(t, acpPersistentReasonNoRecord, reuse1.reason)
	// Its session is now resident, which closes its single-session pool.
	livePool := setPool(setRuntimePoolAtOwnCapacity)
	from := store.PromptExecutionSessionStarting
	for _, to := range []store.PromptExecutionState{
		store.PromptExecutionPlanned, store.PromptExecutionSubmitting, store.PromptExecutionAccepted,
		store.PromptExecutionRunning, store.PromptExecutionSettling, store.PromptExecutionSucceeded,
	} {
		require.NoError(t, h.dispatcher.transitionAttempt(ctx, attempt1, fence, from, to, "complete-"+string(to), nil))
		from = to
	}
	settled, err := h.controlStore.GetPromptAttempt(ctx, attempt1)
	require.NoError(t, err)
	settled.DeliveryState = store.PromptDeliveryReadValidated
	runtimeFence := harnessv2.Fence{
		RuntimeInstanceID: "runtime-instance", SupervisorBootID: "supervisor-boot", ControllerEpoch: uint64(fence.Epoch),
		RuntimePoolUID: "pool-uid", RuntimePoolGeneration: 4,
		RuntimeSessionUID: harnessv2.RuntimeSessionUID(session1.Binding.SessionUID), RuntimeSessionGeneration: session1.Binding.Generation,
		RuntimeProfileDigest: h.profile, ProfileDigestSchemaVersion: harnessv2.ProfileDigestSchemaVersion,
	}
	supervisor := &fakePersistentSupervisor{status: harnessv2.RuntimeSessionStatus{
		RuntimeSessionID:  harnessv2.RuntimeSessionID(runtimeSessionID(runtimeFence)),
		RuntimeSessionUID: runtimeFence.RuntimeSessionUID, Generation: runtimeFence.RuntimeSessionGeneration,
		State: harnessv2.RuntimeSessionStateIdle, Quiescent: true, LiveDescendantCount: 1, LastTransitionAt: time.Now().UTC(),
	}}
	supervisor.fence = &runtimeFence
	runtimeClient := supervisor.client(t, h.profile)
	record, reason, _ := h.dispatcher.prepareACPPersistentSessionKeep(ctx, acpPersistentSessionKeepInput{
		task: turn1, bound: bound, target: acpDispatchTarget{pool: livePool}, runtimeClient: runtimeClient,
		runtimeFence: runtimeFence, fence: fence, session: session1, attemptID: attempt1, attempt: settled,
		mcpDigest: h.mcp, lineageDigest: h.lineage.ConfigDigest, workspaceDigest: testControlDigestForDispatcher("workspace"),
		intent: harnessv2.WorkspaceIntentRead, delta: harnessv2.WorkspaceDeltaDescriptor{State: harnessv2.WorkspaceDeltaNoChange},
		resultText: "answer one", rebaseSupported: true,
	})
	require.Equal(t, "", reason, "turn 1 must keep its session on the pool it alone fills")
	require.NotNil(t, record)
	session1.KeepRecord = record
	require.NoError(t, h.dispatcher.finalizeTaskSessionResult(ctx, turn1, fence, session1, "answer one", "", corev1alpha1.TaskPhaseSucceeded,
		corev1alpha1.TaskDeliveryStatus{State: corev1alpha1.TaskDeliveryStateReadValidated, Outcome: corev1alpha1.TaskDeliveryOutcomeReadValidated}))
	require.True(t, session1.kept)

	// Turn 2 attaches from the warm window and claims the record.
	current := &workspacev1alpha1.ExecutionWorkspace{}
	require.NoError(t, h.kube.Get(ctx, client.ObjectKeyFromObject(workspace), current))
	current.Spec.AttachmentEpoch = 2
	current.Spec.Attachment = &workspacev1alpha1.ExecutionWorkspaceAttachment{
		TaskRef: workspacev1alpha1.ObjectIdentityReference{Name: turn2.Name, UID: turn2.UID}, Epoch: 2,
	}
	current.Annotations[acpWorkspaceAttachedFromWarmAnnotation] = formatACPWorkspaceAttachedFromWarm(
		turn2.UID, 2, record.KeptAt.Add(4*time.Minute).Format(time.RFC3339Nano), string(turn1.UID))
	require.NoError(t, h.kube.Update(ctx, current))
	fence2 := h.fence()
	attempt2 := h.reserve(turn2, fence2)
	reserved2, err := h.controlStore.GetPromptAttempt(ctx, attempt2)
	require.NoError(t, err)
	reuseDigest, err := acpPersistentSessionReuseDigest(acpPersistentSessionReuseInputFor(bound, turn2, h.mcp, reserved2))
	require.NoError(t, err)
	require.Equal(t, record.ReuseDigest, reuseDigest)
	decision := &acpPersistentSessionReuse{attempted: true}
	session2, err := h.prepare(turn2, fence2, h.claimInput(fence2, reuseDigest), decision)
	require.NoError(t, err)
	require.NotNil(t, session2.ResidentClaimed, "turn 2 did not claim the record: %s", decision.reason)
	require.True(t, session2.Reused)
	// The pool still reports Closed at capacity: turn 1's kept session is
	// what fills its only resident slot.
	// The dispatcher's runtime fence carries no RuntimeSession at the gate.
	gateFence := runtimePoolFenceWithSession(runtimeFence, "", 0)
	refusal, _, err := h.dispatcher.acpPersistentSessionRuntimeGate(ctx, turn2, session2.ResidentClaimed, runtimeClient, gateFence, fence2, livePool)
	require.NoError(t, err)
	require.Equal(t, "", refusal, "turn 2 must reuse the session its own Session's pool still holds")
}
