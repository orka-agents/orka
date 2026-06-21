# Remaining frontier readiness checklist

Date: 2026-06-11

This checklist gates implementation of the backend-neutral resident runtime frontier on top of the post-P0 evented runtime worktree.

| Area | Status | Evidence |
| --- | --- | --- |
| Session event aggregation | Ready | Post-P0 task/session event APIs and CLI follow/list commands are present. |
| Trace read model | Ready | `internal/tasktrace` and task trace API/CLI exist. |
| Fork/checkpoint MVP | Ready | Event-sequence fork API and CLI are present; physical snapshot fork remains deferred. |
| Durable approvals MVP | Ready for API/read model | Approval event read model and decision endpoints exist; first high-risk tool integration is still deferred. |
| Event metrics | Ready baseline | Append/list/stream/redaction metrics exist with low-cardinality labels. |
| Harness protocol DTOs | Ready | `internal/harness` defines `orka.harness.v1` DTOs and validation. |
| Event mapping | Ready for conformance | `internal/harness.MapFrameToExecutionEvent` maps frames to existing execution events. |
| Runtime lifecycle state machine | Ready foundation | `internal/harness` validates RuntimeSession states and transitions. |
| Tool execution modes | Ready contract | Observed and brokered modes plus brokered idempotency key are defined. |
| Security requirements | Ready contract | Protocol docs and mapper enforce no raw secrets in persisted events. |
| Conformance fixture | Ready foundation | `internal/harness/harnesstest` fake server and suite cover MVP provider behavior. |
| Non-Substrate provider | Not implemented | The first Kubernetes Service/sidecar provider should be built after the conformance suite is adopted by controller integration. |
| RuntimeSession persistence/API | Deferred | ADR 0008 selects internal-store-first; persistent implementation follows provider integration. |
| Resident daemon/process | Deferred | Requires runtime session claim/release and provider implementation. |
| Substrate provider | Deferred optional | Must remain provider-scoped and pass the same conformance suite. |
| Snapshot-aware fork | Deferred | Logical fork remains available; physical clone/snapshot capability contract follows provider support. |

No missing required contract blocks Wave 1 conformance/client/fake-harness work. Wave 2+ implementation should start by wiring a non-Substrate provider to the frozen DTOs and conformance suite.

Focused verification for the readiness layer:

```bash
go test ./internal/harness/... -run 'DTO|Protocol|RuntimeSession|MapFrame|FakeHarness|Conformance' -v
go test ./internal/api -run 'Task.*Trace|Session.*Event|Fork|Approval|Event' -v
go test ./internal/store/sqlite -run ExecutionEvent -v
```
