package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/store"
)

const selectMemoryConsolidationRunSQL = `SELECT namespace, task_uid, task_name, mode, status, window_start, window_end,
	input, report, created_at, completed_at FROM memory_consolidation_runs`

// memoryConsolidationRunKey is all a BeginMemoryConsolidationRun receipt
// keeps. The run itself holds the transcripts it read until it finishes, and
// a receipt outlives that, so a replay rereads the run as it is now.
type memoryConsolidationRunKey struct {
	Namespace string `json:"namespace"`
	TaskUID   string `json:"taskUID"`
}

// BeginMemoryConsolidationRun records a run unless its Task already began one,
// and writes the stored run back either way. It checks a Task data fence in
// its context.
func (s *Store) BeginMemoryConsolidationRun(ctx context.Context, run *store.MemoryConsolidationRun) error {
	if run == nil {
		return store.ValidationErrorf("memory consolidation run is required")
	}
	key := memoryConsolidationRunKey{Namespace: run.Namespace, TaskUID: run.TaskUID}
	_, err := writeOperationRereading(ctx, s, "BeginMemoryConsolidationRun",
		func(ctx context.Context) (memoryConsolidationRunKey, error) {
			return key, s.withTaskDataFence(ctx, func(ctx context.Context) error {
				return s.beginMemoryConsolidationRunWrite(ctx, run)
			})
		},
		func(ctx context.Context, key memoryConsolidationRunKey) (memoryConsolidationRunKey, error) {
			stored, err := s.readMemoryConsolidationRun(ctx, s.taskDataExecutor(ctx), key.Namespace, key.TaskUID)
			if err != nil {
				return key, err
			}
			*run = *stored
			return key, nil
		})
	return err
}

func (s *Store) beginMemoryConsolidationRunWrite(ctx context.Context, run *store.MemoryConsolidationRun) error {
	if run == nil {
		return store.ValidationErrorf("memory consolidation run is required")
	}
	switch {
	case strings.TrimSpace(run.Namespace) == "" || strings.TrimSpace(run.TaskUID) == "" || strings.TrimSpace(run.TaskName) == "" ||
		strings.TrimSpace(run.Mode) == "":
		return store.ValidationErrorf("a memory consolidation run needs a namespace, Task UID, Task name and mode")
	case run.Status != store.MemoryConsolidationRunning:
		return store.ValidationErrorf("a memory consolidation run begins %q, not %q", store.MemoryConsolidationRunning, run.Status)
	case run.WindowStart.IsZero() || run.WindowEnd.Before(run.WindowStart):
		return store.ValidationErrorf("a memory consolidation run's window must not end before it starts")
	case run.CreatedAt.IsZero():
		return store.ValidationErrorf("createdAt is required")
	case len(run.Input) > store.MaxMemoryConsolidationInputBytes:
		return store.ValidationErrorf("a memory consolidation run's input is at most %d bytes", store.MaxMemoryConsolidationInputBytes)
	}
	executor, err := s.beginTx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = executor.Rollback() }()
	existing, err := s.readMemoryConsolidationRun(ctx, executor, run.Namespace, run.TaskUID)
	switch {
	case err == nil:
		*run = *existing
		return executor.Commit()
	case !errors.Is(err, store.ErrNotFound):
		return err
	}
	leaseStart := run.CreatedAt.UTC().Add(-store.MemoryConsolidationRunLease)
	var other string
	err = executor.QueryRowContext(ctx,
		`SELECT task_uid FROM memory_consolidation_runs
		 WHERE namespace = ? AND status = ? AND created_at > ? LIMIT 1`,
		run.Namespace, store.MemoryConsolidationRunning, leaseStart).Scan(&other)
	switch {
	case err == nil:
		return store.ConflictErrorf("memory consolidation run %q is still running in namespace %q", other, run.Namespace)
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	// The window was chosen from the mode's cursor before this transaction.
	// A run in the mode that completed since then read past its start, so
	// beginning would read those Sessions again.
	err = executor.QueryRowContext(ctx,
		`SELECT task_uid FROM memory_consolidation_runs
		 WHERE namespace = ? AND mode = ? AND status = ? AND window_end > ? LIMIT 1`,
		run.Namespace, run.Mode, store.MemoryConsolidationCompleted, run.WindowStart.UTC()).Scan(&other)
	switch {
	case err == nil:
		return store.PreconditionFailedErrorf("memory consolidation run %q completed past this run's window start", other)
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	// A running run whose lease ended is taken over: marking it abandoned in
	// this transaction means its worker can neither resume it nor complete
	// it, so the two never propose at once.
	if _, err := executor.ExecContext(ctx,
		`UPDATE memory_consolidation_runs SET status = ?, input = NULL
		 WHERE namespace = ? AND status = ? AND created_at <= ?`,
		store.MemoryConsolidationAbandoned, run.Namespace, store.MemoryConsolidationRunning, leaseStart); err != nil {
		return err
	}
	if _, err := executor.ExecContext(ctx,
		`INSERT INTO memory_consolidation_runs
		 (namespace, task_uid, task_name, mode, status, window_start, window_end, input, report, created_at, completed_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL, ?, NULL)`,
		run.Namespace, run.TaskUID, run.TaskName, run.Mode, run.Status, run.WindowStart.UTC(), run.WindowEnd.UTC(), run.Input,
		run.CreatedAt.UTC(),
	); err != nil {
		return uniqueConflict(err)
	}
	stored, err := s.readMemoryConsolidationRun(ctx, executor, run.Namespace, run.TaskUID)
	if err != nil {
		return err
	}
	if err := executor.Commit(); err != nil {
		return err
	}
	*run = *stored
	return nil
}

// CompleteMemoryConsolidationRun finishes a running run and creates its
// proposals in one transaction. A finished run is returned unchanged. It
// checks a Task data fence in its context.
func (s *Store) CompleteMemoryConsolidationRun(ctx context.Context, completion store.MemoryConsolidationCompletion) (*store.MemoryConsolidationRun, error) {
	return writeOperation(ctx, s, "CompleteMemoryConsolidationRun", nil, func(ctx context.Context) (*store.MemoryConsolidationRun, error) {
		var completed *store.MemoryConsolidationRun
		err := s.withTaskDataFence(ctx, func(ctx context.Context) error {
			var err error
			completed, err = s.completeMemoryConsolidationRunWrite(ctx, completion)
			return err
		})
		return completed, err
	})
}

func (s *Store) completeMemoryConsolidationRunWrite(ctx context.Context, completion store.MemoryConsolidationCompletion) (*store.MemoryConsolidationRun, error) {
	switch {
	case strings.TrimSpace(completion.Namespace) == "" || strings.TrimSpace(completion.TaskUID) == "":
		return nil, store.ValidationErrorf("namespace and Task UID are required")
	case completion.Status != store.MemoryConsolidationCompleted && completion.Status != store.MemoryConsolidationSkipped:
		return nil, store.ValidationErrorf("a memory consolidation run completes or is skipped, not %q", completion.Status)
	case len(completion.Report) == 0:
		return nil, store.ValidationErrorf("a finished memory consolidation run needs its report")
	case len(completion.Report) > store.MaxMemoryConsolidationReportBytes:
		return nil, store.ValidationErrorf("a memory consolidation report is at most %d bytes", store.MaxMemoryConsolidationReportBytes)
	case len(completion.Proposals) > store.MaxMemoryConsolidationProposals:
		return nil, store.ValidationErrorf("a memory consolidation run creates at most %d proposals", store.MaxMemoryConsolidationProposals)
	case completion.Status == store.MemoryConsolidationSkipped && len(completion.Proposals) > 0:
		return nil, store.ValidationErrorf("a skipped memory consolidation run creates no proposals")
	case completion.CompletedAt.IsZero():
		return nil, store.ValidationErrorf("completedAt is required")
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	run, err := s.readMemoryConsolidationRun(ctx, tx, completion.Namespace, completion.TaskUID)
	if err != nil {
		return nil, err
	}
	if run.Status != store.MemoryConsolidationRunning {
		return run, tx.Commit()
	}
	// The proposals are created in this transaction, as CreateMemoryProposal
	// creates one, so an invalid proposal leaves the run running.
	txCtx := context.WithValue(ctx, taskDataTransactionKey{}, taskDataTransaction{db: s.db, tx: tx.Tx})
	for i := range completion.Proposals {
		proposal := completion.Proposals[i]
		if proposal.Namespace != completion.Namespace {
			return nil, store.ValidationErrorf("a memory consolidation run's proposals are in its namespace")
		}
		if err := s.createMemoryProposalWrite(txCtx, &proposal); err != nil {
			return nil, err
		}
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE memory_consolidation_runs SET status = ?, report = ?, completed_at = ?, input = NULL
		 WHERE namespace = ? AND task_uid = ? AND status = ?`,
		completion.Status, completion.Report, completion.CompletedAt.UTC(), completion.Namespace, completion.TaskUID,
		store.MemoryConsolidationRunning)
	if err != nil {
		return nil, err
	}
	if rows, err := res.RowsAffected(); err != nil {
		return nil, err
	} else if rows != 1 {
		return nil, store.ConflictErrorf("memory consolidation run %q changed while it completed", completion.TaskUID)
	}
	completed, err := s.readMemoryConsolidationRun(ctx, tx, completion.Namespace, completion.TaskUID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return completed, nil
}

// ReleaseMemoryConsolidationRun releases the run of a Task that ended or is
// being deleted: it clears the run's input and report, removes the run when
// no cursor needs it, and removes earlier released runs a later completed run
// supersedes.
func (s *Store) ReleaseMemoryConsolidationRun(ctx context.Context, namespace, taskUID string) error {
	return writeOperationErr(ctx, s, "ReleaseMemoryConsolidationRun", func(ctx context.Context) error {
		if strings.TrimSpace(namespace) == "" || strings.TrimSpace(taskUID) == "" {
			return store.ValidationErrorf("namespace and Task UID are required")
		}
		tx, err := s.beginTx(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if err := releaseMemoryConsolidationRun(ctx, tx, namespace, taskUID); err != nil {
			return err
		}
		return tx.Commit()
	})
}

// A mode's cursor is its latest completed run's window end or, before any
// completed, its earliest run's window start (GetMemoryConsolidationCursor).
// A run is kept while it could be that run: ties go to the greatest Task UID
// among completed runs and the least among the others.
const (
	supersededCompletedRunSQL = `EXISTS (SELECT 1 FROM memory_consolidation_runs c
		WHERE c.namespace = r.namespace AND c.mode = r.mode AND c.status = 'completed' AND c.task_uid <> r.task_uid
		AND (c.window_end > r.window_end OR (c.window_end = r.window_end AND c.task_uid > r.task_uid)))`
	supersededOtherRunSQL = `(EXISTS (SELECT 1 FROM memory_consolidation_runs c
		WHERE c.namespace = r.namespace AND c.mode = r.mode AND c.status = 'completed')
		OR EXISTS (SELECT 1 FROM memory_consolidation_runs n
		WHERE n.namespace = r.namespace AND n.mode = r.mode AND n.status <> 'completed' AND n.task_uid <> r.task_uid
		AND (n.window_start < r.window_start OR (n.window_start = r.window_start AND n.task_uid < r.task_uid))))`
)

func releaseMemoryConsolidationRun(ctx context.Context, tx *writeTx, namespace, taskUID string) error {
	var mode string
	err := tx.QueryRowContext(ctx, `SELECT mode FROM memory_consolidation_runs WHERE namespace = ? AND task_uid = ?`,
		namespace, taskUID).Scan(&mode)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE memory_consolidation_runs
		 SET status = CASE WHEN status = ? THEN ? ELSE status END, input = NULL, report = NULL
		 WHERE namespace = ? AND task_uid = ?`,
		store.MemoryConsolidationRunning, store.MemoryConsolidationAbandoned, namespace, taskUID); err != nil {
		return err
	}
	// This run goes when no cursor needs it.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM memory_consolidation_runs AS r WHERE r.namespace = ? AND r.task_uid = ? AND (
		   (r.status = 'completed' AND `+supersededCompletedRunSQL+`) OR
		   (r.status <> 'completed' AND `+supersededOtherRunSQL+`))`,
		namespace, taskUID); err != nil {
		return err
	}
	// An earlier released completed run kept as its mode's cursor goes once
	// a later completed run supersedes it. A completed run that still has its
	// report was not released, so its Task may still read it.
	_, err = tx.ExecContext(ctx,
		`DELETE FROM memory_consolidation_runs AS r
		 WHERE r.namespace = ? AND r.mode = ? AND r.status = 'completed' AND r.report IS NULL AND `+supersededCompletedRunSQL,
		namespace, mode)
	return err
}

// GetMemoryConsolidationRun returns the run of a namespace's Task. It checks
// a Task data fence in its context.
func (s *Store) GetMemoryConsolidationRun(ctx context.Context, namespace, taskUID string) (*store.MemoryConsolidationRun, error) {
	var run *store.MemoryConsolidationRun
	err := s.withTaskDataFence(ctx, func(ctx context.Context) error {
		var err error
		run, err = s.readMemoryConsolidationRun(ctx, s.taskDataExecutor(ctx), namespace, taskUID)
		return err
	})
	return run, err
}

// GetMemoryConsolidationCursor returns the namespace's cursor: the window
// end of its latest completed run.
func (s *Store) GetMemoryConsolidationCursor(ctx context.Context, namespace, mode string) (*time.Time, error) {
	if strings.TrimSpace(namespace) == "" || strings.TrimSpace(mode) == "" {
		return nil, store.ValidationErrorf("namespace and mode are required")
	}
	executor := s.taskDataExecutor(ctx)
	for _, query := range []string{
		// The latest completed run in the mode ends the window it read.
		`SELECT window_end FROM memory_consolidation_runs WHERE namespace = ? AND mode = ? AND status = ?
		 ORDER BY window_end DESC LIMIT 1`,
		// Before any completed, the first run's start holds.
		`SELECT window_start FROM memory_consolidation_runs WHERE namespace = ? AND mode = ? AND status <> ?
		 ORDER BY window_start ASC LIMIT 1`,
	} {
		var at time.Time
		err := executor.QueryRowContext(ctx, query, namespace, mode, store.MemoryConsolidationCompleted).Scan(&at)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		cursor := at.UTC()
		return &cursor, nil
	}
	return nil, nil
}

type memoryConsolidationQueryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func (s *Store) readMemoryConsolidationRun(ctx context.Context, q memoryConsolidationQueryer, namespace, taskUID string) (*store.MemoryConsolidationRun, error) {
	var run store.MemoryConsolidationRun
	var completedAt sql.NullTime
	err := q.QueryRowContext(ctx, selectMemoryConsolidationRunSQL+` WHERE namespace = ? AND task_uid = ?`, namespace, taskUID).Scan(
		&run.Namespace, &run.TaskUID, &run.TaskName, &run.Mode, &run.Status, &run.WindowStart, &run.WindowEnd,
		&run.Input, &run.Report, &run.CreatedAt, &completedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	run.WindowStart, run.WindowEnd, run.CreatedAt = run.WindowStart.UTC(), run.WindowEnd.UTC(), run.CreatedAt.UTC()
	if completedAt.Valid {
		completed := completedAt.Time.UTC()
		run.CompletedAt = &completed
	}
	return &run, nil
}

// ListMemoryConsolidationSessions lists the namespace's chat and task
// Sessions changed in (after, through], most recently changed first, and
// counts them all. It checks a Task data fence in its context.
func (s *Store) ListMemoryConsolidationSessions(ctx context.Context, namespace string, after, through time.Time, limit int) ([]store.SessionMetadata, int, error) {
	var sessions []store.SessionMetadata
	var total int
	err := s.withTaskDataFence(ctx, func(ctx context.Context) error {
		var err error
		sessions, total, err = s.listMemoryConsolidationSessionsUnfenced(ctx, namespace, after, through, limit)
		return err
	})
	return sessions, total, err
}

func (s *Store) listMemoryConsolidationSessionsUnfenced(ctx context.Context, namespace string, after, through time.Time, limit int) ([]store.SessionMetadata, int, error) {
	if strings.TrimSpace(namespace) == "" {
		return nil, 0, store.ValidationErrorf("namespace is required")
	}
	if limit < 1 || limit > store.MaxMemoryConsolidationSessions {
		return nil, 0, store.ValidationErrorf("limit must be 1 to %d", store.MaxMemoryConsolidationSessions)
	}
	const changed = ` FROM sessions WHERE namespace = ? AND session_type IN ('task', 'chat') AND updated_at > ? AND updated_at <= ?`
	args := []any{namespace, after.UTC(), through.UTC()}
	var total int
	executor := s.taskDataExecutor(ctx)
	if err := executor.QueryRowContext(ctx, `SELECT COUNT(*)`+changed, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := executor.QueryContext(ctx,
		`SELECT name, session_type, message_count, input_tokens, output_tokens, created_at, updated_at, active_task, active_task_uid`+
			changed+` ORDER BY updated_at DESC, name ASC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close() //nolint:errcheck
	var sessions []store.SessionMetadata
	for rows.Next() {
		var session store.SessionMetadata
		if err := rows.Scan(&session.Name, &session.SessionType, &session.MessageCount, &session.InputTokens, &session.OutputTokens,
			&session.CreatedAt, &session.UpdatedAt, &session.ActiveTask, &session.ActiveTaskUID); err != nil {
			return nil, 0, err
		}
		session.CreatedAt, session.UpdatedAt = session.CreatedAt.UTC(), session.UpdatedAt.UTC()
		sessions = append(sessions, session)
	}
	return sessions, total, rows.Err()
}
