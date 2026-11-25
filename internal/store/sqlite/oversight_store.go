/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/store"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

var _ storagev1.OversightStore = (*Store)(nil)

// oversightSchemaStatements defines oversight decisions, their facet index
// and their outcome label ledger. Seq is AUTOINCREMENT so a value is never
// reused, even after retention removes the newest row: stream cursors rely
// on it. action_narrative and search_text are derived from the body and may be
// rebuilt; they are not covered by the digest.
func oversightSchemaStatements() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS oversight_decisions (
			seq               INTEGER PRIMARY KEY AUTOINCREMENT,
			namespace         TEXT NOT NULL,
			id                TEXT NOT NULL,
			task_name         TEXT NOT NULL,
			agent_name        TEXT NOT NULL,
			run_id            TEXT NOT NULL,
			tool_name         TEXT NOT NULL,
			disposition       TEXT NOT NULL,
			would_have_paused INTEGER NOT NULL,
			max_score         REAL NOT NULL,
			signature_hash    TEXT NOT NULL,
			observed_at       TIMESTAMP NOT NULL,
			recorded_at       TIMESTAMP NOT NULL,
			workload_identity TEXT NOT NULL,
			action_narrative  TEXT NOT NULL,
			search_text       TEXT NOT NULL,
			digest            TEXT NOT NULL,
			body              BLOB NOT NULL,
			UNIQUE(namespace, id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_oversight_decisions_task
			ON oversight_decisions(namespace, task_name, seq)`,
		`CREATE INDEX IF NOT EXISTS idx_oversight_decisions_agent
			ON oversight_decisions(namespace, agent_name, seq)`,
		`CREATE INDEX IF NOT EXISTS idx_oversight_decisions_disposition
			ON oversight_decisions(namespace, disposition, seq)`,
		`CREATE INDEX IF NOT EXISTS idx_oversight_decisions_signature
			ON oversight_decisions(namespace, signature_hash, seq)`,
		`CREATE INDEX IF NOT EXISTS idx_oversight_decisions_observed
			ON oversight_decisions(namespace, observed_at)`,
		`CREATE TABLE IF NOT EXISTS oversight_decision_facets (
			namespace TEXT NOT NULL,
			kind      TEXT NOT NULL,
			value     TEXT NOT NULL,
			seq       INTEGER NOT NULL,
			PRIMARY KEY(namespace, kind, value, seq)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_oversight_decision_facets_seq
			ON oversight_decision_facets(seq)`,
		`CREATE TABLE IF NOT EXISTS oversight_feedback (
			seq         INTEGER PRIMARY KEY AUTOINCREMENT,
			namespace   TEXT NOT NULL,
			decision_id TEXT NOT NULL,
			label       TEXT NOT NULL,
			note        TEXT NOT NULL,
			reviewer    TEXT NOT NULL,
			recorded_at TIMESTAMP NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_oversight_feedback_decision
			ON oversight_feedback(namespace, decision_id, seq)`,
	}
}

func validOversightRecord(namespace string, record storagev1.OversightDecisionRecord) error {
	switch {
	case strings.TrimSpace(record.ID) == "" || strings.TrimSpace(record.TaskName) == "" ||
		strings.TrimSpace(record.Digest) == "" || strings.TrimSpace(record.ToolName) == "" ||
		strings.TrimSpace(record.Disposition) == "":
		return fmt.Errorf("%w: oversight decision id, task, tool, disposition and digest are required", store.ErrValidation)
	case record.Namespace != "" && record.Namespace != namespace:
		return fmt.Errorf("%w: oversight decision belongs to another namespace", store.ErrValidation)
	case record.ObservedAt.IsZero():
		return fmt.Errorf("%w: oversight decision observation time is required", store.ErrValidation)
	case len(record.Body) == 0:
		return fmt.Errorf("%w: oversight decision body is required", store.ErrValidation)
	case len(record.Body) > storagev1.MaxOversightBodyBytes:
		return fmt.Errorf("%w: oversight decision body exceeds %d bytes", store.ErrTooLarge, storagev1.MaxOversightBodyBytes)
	case len(record.SearchText) > storagev1.MaxOversightSearchTextBytes:
		return fmt.Errorf("%w: oversight decision search text exceeds %d bytes", store.ErrTooLarge, storagev1.MaxOversightSearchTextBytes)
	case len(record.ActionNarrative) > storagev1.MaxOversightNarrativeBytes:
		return fmt.Errorf("%w: oversight decision narrative exceeds %d bytes", store.ErrTooLarge, storagev1.MaxOversightNarrativeBytes)
	case len(record.Facets) > storagev1.MaxOversightFacets:
		return fmt.Errorf("%w: oversight decision has more than %d facets", store.ErrTooLarge, storagev1.MaxOversightFacets)
	}
	for _, facet := range record.Facets {
		if facet.Kind == "" || facet.Value == "" {
			return fmt.Errorf("%w: oversight facets need a kind and a value", store.ErrValidation)
		}
	}
	return nil
}

// AppendOversightDecisions stores a batch of decisions of one namespace.
func (s *Store) AppendOversightDecisions(
	ctx context.Context, namespace string, records []storagev1.OversightDecisionRecord,
) ([]storagev1.OversightAppendResult, error) {
	if strings.TrimSpace(namespace) == "" {
		return nil, fmt.Errorf("%w: namespace is required", store.ErrValidation)
	}
	if len(records) == 0 || len(records) > storagev1.MaxOversightBatch {
		return nil, fmt.Errorf("%w: a batch holds 1 to %d oversight decisions", store.ErrValidation, storagev1.MaxOversightBatch)
	}
	for _, record := range records {
		if err := validOversightRecord(namespace, record); err != nil {
			return nil, err
		}
	}
	return writeOperation(ctx, s, "AppendOversightDecisions", nil,
		func(ctx context.Context) ([]storagev1.OversightAppendResult, error) {
			return s.appendOversightDecisions(ctx, namespace, records)
		})
}

func (s *Store) appendOversightDecisions(
	ctx context.Context, namespace string, records []storagev1.OversightDecisionRecord,
) ([]storagev1.OversightAppendResult, error) {
	tx, err := s.beginTx(ctx)
	if err != nil {
		return nil, fmt.Errorf("append oversight decisions: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := s.now().UTC()
	results := make([]storagev1.OversightAppendResult, 0, len(records))
	inBatch := map[string]string{}
	for _, record := range records {
		if digest, ok := inBatch[record.ID]; ok {
			if digest != record.Digest {
				return nil, fmt.Errorf("%w: oversight decision %q appears twice with different content", store.ErrDuplicateMismatch, record.ID)
			}
		}
		inBatch[record.ID] = record.Digest
		var seq int64
		var digest string
		err := tx.QueryRowContext(ctx, `SELECT seq, digest FROM oversight_decisions WHERE namespace = ? AND id = ?`,
			namespace, record.ID).Scan(&seq, &digest)
		switch {
		case err == nil:
			if digest != record.Digest {
				return nil, fmt.Errorf("%w: oversight decision %q is stored with different content", store.ErrDuplicateMismatch, record.ID)
			}
			results = append(results, storagev1.OversightAppendResult{ID: record.ID, Seq: seq, Duplicate: true})
			continue
		case !errors.Is(err, sql.ErrNoRows):
			return nil, fmt.Errorf("append oversight decisions: %w", err)
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO oversight_decisions
			(namespace, id, task_name, agent_name, workload_identity, run_id, tool_name, disposition, would_have_paused,
			 max_score, signature_hash, observed_at, recorded_at, action_narrative, search_text, digest, body)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			namespace, record.ID, record.TaskName, record.AgentName, record.WorkloadIdentity, record.RunID, record.ToolName,
			record.Disposition, record.WouldHavePaused, record.MaxScore, record.SignatureHash, record.ObservedAt.UTC(), now,
			record.ActionNarrative, strings.ToLower(record.SearchText), record.Digest, record.Body)
		if err != nil {
			return nil, fmt.Errorf("append oversight decisions: %w", uniqueConflict(err))
		}
		if seq, err = result.LastInsertId(); err != nil {
			return nil, err
		}
		seen := map[storagev1.OversightFacet]bool{}
		for _, facet := range record.Facets {
			if seen[facet] {
				continue
			}
			seen[facet] = true
			if _, err := tx.ExecContext(ctx, `INSERT INTO oversight_decision_facets (namespace, kind, value, seq) VALUES (?, ?, ?, ?)`,
				namespace, facet.Kind, facet.Value, seq); err != nil {
				return nil, fmt.Errorf("index oversight decision: %w", err)
			}
		}
		results = append(results, storagev1.OversightAppendResult{ID: record.ID, Seq: seq})
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return results, nil
}

const oversightDecisionColumns = `d.seq, d.namespace, d.id, d.task_name, d.agent_name, d.workload_identity, d.run_id,
	d.tool_name, d.disposition, d.would_have_paused, d.max_score, d.signature_hash, d.observed_at, d.recorded_at,
	d.action_narrative, d.search_text, d.digest, d.body`

func scanOversightDecision(row interface{ Scan(...any) error }) (storagev1.OversightDecisionRecord, error) {
	var r storagev1.OversightDecisionRecord
	err := row.Scan(&r.Seq, &r.Namespace, &r.ID, &r.TaskName, &r.AgentName, &r.WorkloadIdentity, &r.RunID, &r.ToolName,
		&r.Disposition, &r.WouldHavePaused, &r.MaxScore, &r.SignatureHash, &r.ObservedAt, &r.RecordedAt,
		&r.ActionNarrative, &r.SearchText, &r.Digest, &r.Body)
	return r, err
}

// GetOversightDecision returns one decision with its facets.
func (s *Store) GetOversightDecision(ctx context.Context, namespace, id string) (storagev1.OversightDecisionRecord, error) {
	record, err := scanOversightDecision(s.db.QueryRowContext(ctx,
		`SELECT `+oversightDecisionColumns+` FROM oversight_decisions d WHERE d.namespace = ? AND d.id = ?`, namespace, id))
	if errors.Is(err, sql.ErrNoRows) {
		return storagev1.OversightDecisionRecord{}, store.ErrNotFound
	}
	if err != nil {
		return storagev1.OversightDecisionRecord{}, fmt.Errorf("get oversight decision: %w", err)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT kind, value FROM oversight_decision_facets WHERE seq = ? ORDER BY kind, value`, record.Seq)
	if err != nil {
		return storagev1.OversightDecisionRecord{}, fmt.Errorf("get oversight decision facets: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var facet storagev1.OversightFacet
		if err := rows.Scan(&facet.Kind, &facet.Value); err != nil {
			return storagev1.OversightDecisionRecord{}, err
		}
		record.Facets = append(record.Facets, facet)
	}
	return record, rows.Err()
}

var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// globPattern turns a glob whose "*" stands for any text into a LIKE
// pattern that matches everything else literally.
func globPattern(glob string) string {
	parts := strings.Split(glob, "*")
	for i, part := range parts {
		parts[i] = likeEscaper.Replace(part)
	}
	return strings.Join(parts, "%")
}

func facetCondition(match storagev1.OversightFacetMatch) (string, []any) {
	if match.Glob {
		return `EXISTS (SELECT 1 FROM oversight_decision_facets f WHERE f.namespace = d.namespace AND f.seq = d.seq
			AND f.kind = ? AND f.value LIKE ? ESCAPE '\')`, []any{match.Kind, globPattern(match.Value)}
	}
	return `EXISTS (SELECT 1 FROM oversight_decision_facets f WHERE f.namespace = d.namespace AND f.seq = d.seq
		AND f.kind = ? AND f.value = ?)`, []any{match.Kind, match.Value}
}

func inList(column string, values []string) (string, []any) {
	marks := strings.TrimSuffix(strings.Repeat("?, ", len(values)), ", ")
	args := make([]any, len(values))
	for i, value := range values {
		args[i] = value
	}
	return column + ` IN (` + marks + `)`, args
}

const latestOversightFeedbackLabel = `(SELECT fb.label FROM oversight_feedback fb WHERE fb.namespace = d.namespace
	AND fb.decision_id = d.id ORDER BY fb.seq DESC LIMIT 1)`

//nolint:gocyclo // One flat list of independent optional filters.
func oversightFilterSQL(filter storagev1.OversightDecisionFilter) (string, []any) {
	where := []string{`d.namespace = ?`}
	args := []any{filter.Namespace}
	add := func(condition string, values ...any) {
		where = append(where, condition)
		args = append(args, values...)
	}
	for column, values := range map[string][]string{
		"d.id": filter.IDs, "d.task_name": filter.TaskNames, "d.agent_name": filter.AgentNames,
		"d.workload_identity": filter.WorkloadIdentities,
		"d.tool_name":         filter.ToolNames, "d.disposition": filter.Dispositions,
	} {
		if len(values) > 0 {
			condition, values := inList(column, values)
			add(condition, values...)
		}
	}
	if filter.TaskPrefix != "" {
		add(`d.task_name LIKE ? ESCAPE '\'`, likeEscaper.Replace(filter.TaskPrefix)+"%")
	}
	if filter.RunID != "" {
		add(`d.run_id = ?`, filter.RunID)
	}
	if filter.WouldHavePaused != nil {
		add(`d.would_have_paused = ?`, *filter.WouldHavePaused)
	}
	if filter.MinScore != nil {
		add(`d.max_score >= ?`, *filter.MinScore)
	}
	if filter.SignatureHash != "" {
		add(`d.signature_hash = ?`, filter.SignatureHash)
	}
	for _, match := range filter.Facets {
		condition, values := facetCondition(match)
		add(condition, values...)
	}
	if len(filter.AnyFacets) > 0 {
		alternatives := make([]string, 0, len(filter.AnyFacets))
		var values []any
		for _, match := range filter.AnyFacets {
			condition, matchValues := facetCondition(match)
			alternatives = append(alternatives, condition)
			values = append(values, matchValues...)
		}
		add(`(`+strings.Join(alternatives, ` OR `)+`)`, values...)
	}
	for _, term := range filter.Text {
		add(`d.search_text LIKE ? ESCAPE '\'`, "%"+likeEscaper.Replace(strings.ToLower(term))+"%")
	}
	switch filter.Feedback {
	case "":
	case storagev1.OversightFeedbackUnreviewed:
		add(latestOversightFeedbackLabel + ` IS NULL`)
	case storagev1.OversightFeedbackReviewed:
		add(latestOversightFeedbackLabel + ` IS NOT NULL`)
	default:
		add(latestOversightFeedbackLabel+` = ?`, filter.Feedback)
	}
	if !filter.ObservedSince.IsZero() {
		add(`d.observed_at >= ?`, filter.ObservedSince.UTC())
	}
	if !filter.ObservedBefore.IsZero() {
		add(`d.observed_at < ?`, filter.ObservedBefore.UTC())
	}
	if filter.AfterSeq > 0 {
		add(`d.seq > ?`, filter.AfterSeq)
	}
	if filter.BeforeSeq > 0 {
		add(`d.seq < ?`, filter.BeforeSeq)
	}
	return strings.Join(where, " AND "), args
}

// ListOversightDecisions lists matching decisions of one namespace, without
// their facets.
func (s *Store) ListOversightDecisions(
	ctx context.Context, filter storagev1.OversightDecisionFilter,
) ([]storagev1.OversightDecisionRecord, error) {
	if strings.TrimSpace(filter.Namespace) == "" {
		return nil, fmt.Errorf("%w: namespace is required", store.ErrValidation)
	}
	limit := filter.Limit
	if limit <= 0 || limit > storagev1.MaxOversightListLimit {
		limit = storagev1.MaxOversightListLimit
	}
	where, args := oversightFilterSQL(filter)
	order := "ASC"
	if filter.Descending {
		order = "DESC"
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+oversightDecisionColumns+` FROM oversight_decisions d WHERE `+where+
		` ORDER BY d.seq `+order+` LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, fmt.Errorf("list oversight decisions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var records []storagev1.OversightDecisionRecord
	for rows.Next() {
		record, err := scanOversightDecision(rows)
		if err != nil {
			return nil, fmt.Errorf("list oversight decisions: %w", err)
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

// RecordOversightFeedback appends outcome labels for stored decisions.
func (s *Store) RecordOversightFeedback(
	ctx context.Context, namespace string, records []storagev1.OversightFeedbackRecord, at time.Time,
) ([]storagev1.OversightFeedbackRecord, error) {
	if strings.TrimSpace(namespace) == "" {
		return nil, fmt.Errorf("%w: namespace is required", store.ErrValidation)
	}
	if len(records) == 0 || len(records) > storagev1.MaxOversightListLimit {
		return nil, fmt.Errorf("%w: feedback holds 1 to %d labels", store.ErrValidation, storagev1.MaxOversightListLimit)
	}
	for _, record := range records {
		if strings.TrimSpace(record.DecisionID) == "" || strings.TrimSpace(record.Label) == "" {
			return nil, fmt.Errorf("%w: feedback needs a decision and a label", store.ErrValidation)
		}
		if record.Namespace != "" && record.Namespace != namespace {
			return nil, fmt.Errorf("%w: feedback belongs to another namespace", store.ErrValidation)
		}
	}
	return writeOperation(ctx, s, "RecordOversightFeedback", nil,
		func(ctx context.Context) ([]storagev1.OversightFeedbackRecord, error) {
			return s.recordOversightFeedback(ctx, namespace, records, at.UTC())
		})
}

func (s *Store) recordOversightFeedback(
	ctx context.Context, namespace string, records []storagev1.OversightFeedbackRecord, at time.Time,
) ([]storagev1.OversightFeedbackRecord, error) {
	tx, err := s.beginTx(ctx)
	if err != nil {
		return nil, fmt.Errorf("record oversight feedback: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	stored := make([]storagev1.OversightFeedbackRecord, 0, len(records))
	for _, record := range records {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM oversight_decisions WHERE namespace = ? AND id = ?`,
			namespace, record.DecisionID).Scan(&exists); err != nil {
			return nil, fmt.Errorf("record oversight feedback: %w", err)
		}
		if exists == 0 {
			return nil, fmt.Errorf("%w: oversight decision %q", store.ErrNotFound, record.DecisionID)
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO oversight_feedback (namespace, decision_id, label, note, reviewer, recorded_at)
			VALUES (?, ?, ?, ?, ?, ?)`, namespace, record.DecisionID, record.Label, record.Note, record.Reviewer, at)
		if err != nil {
			return nil, fmt.Errorf("record oversight feedback: %w", err)
		}
		record.Namespace, record.RecordedAt = namespace, at
		if record.Seq, err = result.LastInsertId(); err != nil {
			return nil, err
		}
		stored = append(stored, record)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return stored, nil
}

// ListLatestOversightFeedback returns the latest label of each named decision.
func (s *Store) ListLatestOversightFeedback(
	ctx context.Context, namespace string, decisionIDs []string,
) (map[string]storagev1.OversightFeedbackRecord, error) {
	latest := map[string]storagev1.OversightFeedbackRecord{}
	for start := 0; start < len(decisionIDs); start += 500 {
		chunk := decisionIDs[start:min(start+500, len(decisionIDs))]
		condition, args := inList("decision_id", chunk)
		rows, err := s.db.QueryContext(ctx, `SELECT seq, decision_id, label, note, reviewer, recorded_at FROM oversight_feedback
			WHERE namespace = ? AND `+condition+` ORDER BY seq`, append([]any{namespace}, args...)...)
		if err != nil {
			return nil, fmt.Errorf("list oversight feedback: %w", err)
		}
		for rows.Next() {
			record := storagev1.OversightFeedbackRecord{Namespace: namespace}
			if err := rows.Scan(&record.Seq, &record.DecisionID, &record.Label, &record.Note, &record.Reviewer, &record.RecordedAt); err != nil {
				_ = rows.Close()
				return nil, err
			}
			latest[record.DecisionID] = record
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return latest, nil
}
