package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/store"
)

func TestSessionTurnFinalizationTaskOwnership(t *testing.T) {
	const taskUID = "finalized-task-uid"
	for _, persistenceOnly := range []bool{true, false} {
		for _, tt := range []struct {
			name        string
			sessionType string
			ownerType   string
			taskName    string
			ownerUID    string
			release     bool
		}{
			{name: "exact Task", taskName: "task-name", ownerUID: taskUID, release: true},
			{name: "legacy UID lock", taskName: taskUID, release: true},
			{name: "recreated Task", taskName: "task-name", ownerUID: "new-task-uid"},
			{name: "legacy name with different UID", taskName: taskUID, ownerUID: "new-task-uid"},
			{name: "different legacy owner", taskName: "different-task-uid"},
			{name: "Gateway type", sessionType: store.SessionTypeGateway, taskName: "gateway-task", ownerUID: taskUID},
			{name: "Gateway owner", ownerType: gatewaySessionOwnerType, taskName: "gateway-task", ownerUID: taskUID},
		} {
			for _, skipTranscript := range []bool{true, false} {
				t.Run(fmt.Sprintf("persistence=%v/%s/skipTranscript=%v", persistenceOnly, tt.name, skipTranscript), func(t *testing.T) {
					ctx := context.Background()
					s := setupTestStore(t)
					request := sessionTurnFinalizationOwnershipFixture(t, s, persistenceOnly, taskUID)
					expires := request.FinalizedAt.Add(time.Hour)
					sessionType := tt.sessionType
					if sessionType == "" {
						sessionType = "task"
					}
					if _, err := s.db.ExecContext(ctx, `UPDATE sessions SET session_type = ?, owner_type = ?,
						active_task = ?, active_task_uid = ?, active_task_expires_at = ? WHERE namespace = ? AND name = ?`,
						sessionType, tt.ownerType, tt.taskName, tt.ownerUID, expires, "ns", "session"); err != nil {
						t.Fatal(err)
					}
					request.SkipTranscriptAppend = skipTranscript
					for range 2 {
						var finalized *store.SessionTurn
						var err error
						if persistenceOnly {
							finalized, err = s.CommitSessionTurnFinalization(ctx, store.CommitSessionTurnFinalizationRequest{
								Key: request.Key, Namespace: "ns", SessionName: "session", Fence: request.Fence,
								ExpectedTurnVersion: request.ExpectedTurnVersion, FinalizationDigest: request.FinalizationDigest,
								TerminalKind: request.TerminalKind, TerminalContent: request.TerminalContent,
								SkipTranscriptAppend: request.SkipTranscriptAppend, Projection: request.Projection, FinalizedAt: request.FinalizedAt,
							})
						} else {
							finalized, err = s.FinalizeSessionTurn(ctx, request)
						}
						if err != nil {
							t.Fatal(err)
						}
						if finalized.State != store.SessionTurnFinalized {
							t.Fatalf("turn state = %s", finalized.State)
						}
					}
					session, err := s.GetSession(ctx, "ns", "session")
					if err != nil {
						t.Fatal(err)
					}
					var remainingExpiry sql.NullTime
					if err := s.db.QueryRowContext(ctx, `SELECT active_task_expires_at FROM sessions WHERE namespace = ? AND name = ?`,
						"ns", "session").Scan(&remainingExpiry); err != nil {
						t.Fatal(err)
					}
					if tt.release {
						if session.ActiveTask != "" || session.ActiveTaskUID != "" || remainingExpiry.Valid {
							t.Fatalf("finalized Task ownership remains: name=%q UID=%q expiry=%v", session.ActiveTask, session.ActiveTaskUID, remainingExpiry)
						}
					} else if session.ActiveTask != tt.taskName || session.ActiveTaskUID != tt.ownerUID || !remainingExpiry.Valid || !remainingExpiry.Time.Equal(expires) {
						t.Fatalf("another owner's lock changed: name=%q UID=%q expiry=%v", session.ActiveTask, session.ActiveTaskUID, remainingExpiry)
					}
					wantMessages := 2
					if skipTranscript {
						wantMessages = 0
					}
					if len(session.Messages) != wantMessages || session.MessageCount != wantMessages {
						t.Fatalf("transcript messages=%d count=%d, want %d", len(session.Messages), session.MessageCount, wantMessages)
					}
				})
			}
		}
	}
}

func TestSessionTurnFinalizationRecordsTaskTokenUsageOnce(t *testing.T) {
	const taskUID = "token-turn-task-uid"
	for _, persistenceOnly := range []bool{true, false} {
		t.Run(fmt.Sprintf("persistence=%v", persistenceOnly), func(t *testing.T) {
			ctx := context.Background()
			s := setupTestStore(t)
			request := sessionTurnFinalizationOwnershipFixture(t, s, persistenceOnly, taskUID)
			for i, observation := range []struct {
				attemptID, counterID string
				input, output        int64
			}{
				// An earlier attempt's usage belongs to that attempt's own turn.
				{attemptID: "0/earlier-prompt", counterID: taskUID + "/earlier-prompt", input: 500, output: 50},
				// This turn's cumulative counter counts its final value once.
				{attemptID: "1/prompt", counterID: taskUID + "/prompt", input: 30, output: 4},
				{attemptID: "1/prompt", counterID: taskUID + "/prompt", input: 120, output: 9},
			} {
				input, output := observation.input, observation.output
				if err := s.RecordUsage(ctx, store.UsageObservation{
					Namespace: "ns", TaskUID: taskUID, TaskName: "task-name", ID: fmt.Sprintf("usage-%d", i),
					AttemptID: observation.attemptID, CounterID: observation.counterID,
					Scope: store.UsageScopeAttempt, Source: store.UsageSourceAgent, InputTokens: &input, OutputTokens: &output,
					Status: "running", ObservedAt: request.FinalizedAt.Add(time.Duration(i-3) * time.Second),
				}); err != nil {
					t.Fatal(err)
				}
			}
			// Replaying the finalization must not count the turn again.
			for range 2 {
				var err error
				if persistenceOnly {
					_, err = s.CommitSessionTurnFinalization(ctx, store.CommitSessionTurnFinalizationRequest{
						Key: request.Key, Namespace: "ns", SessionName: "session", Fence: request.Fence,
						ExpectedTurnVersion: request.ExpectedTurnVersion, FinalizationDigest: request.FinalizationDigest,
						TerminalKind: request.TerminalKind, TerminalContent: request.TerminalContent,
						Projection: request.Projection, FinalizedAt: request.FinalizedAt,
					})
				} else {
					_, err = s.FinalizeSessionTurn(ctx, request)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			session, err := s.GetSession(ctx, "ns", "session")
			if err != nil {
				t.Fatal(err)
			}
			if session.InputTokens != 120 || session.OutputTokens != 9 {
				t.Fatalf("session tokens = %d/%d, want the turn's 120/9 once", session.InputTokens, session.OutputTokens)
			}
		})
	}
}

func sessionTurnFinalizationOwnershipFixture(t *testing.T, s *Store, persistenceOnly bool, taskUID string) store.FinalizeSessionTurnRequest {
	t.Helper()
	ctx := context.Background()
	fence := store.ControllerEpochFence{Name: store.DefaultControllerEpochName, Epoch: 1, HolderID: "controller"}
	if err := s.CreateSession(ctx, &store.SessionRecord{Namespace: "ns", Name: "session", SessionType: "task"}); err != nil {
		t.Fatal(err)
	}
	key := store.SessionTurnKey{SessionUID: "session-uid", LeaseGeneration: 1, TaskUID: taskUID, Attempt: 1, PromptID: "prompt"}
	turn := store.SessionTurn{Key: key, PromptAttemptID: "attempt", RequestDigest: controlTestDigest("turn"), UserPrompt: "user prompt"}
	sessionVersion := int64(1)
	if !persistenceOnly {
		fence = seedControlEpoch(t, s)
		control, err := s.CreateSessionControl(ctx, &store.SessionControl{
			Namespace: "ns", SessionName: "session", SessionUID: key.SessionUID, RequestDigest: controlTestDigest("session"),
		}, fence)
		if err != nil {
			t.Fatal(err)
		}
		control, err = s.AcquireSessionMutationLease(ctx, store.AcquireSessionMutationLeaseRequest{
			Namespace: "ns", SessionName: "session", SessionUID: key.SessionUID, Fence: fence,
			ExpectedVersion: control.Version, TaskUID: key.TaskUID, Attempt: key.Attempt, PromptID: key.PromptID,
			RequestDigest: controlTestDigest("lease"),
		})
		if err != nil {
			t.Fatal(err)
		}
		sessionVersion = control.Version
		attempt, err := s.CreatePromptAttempt(ctx, boundPromptAttemptForSQLiteTest(&store.PromptAttempt{
			Key:        store.PromptAttemptKey{Namespace: "ns", TaskUID: taskUID, Attempt: 1, PromptID: "prompt"},
			SessionUID: key.SessionUID, SessionLeaseGeneration: key.LeaseGeneration, RequestDigest: controlTestDigest("prompt"),
		}), fence)
		if err != nil {
			t.Fatal(err)
		}
		attempt = completePromptAttemptForFinalization(t, s, fence, attempt, store.PromptDeliveryNotRequested)
		turn.PromptAttemptID = attempt.ID
	}
	created, err := s.CreateSessionTurnRecord(ctx, store.CreateSessionTurnRecordRequest{
		Turn: turn, Namespace: "ns", SessionName: "session", Fence: fence,
	})
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"phase":"Succeeded"}`)
	return store.FinalizeSessionTurnRequest{
		Key: key, Fence: fence, ExpectedSessionVersion: sessionVersion, ExpectedTurnVersion: created.Version,
		FinalizationDigest: controlTestDigest("finalization"), TerminalKind: store.SessionTurnAssistantResult, TerminalContent: "answer",
		Projection: store.OutboxProjection{
			ID: "projection", AggregateKind: "SessionTurn", AggregateID: created.ID, ProjectionKind: "TaskStatus",
			Payload: payload, PayloadDigest: controlTestDigest(string(payload)),
		},
		FinalizedAt: time.Now().UTC(),
	}
}
