package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/orka-agents/orka/internal/store"
)

func prepareNativeSessionFinalizationTx(
	ctx context.Context, tx *sql.Tx, turn store.SessionTurn, request store.CommitSessionTurnFinalizationRequest,
) (*nativeSessionSnapshotRecord, error) {
	record, err := getNativeSessionSnapshotRecord(ctx, tx, request.Namespace, request.SessionName)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if errors.Is(err, store.ErrConflict) {
		return nil, deleteNativeSessionSnapshotTx(ctx, tx, turn.ID)
	}
	if err != nil {
		return nil, err
	}
	if record.snapshot.ID != turn.ID {
		return nil, nil
	}
	if record.state != nativeSessionStaged || request.SkipTranscriptAppend ||
		request.TerminalKind != store.SessionTurnAssistantResult || request.PublicationID != "" ||
		!record.snapshot.ExpiresAt.After(time.Now().UTC()) {
		return nil, deleteNativeSessionSnapshotTx(ctx, tx, turn.ID)
	}
	if err := nativeSessionSnapshotStillCurrentTx(ctx, tx, record.snapshot); err != nil {
		if errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrNotFound) {
			return nil, deleteNativeSessionSnapshotTx(ctx, tx, turn.ID)
		}
		return nil, err
	}
	history, err := nativeSessionHistoryTx(ctx, tx, request.Namespace, request.SessionName)
	if err != nil {
		return nil, err
	}
	digest, err := store.NativeSessionHistoryDigest(history)
	if err != nil {
		return nil, err
	}
	if digest != record.historyBefore {
		return nil, deleteNativeSessionSnapshotTx(ctx, tx, turn.ID)
	}
	return &record, nil
}

func finalizeNativeSessionSnapshotTx(ctx context.Context, tx *sql.Tx, record *nativeSessionSnapshotRecord) error {
	if record == nil {
		return nil
	}
	snapshot := &record.snapshot
	history, err := nativeSessionHistoryTx(ctx, tx, snapshot.Namespace, snapshot.SessionName)
	if err != nil {
		return err
	}
	snapshot.HistoryDigest, err = store.NativeSessionHistoryDigest(history)
	if err != nil {
		return err
	}
	snapshot.HistoryMessageCount = len(history)
	snapshot.ThroughMessageID = history[len(history)-1].ID
	metadata, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE native_session_snapshots SET state = 'Finalized', metadata = ?
		WHERE source_turn_id = ? AND state = 'Staged'`, metadata, snapshot.ID)
	if err != nil {
		return err
	}
	return rowsAffectedExactlyOne(result, "native session snapshot finalization")
}

func activateNativeSessionSnapshotTx(ctx context.Context, tx *sql.Tx, turn store.SessionTurn) error {
	var namespace, sessionName string
	err := tx.QueryRowContext(ctx, `SELECT namespace, session_name FROM native_session_snapshots WHERE source_turn_id = ?`,
		turn.ID).Scan(&namespace, &sessionName)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	record, err := getNativeSessionSnapshotRecord(ctx, tx, namespace, sessionName)
	if errors.Is(err, store.ErrConflict) {
		return deleteNativeSessionSnapshotTx(ctx, tx, turn.ID)
	}
	if err != nil {
		return err
	}
	// An activation retry must preserve a ready source while the next turn is
	// open. Only a not-yet-activated candidate needs the latest-turn check.
	if record.state == nativeSessionReady {
		return nil
	}
	if record.state != nativeSessionFinalized || !record.snapshot.ExpiresAt.After(time.Now().UTC()) {
		return deleteNativeSessionSnapshotTx(ctx, tx, turn.ID)
	}
	if err := nativeSessionSnapshotStillCurrentTx(ctx, tx, record.snapshot); err != nil {
		if errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrNotFound) {
			return deleteNativeSessionSnapshotTx(ctx, tx, turn.ID)
		}
		return err
	}
	history, err := nativeSessionHistoryTx(ctx, tx, namespace, sessionName)
	if err != nil {
		return err
	}
	digest, err := store.NativeSessionHistoryDigest(history)
	if err != nil {
		return err
	}
	if digest != record.snapshot.HistoryDigest || len(history) != record.snapshot.HistoryMessageCount ||
		len(history) == 0 || history[len(history)-1].ID != record.snapshot.ThroughMessageID {
		return deleteNativeSessionSnapshotTx(ctx, tx, turn.ID)
	}
	result, err := tx.ExecContext(ctx, `UPDATE native_session_snapshots SET state = 'Ready'
		WHERE source_turn_id = ? AND state = 'Finalized'`, turn.ID)
	if err != nil {
		return err
	}
	return rowsAffectedExactlyOne(result, "native session snapshot activation")
}

func nativeSessionSnapshotStillCurrentTx(ctx context.Context, tx *sql.Tx, snapshot store.NativeSessionSnapshot) error {
	if err := requireNativeSessionSnapshotBinding(ctx, tx, snapshot); err != nil {
		return err
	}
	return requireLatestNativeSessionTurn(ctx, tx, snapshot)
}
