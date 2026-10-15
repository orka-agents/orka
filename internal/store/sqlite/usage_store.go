package sqlite

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/store"
)

var _ store.UsageStore = (*Store)(nil)

func (s *Store) usageTransaction(ctx context.Context, fn func(context.Context) error) error {
	if s.taskDataTx(ctx) != nil {
		return fn(ctx)
	}
	return s.WithTaskDataTransaction(ctx, fn)
}

func (s *Store) RegisterUsageWork(ctx context.Context, work store.UsageWorkRequest) error {
	work.Repository = strings.ToLower(strings.TrimSpace(work.Repository))
	if work.Namespace == "" || work.MonitorUID == "" || work.MonitorName == "" || work.Repository == "" || work.Number <= 0 ||
		(work.Kind != "issue" && work.Kind != "pull_request") {
		return store.ValidationErrorf("usage work requires a namespace, monitor identity, repository and issue or PR identity")
	}
	work.ID = store.UsageWorkID(work.Namespace, work.MonitorUID, work.Repository, work.Kind, work.Number)
	if work.StartedAt.IsZero() {
		work.StartedAt = time.Now().UTC()
	}
	data, err := json.Marshal(work)
	if err != nil {
		return err
	}
	_, err = s.taskDataExecutor(ctx).ExecContext(ctx,
		`INSERT INTO usage_work_requests(namespace, id, started_at, data) VALUES (?, ?, ?, ?)
		 ON CONFLICT(namespace, id) DO NOTHING`, work.Namespace, work.ID, work.StartedAt.UnixNano(), string(data))
	return err
}

func (s *Store) RegisterUsageTask(ctx context.Context, task store.UsageTask) error {
	task.Repository = strings.ToLower(task.Repository)
	if task.Namespace == "" || task.TaskUID == "" || task.TaskName == "" {
		return store.ValidationErrorf("usage task requires namespace, name and UID")
	}
	return s.usageTransaction(ctx, func(ctx context.Context) error {
		db := s.taskDataExecutor(ctx)
		var existing store.UsageTask
		err := readUsageJSON(ctx, db, &existing, `SELECT data FROM usage_tasks WHERE namespace = ? AND task_uid = ?`, task.Namespace, task.TaskUID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			if err := mergeUsageTaskSnapshot(&task, existing); err != nil {
				return err
			}
		}
		if task.WorkID == "" && task.ParentTaskUID != "" {
			var parent store.UsageTask
			err := readUsageJSON(ctx, db, &parent, `SELECT data FROM usage_tasks WHERE namespace = ? AND task_uid = ?`, task.Namespace, task.ParentTaskUID)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if err == nil {
				task.WorkID, task.Repository, task.PRNumber = parent.WorkID, parent.Repository, parent.PRNumber
				if task.NamespaceUID != "" && task.NamespaceUID != parent.NamespaceUID {
					return store.ConflictErrorf("usage task namespace identity differs from parent")
				}
				task.NamespaceUID = parent.NamespaceUID
				task.Role = "delegated"
				if parent.GatewayOwner != nil {
					task.GatewayOwner = parent.GatewayOwner
				}
			}
		}
		if task.WorkID != "" {
			var work store.UsageWorkRequest
			if err := readUsageJSON(ctx, db, &work, `SELECT data FROM usage_work_requests WHERE namespace = ? AND id = ?`, task.Namespace, task.WorkID); err != nil {
				return fmt.Errorf("load usage task work: %w", err)
			}
			if task.Repository != "" && !strings.EqualFold(task.Repository, work.Repository) {
				return store.ConflictErrorf("usage task repository differs from work request")
			}
			if task.NamespaceUID != "" && task.NamespaceUID != work.NamespaceUID {
				return store.ConflictErrorf("usage task namespace identity differs from work")
			}
			task.NamespaceUID = work.NamespaceUID
			task.Repository = work.Repository
		}
		if task.StartedAt.IsZero() {
			task.StartedAt = time.Now().UTC()
		}
		recordUsageTaskPhase(&task)
		data, err := json.Marshal(task)
		if err != nil {
			return err
		}
		_, err = db.ExecContext(ctx, `INSERT INTO usage_tasks(namespace, task_uid, task_name, started_at, data) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(namespace, task_uid) DO UPDATE SET data = excluded.data`, task.Namespace, task.TaskUID, task.TaskName, task.StartedAt.UnixNano(), string(data))
		return err
	})
}

func recordUsageTaskPhase(task *store.UsageTask) {
	// Capture visibility while holding the same transaction lock as report
	// reads. Kubernetes lifecycle timestamps may be rounded or arrive late.
	recordedAt := time.Now().UTC()
	at := task.PhaseObservedAt
	if at.IsZero() {
		at = recordedAt
	}
	state := store.UsageTaskPhase{Phase: task.Phase, ObservedAt: at.UTC(), RecordedAt: recordedAt, Attempt: task.PhaseAttempt}
	if !slices.ContainsFunc(task.PhaseHistory, func(existing store.UsageTaskPhase) bool {
		return existing.Phase == state.Phase && existing.Attempt == state.Attempt && existing.ObservedAt.Equal(state.ObservedAt)
	}) {
		task.PhaseHistory = append(task.PhaseHistory, state)
	}
	// Monitor and Task-controller writes can arrive in either order. Retain
	// distinct observations even for repeated phases, so a late observation
	// between them cannot replace the newer state or corrupt an asOf report.
	slices.SortStableFunc(task.PhaseHistory, func(a, b store.UsageTaskPhase) int {
		if order := cmp.Compare(a.ObservedAt.Unix(), b.ObservedAt.Unix()); order != 0 {
			return order
		}
		// Kubernetes timestamps have second precision. Within a second, use
		// attempts and lifecycle order, with terminal states always last.
		aOrder, bOrder := usageTaskPhaseOrder(a), usageTaskPhaseOrder(b)
		if aOrder == usageTerminalPhaseOrder || bOrder == usageTerminalPhaseOrder {
			if order := cmp.Compare(aOrder, bOrder); order != 0 {
				return order
			}
		}
		if order := cmp.Compare(a.Attempt, b.Attempt); order != 0 {
			return order
		}
		if order := cmp.Compare(aOrder, bOrder); order != 0 {
			return order
		}
		return a.ObservedAt.Compare(b.ObservedAt)
	})
	task.Phase = task.PhaseHistory[len(task.PhaseHistory)-1].Phase
}

const usageTerminalPhaseOrder = 4

func usageTaskPhaseOrder(state store.UsageTaskPhase) int {
	switch state.Phase {
	case "Pending":
		if state.Attempt > 0 {
			// Retrying Pending follows the attempt that just failed.
			return 3
		}
		return 0
	case "Running":
		return 1
	case "Finalizing":
		return 2
	case "Succeeded", "Failed", "Cancelled":
		return usageTerminalPhaseOrder
	default:
		return 0
	}
}

func mergeUsageTaskSnapshot(task *store.UsageTask, existing store.UsageTask) error {
	if existing.TaskName != task.TaskName || (task.WorkID != "" && existing.WorkID != "" && task.WorkID != existing.WorkID) {
		return store.ConflictErrorf("usage task identity or work ownership changed")
	}
	if task.NamespaceUID != "" && task.NamespaceUID != existing.NamespaceUID {
		return store.ConflictErrorf("usage task namespace identity changed")
	}
	task.NamespaceUID = existing.NamespaceUID
	task.PhaseHistory = existing.PhaseHistory
	if existing.GatewayOwner != nil {
		task.GatewayOwner = existing.GatewayOwner
	}
	task.StartedAt = existing.StartedAt
	if task.WorkID == "" {
		task.WorkID = existing.WorkID
	}
	if task.Repository == "" {
		task.Repository = existing.Repository
	}
	if task.PRNumber == 0 {
		task.PRNumber = existing.PRNumber
	}
	if task.Role == "" {
		task.Role = existing.Role
	}
	if task.Runtime == "" {
		task.Runtime = existing.Runtime
	}
	if task.SessionName == "" {
		task.SessionName = existing.SessionName
	}
	return nil
}

func (s *Store) RecordUsage(ctx context.Context, observation store.UsageObservation) error {
	return recordUsage(ctx, s.taskDataExecutor(ctx), observation)
}

func recordUsage(ctx context.Context, db taskDataExecutor, observation store.UsageObservation) error {
	if observation.ID == "" || observation.Namespace == "" || observation.CounterID == "" || observation.ObservedAt.IsZero() {
		return store.ValidationErrorf("usage observation requires identity, namespace, counter and timestamp")
	}
	if observation.TaskUID != "" {
		var namespaceUID string
		err := db.QueryRowContext(ctx, `SELECT COALESCE(json_extract(data, '$.namespaceUID'), '') FROM usage_tasks WHERE namespace = ? AND task_uid = ?`,
			observation.Namespace, observation.TaskUID).Scan(&namespaceUID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			if observation.NamespaceUID != "" && observation.NamespaceUID != namespaceUID {
				return store.ConflictErrorf("usage observation namespace identity differs from Task")
			}
			observation.NamespaceUID = namespaceUID
		}
	}
	switch observation.Scope {
	case store.UsageScopeCall, store.UsageScopeAttempt, store.UsageScopeSession:
	default:
		return store.ValidationErrorf("unsupported usage counter scope")
	}
	switch observation.Source {
	case store.UsageSourceProvider, store.UsageSourceAgent, store.UsageSourceEstimate:
	default:
		return store.ValidationErrorf("unsupported usage source")
	}
	for _, count := range []*int64{observation.InputTokens, observation.OutputTokens, observation.CachedInputTokens, observation.CacheWriteInputTokens} {
		if count != nil && (*count < 0 || *count > store.MaxUsageTokenCount) {
			return store.ValidationErrorf("invalid usage count")
		}
	}
	if observation.InputTokens != nil {
		remainingInput := *observation.InputTokens
		for _, count := range []*int64{observation.CachedInputTokens, observation.CacheWriteInputTokens} {
			if count == nil {
				continue
			}
			if *count > remainingInput {
				return store.ValidationErrorf("cache usage exceeds inclusive input count")
			}
			remainingInput -= *count
		}
	}
	observation.Provider, _, _ = events.RedactAndTruncateExecutionEventText(observation.Provider, 256)
	observation.Model, _, _ = events.RedactAndTruncateExecutionEventText(observation.Model, 256)
	observation.ObservedAt = observation.ObservedAt.UTC()
	data, err := json.Marshal(observation)
	if err != nil {
		return err
	}
	result, err := db.ExecContext(ctx, `INSERT INTO usage_observations(namespace, id, task_uid, counter_id, observed_at, data)
	 VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT(namespace, id) DO NOTHING`, observation.Namespace, observation.ID, observation.TaskUID,
		observation.CounterID, observation.ObservedAt.UnixNano(), string(data))
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil {
		return err
	} else if n > 0 {
		return nil
	}
	var existing store.UsageObservation
	if err := readUsageJSON(ctx, db, &existing, `SELECT data FROM usage_observations WHERE namespace = ? AND id = ?`, observation.Namespace, observation.ID); err != nil {
		return err
	}
	if !reflect.DeepEqual(existing, observation) {
		return store.ConflictErrorf("usage observation identity was reused")
	}
	return nil
}

func (s *Store) LinkUsagePullRequest(ctx context.Context, link store.UsagePRLink) error {
	link.Repository = strings.ToLower(link.Repository)
	if link.Namespace == "" || link.WorkID == "" || link.Repository == "" || link.Number <= 0 || link.EvidenceID == "" {
		return store.ValidationErrorf("usage PR link requires work, repository, PR and publication evidence")
	}
	if link.Origin != store.UsagePRCreated && link.Origin != store.UsagePRAssisted && link.Origin != store.UsagePRReview {
		return store.ValidationErrorf("unsupported PR relationship")
	}
	if link.LinkedAt.IsZero() {
		link.LinkedAt = time.Now().UTC()
	}
	return s.usageTransaction(ctx, func(ctx context.Context) error {
		db := s.taskDataExecutor(ctx)
		var work store.UsageWorkRequest
		if err := readUsageJSON(ctx, db, &work, `SELECT data FROM usage_work_requests WHERE namespace = ? AND id = ?`, link.Namespace, link.WorkID); err != nil {
			return err
		}
		if work.Repository != link.Repository {
			return store.ConflictErrorf("PR and work repository differ")
		}
		link.NamespaceUID = work.NamespaceUID
		var existing store.UsagePRLink
		err := readUsageJSON(ctx, db, &existing, `SELECT data FROM usage_pr_links WHERE namespace = ? AND work_id = ? AND repository = ? AND number = ?`, link.Namespace, link.WorkID, link.Repository, link.Number)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			if existing.Origin == store.UsagePRCreated || link.Origin != store.UsagePRCreated {
				return nil
			}
			link.LinkedAt = existing.LinkedAt
		}
		data, err := json.Marshal(link)
		if err != nil {
			return err
		}
		_, err = db.ExecContext(ctx, `INSERT INTO usage_pr_links(namespace, work_id, repository, number, data) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(namespace, work_id, repository, number) DO UPDATE SET data = excluded.data`, link.Namespace, link.WorkID, link.Repository, link.Number, string(data))
		return err
	})
}

func (s *Store) RecordUsagePullRequest(ctx context.Context, pr store.UsagePullRequest) error {
	pr.Repository = strings.ToLower(pr.Repository)
	if pr.Namespace == "" || pr.Repository == "" || pr.Number <= 0 || pr.ObservedAt.IsZero() {
		return store.ValidationErrorf("usage PR observation requires a namespace, repository, number and timestamp")
	}
	if pr.Ready && (pr.State != "open" || pr.HeadSHA == "" || pr.GitHubID == "") {
		return store.ValidationErrorf("PR readiness requires an identified open PR and exact head")
	}
	if pr.State == "merged" && (pr.GitHubID == "" || pr.MergedAt == nil) {
		return store.ValidationErrorf("merged PR requires GitHub identity and merge time")
	}
	data, err := json.Marshal(pr)
	if err != nil {
		return err
	}
	_, err = s.taskDataExecutor(ctx).ExecContext(ctx, `INSERT INTO usage_pull_requests(namespace, namespace_uid, repository, number, observed_at, data)
	 VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`, pr.Namespace, pr.NamespaceUID, pr.Repository, pr.Number, pr.ObservedAt.UnixNano(), string(data))
	return err
}

func (s *Store) ListUsagePullRequestLinks(ctx context.Context, namespace, monitorUID string, before time.Time, limit int) ([]store.UsagePRLink, error) {
	if limit <= 0 || limit > 100 {
		return nil, store.ValidationErrorf("usage outcome refresh limit must be between 1 and 100")
	}
	return readUsageRows[store.UsagePRLink](ctx, s.taskDataExecutor(ctx), `SELECT l.data FROM usage_pr_links l
	 JOIN usage_work_requests w ON w.namespace = l.namespace AND w.id = l.work_id
	   AND COALESCE(json_extract(w.data, '$.namespaceUID'), '') = COALESCE(json_extract(l.data, '$.namespaceUID'), '')
	 LEFT JOIN usage_pull_requests p ON p.namespace = l.namespace AND p.namespace_uid = COALESCE(json_extract(l.data, '$.namespaceUID'), '')
	   AND p.repository = l.repository AND p.number = l.number
	 WHERE l.namespace = ? AND json_extract(w.data, '$.monitorUID') = ?
	 AND NOT EXISTS (SELECT 1 FROM usage_pull_requests merged
	   WHERE merged.namespace = l.namespace AND merged.namespace_uid = COALESCE(json_extract(l.data, '$.namespaceUID'), '')
	     AND merged.repository = l.repository AND merged.number = l.number
	     AND json_extract(merged.data, '$.state') = 'merged')
	 GROUP BY l.namespace, COALESCE(json_extract(l.data, '$.namespaceUID'), ''), l.repository, l.number
	 HAVING COALESCE(MAX(p.observed_at), 0) < ?
	 ORDER BY COALESCE(MAX(p.observed_at), 0), l.repository, l.number LIMIT ?`, namespace, monitorUID, before.UnixNano(), limit)
}

func (s *Store) LoadUsage(ctx context.Context, filter store.UsageFilter) (store.UsageData, error) {
	var result store.UsageData
	err := s.usageTransaction(ctx, func(ctx context.Context) error {
		var err error
		result, err = loadUsage(ctx, s.taskDataExecutor(ctx), filter)
		return err
	})
	return result, err
}

func loadUsage(ctx context.Context, db taskDataExecutor, filter store.UsageFilter) (store.UsageData, error) {
	var result store.UsageData
	if len(filter.Namespaces) == 0 {
		return result, store.ValidationErrorf("usage report requires explicit namespaces")
	}
	if filter.AsOf.IsZero() {
		filter.AsOf = time.Now().UTC()
	}
	for _, namespace := range filter.Namespaces {
		if namespace == "" {
			return result, store.ValidationErrorf("usage report namespace must not be empty")
		}
		if filter.NamespaceUIDs != nil && filter.NamespaceUIDs[namespace] == "" {
			return result, store.ValidationErrorf("usage report requires a verified namespace UID")
		}
		if err := loadUsageNamespace(ctx, db, namespace, filter, &result); err != nil {
			return store.UsageData{}, err
		}
	}
	var since int64
	err := db.QueryRowContext(ctx, `SELECT retained_since FROM usage_retention WHERE id = 1`).Scan(&since)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return result, err
	}
	if err == nil {
		at := time.Unix(0, since).UTC()
		result.RetainedSince = &at
	}
	return result, nil
}

func readUsageJSON(ctx context.Context, db taskDataExecutor, target any, query string, args ...any) error {
	var data string
	if err := db.QueryRowContext(ctx, query, args...).Scan(&data); err != nil {
		return err
	}
	return json.Unmarshal([]byte(data), target)
}

func readUsageRows[T any](ctx context.Context, db taskDataExecutor, query string, args ...any) ([]T, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := []T{}
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var value T
		if err := json.Unmarshal([]byte(data), &value); err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

// PruneUsage expires whole, inactive work cohorts. It never trims the early
// attempts from a retained request. Cumulative counter baselines remain until
// the counter itself expires, so retention cannot turn an old total into new usage.
func (s *Store) PruneUsage(ctx context.Context, before time.Time) error {
	return s.usageTransaction(ctx, func(ctx context.Context) error {
		db := s.taskDataExecutor(ctx)
		// A work remains active while any Task is nonterminal, a linked PR is
		// open, or a recent model observation belongs to it.
		_, err := db.ExecContext(ctx, `DELETE FROM usage_work_requests AS w WHERE started_at < ?
		 AND NOT EXISTS (SELECT 1 FROM usage_tasks t WHERE t.namespace = w.namespace
		   AND COALESCE(json_extract(t.data, '$.namespaceUID'), '') = COALESCE(json_extract(w.data, '$.namespaceUID'), '') AND
		   (json_extract(t.data, '$.workID') = w.id OR EXISTS
		     (SELECT 1 FROM usage_pr_links l WHERE l.namespace = w.namespace AND l.work_id = w.id
		       AND l.repository = json_extract(t.data, '$.repository') AND l.number = json_extract(t.data, '$.prNumber')))
		   AND (json_extract(t.data, '$.phase') NOT IN ('Succeeded', 'Failed', 'Cancelled') OR t.started_at >= ?
		     OR unixepoch(json_extract(t.data, '$.phaseHistory[#-1].observedAt')) >= ? OR EXISTS
		     (SELECT 1 FROM usage_observations o WHERE o.namespace = t.namespace AND o.task_uid = t.task_uid AND o.observed_at >= ?
		       AND COALESCE(json_extract(o.data, '$.namespaceUID'), '') = COALESCE(json_extract(t.data, '$.namespaceUID'), ''))))
		 AND NOT EXISTS (SELECT 1 FROM usage_pr_links l LEFT JOIN usage_pull_requests p ON p.namespace = l.namespace
		   AND p.namespace_uid = COALESCE(json_extract(l.data, '$.namespaceUID'), '') AND p.repository = l.repository AND p.number = l.number AND p.observed_at =
		     (SELECT MAX(p2.observed_at) FROM usage_pull_requests p2 WHERE p2.namespace = p.namespace AND p2.namespace_uid = p.namespace_uid AND p2.repository = p.repository AND p2.number = p.number)
		   WHERE l.namespace = w.namespace AND l.work_id = w.id
		   AND (json_extract(p.data, '$.state') = 'open'
		     OR unixepoch(COALESCE(json_extract(p.data, '$.mergedAt'), json_extract(p.data, '$.closedAt'))) >= ?
		     OR (COALESCE(json_extract(p.data, '$.state'), 'unknown') NOT IN ('merged', 'closed', 'open')
		       AND (p.observed_at >= ? OR unixepoch(json_extract(l.data, '$.linkedAt')) >= ?))))`,
			before.UnixNano(), before.UnixNano(), before.Unix(), before.UnixNano(), before.Unix(), before.UnixNano(), before.Unix())
		if err != nil {
			return err
		}
		_, err = db.ExecContext(ctx, `DELETE FROM usage_tasks AS t WHERE started_at < ? AND json_extract(data, '$.phase') IN ('Succeeded', 'Failed', 'Cancelled')
		 AND COALESCE(unixepoch(json_extract(data, '$.phaseHistory[#-1].observedAt')), 0) < ?
		 AND NOT EXISTS (SELECT 1 FROM usage_work_requests w WHERE w.namespace = t.namespace AND w.id = json_extract(t.data, '$.workID')
		   AND COALESCE(json_extract(w.data, '$.namespaceUID'), '') = COALESCE(json_extract(t.data, '$.namespaceUID'), ''))
		 AND NOT EXISTS (SELECT 1 FROM usage_pr_links l JOIN usage_work_requests w ON w.namespace = l.namespace AND w.id = l.work_id
		   WHERE l.namespace = t.namespace AND l.repository = json_extract(t.data, '$.repository') AND l.number = json_extract(t.data, '$.prNumber')
		     AND COALESCE(json_extract(l.data, '$.namespaceUID'), '') = COALESCE(json_extract(t.data, '$.namespaceUID'), ''))
		 AND NOT EXISTS (SELECT 1 FROM usage_observations o WHERE o.namespace = t.namespace AND o.task_uid = t.task_uid AND o.observed_at >= ?
		   AND COALESCE(json_extract(o.data, '$.namespaceUID'), '') = COALESCE(json_extract(t.data, '$.namespaceUID'), ''))`, before.UnixNano(), before.Unix(), before.UnixNano())
		if err != nil {
			return err
		}
		for _, query := range []string{
			`DELETE FROM usage_pr_links AS l WHERE NOT EXISTS (SELECT 1 FROM usage_work_requests w WHERE w.namespace = l.namespace AND w.id = l.work_id
			  AND COALESCE(json_extract(w.data, '$.namespaceUID'), '') = COALESCE(json_extract(l.data, '$.namespaceUID'), ''))`,
			`DELETE FROM usage_pull_requests AS p WHERE NOT EXISTS (SELECT 1 FROM usage_pr_links l WHERE l.namespace = p.namespace AND l.repository = p.repository AND l.number = p.number
			  AND COALESCE(json_extract(l.data, '$.namespaceUID'), '') = p.namespace_uid)`,
		} {
			if _, err := db.ExecContext(ctx, query); err != nil {
				return err
			}
		}
		_, err = db.ExecContext(ctx, `DELETE FROM usage_observations AS o WHERE observed_at < ?
		 AND NOT EXISTS (SELECT 1 FROM usage_tasks t WHERE t.namespace = o.namespace AND t.task_uid = o.task_uid
		   AND COALESCE(json_extract(t.data, '$.namespaceUID'), '') = COALESCE(json_extract(o.data, '$.namespaceUID'), ''))
		 AND NOT EXISTS (SELECT 1 FROM usage_observations kept JOIN usage_tasks t ON t.namespace = kept.namespace AND t.task_uid = kept.task_uid
		   WHERE kept.namespace = o.namespace AND kept.counter_id = o.counter_id
		     AND COALESCE(json_extract(kept.data, '$.namespaceUID'), '') = COALESCE(json_extract(o.data, '$.namespaceUID'), ''))
		 AND NOT EXISTS (SELECT 1 FROM usage_observations newer WHERE newer.namespace = o.namespace AND newer.counter_id = o.counter_id AND newer.observed_at >= ?
		   AND COALESCE(json_extract(newer.data, '$.namespaceUID'), '') = COALESCE(json_extract(o.data, '$.namespaceUID'), ''))`, before.UnixNano(), before.UnixNano())
		if err != nil {
			return err
		}
		_, err = db.ExecContext(ctx, `INSERT INTO usage_retention(id, retained_since) VALUES (1, ?) ON CONFLICT(id) DO UPDATE SET retained_since = MAX(retained_since, excluded.retained_since)`, before.UnixNano())
		return err
	})
}
