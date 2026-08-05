# Remaining frontier readiness checklist

Date: 2026-07-25

This checklist gates implementation of the backend-neutral resident runtime frontier on top of the post-P0 evented runtime worktree.

| Area | Status | Evidence |
| --- | --- | --- |
| Session event aggregation | Ready | Post-P0 task/session event APIs and CLI follow/list commands are present. |
| Trace read model | Ready | `internal/tasktrace` and task trace API/CLI exist. |
| Fork/checkpoint MVP | Ready | Event-sequence fork API and CLI are present; physical snapshot fork remains deferred. |
| Durable approvals MVP | Ready for API/read model | Approval event read model and decision endpoints exist; first high-risk tool integration is still deferred. |
| Event metrics | Ready baseline | Append/list/stream/redaction metrics exist with low-cardinality labels. |
| ACP v2 DTOs | Ready foundation | `internal/harness/v2` defines the session-centric `orka.harness.v2` request, event, lifecycle, duplicate, capability, and fencing contracts. |
| Event mapping | Ready foundation | ACP v2 prompt events are bounded and mapped into Orka-owned execution/result state; runtime diagnostics are not canonical Task authority. |
| Runtime lifecycle state machine | Ready foundation | `internal/harness/v2` validates RuntimeSession states from `creating` through validation/publication/finalization and deletion. |
| Prompt-scoped broker authority | Implemented foundation | Each RuntimeSession exposes a credential-protected loopback MCP endpoint. The controller revalidates Task, attempt, prompt lease, exact runtime fences, tool policy, approval evidence, and consequential-effect identity for every call. |
| Security requirements | Implemented foundation | V2 fences, operation capabilities, bounded diagnostics, private session identities, the central provider proxy, clean-room Git credential separation, and artifact/credential brokers are modeled and tested. |
| Conformance fixture | Ready foundation | `internal/harness/v2/conformance` and `conformancetest` cover v2 identity, endpoints, duplicates, fencing, cancellation, and governance claims. |
| Kubernetes RuntimePool provider | Implemented foundation | Controller-owned RuntimePools, exact-Pod routing, atomic status reservations, bounded-wait queue promotion, drain, scale-to-zero, and Codex/Claude/Copilot ACP images exist; full live acceptance remains required. |
| Kubernetes control authority | Implemented foundation | `ControllerEpoch`, `PromptAttempt`, `RuntimeSessionControl`, `BranchClaim`, `Publication`, and `ExternalEffect` status plus coordination Leases are authoritative. SQLite retains transcript/SessionTurn, deferred outbox, and artifact payloads rather than ACP control authority. |
| RuntimeSession persistence/API | Implemented foundation | Kubernetes control records and Leases fence restart/takeover recovery; SQLite persists transcript/session-turn payloads behind those fences. Full live restart/takeover acceptance remains required. |
| External v2 runtime dispatch | Deferred | Registration, probing, and conformance are available, but `runtimeRef` Task planning remains fail-closed until the external v2 dispatcher support boundary is enabled. |
| Resident supervisor/process | Implemented foundation | The ACP supervisor hosts multiple private RuntimeSessions with bounded prompt concurrency and cleanup rules. |
| Substrate provider | Deferred | Must host one RuntimeSession per Actor behind the same v2 lifecycle and pass the same conformance, crash, credential, and publication gates. |
| Snapshot-aware fork | Deferred | Logical fork remains available; physical clone/snapshot capability contract follows provider support. |

The v2 contract and Kubernetes RuntimePool foundation have replaced the earlier turn-oriented frontier. The remaining release gate is the required live acceptance matrix: digest verification, Codex then Claude execution, continuation, active-prompt cancellation/timeout, restart and replacement recovery, clean-room publication/PR reconciliation, drain/scale-to-zero, and cleanup.

Focused verification for the readiness layer:

```bash
go test ./internal/harness/v2/... ./internal/acp/... -run 'Protocol|RuntimeSession|Conformance|Fence|Duplicate|Cancel' -v
go test ./internal/api -run 'Task.*Trace|Session.*Event|Fork|Approval|Event' -v
go test ./internal/store/sqlite -run ExecutionEvent -v
```
