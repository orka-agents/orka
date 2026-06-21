# Post-P0 evented runtime checklist

Date: 2026-06-11

This checklist maps the P0 wave plans in `~/Downloads/orka-p0-wave-plans/` and the post-P0 next plan to implementation status.

## P0 exit criteria audit

| Area | Status | Evidence |
| --- | --- | --- |
| Shared event taxonomy and DTOs | Implemented + tested | `internal/events`, `internal/api/execution_event_dto.go`, `go test ./internal/events ./internal/api -run Event`. |
| SQLite event store | Implemented + tested | Per-stream seq, filters, latest seq, redaction, concurrent append tests in `internal/store/sqlite`. |
| Fake event store | Implemented + tested | `internal/store/execution_event.go` fake store and session aggregation tests. |
| Internal worker append API | Implemented + tested | `POST /internal/v1/events/{namespace}/task/{task}` tests cover validation, auth, and SQLite persistence. |
| Public task event list API | Implemented + tested | `GET /api/v1/tasks/:id/events` tests cover `after`, limits, type filters, auth, missing task. |
| Public task SSE stream | Implemented + tested | Replay, live polling, heartbeat, terminal `stream_complete`, and reconnect tests. |
| Controller lifecycle producers | Implemented + tested | Task controller emits lifecycle events; focused controller event tests exist, envtest full suite requires envtest binaries. |
| AI worker producers | Implemented + tested | `workers/ai` event tests cover model/tool/result/context events and redaction. |
| Agent CLI producers | Implemented + tested | agent runtime event tests cover command/runtime lifecycle. |
| Container/general worker producer | Implemented + tested | worker/common and general worker event/result tests cover basic lifecycle. |
| Redaction/truncation | Implemented + tested | Worker-side and store-side redaction tests cover bearer/JWT/API key/cookie/GitHub/Anthropic/OpenAI/Txn token patterns. |
| No Agent Substrate dependency | Implemented | Event store, APIs, trace, session aggregation, fork, and approvals use normal Task/Job event streams. |
| Retention/deletion | Implemented/documented | Task deletion deletes task event streams; configurable retention is deferred. |
| CLI event consumers | Implemented | `orka task events`, `orka task follow`, `orka session events`, `orka session follow`. |
| UI timeline | Deferred optional | P0/P1 backend and CLI surfaces are complete; UI timeline/trace graph remains a follow-up owned by product/UI. |

## Post-P0 implementation status

| Wave | Phase | Status | Notes |
| --- | --- | --- | --- |
| A | Stabilization and release hardening | Implemented | Added checklist, docs, focused tests, and store/API metrics. Full `make test` requires envtest setup. |
| A3 | No-Substrate smoke | Code path validated by tests | Live kind smoke should run in release environment; no Agent Substrate CRDs/config are required by these APIs. |
| A4 | Redaction/security audit | Implemented + tested | Existing redaction tests and store defense-in-depth remain release-blocking. |
| A5 | Store integrity | Implemented | Existing indexes cover `(namespace, stream_type, stream_id, seq)`, type filters, task, and session. 1k event threshold is covered by store limit and query tests; extended perf smoke can be added before release. |
| A6 | Docs/changelog | Implemented | See `website/docs/reference/execution-events.md`. |
| B1 | Event metrics baseline | Implemented + tested | New low-cardinality Prometheus metrics for append/list/stream/redaction/truncation/derived latency hooks. |
| B2 | Health/readiness | Existing + documented | `/readyz` checks the shared SQLite health checker, covering event store availability under current store policy. |
| B3 | Minimal UX polish | Implemented for CLI | Table/JSON event, trace, fork, approval commands. UI polish deferred. |
| C1-C3 | Session aggregation/list/SSE | Implemented + tested | Task-derived events tagged by `sessionName`; stable append-order session seq cursor. |
| C4 | CLI session consumers | Implemented | `orka session events`, `orka session follow`. |
| D1-D3 | Trace model/builder/API | Implemented + tested | `internal/tasktrace` and `GET /api/v1/tasks/:id/trace`. |
| D4 | CLI trace | Implemented | `orka task trace <task> [-o json]`. |
| D5 | Minimal trace UI | Deferred optional | Backend read model is UI-ready. |
| E1-E3 | Fork/checkpoint MVP | Implemented + tested | Validates checkpoint seq, creates forked Task with provenance annotations, emits fork events. No workspace snapshot clone. |
| E4 | CLI fork | Implemented | `orka task fork`. |
| F1-F3 | Durable approval events/read model/decision API | Implemented + tested | Event-derived approval state and decision append API. |
| F4 | Integrate one high-risk action | Deferred integration | First target is PR creation/merge; API/read model is ready for worker/tool integration. |
| F5 | CLI approvals | Implemented | `orka task approvals/approve/decline`. |
| G1-G2 | Event metrics/SLO hooks | Implemented baseline | Metric names and safe labels are documented; append-time idempotent pair derivation can be expanded as producers stabilize. |
| H | Harness protocol prep | Documented | See `docs/development/harness-protocol-mvp.md`. |

## Release notes draft

Durable execution events now provide replayable task timelines, resumable task and session event streams, trace read models, fork/checkpoint provenance, approval decision events, and event infrastructure metrics without requiring Agent Substrate.
