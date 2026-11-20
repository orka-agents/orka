package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/store"
)

// The legacy SQLite control store has no column for ADR 0059 handoff
// records. It must advertise no support, so the dispatcher never keeps a
// runtime against it, and it must refuse a record instead of dropping it.
func TestSQLiteControlStoreRefusesResidentRuntimes(t *testing.T) {
	db, err := NewDB(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := NewStore(db, "")
	if _, ok := any(s).(store.SessionResidentRuntimeSupport); ok {
		t.Fatal("the SQLite control store must not advertise resident runtime support")
	}
	fence := seedControlEpoch(t, s)
	key := store.SessionTurnKey{SessionUID: controlTestSessionUID, LeaseGeneration: 1, TaskUID: "task", Attempt: 1, PromptID: "prompt"}
	turnID, _ := key.CanonicalID()
	_, err = s.FinalizeSessionTurn(context.Background(), store.FinalizeSessionTurnRequest{
		Key: key, Fence: fence, ExpectedSessionVersion: 1, ExpectedTurnVersion: 1,
		FinalizationDigest: controlTestDigest("finalization"), TerminalKind: store.SessionTurnAssistantResult,
		TerminalContent: "done",
		Projection: store.OutboxProjection{
			ID: "outbox", AggregateKind: controlTestAggregateKind, AggregateID: turnID,
			ProjectionKind: controlTestProjectionKind, Payload: []byte(`{}`), PayloadDigest: controlTestDigest("{}"),
		},
		FinalizedAt:     time.Date(2026, 11, 20, 6, 0, 0, 0, time.UTC),
		ResidentRuntime: &store.SessionResidentRuntime{HandoffID: controlTestDigest("handoff")},
	})
	if !errors.Is(err, store.ErrValidation) {
		t.Fatalf("finalization with a resident runtime = %v, want validation error", err)
	}
}
