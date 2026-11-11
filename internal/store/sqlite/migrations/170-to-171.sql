-- SQLite transition: 170-to-171 (schema version 3 to 4).
-- A retry_pending work action still owns its dedupe key: command intake
-- coalesces into it, monitor cancellation stops it, and its MonitorRun
-- handoff is retried. The unique index now enforces that, as it already did
-- for queued, leased and running actions.
--
-- Earlier layouts let another active action take the key of a retry_pending
-- one. Such a retry_pending action is redundant, so it is cancelled before
-- the index is rebuilt, as CancelWorkActions would. Within each key the
-- queued, leased or running action stays, and otherwise the newest
-- retry_pending one (by created_at, then id). One ranked pass keeps the cost
-- to a sort of the active actions. It adds no rows.
-- Transaction and complete-layout validation are owned by the caller.

UPDATE work_actions
SET status = 'cancelled', blocked_reason = 'dedupe_superseded', error = '',
    completed_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP
WHERE id IN (
    SELECT id FROM (
        SELECT id, status, ROW_NUMBER() OVER (
            PARTITION BY monitor_namespace, monitor_name, dedupe_key
            ORDER BY status = 'retry_pending', created_at DESC, id DESC) AS keep_rank
        FROM work_actions
        WHERE dedupe_key <> '' AND status IN ('queued', 'leased', 'running', 'retry_pending'))
    WHERE status = 'retry_pending' AND keep_rank > 1);

DROP INDEX idx_work_actions_dedupe;

CREATE UNIQUE INDEX idx_work_actions_dedupe
    ON work_actions(monitor_namespace, monitor_name, dedupe_key)
    WHERE dedupe_key <> '' AND status IN ('queued', 'leased', 'running', 'retry_pending');
