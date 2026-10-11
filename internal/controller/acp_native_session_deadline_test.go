package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/events"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
	"github.com/orka-agents/orka/internal/store/storetest"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestACPDispatcherNativeRecoveryDeadlineCancellationResumesOriginalCheckpoint(t *testing.T) {
	testNativeSessionInterruptionResumesCheckpoint(t, false)
}

func TestACPDispatcherNativeRecoveryUserCancellationResumesOriginalCheckpoint(t *testing.T) {
	testNativeSessionInterruptionResumesCheckpoint(t, true)
}

func testNativeSessionInterruptionResumesCheckpoint(t *testing.T, userCancellation bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	const promptID harnessv2.PromptID = "prompt-native-deadline"
	snapshot := storetest.NativeSessionSnapshot(t, "native checkpoint before a deadline-cancelled Task")
	accepted := make(chan struct{})
	streamCancelled := make(chan struct{})
	stopPrompt := make(chan struct{})
	var acceptedOnce, cancelledOnce, stopOnce sync.Once
	var mu sync.Mutex
	var held []harnessv2.StartPromptRequest
	var cancellations []harnessv2.CancelPromptRequest
	var cancelPath string
	fixture, observed := newNativeRecoveryFixture(t, ctx, "native-deadline-then-resume", snapshot, nil,
		func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				expectedCancelPath := cancelPath
				mu.Unlock()
				if r.Method == http.MethodPut && expectedCancelPath != "" && r.URL.Path == expectedCancelPath {
					var request harnessv2.CancelPromptRequest
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Errorf("decode deadline cancellation: %v", err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					if err := request.ValidateAt(time.Now().UTC()); err != nil {
						t.Errorf("invalid deadline cancellation: %v", err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					mu.Lock()
					cancellations = append(cancellations, request)
					mu.Unlock()
					// Cancellation stops the simulated provider directly. Requiring TCP
					// disconnect first makes settlement depend on HTTP transport timing.
					stopOnce.Do(func() { close(stopPrompt) })
					select {
					case <-streamCancelled:
					case <-r.Context().Done():
						return
					}
					writeDispatcherJSON(w, harnessv2.CancelPromptResponse{
						Protocol: harnessv2.ProtocolVersion, Classification: harnessv2.Classification{Class: harnessv2.RequestClassificationFresh},
						BarrierState: harnessv2.CancellationBarrierSettled, SettlementProven: true,
						Settlement: harnessv2.PromptSettlement{
							TerminalEvent: harnessv2.EventCancelled, Outcome: harnessv2.PromptOutcomeCancelled,
							StopReason: harnessv2.ACPStopReasonCancelled, SettledAt: time.Now().UTC(),
						},
					})
					return
				}
				if r.Method != http.MethodPut || !strings.HasSuffix(r.URL.Path, "/prompts/"+string(promptID)) {
					next.ServeHTTP(w, r)
					return
				}
				var request harnessv2.StartPromptRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Errorf("decode held native prompt: %v", err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				path, err := harnessv2.PromptPath(harnessv2.RuntimeSessionID(runtimeSessionID(request.Metadata.Fence)), promptID)
				if err != nil || r.URL.Path != path || request.Metadata.PromptID != promptID {
					t.Errorf("held native prompt has a mismatched path or identity: %v", err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				mu.Lock()
				held = append(held, request)
				cancelPath = path + "/cancel"
				mu.Unlock()
				w.Header().Set("Content-Type", harnessv2.NDJSONMediaType)
				limits := harnessv2.DefaultProtocolLimits()
				encoder, err := harnessv2.NewEventEncoder(w, harnessv2.EventStreamLimits{
					MaxLineBytes: limits.MaxEventLineBytes, MaxTerminalResultBytes: limits.MaxTerminalResultBytes,
					MaxBufferedEvents: limits.MaxBufferedEvents, MaxUpdateEventsPerSecond: limits.MaxUpdateEventsPerSecond,
				}, harnessv2.EventExpectationFromMetadata(request.Metadata))
				if err != nil {
					t.Errorf("held native prompt encoder: %v", err)
					return
				}
				now := time.Now().UTC()
				if err := encoder.Encode(harnessv2.Event{
					Protocol: harnessv2.ProtocolVersion, Type: harnessv2.EventAccepted,
					Identity: harnessv2.EventIdentity{
						RuntimeInstanceID: request.Metadata.Fence.RuntimeInstanceID, SupervisorBootID: request.Metadata.Fence.SupervisorBootID,
						RuntimeSessionUID: request.Metadata.Fence.RuntimeSessionUID, RuntimeSessionGeneration: request.Metadata.Fence.RuntimeSessionGeneration,
						TaskUID: request.Metadata.TaskUID, TaskAttempt: request.Metadata.TaskAttempt, PromptID: request.Metadata.PromptID,
						Sequence: 1, RequestDigest: request.Metadata.RequestDigest, Timestamp: now,
					},
					Accepted: &harnessv2.AcceptedEvent{AcceptedAt: now, Lease: request.Lease, ACPVersion: harnessv2.ACPProfileV1},
				}); err != nil {
					t.Errorf("encode held native prompt acceptance: %v", err)
					return
				}
				w.(http.Flusher).Flush()
				acceptedOnce.Do(func() { close(accepted) })
				select {
				case <-stopPrompt:
				case <-r.Context().Done():
				}
				cancelledOnce.Do(func() { close(streamCancelled) })
			})
		})
	defer fixture.stop()
	persistence := fixture.dispatcher.ResultStore.(*sqlite.Store)
	dispatchQueuedTask(ctx, t, fixture.dispatcher, fixture.task)
	first := fixture.currentTask(t, ctx)
	require.Equal(t, corev1alpha1.TaskPhaseSucceeded, first.Status.Phase)
	original := nativeRecoveryCheckpoint(t, ctx, persistence, first.Spec.SessionRef.Name, 2)
	require.Equal(t, snapshot.Data, original.Snapshot.Data)
	require.Equal(t, snapshot.ProviderSessionID, original.Snapshot.ProviderSessionID)

	timedOut := newNativeRecoveryContinuation(t, ctx, fixture, "native-deadline-turn", "88888888-8888-8888-8888-888888888888", string(promptID))
	attemptID, err := promptAttemptIDFromTask(timedOut)
	require.NoError(t, err)
	deadlineCancels := make(chan context.CancelCauseFunc, 1)
	wantReason := corev1alpha1.TaskExecutionReason(acpTaskTimeoutReason)
	wantMessage := acpTaskTimeoutCancellationSettledMessage
	wantCancel := harnessv2.CancelReasonTaskTimeout
	fixture.dispatcher.runtimeContextFactory = func(parent context.Context, task *corev1alpha1.Task) (context.Context, context.CancelFunc) {
		require.Equal(t, timedOut.UID, task.UID)
		runtimeCtx, cancelCause := context.WithCancelCause(parent)
		deadlineCancels <- cancelCause
		return runtimeCtx, func() { cancelCause(context.Canceled) }
	}
	var cancelAfterRunning <-chan error
	if userCancellation {
		fixture.dispatcher.runtimeContextFactory = nil
		wantReason, wantMessage, wantCancel = "Cancelled", "prompt cancellation settled", harnessv2.CancelReasonControllerShutdown
		cancelAfterRunning = cancelNativeTaskAfterPromptRunning(ctx, fixture, persistence, timedOut, attemptID, accepted)
	} else {
		cancelAfterRunning = cancelRuntimeContextAfterPromptRunning(ctx, persistence, attemptID, accepted, deadlineCancels)
	}
	reserved, target, err := fixture.dispatcher.reserveTask(ctx, timedOut)
	require.NoError(t, err)
	require.NotNil(t, reserved)
	dispatchErr := fixture.dispatcher.executeReservedTask(ctx, reserved, target)
	require.NoError(t, <-cancelAfterRunning)
	current := &corev1alpha1.Task{}
	require.NoError(t, fixture.kubeClient.Get(ctx, client.ObjectKeyFromObject(timedOut), current))
	require.Equal(t, corev1alpha1.TaskPhaseCancelled, current.Status.Phase)
	require.Equal(t, corev1alpha1.TaskExecutionStateCancelled, current.Status.Execution.State)
	require.Equal(t, corev1alpha1.TaskExecutionOutcomeCancelled, current.Status.Execution.Outcome)
	require.Equal(t, wantReason, current.Status.Execution.Reason)
	require.Equal(t, wantMessage, current.Status.Execution.Message)
	require.Empty(t, current.Annotations[nativeCaptureIntentAnnotation])
	attempt, err := persistence.GetPromptAttempt(ctx, attemptID)
	require.NoError(t, err)
	require.Equal(t, store.PromptExecutionCancelled, attempt.ExecutionState)
	require.Equal(t, string(wantReason), attempt.TerminalReason)
	require.Equal(t, wantMessage, attempt.OutcomeMarker)
	turn := nativeRecoveryTurn(t, ctx, persistence, attemptID)
	require.Equal(t, store.SessionTurnFinalized, turn.State)
	require.Equal(t, store.SessionTurnOutcomeMarker, turn.TerminalKind)
	require.Equal(t, store.NativeSessionCaptureDigest(original), turn.NativeSessionDigest)
	carried := nativeRecoveryCheckpoint(t, ctx, persistence, first.Spec.SessionRef.Name, 4)
	require.Equal(t, original.Snapshot, carried.Snapshot)
	require.Equal(t, original.SourceOperationID, carried.SourceOperationID)
	require.Equal(t, original.RuntimeSessionGeneration, carried.RuntimeSessionGeneration)
	require.NotEqual(t, original.ThroughMessageID, carried.ThroughMessageID)
	control, err := persistence.GetSessionControl(ctx, current.Namespace, current.Spec.SessionRef.Name)
	require.NoError(t, err)
	require.Nil(t, control.Lease)
	mu.Lock()
	heldPrompts := append([]harnessv2.StartPromptRequest(nil), held...)
	cancelRequests := append([]harnessv2.CancelPromptRequest(nil), cancellations...)
	mu.Unlock()
	require.Len(t, heldPrompts, 1)
	require.Len(t, cancelRequests, 1)
	request, heldPrompt := cancelRequests[0], heldPrompts[0]
	require.Equal(t, wantCancel, request.Reason)
	require.Equal(t, heldPrompt.Metadata.Fence, request.Metadata.Fence)
	require.Equal(t, heldPrompt.Metadata.TaskUID, request.Metadata.TaskUID)
	require.Equal(t, heldPrompt.Metadata.TaskAttempt, request.Metadata.TaskAttempt)
	require.Equal(t, promptID, request.Metadata.PromptID)
	journal, err := persistence.ListExecutionEvents(ctx, store.ExecutionEventFilter{
		Namespace: current.Namespace, StreamType: events.ExecutionEventStreamTypeTask, StreamID: current.Name, Limit: 100,
	})
	require.NoError(t, err)
	var settlements int
	for _, event := range journal {
		if event.Type != events.ExecutionEventTypeModelRequestFailed {
			continue
		}
		var content map[string]any
		require.NoError(t, json.Unmarshal(event.Content, &content))
		require.Equal(t, string(harnessv2.EventCancelled), content["terminalEvent"])
		require.Equal(t, true, content["controllerSynthesized"])
		require.Equal(t, true, content["settlementProven"])
		settlements++
	}
	require.Equal(t, 1, settlements)
	requireNativeRecoveryNoFallback(t, ctx, persistence, timedOut)
	func() {
		observed.mu.Lock()
		defer observed.mu.Unlock()
		require.Len(t, observed.creates, 2)
		require.Len(t, observed.prompts, 1, "the held prompt bypasses the parent runtime handler")
		require.Len(t, observed.captures, 1, "timeout must carry the old checkpoint, not capture cancelled state")
		t.Logf("timeout settled: cancel=%s, turn=%s, checkpoint messages=%d, lease released=%t, runtime deletes=%d, cleanup complete=%t",
			request.Reason, turn.State, carried.MessageCount, control.Lease == nil, len(observed.deletes), taskScopedRuntimeSessionCleanupComplete(current))
	}()
	// Cleanup must follow the durable carry-forward in this same dispatch, not
	// fail before its later Session finalization defer has settled the turn.
	require.NoError(t, dispatchErr)
	require.True(t, taskScopedRuntimeSessionCleanupComplete(current))
	require.Empty(t, nativeRecoveryRuntimeStatus(t, ctx, fixture.dispatcher, current).Sessions)
	func() {
		observed.mu.Lock()
		defer observed.mu.Unlock()
		require.Len(t, observed.deletes, 2, "the cancelled runtime must be deleted before continuation")
		require.Equal(t, *carried, observed.deletedNative[1])
	}()

	// A fresh dispatcher has no cached runtime binding to mask a lost checkpoint.
	restarted := newNativeRecoveryDispatcher(t, fixture, persistence)
	continued := newNativeRecoveryContinuation(t, ctx, fixture, "native-after-deadline", "99999999-9999-9999-9999-999999999999", "prompt-native-after-deadline")
	dispatchQueuedTask(ctx, t, restarted, continued)
	require.NoError(t, fixture.kubeClient.Get(ctx, client.ObjectKeyFromObject(continued), current))
	require.Equal(t, corev1alpha1.TaskPhaseSucceeded, current.Status.Phase)
	require.True(t, taskScopedRuntimeSessionCleanupComplete(current))
	require.Empty(t, nativeRecoveryRuntimeStatus(t, ctx, restarted, current).Sessions)
	latest := nativeRecoveryCheckpoint(t, ctx, persistence, continued.Spec.SessionRef.Name, 6)
	require.Equal(t, original.Snapshot.Data, latest.Snapshot.Data)
	require.Equal(t, original.Snapshot.ProviderSessionID, latest.Snapshot.ProviderSessionID)
	control, err = persistence.GetSessionControl(ctx, current.Namespace, current.Spec.SessionRef.Name)
	require.NoError(t, err)
	require.Nil(t, control.Lease)
	observed.mu.Lock()
	defer observed.mu.Unlock()
	require.Len(t, observed.creates, 3)
	require.Nil(t, observed.creates[0].NativeRestore)
	require.Len(t, observed.prompts, 2)
	require.Len(t, observed.captures, 2)
	require.Len(t, observed.deletes, 3)
	wantRestore := original.Snapshot
	wantRestore.WorkingDirectory = "/workspace"
	for index, task := range []*corev1alpha1.Task{timedOut, continued} {
		create := observed.creates[index+1]
		require.NotNil(t, create.NativeRestore)
		require.Equal(t, wantRestore, create.NativeRestore.Snapshot)
		require.Equal(t, original.SessionUID, string(create.Metadata.Fence.RuntimeSessionUID))
		require.Greater(t, create.Metadata.Fence.RuntimeSessionGeneration, observed.creates[index].Metadata.Fence.RuntimeSessionGeneration)
		prompt := heldPrompt
		if index == 1 {
			prompt = observed.prompts[1]
		}
		require.Len(t, prompt.Input.Content, 1, "native continuation must not bootstrap from the canonical transcript")
		require.Equal(t, task.Spec.Prompt, prompt.Input.Content[0].Text)
	}
	for index, deleted := range observed.deletes {
		require.Equal(t, observed.creates[index].Metadata.Fence.RuntimeSessionUID, deleted.Metadata.Fence.RuntimeSessionUID)
		require.Equal(t, observed.creates[index].Metadata.Fence.RuntimeSessionGeneration, deleted.Metadata.Fence.RuntimeSessionGeneration)
	}
	for _, task := range []*corev1alpha1.Task{first, timedOut, continued} {
		requireNativeRecoveryNoFallback(t, ctx, persistence, task)
	}
}

func cancelNativeTaskAfterPromptRunning(
	ctx context.Context,
	fixture *taskScopedCreateConflictFixture,
	persistence *sqlite.Store,
	timedOut *corev1alpha1.Task,
	attemptID string,
	accepted <-chan struct{},
) <-chan error {
	cancelled := make(chan error, 1)
	go func() {
		select {
		case <-accepted:
		case <-ctx.Done():
			cancelled <- ctx.Err()
			return
		}
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			attempt, err := persistence.GetPromptAttempt(ctx, attemptID)
			if err != nil {
				cancelled <- err
				return
			}
			if attempt.ExecutionState == store.PromptExecutionRunning {
				target := &corev1alpha1.Task{}
				if err := fixture.kubeClient.Get(ctx, client.ObjectKeyFromObject(timedOut), target); err != nil {
					cancelled <- err
					return
				}
				target.Status.Phase = corev1alpha1.TaskPhaseCancelled
				cancelled <- fixture.kubeClient.Status().Update(ctx, target)
				return
			}
			select {
			case <-ctx.Done():
				cancelled <- ctx.Err()
				return
			case <-ticker.C:
			}
		}
	}()
	return cancelled
}
