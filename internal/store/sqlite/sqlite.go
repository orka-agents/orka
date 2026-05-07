package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	_ "modernc.org/sqlite" // SQLite driver registration
)

var (
	dbSizeBytes = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "orka_store_db_size_bytes",
			Help: "Size of the SQLite database file in bytes",
		},
	)
)

func init() {
	metrics.Registry.MustRegister(dbSizeBytes)
}

// NewDB opens a SQLite database with recommended pragmas for WAL mode,
// busy timeout, synchronous mode, and foreign keys.
func NewDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}

	// Single writer — SQLite only supports one concurrent writer.
	// WAL mode allows concurrent reads alongside the single writer.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0) // keep connection alive

	// Set pragmas per-connection. These are not persistent in SQLite and
	// must be set on each connection. With MaxOpenConns(1), we have exactly one.
	pragmas := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA foreign_keys=ON",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			db.Close() //nolint:errcheck
			return nil, fmt.Errorf("failed to set pragma %q: %w", p, err)
		}
	}

	if err := migrate(db); err != nil {
		db.Close() //nolint:errcheck
		return nil, fmt.Errorf("failed to run migrations: %w", err)
	}

	return db, nil
}

func migrate(db *sql.DB) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS results (
			namespace  TEXT NOT NULL,
			task_name  TEXT NOT NULL,
			data       BLOB NOT NULL,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (namespace, task_name)
		)`,
		`CREATE TABLE IF NOT EXISTS sessions (
			namespace     TEXT NOT NULL,
			name          TEXT NOT NULL,
			session_type  TEXT NOT NULL DEFAULT 'task',
			active_task   TEXT NOT NULL DEFAULT '',
			message_count INTEGER NOT NULL DEFAULT 0,
			input_tokens  INTEGER NOT NULL DEFAULT 0,
			output_tokens INTEGER NOT NULL DEFAULT 0,
			cancelled     BOOLEAN NOT NULL DEFAULT FALSE,
			created_at    TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at    TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (namespace, name)
		)`,
		`CREATE TABLE IF NOT EXISTS session_messages (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			namespace    TEXT NOT NULL,
			session_name TEXT NOT NULL,
			role         TEXT NOT NULL,
			content      TEXT NOT NULL DEFAULT '',
			name         TEXT,
			input        TEXT,
			tool_calls   TEXT,
			tool_call_id TEXT,
			created_at   TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (namespace, session_name) REFERENCES sessions(namespace, name) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS plan_states (
			namespace     TEXT NOT NULL,
			task_name     TEXT NOT NULL,
			iteration     INTEGER NOT NULL DEFAULT 0,
			summary       TEXT NOT NULL DEFAULT '',
			progress_pct  INTEGER NOT NULL DEFAULT 0,
			goal_complete BOOLEAN NOT NULL DEFAULT FALSE,
			plan_document TEXT NOT NULL DEFAULT '',
			created_at    TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at    TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (namespace, task_name)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_session_messages_order ON session_messages(namespace, session_name, id)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_namespace ON sessions(namespace)`,
		`CREATE INDEX IF NOT EXISTS idx_results_namespace ON results(namespace)`,
		`CREATE INDEX IF NOT EXISTS idx_plan_states_namespace ON plan_states(namespace)`,
		`CREATE TABLE IF NOT EXISTS memories (
			id               TEXT PRIMARY KEY,
			namespace        TEXT NOT NULL,
			session_name     TEXT NOT NULL DEFAULT '',
			agent_name       TEXT NOT NULL DEFAULT '',
			task_name        TEXT NOT NULL DEFAULT '',
			parent_task      TEXT NOT NULL DEFAULT '',
			source           TEXT NOT NULL DEFAULT '',
			content          TEXT NOT NULL,
			tags_json        TEXT NOT NULL DEFAULT '[]',
			disabled         BOOLEAN NOT NULL DEFAULT FALSE,
			deleted          BOOLEAN NOT NULL DEFAULT FALSE,
			created_at       TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at       TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			last_recalled_at TIMESTAMP,
			recalled_count   INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_memories_recall
			ON memories(namespace, deleted, disabled, updated_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_memories_task ON memories(namespace, task_name)`,
		`CREATE INDEX IF NOT EXISTS idx_memories_agent ON memories(namespace, agent_name)`,
		`CREATE TABLE IF NOT EXISTS memory_proposals (
			id          TEXT PRIMARY KEY,
			namespace   TEXT NOT NULL,
			task_name   TEXT NOT NULL DEFAULT '',
			agent_name  TEXT NOT NULL DEFAULT '',
			type        TEXT NOT NULL,
			skill_name  TEXT NOT NULL DEFAULT '',
			title       TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			content     TEXT NOT NULL DEFAULT '',
			patch       TEXT NOT NULL DEFAULT '',
			status      TEXT NOT NULL DEFAULT 'pending',
			reviewer    TEXT NOT NULL DEFAULT '',
			review_note TEXT NOT NULL DEFAULT '',
			created_at  TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at  TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			reviewed_at TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_memory_proposals_status
			ON memory_proposals(namespace, status, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_memory_proposals_task
			ON memory_proposals(namespace, task_name, created_at DESC)`,
		`CREATE TABLE IF NOT EXISTS messages (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			namespace   TEXT NOT NULL,
			from_task   TEXT NOT NULL,
			to_task     TEXT NOT NULL,
			parent_task TEXT NOT NULL,
			content     TEXT NOT NULL,
			read        BOOLEAN NOT NULL DEFAULT FALSE,
			created_at  TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_recipient ON messages(namespace, to_task, read)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_parent ON messages(namespace, parent_task)`,
		`CREATE TABLE IF NOT EXISTS artifacts (
			namespace    TEXT NOT NULL,
			task_name    TEXT NOT NULL,
			filename     TEXT NOT NULL,
			content_type TEXT NOT NULL DEFAULT 'application/octet-stream',
			size         INTEGER NOT NULL,
			data         BLOB NOT NULL,
			created_at   TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (namespace, task_name, filename)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_artifacts_task ON artifacts(namespace, task_name)`,
		`CREATE TABLE IF NOT EXISTS security_scan_runs (
			id              TEXT PRIMARY KEY,
			namespace       TEXT NOT NULL,
			repository_scan TEXT NOT NULL,
			task_name       TEXT NOT NULL,
			mode            TEXT NOT NULL,
			phase           TEXT NOT NULL,
			base_commit     TEXT NOT NULL DEFAULT '',
			head_commit     TEXT NOT NULL DEFAULT '',
			commit_count    INTEGER NOT NULL DEFAULT 0,
			summary         TEXT NOT NULL DEFAULT '',
			error_message   TEXT NOT NULL DEFAULT '',
			started_at      TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			completed_at    TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_security_scan_runs_repo
			ON security_scan_runs(namespace, repository_scan, started_at DESC)`,
		`CREATE TABLE IF NOT EXISTS security_threat_models (
			namespace         TEXT NOT NULL,
			repository_scan   TEXT NOT NULL,
			version           INTEGER NOT NULL,
			content           TEXT NOT NULL,
			source            TEXT NOT NULL,
			generated_by_scan TEXT NOT NULL DEFAULT '',
			created_at        TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at        TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (namespace, repository_scan, version)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_security_threat_models_latest
			ON security_threat_models(namespace, repository_scan, version DESC)`,
		`CREATE TABLE IF NOT EXISTS security_findings (
			id                TEXT PRIMARY KEY,
			namespace         TEXT NOT NULL,
			repository_scan   TEXT NOT NULL,
			scan_run_id       TEXT NOT NULL,
			fingerprint       TEXT NOT NULL,
			title             TEXT NOT NULL,
			summary           TEXT NOT NULL,
			severity          TEXT NOT NULL,
			confidence        TEXT NOT NULL,
			validation_status TEXT NOT NULL,
			state             TEXT NOT NULL,
			file_path         TEXT NOT NULL DEFAULT '',
			line              INTEGER NOT NULL DEFAULT 0,
			commit_sha        TEXT NOT NULL DEFAULT '',
			root_cause        TEXT NOT NULL DEFAULT '',
			remediation       TEXT NOT NULL DEFAULT '',
			suggested_action  TEXT NOT NULL DEFAULT '',
			evidence_json     TEXT NOT NULL DEFAULT '',
			validation_json   TEXT NOT NULL DEFAULT '',
			patch_proposal_id TEXT NOT NULL DEFAULT '',
			pr_number         INTEGER,
			pr_url            TEXT NOT NULL DEFAULT '',
			created_at        TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at        TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(namespace, repository_scan, fingerprint)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_security_findings_repo
			ON security_findings(namespace, repository_scan, severity, validation_status, state, updated_at DESC)`,
		`CREATE TABLE IF NOT EXISTS security_patch_proposals (
			id                TEXT PRIMARY KEY,
			namespace         TEXT NOT NULL,
			repository_scan   TEXT NOT NULL,
			finding_id        TEXT NOT NULL,
			task_name         TEXT NOT NULL,
			branch            TEXT NOT NULL,
			diff_artifact     TEXT NOT NULL DEFAULT '',
			summary_artifact  TEXT NOT NULL DEFAULT '',
			status            TEXT NOT NULL,
			pr_number         INTEGER,
			pr_url            TEXT NOT NULL DEFAULT '',
			created_at        TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at        TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_security_patch_proposals_finding
			ON security_patch_proposals(namespace, finding_id, created_at DESC)`,
	}

	for _, stmt := range statements {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("migration failed: %w", err)
		}
	}

	return nil
}

// Store implements both store.ResultStore and store.SessionStore.
type Store struct {
	db     *sql.DB
	dbPath string
}

// NewStore creates a new Store backed by the given SQLite database.
// The dbPath is the filesystem path to the database file (used for metrics and logging).
func NewStore(db *sql.DB, dbPath string) *Store {
	return &Store{db: db, dbPath: dbPath}
}

// Start runs background maintenance and blocks until ctx is cancelled,
// then optimizes and closes the database.
// It satisfies the controller-runtime manager.Runnable interface.
func (s *Store) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("sqlite-store")

	logger.Info("SQLite store is configured — ensure a PersistentVolume is mounted at the store path for data durability", "path", s.dbPath)

	// Update DB size metric periodically
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	// Record initial size
	s.updateDBSizeMetric()

	for {
		select {
		case <-ctx.Done():
			s.db.Exec("PRAGMA optimize") //nolint:errcheck
			return s.db.Close()
		case <-ticker.C:
			s.updateDBSizeMetric()
		}
	}
}

// updateDBSizeMetric reads the database file size and updates the gauge.
func (s *Store) updateDBSizeMetric() {
	if s.dbPath == "" || s.dbPath == ":memory:" {
		return
	}
	info, err := os.Stat(s.dbPath)
	if err != nil {
		return
	}
	dbSizeBytes.Set(float64(info.Size()))
}

// HealthCheck verifies the database is reachable by executing a simple query.
func (s *Store) HealthCheck(ctx context.Context) error {
	var n int
	return s.db.QueryRowContext(ctx, "SELECT 1").Scan(&n)
}
