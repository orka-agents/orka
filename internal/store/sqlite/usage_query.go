package sqlite

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	sqlitedriver "modernc.org/sqlite"

	"github.com/orka-agents/orka/internal/store"
)

func init() {
	sqlitedriver.MustRegisterDeterministicScalarFunction("orka_usage_link_at_or_before", 2, usageLinkAtOrBefore)
}

// PR link timestamps live in JSON. Compare them as Go times to preserve
// nanoseconds, timezone offsets, and dates outside the UnixNano range.
func usageLinkAtOrBefore(_ *sqlitedriver.FunctionContext, args []driver.Value) (driver.Value, error) {
	if len(args) != 2 {
		return nil, errors.New("usage PR link predicate requires two arguments")
	}
	asOf, ok := args[1].(int64)
	if !ok {
		return nil, errors.New("usage PR link predicate requires an integer asOf")
	}
	// Missing or null legacy timestamps decode to Go's zero time.
	var linkedAt time.Time
	if args[0] != nil {
		timestamp, ok := args[0].(string)
		if !ok {
			return nil, errors.New("usage PR link timestamp must be text or null")
		}
		var err error
		linkedAt, err = time.Parse(time.RFC3339Nano, timestamp)
		if err != nil {
			return nil, fmt.Errorf("invalid usage PR link timestamp: %w", err)
		}
	}
	if linkedAt.After(time.Unix(0, asOf)) {
		return int64(0), nil
	}
	return int64(1), nil
}

// Select whole work cohorts and the separate activity-period Tasks before
// reading observations. Counter history, including earlier Tasks, must remain
// available for cumulative deltas and Gateway access checks. Model matching is
// conservative here; the report applies it to the final measurements. PR state
// needs only the latest observation at asOf plus historical merge evidence;
// routine readiness refreshes must not consume the report's record budget.
const usageCohortQuery = `WITH
 selection(namespace, namespace_uid, as_of, from_at, until_at, repository, kind, model, work_id, record_limit) AS (VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)),
 candidate_works AS (
   SELECT w.id FROM usage_work_requests w, selection f
   WHERE w.namespace = f.namespace AND w.started_at <= f.as_of
     AND (f.namespace_uid IS NULL OR json_extract(w.data, '$.namespaceUID') = f.namespace_uid)
     AND (f.work_id = '' OR w.id = f.work_id)
     AND w.started_at >= COALESCE(f.from_at, -9223372036854775808) AND w.started_at < COALESCE(f.until_at, 9223372036854775807)
     AND (f.repository = '' OR json_extract(w.data, '$.repository') = f.repository)
     AND (f.kind = '' OR json_extract(w.data, '$.kind') = f.kind)
 ),
 work_tasks AS (
   SELECT w.id AS work_id, t.task_uid FROM candidate_works w, selection f
   JOIN usage_tasks t INDEXED BY idx_usage_tasks_work ON t.namespace = f.namespace AND json_extract(t.data, '$.workID') = w.id
   WHERE t.started_at <= f.as_of AND (f.namespace_uid IS NULL OR json_extract(t.data, '$.namespaceUID') = f.namespace_uid)
   UNION
   SELECT w.id, t.task_uid FROM candidate_works w, selection f
   JOIN usage_pr_links l ON l.namespace = f.namespace AND l.work_id = w.id
   JOIN usage_tasks t INDEXED BY idx_usage_tasks_pr ON t.namespace = f.namespace AND json_extract(t.data, '$.repository') = l.repository
     AND json_extract(t.data, '$.prNumber') = l.number
     AND COALESCE(json_extract(t.data, '$.namespaceUID'), '') = COALESCE(json_extract(l.data, '$.namespaceUID'), '')
   WHERE t.started_at <= f.as_of
     AND orka_usage_link_at_or_before(json_extract(l.data, '$.linkedAt'), f.as_of)
     AND (f.namespace_uid IS NULL OR json_extract(l.data, '$.namespaceUID') = f.namespace_uid)
 ),
 selected_works AS (
   SELECT w.id FROM candidate_works w, selection f
   WHERE f.model = '' OR EXISTS (
     SELECT 1 FROM work_tasks t JOIN usage_observations o INDEXED BY idx_usage_observations_task ON o.namespace = f.namespace AND o.task_uid = t.task_uid
     WHERE t.work_id = w.id AND o.observed_at <= f.as_of AND json_extract(o.data, '$.model') = f.model
       AND (f.namespace_uid IS NULL OR json_extract(o.data, '$.namespaceUID') = f.namespace_uid))
   LIMIT (SELECT record_limit FROM selection)
 ),
 selected_tasks AS (
   SELECT task_uid FROM work_tasks WHERE work_id IN (SELECT id FROM selected_works)
   UNION
   SELECT t.task_uid FROM usage_tasks t, selection f
   LEFT JOIN usage_work_requests w ON w.namespace = t.namespace AND w.id = json_extract(t.data, '$.workID')
     AND COALESCE(json_extract(w.data, '$.namespaceUID'), '') = COALESCE(json_extract(t.data, '$.namespaceUID'), '')
   WHERE f.work_id = '' AND t.namespace = f.namespace AND t.started_at <= f.as_of
     AND (f.namespace_uid IS NULL OR json_extract(t.data, '$.namespaceUID') = f.namespace_uid)
     AND t.started_at >= COALESCE(f.from_at, -9223372036854775808) AND t.started_at < COALESCE(f.until_at, 9223372036854775807)
     AND (f.repository = '' OR json_extract(t.data, '$.repository') = f.repository)
     AND (f.kind = '' OR COALESCE(json_extract(w.data, '$.kind'),
       CASE WHEN json_extract(t.data, '$.prNumber') > 0 THEN 'pull_request' ELSE '' END) = f.kind)
     AND (f.model = '' OR EXISTS (SELECT 1 FROM usage_observations o INDEXED BY idx_usage_observations_task
       WHERE o.namespace = t.namespace AND o.task_uid = t.task_uid AND o.observed_at <= f.as_of
         AND (f.namespace_uid IS NULL OR json_extract(o.data, '$.namespaceUID') = f.namespace_uid)
         AND json_extract(o.data, '$.model') = f.model))
 ),
 selected_counters AS (
   SELECT o.counter_id FROM usage_observations o INDEXED BY idx_usage_observations_task, selection f
   WHERE o.namespace = f.namespace AND o.observed_at <= f.as_of AND o.task_uid IN (SELECT task_uid FROM selected_tasks)
     AND (f.namespace_uid IS NULL OR json_extract(o.data, '$.namespaceUID') = f.namespace_uid)
   UNION
   SELECT o.counter_id FROM usage_observations o, selection f
   WHERE f.work_id = '' AND o.namespace = f.namespace AND o.observed_at <= f.as_of AND f.repository = '' AND f.kind = ''
     AND (f.namespace_uid IS NULL OR json_extract(o.data, '$.namespaceUID') = f.namespace_uid)
     AND o.observed_at >= COALESCE(f.from_at, -9223372036854775808) AND o.observed_at < COALESCE(f.until_at, 9223372036854775807)
     AND (f.model = '' OR json_extract(o.data, '$.model') = f.model)
     AND NOT EXISTS (SELECT 1 FROM usage_tasks t WHERE t.namespace = o.namespace AND t.task_uid = o.task_uid AND t.started_at <= f.as_of
       AND COALESCE(json_extract(t.data, '$.namespaceUID'), '') = COALESCE(json_extract(o.data, '$.namespaceUID'), ''))
 ),
 selected_observations AS MATERIALIZED (
   SELECT o.* FROM usage_observations o INDEXED BY idx_usage_observations_counter, selection f
   WHERE o.namespace = f.namespace AND o.observed_at <= f.as_of AND o.counter_id IN (SELECT counter_id FROM selected_counters)
     AND (f.namespace_uid IS NULL OR json_extract(o.data, '$.namespaceUID') = f.namespace_uid)
   LIMIT (SELECT record_limit FROM selection)
 ),
 retained_tasks AS MATERIALIZED (
   SELECT t.* FROM (SELECT task_uid FROM selected_tasks UNION SELECT task_uid FROM selected_observations) ids, selection f
   CROSS JOIN usage_tasks t ON t.namespace = f.namespace AND t.task_uid = ids.task_uid
   WHERE t.started_at <= f.as_of AND (f.namespace_uid IS NULL OR json_extract(t.data, '$.namespaceUID') = f.namespace_uid)
   LIMIT (SELECT record_limit FROM selection)
 )
 SELECT 'work' AS kind, w.data, w.started_at AS observed_at, w.id AS row_order FROM usage_work_requests w, selection f
 WHERE w.namespace = f.namespace AND w.started_at <= f.as_of
   AND (f.namespace_uid IS NULL OR json_extract(w.data, '$.namespaceUID') = f.namespace_uid) AND
   (w.id IN (SELECT id FROM selected_works) OR w.id IN (SELECT json_extract(data, '$.workID') FROM retained_tasks))
 UNION ALL SELECT 'task', data, started_at, task_uid FROM retained_tasks
 UNION ALL SELECT 'observation', data, observed_at, recorded_seq FROM selected_observations
 UNION ALL SELECT 'link', l.data, 0, l.work_id FROM usage_pr_links l, selection f
 WHERE l.namespace = f.namespace AND l.work_id IN (SELECT id FROM selected_works)
   AND orka_usage_link_at_or_before(json_extract(l.data, '$.linkedAt'), f.as_of)
   AND (f.namespace_uid IS NULL OR json_extract(l.data, '$.namespaceUID') = f.namespace_uid)
 UNION ALL SELECT 'pr', p.data, p.observed_at, CAST(p.number AS TEXT) FROM usage_pull_requests p, selection f
 WHERE p.namespace = f.namespace AND p.observed_at <= f.as_of AND (f.namespace_uid IS NULL OR p.namespace_uid = f.namespace_uid)
   AND (json_extract(p.data, '$.mergedAt') IS NOT NULL OR p.observed_at =
     (SELECT MAX(latest.observed_at) FROM usage_pull_requests latest
      WHERE latest.namespace = p.namespace AND latest.namespace_uid = p.namespace_uid
        AND latest.repository = p.repository AND latest.number = p.number AND latest.observed_at <= f.as_of))
   AND (p.repository, p.number) IN
   (SELECT l.repository, l.number FROM usage_pr_links l WHERE l.namespace = f.namespace AND l.work_id IN (SELECT id FROM selected_works)
     AND orka_usage_link_at_or_before(json_extract(l.data, '$.linkedAt'), f.as_of)
     AND COALESCE(json_extract(l.data, '$.namespaceUID'), '') = p.namespace_uid)
 ORDER BY kind, observed_at, row_order LIMIT (SELECT record_limit FROM selection)`

func loadUsageNamespace(ctx context.Context, db taskDataExecutor, namespace string, filter store.UsageFilter, result *store.UsageData) error {
	var namespaceUID, from, until any
	if filter.NamespaceUIDs != nil {
		namespaceUID = filter.NamespaceUIDs[namespace]
	}
	if !filter.From.IsZero() {
		from = filter.From.UnixNano()
	}
	if !filter.Until.IsZero() {
		until = filter.Until.UnixNano()
	}
	limit := -1
	loaded := len(result.Works) + len(result.Tasks) + len(result.Observations) + len(result.Links) + len(result.PullRequests)
	if filter.MaxRecords > 0 {
		limit = filter.MaxRecords - loaded + 1
	}
	rows, err := db.QueryContext(ctx, usageCohortQuery, namespace, namespaceUID, filter.AsOf.UnixNano(), from, until, strings.ToLower(filter.Repository), filter.Kind, filter.Model, filter.WorkID, limit)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		loaded++
		if filter.MaxRecords > 0 && loaded > filter.MaxRecords {
			return store.ErrUsageSelectionTooLarge
		}
		var kind, data, rowOrder string
		var observedAt int64
		if err := rows.Scan(&kind, &data, &observedAt, &rowOrder); err != nil {
			return err
		}
		switch kind {
		case "work":
			err = appendUsageRow(&result.Works, data)
		case "task":
			err = appendUsageRow(&result.Tasks, data)
		case "observation":
			err = appendUsageRow(&result.Observations, data)
		case "link":
			err = appendUsageRow(&result.Links, data)
		case "pr":
			err = appendUsageRow(&result.PullRequests, data)
		}
		if err != nil {
			return err
		}
	}
	return rows.Err()
}

func appendUsageRow[T any](values *[]T, data string) error {
	var value T
	if err := json.Unmarshal([]byte(data), &value); err != nil {
		return err
	}
	*values = append(*values, value)
	return nil
}
