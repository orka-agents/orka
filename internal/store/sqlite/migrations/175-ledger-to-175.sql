-- SQLite layout repair at schema version 8: 175-ledger-to-175.
-- An unreleased build wrote version 8 with the feedback attempt ledger but
-- without the feedback outcome table. This adds the table and recovers one
-- outcome per admitted feedback repair command from what that build kept:
-- - a repair job that settled from its Task, whatever its command's status,
--   recovers succeeded, no_change (succeeded without a push), or failed, and
--   charges every admitted ID, since the targets its Task froze were not
--   recorded;
-- - a finished command whose job is still queued and was not left by a
--   failed Task create may have run its Task, as when a cancellation settled
--   the command first, so it recovers failed and keeps every charge;
-- - otherwise a finished command whose work action was blocked as withdrawn
--   recovers withdrawn, and any other finished command failed, neither of
--   which ran a Task or charges anything;
-- - any other command has not finished and stays reserved, which reserves
--   every admitted ID. Its handoff or ingestion starts it from its Task.
-- Transaction and complete-layout validation are owned by the caller.

CREATE TABLE IF NOT EXISTS monitor_feedback_outcomes (
			monitor_namespace TEXT NOT NULL,
			command_event_id  TEXT NOT NULL,
			monitor_name      TEXT NOT NULL,
			repo              TEXT NOT NULL,
			pr_number         INTEGER NOT NULL,
			head_sha          TEXT NOT NULL,
			state             TEXT NOT NULL CHECK(state IN ('reserved', 'started', 'withdrawn', 'no_change', 'failed', 'succeeded')),
			admitted_ids      TEXT NOT NULL,
			targets           TEXT NOT NULL DEFAULT '[]',
			task_name         TEXT NOT NULL DEFAULT '',
			reason            TEXT NOT NULL DEFAULT '',
			created_at        TIMESTAMP NOT NULL,
			updated_at        TIMESTAMP NOT NULL,
			started_at        TIMESTAMP,
			completed_at      TIMESTAMP,
			PRIMARY KEY(monitor_namespace, command_event_id)
		);

CREATE INDEX IF NOT EXISTS idx_monitor_feedback_outcomes_pull_request
			ON monitor_feedback_outcomes(monitor_namespace, monitor_name, repo, pr_number);

WITH commands AS (
	SELECT monitor_namespace, command_event_id, MIN(monitor_name) AS monitor_name, MIN(repo) AS repo,
	       MIN(pr_number) AS pr_number, MIN(head_sha) AS head_sha, MIN(repair_job_id) AS repair_job_id,
	       MIN(created_at) AS created_at
	FROM monitor_feedback_attempts
	GROUP BY monitor_namespace, command_event_id
), recovered AS (
	SELECT commands.*,
	       (SELECT json_group_array(feedback_id) FROM (
	         SELECT feedback_id FROM monitor_feedback_attempts AS attempts
	         WHERE attempts.monitor_namespace = commands.monitor_namespace AND attempts.command_event_id = commands.command_event_id
	         ORDER BY feedback_id)) AS admitted_ids,
	       COALESCE(job.phase IS NOT NULL AND job.phase NOT IN ('', 'queued'), 0) AS settled,
	       COALESCE(job.phase IN ('', 'queued') AND job.task_name <> '' AND job.last_error <> 'repair_task_create_failed', 0) AS may_have_run,
	       job.phase AS job_phase,
	       COALESCE(job.pushed_sha, '') AS pushed_sha,
	       COALESCE(job.task_name, '') AS task_name,
	       COALESCE(job.last_error, '') AS job_error,
	       job.created_at AS job_created_at,
	       job.completed_at AS job_completed_at,
	       COALESCE(command.status = 'processed', 0) AS finished,
	       command.processed_at AS processed_at,
	       (SELECT action.blocked_reason FROM work_actions AS action
	        WHERE action.monitor_namespace = commands.monitor_namespace AND action.command_event_id = commands.command_event_id
	          AND action.status = 'blocked' AND action.blocked_reason IN ('review_feedback_withdrawn', 'review_feedback_needs_person')
	        LIMIT 1) AS withdrawn_reason,
	       (SELECT COALESCE(NULLIF(action.blocked_reason, ''), NULLIF(action.error, ''), action.status) FROM work_actions AS action
	        WHERE action.monitor_namespace = commands.monitor_namespace AND action.command_event_id = commands.command_event_id
	        LIMIT 1) AS action_reason
	FROM commands
	LEFT JOIN command_events AS command ON command.monitor_namespace = commands.monitor_namespace AND command.id = commands.command_event_id
	LEFT JOIN repair_jobs AS job ON job.monitor_namespace = commands.monitor_namespace AND job.id = commands.repair_job_id
), classified AS (
	SELECT recovered.*,
	       settled OR (finished AND may_have_run) AS ran,
	       CASE
	         WHEN settled AND job_phase = 'succeeded' AND pushed_sha = '' THEN 'no_change'
	         WHEN settled AND job_phase = 'succeeded' THEN 'succeeded'
	         WHEN settled OR (finished AND may_have_run) THEN 'failed'
	         WHEN finished AND withdrawn_reason IS NOT NULL THEN 'withdrawn'
	         WHEN finished THEN 'failed'
	         ELSE 'reserved'
	       END AS state
	FROM recovered
)
INSERT INTO monitor_feedback_outcomes
	(monitor_namespace, command_event_id, monitor_name, repo, pr_number, head_sha, state, admitted_ids, targets,
	 task_name, reason, created_at, updated_at, started_at, completed_at)
SELECT monitor_namespace, command_event_id, monitor_name, repo, pr_number, head_sha, state, admitted_ids,
       CASE WHEN ran THEN admitted_ids ELSE '[]' END,
       CASE WHEN ran THEN task_name ELSE '' END,
       CASE
         WHEN state = 'reserved' THEN ''
         WHEN ran THEN COALESCE(NULLIF(job_error, ''), 'recovered_from_attempt_ledger')
         WHEN state = 'withdrawn' THEN withdrawn_reason
         ELSE COALESCE(NULLIF(action_reason, ''), 'recovered_from_attempt_ledger')
       END,
       created_at, created_at,
       CASE WHEN ran THEN COALESCE(job_created_at, created_at) END,
       CASE WHEN state = 'reserved' THEN NULL ELSE COALESCE(job_completed_at, processed_at, created_at) END
FROM classified;
