package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/store"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

// AdmitMonitorFeedbackRepair creates a feedback repair's command event and
// attempts in one transaction.
func (s *Store) AdmitMonitorFeedbackRepair(ctx context.Context, admission store.MonitorFeedbackAdmission) (bool, error) {
	normalized, err := normalizeMonitorFeedbackAdmission(admission)
	if err != nil {
		return false, err
	}
	return writeOperation(ctx, s, "AdmitMonitorFeedbackRepair", nil, func(ctx context.Context) (bool, error) {
		created := false
		err := s.inWriteTransaction(ctx, func(ctx context.Context, tx *sql.Tx) error {
			var err error
			created, err = admitMonitorFeedbackRepair(ctx, s, tx, normalized)
			return err
		})
		return created, err
	})
}

func admitMonitorFeedbackRepair(ctx context.Context, s *Store, tx *sql.Tx, admission store.MonitorFeedbackAdmission) (bool, error) {
	command := admission.Command
	if err := s.createCommandEventWrite(ctx, &command); err != nil {
		if !errors.Is(err, store.ErrConflict) {
			return false, err
		}
		// The same command admitted again with the same attempts is a
		// replay; anything else reuses a command ID or dedupe key.
		// Only the same command, for the same monitor and pull request, can
		// be a replay; any other unique key is a different command.
		var monitorName, kind, headSHA, intent string
		var number int64
		err := tx.QueryRowContext(ctx, `SELECT monitor_name, kind, number, head_sha, intent FROM command_events WHERE monitor_namespace = ? AND id = ?`,
			command.MonitorNamespace, command.ID).Scan(&monitorName, &kind, &number, &headSHA, &intent)
		if errors.Is(err, sql.ErrNoRows) {
			return false, store.ConflictErrorf("feedback repair command %q conflicts with another command event", command.ID)
		}
		if err != nil {
			return false, err
		}
		if monitorName != command.MonitorName || kind != command.Kind || number != command.Number || headSHA != command.HeadSHA || intent != command.Intent {
			return false, store.ConflictErrorf("command event %q already exists for another monitor, pull request, head, or intent", command.ID)
		}
		existing, listErr := listMonitorFeedbackAttemptsForCommand(ctx, tx, command.MonitorNamespace, command.ID)
		if listErr != nil {
			return false, listErr
		}
		if !sameMonitorFeedbackAttempts(existing, admission.Attempts) {
			return false, store.ConflictErrorf("command event %q was already admitted with other feedback attempts", command.ID)
		}
		return false, nil
	}
	admitted := make([]string, 0, len(admission.Attempts))
	for _, attempt := range admission.Attempts {
		admitted = append(admitted, attempt.FeedbackID)
	}
	if err := insertMonitorFeedbackOutcome(ctx, tx, command, admission.Attempts[0], admitted); err != nil {
		return false, err
	}
	for _, attempt := range admission.Attempts {
		var prior int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM monitor_feedback_attempts
			 WHERE monitor_namespace = ? AND monitor_name = ? AND repo = ? AND pr_number = ? AND feedback_id = ?`,
			attempt.MonitorNamespace, attempt.MonitorName, attempt.Repo, attempt.PRNumber, attempt.FeedbackID,
		).Scan(&prior); err != nil {
			return false, err
		}
		if prior != attempt.Attempt-1 {
			return false, store.ConflictErrorf("feedback %q has %d attempts, so attempt %d cannot be admitted", attempt.FeedbackID, prior, attempt.Attempt)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO monitor_feedback_attempts
			 (id, monitor_namespace, monitor_name, repo, pr_number, feedback_id, attempt, head_sha,
			  snapshot_digest, command_event_id, repair_job_id, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			attempt.ID, attempt.MonitorNamespace, attempt.MonitorName, attempt.Repo, attempt.PRNumber, attempt.FeedbackID,
			attempt.Attempt, attempt.HeadSHA, attempt.SnapshotDigest, attempt.CommandEventID, attempt.RepairJobID, attempt.CreatedAt,
		); err != nil {
			return false, uniqueConflict(err)
		}
	}
	return true, nil
}

// normalizeMonitorFeedbackAdmission validates an admission and fills the
// creation times the caller left zero.
func normalizeMonitorFeedbackAdmission(admission store.MonitorFeedbackAdmission) (store.MonitorFeedbackAdmission, error) {
	command := admission.Command
	if strings.TrimSpace(command.ID) == "" || strings.TrimSpace(command.MonitorNamespace) == "" || strings.TrimSpace(command.MonitorName) == "" || command.Number <= 0 {
		return admission, store.ValidationErrorf("feedback repair command needs an ID, monitor, and pull request number")
	}
	if len(admission.Attempts) == 0 || len(admission.Attempts) > storagev1.MaxMonitorFeedbackAttemptsPerAdmission {
		return admission, store.ValidationErrorf("feedback repair needs 1 to %d attempts, not %d", storagev1.MaxMonitorFeedbackAttemptsPerAdmission, len(admission.Attempts))
	}
	if command.CreatedAt.IsZero() {
		command.CreatedAt = time.Now().UTC()
	}
	attempts := slices.Clone(admission.Attempts)
	seen := map[string]bool{}
	for i := range attempts {
		attempt := &attempts[i]
		switch {
		case strings.TrimSpace(attempt.ID) == "" || strings.TrimSpace(attempt.FeedbackID) == "" || strings.TrimSpace(attempt.Repo) == "" ||
			strings.TrimSpace(attempt.HeadSHA) == "" || strings.TrimSpace(attempt.RepairJobID) == "" || attempt.Attempt < 1:
			return admission, store.ValidationErrorf("feedback attempt %d needs an ID, feedback ID, repository, head, repair job, and attempt number", i)
		case attempt.MonitorNamespace != command.MonitorNamespace || attempt.MonitorName != command.MonitorName ||
			attempt.PRNumber != command.Number || attempt.CommandEventID != command.ID:
			return admission, store.ValidationErrorf("feedback attempt %q does not belong to command %q", attempt.ID, command.ID)
		case seen[attempt.FeedbackID]:
			return admission, store.ValidationErrorf("feedback %q is attempted twice by command %q", attempt.FeedbackID, command.ID)
		}
		seen[attempt.FeedbackID] = true
		if attempt.CreatedAt.IsZero() {
			attempt.CreatedAt = command.CreatedAt
		}
	}
	return store.MonitorFeedbackAdmission{Command: command, Attempts: attempts}, nil
}

func sameMonitorFeedbackAttempts(existing, requested []store.MonitorFeedbackAttempt) bool {
	key := func(attempt store.MonitorFeedbackAttempt) string {
		return strings.Join([]string{attempt.ID, attempt.MonitorNamespace, attempt.MonitorName, attempt.Repo, strconv.FormatInt(attempt.PRNumber, 10), attempt.FeedbackID,
			strconv.Itoa(attempt.Attempt), attempt.HeadSHA, attempt.SnapshotDigest, attempt.CommandEventID, attempt.RepairJobID}, "\x00")
	}
	if len(existing) != len(requested) {
		return false
	}
	have := map[string]int{}
	for _, attempt := range existing {
		have[key(attempt)]++
	}
	for _, attempt := range requested {
		if have[key(attempt)] == 0 {
			return false
		}
		have[key(attempt)]--
	}
	return true
}

func listMonitorFeedbackAttemptsForCommand(ctx context.Context, tx *sql.Tx, namespace, commandID string) ([]store.MonitorFeedbackAttempt, error) {
	rows, err := tx.QueryContext(ctx, monitorFeedbackAttemptSelectSQL()+` WHERE monitor_namespace = ? AND command_event_id = ?`, namespace, commandID)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var attempts []store.MonitorFeedbackAttempt
	for rows.Next() {
		attempt, err := scanMonitorFeedbackAttempt(rows)
		if err != nil {
			return nil, err
		}
		attempts = append(attempts, attempt)
	}
	return attempts, rows.Err()
}

// ListMonitorFeedbackAttempts lists feedback attempts newest first.
func (s *Store) ListMonitorFeedbackAttempts(ctx context.Context, filter store.MonitorFeedbackAttemptFilter) ([]store.MonitorFeedbackAttempt, string, error) {
	q := newMonitorQuery(monitorFeedbackAttemptSelectSQL(), filter.Namespace)
	monitorFilter(q, "monitor_name", filter.MonitorName)
	monitorFilter(q, "repo", filter.Repo)
	monitorFilter(q, "pr_number", filter.PRNumber)
	monitorFilter(q, "feedback_id", filter.FeedbackID)
	monitorFilter(q, "command_event_id", filter.CommandEventID)
	return queryPage(ctx, s.db, q, "created_at DESC, id DESC", filter.Cursor, filter.Limit, scanMonitorFeedbackAttempt)
}

func monitorFeedbackAttemptSelectSQL() string {
	return `SELECT id, monitor_namespace, monitor_name, repo, pr_number, feedback_id, attempt, head_sha,
	        snapshot_digest, command_event_id, repair_job_id, created_at FROM monitor_feedback_attempts`
}

func scanMonitorFeedbackAttempt(r rowScanner) (store.MonitorFeedbackAttempt, error) {
	var attempt store.MonitorFeedbackAttempt
	err := r.Scan(&attempt.ID, &attempt.MonitorNamespace, &attempt.MonitorName, &attempt.Repo, &attempt.PRNumber,
		&attempt.FeedbackID, &attempt.Attempt, &attempt.HeadSHA, &attempt.SnapshotDigest, &attempt.CommandEventID,
		&attempt.RepairJobID, &attempt.CreatedAt)
	attempt.CreatedAt = attempt.CreatedAt.UTC()
	return attempt, err
}

// insertMonitorFeedbackOutcome creates the reserved outcome of an admitted
// feedback repair.
func insertMonitorFeedbackOutcome(ctx context.Context, tx *sql.Tx, command store.CommandEvent, attempt store.MonitorFeedbackAttempt, admitted []string) error {
	encoded, err := encodeMonitorFeedbackIDs(admitted)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO monitor_feedback_outcomes
		 (monitor_namespace, command_event_id, monitor_name, repo, pr_number, head_sha, state, admitted_ids, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		command.MonitorNamespace, command.ID, command.MonitorName, attempt.Repo, command.Number, attempt.HeadSHA,
		store.MonitorFeedbackOutcomeReserved, encoded, command.CreatedAt, command.CreatedAt)
	return uniqueConflict(err)
}

// encodeMonitorFeedbackIDs encodes feedback IDs, sorted and without
// duplicates, as a JSON array.
func encodeMonitorFeedbackIDs(ids []string) (string, error) {
	sorted := slices.Clone(ids)
	slices.Sort(sorted)
	sorted = slices.Compact(sorted)
	if sorted == nil {
		sorted = []string{}
	}
	encoded, err := json.Marshal(sorted)
	return string(encoded), err
}

// transitionMonitorFeedbackOutcome runs one outcome transition in a write
// operation: apply reads the current outcome and changes it, and the stored
// outcome is returned.
func (s *Store) transitionMonitorFeedbackOutcome(ctx context.Context, method, namespace, commandEventID string, apply func(context.Context, *sql.Tx, *store.MonitorFeedbackOutcome) error) (*store.MonitorFeedbackOutcome, error) {
	return writeOperation(ctx, s, method, nil, func(ctx context.Context) (*store.MonitorFeedbackOutcome, error) {
		var outcome *store.MonitorFeedbackOutcome
		err := s.inWriteTransaction(ctx, func(ctx context.Context, tx *sql.Tx) error {
			current, err := getMonitorFeedbackOutcome(ctx, tx, namespace, commandEventID)
			if err != nil {
				return err
			}
			if err := apply(ctx, tx, current); err != nil {
				return err
			}
			outcome, err = getMonitorFeedbackOutcome(ctx, tx, namespace, commandEventID)
			return err
		})
		return outcome, err
	})
}

func validateMonitorFeedbackOutcomeKey(namespace, commandEventID string) error {
	if strings.TrimSpace(namespace) == "" || strings.TrimSpace(commandEventID) == "" {
		return store.ValidationErrorf("a feedback repair outcome needs a namespace and command event")
	}
	return nil
}

// PrepareMonitorFeedbackRepairTask moves a reserved feedback outcome to
// creating before its Task create is requested.
func (s *Store) PrepareMonitorFeedbackRepairTask(ctx context.Context, request store.PrepareMonitorFeedbackRepairTaskRequest) (*store.MonitorFeedbackOutcome, error) {
	if err := validateMonitorFeedbackOutcomeKey(request.Namespace, request.CommandEventID); err != nil {
		return nil, err
	}
	if strings.TrimSpace(request.TaskName) == "" {
		return nil, store.ValidationErrorf("preparing feedback repair %q needs a Task name", request.CommandEventID)
	}
	return s.transitionMonitorFeedbackOutcome(ctx, "PrepareMonitorFeedbackRepairTask", request.Namespace, request.CommandEventID, func(ctx context.Context, tx *sql.Tx, current *store.MonitorFeedbackOutcome) error {
		switch {
		case current.State == store.MonitorFeedbackOutcomeReserved:
			now := time.Now().UTC()
			return updateMonitorFeedbackOutcome(ctx, tx, current, `state = ?, task_name = ?, task_create_at = ?, updated_at = ?`,
				store.MonitorFeedbackOutcomeCreating, request.TaskName, now, now)
		case (current.State == store.MonitorFeedbackOutcomeCreating || current.State == store.MonitorFeedbackOutcomeStarted) && current.TaskName == request.TaskName:
			return nil
		}
		return store.ConflictErrorf("feedback repair %q is %s and cannot prepare Task %q", request.CommandEventID, current.State, request.TaskName)
	})
}

// ReleaseMonitorFeedbackRepairTask returns a creating feedback outcome to
// reserved after a definite refusal of its Task create.
func (s *Store) ReleaseMonitorFeedbackRepairTask(ctx context.Context, request store.ReleaseMonitorFeedbackRepairTaskRequest) (*store.MonitorFeedbackOutcome, error) {
	if err := validateMonitorFeedbackOutcomeKey(request.Namespace, request.CommandEventID); err != nil {
		return nil, err
	}
	return s.transitionMonitorFeedbackOutcome(ctx, "ReleaseMonitorFeedbackRepairTask", request.Namespace, request.CommandEventID, func(ctx context.Context, tx *sql.Tx, current *store.MonitorFeedbackOutcome) error {
		switch current.State {
		case store.MonitorFeedbackOutcomeReserved:
			return nil
		case store.MonitorFeedbackOutcomeCreating:
			return updateMonitorFeedbackOutcome(ctx, tx, current, `state = ?, task_name = '', task_create_at = NULL, updated_at = ?`,
				store.MonitorFeedbackOutcomeReserved, time.Now().UTC())
		}
		return store.ConflictErrorf("feedback repair %q is %s and cannot release its Task create", request.CommandEventID, current.State)
	})
}

// StartMonitorFeedbackRepair moves a reserved or creating feedback outcome
// to started.
func (s *Store) StartMonitorFeedbackRepair(ctx context.Context, request store.StartMonitorFeedbackRepairRequest) (*store.MonitorFeedbackOutcome, error) {
	if err := validateMonitorFeedbackOutcomeKey(request.Namespace, request.CommandEventID); err != nil {
		return nil, err
	}
	if strings.TrimSpace(request.TaskName) == "" {
		return nil, store.ValidationErrorf("starting feedback repair %q needs a Task name", request.CommandEventID)
	}
	if len(request.Targets) == 0 || slices.ContainsFunc(request.Targets, func(id string) bool { return strings.TrimSpace(id) == "" }) {
		return nil, store.ValidationErrorf("starting feedback repair %q needs at least one feedback target", request.CommandEventID)
	}
	targets := slices.Clone(request.Targets)
	slices.Sort(targets)
	targets = slices.Compact(targets)
	return s.transitionMonitorFeedbackOutcome(ctx, "StartMonitorFeedbackRepair", request.Namespace, request.CommandEventID, func(ctx context.Context, tx *sql.Tx, current *store.MonitorFeedbackOutcome) error {
		startable := current.State == store.MonitorFeedbackOutcomeReserved ||
			current.State == store.MonitorFeedbackOutcomeCreating && current.TaskName == request.TaskName
		switch {
		case startable:
			for _, target := range targets {
				if !slices.Contains(current.AdmittedIDs, target) {
					return store.ValidationErrorf("feedback %q was not admitted by command %q", target, request.CommandEventID)
				}
			}
			encoded, err := encodeMonitorFeedbackIDs(targets)
			if err != nil {
				return err
			}
			now := time.Now().UTC()
			return updateMonitorFeedbackOutcome(ctx, tx, current, `state = ?, targets = ?, task_name = ?, started_at = ?, updated_at = ?`,
				store.MonitorFeedbackOutcomeStarted, encoded, request.TaskName, now, now)
		case current.StartedAt != nil && slices.Equal(current.Targets, targets) && current.TaskName == request.TaskName:
			// A repeat of the same start.
			return nil
		}
		return store.ConflictErrorf("feedback repair %q is %s and cannot start with these targets", request.CommandEventID, current.State)
	})
}

// FinishMonitorFeedbackRepair moves a feedback outcome to a terminal state.
func (s *Store) FinishMonitorFeedbackRepair(ctx context.Context, request store.FinishMonitorFeedbackRepairRequest) (*store.MonitorFeedbackOutcome, error) {
	if err := validateMonitorFeedbackOutcomeKey(request.Namespace, request.CommandEventID); err != nil {
		return nil, err
	}
	if !store.IsMonitorFeedbackOutcomeTerminal(request.State) {
		return nil, store.ValidationErrorf("feedback outcome state %q is not terminal", request.State)
	}
	return s.transitionMonitorFeedbackOutcome(ctx, "FinishMonitorFeedbackRepair", request.Namespace, request.CommandEventID, func(ctx context.Context, tx *sql.Tx, current *store.MonitorFeedbackOutcome) error {
		if current.State == request.State {
			return nil
		}
		if !monitorFeedbackOutcomeCanFinish(current.State, request.State) {
			return store.ConflictErrorf("feedback repair %q is %s and cannot end as %s", request.CommandEventID, current.State, request.State)
		}
		now := time.Now().UTC()
		return updateMonitorFeedbackOutcome(ctx, tx, current, `state = ?, reason = ?, result_sha = ?, completed_at = ?, updated_at = ?`,
			request.State, request.Reason, request.ResultSHA, now, now)
	})
}

// MarkMonitorFeedbackOutcomeProjected records that a terminal outcome's
// projections record its end.
func (s *Store) MarkMonitorFeedbackOutcomeProjected(ctx context.Context, request store.MarkMonitorFeedbackOutcomeProjectedRequest) (*store.MonitorFeedbackOutcome, error) {
	if err := validateMonitorFeedbackOutcomeKey(request.Namespace, request.CommandEventID); err != nil {
		return nil, err
	}
	return s.transitionMonitorFeedbackOutcome(ctx, "MarkMonitorFeedbackOutcomeProjected", request.Namespace, request.CommandEventID, func(ctx context.Context, tx *sql.Tx, current *store.MonitorFeedbackOutcome) error {
		switch {
		case !store.IsMonitorFeedbackOutcomeTerminal(current.State):
			return store.ConflictErrorf("feedback repair %q is %s and has not ended", request.CommandEventID, current.State)
		case current.ProjectedAt != nil:
			return nil
		}
		now := time.Now().UTC()
		return updateMonitorFeedbackOutcome(ctx, tx, current, `projected_at = ?, updated_at = ?`, now, now)
	})
}

// monitorFeedbackOutcomeCanFinish reports whether an outcome in state from
// may end in the terminal state to.
func monitorFeedbackOutcomeCanFinish(from, to string) bool {
	switch from {
	case store.MonitorFeedbackOutcomeReserved:
		return to == store.MonitorFeedbackOutcomeWithdrawn || to == store.MonitorFeedbackOutcomeFailed
	case store.MonitorFeedbackOutcomeCreating:
		return to == store.MonitorFeedbackOutcomeFailed
	case store.MonitorFeedbackOutcomeStarted:
		return to == store.MonitorFeedbackOutcomeNoChange || to == store.MonitorFeedbackOutcomeFailed || to == store.MonitorFeedbackOutcomeSucceeded
	}
	return false
}

// updateMonitorFeedbackOutcome applies assignments to current, provided it
// is still in the state it was read in.
func updateMonitorFeedbackOutcome(ctx context.Context, tx *sql.Tx, current *store.MonitorFeedbackOutcome, assignments string, values ...any) error {
	args := append(values, current.MonitorNamespace, current.CommandEventID, current.State)
	result, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE monitor_feedback_outcomes SET %s WHERE monitor_namespace = ? AND command_event_id = ? AND state = ?`, assignments), args...)
	if err != nil {
		return err
	}
	if rows, err := result.RowsAffected(); err != nil {
		return err
	} else if rows != 1 {
		return store.ConflictErrorf("feedback repair %q changed concurrently", current.CommandEventID)
	}
	return nil
}

// GetMonitorFeedbackOutcome returns one feedback repair's outcome.
func (s *Store) GetMonitorFeedbackOutcome(ctx context.Context, namespace, commandEventID string) (*store.MonitorFeedbackOutcome, error) {
	return getMonitorFeedbackOutcome(ctx, s.taskDataExecutor(ctx), namespace, commandEventID)
}

type monitorFeedbackQueryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func getMonitorFeedbackOutcome(ctx context.Context, q monitorFeedbackQueryer, namespace, commandEventID string) (*store.MonitorFeedbackOutcome, error) {
	outcome, err := scanMonitorFeedbackOutcome(q.QueryRowContext(ctx, monitorFeedbackOutcomeSelectSQL()+` WHERE monitor_namespace = ? AND command_event_id = ?`, namespace, commandEventID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &outcome, nil
}

// ListMonitorFeedbackOutcomes lists feedback outcomes newest first.
func (s *Store) ListMonitorFeedbackOutcomes(ctx context.Context, filter store.MonitorFeedbackOutcomeFilter) ([]store.MonitorFeedbackOutcome, string, error) {
	q := newMonitorQuery(monitorFeedbackOutcomeSelectSQL(), filter.Namespace)
	monitorFilter(q, "monitor_name", filter.MonitorName)
	monitorFilter(q, "repo", filter.Repo)
	monitorFilter(q, "pr_number", filter.PRNumber)
	monitorFilter(q, "head_sha", filter.HeadSHA)
	monitorFilter(q, "state", filter.State)
	if filter.Unprojected {
		q.sql.WriteString(" AND projected_at IS NULL AND state IN ('withdrawn', 'no_change', 'failed', 'succeeded')")
	}
	return queryPage(ctx, s.db, q, "created_at DESC, command_event_id DESC", filter.Cursor, filter.Limit, scanMonitorFeedbackOutcome)
}

func monitorFeedbackOutcomeSelectSQL() string {
	return `SELECT monitor_namespace, command_event_id, monitor_name, repo, pr_number, head_sha, state, admitted_ids, targets,
	        task_name, reason, result_sha, created_at, updated_at, task_create_at, started_at, completed_at, projected_at
	        FROM monitor_feedback_outcomes`
}

func scanMonitorFeedbackOutcome(r rowScanner) (store.MonitorFeedbackOutcome, error) {
	var outcome store.MonitorFeedbackOutcome
	var admitted, targets string
	var creating, started, completed, projected sql.NullTime
	if err := r.Scan(&outcome.MonitorNamespace, &outcome.CommandEventID, &outcome.MonitorName, &outcome.Repo, &outcome.PRNumber,
		&outcome.HeadSHA, &outcome.State, &admitted, &targets, &outcome.TaskName, &outcome.Reason, &outcome.ResultSHA, &outcome.CreatedAt,
		&outcome.UpdatedAt, &creating, &started, &completed, &projected); err != nil {
		return outcome, err
	}
	if err := json.Unmarshal([]byte(admitted), &outcome.AdmittedIDs); err != nil {
		return outcome, fmt.Errorf("decode feedback repair %q admitted IDs: %w", outcome.CommandEventID, err)
	}
	if err := json.Unmarshal([]byte(targets), &outcome.Targets); err != nil {
		return outcome, fmt.Errorf("decode feedback repair %q targets: %w", outcome.CommandEventID, err)
	}
	if len(outcome.Targets) == 0 {
		outcome.Targets = nil
	}
	outcome.CreatedAt, outcome.UpdatedAt = outcome.CreatedAt.UTC(), outcome.UpdatedAt.UTC()
	outcome.TaskCreateAt = monitorFeedbackOutcomeTime(creating)
	outcome.StartedAt = monitorFeedbackOutcomeTime(started)
	outcome.CompletedAt = monitorFeedbackOutcomeTime(completed)
	outcome.ProjectedAt = monitorFeedbackOutcomeTime(projected)
	return outcome, nil
}

func monitorFeedbackOutcomeTime(value sql.NullTime) *time.Time {
	if !value.Valid {
		return nil
	}
	at := value.Time.UTC()
	return &at
}
