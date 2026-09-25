package controller

import (
	"errors"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func nativeLostRuntimeCleanupFixture(t *testing.T) (*nativeRuntimeTestHarness, *corev1alpha1.Task) {
	t.Helper()
	h := newNativeRuntimeTestHarness(t)
	h.r.ControllerNamespace = "native-cleanup-control"
	h.until(t, nativeTestServing)
	pool := runtimePoolTestGetPool(t, h.r, h.pool)
	task := runtimePoolRetirementTask(t, &pool, "lost-session-turn")
	task.Status.Phase = corev1alpha1.TaskPhaseFailed
	task.Status.Execution.State = corev1alpha1.TaskExecutionStateOutcomeUnknown
	task.Status.Execution.Outcome = corev1alpha1.TaskExecutionOutcomeOutcomeUnknown
	require.NoError(t, h.r.Create(t.Context(), task))
	delete(h.api.actors, h.record(t).Attempt.Name)
	h.step(t)
	pool = runtimePoolTestGetPool(t, h.r, h.pool)
	pool.Spec.DesiredReplicas = 0
	require.NoError(t, h.r.Update(t.Context(), &pool))
	require.Error(t, h.r.recordFailedNativeSubstrateTaskCleanup(t.Context(), &pool), "Actor loss alone cannot prove that its workload stopped")
	h.until(t, func(pool *corev1alpha1.RuntimePool, record *substrateNativeState) bool {
		return record.Phase == substrateNativeFailed && record.Attempt == nil && pool.Status.Lifecycle == corev1alpha1.RuntimePoolLifecycleDegraded
	})
	require.NoError(t, h.r.Get(t.Context(), client.ObjectKeyFromObject(task), task))
	require.Empty(t, task.Status.Execution.RuntimeSessionCleanupDigest)
	require.Empty(t, h.api.actors)
	require.False(t, h.api.deleteWithLivePod)
	return h, task
}

func TestNativeSubstrateFailedCleanupPreservesSessionRetirement(t *testing.T) {
	h, task := nativeLostRuntimeCleanupFixture(t)
	// A new controller must recover receipts for failures whose exact workload
	// cleanup already completed, without recreating the lost Actor or Task.
	h.r.ControllerEpoch++
	h.step(t)
	require.NoError(t, h.r.Get(t.Context(), client.ObjectKeyFromObject(task), task))
	require.True(t, runtimeSessionCleanupCompleteForUID(task, task.UID))
	require.Equal(t, corev1alpha1.TaskPhaseFailed, task.Status.Phase)
	require.Equal(t, corev1alpha1.TaskExecutionStateOutcomeUnknown, task.Status.Execution.State)
	require.Equal(t, corev1alpha1.TaskExecutionOutcomeOutcomeUnknown, task.Status.Execution.Outcome)
	require.EqualValues(t, 1, task.Status.Execution.Attempt)
	require.NotEmpty(t, h.record(t).Failure)
	require.Nil(t, h.record(t).Attempt)
	require.Empty(t, h.api.actors)
	dispatcher := &ACPDispatcher{Client: h.r.Client, APIReader: h.r.Client}
	ready, err := dispatcher.reconcileRecoveredRuntimeSession(t.Context(), task, task.UID, true, &sessionRuntimeCleanupFence{})
	require.NoError(t, err)
	require.True(t, ready, "Session deletion lost the completed native runtime retirement proof")
}

func TestNativeSubstrateFailedDeletionPreservesSessionRetirement(t *testing.T) {
	h, task := nativeLostRuntimeCleanupFixture(t)
	pool := runtimePoolTestGetPool(t, h.r, h.pool)
	require.NoError(t, h.r.Delete(t.Context(), &pool))
	originalClient := h.r.Client
	h.r.Client = &providerRetirementReceiptFailureClient{Client: originalClient, err: errors.New("injected native cleanup receipt failure")}
	_, err := h.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&pool)})
	require.ErrorContains(t, err, "injected native cleanup receipt failure")
	require.NoError(t, h.r.Get(t.Context(), client.ObjectKeyFromObject(&pool), &pool))
	require.NotNil(t, h.record(t), "pool deletion discarded the journal before preserving retirement proof")
	h.r.Client = originalClient
	nativeDeletePool(t, h, &pool)
	require.NoError(t, h.r.Get(t.Context(), client.ObjectKeyFromObject(task), task))
	require.True(t, runtimeSessionCleanupCompleteForUID(task, task.UID))
	require.Equal(t, corev1alpha1.TaskExecutionStateOutcomeUnknown, task.Status.Execution.State)
}

func TestNativeSubstrateFailedCleanupRejectsUnprovedTaskAuthority(t *testing.T) {
	for _, mismatch := range []string{"binding integrity", "runtime profile", "missing boot", "missing instance", "missing generation", "external runtime", "missing journal", "unavailable journal", "receipt write failure"} {
		t.Run(mismatch, func(t *testing.T) {
			h, task := nativeLostRuntimeCleanupFixture(t)
			pool := runtimePoolTestGetPool(t, h.r, h.pool)
			switch mismatch {
			case "binding integrity":
				task.Status.AgentExecutionBinding.BindingDigest = "changed-binding"
			case "runtime profile":
				task.Status.Execution.RuntimeSessionProfileDigest = "another-profile"
			case "missing boot":
				task.Status.Execution.RuntimeSessionSupervisorBootID = ""
			case "missing instance":
				task.Status.Execution.RuntimeInstanceID = ""
			case "missing generation":
				task.Status.Execution.RuntimeSessionGeneration = 0
			case "external runtime":
				task.Status.Execution.AgentRuntimeUID = "external-runtime-uid"
			case "missing journal":
				cm, _, err := h.r.readNativeSubstrateState(t.Context(), &pool)
				require.NoError(t, err)
				require.NoError(t, h.r.Delete(t.Context(), cm))
			case "unavailable journal":
				h.r.APIReader = recoveryJournalUnavailableReader{Reader: h.r.Client}
			}
			require.NoError(t, h.r.Status().Update(t.Context(), task))
			if mismatch == "receipt write failure" {
				h.r.Client = &providerRetirementReceiptFailureClient{Client: h.r.Client, err: errors.New("injected native cleanup receipt failure")}
			}
			require.Error(t, h.r.recordFailedNativeSubstrateTaskCleanup(t.Context(), &pool))
			require.NoError(t, h.r.Get(t.Context(), client.ObjectKeyFromObject(task), task))
			require.Empty(t, task.Status.Execution.RuntimeSessionCleanupDigest, "unverified retirement cannot release Session cleanup")
			require.Equal(t, corev1alpha1.TaskExecutionStateOutcomeUnknown, task.Status.Execution.State)
		})
	}
}

func TestNativeSubstrateDrainPreservesOriginalSessionCleanup(t *testing.T) {
	for _, operation := range []string{"scale-down", "rollout", "suspend"} {
		for _, failure := range []string{"none", "receipt write failure", "Task boot mismatch", "live descendant"} {
			t.Run(operation+"/"+failure, func(t *testing.T) {
				h := newNativeRuntimeTestHarness(t)
				h.until(t, nativeTestServing)
				original := h.record(t)
				current := runtimePoolTestGetPool(t, h.r, h.pool)
				task := runtimePoolRetirementTask(t, &current, "native-turn")
				var wantErr error
				if failure == "Task boot mismatch" {
					task.Status.Execution.RuntimeSessionSupervisorBootID = "foreign-boot"
					wantErr = store.ErrConflict
				}
				if err := h.r.Create(t.Context(), task); err != nil {
					t.Fatal(err)
				}
				if operation == "rollout" {
					// A new controller epoch rotates the Actor while retaining
					// the old instance's frozen cleanup authority.
					h.r.ControllerEpoch++
				} else {
					current.Spec.DesiredReplicas = 0
					if operation == "suspend" {
						current.Annotations[runtimePoolWorkspaceSuspendAnnotation] = booleanTrueValue
					}
					current.Generation++
					if err := h.r.Update(t.Context(), &current); err != nil {
						t.Fatal(err)
					}
				}
				h.until(t, func(*corev1alpha1.RuntimePool, *substrateNativeState) bool {
					return h.supervisor.drainCalls > 0
				})
				h.supervisor.probe.Status.Lifecycle = harnessv2.SupervisorLifecycleDraining
				h.supervisor.probe.Status.Drain = harnessv2.DrainStatus{
					Requested: true, Reason: h.supervisor.drainReason, RequestedAt: runtimePoolTestNow,
				}
				if failure == "live descendant" {
					h.supervisor.probe.Status.Pressure.LiveDescendants = 1
				}
				originalClient := h.r.Client
				if failure == "receipt write failure" {
					wantErr = errors.New("injected native retirement receipt failure")
					h.r.Client = &providerRetirementReceiptFailureClient{
						Client: originalClient, err: wantErr,
					}
				}
				_, err := h.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(h.pool)})
				if !errors.Is(err, wantErr) {
					t.Fatalf("native %s with %s = %v, want %v", operation, failure, err, wantErr)
				}
				current = runtimePoolTestGetPool(t, h.r, h.pool)
				if err := h.r.Get(t.Context(), client.ObjectKeyFromObject(task), task); err != nil {
					t.Fatal(err)
				}
				if failure != "none" {
					if current.Status.ActiveInstance == nil || current.Status.Lifecycle == corev1alpha1.RuntimePoolLifecycleQuiescent ||
						h.record(t).Pending != nil || h.api.suspends != 0 || h.api.deletes != 0 {
						t.Fatal("unproved Task cleanup advanced native runtime retirement")
					}
					if task.Status.Execution.RuntimeSessionCleanupDigest != "" {
						t.Fatal("unproved Task cleanup produced a receipt")
					}
					h.r.Client = originalClient
					task.Status.Execution.RuntimeSessionSupervisorBootID = original.Attempt.BootID
					if err := h.r.Status().Update(t.Context(), task); err != nil {
						t.Fatal(err)
					}
					h.until(t, func(pool *corev1alpha1.RuntimePool, _ *substrateNativeState) bool {
						return pool.Status.Lifecycle == corev1alpha1.RuntimePoolLifecycleQuiescent
					})
				}
				if err := h.r.Get(t.Context(), client.ObjectKeyFromObject(task), task); err != nil {
					t.Fatal(err)
				}
				if !runtimeSessionCleanupCompleteForUID(task, task.UID) {
					t.Fatal("native drain reached quiescence without preserving original Session cleanup")
				}
				if operation == "rollout" {
					h.until(t, nativeTestServing)
				} else {
					h.until(t, func(pool *corev1alpha1.RuntimePool, _ *substrateNativeState) bool {
						return pool.Status.Lifecycle == corev1alpha1.RuntimePoolLifecycleStopped && pool.Status.ActiveInstance == nil
					})
				}
				if h.api.actors[original.Attempt.Name] != nil {
					t.Fatal("native fixture did not retire the original Actor")
				}
				worker := original.Attempt.Worker
				if err := h.r.Get(t.Context(), client.ObjectKey{Namespace: worker.Namespace, Name: worker.Pod}, &corev1.Pod{}); !apierrors.IsNotFound(err) {
					t.Fatalf("original worker Pod after native retirement = %v", err)
				}
				dispatcher := &ACPDispatcher{Client: h.r.Client, APIReader: h.r.Client}
				if ready, err := dispatcher.reconcileRecoveredRuntimeSession(t.Context(), task, task.UID, true, &sessionRuntimeCleanupFence{}); err != nil || !ready {
					t.Fatalf("original Session cleanup after native %s = %v, %v", operation, ready, err)
				}
			})
		}
	}
}
