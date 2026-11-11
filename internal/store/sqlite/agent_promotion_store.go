package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/orka-agents/orka/internal/store"
)

func agentPromotionSchemaStatements() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS agent_promotions (
			seq         INTEGER PRIMARY KEY,
			namespace   TEXT NOT NULL,
			agent_uid   TEXT NOT NULL,
			id          TEXT NOT NULL,
			state       TEXT NOT NULL,
			record_json TEXT NOT NULL,
			created_at  TIMESTAMP NOT NULL
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_agent_promotions_id ON agent_promotions(namespace, id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_agent_promotions_open ON agent_promotions(namespace, agent_uid) WHERE state IN ('pending', 'applying')`,
	}
}

func validateAgentPromotion(record *store.AgentPromotionRecord) error {
	if record == nil {
		return store.ValidationErrorf("agent promotion record is required")
	}
	if record.SchemaVersion != store.AgentPromotionSchemaVersion {
		return store.ValidationErrorf("agent promotion schemaVersion must be %q", store.AgentPromotionSchemaVersion)
	}
	for _, field := range []struct{ name, value string }{
		{"id", record.ID}, {gatewayNamespaceField, record.Namespace}, {"agent", record.Agent}, {"agentUID", record.AgentUID},
		{"agentResourceVersion", record.AgentResourceVersion}, {"actor.username", record.Actor.Username},
	} {
		if err := requireEvalField("agent promotion", field.name, field.value); err != nil {
			return err
		}
	}
	switch record.Action {
	case store.AgentPromotionPromote, store.AgentPromotionCanary:
		if record.DecisionID == "" || record.CandidateRunUID == "" || record.CandidateAgent == nil {
			return store.ValidationErrorf("agent promotion %s needs decisionID, candidateRunUID and candidateAgent", record.Action)
		}
		if record.Action == store.AgentPromotionCanary && (record.Percent < 1 || record.Percent > 100) {
			return store.ValidationErrorf("agent promotion canary percent must be 1 to 100")
		}
	case store.AgentPromotionRollback, store.AgentPromotionAbort:
		if record.PromotionID == "" {
			return store.ValidationErrorf("agent promotion %s needs promotionID", record.Action)
		}
	default:
		return store.ValidationErrorf("agent promotion action must be promote, rollback, canary or abort")
	}
	if record.State != store.AgentPromotionPending {
		return store.ValidationErrorf("agent promotion records start pending")
	}
	if record.AgentGeneration < 1 || record.CreatedAt.IsZero() || record.UpdatedAt.IsZero() {
		return store.ValidationErrorf("agent promotion needs agentGeneration, createdAt and updatedAt")
	}
	return nil
}

func encodeAgentPromotion(record *store.AgentPromotionRecord) ([]byte, error) {
	encoded, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("encode agent promotion: %w", err)
	}
	if len(encoded) > store.MaxAgentPromotionRecordBytes {
		return nil, store.ValidationErrorf("agent promotion record is %d bytes; at most %d", len(encoded), store.MaxAgentPromotionRecordBytes)
	}
	return encoded, nil
}

// CreateAgentPromotion adds a pending record unless another record for the
// same Agent is open.
func (s *Store) CreateAgentPromotion(ctx context.Context, record *store.AgentPromotionRecord) error {
	if err := validateAgentPromotion(record); err != nil {
		return err
	}
	normalized := *record
	normalized.CreatedAt, normalized.UpdatedAt = record.CreatedAt.UTC(), record.UpdatedAt.UTC()
	encoded, err := encodeAgentPromotion(&normalized)
	if err != nil {
		return err
	}
	return writeOperationErr(ctx, s, "CreateAgentPromotion", func(ctx context.Context) error {
		return s.inWriteTransaction(ctx, func(ctx context.Context, _ *sql.Tx) error {
			executor := s.taskDataExecutor(ctx)
			var existing string
			err := executor.QueryRowContext(ctx, `SELECT record_json FROM agent_promotions WHERE namespace = ? AND id = ?`,
				normalized.Namespace, normalized.ID).Scan(&existing)
			switch {
			case err == nil:
				// A repeat of the create succeeds when only the fields later
				// transitions change differ.
				var stored store.AgentPromotionRecord
				if json.Unmarshal([]byte(existing), &stored) == nil {
					stored.State, stored.UpdatedAt, stored.Reason = normalized.State, normalized.UpdatedAt, normalized.Reason
					stored.AppliedGeneration, stored.AppliedDigest = normalized.AppliedGeneration, normalized.AppliedDigest
					if again, err := json.Marshal(&stored); err == nil && bytes.Equal(again, encoded) {
						return nil
					}
				}
				return fmt.Errorf("%w: agent promotion %s already holds a different record", store.ErrDuplicateMismatch, normalized.ID)
			case !errors.Is(err, sql.ErrNoRows):
				return err
			}
			var open string
			err = executor.QueryRowContext(ctx,
				`SELECT id FROM agent_promotions WHERE namespace = ? AND agent_uid = ? AND state IN ('pending', 'applying')`,
				normalized.Namespace, normalized.AgentUID).Scan(&open)
			switch {
			case err == nil:
				return fmt.Errorf("%w: agent %s/%s has an open rollout action %s", store.ErrConflict, normalized.Namespace, normalized.Agent, open)
			case !errors.Is(err, sql.ErrNoRows):
				return err
			}
			_, err = executor.ExecContext(ctx,
				`INSERT INTO agent_promotions (namespace, agent_uid, id, state, record_json, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
				normalized.Namespace, normalized.AgentUID, normalized.ID, normalized.State, string(encoded), normalized.CreatedAt)
			return err
		})
	})
}

// agentPromotionMoves are the allowed state transitions.
var agentPromotionMoves = map[string][]string{
	store.AgentPromotionPending:  {store.AgentPromotionApplying, store.AgentPromotionFailed, store.AgentPromotionCanceled},
	store.AgentPromotionApplying: {store.AgentPromotionComplete, store.AgentPromotionFailed},
}

// TransitionAgentPromotion compares and swaps one record's state.
func (s *Store) TransitionAgentPromotion(ctx context.Context, transition store.AgentPromotionTransition) (*store.AgentPromotionRecord, error) {
	if transition.Namespace == "" || transition.ID == "" || transition.UpdatedAt.IsZero() {
		return nil, store.ValidationErrorf("agent promotion transition needs namespace, id and updatedAt")
	}
	allowed := false
	for _, to := range agentPromotionMoves[transition.From] {
		allowed = allowed || to == transition.To
	}
	if !allowed {
		return nil, store.ValidationErrorf("agent promotion cannot move from %q to %q", transition.From, transition.To)
	}
	// The receipt keeps the updated record, so a replay of a committed
	// transition returns it rather than a state conflict.
	return writeOperation(ctx, s, "TransitionAgentPromotion", nil, func(ctx context.Context) (*store.AgentPromotionRecord, error) {
		var updated *store.AgentPromotionRecord
		err := s.inWriteTransaction(ctx, func(ctx context.Context, _ *sql.Tx) error {
			executor := s.taskDataExecutor(ctx)
			var encoded string
			err := executor.QueryRowContext(ctx, `SELECT record_json FROM agent_promotions WHERE namespace = ? AND id = ?`,
				transition.Namespace, transition.ID).Scan(&encoded)
			if errors.Is(err, sql.ErrNoRows) {
				return store.ErrNotFound
			}
			if err != nil {
				return err
			}
			var record store.AgentPromotionRecord
			if err := json.Unmarshal([]byte(encoded), &record); err != nil {
				return fmt.Errorf("decode agent promotion: %w", err)
			}
			if record.State != transition.From {
				return fmt.Errorf("%w: agent promotion %s is %s, not %s", store.ErrConflict, record.ID, record.State, transition.From)
			}
			if transition.RequireLatestApproval {
				if err := requireLatestEvalApproval(ctx, executor, &record); err != nil {
					return err
				}
			}
			record.State, record.UpdatedAt = transition.To, transition.UpdatedAt.UTC()
			if transition.AppliedGeneration != 0 {
				record.AppliedGeneration = transition.AppliedGeneration
			}
			if transition.AppliedDigest != "" {
				record.AppliedDigest = transition.AppliedDigest
			}
			if transition.Reason != "" {
				record.Reason = transition.Reason
			}
			next, err := encodeAgentPromotion(&record)
			if err != nil {
				return err
			}
			if _, err := executor.ExecContext(ctx, `UPDATE agent_promotions SET state = ?, record_json = ? WHERE namespace = ? AND id = ?`,
				record.State, string(next), record.Namespace, record.ID); err != nil {
				return err
			}
			updated = &record
			return nil
		})
		return updated, err
	})
}

// requireLatestEvalApproval fails unless the record's decision is the latest
// decision of its experiment and approves it.
func requireLatestEvalApproval(ctx context.Context, executor taskDataExecutor, record *store.AgentPromotionRecord) error {
	if record.DecisionID == "" {
		return nil
	}
	var encoded string
	err := executor.QueryRowContext(ctx,
		`SELECT record_json FROM eval_decisions WHERE namespace = ? AND candidate_run_uid = ? ORDER BY seq DESC LIMIT 1`,
		record.Namespace, record.CandidateRunUID).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: experiment %s has no decision", store.ErrConflict, record.CandidateRun)
	}
	if err != nil {
		return err
	}
	var latest store.EvalDecisionRecord
	if err := json.Unmarshal([]byte(encoded), &latest); err != nil {
		return fmt.Errorf("decode eval decision: %w", err)
	}
	if latest.ID != record.DecisionID || latest.Decision != store.EvalDecisionApproved {
		return fmt.Errorf("%w: decision %s is no longer the approval in force for %s", store.ErrConflict, record.DecisionID, record.CandidateRun)
	}
	return nil
}

// GetEvalDecision returns a decision by its ID. Decision IDs are unique only
// per experiment, so an ID two experiments share is a conflict, not a guess.
func (s *Store) GetEvalDecision(ctx context.Context, namespace, id string) (*store.EvalDecisionRecord, error) {
	rows, err := s.taskDataExecutor(ctx).QueryContext(ctx,
		`SELECT record_json FROM eval_decisions WHERE namespace = ? AND id = ? LIMIT 2`, namespace, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var found []string
	for rows.Next() {
		var encoded string
		if err := rows.Scan(&encoded); err != nil {
			return nil, err
		}
		found = append(found, encoded)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	switch len(found) {
	case 0:
		return nil, store.ErrNotFound
	case 1:
	default:
		return nil, fmt.Errorf("%w: decision %s names more than one experiment", store.ErrConflict, id)
	}
	var decision store.EvalDecisionRecord
	if err := json.Unmarshal([]byte(found[0]), &decision); err != nil {
		return nil, fmt.Errorf("decode eval decision: %w", err)
	}
	return &decision, nil
}

// ListAgentPromotions returns an Agent's records, oldest first.
func (s *Store) ListAgentPromotions(ctx context.Context, namespace, agentUID string) ([]store.AgentPromotionRecord, error) {
	return s.listAgentPromotions(ctx,
		`SELECT record_json FROM agent_promotions WHERE namespace = ? AND agent_uid = ? ORDER BY seq`, namespace, agentUID)
}

// ListOpenAgentPromotions returns every pending or applying record.
func (s *Store) ListOpenAgentPromotions(ctx context.Context) ([]store.AgentPromotionRecord, error) {
	return s.listAgentPromotions(ctx,
		`SELECT record_json FROM agent_promotions WHERE state IN ('pending', 'applying') ORDER BY seq`)
}

func (s *Store) listAgentPromotions(ctx context.Context, query string, args ...any) ([]store.AgentPromotionRecord, error) {
	rows, err := s.taskDataExecutor(ctx).QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var records []store.AgentPromotionRecord
	for rows.Next() {
		var encoded string
		if err := rows.Scan(&encoded); err != nil {
			return nil, err
		}
		var record store.AgentPromotionRecord
		if err := json.Unmarshal([]byte(encoded), &record); err != nil {
			return nil, fmt.Errorf("decode agent promotion: %w", err)
		}
		records = append(records, record)
	}
	return records, rows.Err()
}
