# Execution Event Contract Dependency Notes

Wave 0 defines the required P0 execution event contract for later implementation lanes. It does not require Agent Substrate, resident actors, fork/checkpoint support, full trace graphs, approvals workflows, harness protocol work, remote harnesses, or session stream endpoints.

## Stable contracts from Wave 0

- `internal/events`: event type constants, severity constants, stream type constants, validation helpers, and redaction/truncation helpers.
- `internal/store`: `ExecutionEvent`, `ExecutionEventFilter`, `ExecutionEventStore`, and an in-memory fake store for tests.
- `internal/api`: DTOs for worker event submission and event list/submit responses.
- `workers/common`: `EventRecorder`, no-op recorder, fake recorder, and event option helpers.
- `internal/testdata/execution-events`: fixture timelines that UI, CLI, and API tests can consume without importing store internals.

## Dependency graph

```text
Wave 0 shared contracts
├── hard blocker: SQLite execution event persistence
├── hard blocker: API submit/list handlers
├── hard blocker: worker HTTP event client
├── hard blocker: CLI task timeline rendering
├── hard blocker: UI task timeline rendering
└── parallelizable after API/list contract: SSE task event streaming
```

The Wave 0 contract is intentionally task-stream-only for P0. `sessionName` can be used as event metadata, but P0 does not define or require session stream endpoints.

## Suggested issue labels

- `execution-events/p0-contract`: standalone contract work that should not depend on integration lanes.
- `execution-events/standalone`: implementation work that can be developed against fakes/fixtures.
- `execution-events/integration-gate`: work that requires two or more lanes to be wired together before it can be considered complete.
- `execution-events/follow-up`: deferred P1+ work outside the Required P0 contract.

## Suggested Wave 1 issue slices

1. `wave-1: implement sqlite execution event store` — hard-blocked on `internal/store.ExecutionEventStore`.
2. `wave-1: add internal worker event submit/list API handlers` — hard-blocked on API DTOs and fake store.
3. `wave-1: add worker HTTP event recorder client` — hard-blocked on `workers/common.EventRecorder` and submission DTOs.
4. `wave-1: render CLI task execution timeline from list response` — hard-blocked on fixtures and list DTOs.
5. `wave-1: render UI task execution timeline from fixture/list response` — hard-blocked on fixtures and list DTOs.
6. `wave-1: add task event SSE against fake/store contract` — parallelizable once list/store contracts are stable; not required for Wave 0.
