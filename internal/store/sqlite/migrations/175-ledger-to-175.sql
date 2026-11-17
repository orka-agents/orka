-- SQLite layout repair at schema version 8: 175-ledger-to-175.
-- An unreleased build wrote version 8 with the feedback attempt ledger but
-- without the feedback outcome table. This adds the table and recovers one
-- outcome per admitted feedback repair command from what that build kept:
-- a command that has not finished stays reserved, which reserves every
-- admitted feedback ID; a finished command whose repair job settled from its
-- Task is succeeded or failed and charges every admitted ID, since the
-- targets its Task froze were not recorded; any other finished command never
-- ran a Task and charges nothing.
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
	       CASE
	         WHEN command.status IS NULL OR command.status <> 'processed' THEN 'reserved'
	         WHEN job.phase = 'succeeded' THEN 'succeeded'
	         ELSE 'failed'
	       END AS state,
	       command.status IS NOT NULL AND command.status = 'processed' AND job.phase IS NOT NULL AND job.phase <> 'queued' AS ran,
	       COALESCE(job.task_name, '') AS task_name,
	       job.created_at AS job_created_at,
	       command.processed_at AS processed_at
	FROM commands
	LEFT JOIN command_events AS command ON command.monitor_namespace = commands.monitor_namespace AND command.id = commands.command_event_id
	LEFT JOIN repair_jobs AS job ON job.monitor_namespace = commands.monitor_namespace AND job.id = commands.repair_job_id
)
INSERT INTO monitor_feedback_outcomes
	(monitor_namespace, command_event_id, monitor_name, repo, pr_number, head_sha, state, admitted_ids, targets,
	 task_name, reason, created_at, updated_at, started_at, completed_at)
SELECT monitor_namespace, command_event_id, monitor_name, repo, pr_number, head_sha,
       CASE WHEN state = 'succeeded' AND NOT ran THEN 'failed' ELSE state END,
       admitted_ids,
       CASE WHEN ran THEN admitted_ids ELSE '[]' END,
       CASE WHEN ran THEN task_name ELSE '' END,
       CASE WHEN state = 'reserved' THEN '' ELSE 'recovered_from_attempt_ledger' END,
       created_at, created_at,
       CASE WHEN ran THEN COALESCE(job_created_at, created_at) END,
       CASE WHEN state = 'reserved' THEN NULL ELSE COALESCE(processed_at, created_at) END
FROM recovered;
