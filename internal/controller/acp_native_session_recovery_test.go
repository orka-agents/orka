package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/events"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
	"github.com/orka-agents/orka/internal/store/storetest"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The transaction succeeds, but its caller never receives the commit receipt.
// Returning an error here leaves the live dispatcher unable to authorize DELETE.
// A fresh dispatcher must recover solely from the SQLite SessionTurn receipt.
type nativeRecoveryCommitResponseLossStore struct {
	store.DurableControlStore
	loseResponse atomic.Bool
	commits      atomic.Int32
	resumes      atomic.Int32
}

func (s *nativeRecoveryCommitResponseLossStore) FinalizeSessionTurn(ctx context.Context, request store.FinalizeSessionTurnRequest) (*store.SessionTurn, error) {
	turn, err := s.DurableControlStore.FinalizeSessionTurn(ctx, request)
	if err == nil && request.NativeSession != nil {
		s.commits.Add(1)
		if s.loseResponse.Swap(false) {
			return nil, errors.New("injected loss of committed native checkpoint response")
		}
	}
	return turn, err
}

func (s *nativeRecoveryCommitResponseLossStore) ResumeSessionTurnFinalization(ctx context.Context, request store.ResumeSessionTurnFinalizationRequest) (*store.SessionTurn, error) {
	s.resumes.Add(1)
	return s.DurableControlStore.ResumeSessionTurnFinalization(ctx, request)
}

type nativeRecoveryRuntimeObservations struct {
	mu            sync.Mutex
	creates       []harnessv2.CreateRuntimeSessionRequest
	prompts       []harnessv2.StartPromptRequest
	captures      []harnessv2.CaptureNativeSessionRequest
	deletes       []harnessv2.DeleteRuntimeSessionRequest
	captureLeases []store.SessionMutationLease
	deletedNative []store.NativeSessionRecord
}

func newNativeRecoveryDispatcher(t *testing.T, fixture *taskScopedCreateConflictFixture, controls store.DurableControlStore) *ACPDispatcher {
	t.Helper()
	persistence := fixture.dispatcher.ResultStore.(*sqlite.Store)
	continuity, err := NewACPSessionContinuity(ACPSessionContinuityConfig{
		SessionControls: controls, Transcripts: persistence, Publications: persistence, BranchClaims: persistence,
		NewSessionUID: func() (string, error) { return "native-recovery-owner", nil },
	})
	require.NoError(t, err)
	return &ACPDispatcher{
		Client: fixture.kubeClient, APIReader: fixture.kubeClient, Store: controls,
		ResultStore: persistence, EventStore: persistence, PlanStore: persistence,
		Snapshots: persistence, Sessions: continuity, Epochs: fixture.dispatcher.Epochs,
	}
}

func newNativeRecoveryFixture(
	t *testing.T,
	ctx context.Context,
	name string,
	snapshot harnessv2.NativeSessionSnapshot,
	terminals map[harnessv2.PromptID]harnessv2.EventType,
	wrap func(http.Handler) http.Handler,
) (*taskScopedCreateConflictFixture, *nativeRecoveryRuntimeObservations) {
	t.Helper()
	observed := &nativeRecoveryRuntimeObservations{}
	var persistence *sqlite.Store
	fixture := newTaskScopedCreateConflictFixture(t, ctx, name, "77777777-7777-7777-7777-777777777777",
		func(profile harnessv2.RuntimeProfile, digest harnessv2.ProfileDigest, _ *client.Client) *httptest.Server {
			server := newDispatcherRuntimeServerWithOptions(t, profile, digest, dispatcherRuntimeServerOptions{
				nativeSnapshot: &snapshot, terminalEvents: terminals,
				onPrompt: func(request harnessv2.StartPromptRequest) {
					observed.mu.Lock()
					defer observed.mu.Unlock()
					observed.prompts = append(observed.prompts, request)
				},
				onNativeCapture: func(request harnessv2.CaptureNativeSessionRequest) error {
					control, err := persistence.GetSessionControl(ctx, "default", name)
					if err != nil {
						return err
					}
					if control.Lease == nil {
						return errors.New("native capture reached runtime without Session lease")
					}
					observed.mu.Lock()
					defer observed.mu.Unlock()
					observed.captures = append(observed.captures, request)
					observed.captureLeases = append(observed.captureLeases, *control.Lease)
					return nil
				},
				onDelete: func(request harnessv2.DeleteRuntimeSessionRequest) {
					record, err := persistence.GetNativeSession(ctx, "default", name, string(request.Metadata.Fence.RuntimeSessionUID))
					if err != nil {
						t.Errorf("runtime deleted without loadable native checkpoint: %v", err)
						return
					}
					observed.mu.Lock()
					defer observed.mu.Unlock()
					observed.deletes = append(observed.deletes, request)
					observed.deletedNative = append(observed.deletedNative, *record)
				},
			}, func(request harnessv2.CreateRuntimeSessionRequest) {
				if err := persistence.BindSessionCleanupIdentity(ctx, "default", name, string(request.Metadata.Fence.RuntimeSessionUID)); err != nil {
					t.Errorf("bind native checkpoint cleanup identity: %v", err)
				}
				observed.mu.Lock()
				defer observed.mu.Unlock()
				observed.creates = append(observed.creates, request)
			})
			if wrap == nil {
				return server
			}
			t.Cleanup(server.Close)
			return httptest.NewServer(wrap(server.Config.Handler))
		}, func(task *corev1alpha1.Task) {
			task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: name, Create: true, Append: true}
		})
	persistence = fixture.dispatcher.ResultStore.(*sqlite.Store)
	fixture.dispatcher = newNativeRecoveryDispatcher(t, fixture, persistence)
	return fixture, observed
}

func nativeRecoveryTurn(t *testing.T, ctx context.Context, persistence *sqlite.Store, attemptID string) *store.SessionTurn {
	t.Helper()
	attempt, err := persistence.GetPromptAttempt(ctx, attemptID)
	require.NoError(t, err)
	id, err := (store.SessionTurnKey{
		SessionUID: attempt.SessionUID, LeaseGeneration: attempt.SessionLeaseGeneration,
		TaskUID: attempt.Key.TaskUID, Attempt: attempt.Key.Attempt, PromptID: attempt.Key.PromptID,
	}).CanonicalID()
	require.NoError(t, err)
	turn, err := persistence.GetSessionTurn(ctx, id)
	require.NoError(t, err)
	return turn
}

func nativeRecoveryCheckpoint(t *testing.T, ctx context.Context, persistence *sqlite.Store, name string, count int) *store.NativeSessionRecord {
	t.Helper()
	control, err := persistence.GetSessionControl(ctx, "default", name)
	require.NoError(t, err)
	record, err := persistence.GetNativeSession(ctx, control.Namespace, control.SessionName, control.SessionUID)
	require.NoError(t, err)
	history, err := persistence.LoadTranscript(ctx, control.Namespace, control.SessionName, 100)
	require.NoError(t, err)
	require.Len(t, history, count)
	require.Equal(t, count, record.MessageCount)
	require.Equal(t, history[len(history)-1].ID, record.ThroughMessageID)
	require.Equal(t, control.SessionUID, record.SessionUID)
	return record
}

func nativeRecoveryRuntimeStatus(t *testing.T, ctx context.Context, dispatcher *ACPDispatcher, task *corev1alpha1.Task) *harnessv2.StatusResponse {
	t.Helper()
	pool := &corev1alpha1.RuntimePool{}
	require.NoError(t, dispatcher.APIReader.Get(ctx, client.ObjectKey{Namespace: task.Namespace, Name: task.Status.Execution.RuntimePoolName}, pool))
	runtimeClient, _, _, _, err := dispatcher.runtimePoolClient(ctx, pool)
	require.NoError(t, err)
	status, err := runtimeClient.Status(ctx)
	require.NoError(t, err)
	return status
}

func requireNativeRecoveryNoFallback(t *testing.T, ctx context.Context, persistence *sqlite.Store, task *corev1alpha1.Task) {
	t.Helper()
	journal, err := persistence.ListExecutionEvents(ctx, store.ExecutionEventFilter{
		Namespace: task.Namespace, StreamType: events.ExecutionEventStreamTypeTask, StreamID: task.Name, Limit: 100,
	})
	require.NoError(t, err)
	for _, event := range journal {
		require.NotEqual(t, events.ExecutionEventTypeNativeSessionCaptureSkipped, event.Type)
	}
}

func newNativeRecoveryContinuation(t *testing.T, ctx context.Context, fixture *taskScopedCreateConflictFixture, name string, uid types.UID, promptID string) *corev1alpha1.Task {
	t.Helper()
	continued := fixture.task.DeepCopy()
	continued.Name, continued.UID, continued.ResourceVersion = name, uid, ""
	continued.Spec.Prompt = "continue " + name
	continued.Spec.SessionRef.Create = false
	continued.Status = corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending, Attempts: 1, Execution: &corev1alpha1.TaskExecutionStatus{
		State: corev1alpha1.TaskExecutionStateQueued, Attempt: 1, PromptID: promptID,
		RuntimePoolName: fixture.task.Status.Execution.RuntimePoolName, RuntimePoolUID: "pool-uid", ControllerEpoch: 1,
		RequestDigest: testControlDigestForDispatcher(name),
	}}
	require.NoError(t, fixture.kubeClient.Create(ctx, continued))
	persistence := fixture.dispatcher.ResultStore.(*sqlite.Store)
	agent := &corev1alpha1.Agent{}
	require.NoError(t, fixture.kubeClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: "agent"}, agent))
	continued = prepareBoundACPDispatcherTaskForTest(t, ctx, fixture.kubeClient, fixture.kubeClient.Scheme(), persistence, continued, agent,
		ACPRuntimeImages{Codex: "docker.io/example/acp@sha256:" + strings.Repeat("a", 64)})
	fence, err := fixture.dispatcher.Epochs.CurrentFence(ctx)
	require.NoError(t, err)
	key := store.PromptAttemptKey{Namespace: continued.Namespace, TaskUID: string(continued.UID), Attempt: 1, PromptID: promptID}
	id, err := key.CanonicalID()
	require.NoError(t, err)
	_, err = persistence.CreatePromptAttempt(ctx, boundPromptAttemptForTest(&store.PromptAttempt{
		ID: id, Key: key, RequestDigest: continued.Status.Execution.RequestDigest,
		BindingDigest: continued.Status.AgentExecutionBinding.BindingDigest, SnapshotDigest: continued.Status.AgentExecutionBinding.Snapshot.Digest,
		ExecutionState: store.PromptExecutionQueued, DeliveryState: store.PromptDeliveryNotRequested,
	}), fence)
	require.NoError(t, err)
	return continued
}

func TestACPDispatcherNativeRecoveryLostCaptureResponse(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	snapshot := storetest.NativeSessionSnapshot(t, "completed native conversation with a lost capture response")
	var receipt harnessv2.CaptureNativeSessionResponse
	var sends []harnessv2.CaptureNativeSessionRequest
	var receiptMu sync.Mutex
	var allowReceipt atomic.Bool
	fixture, observed := newNativeRecoveryFixture(t, ctx, "native-lost-capture-response", snapshot, nil,
		func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/native-session") {
					next.ServeHTTP(w, r)
					return
				}
				receiptMu.Lock()
				defer receiptMu.Unlock()
				// Read the actual request without changing what the fixture sees.
				var request harnessv2.CaptureNativeSessionRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Errorf("decode capture request: %v", err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				body, err := json.Marshal(request)
				if err != nil {
					t.Errorf("encode capture request: %v", err)
					return
				}
				r.Body = io.NopCloser(bytes.NewReader(body))
				sends = append(sends, request)
				if len(sends) == 1 {
					captured := httptest.NewRecorder()
					next.ServeHTTP(captured, r)
					if captured.Code != http.StatusOK {
						t.Errorf("capture did not apply before response loss: HTTP %d", captured.Code)
						return
					}
					if err := json.Unmarshal(captured.Body.Bytes(), &receipt); err != nil {
						t.Errorf("decode applied capture receipt: %v", err)
						return
					}
				}
				if request.Metadata.OperationID != sends[0].Metadata.OperationID || request.Metadata.RequestDigest != sends[0].Metadata.RequestDigest {
					t.Error("recovery changed the surviving native capture identity")
					w.WriteHeader(http.StatusConflict)
					return
				}
				// Keep the receipt unavailable through the live dispatcher's own
				// finalization retries. Only the fresh dispatcher may retrieve it.
				if !allowReceipt.Load() {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"protocol":`))
					return
				}
				receipt.Classification = harnessv2.Classification{Class: harnessv2.RequestClassificationDuplicate, Phase: harnessv2.OperationPhaseApplied}
				writeDispatcherJSON(w, receipt)
			})
		})
	defer fixture.stop()
	persistence := fixture.dispatcher.ResultStore.(*sqlite.Store)
	reserved, target, err := fixture.dispatcher.reserveTask(ctx, fixture.task)
	require.NoError(t, err)
	require.NotNil(t, reserved)
	require.ErrorContains(t, fixture.dispatcher.executeReservedTask(ctx, reserved, target), "capture private native Session")
	current := fixture.currentTask(t, ctx)
	intentJSON := current.Annotations[nativeCaptureIntentAnnotation]
	require.NotEmpty(t, intentJSON)
	var intent nativeCaptureIntent
	require.NoError(t, json.Unmarshal([]byte(intentJSON), &intent))
	control, err := persistence.GetSessionControl(ctx, current.Namespace, current.Spec.SessionRef.Name)
	require.NoError(t, err)
	require.NotNil(t, control.Lease)
	_, err = persistence.GetNativeSession(ctx, control.Namespace, control.SessionName, control.SessionUID)
	require.ErrorIs(t, err, store.ErrNotFound)
	require.Equal(t, store.SessionTurnOpen, nativeRecoveryTurn(t, ctx, persistence, fixture.attemptID).State)
	resident := nativeRecoveryRuntimeStatus(t, ctx, fixture.dispatcher, current)
	require.Len(t, resident.Sessions, 1)
	require.Equal(t, harnessv2.RuntimeSessionStatePoisoned, resident.Sessions[0].State)
	require.Equal(t, intent.Request.Metadata.Fence.RuntimeSessionUID, resident.Sessions[0].RuntimeSessionUID)
	require.Equal(t, intent.Request.Metadata.Fence.RuntimeSessionGeneration, resident.Sessions[0].Generation)
	observed.mu.Lock()
	require.Len(t, observed.prompts, 1)
	require.Len(t, observed.captures, 1)
	require.Empty(t, observed.deletes)
	require.Equal(t, *control.Lease, observed.captureLeases[0])
	observed.mu.Unlock()

	receiptMu.Lock()
	requestsBeforeRecovery := len(sends)
	receiptMu.Unlock()
	allowReceipt.Store(true)
	restarted := newNativeRecoveryDispatcher(t, fixture, persistence)
	fence, err := restarted.Epochs.CurrentFence(ctx)
	require.NoError(t, err)
	require.NoError(t, restarted.recoverStaleTask(ctx, current, fence))
	checkpoint := nativeRecoveryCheckpoint(t, ctx, persistence, current.Spec.SessionRef.Name, 2)
	require.Equal(t, snapshot.Data, checkpoint.Snapshot.Data)
	require.Equal(t, snapshot.DataDigest, checkpoint.Snapshot.DataDigest)
	require.Equal(t, snapshot.ProviderSessionID, checkpoint.Snapshot.ProviderSessionID)
	require.Equal(t, string(intent.Request.Metadata.OperationID), checkpoint.SourceOperationID)
	require.Equal(t, int64(intent.Request.Metadata.Fence.RuntimeSessionGeneration), checkpoint.RuntimeSessionGeneration)
	completed := fixture.currentTask(t, ctx)
	require.True(t, taskScopedRuntimeSessionCleanupComplete(completed))
	require.Equal(t, intentJSON, completed.Annotations[nativeCaptureIntentAnnotation])
	require.Empty(t, nativeRecoveryRuntimeStatus(t, ctx, restarted, completed).Sessions)
	control, err = persistence.GetSessionControl(ctx, control.Namespace, control.SessionName)
	require.NoError(t, err)
	require.Nil(t, control.Lease)
	require.Equal(t, store.SessionTurnFinalized, nativeRecoveryTurn(t, ctx, persistence, fixture.attemptID).State)
	// A second recovery must not resubmit the prompt, retrieve another capture,
	// duplicate transcript messages, or delete a later runtime generation.
	require.NoError(t, restarted.recoverStaleTask(ctx, completed, fence))
	require.Equal(t, checkpoint, nativeRecoveryCheckpoint(t, ctx, persistence, control.SessionName, 2))
	receiptMu.Lock()
	require.GreaterOrEqual(t, requestsBeforeRecovery, 1)
	require.Len(t, sends, requestsBeforeRecovery+1, "fresh recovery retrieves the immutable receipt exactly once")
	for _, request := range sends {
		require.Equal(t, intent.Request, request)
	}
	receiptMu.Unlock()
	observed.mu.Lock()
	defer observed.mu.Unlock()
	require.Len(t, observed.prompts, 1)
	require.Len(t, observed.captures, 1, "receipt retrieval must not capture the stopped child again")
	require.Len(t, observed.deletes, 1)
	require.Equal(t, *checkpoint, observed.deletedNative[0])
	require.Equal(t, intent.Request.Metadata.Fence.RuntimeSessionUID, observed.deletes[0].Metadata.Fence.RuntimeSessionUID)
	require.Equal(t, intent.Request.Metadata.Fence.RuntimeSessionGeneration, observed.deletes[0].Metadata.Fence.RuntimeSessionGeneration)
	requireNativeRecoveryNoFallback(t, ctx, persistence, current)
}

func TestACPDispatcherNativeRecoveryCommittedCheckpointBeforeDelete(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	snapshot := storetest.NativeSessionSnapshot(t, "checkpoint committed before dispatcher receives its receipt")
	fixture, observed := newNativeRecoveryFixture(t, ctx, "native-committed-before-delete", snapshot, nil, nil)
	defer fixture.stop()
	persistence := fixture.dispatcher.ResultStore.(*sqlite.Store)
	lost := &nativeRecoveryCommitResponseLossStore{DurableControlStore: persistence}
	lost.loseResponse.Store(true)
	fixture.dispatcher = newNativeRecoveryDispatcher(t, fixture, lost)
	reserved, target, err := fixture.dispatcher.reserveTask(ctx, fixture.task)
	require.NoError(t, err)
	require.NotNil(t, reserved)
	require.ErrorContains(t, fixture.dispatcher.executeReservedTask(ctx, reserved, target), "injected loss of committed native checkpoint response")
	current := fixture.currentTask(t, ctx)
	require.NotEmpty(t, current.Annotations[nativeCaptureIntentAnnotation])
	require.False(t, taskScopedRuntimeSessionCleanupComplete(current))
	checkpoint := nativeRecoveryCheckpoint(t, ctx, persistence, current.Spec.SessionRef.Name, 2)
	require.Equal(t, snapshot.Data, checkpoint.Snapshot.Data)
	require.Equal(t, snapshot.DataDigest, checkpoint.Snapshot.DataDigest)
	require.Equal(t, snapshot.ProviderSessionID, checkpoint.Snapshot.ProviderSessionID)
	turn := nativeRecoveryTurn(t, ctx, persistence, fixture.attemptID)
	require.Equal(t, store.SessionTurnFinalized, turn.State)
	require.Equal(t, store.SessionTurnAssistantResult, turn.TerminalKind)
	require.Equal(t, store.NativeSessionCaptureDigest(checkpoint), turn.NativeSessionDigest)
	control, err := persistence.GetSessionControl(ctx, current.Namespace, current.Spec.SessionRef.Name)
	require.NoError(t, err)
	require.Nil(t, control.Lease, "the atomic commit already released the exact Session lease")
	resident := nativeRecoveryRuntimeStatus(t, ctx, fixture.dispatcher, current)
	require.Len(t, resident.Sessions, 1)
	require.Equal(t, harnessv2.RuntimeSessionStatePoisoned, resident.Sessions[0].State)
	require.Equal(t, checkpoint.Snapshot.RuntimeSessionUID, resident.Sessions[0].RuntimeSessionUID)
	require.Equal(t, uint64(checkpoint.RuntimeSessionGeneration), resident.Sessions[0].Generation)
	observed.mu.Lock()
	require.Len(t, observed.prompts, 1)
	require.Len(t, observed.captures, 1)
	require.Empty(t, observed.deletes, "no DELETE may precede recovery of the committed turn receipt")
	require.Equal(t, string(observed.captures[0].Metadata.OperationID), checkpoint.SourceOperationID)
	observed.mu.Unlock()

	restarted := newNativeRecoveryDispatcher(t, fixture, lost)
	fence, err := restarted.Epochs.CurrentFence(ctx)
	require.NoError(t, err)
	require.NoError(t, restarted.recoverStaleTask(ctx, current, fence))
	completed := fixture.currentTask(t, ctx)
	require.True(t, taskScopedRuntimeSessionCleanupComplete(completed))
	require.NoError(t, restarted.recoverStaleTask(ctx, completed, fence))
	require.Empty(t, nativeRecoveryRuntimeStatus(t, ctx, restarted, completed).Sessions)
	require.Equal(t, int32(1), lost.commits.Load(), "recovery must resume, not submit another finalization")
	require.GreaterOrEqual(t, lost.resumes.Load(), int32(1))
	require.Equal(t, turn, nativeRecoveryTurn(t, ctx, persistence, fixture.attemptID))
	require.Equal(t, checkpoint, nativeRecoveryCheckpoint(t, ctx, persistence, current.Spec.SessionRef.Name, 2))
	observed.mu.Lock()
	defer observed.mu.Unlock()
	require.Len(t, observed.prompts, 1)
	require.Len(t, observed.captures, 1)
	require.Len(t, observed.deletes, 1)
	require.Equal(t, *checkpoint, observed.deletedNative[0])
	require.Equal(t, checkpoint.RuntimeSessionGeneration, int64(observed.deletes[0].Metadata.Fence.RuntimeSessionGeneration))
	requireNativeRecoveryNoFallback(t, ctx, persistence, current)
}

func TestACPDispatcherNativeRecoveryCancellationResumesOriginalCheckpoint(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	snapshot := storetest.NativeSessionSnapshot(t, "native checkpoint from the first successful Task")
	fixture, observed := newNativeRecoveryFixture(t, ctx, "native-cancel-then-resume", snapshot,
		map[harnessv2.PromptID]harnessv2.EventType{"prompt-native-cancelled": harnessv2.EventCancelled}, nil)
	defer fixture.stop()
	persistence := fixture.dispatcher.ResultStore.(*sqlite.Store)
	dispatchQueuedTask(ctx, t, fixture.dispatcher, fixture.task)
	original := nativeRecoveryCheckpoint(t, ctx, persistence, fixture.task.Spec.SessionRef.Name, 2)
	require.Equal(t, snapshot.Data, original.Snapshot.Data)
	require.Equal(t, snapshot.ProviderSessionID, original.Snapshot.ProviderSessionID)
	cancelled := newNativeRecoveryContinuation(t, ctx, fixture, "native-cancelled-turn", "88888888-8888-8888-8888-888888888888", "prompt-native-cancelled")
	dispatchQueuedTask(ctx, t, fixture.dispatcher, cancelled)
	current := &corev1alpha1.Task{}
	require.NoError(t, fixture.kubeClient.Get(ctx, client.ObjectKeyFromObject(cancelled), current))
	require.Equal(t, corev1alpha1.TaskPhaseCancelled, current.Status.Phase)
	require.Equal(t, corev1alpha1.TaskExecutionOutcomeCancelled, current.Status.Execution.Outcome)
	require.True(t, taskScopedRuntimeSessionCleanupComplete(current))
	require.Empty(t, current.Annotations[nativeCaptureIntentAnnotation], "a cancelled child must retire without a new capture intent")
	require.Empty(t, nativeRecoveryRuntimeStatus(t, ctx, fixture.dispatcher, current).Sessions)
	attemptID, err := promptAttemptIDFromTask(current)
	require.NoError(t, err)
	turn := nativeRecoveryTurn(t, ctx, persistence, attemptID)
	require.Equal(t, store.SessionTurnFinalized, turn.State)
	require.Equal(t, store.SessionTurnOutcomeMarker, turn.TerminalKind)
	require.Equal(t, store.NativeSessionCaptureDigest(original), turn.NativeSessionDigest)
	carried := nativeRecoveryCheckpoint(t, ctx, persistence, fixture.task.Spec.SessionRef.Name, 4)
	require.Equal(t, original.Snapshot, carried.Snapshot)
	require.Equal(t, original.SourceOperationID, carried.SourceOperationID)
	require.Equal(t, original.RuntimeSessionGeneration, carried.RuntimeSessionGeneration)
	require.NotEqual(t, original.ThroughMessageID, carried.ThroughMessageID)
	control, err := persistence.GetSessionControl(ctx, current.Namespace, current.Spec.SessionRef.Name)
	require.NoError(t, err)
	require.Nil(t, control.Lease)
	observed.mu.Lock()
	require.Len(t, observed.prompts, 2)
	require.Len(t, observed.captures, 1, "cancellation carries the prior checkpoint without capturing failed state")
	require.Len(t, observed.deletes, 2, "the cancelled runtime must retire before the next turn")
	require.Equal(t, *carried, observed.deletedNative[1])
	observed.mu.Unlock()

	// No live binding survives this dispatcher restart. The next real Task must
	// restore the old native bytes at the marker's new canonical boundary.
	restarted := newNativeRecoveryDispatcher(t, fixture, persistence)
	fence, err := restarted.Epochs.CurrentFence(ctx)
	require.NoError(t, err)
	require.NoError(t, restarted.recoverStaleTask(ctx, current, fence))
	require.Equal(t, carried, nativeRecoveryCheckpoint(t, ctx, persistence, current.Spec.SessionRef.Name, 4))
	continued := newNativeRecoveryContinuation(t, ctx, fixture, "native-after-cancellation", "99999999-9999-9999-9999-999999999999", "prompt-native-after-cancellation")
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
	for index, task := range []*corev1alpha1.Task{cancelled, continued} {
		create := observed.creates[index+1]
		require.NotNil(t, create.NativeRestore)
		require.Equal(t, original.Snapshot.Data, create.NativeRestore.Snapshot.Data)
		require.Equal(t, original.Snapshot.DataDigest, create.NativeRestore.Snapshot.DataDigest)
		require.Equal(t, original.Snapshot.ProviderSessionID, create.NativeRestore.Snapshot.ProviderSessionID)
		require.Equal(t, original.SessionUID, string(create.Metadata.Fence.RuntimeSessionUID))
		require.Greater(t, create.Metadata.Fence.RuntimeSessionGeneration, observed.creates[index].Metadata.Fence.RuntimeSessionGeneration)
		require.Equal(t, "/workspace", create.NativeRestore.Snapshot.WorkingDirectory)
		prompt := observed.prompts[index+1]
		require.Len(t, prompt.Input.Content, 1, "native continuation must not replay canonical transcript")
		require.Equal(t, task.Spec.Prompt, prompt.Input.Content[0].Text)
		requireNativeRecoveryNoFallback(t, ctx, persistence, task)
	}
	require.Len(t, observed.prompts, 3)
	require.Len(t, observed.captures, 2)
	require.Len(t, observed.deletes, 3)
	for index, deleted := range observed.deletes {
		require.Equal(t, observed.creates[index].Metadata.Fence.RuntimeSessionUID, deleted.Metadata.Fence.RuntimeSessionUID)
		require.Equal(t, observed.creates[index].Metadata.Fence.RuntimeSessionGeneration, deleted.Metadata.Fence.RuntimeSessionGeneration)
	}
}
