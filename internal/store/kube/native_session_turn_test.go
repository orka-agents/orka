package kube

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	controlstore "github.com/orka-agents/orka/internal/store"
	sqlitestore "github.com/orka-agents/orka/internal/store/sqlite"
	"github.com/orka-agents/orka/internal/store/storetest"
	"github.com/stretchr/testify/require"
)

func TestCrossStoreCarriedNativeCheckpoint(t *testing.T) {
	for _, terminal := range []controlstore.PromptExecutionState{controlstore.PromptExecutionFailed, controlstore.PromptExecutionCancelled, controlstore.PromptExecutionOutcomeUnknown} {
		t.Run(string(terminal), func(t *testing.T) {
			ctx := context.Background()
			_, rawClient, fence := newTestStoreWithEpoch(t)
			db, err := sqlitestore.NewDB(filepath.Join(t.TempDir(), "turns.db"))
			if err != nil {
				t.Fatalf("NewDB: %v", err)
			}
			t.Cleanup(func() { _ = db.Close() })
			sqliteStore := sqlitestore.NewStore(db, "")
			cipher, err := sqlitestore.NewAgentExecutionSnapshotCipher(bytes.Repeat([]byte{7}, 32))
			require.NoError(t, err)
			require.NoError(t, sqliteStore.SetAgentExecutionSnapshotCipher(cipher))
			snapshot := storetest.NativeSessionSnapshot(t, "retained native context")
			snapshot.RuntimeSessionUID, snapshot.RuntimeProfileDigest, snapshot.WorkingDirectory = "", "", ""
			_, err = sqliteStore.StageNativeSessionImport(ctx, controlstore.NativeSessionImport{Namespace: "tenant-a", SessionName: "session-finalize", OperationID: "native-import", RequestDigest: controlstore.NativeSessionImportDigest("tenant-a", "session-finalize", snapshot.DataDigest), Snapshot: snapshot})
			require.NoError(t, err)

			kubeStore, err := NewComposite(rawClient, testControlNamespace, sqliteStore)
			if err != nil {
				t.Fatalf("NewComposite: %v", err)
			}
			control, err := kubeStore.CreateSessionControl(ctx, &controlstore.SessionControl{
				Namespace: "tenant-a", SessionName: "session-finalize", SessionUID: "session-finalize-uid",
				RequestDigest: testDigest("session-finalize"),
			}, fence)
			if err != nil {
				t.Fatalf("CreateSessionControl: %v", err)
			}
			leaseExpires := testNow.Add(15 * time.Minute)
			promptRequestDigest := testDigest("prompt-finalize")
			leaseRequestDigest, err := controlstore.SessionMutationLeaseRequestDigest(
				control.SessionUID, control.LeaseGeneration+1, "task-finalize", 1, "prompt-finalize", promptRequestDigest,
			)
			if err != nil {
				t.Fatal(err)
			}
			control, err = kubeStore.AcquireSessionMutationLease(ctx, controlstore.AcquireSessionMutationLeaseRequest{
				Namespace: control.Namespace, SessionName: control.SessionName, SessionUID: control.SessionUID,
				Fence: fence, ExpectedVersion: control.Version, ExpectedLeaseGeneration: control.LeaseGeneration,
				TaskUID: "task-finalize", Attempt: 1, PromptID: "prompt-finalize",
				RequestDigest: leaseRequestDigest, AcquiredAt: testNow, ExpiresAt: &leaseExpires,
				Lineage: testSessionLineageClaim(control),
			})
			if err != nil {
				t.Fatalf("AcquireSessionMutationLease: %v", err)
			}
			attemptKey := controlstore.PromptAttemptKey{Namespace: "tenant-a", TaskUID: "task-finalize", Attempt: 1, PromptID: "prompt-finalize"}
			ensureActiveAgentTask(t, ctx, rawClient, attemptKey.Namespace, attemptKey.TaskUID, attemptKey.TaskUID)
			attempt, err := kubeStore.CreatePromptAttempt(ctx, boundPromptAttemptForKubeTest(&controlstore.PromptAttempt{Key: attemptKey, RequestDigest: promptRequestDigest}), fence)
			if err != nil {
				t.Fatalf("CreatePromptAttempt: %v", err)
			}
			attempt = advancePromptAttemptToSuccess(t, ctx, kubeStore, fence, attempt, control, terminal)
			turnKey := controlstore.SessionTurnKey{SessionUID: control.SessionUID, LeaseGeneration: control.LeaseGeneration, TaskUID: attemptKey.TaskUID, Attempt: attemptKey.Attempt, PromptID: attemptKey.PromptID}
			turn, err := kubeStore.CreateSessionTurn(ctx, controlstore.CreateSessionTurnRequest{
				Turn:  controlstore.SessionTurn{Key: turnKey, PromptAttemptID: attempt.ID, RequestDigest: testDigest("turn-finalize"), UserPrompt: "finish the task"},
				Fence: fence, ExpectedSessionVersion: control.Version,
			})
			if err != nil {
				t.Fatalf("CreateSessionTurn: %v", err)
			}
			projectionPayload := []byte(`{"phase":"Succeeded"}`)
			projection := controlstore.OutboxProjection{
				ID: "projection-finalize", AggregateKind: sessionTurnAggregateKind, AggregateID: turn.ID,
				ProjectionKind: "TaskTerminalStatus", Payload: projectionPayload, PayloadDigest: testBytesDigest(projectionPayload),
			}
			finalize := controlstore.FinalizeSessionTurnRequest{
				Key: turnKey, Fence: fence, ExpectedSessionVersion: control.Version, ExpectedTurnVersion: turn.Version,
				FinalizationDigest: testDigest("finalize-turn"), TerminalKind: controlstore.SessionTurnOutcomeMarker,
				TerminalContent: string(terminal), Projection: projection, FinalizedAt: testNow.Add(time.Hour),
			}

			before, err := sqliteStore.GetNativeSession(ctx, control.Namespace, control.SessionName, control.SessionUID)
			require.NoError(t, err)
			finalize.NativeSession, finalize.NativeSessionCarried = before, true
			fresh := finalize
			fresh.NativeSessionCarried = false
			_, err = kubeStore.FinalizeSessionTurn(ctx, fresh)
			require.ErrorIs(t, err, controlstore.ErrValidation)
			suppressed := finalize
			suppressed.SkipTranscriptAppend = true
			_, err = kubeStore.FinalizeSessionTurn(ctx, suppressed)
			require.ErrorIs(t, err, controlstore.ErrValidation)
			settled, err := kubeStore.FinalizeSessionTurn(ctx, finalize)
			require.NoError(t, err)
			require.Equal(t, controlstore.SessionTurnFinalized, settled.State)
			_, err = kubeStore.FinalizeSessionTurn(ctx, finalize)
			require.NoError(t, err, "exact replay must converge")
			control, err = kubeStore.GetSessionControl(ctx, control.Namespace, control.SessionName)
			require.NoError(t, err)
			require.Nil(t, control.Lease)
			after, err := sqliteStore.GetNativeSession(ctx, control.Namespace, control.SessionName, control.SessionUID)
			require.NoError(t, err)
			require.Equal(t, before.Snapshot, after.Snapshot)
			require.Equal(t, before.SourceOperationID, after.SourceOperationID)
			require.Equal(t, 2, after.MessageCount)
			require.NotEmpty(t, after.ThroughMessageID)
			transcript, err := sqliteStore.LoadTranscript(ctx, control.Namespace, control.SessionName, 100)
			require.NoError(t, err)
			require.Len(t, transcript, 2)
		})
	}
}
