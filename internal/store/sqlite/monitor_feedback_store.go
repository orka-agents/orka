package sqlite

import (
	"context"
	"database/sql"
	"errors"
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
