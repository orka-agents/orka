# Execution events Wave 1 integration readiness

Wave 1 implements the independent P0 foundations from `wave-01-parallel-foundations.md` against SQLite, fake stores, and fake HTTP servers. Optional UI/CLI fixture consumers are intentionally not included in this worktree.

## Fake-to-real integration mapping for Wave 2

| Wave 1 lane | Foundation test coverage | Wave 2 integration target |
| --- | --- | --- |
| 1A SQLite event store | `go test ./internal/store/sqlite -run ExecutionEvent -v` validates schema, per-stream sequence allocation, list filters, latest sequence, delete behavior, and concurrent same-stream appends. | Run internal write API and public read/stream APIs against the real `sqlite.Store` to verify persisted events round-trip through controller routes. |
| 1B Internal event write API | `go test ./internal/api -run 'Internal.*Event|SubmitExecutionEvent' -v` validates auth namespace checks, request validation, redaction/truncation, ignored client metadata, and fake-store append behavior. | Point the handler at SQLite and verify worker-submitted events persist with assigned `id`, `seq`, and `createdAt`. |
| 1C Worker HTTP client | `go test ./workers/common -run Event -v` validates env-derived endpoint construction, service-account bearer auth, short-timeout warning-only failures, and worker-side sanitization against `httptest.Server`. | Run a worker pod against the controller internal route and verify service-account auth plus event persistence. |
| 1D Public task event list API | `go test ./internal/api -run 'ListTaskEvents|Task.*Events' -v` validates task visibility, namespace authorization, `after`, `limit`, repeated `type` filters, latest sequence, and fake-store DTO mapping. | Query `/api/v1/tasks/:id/events` against SQLite-backed data created by the internal write path. |
| 1E SSE task stream skeleton | `go test ./internal/api -run 'StreamTaskEvents|SSE' -v` validates replay, polling for newly appended fake events, heartbeat frames, and cancellation. | Stream `/api/v1/tasks/:id/stream` while worker events are appended to SQLite and verify replay plus live polling behavior. |
