package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestStandaloneRuntimeCleanupRetiresPreviousControllerEpoch(t *testing.T) {
	for _, rotated := range []bool{false, true} {
		name := "same endpoint"
		if rotated {
			name = "frozen endpoint after rotation"
		}
		t.Run(name, func(t *testing.T) {
			f := newStandaloneRuntimeCleanupFixture(t, rotated, nil)
			original := f.task.DeepCopy()
			complete, err := f.base.dispatcher.cleanupRecoveredTaskScopedRuntimeSession(f.base.ctx, f.task)
			if err != nil || !complete {
				t.Fatalf("previous-epoch standalone cleanup: complete=%t err=%v", complete, err)
			}
			if f.base.deleteCalls.Load() != 1 || f.base.createCalls.Load() != 1 || f.promptCalls.Load() != 0 {
				t.Fatalf("cleanup replayed or failed to retire: creates=%d deletes=%d prompts=%d", f.base.createCalls.Load(), f.base.deleteCalls.Load(), f.promptCalls.Load())
			}
			deleted := <-f.base.deleteRequests
			if harnessv2.CompareFence(f.originalFence, deleted.Metadata.Fence, true) != harnessv2.FenceMatch {
				t.Fatal("cleanup changed the original runtime session fence")
			}
			current := f.currentTask(t)
			if !runtimeSessionCleanupCompleteForUID(current, original.UID) {
				t.Fatal("cleanup did not persist its exact receipt")
			}
			current.Status.Execution.RuntimeSessionCleanupDigest = ""
			if !reflect.DeepEqual(current.Status, original.Status) {
				t.Fatal("cleanup changed terminal outcome or mutable controller epoch")
			}
			projection, err := f.base.controlStore.GetOutboxProjection(f.base.ctx, f.projection.ID)
			if err != nil || !bytes.Equal(projection.Payload, f.projection.Payload) || projection.PayloadDigest != f.projection.PayloadDigest {
				t.Fatalf("cleanup rewrote immutable terminal evidence: %v", err)
			}
			if complete, err := f.base.dispatcher.cleanupRecoveredTaskScopedRuntimeSession(f.base.ctx, f.currentTask(t)); err != nil || !complete || f.base.deleteCalls.Load() != 1 {
				t.Fatalf("cleanup receipt was not idempotent: complete=%t err=%v deletes=%d", complete, err, f.base.deleteCalls.Load())
			}
		})
	}
}

func TestStandaloneRuntimeCleanupRejectsReplacementRegistration(t *testing.T) {
	for _, observed := range []bool{false, true} {
		name := "without observed identity"
		if observed {
			name = "with copied observed identity"
		}
		t.Run(name, func(t *testing.T) {
			f := newStandaloneRuntimeCleanupFixture(t, false, nil)
			original := f.currentTask(t)
			var replacementCalls atomic.Int32
			replacementServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				replacementCalls.Add(1)
				w.WriteHeader(http.StatusInternalServerError)
			}))
			t.Cleanup(replacementServer.Close)
			replacement := f.base.runtime.DeepCopy()
			if err := f.base.client.Delete(f.base.ctx, f.base.runtime); err != nil {
				t.Fatal(err)
			}
			replacement.ResourceVersion = ""
			replacement.UID = "replacement-runtime-uid"
			replacement.Spec.Deployment.Endpoint = replacementServer.URL
			if !observed {
				replacement.Status = corev1alpha1.AgentRuntimeStatus{}
			}
			if err := f.base.client.Create(f.base.ctx, replacement); err != nil {
				t.Fatal(err)
			}

			prepared, err := f.base.dispatcher.prepareRecoveredTaskScopedRuntimeSessionForSettlement(f.base.ctx, f.task)
			if err != nil || !prepared {
				t.Fatalf("durable terminal settlement was blocked: prepared=%t err=%v", prepared, err)
			}
			complete, err := f.base.dispatcher.cleanupRecoveredTaskScopedRuntimeSession(f.base.ctx, f.task)
			if !errors.Is(err, store.ErrConflict) || complete {
				t.Fatalf("replacement registration authorized cleanup: complete=%t err=%v", complete, err)
			}
			if replacementCalls.Load() != 0 || f.base.deleteCalls.Load() != 0 || f.base.createCalls.Load() != 1 || f.promptCalls.Load() != 0 {
				t.Fatalf("replacement changed runtime state: replacement requests=%d deletes=%d creates=%d prompts=%d", replacementCalls.Load(), f.base.deleteCalls.Load(), f.base.createCalls.Load(), f.promptCalls.Load())
			}
			if current := f.currentTask(t); !reflect.DeepEqual(current.Status, original.Status) {
				t.Fatal("replacement registration changed terminal authority or wrote a cleanup receipt")
			}
			projection, err := f.base.controlStore.GetOutboxProjection(f.base.ctx, f.projection.ID)
			if err != nil || !bytes.Equal(projection.Payload, f.projection.Payload) || projection.PayloadDigest != f.projection.PayloadDigest {
				t.Fatalf("replacement registration rewrote immutable terminal evidence: %v", err)
			}
		})
	}
}

func TestStandaloneRuntimeCleanupRequiresImmutableTerminalAuthority(t *testing.T) {
	for _, change := range []string{
		"missing projection", "wrong payload digest", "wrong aggregate", "wrong projection identity",
		"missing epoch", "zero epoch", "future epoch", "changed runtime epoch", "wrong projected Task",
		"wrong projected boot", "wrong projected session", "nonterminal attempt", "attempt binding drift",
		"nonterminal Task", "live binding drift", "live session drift", "live Task UID drift",
	} {
		t.Run(change, func(t *testing.T) {
			f := newStandaloneRuntimeCleanupFixture(t, false, nil)
			projection, attempt := *f.projection, *f.attempt
			control := &standaloneCleanupEvidenceStore{DurableControlStore: f.base.controlStore, projection: &projection, attempt: &attempt}
			f.base.dispatcher.Store = control
			var payload taskTerminalProjection
			if err := json.Unmarshal(projection.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			reencode := true
			switch change {
			case "missing projection":
				control.missing = true
			case "wrong payload digest":
				projection.PayloadDigest = testControlDigestForDispatcher("wrong-projection")
				reencode = false
			case "wrong aggregate":
				projection.AggregateID = "another-task-uid"
			case "wrong projection identity":
				projection.ProjectionKind = "another-projection"
			case "missing epoch", "zero epoch":
				payload.Execution.ControllerEpoch = 0
			case "future epoch":
				payload.Execution.ControllerEpoch = f.owner.Epoch + 1
			case "changed runtime epoch":
				payload.Execution.ControllerEpoch = f.owner.Epoch
			case "wrong projected Task":
				payload.TaskUID = "another-task-uid"
			case "wrong projected boot":
				payload.Execution.RuntimeSessionSupervisorBootID = "another-boot"
			case "wrong projected session":
				payload.Execution.RuntimeSessionUID = "another-session"
			case "nonterminal attempt":
				attempt.ExecutionState = store.PromptExecutionRunning
			case "attempt binding drift":
				attempt.BindingDigest = testControlDigestForDispatcher("other-binding")
			case "nonterminal Task":
				f.task.Status.Execution.State = corev1alpha1.TaskExecutionStateRunning
			case "live binding drift":
				f.task.Status.AgentExecutionBinding.BindingDigest = testControlDigestForDispatcher("other-binding")
			case "live session drift":
				f.task.Status.Execution.RuntimeSessionUID = "another-session"
			case "live Task UID drift":
				f.task.UID = "replacement-task-uid"
			}
			if reencode {
				encoded, err := json.Marshal(payload)
				if err != nil {
					t.Fatal(err)
				}
				projection.Payload, projection.PayloadDigest = encoded, store.CanonicalBytesDigest(encoded)
			}
			complete, err := f.base.dispatcher.cleanupRecoveredTaskScopedRuntimeSession(f.base.ctx, f.task)
			if complete {
				t.Fatalf("invalid cleanup authority returned complete: %v", err)
			}
			if f.base.deleteCalls.Load() != 0 || f.currentTask(t).Status.Execution.RuntimeSessionCleanupDigest != "" {
				t.Fatal("invalid cleanup authority sent DELETE or wrote a receipt")
			}
		})
	}
}

func TestStandaloneRuntimeCleanupRevalidatesDuringFinalStatus(t *testing.T) {
	for _, change := range []string{"binding", "projection", "leadership", "boot"} {
		t.Run(change, func(t *testing.T) {
			var armed atomic.Bool
			var probes atomic.Int32
			var f *standaloneRuntimeCleanupFixture
			var control *standaloneCleanupEvidenceStore
			mutated := make(chan error, 1)
			f = newStandaloneRuntimeCleanupFixture(t, false, func(status *harnessv2.StatusResponse) {
				if !armed.Load() || probes.Add(1) != 3 {
					return
				}
				switch change {
				case "binding":
					current := &corev1alpha1.Task{}
					err := f.base.client.Get(f.base.ctx, client.ObjectKeyFromObject(f.task), current)
					if err == nil {
						current.Status.AgentExecutionBinding.BindingDigest = testControlDigestForDispatcher("changed-during-status")
						err = f.base.client.Status().Update(f.base.ctx, current)
					}
					mutated <- err
				case "projection":
					control.changed.Store(true)
					mutated <- nil
				case "leadership":
					mutated <- f.advanceEpoch()
				case "boot":
					status.Fence.SupervisorBootID = "replacement-boot"
					mutated <- nil
				}
			})
			control = &standaloneCleanupEvidenceStore{DurableControlStore: f.base.controlStore, projection: f.projection, attempt: f.attempt}
			f.base.dispatcher.Store = control
			armed.Store(true)
			complete, err := f.base.dispatcher.cleanupRecoveredTaskScopedRuntimeSession(f.base.ctx, f.task)
			if err == nil || complete {
				t.Fatalf("authority drift during final status was accepted: complete=%t err=%v", complete, err)
			}
			select {
			case err := <-mutated:
				if err != nil {
					t.Fatal(err)
				}
			default:
				t.Fatal("cleanup never reached its final pre-delete status check")
			}
			if f.base.deleteCalls.Load() != 0 || f.currentTask(t).Status.Execution.RuntimeSessionCleanupDigest != "" {
				t.Fatal("changed cleanup authority sent DELETE or wrote a receipt")
			}
		})
	}
}

func TestStandaloneRuntimeCleanupReceiptRequiresCurrentOwner(t *testing.T) {
	f := newStandaloneRuntimeCleanupFixture(t, false, nil)
	guard := &standaloneCleanupReceiptTakeoverStore{DurableControlStore: f.base.controlStore}
	f.base.dispatcher.Store = guard
	complete, err := f.base.dispatcher.cleanupRecoveredTaskScopedRuntimeSession(f.base.ctx, f.task)
	if !errors.Is(err, store.ErrConflict) || complete || guard.calls != 1 {
		t.Fatalf("receipt did not reject takeover after DELETE: complete=%t err=%v guards=%d", complete, err, guard.calls)
	}
	if f.base.deleteCalls.Load() != 1 || f.currentTask(t).Status.Execution.RuntimeSessionCleanupDigest != "" {
		t.Fatal("stale cleanup owner wrote a receipt or failed to reach exact DELETE")
	}
}

func TestStandaloneRuntimeCleanupAuthenticatesAbsentSession(t *testing.T) {
	for _, changedBoot := range []bool{false, true} {
		name := "exact boot"
		if changedBoot {
			name = "different boot"
		}
		t.Run(name, func(t *testing.T) {
			var armed atomic.Bool
			f := newStandaloneRuntimeCleanupFixture(t, false, func(status *harnessv2.StatusResponse) {
				if armed.Load() {
					status.Sessions = nil
					status.Pressure.ResidentSessions = 0
					if changedBoot {
						status.Fence.SupervisorBootID = "replacement-boot"
					}
				}
			})
			armed.Store(true)
			complete, err := f.base.dispatcher.cleanupRecoveredTaskScopedRuntimeSession(f.base.ctx, f.task)
			if changedBoot {
				if err == nil || complete || f.currentTask(t).Status.Execution.RuntimeSessionCleanupDigest != "" {
					t.Fatalf("different boot's session absence authorized cleanup: complete=%t err=%v", complete, err)
				}
			} else if err != nil || !complete || !runtimeSessionCleanupCompleteForUID(f.currentTask(t), f.task.UID) {
				t.Fatalf("exact authenticated absence did not settle cleanup: complete=%t err=%v", complete, err)
			}
			if f.base.deleteCalls.Load() != 0 {
				t.Fatal("absent-session check sent a DELETE")
			}
		})
	}
}

type standaloneRuntimeCleanupFixture struct {
	*sessionCleanupAuthorityClientFixture
	attempt     *store.PromptAttempt
	projection  *store.OutboxProjection
	promptCalls atomic.Int32
}

func newStandaloneRuntimeCleanupFixture(t *testing.T, rotated bool, transform func(*harnessv2.StatusResponse)) *standaloneRuntimeCleanupFixture {
	t.Helper()
	f := &standaloneRuntimeCleanupFixture{}
	base := newExternalACPDispatchFixtureWithOptions(t, "standalone-cleanup", testAgentRuntimeMCPPolicy(), externalACPDispatchFixtureOptions{
		statusTransform: transform, promptObserver: func(harnessv2.StartPromptRequest) { f.promptCalls.Add(1) },
	})
	queued := base.queueTask(t, "standalone-cleanup-task", types.UID("standalone-cleanup-task-uid"), "settled", nil)
	task, bound, original := createExternalRuntimeSessionForRecovery(t, base, queued, "standalone-cleanup-create")
	originalOwner, err := base.epochs.CurrentFence(base.ctx)
	if err != nil {
		t.Fatal(err)
	}
	attemptID, err := promptAttemptIDFromTask(task)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := base.controlStore.GetPromptAttempt(base.ctx, attemptID)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []store.PromptExecutionState{
		store.PromptExecutionReserved, store.PromptExecutionSessionStarting, store.PromptExecutionPlanned,
		store.PromptExecutionSubmitting, store.PromptExecutionSubmittedUnknown, store.PromptExecutionOutcomeUnknown,
	} {
		op := "standalone-cleanup-" + string(state)
		marker := ""
		if state == store.PromptExecutionOutcomeUnknown {
			marker = "runtime loss left the execution outcome unknown"
		}
		attempt, err = base.controlStore.TransitionPromptAttemptExecution(base.ctx, store.PromptAttemptExecutionTransition{
			ID: attempt.ID, Fence: originalOwner, ExpectedVersion: attempt.Version, ExpectedState: attempt.ExecutionState,
			NewState: state, OperationID: op, OperationDigest: testControlDigestForDispatcher(op), UpdatedAt: time.Now().UTC(),
			RuntimeInstanceID: task.Status.Execution.RuntimeInstanceID, SessionUID: task.Status.Execution.RuntimeSessionUID,
			SessionLeaseGeneration: task.Status.Execution.RuntimeSessionGeneration,
			OutcomeMarker:          marker,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	task.Status.Phase = corev1alpha1.TaskPhaseFailed
	task.Status.Execution.State = corev1alpha1.TaskExecutionStateOutcomeUnknown
	task.Status.Execution.Outcome = corev1alpha1.TaskExecutionOutcomeOutcomeUnknown
	task.Status.Execution.ControllerEpoch = originalOwner.Epoch
	task.Status.Delivery = &corev1alpha1.TaskDeliveryStatus{State: corev1alpha1.TaskDeliveryStateNotRequested, Outcome: corev1alpha1.TaskDeliveryOutcomeNotRequested}
	if err := base.client.Status().Update(base.ctx, task); err != nil {
		t.Fatal(err)
	}
	payload := taskTerminalProjection{
		Namespace: task.Namespace, Task: task.Name, TaskUID: string(task.UID), Attempt: task.Status.Execution.Attempt,
		BindingDigest: task.Status.AgentExecutionBinding.BindingDigest,
		Phase:         task.Status.Phase, Execution: *task.Status.Execution.DeepCopy(), Delivery: task.Status.Delivery.DeepCopy(),
	}
	if err := enqueueDurableTaskTerminalProjection(base.ctx, base.controlStore, originalOwner, task, payload); err != nil {
		t.Fatal(err)
	}
	projection, err := base.controlStore.GetOutboxProjection(base.ctx, standaloneTaskTerminalProjectionID(task, task.Status.Execution.Attempt))
	if err != nil {
		t.Fatal(err)
	}
	epochs, stop := startACPRecoveryEpochManager(t, base.ctx, base.controlStore, "standalone-cleanup-successor")
	t.Cleanup(stop)
	base.dispatcher.Epochs = epochs
	owner, err := epochs.CurrentFence(base.ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Recovery advances mutable status; original runtime authority survives only
	// in the immutable projection and exact authenticated resident boot.
	task.Status.Execution.ControllerEpoch = owner.Epoch
	if err := base.client.Status().Update(base.ctx, task); err != nil {
		t.Fatal(err)
	}
	if rotated {
		replacement := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			t.Error("cleanup contacted replacement endpoint")
			w.WriteHeader(http.StatusInternalServerError)
		}))
		t.Cleanup(replacement.Close)
		runtime := base.runtime.DeepCopy()
		runtime.Spec.Deployment.Endpoint = replacement.URL
		runtime.Generation++
		if err := base.client.Update(base.ctx, runtime); err != nil {
			t.Fatal(err)
		}
	}
	f.sessionCleanupAuthorityClientFixture = &sessionCleanupAuthorityClientFixture{
		base: base, task: task, bound: bound, originalFence: original, owner: owner, rotated: rotated,
	}
	f.attempt, f.projection = attempt, projection
	return f
}

func (f *standaloneRuntimeCleanupFixture) currentTask(t *testing.T) *corev1alpha1.Task {
	t.Helper()
	current := &corev1alpha1.Task{}
	if err := f.base.client.Get(f.base.ctx, client.ObjectKeyFromObject(f.task), current); err != nil {
		t.Fatal(err)
	}
	return current
}

type standaloneCleanupEvidenceStore struct {
	store.DurableControlStore
	projection *store.OutboxProjection
	attempt    *store.PromptAttempt
	missing    bool
	changed    atomic.Bool
}

func (s *standaloneCleanupEvidenceStore) GetOutboxProjection(ctx context.Context, id string) (*store.OutboxProjection, error) {
	if id == s.projection.ID {
		if s.missing {
			return nil, store.ErrNotFound
		}
		copy := *s.projection
		if s.changed.Load() {
			copy.PayloadDigest = testControlDigestForDispatcher("changed-during-status")
		}
		return &copy, nil
	}
	return s.DurableControlStore.GetOutboxProjection(ctx, id)
}

func (s *standaloneCleanupEvidenceStore) GetPromptAttempt(ctx context.Context, id string) (*store.PromptAttempt, error) {
	if id == s.attempt.ID {
		return s.attempt, nil
	}
	return s.DurableControlStore.GetPromptAttempt(ctx, id)
}

func (s *standaloneCleanupEvidenceStore) GetControllerEpochFence(ctx context.Context, name string) (store.ControllerEpochFence, error) {
	return s.DurableControlStore.(interface {
		GetControllerEpochFence(context.Context, string) (store.ControllerEpochFence, error)
	}).GetControllerEpochFence(ctx, name)
}

type standaloneCleanupReceiptTakeoverStore struct {
	store.DurableControlStore
	calls int
}

func (s *standaloneCleanupReceiptTakeoverStore) GetControllerEpochFence(ctx context.Context, name string) (store.ControllerEpochFence, error) {
	return s.DurableControlStore.(interface {
		GetControllerEpochFence(context.Context, string) (store.ControllerEpochFence, error)
	}).GetControllerEpochFence(ctx, name)
}

func (s *standaloneCleanupReceiptTakeoverStore) WithControllerEpochMutation(ctx context.Context, fence store.ControllerEpochFence, fn func(context.Context) error) error {
	s.calls++
	guard := sessionCleanupReceiptTakeoverStore{DurableControlStore: s.DurableControlStore, takeover: true}
	return guard.WithControllerEpochMutation(ctx, fence, fn)
}

func TestStandaloneRuntimeCleanupBindingAcceptsRestoredSourceUID(t *testing.T) {
	f := newStandaloneRuntimeCleanupFixture(t, false, nil)
	source := f.task.UID
	restored := f.task.DeepCopy()
	restored.UID = "restored-live-uid"
	if _, err := standaloneRuntimeCleanupBinding(restored, source); err != nil {
		t.Fatalf("restored incarnation with the frozen source UID was rejected: %v", err)
	}
	if _, err := standaloneRuntimeCleanupBinding(restored, restored.UID); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("live UID that is not the frozen binding UID was accepted: %v", err)
	}
}

func TestExternalRuntimeCleanupMutationAllowedDistinguishesStandaloneAuthority(t *testing.T) {
	sessionTeardown := &externalRuntimeCleanupAuthority{sessionCleanup: &sessionRuntimeCleanupFence{}}
	standalone := &externalRuntimeCleanupAuthority{sessionCleanup: &sessionRuntimeCleanupFence{allowPublicationFinalization: true}}
	for _, test := range []struct {
		operation          string
		wantSession        bool
		wantStandalone     bool
		wantWithoutCleanup bool
	}{
		{operation: "delete_runtime_session", wantSession: true, wantStandalone: true, wantWithoutCleanup: true},
		{operation: "finalize_runtime_session_publication", wantSession: false, wantStandalone: true, wantWithoutCleanup: true},
		{operation: "cancel_prompt", wantSession: false, wantStandalone: false, wantWithoutCleanup: true},
		{operation: "create_workspace_delta", wantSession: false, wantStandalone: false, wantWithoutCleanup: true},
		{operation: "create_runtime_session", wantSession: false, wantStandalone: false, wantWithoutCleanup: false},
		{operation: "start_prompt", wantSession: false, wantStandalone: false, wantWithoutCleanup: false},
	} {
		t.Run(test.operation, func(t *testing.T) {
			if got := externalRuntimeCleanupMutationAllowed(sessionTeardown, test.operation); got != test.wantSession {
				t.Fatalf("session teardown %q = %t, want %t", test.operation, got, test.wantSession)
			}
			if got := externalRuntimeCleanupMutationAllowed(standalone, test.operation); got != test.wantStandalone {
				t.Fatalf("standalone cleanup %q = %t, want %t", test.operation, got, test.wantStandalone)
			}
			if got := externalRuntimeCleanupMutationAllowed(&externalRuntimeCleanupAuthority{}, test.operation); got != test.wantWithoutCleanup {
				t.Fatalf("frozen authority %q = %t, want %t", test.operation, got, test.wantWithoutCleanup)
			}
		})
	}
}

func TestStandaloneRuntimeCleanupFencePermitsPublicationFinalization(t *testing.T) {
	f := newStandaloneRuntimeCleanupFixture(t, false, nil)
	fence, err := f.base.dispatcher.standaloneRuntimeCleanupFence(f.base.ctx, f.currentTask(t), f.task.UID, f.owner)
	if err != nil || fence == nil {
		t.Fatalf("previous-epoch standalone cleanup fence: fence=%v err=%v", fence, err)
	}
	if !fence.allowPublicationFinalization {
		t.Fatal("standalone cleanup fence cannot finalize a prepared publication before deletion")
	}
}
