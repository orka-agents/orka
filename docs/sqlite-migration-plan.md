# SQLite Migration Plan: ConfigMap → Embedded Database

## Overview

Replace ConfigMap-based storage for **task results** and **session transcripts** with an in-process embedded SQLite database (via `modernc.org/sqlite`). Skills remain as ConfigMaps (small, declarative, read-only). ConfigMap storage backend is **not** being kept — SQLite is the sole implementation.

### Goals

- Remove the 1MB ConfigMap size limit for results and sessions
- Enable efficient querying, pagination, and filtering of sessions
- Maintain zero external dependencies (SQLite is embedded, pure Go, no CGO)
- Fix existing bugs (AI worker missing truncation, no owner refs on worker-created CMs, non-atomic session locking)
- Simplify result/session lifecycle (no orphaned ConfigMaps, no finalizer cleanup for storage)

### Non-Goals

- Replacing ConfigMaps for skills (they stay as-is)
- Adding external database support (PostgreSQL, etc.) — future work behind the `Store` interface
- Distributed/multi-replica controllers (SQLite is single-writer; HA requires a different backend)

---

## Current State

### What Uses ConfigMaps Today

| Use Case | ConfigMap Name | Data Key | Labels | Created By |
|---|---|---|---|---|
| Task results | `{taskName}-result` | `result` | `orka.ai/result=true` | Workers (AI, Claude, Copilot) or Controller (container tasks via pod logs) |
| Task sessions | `session-{sessionName}` | `transcript.jsonl` | `orka.ai/session=true` | SessionManager |
| Chat sessions | `chat-session-{sessionID}` | `transcript.jsonl` | `orka.ai/session=true`, `orka.ai/session-type=chat` | Chat handler |

### Files That Touch ConfigMaps (Results & Sessions)

| Component | File | Functions |
|---|---|---|
| Controller | `internal/controller/task_controller.go` | `collectResult()`, `handleDeletion()`, `handleRunning()`, `completeTask()` |
| Session Manager | `internal/controller/session_manager.go` | `createSession()`, `IsLocked()`, `AcquireLock()`, `ReleaseLock()`, `AppendMessages()`, `LoadTranscript()`, `GetSession()`, `DeleteSession()`, `ListSessions()` |
| API Handlers | `internal/api/handlers.go` | `GetTaskResult()`, `ListSessions()`, `GetSession()`, `DeleteSession()` |
| Chat Handler | `internal/api/chat.go` | `loadChatSession()`, `saveChatSession()`, `HandleCancelChat()` |
| Chat Tools | `internal/api/chat_tool_executor.go` | `executeFetchTaskOutput()` |
| Coordination | `internal/tools/wait_for_tasks.go` | Result reading in `Execute()` |
| AI Worker | `workers/ai/main.go` | `writeResult()`, `loadSessionContext()` |
| Claude Worker | `workers/agent/claude/main.go` | `writeResult()` |
| Copilot Worker | `workers/agent/copilot/main.go` | `writeResult()` |
| Job Builder | `internal/controller/job_builder.go` | Env var `ORKA_RESULT_CONFIGMAP`, session volume mount |

### Known Issues with ConfigMap Storage

1. **1MB hard limit** — results and sessions silently truncated or fail
2. **AI worker has no truncation guard** — can fail on large results
3. **Worker-created ConfigMaps lack owner references** — leaked if finalizer fails
4. **Session locking is not atomic** — read-then-update without optimistic concurrency
5. **Malformed JSONL lines silently dropped** — no error reporting
6. **General worker relies on pod logs** — result lost if pod evicted before log collection

---

## Architecture

### Storage Interface

Introduce a `Store` interface in a new package `internal/store/`:

```go
package store

import (
    "context"
    "time"
)

// ResultStore handles task result persistence.
type ResultStore interface {
    SaveResult(ctx context.Context, namespace, taskName string, data []byte) error
    GetResult(ctx context.Context, namespace, taskName string) ([]byte, error)
    DeleteResult(ctx context.Context, namespace, taskName string) error
}

// SessionStore handles session transcript persistence.
type SessionStore interface {
    CreateSession(ctx context.Context, session *SessionRecord) error
    GetSession(ctx context.Context, namespace, name string) (*SessionRecord, error)
    ListSessions(ctx context.Context, namespace string) ([]SessionMetadata, error)
    DeleteSession(ctx context.Context, namespace, name string) error

    // Locking
    AcquireLock(ctx context.Context, namespace, name, taskName string) error
    ReleaseLock(ctx context.Context, namespace, name, taskName string) error
    IsLocked(ctx context.Context, namespace, name, currentTask string) (bool, error)

    // Transcript
    AppendMessages(ctx context.Context, namespace, name string, messages []SessionMessage) error
    LoadTranscript(ctx context.Context, namespace, name string, maxMessages int) ([]SessionMessage, error)
}

// SessionRecord represents a full session.
type SessionRecord struct {
    Namespace    string
    Name         string
    SessionType  string // "task" or "chat"
    ActiveTask   string
    MessageCount int
    InputTokens  int
    OutputTokens int
    Cancelled    bool
    CreatedAt    time.Time
    UpdatedAt    time.Time
    Messages     []SessionMessage
}

// SessionMetadata is the lightweight listing representation.
type SessionMetadata struct {
    Name         string
    SessionType  string
    MessageCount int
    InputTokens  int
    OutputTokens int
    CreatedAt    time.Time
    UpdatedAt    time.Time
    ActiveTask   string
}

// SessionMessage is a single transcript entry.
type SessionMessage struct {
    Role       string         `json:"role"`
    Content    string         `json:"content"`
    Name       string         `json:"name,omitempty"`
    Input      map[string]any `json:"input,omitempty"`
    ToolCalls  any            `json:"toolCalls,omitempty"`
    ToolCallID string         `json:"toolCallID,omitempty"`
    Timestamp  time.Time      `json:"ts"`
}
```

### Package Layout

```
internal/store/
├── store.go              # Interface definitions (ResultStore, SessionStore)
├── types.go              # Shared types (SessionRecord, SessionMessage, etc.)
├── sqlite/
│   ├── sqlite.go         # DB initialization, migrations, WAL mode, connection config
│   ├── result_store.go   # SQLite ResultStore implementation
│   ├── session_store.go  # SQLite SessionStore implementation
│   └── *_test.go         # Tests using :memory: database
```

No `configmap/` backend. The interfaces exist for future backends (e.g., PostgreSQL) but only SQLite is implemented.

### SQLite Schema

```sql
-- Task results
CREATE TABLE IF NOT EXISTS results (
    namespace  TEXT NOT NULL,
    task_name  TEXT NOT NULL,
    data       BLOB NOT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (namespace, task_name)
);

-- Sessions
CREATE TABLE IF NOT EXISTS sessions (
    namespace     TEXT NOT NULL,
    name          TEXT NOT NULL,
    session_type  TEXT NOT NULL DEFAULT 'task',  -- 'task' or 'chat'
    active_task   TEXT NOT NULL DEFAULT '',       -- lock holder (empty = unlocked)
    message_count INTEGER NOT NULL DEFAULT 0,
    input_tokens  INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    cancelled     BOOLEAN NOT NULL DEFAULT FALSE,
    created_at    TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at    TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (namespace, name)
);

-- Session messages (normalized, not JSONL blob)
CREATE TABLE IF NOT EXISTS session_messages (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    namespace    TEXT NOT NULL,
    session_name TEXT NOT NULL,
    role         TEXT NOT NULL,
    content      TEXT NOT NULL DEFAULT '',  -- empty for tool-call-only messages
    name         TEXT,
    input        TEXT,           -- JSON-encoded map
    tool_calls   TEXT,           -- JSON-encoded
    tool_call_id TEXT,
    created_at   TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (namespace, session_name) REFERENCES sessions(namespace, name) ON DELETE CASCADE
);

-- Single composite index covers both lookup and ordering
CREATE INDEX idx_session_messages_order ON session_messages(namespace, session_name, id);
CREATE INDEX idx_sessions_namespace ON sessions(namespace);
CREATE INDEX idx_results_namespace ON results(namespace);
```

### SQLite Connection Configuration

Required pragmas set at connection open in `NewDB()` — these are not optional:

```go
func NewDB(path string) (*sql.DB, error) {
    db, err := sql.Open("sqlite", path+"?_journal_mode=WAL&_busy_timeout=5000&_synchronous=NORMAL&_foreign_keys=ON")
    if err != nil {
        return nil, err
    }

    // Single writer — SQLite only supports one concurrent writer.
    // WAL mode allows concurrent reads alongside the single writer.
    db.SetMaxOpenConns(1)
    db.SetMaxIdleConns(1)
    db.SetConnMaxLifetime(0) // keep connection alive

    // Run migrations
    if err := migrate(db); err != nil {
        db.Close()
        return nil, fmt.Errorf("failed to run migrations: %w", err)
    }

    return db, nil
}
```

**Why each pragma matters:**
- `journal_mode=WAL` — allows concurrent reads while writing; without it, reconciler + API goroutines get `SQLITE_BUSY`
- `busy_timeout=5000` — wait 5s instead of failing immediately on lock contention
- `synchronous=NORMAL` — committed transactions survive process crashes (not OS crashes); acceptable for Orka's throughput
- `foreign_keys=ON` — **not persistent in SQLite**, must be set per connection; required for `ON DELETE CASCADE` on `session_messages`

### Graceful Shutdown

Register the SQLite store as a `manager.Runnable` via `mgr.Add()`:

```go
func (s *Store) Start(ctx context.Context) error {
    <-ctx.Done()
    s.db.Exec("PRAGMA optimize")
    return s.db.Close()
}
```

This ensures WAL is checkpointed and the connection is closed cleanly on SIGTERM.

### CRD Change: `ResultReference`

Current `ResultReference` in `task_types.go` is ConfigMap-specific:

```go
// BEFORE
type ResultReference struct {
    ConfigMapName string `json:"configMapName"`
    Key           string `json:"key"`
}
```

Replace with a storage-agnostic signal:

```go
// AFTER
type ResultReference struct {
    Available bool `json:"available"`
}
```

The controller sets `Available: true` when a result is stored. Consumers call `ResultStore.GetResult(namespace, taskName)` to retrieve it. This requires:
- `make manifests generate` after editing `task_types.go`
- Update all code referencing `ResultRef.ConfigMapName` / `ResultRef.Key`
- This is a **breaking CRD schema change** — document in release notes

### Worker Communication: Internal HTTP Endpoints

Workers submit results and fetch session transcripts via the controller's API server. All endpoints use path-based routing on the **existing API port** (`:8080`) under the `/internal/` prefix.

#### Result Submission

```
POST /internal/v1/results/{namespace}/{taskName}
Content-Type: application/octet-stream
Body: raw result bytes
```

- Auth: ServiceAccount token (reuse existing middleware)
- Authorization: Verify caller SA namespace matches `{namespace}` in URL (prevent cross-namespace writes)
- Body size limit: 10MB
- Handler: `ResultStore.SaveResult()` — idempotent via `INSERT OR REPLACE`
- Workers retry with exponential backoff (1s, 2s, 4s — 3 attempts) on failure
- Worker exits non-zero if all retries fail → Job fails → controller doesn't mark task Succeeded

#### Session Transcript Delivery

```
GET /internal/v1/sessions/{namespace}/{name}/transcript
```

- Auth: ServiceAccount token
- Returns: JSONL transcript (same format workers currently read from `/session/transcript.jsonl`)
- Consumed by an **init container** in the worker pod that writes to a shared `emptyDir` at `/session/transcript.jsonl`
- This preserves existing worker `loadSessionContext()` code — zero changes to AI/Claude/Copilot workers for session reading

#### `wait_for_tasks` Coordination Tool

`wait_for_tasks` runs inside worker pods and currently reads result ConfigMaps via the K8s API. Post-migration:

- Inject `ORKA_CONTROLLER_URL` env var into worker pods via `job_builder.go`
- `wait_for_tasks` calls `GET /api/v1/tasks/{id}/result` (existing public endpoint) instead of reading ConfigMaps
- The public result endpoint calls `ResultStore.GetResult()` internally

---

## Implementation Steps

### Step 1: Store Package & SQLite Implementation

- Create `internal/store/store.go` with `ResultStore` and `SessionStore` interfaces
- Create `internal/store/types.go` with shared types
- Add `modernc.org/sqlite` dependency: `go get modernc.org/sqlite`
- Implement `internal/store/sqlite/sqlite.go` with `NewDB()`, migrations, pragma config, graceful shutdown
- Implement `internal/store/sqlite/result_store.go` (`SaveResult`, `GetResult`, `DeleteResult`)
- Implement `internal/store/sqlite/session_store.go` (all session operations, atomic locking)
- Full unit tests with `:memory:` database

**Note on binary size:** `modernc.org/sqlite` adds ~15-25MB to the controller binary (pure-Go transpiled C). Build time increases ~60-90s for cold builds. No Dockerfile changes needed — `CGO_ENABLED=0` is compatible.

### Step 2: CRD Schema Change

- Update `ResultReference` in `api/v1alpha1/task_types.go` to `Available bool`
- Run `make manifests generate`
- Update all code referencing `ResultRef.ConfigMapName` / `ResultRef.Key`

### Step 3: Internal HTTP Endpoints

- Add `POST /internal/v1/results/{namespace}/{taskName}` — result submission
- Add `GET /internal/v1/sessions/{namespace}/{name}/transcript` — session transcript
- Auth middleware with namespace scoping: verify caller SA namespace matches URL namespace
- Body size limit on result submission (10MB)
- Serve on existing API port (`:8080`) with `/internal/` path prefix — avoids port conflicts (healthPort is `:8082`)

### Step 4: Update Workers

- Replace `writeResult()` in AI, Claude, Copilot workers with HTTP POST to controller
- Add retry with exponential backoff (1s, 2s, 4s — 3 attempts max)
- Exit non-zero on all retries exhausted
- Add env vars: `ORKA_RESULT_ENDPOINT`, `ORKA_CONTROLLER_URL`
- Update `wait_for_tasks` tool to use HTTP GET instead of ConfigMap read
- Remove Kubernetes client ConfigMap creation code from workers
- General worker: unchanged (controller reads pod logs via `collectResult()`)

### Step 5: Update Controller

- Inject `ResultStore` and `SessionStore` into `TaskReconciler`
- Update `collectResult()`: for container tasks, call `ResultStore.SaveResult()` instead of creating a ConfigMap
- Update `handleDeletion()`: call `ResultStore.DeleteResult()` instead of deleting ConfigMap
- Update `handleRunning()`: call `ResultStore.GetResult()` for child task results
- Update `completeTask()`: call `SessionStore.AppendMessages()` instead of ConfigMap-based session append
- Inject `SessionStore` into `SessionManager` (keep `SessionManager` as orchestration layer — acquire→job→append→release lifecycle)
- Update API handlers (`GetTaskResult`, `ListSessions`, `GetSession`, `DeleteSession`) to use store interfaces
- Update chat handler (`loadChatSession`, `saveChatSession`, `HandleCancelChat`) to use `SessionStore`
- Update `executeFetchTaskOutput` to use `ResultStore.GetResult()`

### Step 6: Update Job Builder

- Remove `ORKA_RESULT_CONFIGMAP` env var, add `ORKA_RESULT_ENDPOINT`
- Add `ORKA_CONTROLLER_URL` for `wait_for_tasks` coordination
- Remove session ConfigMap volume mount
- Add init container that fetches transcript via `GET /internal/v1/sessions/.../transcript` and writes to shared `emptyDir` at `/session/transcript.jsonl`
- Shared `emptyDir` volume between init container and main worker container

### Step 7: Update Deployment Manifests

**Kustomize (`config/manager/manager.yaml`):**
- Add `emptyDir: {}` volume named `store` mounted at `/data`
- Controller runs with `readOnlyRootFilesystem: true` — the emptyDir provides the writable path
- Add `--store-backend=sqlite` and `--store-path=/data/orka.db` args
- Set `fsGroup: 65532` in pod security context (matches distroless nonroot UID)

**Helm chart:**
- Add `emptyDir` volume mount at `/data` (default)
- When `store.sqlite.persistence.enabled: true`, create PVC instead of emptyDir
- When PVC enabled, set `strategy.type: Recreate` (RWO PVC deadlocks with RollingUpdate)
- Add `--store-backend` and `--store-path` to deployment args

### Step 8: Update RBAC

- Remove ConfigMap `create/update/patch` from `config/rbac/worker_role.yaml`
- Workers retain ConfigMap `get/list/watch` (still needed for skills, secrets)
- Workers retain Task `create/get/list/watch` (coordination)

### Step 9: Observability

- Add `orka_store_db_size_bytes` gauge metric (updated every 60s via `os.Stat` on DB file) — most important capacity signal
- Add `WARN`-level log on controller startup when no PVC is configured: `"store is ephemeral — data will be lost on pod restart; set store.sqlite.persistence.enabled=true for durability"`
- Add `SELECT 1` health check in readiness probe

### Step 10: Migration Tool

- Add `orka migrate` CLI command to `cmd/cli/`
- Reads existing result ConfigMaps (`orka.ai/result=true`) and inserts into SQLite
- Reads existing session ConfigMaps (`orka.ai/session=true`) and inserts into SQLite
- Idempotent (`INSERT OR IGNORE`)
- Only relevant for PVC-backed deployments (ephemeral storage has nothing to persist into)
- Optionally deletes migrated ConfigMaps (`--cleanup`)

### Step 11: Tests

Test files requiring modification (~15-25 files):

| Category | Changes |
|---|---|
| Controller tests | Replace ConfigMap fixtures with `ResultStore.SaveResult()` / `SessionStore` calls; inject `:memory:` SQLite |
| API handler tests | Replace ConfigMap creation via fake client with store interface calls |
| Worker tests | Replace K8s client mocks with `httptest.NewServer` for result POST |
| Session manager tests | Rewrite to test against `SessionStore` interface |
| New store tests | Interface-conformance tests for SQLite implementation |
| Integration tests | Full task lifecycle with SQLite backend |

### Step 12: Documentation

- Release notes: breaking CRD change (`ResultReference`), ConfigMap storage removed
- Upgrade runbook: drain pending tasks before upgrading (`kubectl get tasks --field-selector status.phase=Running`), or wait for in-flight tasks to complete
- Helm NOTES.txt: storage type warning for ephemeral default
- Backup guide: PVC users can use VolumeSnapshot; `orka backup` CLI available for manual export

---

## Controller Flag Changes

```go
// New flags in cmd/main.go
flag.StringVar(&storeBackend, "store-backend", "sqlite", "Storage backend (sqlite)")
flag.StringVar(&storePath, "store-path", "/data/orka.db", "Path to SQLite database file")
```

The `--store-backend` flag defaults to `sqlite` (only implementation). It exists so a future PostgreSQL backend can be added without restructuring flags. Unknown values fail fast at startup.

## Helm Chart Changes

```yaml
# values.yaml additions
store:
  path: /data/orka.db
  persistence:
    enabled: false       # default: ephemeral (emptyDir), no PVC
    size: 1Gi            # only when enabled: true
    storageClass: ""     # use cluster default
    accessMode: ReadWriteOnce
```

No `backend` field — there's only one backend. The `path` and `persistence` settings are all that's needed.

## Risk Assessment

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| SQLite corruption on unclean shutdown | Low | High | WAL mode + `PRAGMA synchronous=NORMAL`; graceful shutdown via `mgr.Add()` Runnable; PVC for durability |
| Performance under high concurrency | Low | Medium | SQLite handles ~100k writes/sec; Orka is controller-level throughput; WAL allows concurrent reads; `MaxOpenConns(1)` serializes writes |
| Data loss on pod restart (ephemeral default) | Medium | Medium | Startup WARN log; Helm NOTES.txt warning; PVC opt-in for production |
| Worker→controller HTTP failure during rolling update | Medium | Medium | Workers retry with exponential backoff (3 attempts); exit non-zero on failure; controller marks job as failed |
| Binary size increase (~15-25MB) | Certain | Low | Expected and documented; no runtime impact |
| In-flight tasks during upgrade | Medium | Medium | Upgrade runbook: drain tasks first; old workers writing ConfigMaps won't be read by new controller |
| `readOnlyRootFilesystem` conflict | Certain | High | emptyDir volume at `/data` in all manifests; tested in CI |

## Upgrade Path

For existing clusters upgrading from ConfigMap-based storage:

1. **Wait for in-flight tasks to complete**: `kubectl get tasks -A --field-selector status.phase=Running` — ensure no tasks are running
2. **Upgrade controller + workers simultaneously** (same release has both changes)
3. **Run migration** (PVC users only): `orka migrate --store-path /data/orka.db --cleanup` to move existing ConfigMap data to SQLite
4. **Verify**: `kubectl get configmaps -l orka.ai/result=true` should show no results; `kubectl get configmaps -l orka.ai/session=true` should show no sessions (skills ConfigMaps remain)

For ephemeral (emptyDir) users: no migration needed. Old ConfigMap data is simply abandoned.

## Testing Strategy

1. **Unit tests**: SQLite store tested with `:memory:` database; interface-conformance tests
2. **Controller tests**: Inject `:memory:` SQLite store; test reconciliation lifecycle
3. **API tests**: `httptest.NewServer` for internal endpoints; store interface injection
4. **Worker tests**: `httptest.NewServer` to mock controller result endpoint; test retry logic
5. **Integration tests**: Full task lifecycle with SQLite backend on disk
6. **Migration tests**: Verify ConfigMap→SQLite migration preserves all data

## Estimated Effort

| Step | Effort | Notes |
|---|---|---|
| Steps 1-3 (store + CRD + endpoints) | ~2-3 days | Core infrastructure |
| Steps 4-6 (workers + controller + job builder) | ~2-3 days | Most code changes, highest risk |
| Steps 7-9 (manifests + RBAC + observability) | ~1 day | Mostly config |
| Steps 10-12 (migration + tests + docs) | ~1-2 days | Polish |
| **Total** | **~6-9 days** | |

## Decision Log

| Decision | Choice | Rationale |
|---|---|---|
| Embedded DB engine | SQLite via `modernc.org/sqlite` | Pure Go (no CGO), SQL for session queries, single-file, mature. Pin this — do not switch to `mattn/go-sqlite3` (requires CGO, breaks Dockerfile). |
| ConfigMap backend | Removed entirely | One code path, fewer bugs, no orphaned resources. Interfaces exist for future PostgreSQL. |
| Storage default | Ephemeral (`emptyDir`) | Zero dependencies, zero config. PVC opt-in for durability. Startup log warns users. |
| Worker result delivery | Internal HTTP endpoint on existing API port | Clean API, no shared PVC, reuses existing auth, avoids port conflicts |
| Session transcript delivery | Init container fetching via HTTP | Zero changes to worker `loadSessionContext()` code |
| `wait_for_tasks` coordination | HTTP GET to public result endpoint | Workers already have `ORKA_CONTROLLER_URL`; reuses existing `/api/v1/tasks/{id}/result` |
| Session locking | SQL `UPDATE ... WHERE active_task = ''` | Atomic, fixes current race condition |
| Skills storage | Keep ConfigMaps | Small, declarative, no size pressure |
| Connection pooling | `MaxOpenConns(1)` | SQLite single-writer; avoids `SQLITE_BUSY` in `database/sql` pool |
| Deployment strategy with PVC | `Recreate` (not `RollingUpdate`) | RWO PVC deadlocks with RollingUpdate — new pod can't mount while old holds it |
| Internal endpoint auth | SA token + namespace match | Prevents cross-namespace result overwrites |
| `SessionManager` | Keep as orchestration layer | Store is persistence; manager is lifecycle (acquire→job→append→release). Different concerns. |
