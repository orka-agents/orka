package sqlite

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/store"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

func TestResolveOperationExpiresAtTheRetentionBound(t *testing.T) {
	ctx := context.Background()
	s := setupTestStore(t)
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	s.clock = func() time.Time { return now }
	dataset := advanceTestEpoch(t, s, testWriteFence)
	bound := storagev1.MinReceiptRetention - storagev1.MaxClockSkew
	resolve := func(issuedAt time.Time) (storagev1.OperationOutcome, error) {
		_, envelope := testEnvelope(t, ctx, dataset, "SaveResult", issuedAt, "ns", "task", []byte("x"))
		return s.ResolveOperation(ctx, storagev1.ResolveOperationRequest{
			DatasetID: dataset, OperationID: envelope.OperationID, Method: "SaveResult",
			RequestDigest: envelope.RequestDigest, IssuedAt: issuedAt,
		})
	}
	_, err := resolve(now.Add(-bound - time.Nanosecond))
	require.ErrorIs(t, err, storagev1.ErrOperationExpired, "its receipt could already have been pruned")
	outcome, err := resolve(now.Add(-bound))
	require.NoError(t, err)
	require.Equal(t, storagev1.OperationAborted, outcome.State)
}

func TestAdministrativeMethodsRefuseAnEnclosingTransaction(t *testing.T) {
	ctx := context.Background()
	s := setupTestStore(t)
	dataset := advanceTestEpoch(t, s, testWriteFence)
	_, envelope := testEnvelope(t, ctx, dataset, "SaveResult", time.Now().UTC(), "ns", "task", []byte("x"))
	err := s.WithTaskDataTransaction(ctx, func(txCtx context.Context) error {
		_, err := s.ResolveOperation(txCtx, storagev1.ResolveOperationRequest{
			DatasetID: dataset, OperationID: envelope.OperationID, Method: "SaveResult",
			RequestDigest: envelope.RequestDigest, IssuedAt: envelope.IssuedAt,
		})
		require.ErrorIs(t, err, store.ErrValidation, "an abort must be durable when it is reported")
		at := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
		_, err = s.AdvanceControllerEpoch(txCtx, storagev1.AdvanceControllerEpochRequest{
			DatasetID: dataset, InstallationID: "installation",
			Epoch: store.ControllerEpoch{
				Name: store.DefaultControllerEpochName, Epoch: 2, HolderID: "successor",
				RequestDigest: controlTestDigest("epoch-successor"), Version: 2, AcquiredAt: at, UpdatedAt: at,
			},
		})
		require.ErrorIs(t, err, store.ErrValidation, "an advance must be durable when it returns")
		return nil
	})
	require.NoError(t, err)
}

func TestJoinedWriteTransactionKeepsItsOwnRollback(t *testing.T) {
	ctx := context.Background()
	s := setupTestStore(t)
	err := s.WithTaskDataTransaction(ctx, func(txCtx context.Context) error {
		discarded, err := s.beginTx(txCtx)
		require.NoError(t, err)
		_, err = discarded.ExecContext(txCtx, `INSERT INTO results(namespace, task_name, data) VALUES ('ns', 'discarded', x'00')`)
		require.NoError(t, err)
		require.NoError(t, discarded.Rollback())
		require.ErrorIs(t, discarded.Commit(), sql.ErrTxDone)

		kept, err := s.beginTx(txCtx)
		require.NoError(t, err)
		_, err = kept.ExecContext(txCtx, `INSERT INTO results(namespace, task_name, data) VALUES ('ns', 'kept', x'00')`)
		require.NoError(t, err)
		require.NoError(t, kept.Commit())
		require.ErrorIs(t, kept.Rollback(), sql.ErrTxDone, "a deferred Rollback after Commit undoes nothing")
		return nil
	})
	require.NoError(t, err)
	_, err = s.GetResult(ctx, "ns", "discarded")
	require.ErrorIs(t, err, store.ErrNotFound, "a joined method that rolls back leaves nothing behind")
	_, err = s.GetResult(ctx, "ns", "kept")
	require.NoError(t, err)
}

func TestDeliveryMarkInsideAnOperationDoesNotWaitForTheAppendMutex(t *testing.T) {
	s := setupTestStore(t)
	dataset := advanceTestEpoch(t, s, testWriteFence)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// An ordinary append holds the mutex while it waits for the connection,
	// which the operation already holds.
	s.executionEventMu.Lock()
	defer s.executionEventMu.Unlock()
	now := time.Now().UTC()
	markCtx, _ := testEnvelope(t, ctx, dataset, "MarkGatewayDeliveryDelivered", now, "ns", "delivery", "owner", "message", now)
	// Lock ignores ctx, so a call that waits for the mutex is detected by a
	// bounded wait rather than its deadline.
	done := make(chan error, 1)
	go func() { done <- s.MarkGatewayDeliveryDelivered(markCtx, "ns", "delivery", "owner", "message", now) }()
	select {
	case err := <-done:
		require.ErrorIs(t, err, store.ErrNotFound)
	case <-time.After(5 * time.Second):
		s.executionEventMu.Unlock()
		<-done
		s.executionEventMu.Lock()
		t.Fatal("an enveloped delivery mark waited for the event-append mutex while holding the connection")
	}
}

func TestSecurityWritesStayInsideTheOperation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s := setupTestStore(t)
	dataset := advanceTestEpoch(t, s, testWriteFence)
	now := time.Now().UTC()

	initial, bound := testPatchProposalPublication("enveloped")
	require.NoError(t, s.CreatePatchProposal(ctx, initial))
	require.NoError(t, s.BindPatchProposalPublicationEvidence(ctx, bound))
	identical := clonePatchProposal(bound)
	identical.PublicationEvidence = nil
	updateCtx, _ := testEnvelope(t, ctx, dataset, "UpdatePatchProposal", now, identical)
	require.NoError(t, s.UpdatePatchProposal(updateCtx, identical), "an identical update of a published proposal")

	identity := store.ScanTaskIdentity{Namespace: "ns", RepositoryScan: "repo", ScanRunID: "run", TaskName: "mapper", TaskUID: "uid-1", Stage: "mapper"}
	require.NoError(t, s.CreateScanRun(ctx, &store.ScanRun{ID: "run", Namespace: "ns", RepositoryScan: "repo", Phase: "running"}))
	applied, err := s.ApplyScanTaskIngestion(ctx, &store.ScanTaskIngestion{ScanTaskIdentity: identity}, nil,
		func(_ store.SecurityStore, run *store.ScanRun) error { run.SliceCount++; return nil })
	require.NoError(t, err)
	require.True(t, applied)
	completeCtx, complete := testEnvelope(t, ctx, dataset, "CompleteScanTaskIngestion", now, identity)
	require.NoError(t, s.CompleteScanTaskIngestion(completeCtx, identity))
	require.NoError(t, s.CompleteScanTaskIngestion(completeCtx, identity), "a replay")
	resolved, err := s.ResolveOperation(ctx, storagev1.ResolveOperationRequest{
		DatasetID: dataset, OperationID: complete.OperationID, Method: "CompleteScanTaskIngestion",
		RequestDigest: complete.RequestDigest, IssuedAt: complete.IssuedAt,
	})
	require.NoError(t, err)
	require.Equal(t, storagev1.OperationCommitted, resolved.State, "a committed completion is never reported as aborted")
	advanceTestEpoch(t, s, store.ControllerEpochFence{Name: store.DefaultControllerEpochName, Epoch: 2, HolderID: "successor"})
	staleCtx, _ := testEnvelope(t, ctx, dataset, "CompleteScanTaskIngestion", now, identity)
	require.ErrorIs(t, s.CompleteScanTaskIngestion(staleCtx, identity), store.ErrFenced)
}

func TestIsLockedClearsAnExpiredLockInsideAnOperation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s := setupTestStore(t)
	dataset := advanceTestEpoch(t, s, testWriteFence)
	require.NoError(t, s.CreateSession(ctx, &store.SessionRecord{Namespace: "ns", Name: "session", SessionType: "task"}))
	acquiredAt := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	require.NoError(t, s.AcquireLockUntil(ctx, "ns", "session", "task-a", "uid-a", acquiredAt, acquiredAt.Add(time.Minute)))

	// The lock is judged at the request time, when it has expired.
	checkedAt := acquiredAt.Add(time.Minute)
	lockedCtx, _ := testEnvelope(t, ctx, dataset, "IsLocked", time.Now().UTC(), "ns", "session", "task-b", "uid-b", checkedAt)
	locked, err := s.IsLocked(lockedCtx, "ns", "session", "task-b", "uid-b", checkedAt)
	require.NoError(t, err)
	require.False(t, locked, "an expired lock is cleared")
	sessions, err := s.ListSessions(ctx, "ns")
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	require.Empty(t, sessions[0].ActiveTaskUID)

	require.NoError(t, s.AcquireLock(ctx, "ns", "session", "task-c", "uid-c", checkedAt))
	locked, err = s.IsLocked(lockedCtx, "ns", "session", "task-b", "uid-b", checkedAt)
	require.NoError(t, err)
	require.False(t, locked, "a replay returns the original answer and changes nothing")
	sessions, err = s.ListSessions(ctx, "ns")
	require.NoError(t, err)
	require.Equal(t, "uid-c", sessions[0].ActiveTaskUID)
}
