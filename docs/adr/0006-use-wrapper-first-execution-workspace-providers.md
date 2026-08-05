# ADR 0006: Use wrapper-first Execution Workspace providers

Date: 2026-05-21

## Status

Superseded by the ACP core RuntimePool cutover.

## Original decision

The original execution-workspace prototype placed provider ownership in a per-Task worker path: the controller created a worker Job, the worker claimed an upstream workspace, staged another worker process, and handled result submission and cleanup.

## Superseding decision

Built-in `type: agent` Tasks now use only `orka.harness.v2` RuntimePools and private RuntimeSessions. There is no per-Task agent Job or worker-based fallback. Top-level `Task.spec.workspace` defines verified repository input and clean-room publication policy; `Task.spec.execution.workspace` is rejected by the ACP core runtime.

Future agent-sandbox or Substrate support must host one RuntimeSession behind the same v2 lifecycle. Orka remains authoritative for attempt/session fences, prompt leases, cancellation, transcript/finalization, workspace deltas, publication, and delivery receipts. Source-read, target-read, target-write, and forge credentials remain outside the ACP process tree.
