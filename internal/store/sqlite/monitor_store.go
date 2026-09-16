package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/store"
)

func defaultMonitorLimit(limit int) int {
	if limit <= 0 {
		return 20
	}
	if limit > 200 {
		return 200
	}
	return limit
}

// monitorQuery accumulates one namespace-scoped monitor SELECT and its bound
// arguments so every paged list shares the same cursor, limit, and row loop.
type monitorQuery struct {
	sql  strings.Builder
	args []any
}

func newMonitorQuery(selectSQL, namespace string) *monitorQuery {
	q := &monitorQuery{args: []any{namespace}}
	q.sql.WriteString(selectSQL)
	q.sql.WriteString(" WHERE monitor_namespace = ?")
	return q
}

// monitorFilter appends an equality predicate unless value is its zero value.
func monitorFilter[T comparable](q *monitorQuery, column string, value T) {
	var zero T
	if value == zero {
		return
	}
	q.sql.WriteString(" AND " + column + " = ?")
	q.args = append(q.args, value)
}

// queryOne scans exactly one row and maps sql.ErrNoRows to store.ErrNotFound.
func queryOne[T any](ctx context.Context, db *sql.DB, scan func(rowScanner) (T, error), query string, args ...any) (*T, error) {
	value, err := scan(db.QueryRowContext(ctx, query, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &value, nil
}

// queryPage runs one offset-paged monitor query and returns the next cursor.
func queryPage[T any](ctx context.Context, db *sql.DB, q *monitorQuery, orderBy, cursor string, limit int, scan func(rowScanner) (T, error)) ([]T, string, error) {
	offset, err := parseOffsetCursor(cursor)
	if err != nil {
		return nil, "", err
	}
	limit = defaultMonitorLimit(limit)
	q.sql.WriteString(" ORDER BY " + orderBy + " LIMIT ? OFFSET ?")
	args := append(q.args, limit, offset)
	rows, err := db.QueryContext(ctx, q.sql.String(), args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close() //nolint:errcheck
	var results []T
	for rows.Next() {
		value, err := scan(rows)
		if err != nil {
			return nil, "", err
		}
		results = append(results, value)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	return results, nextOffsetCursor(offset, len(results), limit), nil
}

// UpsertRepositoryMonitor inserts or updates normalized monitor metadata.
func (s *Store) UpsertRepositoryMonitor(ctx context.Context, monitor *store.RepositoryMonitorRecord) error {
	if monitor == nil {
		return store.ValidationErrorf("repository monitor is required")
	}
	if monitor.Namespace == "" || monitor.Name == "" {
		return store.ValidationErrorf("repository monitor namespace and name are required")
	}
	now := time.Now()
	if monitor.CreatedAt.IsZero() {
		monitor.CreatedAt = now
	}
	monitor.UpdatedAt = now

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var existing store.RepositoryMonitorRecord
	err = tx.QueryRowContext(ctx,
		`SELECT namespace, name, uid, repo_url, owner, repository, branch, generation, created_at, updated_at
		 FROM repository_monitors
		 WHERE namespace = ? AND name = ?`,
		monitor.Namespace, monitor.Name,
	).Scan(
		&existing.Namespace, &existing.Name, &existing.UID, &existing.RepoURL, &existing.Owner,
		&existing.Repository, &existing.Branch, &existing.Generation, &existing.CreatedAt, &existing.UpdatedAt,
	)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return err
	case repositoryMonitorIdentityChanged(existing, *monitor):
		if _, err := deleteRepositoryMonitorDependentState(ctx, tx, monitor.Namespace, monitor.Name); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM repository_monitors WHERE namespace = ? AND name = ?`,
			monitor.Namespace, monitor.Name,
		); err != nil {
			return err
		}
	}

	_, err = tx.ExecContext(ctx,
		`INSERT INTO repository_monitors
		 (namespace, name, uid, repo_url, owner, repository, branch, generation, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(namespace, name) DO UPDATE SET
		   uid = excluded.uid,
		   repo_url = excluded.repo_url,
		   owner = excluded.owner,
		   repository = excluded.repository,
		   branch = excluded.branch,
		   generation = excluded.generation,
		   updated_at = excluded.updated_at`,
		monitor.Namespace, monitor.Name, monitor.UID, monitor.RepoURL, monitor.Owner, monitor.Repository,
		monitor.Branch, monitor.Generation, monitor.CreatedAt, monitor.UpdatedAt,
	)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func repositoryMonitorIdentityChanged(existing, next store.RepositoryMonitorRecord) bool {
	return existing.UID != next.UID ||
		existing.RepoURL != next.RepoURL ||
		existing.Owner != next.Owner ||
		existing.Repository != next.Repository ||
		existing.Branch != next.Branch
}

// GetRepositoryMonitor fetches normalized monitor metadata.
func (s *Store) GetRepositoryMonitor(ctx context.Context, namespace, name string) (*store.RepositoryMonitorRecord, error) {
	var monitor store.RepositoryMonitorRecord
	err := s.db.QueryRowContext(ctx,
		`SELECT namespace, name, uid, repo_url, owner, repository, branch, generation, created_at, updated_at
		 FROM repository_monitors
		 WHERE namespace = ? AND name = ?`,
		namespace, name,
	).Scan(
		&monitor.Namespace, &monitor.Name, &monitor.UID, &monitor.RepoURL, &monitor.Owner,
		&monitor.Repository, &monitor.Branch, &monitor.Generation, &monitor.CreatedAt, &monitor.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &monitor, nil
}

// DeleteRepositoryMonitor deletes normalized monitor metadata.
func (s *Store) DeleteRepositoryMonitor(ctx context.Context, namespace, name string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var deletedRows int64
	result, err := tx.ExecContext(ctx,
		`DELETE FROM repository_monitors WHERE namespace = ? AND name = ?`,
		namespace, name,
	)
	if err != nil {
		return err
	}
	if rows, err := result.RowsAffected(); err == nil {
		deletedRows += rows
	}

	dependentRows, err := deleteRepositoryMonitorDependentState(ctx, tx, namespace, name)
	if err != nil {
		return err
	}
	deletedRows += dependentRows
	if deletedRows == 0 {
		return store.ErrNotFound
	}
	return tx.Commit()
}

func deleteRepositoryMonitorDependentState(ctx context.Context, tx *sql.Tx, namespace, name string) (int64, error) {
	var deletedRows int64
	for _, stmt := range []string{
		`DELETE FROM monitor_runs WHERE monitor_namespace = ? AND monitor_name = ?`,
		`DELETE FROM monitor_items WHERE monitor_namespace = ? AND monitor_name = ?`,
		`DELETE FROM work_actions WHERE monitor_namespace = ? AND monitor_name = ?`,
		`DELETE FROM action_records WHERE monitor_namespace = ? AND monitor_name = ?`,
		`DELETE FROM review_records WHERE monitor_namespace = ? AND monitor_name = ?`,
		`DELETE FROM review_publish_records WHERE monitor_namespace = ? AND monitor_name = ?`,
		`DELETE FROM command_events WHERE monitor_namespace = ? AND monitor_name = ?`,
		`DELETE FROM implementation_jobs WHERE monitor_namespace = ? AND monitor_name = ?`,
		`DELETE FROM github_mutation_records WHERE monitor_namespace = ? AND monitor_name = ?`,
		`DELETE FROM repair_jobs WHERE monitor_namespace = ? AND monitor_name = ?`,
		`DELETE FROM monitor_events WHERE monitor_namespace = ? AND monitor_name = ?`,
	} {
		result, err := tx.ExecContext(ctx, stmt, namespace, name)
		if err != nil {
			return 0, err
		}
		if rows, err := result.RowsAffected(); err == nil {
			deletedRows += rows
		}
	}
	return deletedRows, nil
}

// CreateMonitorRun inserts a new monitor run.
func (s *Store) CreateMonitorRun(ctx context.Context, run *store.MonitorRun) error {
	if run == nil {
		return store.ValidationErrorf("monitor run is required")
	}
	if run.ID == "" || run.MonitorNamespace == "" || run.MonitorName == "" {
		return store.ValidationErrorf("monitor run id, namespace, and monitor name are required")
	}
	if run.Phase == "" {
		run.Phase = "queued"
	}
	if run.StartedAt.IsZero() {
		run.StartedAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO monitor_runs
		 (id, monitor_namespace, monitor_name, trigger, target_kind, target_number, target_sha, command_event_id, phase,
		  started_at, completed_at, selected_count, created_task_count, skipped_count, error)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		run.ID, run.MonitorNamespace, run.MonitorName, run.Trigger, run.TargetKind, run.TargetNumber,
		run.TargetSHA, run.CommandEventID, run.Phase, run.StartedAt, run.CompletedAt, run.SelectedCount, run.CreatedTaskCount,
		run.SkippedCount, run.Error,
	)
	if err != nil && isSQLiteConstraintError(err) {
		return fmt.Errorf("%w: active monitor run already exists", store.ErrConflict)
	}
	return err
}

// UpdateMonitorRun updates a monitor run.
func (s *Store) UpdateMonitorRun(ctx context.Context, run *store.MonitorRun) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE monitor_runs
		 SET trigger = ?, target_kind = ?, target_number = ?, target_sha = ?, command_event_id = ?, phase = ?,
		     started_at = ?, completed_at = ?, selected_count = ?, created_task_count = ?,
		     skipped_count = ?, error = ?
		 WHERE monitor_namespace = ? AND id = ?`,
		run.Trigger, run.TargetKind, run.TargetNumber, run.TargetSHA, run.CommandEventID, run.Phase, run.StartedAt,
		run.CompletedAt, run.SelectedCount, run.CreatedTaskCount, run.SkippedCount, run.Error,
		run.MonitorNamespace, run.ID,
	)
	if err != nil && isSQLiteConstraintError(err) {
		return fmt.Errorf("%w: active monitor run already exists", store.ErrConflict)
	}
	return err
}

// GetMonitorRun fetches a monitor run by ID.
func (s *Store) GetMonitorRun(ctx context.Context, namespace, id string) (*store.MonitorRun, error) {
	return queryOne(ctx, s.db, scanMonitorRun, monitorRunSelectSQL()+` WHERE monitor_namespace = ? AND id = ?`, namespace, id)
}

func monitorRunSelectSQL() string {
	return `SELECT id, monitor_namespace, monitor_name, trigger, target_kind, target_number, target_sha, command_event_id,
	        phase, started_at, completed_at, selected_count, created_task_count, skipped_count, error
	        FROM monitor_runs`
}

func scanMonitorRun(r rowScanner) (store.MonitorRun, error) {
	var run store.MonitorRun
	var completedAt sql.NullTime
	if err := r.Scan(
		&run.ID, &run.MonitorNamespace, &run.MonitorName, &run.Trigger, &run.TargetKind, &run.TargetNumber,
		&run.TargetSHA, &run.CommandEventID, &run.Phase, &run.StartedAt, &completedAt, &run.SelectedCount,
		&run.CreatedTaskCount, &run.SkippedCount, &run.Error,
	); err != nil {
		return store.MonitorRun{}, err
	}
	if completedAt.Valid {
		run.CompletedAt = &completedAt.Time
	}
	return run, nil
}

// ListMonitorRuns lists monitor runs ordered newest first unless OldestFirst is set.
func (s *Store) ListMonitorRuns(ctx context.Context, filter store.MonitorRunFilter) ([]store.MonitorRun, string, error) {
	q := newMonitorQuery(monitorRunSelectSQL(), filter.Namespace)
	monitorFilter(q, "monitor_name", filter.MonitorName)
	monitorFilter(q, "trigger", filter.Trigger)
	monitorFilter(q, "target_kind", filter.TargetKind)
	monitorFilter(q, "target_number", filter.TargetNumber)
	monitorFilter(q, "target_sha", filter.TargetSHA)
	monitorFilter(q, "phase", filter.Phase)
	orderBy := "started_at DESC, id DESC"
	if filter.OldestFirst {
		orderBy = "started_at ASC, id ASC"
	}
	return queryPage(ctx, s.db, q, orderBy, filter.Cursor, filter.Limit, scanMonitorRun)
}

func monitorItemKey(item *store.MonitorItem) string {
	if item.ItemKey != "" {
		return item.ItemKey
	}
	if item.Kind == "commit" && item.SHA != "" {
		return item.SHA
	}
	if item.Number != 0 {
		return strconv.FormatInt(item.Number, 10)
	}
	return item.SHA
}

// UpsertMonitorItem inserts or updates the latest state for one monitor item.
func (s *Store) UpsertMonitorItem(ctx context.Context, item *store.MonitorItem) error {
	if item == nil {
		return store.ValidationErrorf("monitor item is required")
	}
	item.ItemKey = monitorItemKey(item)
	if item.MonitorNamespace == "" || item.MonitorName == "" || item.Kind == "" || item.ItemKey == "" {
		return store.ValidationErrorf("monitor item namespace, monitor name, kind, and item key are required")
	}
	now := time.Now()
	if item.LastSeenAt.IsZero() {
		item.LastSeenAt = now
	}
	item.UpdatedAt = now
	if item.GitHubUpdatedAt.IsZero() {
		item.GitHubUpdatedAt = item.UpdatedAt
	}
	if item.LabelsJSON == "" {
		item.LabelsJSON = "[]"
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO monitor_items
		 (monitor_namespace, monitor_name, kind, item_key, number, sha, title, body, html_url, author, state, labels_json,
		  snapshot_digest, github_updated_at, workflow_phase, linked_pr_number, last_command_id, last_command_intent,
		  last_action_id, last_action_kind, last_action_task_name, base_branch, head_branch, head_sha, base_sha, draft, mergeable_state, ci_state, skip_reason,
		  last_review_id, last_reviewed_head_sha, last_verdict, repair_state, automerge_state, status_comment_id,
		  status_comment_url, last_publish_id, last_publish_phase, last_publish_reason, last_publish_url, updated_at, last_seen_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(monitor_namespace, monitor_name, kind, item_key) DO UPDATE SET
		   number = excluded.number,
		   sha = excluded.sha,
		   title = excluded.title,
		   body = excluded.body,
		   html_url = excluded.html_url,
		   author = excluded.author,
		   state = excluded.state,
		   labels_json = excluded.labels_json,
		   snapshot_digest = excluded.snapshot_digest,
		   github_updated_at = excluded.github_updated_at,
		   workflow_phase = excluded.workflow_phase,
		   linked_pr_number = excluded.linked_pr_number,
		   last_command_id = excluded.last_command_id,
		   last_command_intent = excluded.last_command_intent,
		   last_action_id = excluded.last_action_id,
		   last_action_kind = excluded.last_action_kind,
		   last_action_task_name = excluded.last_action_task_name,
		   base_branch = excluded.base_branch,
		   head_branch = excluded.head_branch,
		   head_sha = excluded.head_sha,
		   base_sha = excluded.base_sha,
		   draft = excluded.draft,
		   mergeable_state = excluded.mergeable_state,
		   ci_state = excluded.ci_state,
		   skip_reason = excluded.skip_reason,
		   last_review_id = excluded.last_review_id,
		   last_reviewed_head_sha = excluded.last_reviewed_head_sha,
		   last_verdict = excluded.last_verdict,
		   repair_state = excluded.repair_state,
		   automerge_state = excluded.automerge_state,
		   status_comment_id = excluded.status_comment_id,
		   status_comment_url = excluded.status_comment_url,
		   last_publish_id = excluded.last_publish_id,
		   last_publish_phase = excluded.last_publish_phase,
		   last_publish_reason = excluded.last_publish_reason,
		   last_publish_url = excluded.last_publish_url,
		   updated_at = excluded.updated_at,
		   last_seen_at = excluded.last_seen_at`,
		item.MonitorNamespace, item.MonitorName, item.Kind, item.ItemKey, item.Number, item.SHA,
		item.Title, item.Body, item.HTMLURL, item.Author, item.State, item.LabelsJSON, item.SnapshotDigest, item.GitHubUpdatedAt,
		item.WorkflowPhase, item.LinkedPRNumber, item.LastCommandID, item.LastCommandIntent,
		item.LastActionID, item.LastActionKind, item.LastActionTaskName, item.BaseBranch, item.HeadBranch,
		item.HeadSHA, item.BaseSHA, item.Draft, item.MergeableState, item.CIState, item.SkipReason,
		item.LastReviewID, item.LastReviewedHeadSHA, item.LastVerdict, item.RepairState, item.AutomergeState,
		item.StatusCommentID, item.StatusCommentURL, item.LastPublishID, item.LastPublishPhase, item.LastPublishReason,
		item.LastPublishURL, item.UpdatedAt, item.LastSeenAt,
	)
	return err
}

// GetMonitorItem fetches one monitor item by its durable key.
func (s *Store) GetMonitorItem(ctx context.Context, namespace, monitorName, kind, itemKey string) (*store.MonitorItem, error) {
	return queryOne(ctx, s.db, scanMonitorItem, monitorItemSelectSQL()+`
		 WHERE monitor_namespace = ? AND monitor_name = ? AND kind = ? AND item_key = ?`,
		namespace, monitorName, kind, itemKey)
}

func monitorItemSelectSQL() string {
	return `SELECT monitor_namespace, monitor_name, kind, item_key, number, sha, title, body, html_url, author, state, labels_json,
	        snapshot_digest, github_updated_at, workflow_phase, linked_pr_number, last_command_id, last_command_intent,
	        last_action_id, last_action_kind, last_action_task_name, base_branch, head_branch, head_sha, base_sha, draft, mergeable_state, ci_state, skip_reason,
	        last_review_id, last_reviewed_head_sha, last_verdict, repair_state, automerge_state, status_comment_id,
	        status_comment_url, last_publish_id, last_publish_phase, last_publish_reason, last_publish_url,
	        updated_at, last_seen_at FROM monitor_items`
}

func scanMonitorItem(r rowScanner) (store.MonitorItem, error) {
	var item store.MonitorItem
	err := r.Scan(
		&item.MonitorNamespace, &item.MonitorName, &item.Kind, &item.ItemKey, &item.Number, &item.SHA,
		&item.Title, &item.Body, &item.HTMLURL, &item.Author, &item.State, &item.LabelsJSON, &item.SnapshotDigest, &item.GitHubUpdatedAt,
		&item.WorkflowPhase, &item.LinkedPRNumber, &item.LastCommandID, &item.LastCommandIntent,
		&item.LastActionID, &item.LastActionKind, &item.LastActionTaskName, &item.BaseBranch, &item.HeadBranch,
		&item.HeadSHA, &item.BaseSHA, &item.Draft, &item.MergeableState, &item.CIState, &item.SkipReason,
		&item.LastReviewID, &item.LastReviewedHeadSHA, &item.LastVerdict, &item.RepairState, &item.AutomergeState,
		&item.StatusCommentID, &item.StatusCommentURL, &item.LastPublishID, &item.LastPublishPhase, &item.LastPublishReason,
		&item.LastPublishURL, &item.UpdatedAt, &item.LastSeenAt,
	)
	return item, err
}

// ListMonitorItems lists current monitor items.
func (s *Store) ListMonitorItems(ctx context.Context, filter store.MonitorItemFilter) ([]store.MonitorItem, string, error) {
	q := newMonitorQuery(monitorItemSelectSQL(), filter.Namespace)
	monitorFilter(q, "monitor_name", filter.MonitorName)
	monitorFilter(q, "kind", filter.Kind)
	monitorFilter(q, "number", filter.Number)
	monitorFilter(q, "state", filter.State)
	monitorFilter(q, "last_verdict", filter.ReviewVerdict)
	monitorFilter(q, "repair_state", filter.RepairState)
	monitorFilter(q, "automerge_state", filter.AutomergeState)
	return queryPage(ctx, s.db, q, "updated_at DESC, item_key ASC", filter.Cursor, filter.Limit, scanMonitorItem)
}

// CreateActionRecord inserts a durable action record.
func (s *Store) CreateActionRecord(ctx context.Context, record *store.ActionRecord) error {
	if record == nil {
		return store.ValidationErrorf("action record is required")
	}
	if record.ID == "" || record.MonitorNamespace == "" || record.MonitorName == "" || record.Kind == "" || record.ActionKind == "" {
		return store.ValidationErrorf("action record id, monitor namespace, monitor name, kind, and action kind are required")
	}
	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Now()
	}
	if record.PayloadJSON == "" {
		record.PayloadJSON = "{}"
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO action_records
		 (id, monitor_namespace, monitor_name, kind, number, action_kind, snapshot_digest, head_sha,
		  task_name, command_event_id, work_action_id, monitor_generation, verdict, confidence, summary,
		  payload_json, payload_digest, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		record.ID, record.MonitorNamespace, record.MonitorName, record.Kind, record.Number, record.ActionKind,
		record.SnapshotDigest, record.HeadSHA, record.TaskName, record.CommandEventID, record.WorkActionID,
		record.MonitorGeneration, record.Verdict, record.Confidence, record.Summary, record.PayloadJSON,
		record.PayloadDigest, record.CreatedAt,
	)
	return err
}

// UpdateActionRecord replaces result fields when sensitive content must be redacted.
func (s *Store) UpdateActionRecord(ctx context.Context, record *store.ActionRecord) error {
	if record == nil {
		return store.ValidationErrorf("action record is required")
	}
	if record.ID == "" || record.MonitorNamespace == "" {
		return store.ValidationErrorf("action record id and monitor namespace are required")
	}
	if record.PayloadJSON == "" {
		record.PayloadJSON = "{}"
	}
	result, err := s.db.ExecContext(ctx,
		`UPDATE action_records
		 SET verdict = ?, confidence = ?, summary = ?, payload_json = ?, payload_digest = ?
		 WHERE monitor_namespace = ? AND id = ?`,
		record.Verdict, record.Confidence, record.Summary, record.PayloadJSON, record.PayloadDigest,
		record.MonitorNamespace, record.ID,
	)
	if err != nil {
		return err
	}
	if rows, err := result.RowsAffected(); err == nil && rows == 0 {
		return store.ErrNotFound
	}
	return nil
}

// GetActionRecord fetches an action record by ID.
func (s *Store) GetActionRecord(ctx context.Context, namespace, id string) (*store.ActionRecord, error) {
	return queryOne(ctx, s.db, scanActionRecord, actionRecordSelectSQL()+` WHERE monitor_namespace = ? AND id = ?`, namespace, id)
}

func actionRecordSelectSQL() string {
	return `SELECT id, monitor_namespace, monitor_name, kind, number, action_kind, snapshot_digest, head_sha,
	        task_name, command_event_id, work_action_id, monitor_generation, verdict, confidence, summary,
	        payload_json, payload_digest, created_at FROM action_records`
}

func scanActionRecord(r rowScanner) (store.ActionRecord, error) {
	var record store.ActionRecord
	err := r.Scan(
		&record.ID, &record.MonitorNamespace, &record.MonitorName, &record.Kind, &record.Number,
		&record.ActionKind, &record.SnapshotDigest, &record.HeadSHA, &record.TaskName, &record.CommandEventID,
		&record.WorkActionID, &record.MonitorGeneration, &record.Verdict, &record.Confidence, &record.Summary,
		&record.PayloadJSON, &record.PayloadDigest, &record.CreatedAt,
	)
	return record, err
}

// ListActionRecords lists action records ordered newest first.
func (s *Store) ListActionRecords(ctx context.Context, filter store.ActionRecordFilter) ([]store.ActionRecord, string, error) {
	q := newMonitorQuery(actionRecordSelectSQL(), filter.Namespace)
	monitorFilter(q, "monitor_name", filter.MonitorName)
	monitorFilter(q, "kind", filter.Kind)
	monitorFilter(q, "number", filter.Number)
	monitorFilter(q, "action_kind", filter.ActionKind)
	monitorFilter(q, "task_name", filter.TaskName)
	return queryPage(ctx, s.db, q, "created_at DESC, id DESC", filter.Cursor, filter.Limit, scanActionRecord)
}

// CreateReviewRecord inserts an immutable review record.
func (s *Store) CreateReviewRecord(ctx context.Context, record *store.ReviewRecord) error {
	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO review_records
		 (id, monitor_namespace, monitor_name, kind, number, head_sha, task_name, task_namespace,
		  verdict, confidence, repairable, security_status, findings_json, summary, suggested_comment,
		  validation_task, validation_image, validation_command_digest, validation_status, validation_evidence,
		  rendered_comment, marker, github_review_id, github_comment_id, github_comment_url, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		record.ID, record.MonitorNamespace, record.MonitorName, record.Kind, record.Number, record.HeadSHA,
		record.TaskName, record.TaskNamespace, record.Verdict, record.Confidence, record.Repairable,
		record.SecurityStatus, record.FindingsJSON, record.Summary, record.SuggestedComment,
		record.ValidationTask, record.ValidationImage, record.ValidationCommandDigest, record.ValidationStatus,
		record.ValidationEvidence,
		record.RenderedComment, record.Marker, record.GitHubReviewID, record.GitHubCommentID,
		record.GitHubCommentURL, record.CreatedAt,
	)
	return err
}

// GetReviewRecord fetches a review record by ID.
func (s *Store) GetReviewRecord(ctx context.Context, namespace, id string) (*store.ReviewRecord, error) {
	return queryOne(ctx, s.db, scanReviewRecord, reviewRecordSelectSQL()+" WHERE monitor_namespace = ? AND id = ?", namespace, id)
}

func reviewRecordSelectSQL() string {
	return `SELECT id, monitor_namespace, monitor_name, kind, number, head_sha, task_name, task_namespace,
	        verdict, confidence, repairable, security_status, findings_json, summary, suggested_comment,
	        validation_task, validation_image, validation_command_digest, validation_status, validation_evidence,
	        rendered_comment, marker, github_review_id, github_comment_id, github_comment_url, created_at
	        FROM review_records`
}

func scanReviewRecord(r rowScanner) (store.ReviewRecord, error) {
	var record store.ReviewRecord
	err := r.Scan(
		&record.ID, &record.MonitorNamespace, &record.MonitorName, &record.Kind, &record.Number,
		&record.HeadSHA, &record.TaskName, &record.TaskNamespace, &record.Verdict, &record.Confidence,
		&record.Repairable, &record.SecurityStatus, &record.FindingsJSON, &record.Summary,
		&record.SuggestedComment, &record.ValidationTask, &record.ValidationImage,
		&record.ValidationCommandDigest, &record.ValidationStatus, &record.ValidationEvidence,
		&record.RenderedComment, &record.Marker, &record.GitHubReviewID,
		&record.GitHubCommentID, &record.GitHubCommentURL, &record.CreatedAt,
	)
	return record, err
}

// ListReviewRecords lists review records ordered newest first.
func (s *Store) ListReviewRecords(ctx context.Context, filter store.ReviewRecordFilter) ([]store.ReviewRecord, string, error) {
	q := newMonitorQuery(reviewRecordSelectSQL(), filter.Namespace)
	monitorFilter(q, "monitor_name", filter.MonitorName)
	monitorFilter(q, "kind", filter.Kind)
	monitorFilter(q, "number", filter.Number)
	monitorFilter(q, "head_sha", filter.HeadSHA)
	monitorFilter(q, "verdict", filter.Verdict)
	return queryPage(ctx, s.db, q, "created_at DESC, id DESC", filter.Cursor, filter.Limit, scanReviewRecord)
}

// CreateReviewPublishRecord inserts a review publish attempt/outcome record.
func (s *Store) CreateReviewPublishRecord(ctx context.Context, record *store.ReviewPublishRecord) error {
	if record == nil {
		return store.ValidationErrorf("review publish record is required")
	}
	if record.ID == "" || record.MonitorNamespace == "" || record.MonitorName == "" {
		return store.ValidationErrorf("review publish record id, namespace, and monitor name are required")
	}
	now := time.Now()
	if record.CreatedAt.IsZero() {
		record.CreatedAt = now
	}
	if record.UpdatedAt.IsZero() {
		record.UpdatedAt = record.CreatedAt
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO review_publish_records
		 (id, monitor_namespace, monitor_name, item_kind, item_number, head_sha, run_id,
		  review_task_name, review_record_id, phase, event, github_review_id, github_review_url,
		  body_digest, inline_comment_count, skip_reason, error, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		record.ID, record.MonitorNamespace, record.MonitorName, record.ItemKind, record.ItemNumber,
		record.HeadSHA, record.RunID, record.ReviewTaskName, record.ReviewRecordID, record.Phase,
		record.Event, record.GitHubReviewID, record.GitHubReviewURL, record.BodyDigest,
		record.InlineCommentCount, record.SkipReason, record.Error, record.CreatedAt, record.UpdatedAt,
	)
	return err
}

// UpdateReviewPublishRecord updates a review publish attempt/outcome record.
func (s *Store) UpdateReviewPublishRecord(ctx context.Context, record *store.ReviewPublishRecord) error {
	if record == nil {
		return store.ValidationErrorf("review publish record is required")
	}
	if record.ID == "" || record.MonitorNamespace == "" {
		return store.ValidationErrorf("review publish record id and namespace are required")
	}
	if record.UpdatedAt.IsZero() {
		record.UpdatedAt = time.Now()
	}
	result, err := s.db.ExecContext(ctx,
		`UPDATE review_publish_records
		 SET phase = ?, event = ?, github_review_id = ?, github_review_url = ?, body_digest = ?,
		     inline_comment_count = ?, skip_reason = ?, error = ?, updated_at = ?
		 WHERE monitor_namespace = ? AND id = ?`,
		record.Phase, record.Event, record.GitHubReviewID, record.GitHubReviewURL, record.BodyDigest,
		record.InlineCommentCount, record.SkipReason, record.Error, record.UpdatedAt,
		record.MonitorNamespace, record.ID,
	)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return store.ErrNotFound
	}
	return nil
}

// GetReviewPublishRecord fetches a review publish record by ID.
func (s *Store) GetReviewPublishRecord(ctx context.Context, namespace, id string) (*store.ReviewPublishRecord, error) {
	return queryOne(ctx, s.db, scanReviewPublishRecord, reviewPublishRecordSelectSQL()+" WHERE monitor_namespace = ? AND id = ?", namespace, id)
}

func reviewPublishRecordSelectSQL() string {
	return `SELECT id, monitor_namespace, monitor_name, item_kind, item_number, head_sha, run_id,
	        review_task_name, review_record_id, phase, event, github_review_id, github_review_url,
	        body_digest, inline_comment_count, skip_reason, error, created_at, updated_at
	        FROM review_publish_records`
}

func scanReviewPublishRecord(r rowScanner) (store.ReviewPublishRecord, error) {
	var record store.ReviewPublishRecord
	err := r.Scan(
		&record.ID, &record.MonitorNamespace, &record.MonitorName, &record.ItemKind, &record.ItemNumber,
		&record.HeadSHA, &record.RunID, &record.ReviewTaskName, &record.ReviewRecordID, &record.Phase,
		&record.Event, &record.GitHubReviewID, &record.GitHubReviewURL, &record.BodyDigest,
		&record.InlineCommentCount, &record.SkipReason, &record.Error, &record.CreatedAt, &record.UpdatedAt,
	)
	return record, err
}

// ListReviewPublishRecords lists review publish records ordered newest first.
func (s *Store) ListReviewPublishRecords(ctx context.Context, filter store.ReviewPublishRecordFilter) ([]store.ReviewPublishRecord, string, error) {
	q := newMonitorQuery(reviewPublishRecordSelectSQL(), filter.Namespace)
	monitorFilter(q, "monitor_name", filter.MonitorName)
	monitorFilter(q, "item_kind", filter.ItemKind)
	monitorFilter(q, "item_number", filter.ItemNumber)
	monitorFilter(q, "head_sha", filter.HeadSHA)
	monitorFilter(q, "review_record_id", filter.ReviewRecordID)
	monitorFilter(q, "phase", filter.Phase)
	return queryPage(ctx, s.db, q, "updated_at DESC, id DESC", filter.Cursor, filter.Limit, scanReviewPublishRecord)
}

// CreateCommandEvent inserts a maintainer command event.
func (s *Store) CreateCommandEvent(ctx context.Context, event *store.CommandEvent) error {
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO command_events
		 (id, monitor_namespace, monitor_name, repo, kind, number, source, delivery_id, label, monitor_generation,
		  dedupe_key, idempotency_key, comment_id, comment_url, author, author_association, permission,
		  command, intent, head_sha, issue_snapshot_digest, status, status_comment_id,
		  created_repair_job_id, created_at, processed_at, error)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		event.ID, event.MonitorNamespace, event.MonitorName, event.Repo, event.Kind, event.Number,
		event.Source, event.DeliveryID, event.Label, event.MonitorGeneration, event.DedupeKey, event.IdempotencyKey,
		event.CommentID, event.CommentURL, event.Author, event.AuthorAssociation, event.Permission,
		event.Command, event.Intent, event.HeadSHA, event.IssueSnapshotDigest, event.Status, event.StatusCommentID,
		event.CreatedRepairJobID, event.CreatedAt, event.ProcessedAt, event.Error,
	)
	return err
}

// UpdateCommandEvent updates command processing state.
func (s *Store) UpdateCommandEvent(ctx context.Context, event *store.CommandEvent) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE command_events
		 SET repo = ?, kind = ?, number = ?, source = ?, delivery_id = ?, label = ?, monitor_generation = ?,
		     dedupe_key = ?, idempotency_key = ?, comment_id = ?, comment_url = ?, author = ?,
		     author_association = ?, permission = ?, command = ?, intent = ?, head_sha = ?, issue_snapshot_digest = ?,
		     status = ?, status_comment_id = ?, created_repair_job_id = ?, processed_at = ?, error = ?
		 WHERE monitor_namespace = ? AND id = ?`,
		event.Repo, event.Kind, event.Number, event.Source, event.DeliveryID, event.Label, event.MonitorGeneration,
		event.DedupeKey, event.IdempotencyKey, event.CommentID, event.CommentURL, event.Author,
		event.AuthorAssociation, event.Permission, event.Command, event.Intent, event.HeadSHA, event.IssueSnapshotDigest,
		event.Status, event.StatusCommentID, event.CreatedRepairJobID, event.ProcessedAt, event.Error,
		event.MonitorNamespace, event.ID,
	)
	return err
}

// GetCommandEvent fetches a command event by ID.
func (s *Store) GetCommandEvent(ctx context.Context, namespace, id string) (*store.CommandEvent, error) {
	return queryOne(ctx, s.db, scanCommandEvent, commandEventSelectSQL()+` WHERE monitor_namespace = ? AND id = ?`, namespace, id)
}

func commandEventSelectSQL() string {
	return `SELECT id, monitor_namespace, monitor_name, repo, kind, number, source, delivery_id, label, monitor_generation,
	        dedupe_key, idempotency_key, comment_id, comment_url, author, author_association, permission,
	        command, intent, head_sha, issue_snapshot_digest, status, status_comment_id, created_repair_job_id,
	        created_at, processed_at, error FROM command_events`
}

func scanCommandEvent(r rowScanner) (store.CommandEvent, error) {
	var event store.CommandEvent
	var processedAt sql.NullTime
	if err := r.Scan(
		&event.ID, &event.MonitorNamespace, &event.MonitorName, &event.Repo, &event.Kind, &event.Number,
		&event.Source, &event.DeliveryID, &event.Label, &event.MonitorGeneration, &event.DedupeKey, &event.IdempotencyKey,
		&event.CommentID, &event.CommentURL, &event.Author, &event.AuthorAssociation, &event.Permission,
		&event.Command, &event.Intent, &event.HeadSHA, &event.IssueSnapshotDigest, &event.Status, &event.StatusCommentID,
		&event.CreatedRepairJobID, &event.CreatedAt, &processedAt, &event.Error,
	); err != nil {
		return store.CommandEvent{}, err
	}
	if processedAt.Valid {
		event.ProcessedAt = &processedAt.Time
	}
	return event, nil
}

// ListCommandEvents lists command intake events.
func (s *Store) ListCommandEvents(ctx context.Context, filter store.CommandEventFilter) ([]store.CommandEvent, string, error) {
	q := newMonitorQuery(commandEventSelectSQL(), filter.Namespace)
	monitorFilter(q, "monitor_name", filter.MonitorName)
	monitorFilter(q, "kind", filter.Kind)
	monitorFilter(q, "number", filter.Number)
	monitorFilter(q, "intent", filter.Intent)
	monitorFilter(q, "status", filter.Status)
	return queryPage(ctx, s.db, q, "created_at DESC, id DESC", filter.Cursor, filter.Limit, scanCommandEvent)
}

// CreateRepairJob inserts a repair job.
func (s *Store) CreateRepairJob(ctx context.Context, job *store.RepairJob) error {
	now := time.Now()
	if job.CreatedAt.IsZero() {
		job.CreatedAt = now
	}
	job.UpdatedAt = now
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO repair_jobs
		 (id, monitor_namespace, monitor_name, repo, pr_number, intent, source, head_sha, base_sha, base_branch,
		  phase, repair_count_pr, repair_count_head, validation_attempts, review_fix_attempts,
		  task_name, branch, pushed_sha, last_error, created_at, updated_at, completed_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		job.ID, job.MonitorNamespace, job.MonitorName, job.Repo, job.PRNumber, job.Intent, job.Source,
		job.HeadSHA, job.BaseSHA, job.BaseBranch, job.Phase, job.RepairCountPR, job.RepairCountHead,
		job.ValidationAttempts, job.ReviewFixAttempts, job.TaskName, job.Branch, job.PushedSHA,
		job.LastError, job.CreatedAt, job.UpdatedAt, job.CompletedAt,
	)
	return err
}

// UpdateRepairJob updates repair job state.
func (s *Store) UpdateRepairJob(ctx context.Context, job *store.RepairJob) error {
	job.UpdatedAt = time.Now()
	_, err := s.db.ExecContext(ctx,
		`UPDATE repair_jobs
		 SET repo = ?, pr_number = ?, intent = ?, source = ?, head_sha = ?, base_sha = ?, base_branch = ?, phase = ?,
		     repair_count_pr = ?, repair_count_head = ?, validation_attempts = ?, review_fix_attempts = ?,
		     task_name = ?, branch = ?, pushed_sha = ?, last_error = ?, updated_at = ?, completed_at = ?
		 WHERE monitor_namespace = ? AND id = ?`,
		job.Repo, job.PRNumber, job.Intent, job.Source, job.HeadSHA, job.BaseSHA, job.BaseBranch, job.Phase,
		job.RepairCountPR, job.RepairCountHead, job.ValidationAttempts, job.ReviewFixAttempts,
		job.TaskName, job.Branch, job.PushedSHA, job.LastError, job.UpdatedAt, job.CompletedAt,
		job.MonitorNamespace, job.ID,
	)
	return err
}

// GetRepairJob fetches a repair job by ID.
func (s *Store) GetRepairJob(ctx context.Context, namespace, id string) (*store.RepairJob, error) {
	return queryOne(ctx, s.db, scanRepairJob, repairJobSelectSQL()+" WHERE monitor_namespace = ? AND id = ?", namespace, id)
}

func repairJobSelectSQL() string {
	return `SELECT id, monitor_namespace, monitor_name, repo, pr_number, intent, source, head_sha,
	        base_sha, base_branch, phase, repair_count_pr, repair_count_head, validation_attempts, review_fix_attempts,
	        task_name, branch, pushed_sha, last_error, created_at, updated_at, completed_at FROM repair_jobs`
}

func scanRepairJob(r rowScanner) (store.RepairJob, error) {
	var job store.RepairJob
	var completedAt sql.NullTime
	if err := r.Scan(
		&job.ID, &job.MonitorNamespace, &job.MonitorName, &job.Repo, &job.PRNumber, &job.Intent,
		&job.Source, &job.HeadSHA, &job.BaseSHA, &job.BaseBranch, &job.Phase, &job.RepairCountPR, &job.RepairCountHead,
		&job.ValidationAttempts, &job.ReviewFixAttempts, &job.TaskName, &job.Branch, &job.PushedSHA,
		&job.LastError, &job.CreatedAt, &job.UpdatedAt, &completedAt,
	); err != nil {
		return store.RepairJob{}, err
	}
	if completedAt.Valid {
		job.CompletedAt = &completedAt.Time
	}
	return job, nil
}

// ListRepairJobs lists repair jobs ordered by update time.
func (s *Store) ListRepairJobs(ctx context.Context, filter store.RepairJobFilter) ([]store.RepairJob, string, error) {
	q := newMonitorQuery(repairJobSelectSQL(), filter.Namespace)
	monitorFilter(q, "monitor_name", filter.MonitorName)
	monitorFilter(q, "repo", filter.Repo)
	monitorFilter(q, "pr_number", filter.PRNumber)
	monitorFilter(q, "intent", filter.Intent)
	monitorFilter(q, "phase", filter.Phase)
	return queryPage(ctx, s.db, q, "updated_at DESC, id DESC", filter.Cursor, filter.Limit, scanRepairJob)
}

// CreateMonitorEvent inserts an audit event.
func (s *Store) CreateMonitorEvent(ctx context.Context, event *store.MonitorEvent) error {
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now()
	}
	if event.MetadataJSON == "" {
		event.MetadataJSON = "{}"
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO monitor_events
		 (id, monitor_namespace, monitor_name, run_id, item_kind, item_number, item_sha,
		  event_type, actor, summary, metadata_json, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		event.ID, event.MonitorNamespace, event.MonitorName, event.RunID, event.ItemKind,
		event.ItemNumber, event.ItemSHA, event.EventType, event.Actor, event.Summary,
		event.MetadataJSON, event.CreatedAt,
	)
	return err
}

// ListMonitorEvents lists audit events ordered newest first.
func (s *Store) ListMonitorEvents(ctx context.Context, filter store.MonitorEventFilter) ([]store.MonitorEvent, string, error) {
	q := newMonitorQuery(monitorEventSelectSQL(), filter.Namespace)
	monitorFilter(q, "id", filter.ID)
	monitorFilter(q, "monitor_name", filter.MonitorName)
	monitorFilter(q, "run_id", filter.RunID)
	monitorFilter(q, "item_kind", filter.ItemKind)
	monitorFilter(q, "item_number", filter.ItemNumber)
	monitorFilter(q, "event_type", filter.EventType)
	return queryPage(ctx, s.db, q, "created_at DESC, id DESC", filter.Cursor, filter.Limit, scanMonitorEvent)
}

func monitorEventSelectSQL() string {
	return `SELECT id, monitor_namespace, monitor_name, run_id, item_kind, item_number, item_sha,
	        event_type, actor, summary, metadata_json, created_at FROM monitor_events`
}

func scanMonitorEvent(r rowScanner) (store.MonitorEvent, error) {
	var event store.MonitorEvent
	err := r.Scan(
		&event.ID, &event.MonitorNamespace, &event.MonitorName, &event.RunID, &event.ItemKind,
		&event.ItemNumber, &event.ItemSHA, &event.EventType, &event.Actor, &event.Summary,
		&event.MetadataJSON, &event.CreatedAt,
	)
	return event, err
}

// CreateWorkAction inserts a durable workflow action.
func (s *Store) CreateWorkAction(ctx context.Context, action *store.WorkAction) error {
	if action == nil {
		return store.ValidationErrorf("work action is required")
	}
	if action.ID == "" || action.MonitorNamespace == "" || action.MonitorName == "" || action.Status == "" {
		return store.ValidationErrorf("work action id, monitor namespace, monitor name, and status are required")
	}
	now := time.Now()
	if action.CreatedAt.IsZero() {
		action.CreatedAt = now
	}
	action.UpdatedAt = now
	if action.MetadataJSON == "" {
		action.MetadataJSON = "{}"
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO work_actions
		 (id, monitor_namespace, monitor_name, run_id, command_event_id, monitor_generation,
		  target_kind, target_number, target_sha, target_snapshot_digest, intent, desired_action,
		  depends_on_action_id, dedupe_key, idempotency_key, status, phase, attempt,
		  lease_owner, lease_expires_at, task_name, blocked_reason, error, artifact_ids,
		  payload_digest, metadata_json, created_at, updated_at, completed_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		action.ID, action.MonitorNamespace, action.MonitorName, action.RunID, action.CommandEventID,
		action.MonitorGeneration, action.TargetKind, action.TargetNumber, action.TargetSHA,
		action.TargetSnapshotDigest, action.Intent, action.DesiredAction, action.DependsOnActionID,
		action.DedupeKey, action.IdempotencyKey, action.Status, action.Phase, action.Attempt,
		action.LeaseOwner, action.LeaseExpiresAt, action.TaskName, action.BlockedReason, action.Error,
		action.ArtifactIDs, action.PayloadDigest, action.MetadataJSON, action.CreatedAt, action.UpdatedAt,
		action.CompletedAt,
	)
	return err
}

// UpdateWorkAction updates durable workflow action state.
func (s *Store) UpdateWorkAction(ctx context.Context, action *store.WorkAction) error {
	if action == nil {
		return store.ValidationErrorf("work action is required")
	}
	action.UpdatedAt = time.Now()
	if action.MetadataJSON == "" {
		action.MetadataJSON = "{}"
	}
	result, err := s.db.ExecContext(ctx,
		`UPDATE work_actions SET run_id = ?, command_event_id = ?, monitor_generation = ?,
		 target_kind = ?, target_number = ?, target_sha = ?, target_snapshot_digest = ?, intent = ?,
		 desired_action = ?, depends_on_action_id = ?, dedupe_key = ?, idempotency_key = ?, status = ?,
		 phase = ?, attempt = ?, lease_owner = ?, lease_expires_at = ?, task_name = ?, blocked_reason = ?,
		 error = ?, artifact_ids = ?, payload_digest = ?, metadata_json = ?, updated_at = ?, completed_at = ?
		 WHERE monitor_namespace = ? AND id = ?`,
		action.RunID, action.CommandEventID, action.MonitorGeneration, action.TargetKind, action.TargetNumber,
		action.TargetSHA, action.TargetSnapshotDigest, action.Intent, action.DesiredAction,
		action.DependsOnActionID, action.DedupeKey, action.IdempotencyKey, action.Status, action.Phase,
		action.Attempt, action.LeaseOwner, action.LeaseExpiresAt, action.TaskName, action.BlockedReason,
		action.Error, action.ArtifactIDs, action.PayloadDigest, action.MetadataJSON, action.UpdatedAt,
		action.CompletedAt, action.MonitorNamespace, action.ID,
	)
	if err != nil {
		return err
	}
	if rows, err := result.RowsAffected(); err == nil && rows == 0 {
		return store.ErrNotFound
	}
	return nil
}

func workActionSelectSQL() string {
	return `SELECT id, monitor_namespace, monitor_name, run_id, command_event_id, monitor_generation,
	        target_kind, target_number, target_sha, target_snapshot_digest, intent, desired_action,
	        depends_on_action_id, dedupe_key, idempotency_key, status, phase, attempt, lease_owner,
	        lease_expires_at, task_name, blocked_reason, error, artifact_ids, payload_digest,
	        metadata_json, created_at, updated_at, completed_at FROM work_actions`
}

func scanWorkAction(r rowScanner) (store.WorkAction, error) {
	var action store.WorkAction
	var leaseExpiresAt, completedAt sql.NullTime
	if err := r.Scan(
		&action.ID, &action.MonitorNamespace, &action.MonitorName, &action.RunID, &action.CommandEventID,
		&action.MonitorGeneration, &action.TargetKind, &action.TargetNumber, &action.TargetSHA,
		&action.TargetSnapshotDigest, &action.Intent, &action.DesiredAction, &action.DependsOnActionID,
		&action.DedupeKey, &action.IdempotencyKey, &action.Status, &action.Phase, &action.Attempt,
		&action.LeaseOwner, &leaseExpiresAt, &action.TaskName, &action.BlockedReason, &action.Error,
		&action.ArtifactIDs, &action.PayloadDigest, &action.MetadataJSON, &action.CreatedAt,
		&action.UpdatedAt, &completedAt,
	); err != nil {
		return store.WorkAction{}, err
	}
	if leaseExpiresAt.Valid {
		action.LeaseExpiresAt = &leaseExpiresAt.Time
	}
	if completedAt.Valid {
		action.CompletedAt = &completedAt.Time
	}
	return action, nil
}

// GetWorkAction fetches one workflow action by ID.
func (s *Store) GetWorkAction(ctx context.Context, namespace, id string) (*store.WorkAction, error) {
	return queryOne(ctx, s.db, scanWorkAction, workActionSelectSQL()+` WHERE monitor_namespace = ? AND id = ?`, namespace, id)
}

// ListWorkActions lists workflow actions ordered by update time.
func (s *Store) ListWorkActions(ctx context.Context, filter store.WorkActionFilter) ([]store.WorkAction, string, error) {
	q := newMonitorQuery(workActionSelectSQL(), filter.Namespace)
	monitorFilter(q, "monitor_name", filter.MonitorName)
	monitorFilter(q, "target_kind", filter.TargetKind)
	monitorFilter(q, "target_number", filter.TargetNumber)
	monitorFilter(q, "target_sha", filter.TargetSHA)
	monitorFilter(q, "intent", filter.Intent)
	monitorFilter(q, "desired_action", filter.DesiredAction)
	monitorFilter(q, "status", filter.Status)
	monitorFilter(q, "run_id", filter.RunID)
	monitorFilter(q, "command_event_id", filter.CommandEventID)
	monitorFilter(q, "task_name", filter.TaskName)
	monitorFilter(q, "dedupe_key", filter.DedupeKey)
	return queryPage(ctx, s.db, q, "updated_at DESC, id DESC", filter.Cursor, filter.Limit, scanWorkAction)
}

// CancelWorkActions cancels non-terminal workflow actions for a target.
func (s *Store) CancelWorkActions(ctx context.Context, namespace, monitorName, targetKind string, targetNumber int64, reason string) (int, error) {
	now := time.Now()
	result, err := s.db.ExecContext(ctx,
		`UPDATE work_actions SET status = 'cancelled', blocked_reason = ?, error = '', completed_at = ?, updated_at = ?
		 WHERE monitor_namespace = ? AND monitor_name = ? AND target_kind = ? AND target_number = ?
		 AND status IN ('queued', 'leased', 'running', 'retry_pending')`, reason, now, now, namespace, monitorName, targetKind, targetNumber)
	if err != nil {
		return 0, err
	}
	rows, _ := result.RowsAffected()
	return int(rows), nil
}

// CreateImplementationJob inserts an implementation job.
func (s *Store) CreateImplementationJob(ctx context.Context, job *store.ImplementationJob) error {
	if job == nil {
		return store.ValidationErrorf("implementation job is required")
	}
	if job.ID == "" || job.MonitorNamespace == "" || job.MonitorName == "" {
		return store.ValidationErrorf("implementation job id, monitor namespace, and monitor name are required")
	}
	now := time.Now()
	if job.CreatedAt.IsZero() {
		job.CreatedAt = now
	}
	job.UpdatedAt = now
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO implementation_jobs
		 (id, monitor_namespace, monitor_name, repo, issue_number, plan_id, snapshot_digest, phase,
		  attempt, branch, patch_artifact_id, pr_number, validation_state, task_name, mutation_task_name,
		  command_event_id, work_action_id, monitor_generation, error, created_at, updated_at, completed_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		job.ID, job.MonitorNamespace, job.MonitorName, job.Repo, job.IssueNumber, job.PlanID,
		job.SnapshotDigest, job.Phase, job.Attempt, job.Branch, job.PatchArtifactID, job.PRNumber,
		job.ValidationState, job.TaskName, job.MutationTaskName, job.CommandEventID, job.WorkActionID,
		job.MonitorGeneration, job.Error, job.CreatedAt, job.UpdatedAt, job.CompletedAt,
	)
	return err
}

// UpdateImplementationJob updates implementation job state.
func (s *Store) UpdateImplementationJob(ctx context.Context, job *store.ImplementationJob) error {
	if job == nil {
		return store.ValidationErrorf("implementation job is required")
	}
	job.UpdatedAt = time.Now()
	result, err := s.db.ExecContext(ctx,
		`UPDATE implementation_jobs SET repo = ?, issue_number = ?, plan_id = ?, snapshot_digest = ?, phase = ?,
		 attempt = ?, branch = ?, patch_artifact_id = ?, pr_number = ?, validation_state = ?, task_name = ?,
		 mutation_task_name = ?, command_event_id = ?, work_action_id = ?, monitor_generation = ?, error = ?,
		 updated_at = ?, completed_at = ? WHERE monitor_namespace = ? AND id = ?`,
		job.Repo, job.IssueNumber, job.PlanID, job.SnapshotDigest, job.Phase, job.Attempt, job.Branch,
		job.PatchArtifactID, job.PRNumber, job.ValidationState, job.TaskName, job.MutationTaskName,
		job.CommandEventID, job.WorkActionID, job.MonitorGeneration, job.Error, job.UpdatedAt,
		job.CompletedAt, job.MonitorNamespace, job.ID,
	)
	if err != nil {
		return err
	}
	if rows, err := result.RowsAffected(); err == nil && rows == 0 {
		return store.ErrNotFound
	}
	return nil
}

func implementationJobSelectSQL() string {
	return `SELECT id, monitor_namespace, monitor_name, repo, issue_number, plan_id, snapshot_digest,
	        phase, attempt, branch, patch_artifact_id, pr_number, validation_state, task_name,
	        mutation_task_name, command_event_id, work_action_id, monitor_generation, error,
	        created_at, updated_at, completed_at FROM implementation_jobs`
}

func scanImplementationJob(r rowScanner) (store.ImplementationJob, error) {
	var job store.ImplementationJob
	var completedAt sql.NullTime
	if err := r.Scan(&job.ID, &job.MonitorNamespace, &job.MonitorName, &job.Repo, &job.IssueNumber,
		&job.PlanID, &job.SnapshotDigest, &job.Phase, &job.Attempt, &job.Branch, &job.PatchArtifactID,
		&job.PRNumber, &job.ValidationState, &job.TaskName, &job.MutationTaskName, &job.CommandEventID,
		&job.WorkActionID, &job.MonitorGeneration, &job.Error, &job.CreatedAt, &job.UpdatedAt, &completedAt,
	); err != nil {
		return store.ImplementationJob{}, err
	}
	if completedAt.Valid {
		job.CompletedAt = &completedAt.Time
	}
	return job, nil
}

// GetImplementationJob fetches one implementation job.
func (s *Store) GetImplementationJob(ctx context.Context, namespace, id string) (*store.ImplementationJob, error) {
	return queryOne(ctx, s.db, scanImplementationJob, implementationJobSelectSQL()+` WHERE monitor_namespace = ? AND id = ?`, namespace, id)
}

// ListImplementationJobs lists implementation jobs ordered by update time.
func (s *Store) ListImplementationJobs(ctx context.Context, filter store.ImplementationJobFilter) ([]store.ImplementationJob, string, error) {
	q := newMonitorQuery(implementationJobSelectSQL(), filter.Namespace)
	monitorFilter(q, "monitor_name", filter.MonitorName)
	monitorFilter(q, "repo", filter.Repo)
	monitorFilter(q, "issue_number", filter.IssueNumber)
	monitorFilter(q, "phase", filter.Phase)
	monitorFilter(q, "task_name", filter.TaskName)
	return queryPage(ctx, s.db, q, "updated_at DESC, id DESC", filter.Cursor, filter.Limit, scanImplementationJob)
}

// CountImplementationJobs counts jobs matching durable workflow filters without
// materializing historical rows. When ExcludeWorkActionStatuses is set, jobs
// with a linked work action in one of those statuses are excluded; jobs without
// a linked/found action remain counted, matching controller capacity semantics.
func (s *Store) CountImplementationJobs(ctx context.Context, filter store.ImplementationJobFilter) (int, error) {
	query := strings.Builder{}
	query.WriteString("SELECT COUNT(*) FROM implementation_jobs j")
	if len(filter.ExcludeWorkActionStatuses) > 0 {
		query.WriteString(" LEFT JOIN work_actions a ON a.monitor_namespace = j.monitor_namespace AND a.id = j.work_action_id")
	}
	query.WriteString(" WHERE j.monitor_namespace = ?")
	args := []any{filter.Namespace}
	if filter.MonitorName != "" {
		query.WriteString(" AND j.monitor_name = ?")
		args = append(args, filter.MonitorName)
	}
	if filter.Repo != "" {
		query.WriteString(" AND j.repo = ?")
		args = append(args, filter.Repo)
	}
	if filter.IssueNumber != 0 {
		query.WriteString(" AND j.issue_number = ?")
		args = append(args, filter.IssueNumber)
	}
	if filter.Phase != "" {
		query.WriteString(" AND j.phase = ?")
		args = append(args, filter.Phase)
	}
	if len(filter.Phases) > 0 {
		query.WriteString(" AND j.phase IN (")
		query.WriteString(strings.TrimSuffix(strings.Repeat("?,", len(filter.Phases)), ","))
		query.WriteString(")")
		for _, phase := range filter.Phases {
			args = append(args, phase)
		}
	}
	if filter.TaskName != "" {
		query.WriteString(" AND j.task_name = ?")
		args = append(args, filter.TaskName)
	}
	if len(filter.ExcludeWorkActionStatuses) > 0 {
		query.WriteString(" AND (j.work_action_id = '' OR a.id IS NULL OR a.status NOT IN (")
		query.WriteString(strings.TrimSuffix(strings.Repeat("?,", len(filter.ExcludeWorkActionStatuses)), ","))
		query.WriteString("))")
		for _, status := range filter.ExcludeWorkActionStatuses {
			args = append(args, status)
		}
	}
	var count int
	if err := s.db.QueryRowContext(ctx, query.String(), args...).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

// CreateGitHubMutationRecord inserts an immutable GitHub mutation audit record.
func (s *Store) CreateGitHubMutationRecord(ctx context.Context, record *store.GitHubMutationRecord) error {
	if record == nil {
		return store.ValidationErrorf("github mutation record is required")
	}
	if record.ID == "" || record.MonitorNamespace == "" || record.MonitorName == "" || record.Operation == "" {
		return store.ValidationErrorf("github mutation record id, monitor namespace, monitor name, and operation are required")
	}
	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO github_mutation_records
		 (id, monitor_namespace, monitor_name, run_id, command_event_id, work_action_id, monitor_generation,
		  operation, target_kind, target_number, target_sha, actor, reason, request_digest, github_url,
		  github_request_id, external_id, status, error, pending_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		record.ID, record.MonitorNamespace, record.MonitorName, record.RunID, record.CommandEventID,
		record.WorkActionID, record.MonitorGeneration, record.Operation, record.TargetKind,
		record.TargetNumber, record.TargetSHA, record.Actor, record.Reason, record.RequestDigest,
		record.GitHubURL, record.GitHubRequestID, record.ExternalID, record.Status, record.Error,
		record.PendingAt, record.CreatedAt,
	)
	return err
}

// UpdateGitHubMutationRecord updates the durable outcome of one mutation attempt.
func (s *Store) UpdateGitHubMutationRecord(ctx context.Context, record *store.GitHubMutationRecord) error {
	if record == nil {
		return store.ValidationErrorf("github mutation record is required")
	}
	if record.ID == "" || record.MonitorNamespace == "" || record.MonitorName == "" || record.Operation == "" {
		return store.ValidationErrorf("github mutation record id, monitor namespace, monitor name, and operation are required")
	}
	result, err := s.db.ExecContext(ctx,
		`UPDATE github_mutation_records
		 SET monitor_name = ?, run_id = ?, command_event_id = ?, work_action_id = ?, monitor_generation = ?,
		     operation = ?, target_kind = ?, target_number = ?, target_sha = ?, actor = ?, reason = ?,
		     request_digest = ?, github_url = ?, github_request_id = ?, external_id = ?, status = ?, error = ?,
		     pending_at = ?
		 WHERE monitor_namespace = ? AND id = ?`,
		record.MonitorName, record.RunID, record.CommandEventID, record.WorkActionID, record.MonitorGeneration,
		record.Operation, record.TargetKind, record.TargetNumber, record.TargetSHA, record.Actor, record.Reason,
		record.RequestDigest, record.GitHubURL, record.GitHubRequestID, record.ExternalID, record.Status, record.Error,
		record.PendingAt,
		record.MonitorNamespace, record.ID,
	)
	if err != nil {
		return err
	}
	if rows, err := result.RowsAffected(); err == nil && rows == 0 {
		return store.ErrNotFound
	}
	return nil
}

func githubMutationRecordSelectSQL() string {
	return `SELECT id, monitor_namespace, monitor_name, run_id, command_event_id, work_action_id,
	        monitor_generation, operation, target_kind, target_number, target_sha, actor, reason,
	        request_digest, github_url, github_request_id, external_id, status, error, pending_at, created_at
	        FROM github_mutation_records`
}

func scanGitHubMutationRecord(r rowScanner) (store.GitHubMutationRecord, error) {
	var record store.GitHubMutationRecord
	var pendingAt sql.NullTime
	if err := r.Scan(&record.ID, &record.MonitorNamespace, &record.MonitorName, &record.RunID,
		&record.CommandEventID, &record.WorkActionID, &record.MonitorGeneration, &record.Operation,
		&record.TargetKind, &record.TargetNumber, &record.TargetSHA, &record.Actor, &record.Reason,
		&record.RequestDigest, &record.GitHubURL, &record.GitHubRequestID, &record.ExternalID,
		&record.Status, &record.Error, &pendingAt, &record.CreatedAt,
	); err != nil {
		return store.GitHubMutationRecord{}, err
	}
	if pendingAt.Valid {
		value := pendingAt.Time
		record.PendingAt = &value
	}
	return record, nil
}

// GetGitHubMutationRecord fetches one mutation record.
func (s *Store) GetGitHubMutationRecord(ctx context.Context, namespace, id string) (*store.GitHubMutationRecord, error) {
	return queryOne(ctx, s.db, scanGitHubMutationRecord, githubMutationRecordSelectSQL()+` WHERE monitor_namespace = ? AND id = ?`, namespace, id)
}

// ListGitHubMutationRecords lists mutation records ordered newest first.
func (s *Store) ListGitHubMutationRecords(ctx context.Context, filter store.GitHubMutationRecordFilter) ([]store.GitHubMutationRecord, string, error) {
	q := newMonitorQuery(githubMutationRecordSelectSQL(), filter.Namespace)
	monitorFilter(q, "monitor_name", filter.MonitorName)
	monitorFilter(q, "operation", filter.Operation)
	monitorFilter(q, "target_kind", filter.TargetKind)
	monitorFilter(q, "target_number", filter.TargetNumber)
	monitorFilter(q, "status", filter.Status)
	return queryPage(ctx, s.db, q, "created_at DESC, id DESC", filter.Cursor, filter.Limit, scanGitHubMutationRecord)
}
