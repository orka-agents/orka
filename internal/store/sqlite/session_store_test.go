package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/store"
)

func TestAcquireLockSerializesGatewayAuthorizationWithTerminalTransition(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "gateway-lock-race.db")
	lockDB, err := NewDB(dbPath)
	if err != nil {
		t.Fatalf("NewDB(lock): %v", err)
	}
	t.Cleanup(func() { _ = lockDB.Close() })
	terminalDB, err := NewDB(dbPath)
	if err != nil {
		t.Fatalf("NewDB(terminal): %v", err)
	}
	t.Cleanup(func() { _ = terminalDB.Close() })

	lockStore := NewStore(lockDB, dbPath)
	now := time.Now().UTC().Truncate(time.Second)
	event := testGatewayEvent(now, "lock-terminal-race")
	event.BindingUID = testGatewayBindingUID
	if _, _, err := lockStore.AdmitGatewayEvent(ctx, store.GatewayEventAdmission{
		Event: event, AppendUserMessage: true, PendingLimit: 100,
	}); err != nil {
		t.Fatalf("AdmitGatewayEvent: %v", err)
	}
	claimed, err := lockStore.ClaimNextGatewayEvent(ctx, event.Namespace, "dispatcher", now, time.Minute)
	if err != nil {
		t.Fatalf("ClaimNextGatewayEvent: %v", err)
	}
	const taskUID = "task-uid-lock-terminal-race"
	if err := lockStore.MarkGatewayEventTaskCreated(
		ctx, event.Namespace, event.ID, claimed.TaskName, taskUID, "dispatcher", now,
	); err != nil {
		t.Fatalf("MarkGatewayEventTaskCreated: %v", err)
	}

	terminalTx, err := terminalDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx(terminal): %v", err)
	}
	terminalEvent, err := getGatewayEventQuery(ctx, terminalTx, event.Namespace, event.ID)
	if err != nil {
		_ = terminalTx.Rollback()
		t.Fatalf("getGatewayEventQuery: %v", err)
	}
	if err := expireGatewayEventTx(ctx, terminalTx, terminalEvent, "", "expired", now.Add(time.Second)); err != nil {
		_ = terminalTx.Rollback()
		t.Fatalf("expireGatewayEventTx: %v", err)
	}

	var acquireErr error
	acquireDone := make(chan struct{})
	go func() {
		acquireErr = lockStore.AcquireLock(ctx, event.Namespace, event.SessionName, event.TaskName, taskUID)
		close(acquireDone)
	}()

	if err := waitForSQLiteConnectionHeld(ctx, lockDB, acquireDone); err != nil {
		_ = terminalTx.Rollback()
		<-acquireDone
		t.Fatal(err)
	}
	if err := terminalTx.Commit(); err != nil {
		t.Fatalf("Commit(terminal): %v", err)
	}

	select {
	case <-acquireDone:
	case <-time.After(5 * time.Second):
		t.Fatal("AcquireLock did not return after terminal transition committed")
	}
	if !errors.Is(acquireErr, store.ErrValidation) {
		t.Fatalf("AcquireLock error = %v, want ErrValidation after terminal transition", acquireErr)
	}
	session, err := lockStore.GetSession(ctx, event.Namespace, event.SessionName)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if session.ActiveTask != "" || session.ActiveTaskUID != "" {
		t.Fatalf("terminal session lock = (%q, %q), want unlocked", session.ActiveTask, session.ActiveTaskUID)
	}
}

func waitForSQLiteConnectionHeld(ctx context.Context, db *sql.DB, operationDone <-chan struct{}) error {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-operationDone:
			return errors.New("AcquireLock returned before the terminal transition committed")
		default:
		}

		probeCtx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
		var one int
		err := db.QueryRowContext(probeCtx, "SELECT 1").Scan(&one)
		cancel()
		if errors.Is(err, context.DeadlineExceeded) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("probe lock database connection: %w", err)
		}
		time.Sleep(time.Millisecond)
	}
	return errors.New("AcquireLock never held the lock database connection while waiting for the writer")
}
