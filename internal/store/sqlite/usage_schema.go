package sqlite

func usageSchema() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS usage_work_requests (
			namespace TEXT NOT NULL, id TEXT NOT NULL, started_at INTEGER NOT NULL,
			data TEXT NOT NULL, PRIMARY KEY (namespace, id)
		)`,
		`CREATE TABLE IF NOT EXISTS usage_tasks (
			namespace TEXT NOT NULL, task_uid TEXT NOT NULL, task_name TEXT NOT NULL,
			started_at INTEGER NOT NULL, data TEXT NOT NULL, PRIMARY KEY (namespace, task_uid)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_usage_work_started ON usage_work_requests(namespace, started_at)`,
		`CREATE INDEX IF NOT EXISTS idx_usage_tasks_started ON usage_tasks(namespace, started_at)`,
		`CREATE INDEX IF NOT EXISTS idx_usage_tasks_work ON usage_tasks(namespace, json_extract(data, '$.workID'))`,
		`CREATE INDEX IF NOT EXISTS idx_usage_tasks_pr ON usage_tasks(namespace, json_extract(data, '$.repository'), json_extract(data, '$.prNumber'))`,
		`CREATE INDEX IF NOT EXISTS idx_usage_tasks_name ON usage_tasks(namespace, task_name)`,
		`CREATE TABLE IF NOT EXISTS usage_observations (
			recorded_seq INTEGER PRIMARY KEY AUTOINCREMENT,
			namespace TEXT NOT NULL, id TEXT NOT NULL, task_uid TEXT NOT NULL,
			counter_id TEXT NOT NULL, observed_at INTEGER NOT NULL, data TEXT NOT NULL,
			UNIQUE (namespace, id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_usage_observations_time ON usage_observations(namespace, observed_at)`,
		`CREATE INDEX IF NOT EXISTS idx_usage_observations_task ON usage_observations(namespace, task_uid)`,
		`CREATE INDEX IF NOT EXISTS idx_usage_observations_counter ON usage_observations(namespace, counter_id)`,
		`CREATE TABLE IF NOT EXISTS usage_pr_links (
			namespace TEXT NOT NULL, work_id TEXT NOT NULL, repository TEXT NOT NULL,
			number INTEGER NOT NULL, data TEXT NOT NULL,
			PRIMARY KEY (namespace, work_id, repository, number)
		)`,
		`CREATE TABLE IF NOT EXISTS usage_pull_requests (
			namespace TEXT NOT NULL, namespace_uid TEXT NOT NULL, repository TEXT NOT NULL, number INTEGER NOT NULL,
			observed_at INTEGER NOT NULL, data TEXT NOT NULL,
			PRIMARY KEY (namespace, namespace_uid, repository, number, observed_at)
		)`,
		`CREATE TABLE IF NOT EXISTS usage_retention (
			id INTEGER PRIMARY KEY CHECK (id = 1), retained_since INTEGER NOT NULL
		)`,
	}
}
