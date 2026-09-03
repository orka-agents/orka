# ADR 0008: Start RuntimeSession persistence as an internal store

Date: 2026-06-11

## Status

Superseded for ACP control authority by the Kubernetes hard cutover. Retained as
the historical record for why RuntimeSession was not initially exposed as a
public process-management CRD.

## Context

The remaining frontier introduces backend-neutral runtime sessions that can be claimed, reused, released, retained, suspended, and deleted. The lifecycle must support non-Substrate providers first while keeping Agent Substrate optional. Runtime sessions require strict namespace ownership, exact instance fencing, and cleanup semantics. The Kubernetes RuntimePool provider and v2 conformance suite now supply the first implementation seam without requiring a public RuntimeSession CRD.

## Decision

The original decision was to start with an internal RuntimeSession state model
and persistence boundary, then add a CRD only after the non-Substrate provider
and cleanup loop had stable status requirements.

The frozen state machine still lives in `internal/harness/v2` so the controller
and supervisor share validation. The hard cutover now makes
`RuntimeSessionControl` and the other ACP control CRDs authoritative through
status `resourceVersion` CAS, with coordination Leases for controller epoch and
session mutation ownership. SQLite remains the payload store for transcripts,
SessionTurns, deferred outbox projections, and artifacts.

## Consequences

- Provider-private process details remain inside the runtime; the public control
  record stores only Orka-owned lifecycle, fences, leases, and safe receipts.
- Namespace ownership, cleanup policy, and transition validation are enforced
  by Kubernetes-authoritative records rather than SQLite control rows.
- Operator visibility is available through Task/RuntimePool status, the API/CLI,
  and the ACP control CRDs.
- Cross-store finalization must preserve session UID/generation,
  pool/runtime/controller fences, the active attempt/prompt, publication state,
  transcript continuity, and the exact deferred outbox projection.

## Revisit

Any future public process-detail API must remain a projection of this authority.
It must not introduce prompt replay, split authority between SQLite and
Kubernetes, or weaken v2 duplicate/fencing rules.
