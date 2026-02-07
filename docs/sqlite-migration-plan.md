# SQLite Migration Plan: ConfigMap → Embedded Database

## Overview

Replace ConfigMap-based storage for **task results** and **session transcripts** with an embedded SQLite database (via `modernc.org/sqlite`). Skills will remain as ConfigMaps since they are small, declarative, and read-only.

### Goals

- Remove the 1MB ConfigMap size limit for results and sessions
- Enable efficient querying, pagination, and filtering of sessions
- Maintain zero external dependencies (SQLite is embedded, pure Go)
- Keep ConfigMap as a fallback/default for simple deployments (feature flag)
- Fix existing bugs (AI worker missing truncation, no owner refs on worker-created CMs, non-atomic session locking)

### Non-Goals

- Replacing ConfigMaps for skills (they stay as-is)
- Adding external database support (PostgreSQL, etc.) — future work
- Changing the worker architecture (workers still write results; mechanism changes)

---

## Current State

### What Uses ConfigMaps Today

| Use Case | ConfigMap Name | Data Key | Labels | Created By |
|---|---|---|---|---|
| Task results | `{taskName}-result` | `result` | `mercan.ai/result=true` | Workers (AI, Claude, Copilot) or Controller (container tasks via pod logs) |
| Task sessions | `session-{sessionName}` | `transcript.jsonl` | `mercan.ai/session=true` | SessionManager |
| Chat sessions | `chat-session-{sessionID}` | `transcript.jsonl` | `mercan.ai/session=true`, `mercan.ai/session-type=chat` | Chat handler |

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
| Job Builder | `internal/controller/job_builder.go` | Env var `MERCAN_RESULT_CONFIGMAP`, session volume mount |

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

import "context"

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

### Implementations

```
internal/store/
├── store.go              # Interface definitions
├── configmap/
│   ├── result_store.go   # ConfigMap ResultStore (existing behavior, extracted)
│   ├── session_store.go  # ConfigMap SessionStore (existing behavior, extracted)
│   └── *_test.go
├── sqlite/
│   ├── sqlite.go         # DB initialization, migrations, connection management
│   ├── result_store.go   # SQLite ResultStore
│   ├── session_store.go  # SQLite SessionStore
│   └── *_test.go
```

### SQLite Schema

```sql
-- Task results
CREATE TABLE IF NOT EXISTS results (
    namespace TEXT NOT NULL,
    task_name TEXT NOT NULL,
    data      BLOB NOT NULL,
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
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    namespace  TEXT NOT NULL,
    session_name TEXT NOT NULL,
    role       TEXT NOT NULL,
    content    TEXT NOT NULL,
    name       TEXT,
    input      TEXT,           -- JSON-encoded map
    tool_calls TEXT,           -- JSON-encoded
    tool_call_id TEXT,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (namespace, session_name) REFERENCES sessions(namespace, name) ON DELETE CASCADE
);

CREATE INDEX idx_session_messages_session ON session_messages(namespace, session_name);
CREATE INDEX idx_session_messages_order ON session_messages(namespace, session_name, id);
CREATE INDEX idx_sessions_namespace ON sessions(namespace);
CREATE INDEX idx_results_namespace ON results(namespace);
```

### Worker Communication Change

Currently workers write results directly to ConfigMaps via the Kubernetes API. With SQLite, the database lives in the controller process. Workers need a different mechanism:

**Option A: Internal gRPC/HTTP endpoint (Recommended)**
- Controller exposes a lightweight internal endpoint (e.g., `/internal/results`) on a separate port
- Workers call this endpoint to submit results instead of creating ConfigMaps
- Secured via ServiceAccount token (same auth as existing API)
- Pros: Clean separation, no shared storage needed between pods
- Cons: New endpoint to implement

**Option B: Shared PVC between controller and workers**
- Both controller and worker pods mount the same PVC
- Workers write result files to a directory; controller reads them
- Pros: Simple file I/O
- Cons: PVC access mode limitations (ReadWriteMany needed), coupling

**Option C: Keep ConfigMap for worker → controller result passing, store in SQLite on read**
- Workers still write results to ConfigMaps (no worker changes)
- Controller reads the ConfigMap in `collectResult()`, stores in SQLite, then deletes the ConfigMap
- Sessions are controller-only, so they move to SQLite directly
- Pros: No worker changes needed, incremental migration
- Cons: Still temporarily uses ConfigMaps (and their size limit) for result transfer

**Recommendation: Option C for Phase 1, Option A for Phase 2**
- Phase 1 requires zero worker changes and is fully backward-compatible
- Phase 2 removes the ConfigMap intermediary and lifts the 1MB limit for results too

---

## Implementation Phases

### Phase 1: Storage Abstraction + SQLite for Sessions (Controller-Side)

**Scope**: Sessions move fully to SQLite. Results use SQLite for storage but ConfigMap for worker→controller transfer.

#### Step 1.1: Create store package with interfaces
- Create `internal/store/store.go` with `ResultStore` and `SessionStore` interfaces
- Create `internal/store/types.go` with shared types (`SessionRecord`, `SessionMessage`, etc.)

#### Step 1.2: Extract existing ConfigMap logic into `configmap` backend
- Create `internal/store/configmap/result_store.go` — wrap existing `collectResult()` / `handleDeletion()` logic
- Create `internal/store/configmap/session_store.go` — wrap existing `session_manager.go` logic
- Tests: Port existing session manager tests + add new interface-level tests

#### Step 1.3: Implement SQLite backend
- Add `modernc.org/sqlite` dependency: `go get modernc.org/sqlite`
- Create `internal/store/sqlite/sqlite.go`:
  - `NewDB(path string) (*DB, error)` — opens DB, runs migrations, enables WAL mode
  - Auto-create tables on first run
  - Connection pooling via `database/sql`
- Create `internal/store/sqlite/result_store.go`:
  - `SaveResult()` — `INSERT OR REPLACE`
  - `GetResult()` — `SELECT data FROM results WHERE ...`
  - `DeleteResult()` — `DELETE FROM results WHERE ...`
- Create `internal/store/sqlite/session_store.go`:
  - `CreateSession()` — `INSERT INTO sessions ...`
  - `AcquireLock()` — `UPDATE sessions SET active_task = ? WHERE ... AND active_task = ''` (atomic!)
  - `ReleaseLock()` — `UPDATE sessions SET active_task = '' WHERE ... AND active_task = ?`
  - `AppendMessages()` — `INSERT INTO session_messages ...` + update counters
  - `LoadTranscript()` — `SELECT ... ORDER BY id DESC LIMIT ? ` then reverse (efficient pagination)
  - `ListSessions()` — `SELECT ... FROM sessions WHERE namespace = ?`
- Tests: Full unit tests with in-memory SQLite (`:memory:`)

#### Step 1.4: Wire into controller
- Add `--store-backend` flag to controller: `configmap` (default) or `sqlite`
- Add `--store-path` flag: path to SQLite database file (default: `/data/mercan.db`)
- Update `cmd/main.go` to instantiate the chosen backend
- Inject `ResultStore` and `SessionStore` into:
  - `TaskReconciler` (replace direct ConfigMap calls)
  - `SessionManager` (replace internal ConfigMap calls, or replace SessionManager entirely)
  - API server handlers (replace direct ConfigMap reads)
  - Chat handler (replace `loadChatSession()` / `saveChatSession()`)
- Update `job_builder.go`:
  - When using SQLite backend, skip mounting session ConfigMap as volume
  - Instead, inject session transcript as an init container that calls the internal API, OR pass session messages via env var / downward API (for small sessions)
  - For Phase 1: keep session volume mount working with ConfigMap backend; for SQLite, serialize transcript to a ConfigMap on-the-fly before job creation (temporary bridge)

#### Step 1.5: Update deployment manifests
- Add PVC for SQLite storage in `config/manager/manager.yaml` (conditional on store backend)
- Volume mount `/data` in controller pod
- Add `--store-backend` and `--store-path` to deployment args
- Update Helm chart `values.yaml` with store configuration

#### Step 1.6: Migration tool
- Add `mercan migrate` CLI command to `cmd/cli/`:
  - Reads all existing result ConfigMaps (`mercan.ai/result=true`) and inserts into SQLite
  - Reads all existing session ConfigMaps (`mercan.ai/session=true`) and inserts into SQLite
  - Optionally deletes migrated ConfigMaps (`--cleanup`)
  - Idempotent (uses `INSERT OR IGNORE`)

### Phase 2: Worker Result Submission via Internal API

**Scope**: Remove ConfigMap intermediary for results. Workers submit results directly to the controller.

#### Step 2.1: Add internal result endpoint
- Add `POST /internal/v1/results/{namespace}/{taskName}` to the API server
- Request body: raw result bytes
- Auth: ServiceAccount token (reuse existing middleware)
- Handler: calls `ResultStore.SaveResult()`
- Listen on a separate port (e.g., `:8082`) or same port with path-based routing

#### Step 2.2: Update workers
- Replace `writeResult()` in all workers (AI, Claude, Copilot) with HTTP POST to the internal endpoint
- Add env var `MERCAN_RESULT_ENDPOINT` (e.g., `http://mercan-controller.mercan-system:8082/internal/v1/results`)
- Inject via `job_builder.go`
- Remove all Kubernetes client ConfigMap creation code from workers
- General worker: no change needed (controller still reads pod logs)

#### Step 2.3: Session context delivery
- Add `GET /internal/v1/sessions/{namespace}/{name}/transcript` endpoint
- Workers call this instead of reading mounted ConfigMap volume
- Or: controller writes transcript to a temporary ConfigMap/Secret before job creation (simpler, but temporary)

#### Step 2.4: Update RBAC
- Workers no longer need ConfigMap `create/update` permissions for results
- Update `config/rbac/worker_role.yaml`

#### Step 2.5: Remove ConfigMap result cleanup
- Remove result ConfigMap deletion from `handleDeletion()` finalizer
- Remove owner reference logic from `collectResult()`
- Clean up dead code paths

### Phase 3: Polish & Optimization

#### Step 3.1: Database maintenance
- Add periodic `VACUUM` (e.g., daily via goroutine)
- Add `PRAGMA optimize` on connection close
- Configure WAL checkpoint size
- Add database size metric to Prometheus

#### Step 3.2: Backup/restore
- Add `mercan backup` CLI command — copies SQLite file (with `VACUUM INTO`)
- Add `mercan restore` CLI command
- Document backup strategy in ops guide

#### Step 3.3: Observability
- Add metrics: `mercan_store_operations_total{operation,backend,status}`
- Add metrics: `mercan_store_duration_seconds{operation,backend}`
- Add metrics: `mercan_store_db_size_bytes`
- Add health check: `SELECT 1` in readiness probe

#### Step 3.4: Remove ConfigMap backend (optional)
- Once SQLite is proven in production, consider removing ConfigMap backend
- Or keep it as the zero-dependency default for simple/dev deployments

---

## Controller Flag Changes

```go
// New flags in cmd/main.go
flag.StringVar(&storeBackend, "store-backend", "configmap", "Storage backend: configmap or sqlite")
flag.StringVar(&storePath, "store-path", "/data/mercan.db", "Path to SQLite database file (sqlite backend only)")
```

## Helm Chart Changes

```yaml
# values.yaml additions
store:
  backend: configmap  # configmap | sqlite
  sqlite:
    path: /data/mercan.db
    persistence:
      enabled: true
      size: 1Gi
      storageClass: ""  # use default
      accessMode: ReadWriteOnce
```

## Risk Assessment

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| SQLite corruption on unclean shutdown | Low | High | WAL mode + `PRAGMA synchronous=NORMAL`; PVC ensures fsync; backup strategy |
| Performance under high concurrency | Low | Medium | SQLite handles ~100k writes/sec; Mercan is controller-level throughput; WAL allows concurrent reads |
| PVC availability | Medium | High | Document storage class requirements; fallback to ConfigMap backend |
| Migration data loss | Low | High | Idempotent migration tool; run with `--dry-run` first; keep ConfigMaps until verified |
| Worker→controller connectivity (Phase 2) | Low | Medium | Internal service DNS; retry with backoff; fall back to ConfigMap on failure |

## Testing Strategy

1. **Unit tests**: Each store implementation tested independently with interface-conformance tests
2. **Integration tests**: SQLite backend with `:memory:` database in controller tests
3. **Migration tests**: Verify ConfigMap→SQLite migration preserves all data
4. **E2E tests**: Full task lifecycle with SQLite backend
5. **Backward compatibility**: ConfigMap backend must remain fully functional

## Estimated Effort

| Phase | Effort | Dependencies |
|---|---|---|
| Phase 1 (abstraction + SQLite sessions) | ~3-4 days | None |
| Phase 2 (worker result API) | ~2-3 days | Phase 1 |
| Phase 3 (polish) | ~1-2 days | Phase 2 |
| **Total** | **~6-9 days** | |

## Decision Log

| Decision | Choice | Rationale |
|---|---|---|
| Embedded DB engine | SQLite via `modernc.org/sqlite` | Pure Go (no CGO), SQL for session queries, single-file, mature |
| Migration strategy | Phased (ConfigMap bridge → full SQLite) | Zero worker changes in Phase 1, low risk |
| Worker result delivery (Phase 2) | Internal HTTP endpoint | Clean API, no shared PVC, reuses existing auth |
| Skills storage | Keep ConfigMaps | Small, declarative, no size pressure |
| ConfigMap backend | Keep as default | Zero-dependency option for dev/simple deployments |
| Session locking | SQL `UPDATE ... WHERE active_task = ''` | Atomic, fixes current race condition |
