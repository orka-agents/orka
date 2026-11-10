package sqlite

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/store"
)

func agentPromotionTestRecord(id, agentUID string) *store.AgentPromotionRecord {
	at := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	return &store.AgentPromotionRecord{
		SchemaVersion: store.AgentPromotionSchemaVersion, ID: id, Namespace: "team-a", Agent: "reviewer", AgentUID: agentUID,
		Action: store.AgentPromotionPromote, State: store.AgentPromotionPending,
		DecisionID: "d1", CandidateRun: "passed", CandidateRunUID: "passed-uid",
		CandidateAgent:       &store.EvalAgentIdentity{Name: "reviewer-c-1", UID: "candidate-uid", Generation: 1},
		Previous:             store.AgentPromotionValues{SystemPrompt: "old", Model: "openai/gpt-4.1"},
		Target:               store.AgentPromotionValues{SystemPrompt: "new", Model: "openai/gpt-4.1"},
		AgentResourceVersion: "10", AgentGeneration: 3,
		Actor:     store.EvalDecisionActor{Username: "alice", Groups: []string{"promoters"}},
		CreatedAt: at, UpdatedAt: at,
	}
}

func agentPromotionMove(record *store.AgentPromotionRecord, from, to string) store.AgentPromotionTransition {
	return store.AgentPromotionTransition{Namespace: record.Namespace, ID: record.ID, From: from, To: to,
		UpdatedAt: record.CreatedAt.Add(time.Minute)}
}

// One Agent has at most one open rollout action; a closed one frees it.
func TestAgentPromotionOneOpenPerAgent(t *testing.T) {
	s := newEvalTrialTestStore(t)
	ctx := t.Context()
	first := agentPromotionTestRecord("p1", "agent-uid")
	require.NoError(t, s.CreateAgentPromotion(ctx, first))
	require.NoError(t, s.CreateAgentPromotion(ctx, first), "a repeat of the same create succeeds")
	changed := *first
	changed.Reason = "different"
	changed.Target.SystemPrompt = "other"
	require.ErrorIs(t, s.CreateAgentPromotion(ctx, &changed), store.ErrDuplicateMismatch)

	second := agentPromotionTestRecord("p2", "agent-uid")
	require.ErrorIs(t, s.CreateAgentPromotion(ctx, second), store.ErrConflict)
	require.NoError(t, s.CreateAgentPromotion(ctx, agentPromotionTestRecord("p3", "other-agent-uid")))

	_, err := s.TransitionAgentPromotion(ctx, agentPromotionMove(first, store.AgentPromotionPending, store.AgentPromotionCanceled))
	require.NoError(t, err)
	require.NoError(t, s.CreateAgentPromotion(ctx, second))

	records, err := s.ListAgentPromotions(ctx, "team-a", "agent-uid")
	require.NoError(t, err)
	require.Len(t, records, 2)
	require.Equal(t, []string{"p1", "p2"}, []string{records[0].ID, records[1].ID})
	require.Equal(t, store.AgentPromotionCanceled, records[0].State)
	open, err := s.ListOpenAgentPromotions(ctx)
	require.NoError(t, err)
	require.Len(t, open, 2)
	require.Equal(t, []string{"p3", "p2"}, []string{open[0].ID, open[1].ID}, "open records list in creation order")
}

// Transitions are a compare-and-swap on the state along the allowed moves,
// and record what the update applied.
func TestAgentPromotionTransitions(t *testing.T) {
	s := newEvalTrialTestStore(t)
	ctx := t.Context()
	record := agentPromotionTestRecord("p1", "agent-uid")
	require.NoError(t, s.CreateAgentPromotion(ctx, record))

	_, err := s.TransitionAgentPromotion(ctx, agentPromotionMove(record, store.AgentPromotionPending, store.AgentPromotionComplete))
	require.ErrorIs(t, err, store.ErrValidation)
	_, err = s.TransitionAgentPromotion(ctx, agentPromotionMove(record, store.AgentPromotionApplying, store.AgentPromotionComplete))
	require.ErrorIs(t, err, store.ErrConflict, "the record is pending, not applying")
	missing := agentPromotionMove(record, store.AgentPromotionPending, store.AgentPromotionApplying)
	missing.ID = "nope"
	_, err = s.TransitionAgentPromotion(ctx, missing)
	require.ErrorIs(t, err, store.ErrNotFound)

	_, err = s.TransitionAgentPromotion(ctx, agentPromotionMove(record, store.AgentPromotionPending, store.AgentPromotionApplying))
	require.NoError(t, err)
	done := agentPromotionMove(record, store.AgentPromotionApplying, store.AgentPromotionComplete)
	done.AppliedGeneration, done.AppliedDigest = 4, "sha256:applied"
	updated, err := s.TransitionAgentPromotion(ctx, done)
	require.NoError(t, err)
	require.Equal(t, store.AgentPromotionComplete, updated.State)
	require.Equal(t, int64(4), updated.AppliedGeneration)
	require.Equal(t, "sha256:applied", updated.AppliedDigest)
	_, err = s.TransitionAgentPromotion(ctx, agentPromotionMove(record, store.AgentPromotionComplete, store.AgentPromotionFailed))
	require.ErrorIs(t, err, store.ErrValidation, "a closed record never moves")
}

// A move to applying that requires the approval checks, in the same
// transaction, that the record's decision is still the latest approval.
func TestAgentPromotionRequiresTheLatestApproval(t *testing.T) {
	s := newEvalTrialTestStore(t)
	ctx := t.Context()
	passed := reserveEvalTestExperiment(t, s, "passed")
	outcome, err := s.RecordEvalExperimentOutcome(ctx, evalOutcomeTestRecord(passed, store.EvalExperimentResultPassed))
	require.NoError(t, err)

	record := agentPromotionTestRecord("p1", "agent-uid")
	record.CandidateRunUID = passed.CandidateRunUID
	require.NoError(t, s.CreateAgentPromotion(ctx, record))
	apply := agentPromotionMove(record, store.AgentPromotionPending, store.AgentPromotionApplying)
	apply.RequireLatestApproval = true
	_, err = s.TransitionAgentPromotion(ctx, apply)
	require.ErrorIs(t, err, store.ErrConflict, "no decision yet")

	require.NoError(t, s.AppendEvalDecision(ctx, evalDecisionTestRecord(outcome, "d1", store.EvalDecisionApproved)))
	decision, err := s.GetEvalDecision(ctx, "team-a", "d1")
	require.NoError(t, err)
	require.Equal(t, passed.CandidateRunUID, decision.CandidateRunUID)
	_, err = s.GetEvalDecision(ctx, "team-a", "nope")
	require.ErrorIs(t, err, store.ErrNotFound)

	reject := evalDecisionTestRecord(outcome, "d2", store.EvalDecisionRejected)
	reject.Binding = nil
	require.NoError(t, s.AppendEvalDecision(ctx, reject))
	_, err = s.TransitionAgentPromotion(ctx, apply)
	require.ErrorIs(t, err, store.ErrConflict, "a later rejection withdraws the approval")

	require.NoError(t, s.AppendEvalDecision(ctx, evalDecisionTestRecord(outcome, "d3", store.EvalDecisionApproved)))
	_, err = s.TransitionAgentPromotion(ctx, apply)
	require.ErrorIs(t, err, store.ErrConflict, "a newer approval is not the one this record carries")

	record2 := agentPromotionTestRecord("p2", "agent-2")
	record2.CandidateRunUID, record2.DecisionID = passed.CandidateRunUID, "d3"
	require.NoError(t, s.CreateAgentPromotion(ctx, record2))
	apply2 := agentPromotionMove(record2, store.AgentPromotionPending, store.AgentPromotionApplying)
	apply2.RequireLatestApproval = true
	_, err = s.TransitionAgentPromotion(ctx, apply2)
	require.NoError(t, err)
}

func TestAgentPromotionValidates(t *testing.T) {
	s := newEvalTrialTestStore(t)
	for name, mutate := range map[string]func(*store.AgentPromotionRecord){
		"schema":        func(r *store.AgentPromotionRecord) { r.SchemaVersion = "v0" },
		"agent uid":     func(r *store.AgentPromotionRecord) { r.AgentUID = "" },
		"action":        func(r *store.AgentPromotionRecord) { r.Action = "canary" },
		"not pending":   func(r *store.AgentPromotionRecord) { r.State = store.AgentPromotionComplete },
		"no decision":   func(r *store.AgentPromotionRecord) { r.DecisionID = "" },
		"no promotion":  func(r *store.AgentPromotionRecord) { r.Action = store.AgentPromotionRollback },
		"no generation": func(r *store.AgentPromotionRecord) { r.AgentGeneration = 0 },
		"no actor":      func(r *store.AgentPromotionRecord) { r.Actor.Username = "" },
		"too large": func(r *store.AgentPromotionRecord) {
			r.Target.SystemPrompt = string(make([]byte, store.MaxAgentPromotionRecordBytes))
		},
	} {
		t.Run(name, func(t *testing.T) {
			record := agentPromotionTestRecord("p1", "agent-uid")
			mutate(record)
			require.ErrorIs(t, s.CreateAgentPromotion(t.Context(), record), store.ErrValidation)
		})
	}
}
