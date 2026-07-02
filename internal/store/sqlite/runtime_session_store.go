/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package sqlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sozercan/orka/internal/harness"
	"github.com/sozercan/orka/internal/store"
)

const (
	defaultRuntimeSessionLimit = 50
	maxRuntimeSessionLimit     = 200
)

var _ harness.RuntimeSessionStore = (*Store)(nil)

var errRuntimeSessionCleanupSkipped = errors.New("runtime session cleanup skipped")

// CreateRuntimeSession persists a new backend-neutral RuntimeSession.
func (s *Store) CreateRuntimeSession(ctx context.Context, session *harness.RuntimeSession) error {
	normalized, err := normalizeRuntimeSessionForCreate(session)
	if err != nil {
		return err
	}
	if err := insertRuntimeSession(ctx, s.db, normalized); err != nil {
		if isSQLiteConstraintError(err) {
			return fmt.Errorf("%w: runtime session %s/%s already exists", store.ErrConflict, normalized.Owner.Namespace, normalized.ID)
		}
		return err
	}
	*session = normalized
	return nil
}

// GetRuntimeSession loads a RuntimeSession by namespace and canonical ID.
func (s *Store) GetRuntimeSession(ctx context.Context, namespace string, id harness.RuntimeSessionID) (*harness.RuntimeSession, error) {
	session, err := getRuntimeSessionTx(ctx, s.db, namespace, id)
	if err != nil {
		return nil, err
	}
	return &session, nil
}

// ListRuntimeSessions returns RuntimeSessions matching filter, ordered by most recent update first.
func (s *Store) ListRuntimeSessions(ctx context.Context, filter harness.RuntimeSessionFilter) ([]harness.RuntimeSession, string, error) {
	filter, limit, offset, err := validateRuntimeSessionFilter(filter)
	if err != nil {
		return nil, "", err
	}

	query := strings.Builder{}
	query.WriteString(runtimeSessionSelectSQL())
	clauses := []string{"namespace = ?"}
	args := []any{filter.Namespace}
	addClause := func(clause string, arg any) {
		clauses = append(clauses, clause)
		args = append(args, arg)
	}
	if filter.SessionName != "" {
		addClause("session_name = ?", filter.SessionName)
	}
	if filter.ActiveTask != "" {
		addClause("active_task = ?", filter.ActiveTask)
	}
	if filter.AgentName != "" {
		addClause("agent_name = ?", filter.AgentName)
	}
	if filter.Provider != "" {
		addClause("provider = ?", string(filter.Provider))
	}
	if len(filter.States) > 0 {
		placeholders := make([]string, 0, len(filter.States))
		for _, state := range filter.States {
			placeholders = append(placeholders, "?")
			args = append(args, string(state))
		}
		clauses = append(clauses, "state IN ("+strings.Join(placeholders, ",")+")")
	} else if !filter.IncludeDeleted {
		addClause("state <> ?", string(harness.RuntimeSessionStateDeleted))
	}
	if len(filter.CleanupPolicies) > 0 {
		placeholders := make([]string, 0, len(filter.CleanupPolicies))
		for _, policy := range filter.CleanupPolicies {
			placeholders = append(placeholders, "?")
			args = append(args, string(policy))
		}
		clauses = append(clauses, "cleanup_policy IN ("+strings.Join(placeholders, ",")+")")
	}
	query.WriteString(" WHERE ")
	query.WriteString(strings.Join(clauses, " AND "))
	query.WriteString(" ORDER BY updated_at DESC, id ASC LIMIT ? OFFSET ?")
	args = append(args, limit, offset)

	rows, err := s.db.QueryContext(ctx, query.String(), args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close() //nolint:errcheck

	sessions := []harness.RuntimeSession{}
	for rows.Next() {
		session, err := scanRuntimeSession(rows)
		if err != nil {
			return nil, "", err
		}
		sessions = append(sessions, session)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	return sessions, nextOffsetCursor(offset, len(sessions), limit), nil
}

// TransitionRuntimeSession validates and persists an optimistic RuntimeSession state transition.
func (s *Store) TransitionRuntimeSession(ctx context.Context, transition harness.RuntimeSessionTransition) (*harness.RuntimeSession, error) {
	transition, err := normalizeRuntimeSessionTransition(transition)
	if err != nil {
		return nil, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	session, err := getRuntimeSessionTx(ctx, tx, transition.Namespace, transition.ID)
	if err != nil {
		return nil, err
	}
	if session.State != transition.From {
		return nil, fmt.Errorf("%w: runtime session %s/%s is %s, expected %s", store.ErrConflict, transition.Namespace, transition.ID, session.State, transition.From)
	}
	if transition.ActiveTask != nil {
		session.Owner.ActiveTask = strings.TrimSpace(*transition.ActiveTask)
	}
	if err := session.Transition(transition.To, transition.UpdatedAt); err != nil {
		return nil, store.ValidationErrorf("invalid runtime session transition: %v", err)
	}
	if err := updateRuntimeSessionTx(ctx, tx, session); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &session, nil
}

// PruneDeletedRuntimeSession physically prunes a RuntimeSession after it reaches Deleted.
func (s *Store) PruneDeletedRuntimeSession(ctx context.Context, namespace string, id harness.RuntimeSessionID) error {
	namespace, id, err := normalizeRuntimeSessionKey(namespace, id)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM runtime_sessions WHERE namespace = ? AND id = ? AND state = ?`,
		namespace,
		string(id),
		string(harness.RuntimeSessionStateDeleted),
	)
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows > 0 {
		return nil
	}
	session, err := getRuntimeSessionTx(ctx, s.db, namespace, id)
	if err != nil {
		return err
	}
	return store.ValidationErrorf("runtime session %s/%s must be %s before physical deletion (current state: %s)", namespace, id, harness.RuntimeSessionStateDeleted, session.State)
}

func (s *Store) ClaimRuntimeSession(
	ctx context.Context,
	namespace, sessionName string,
	provider harness.ProviderKind,
	agentName, activeTask string,
	allowReuse bool,
	now time.Time,
) (*harness.RuntimeSession, error) {
	now = normalizeRuntimeSessionTime(now)
	namespace = strings.TrimSpace(namespace)
	sessionName = strings.TrimSpace(sessionName)
	agentName = strings.TrimSpace(agentName)
	activeTask = strings.TrimSpace(activeTask)
	provider = harness.ProviderKind(strings.TrimSpace(string(provider)))
	if namespace == "" {
		return nil, store.ValidationErrorf("runtime session namespace is required")
	}
	if sessionName == "" {
		return nil, store.ValidationErrorf("runtime session owner session name is required")
	}
	if provider == "" {
		return nil, store.ValidationErrorf("runtime session provider is required")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var existing harness.RuntimeSession
	if allowReuse {
		existing, err = findReusableRuntimeSession(ctx, tx, namespace, sessionName, provider, agentName)
	} else {
		existing, err = findActiveRuntimeSessionForTask(ctx, tx, namespace, sessionName, provider, agentName, activeTask)
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	if errors.Is(err, store.ErrNotFound) {
		session := harness.RuntimeSession{
			ID: generatedRuntimeSessionID(namespace, sessionName, provider),
			Owner: harness.RuntimeSessionOwner{
				Namespace: namespace, SessionName: sessionName, ActiveTask: activeTask, AgentName: agentName, Provider: provider,
			},
			State:         harness.RuntimeSessionStateTurnRunning,
			CleanupPolicy: harness.RuntimeCleanupPolicyDelete,
			IdleTimeout:   harness.DefaultRuntimeSessionIdleTimeout,
			CreatedAt:     now,
			UpdatedAt:     now,
		}
		if err := session.Validate(); err != nil {
			return nil, store.ValidationErrorf("%s", err.Error())
		}
		if err := insertRuntimeSessionTx(ctx, tx, session); err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return &session, nil
	}
	if existing.Owner.ActiveTask != "" && existing.Owner.ActiveTask != activeTask && existing.State == harness.RuntimeSessionStateTurnRunning {
		return nil, fmt.Errorf("runtime session %s is active for task %q", existing.ID, existing.Owner.ActiveTask)
	}
	if err := transitionRuntimeSessionForClaim(&existing, now); err != nil {
		return nil, err
	}
	existing.Owner.ActiveTask = activeTask
	if err := updateRuntimeSessionTx(ctx, tx, existing); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &existing, nil
}

func (s *Store) MarkRuntimeSessionActiveTurn(ctx context.Context, namespace string, id harness.RuntimeSessionID, activeTask string, now time.Time) (*harness.RuntimeSession, error) {
	return s.updateRuntimeSessionState(ctx, namespace, id, now, func(session *harness.RuntimeSession, at time.Time) error {
		if err := transitionRuntimeSessionForClaim(session, at); err != nil {
			return err
		}
		session.Owner.ActiveTask = strings.TrimSpace(activeTask)
		return nil
	})
}

func (s *Store) MarkRuntimeSessionIdle(ctx context.Context, namespace string, id harness.RuntimeSessionID, now time.Time) (*harness.RuntimeSession, error) {
	return s.updateRuntimeSessionState(ctx, namespace, id, now, func(session *harness.RuntimeSession, at time.Time) error {
		if err := transitionRuntimeSessionTo(session, harness.RuntimeSessionStateIdle, at); err != nil {
			return err
		}
		session.Owner.ActiveTask = ""
		return nil
	})
}

func (s *Store) RetainRuntimeSession(ctx context.Context, namespace string, id harness.RuntimeSessionID, now time.Time) (*harness.RuntimeSession, error) {
	return s.updateRuntimeSessionState(ctx, namespace, id, now, func(session *harness.RuntimeSession, at time.Time) error {
		session.CleanupPolicy = harness.RuntimeCleanupPolicyRetain
		session.Owner.ActiveTask = ""
		return transitionRuntimeSessionTo(session, harness.RuntimeSessionStateRetained, at)
	})
}

func (s *Store) ReleaseRuntimeSession(ctx context.Context, namespace string, id harness.RuntimeSessionID, cleanupPolicy harness.RuntimeCleanupPolicy, now time.Time) (*harness.RuntimeSession, error) {
	if cleanupPolicy == "" {
		cleanupPolicy = harness.RuntimeCleanupPolicyDelete
	}
	switch cleanupPolicy {
	case harness.RuntimeCleanupPolicyRetain:
		return s.RetainRuntimeSession(ctx, namespace, id, now)
	case harness.RuntimeCleanupPolicySuspend:
		return s.updateRuntimeSessionState(ctx, namespace, id, now, func(session *harness.RuntimeSession, at time.Time) error {
			session.CleanupPolicy = cleanupPolicy
			session.Owner.ActiveTask = ""
			return transitionRuntimeSessionTo(session, harness.RuntimeSessionStateSuspended, at)
		})
	default:
		return s.DeleteRuntimeSession(ctx, namespace, id, now)
	}
}

func (s *Store) DeleteRuntimeSession(ctx context.Context, namespace string, id harness.RuntimeSessionID, now time.Time) (*harness.RuntimeSession, error) {
	return s.updateRuntimeSessionState(ctx, namespace, id, now, func(session *harness.RuntimeSession, at time.Time) error {
		session.Owner.ActiveTask = ""
		return transitionRuntimeSessionTo(session, harness.RuntimeSessionStateDeleted, at)
	})
}

func (s *Store) MarkRuntimeSessionFailed(ctx context.Context, namespace string, id harness.RuntimeSessionID, state harness.RuntimeSessionState, _ string, now time.Time) (*harness.RuntimeSession, error) {
	if state == "" {
		state = harness.RuntimeSessionStateFailed
	}
	if state != harness.RuntimeSessionStateFailed && state != harness.RuntimeSessionStateUnhealthy {
		return nil, store.ValidationErrorf("runtime session failure state must be Failed or Unhealthy")
	}
	return s.updateRuntimeSessionState(ctx, namespace, id, now, func(session *harness.RuntimeSession, at time.Time) error {
		session.Owner.ActiveTask = ""
		return transitionRuntimeSessionTo(session, state, at)
	})
}

func (s *Store) CleanupIdleRuntimeSessions(ctx context.Context, now time.Time) ([]harness.RuntimeSession, error) {
	now = normalizeRuntimeSessionTime(now)
	rows, err := s.db.QueryContext(ctx,
		runtimeSessionSelectSQL()+` WHERE state IN (?, ?, ?) AND idle_timeout_ns > 0`,
		string(harness.RuntimeSessionStateIdle),
		string(harness.RuntimeSessionStateRetained),
		string(harness.RuntimeSessionStateSuspended),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	candidates := []harness.RuntimeSession{}
	for rows.Next() {
		session, err := scanRuntimeSession(rows)
		if err != nil {
			return nil, err
		}
		if !session.UpdatedAt.Add(session.IdleTimeout).After(now) {
			candidates = append(candidates, session)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	deleted := []harness.RuntimeSession{}
	for _, candidate := range candidates {
		session, ok, err := s.deleteExpiredInactiveRuntimeSession(ctx, candidate, now)
		if err != nil {
			return deleted, err
		}
		if ok {
			deleted = append(deleted, *session)
		}
	}
	return deleted, nil
}

func (s *Store) deleteExpiredInactiveRuntimeSession(ctx context.Context, candidate harness.RuntimeSession, now time.Time) (*harness.RuntimeSession, bool, error) {
	session, err := s.updateRuntimeSessionState(ctx, candidate.Owner.Namespace, candidate.ID, now, func(session *harness.RuntimeSession, at time.Time) error {
		if !runtimeSessionCleanupEligible(*session, at) {
			return errRuntimeSessionCleanupSkipped
		}
		session.Owner.ActiveTask = ""
		return transitionRuntimeSessionTo(session, harness.RuntimeSessionStateDeleted, at)
	})
	if err != nil {
		if errors.Is(err, errRuntimeSessionCleanupSkipped) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return session, true, nil
}

type runtimeSessionQueryable interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type runtimeSessionExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func getRuntimeSessionTx(ctx context.Context, db runtimeSessionQueryable, namespace string, id harness.RuntimeSessionID) (harness.RuntimeSession, error) {
	namespace, id, err := normalizeRuntimeSessionKey(namespace, id)
	if err != nil {
		return harness.RuntimeSession{}, err
	}
	row := db.QueryRowContext(ctx, runtimeSessionSelectSQL()+` WHERE namespace = ? AND id = ?`, namespace, strings.TrimSpace(string(id)))
	session, err := scanRuntimeSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return harness.RuntimeSession{}, store.ErrNotFound
	}
	if err != nil {
		return harness.RuntimeSession{}, err
	}
	return session, nil
}

func runtimeSessionSelectSQL() string {
	return `SELECT id, namespace, session_name, active_task, agent_name, provider, state, cleanup_policy,
		idle_timeout_ns, max_lifetime_ns, created_at, updated_at FROM runtime_sessions`
}

type runtimeSessionScanner interface {
	Scan(dest ...any) error
}

func scanRuntimeSession(scanner runtimeSessionScanner) (harness.RuntimeSession, error) {
	var session harness.RuntimeSession
	var provider, state, cleanupPolicy string
	var idleTimeoutNS, maxLifetimeNS int64
	err := scanner.Scan(
		&session.ID,
		&session.Owner.Namespace,
		&session.Owner.SessionName,
		&session.Owner.ActiveTask,
		&session.Owner.AgentName,
		&provider,
		&state,
		&cleanupPolicy,
		&idleTimeoutNS,
		&maxLifetimeNS,
		&session.CreatedAt,
		&session.UpdatedAt,
	)
	if err != nil {
		return harness.RuntimeSession{}, err
	}
	session.Owner.Provider = harness.ProviderKind(provider)
	session.State = harness.RuntimeSessionState(state)
	session.CleanupPolicy = harness.RuntimeCleanupPolicy(cleanupPolicy)
	session.IdleTimeout = time.Duration(idleTimeoutNS)
	session.MaxLifetime = time.Duration(maxLifetimeNS)
	return session, nil
}

func insertRuntimeSession(ctx context.Context, exec runtimeSessionExecer, session harness.RuntimeSession) error {
	_, err := exec.ExecContext(ctx,
		`INSERT INTO runtime_sessions (
			id, namespace, session_name, active_task, agent_name, provider, state, cleanup_policy,
			idle_timeout_ns, max_lifetime_ns, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(session.ID), session.Owner.Namespace, session.Owner.SessionName, session.Owner.ActiveTask,
		session.Owner.AgentName, string(session.Owner.Provider), string(session.State), string(session.CleanupPolicy),
		int64(session.IdleTimeout), int64(session.MaxLifetime), session.CreatedAt, session.UpdatedAt,
	)
	return err
}

func insertRuntimeSessionTx(ctx context.Context, tx *sql.Tx, session harness.RuntimeSession) error {
	return insertRuntimeSession(ctx, tx, session)
}

func updateRuntimeSessionTx(ctx context.Context, tx *sql.Tx, session harness.RuntimeSession) error {
	if err := session.Validate(); err != nil {
		return store.ValidationErrorf("%s", err.Error())
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE runtime_sessions
		 SET session_name = ?, active_task = ?, agent_name = ?, provider = ?, state = ?, cleanup_policy = ?,
		     idle_timeout_ns = ?, max_lifetime_ns = ?, updated_at = ?
		 WHERE namespace = ? AND id = ?`,
		session.Owner.SessionName,
		session.Owner.ActiveTask,
		session.Owner.AgentName,
		string(session.Owner.Provider),
		string(session.State),
		string(session.CleanupPolicy),
		int64(session.IdleTimeout),
		int64(session.MaxLifetime),
		session.UpdatedAt,
		session.Owner.Namespace,
		string(session.ID),
	)
	if err != nil {
		return err
	}
	return ensureRowsAffected(res)
}

func findActiveRuntimeSessionForTask(ctx context.Context, tx *sql.Tx, namespace, sessionName string, provider harness.ProviderKind, agentName, activeTask string) (harness.RuntimeSession, error) {
	row := tx.QueryRowContext(ctx,
		runtimeSessionSelectSQL()+` WHERE namespace = ? AND session_name = ? AND provider = ? AND agent_name = ? AND active_task = ? AND state = ?
		 ORDER BY updated_at DESC, id ASC LIMIT 1`,
		strings.TrimSpace(namespace), strings.TrimSpace(sessionName), strings.TrimSpace(string(provider)), strings.TrimSpace(agentName), strings.TrimSpace(activeTask),
		string(harness.RuntimeSessionStateTurnRunning),
	)
	session, err := scanRuntimeSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return harness.RuntimeSession{}, store.ErrNotFound
	}
	return session, err
}

func findReusableRuntimeSession(ctx context.Context, tx *sql.Tx, namespace, sessionName string, provider harness.ProviderKind, agentName string) (harness.RuntimeSession, error) {
	row := tx.QueryRowContext(ctx,
		runtimeSessionSelectSQL()+` WHERE namespace = ? AND session_name = ? AND provider = ? AND agent_name = ?
		   AND state IN (?, ?, ?, ?, ?)
		 ORDER BY updated_at DESC, id ASC LIMIT 1`,
		strings.TrimSpace(namespace), strings.TrimSpace(sessionName), strings.TrimSpace(string(provider)), strings.TrimSpace(agentName),
		string(harness.RuntimeSessionStateReady),
		string(harness.RuntimeSessionStateIdle),
		string(harness.RuntimeSessionStateRetained),
		string(harness.RuntimeSessionStateSuspended),
		string(harness.RuntimeSessionStateTurnRunning),
	)
	session, err := scanRuntimeSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return harness.RuntimeSession{}, store.ErrNotFound
	}
	return session, err
}

func (s *Store) updateRuntimeSessionState(ctx context.Context, namespace string, id harness.RuntimeSessionID, now time.Time, mutate func(*harness.RuntimeSession, time.Time) error) (*harness.RuntimeSession, error) {
	now = normalizeRuntimeSessionTime(now)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	session, err := getRuntimeSessionTx(ctx, tx, namespace, id)
	if err != nil {
		return nil, err
	}
	if err := mutate(&session, now); err != nil {
		return nil, err
	}
	if err := updateRuntimeSessionTx(ctx, tx, session); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &session, nil
}

func runtimeSessionCleanupEligible(session harness.RuntimeSession, now time.Time) bool {
	if strings.TrimSpace(session.Owner.ActiveTask) != "" || session.IdleTimeout <= 0 {
		return false
	}
	switch session.State {
	case harness.RuntimeSessionStateIdle, harness.RuntimeSessionStateRetained, harness.RuntimeSessionStateSuspended:
	default:
		return false
	}
	return !session.UpdatedAt.Add(session.IdleTimeout).After(now)
}

func normalizeRuntimeSessionForCreate(session *harness.RuntimeSession) (harness.RuntimeSession, error) {
	if session == nil {
		return harness.RuntimeSession{}, store.ValidationErrorf("runtime session is required")
	}
	normalized := *session
	normalizeRuntimeSession(&normalized, time.Now().UTC())
	if err := normalized.Validate(); err != nil {
		return harness.RuntimeSession{}, store.ValidationErrorf("invalid runtime session: %v", err)
	}
	return normalized, nil
}

func normalizeRuntimeSession(session *harness.RuntimeSession, now time.Time) {
	if session == nil {
		return
	}
	now = normalizeRuntimeSessionTime(now)
	session.ID = harness.RuntimeSessionID(strings.TrimSpace(string(session.ID)))
	session.Owner.Namespace = strings.TrimSpace(session.Owner.Namespace)
	session.Owner.SessionName = strings.TrimSpace(session.Owner.SessionName)
	session.Owner.ActiveTask = strings.TrimSpace(session.Owner.ActiveTask)
	session.Owner.AgentName = strings.TrimSpace(session.Owner.AgentName)
	session.Owner.Provider = harness.ProviderKind(strings.TrimSpace(string(session.Owner.Provider)))
	if session.State == "" {
		session.State = harness.RuntimeSessionStatePending
	}
	if session.CleanupPolicy == "" {
		session.CleanupPolicy = harness.RuntimeCleanupPolicyDelete
	}
	if session.CreatedAt.IsZero() {
		session.CreatedAt = now
	} else {
		session.CreatedAt = session.CreatedAt.UTC()
	}
	if session.UpdatedAt.IsZero() {
		session.UpdatedAt = session.CreatedAt
	} else {
		session.UpdatedAt = session.UpdatedAt.UTC()
	}
}

func normalizeRuntimeSessionTransition(transition harness.RuntimeSessionTransition) (harness.RuntimeSessionTransition, error) {
	namespace, id, err := normalizeRuntimeSessionKey(transition.Namespace, transition.ID)
	if err != nil {
		return harness.RuntimeSessionTransition{}, err
	}
	transition.Namespace = namespace
	transition.ID = id
	if err := harness.ValidateRuntimeSessionTransition(transition.From, transition.To); err != nil {
		return harness.RuntimeSessionTransition{}, store.ValidationErrorf("invalid runtime session transition: %v", err)
	}
	transition.UpdatedAt = normalizeRuntimeSessionTime(transition.UpdatedAt)
	return transition, nil
}

func validateRuntimeSessionFilter(filter harness.RuntimeSessionFilter) (harness.RuntimeSessionFilter, int, int, error) {
	filter.Namespace = strings.TrimSpace(filter.Namespace)
	if filter.Namespace == "" {
		return harness.RuntimeSessionFilter{}, 0, 0, store.ValidationErrorf("runtime session namespace is required")
	}
	filter.SessionName = strings.TrimSpace(filter.SessionName)
	filter.ActiveTask = strings.TrimSpace(filter.ActiveTask)
	filter.AgentName = strings.TrimSpace(filter.AgentName)
	filter.Provider = harness.ProviderKind(strings.TrimSpace(string(filter.Provider)))
	for _, state := range filter.States {
		if !harness.IsKnownRuntimeSessionState(state) {
			return harness.RuntimeSessionFilter{}, 0, 0, store.ValidationErrorf("unsupported runtime session state %q", state)
		}
	}
	for _, policy := range filter.CleanupPolicies {
		if !harness.IsKnownRuntimeCleanupPolicy(policy) {
			return harness.RuntimeSessionFilter{}, 0, 0, store.ValidationErrorf("unsupported runtime cleanup policy %q", policy)
		}
	}
	offset, err := parseOffsetCursor(filter.Cursor)
	if err != nil {
		return harness.RuntimeSessionFilter{}, 0, 0, store.ValidationErrorf("invalid runtime session cursor: %v", err)
	}
	limit := boundedLimit(filter.Limit, defaultRuntimeSessionLimit, maxRuntimeSessionLimit)
	return filter, limit, offset, nil
}

func normalizeRuntimeSessionKey(namespace string, id harness.RuntimeSessionID) (string, harness.RuntimeSessionID, error) {
	namespace = strings.TrimSpace(namespace)
	id = harness.RuntimeSessionID(strings.TrimSpace(string(id)))
	if namespace == "" {
		return "", "", store.ValidationErrorf("runtime session namespace is required")
	}
	if id == "" {
		return "", "", store.ValidationErrorf("runtime session id is required")
	}
	return namespace, id, nil
}

func normalizeRuntimeSessionTime(now time.Time) time.Time {
	if now.IsZero() {
		return time.Now().UTC()
	}
	return now.UTC()
}

func transitionRuntimeSessionForClaim(session *harness.RuntimeSession, now time.Time) error {
	switch session.State {
	case harness.RuntimeSessionStateReady, harness.RuntimeSessionStateIdle:
		return session.Transition(harness.RuntimeSessionStateTurnRunning, now)
	case harness.RuntimeSessionStateRetained, harness.RuntimeSessionStateSuspended, harness.RuntimeSessionStateUnhealthy:
		if err := session.Transition(harness.RuntimeSessionStateBooting, now); err != nil {
			return err
		}
		if err := session.Transition(harness.RuntimeSessionStateReady, now); err != nil {
			return err
		}
		return session.Transition(harness.RuntimeSessionStateTurnRunning, now)
	case harness.RuntimeSessionStateTurnRunning:
		session.UpdatedAt = normalizeRuntimeSessionTime(now)
		return nil
	default:
		return fmt.Errorf("runtime session %s is not reusable from state %s", session.ID, session.State)
	}
}

func transitionRuntimeSessionTo(session *harness.RuntimeSession, target harness.RuntimeSessionState, now time.Time) error {
	if session.State == target {
		session.UpdatedAt = normalizeRuntimeSessionTime(now)
		return nil
	}
	if harness.RuntimeSessionTransitionAllowed(session.State, target) {
		return session.Transition(target, now)
	}
	if target == harness.RuntimeSessionStateDeleted {
		if session.State != harness.RuntimeSessionStateDeleting {
			if err := session.Transition(harness.RuntimeSessionStateDeleting, now); err != nil {
				return err
			}
		}
		return session.Transition(harness.RuntimeSessionStateDeleted, now)
	}
	if target == harness.RuntimeSessionStateRetained || target == harness.RuntimeSessionStateSuspended {
		if session.State == harness.RuntimeSessionStateTurnRunning || session.State == harness.RuntimeSessionStateReady {
			if err := session.Transition(harness.RuntimeSessionStateIdle, now); err != nil {
				return err
			}
		}
		if session.State != harness.RuntimeSessionStateReleasing {
			if err := session.Transition(harness.RuntimeSessionStateReleasing, now); err != nil {
				return err
			}
		}
		return session.Transition(target, now)
	}
	return session.Transition(target, now)
}

func generatedRuntimeSessionID(namespace, sessionName string, provider harness.ProviderKind) harness.RuntimeSessionID {
	var random [4]byte
	_, _ = rand.Read(random[:])
	prefix := strings.NewReplacer("/", "-", "_", "-", ":", "-").Replace(strings.ToLower(strings.TrimSpace(sessionName)))
	prefix = strings.Trim(prefix, "-")
	if prefix == "" {
		prefix = "runtime"
	}
	if len(prefix) > 32 {
		prefix = strings.Trim(prefix[:32], "-")
	}
	seed := hex.EncodeToString(random[:])
	if seed == "" {
		seed = hex.EncodeToString([]byte(namespace + string(provider)))[:8]
	}
	return harness.RuntimeSessionID(prefix + "-" + seed)
}
