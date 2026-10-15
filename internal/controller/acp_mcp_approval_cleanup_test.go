package controller

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/approvals"
	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/store"
	storesqlite "github.com/orka-agents/orka/internal/store/sqlite"
)

// Intercept real SQLite boundaries, never replace its reads, writes, or locks.
// The append fallback exposes the pre-fix writer boundary as well, so the
// cleanup-wins regression fails on resurrected rows rather than a missing hook.
type approvalCleanupEventStore struct {
	*storesqlite.Store
	beforeTransaction func(context.Context) error
	afterRead         func(context.Context) error
	beforeAppend      func(*store.ExecutionEvent) error
	readError         error
}

type approvalCleanupTransactionKey struct{}

func (s *approvalCleanupEventStore) WithTaskDataTransaction(ctx context.Context, fn func(context.Context) error) error {
	if s.beforeTransaction != nil {
		if err := s.beforeTransaction(ctx); err != nil {
			return err
		}
	}
	return s.Store.WithTaskDataTransaction(ctx, func(txCtx context.Context) error {
		return fn(context.WithValue(txCtx, approvalCleanupTransactionKey{}, true))
	})
}

func (s *approvalCleanupEventStore) ListExecutionEvents(ctx context.Context, filter store.ExecutionEventFilter) ([]store.ExecutionEvent, error) {
	if ctx.Value(approvalCleanupTransactionKey{}) != nil && s.readError != nil {
		return nil, s.readError
	}
	listed, err := s.Store.ListExecutionEvents(ctx, filter)
	if err == nil && ctx.Value(approvalCleanupTransactionKey{}) != nil && s.afterRead != nil {
		err = s.afterRead(ctx)
	}
	return listed, err
}

func (s *approvalCleanupEventStore) AppendExecutionEventIfAbsent(ctx context.Context, event *store.ExecutionEvent, key string) (*store.ExecutionEvent, bool, error) {
	if event.Type != events.ExecutionEventTypeApprovalRequested && ctx.Value(approvalCleanupTransactionKey{}) == nil && s.beforeTransaction != nil {
		if err := s.beforeTransaction(ctx); err != nil {
			return nil, false, err
		}
	}
	if s.beforeAppend != nil {
		if err := s.beforeAppend(event); err != nil {
			return nil, false, err
		}
	}
	return s.Store.AppendExecutionEventIfAbsent(ctx, event, key)
}

func approvalCleanupBarrier(t *testing.T) (func(context.Context) error, <-chan struct{}, func()) {
	t.Helper()
	entered, release := make(chan struct{}), make(chan struct{})
	var enterOnce, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	return func(ctx context.Context) error {
		enterOnce.Do(func() { close(entered) })
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}, entered, unblock
}

func awaitApprovalCleanupSignal(t *testing.T, ctx context.Context, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-ctx.Done():
		t.Fatal("projection did not reach the retained-request transaction boundary")
	}
}

func awaitApprovalCleanupResult(t *testing.T, ctx context.Context, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		t.Fatal("approval cleanup operation did not finish")
		return ctx.Err()
	}
}

func deleteApprovalHistory(ctx context.Context, f *mcpApprovalRecoveryFixture) error {
	return f.events.DeleteExecutionEvents(ctx, f.task.Namespace, events.ExecutionEventStreamTypeTask, f.task.Name)
}

func requireApprovalHistoryEmpty(t *testing.T, f *mcpApprovalRecoveryFixture) {
	t.Helper()
	listed, err := f.events.ListExecutionEvents(f.ctx, store.ExecutionEventFilter{
		Namespace: f.task.Namespace, StreamType: events.ExecutionEventStreamTypeTask, StreamID: f.task.Name,
	})
	require.NoError(t, err)
	require.Empty(t, listed, "late projection must not resurrect any cleaned Task rows")
}

func requireCancelledApprovalReceipt(t *testing.T, f *mcpApprovalRecoveryFixture, call *acpMCPApprovalCall, original *store.ExternalEffect) *store.ExternalEffect {
	t.Helper()
	current, err := f.control.GetExternalEffectByIdentity(f.ctx, original.Identity)
	require.NoError(t, err)
	require.Equal(t, store.ExternalEffectFailed, current.State)
	require.Zero(t, current.Attempts)
	require.Empty(t, current.LeaseOwner)
	require.Nil(t, current.LeaseExpiresAt)
	outcome, reason, result, err := acpMCPApprovalReceiptOutcome(current, call.ID)
	require.NoError(t, err)
	require.Equal(t, "not_started", outcome)
	require.Equal(t, "approval_cancelled", reason)
	var denial map[string]any
	require.NoError(t, json.Unmarshal(result, &denial))
	require.Equal(t, true, denial["isError"])
	require.Equal(t, call.ID, denial["approvalID"])
	require.Equal(t, "approval_cancelled", denial["code"])
	require.Zero(t, f.count.Load())
	return current
}

func TestMCPApprovalCleanupWinsBrokerProjection(t *testing.T) {
	for _, path := range []string{"late_cancel", "terminal_receipt", "unknown"} {
		t.Run(path, func(t *testing.T) {
			f := newMCPApprovalRecoveryFixture(t)
			call, effect := seedMCPApprovalPending(t, f, true)
			credentials, err := f.broker.Credentials.ResolveACPMCPBrokerCredentials(f.ctx, f.request)
			require.NoError(t, err)
			var saved *store.ExternalEffect
			if path != "unknown" {
				_, _, err = f.broker.blockApproval(f.ctx, call, credentials, acpApprovalCodeCancelled)
				require.NoError(t, err)
				saved = requireCancelledApprovalReceipt(t, f, call, effect)
			} else {
				call, effect = f.seed(t, store.ExternalEffectOutcomeUnknown, "running", true, nil)
				saved = effect
			}
			pause, entered, release := approvalCleanupBarrier(t)
			f.broker.ApprovalEvents = &approvalCleanupEventStore{Store: f.events, beforeTransaction: pause}
			done := make(chan error, 1)
			go func() {
				var err error
				switch path {
				case "late_cancel":
					_, _, err = f.broker.revokeApproval(f.ctx, call, credentials, errACPMCPTaskCancelled)
				case "terminal_receipt":
					_, err = f.broker.replayApprovalResult(f.ctx, call, saved)
				case "unknown":
					_, _, err = f.broker.waitAndExecuteApproval(f.ctx, call, "", saved, credentials)
				}
				done <- err
			}()
			awaitApprovalCleanupSignal(t, f.ctx, entered)
			require.NoError(t, deleteApprovalHistory(f.ctx, f))
			release()
			require.NoError(t, awaitApprovalCleanupResult(t, f.ctx, done))
			requireApprovalHistoryEmpty(t, f)
			current, err := f.control.GetExternalEffectByIdentity(f.ctx, effect.Identity)
			require.NoError(t, err)
			require.Equal(t, saved, current, "projection must not rewrite durable execution evidence")
			if path != "unknown" {
				requireCancelledApprovalReceipt(t, f, call, effect)
			} else {
				require.EqualValues(t, 1, f.count.Load())
				require.Equal(t, store.ExternalEffectOutcomeUnknown, current.State)
			}
			_, err = f.broker.loadApprovalDecision(f.ctx, call)
			require.Error(t, err, "a cleaned review must not authorize a late continuation")
		})
	}
}

func TestMCPApprovalCleanupWinsRecoveryPair(t *testing.T) {
	f := newMCPApprovalRecoveryFixture(t)
	call, effect := seedMCPApprovalPending(t, f, true)
	f.task.Status.Phase = corev1alpha1.TaskPhaseCancelled
	require.NoError(t, f.kube.Update(f.ctx, f.task))
	pause, entered, release := approvalCleanupBarrier(t)
	f.dispatcher.EventStore = &approvalCleanupEventStore{Store: f.events, beforeTransaction: pause}
	done := make(chan error, 1)
	go func() { done <- f.reconcile(t) }()
	awaitApprovalCleanupSignal(t, f.ctx, entered)
	saved := requireCancelledApprovalReceipt(t, f, call, effect)
	require.NoError(t, deleteApprovalHistory(f.ctx, f))
	release()
	require.NoError(t, awaitApprovalCleanupResult(t, f.ctx, done))
	requireApprovalHistoryEmpty(t, f)
	require.Equal(t, saved, requireCancelledApprovalReceipt(t, f, call, effect))
	require.NoError(t, f.reconcile(t))
	requireApprovalHistoryEmpty(t, f)
}

func TestMCPApprovalProjectionWinsCleanup(t *testing.T) {
	for _, recovery := range []bool{false, true} {
		name := "broker"
		if recovery {
			name = "recovery_pair"
		}
		t.Run(name, func(t *testing.T) {
			f := newMCPApprovalRecoveryFixture(t)
			call, effect := seedMCPApprovalPending(t, f, true)
			credentials, err := f.broker.Credentials.ResolveACPMCPBrokerCredentials(f.ctx, f.request)
			require.NoError(t, err)
			_, _, err = f.broker.blockApproval(f.ctx, call, credentials, acpApprovalCodeCancelled)
			require.NoError(t, err)
			saved := requireCancelledApprovalReceipt(t, f, call, effect)
			pause, entered, release := approvalCleanupBarrier(t)
			wrapped := &approvalCleanupEventStore{Store: f.events, afterRead: pause}
			f.broker.ApprovalEvents, f.dispatcher.EventStore = wrapped, wrapped
			done := make(chan error, 1)
			go func() {
				if recovery {
					done <- f.reconcile(t)
				} else {
					done <- f.broker.approvalDecision(f.ctx, call, events.ExecutionEventTypeApprovalCancelled, "approval_cancelled")
				}
			}()
			ctx, cancel := context.WithTimeout(f.ctx, 2*time.Second)
			defer cancel()
			awaitApprovalCleanupSignal(t, ctx, entered)
			// With the retained request already read, cleanup cannot acquire the
			// SQLite writer. A bounded cancellation proves blocking without sleeps.
			blockedCtx, stop := context.WithTimeout(f.ctx, 100*time.Millisecond)
			err = deleteApprovalHistory(blockedCtx, f)
			stop()
			require.ErrorIs(t, err, context.DeadlineExceeded)
			cleaned := make(chan error, 1)
			go func() { cleaned <- deleteApprovalHistory(f.ctx, f) }()
			release()
			require.NoError(t, awaitApprovalCleanupResult(t, f.ctx, done))
			require.NoError(t, awaitApprovalCleanupResult(t, f.ctx, cleaned))
			requireApprovalHistoryEmpty(t, f)
			require.Equal(t, saved, requireCancelledApprovalReceipt(t, f, call, effect))
		})
	}
}

func TestMCPApprovalMissingStartRequestBlocksExecution(t *testing.T) {
	f := newMCPApprovalRecoveryFixture(t)
	call, _ := seedMCPApprovalPending(t, f, true)
	f.decide(call.ID, events.ExecutionEventTypeApprovalApproved)
	authorizer := f.broker.Prompts.(DurableACPMCPPromptAuthorizer)
	registerApprovalLease(t, authorizer.PromptLeases, f.ctx, f.request)
	pause, entered, release := approvalCleanupBarrier(t)
	f.broker.ApprovalEvents = &approvalCleanupEventStore{Store: f.events, beforeTransaction: pause}
	done := f.startContext(f.ctx, f.request)
	awaitApprovalCleanupSignal(t, f.ctx, entered)
	require.NoError(t, deleteApprovalHistory(f.ctx, f))
	release()
	response := awaitMCPApprovalResult(t, done)
	require.True(t, response.IsError)
	require.Contains(t, string(response.Result), `"code":"approval_stale"`)
	require.Zero(t, f.count.Load(), "skipping a required running record must not count as a successful start")
	requireApprovalHistoryEmpty(t, f)
}

func TestMCPApprovalProjectionRequiresExactRetainedRequest(t *testing.T) {
	for _, path := range []string{"broker_decision", "broker_outcome", "recovery"} {
		for _, mismatch := range []string{"absent", "decision_only", "task_uid", "missing_uid", "binding", "expiry", "approval_id", "namespace", "task_name"} {
			t.Run(path+"/"+mismatch, func(t *testing.T) {
				f := newMCPApprovalRecoveryFixture(t)
				call, effect := seedMCPApprovalPending(t, f, true)
				credentials, err := f.broker.Credentials.ResolveACPMCPBrokerCredentials(f.ctx, f.request)
				require.NoError(t, err)
				_, _, err = f.broker.blockApproval(f.ctx, call, credentials, acpApprovalCodeCancelled)
				require.NoError(t, err)
				expected, listed := f.approval(t)
				request := listed[0]
				require.Equal(t, events.ExecutionEventTypeApprovalRequested, request.Type)
				require.NoError(t, deleteApprovalHistory(f.ctx, f))
				var payload map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(request.Content, &payload))
				switch mismatch {
				case "task_uid":
					payload["taskUID"] = json.RawMessage(`"replacement-task-uid"`)
				case "missing_uid":
					delete(payload, "taskUID")
				case "binding":
					var binding approvals.CallBinding
					require.NoError(t, json.Unmarshal(payload["binding"], &binding))
					binding.RequestDigest = store.CanonicalBytesDigest([]byte("different request"))
					payload["binding"], err = json.Marshal(binding)
					require.NoError(t, err)
				case "expiry":
					payload["expiresAt"] = json.RawMessage(`"2000-01-01T00:00:00Z"`)
				case "approval_id":
					payload["approvalID"] = json.RawMessage(`"another-approval"`)
				case "namespace":
					request.Namespace = "another-namespace"
				case "task_name":
					request.StreamID, request.TaskName = "another-task", "another-task"
				case "decision_only":
					request.Type = events.ExecutionEventTypeApprovalCancelled
				}
				request.Content, err = json.Marshal(payload)
				require.NoError(t, err)
				if mismatch != "absent" {
					_, err = f.events.AppendExecutionEvent(f.ctx, &request)
					require.NoError(t, err)
				}
				before, err := f.events.ListExecutionEvents(f.ctx, store.ExecutionEventFilter{Namespace: f.task.Namespace})
				require.NoError(t, err)
				switch path {
				case "broker_decision":
					err = f.broker.approvalDecision(f.ctx, call, events.ExecutionEventTypeApprovalExpired, "approval_expired")
				case "broker_outcome":
					err = f.broker.approvalOutcome(f.ctx, call, "unknown", acpMCPApprovalUnknownReason, nil)
				case "recovery":
					// Exercise the production recovery projection with a valid but
					// now-stale snapshot, just as cleanup/name reuse can leave it.
					err = f.dispatcher.projectMCPApprovalExecution(f.ctx, f.task, expected, effect.Identity)
				}
				require.NoError(t, err)
				after, err := f.events.ListExecutionEvents(f.ctx, store.ExecutionEventFilter{Namespace: f.task.Namespace})
				require.NoError(t, err)
				require.Equal(t, before, after, "a stale call must not borrow replacement or incomplete request evidence")
				requireCancelledApprovalReceipt(t, f, call, effect)
			})
		}
	}
}

func TestMCPApprovalProjectionReadErrorsRemainRetryable(t *testing.T) {
	for _, path := range []string{"decision", "outcome", "recovery"} {
		t.Run(path, func(t *testing.T) {
			f := newMCPApprovalRecoveryFixture(t)
			call, effect := seedMCPApprovalPending(t, f, true)
			f.task.Status.Phase = corev1alpha1.TaskPhaseCancelled
			require.NoError(t, f.kube.Update(f.ctx, f.task))
			_, before := f.approval(t)
			// Even a conflict-shaped read failure is not evidence of absence
			// or of a reviewer winning a terminal decision append.
			outage := store.ConflictErrorf("injected retained-request read failure")
			wrapped := &approvalCleanupEventStore{Store: f.events, readError: outage}
			f.broker.ApprovalEvents, f.dispatcher.EventStore = wrapped, wrapped
			project := func() error {
				switch path {
				case "decision":
					return f.broker.approvalDecision(f.ctx, call, events.ExecutionEventTypeApprovalCancelled, "approval_cancelled")
				case "outcome":
					return f.broker.approvalOutcome(f.ctx, call, "unknown", acpMCPApprovalUnknownReason, nil)
				default:
					return f.reconcile(t)
				}
			}
			require.ErrorIs(t, project(), outage)
			_, after := f.approval(t)
			require.Equal(t, before, after)
			wrapped.readError = nil
			require.NoError(t, project())
			approval, _ := f.approval(t)
			if path == "outcome" {
				require.Equal(t, "unknown", approval.ExecutionOutcome)
			} else {
				require.Equal(t, approvals.StatusCancelled, approval.Status)
			}
			if path == "recovery" {
				requireCancelledApprovalReceipt(t, f, call, effect)
			}
		})
	}
}

func TestMCPApprovalRecoveryPairRollsBackOnOutcomeWriteError(t *testing.T) {
	f := newMCPApprovalRecoveryFixture(t)
	call, effect := seedMCPApprovalPending(t, f, true)
	f.task.Status.Phase = corev1alpha1.TaskPhaseCancelled
	require.NoError(t, f.kube.Update(f.ctx, f.task))
	outage := errors.New("injected outcome write failure")
	f.dispatcher.EventStore = &approvalCleanupEventStore{Store: f.events, beforeAppend: func(event *store.ExecutionEvent) error {
		if event.Type == events.ExecutionEventTypeApprovalExecutionUpdated {
			return outage
		}
		return nil
	}}
	require.ErrorIs(t, f.reconcile(t), outage)
	current, listed := f.approval(t)
	require.Equal(t, approvals.StatusPending, current.Status, "decision and outcome must roll back together")
	require.Len(t, listed, 1)
	saved := requireCancelledApprovalReceipt(t, f, call, effect)
	f.dispatcher.EventStore = f.events
	require.NoError(t, f.reconcile(t))
	current, listed = f.approval(t)
	require.Equal(t, approvals.StatusCancelled, current.Status)
	require.Equal(t, "not_started", current.ExecutionOutcome)
	require.Equal(t, "approval_cancelled", current.ExecutionReason)
	require.Len(t, listed, 3)
	require.Equal(t, saved, requireCancelledApprovalReceipt(t, f, call, effect))
}
