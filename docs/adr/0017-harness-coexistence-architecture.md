# ADR 0017: Harness v1/v2 coexistence architecture and ownership contracts

Date: 2026-08-05

## Status

Superseded by ADR 0018 on 2026-08-07. This record is retained as the historical
architecture for the rejected shared-population design; it is no longer an
implementation or release contract.

Previously accepted. Implements the strategy selected in ADR 0016; the normative
specification is `docs/harness-v1-v2-coexistence-plan.md` (Revision 4). Where
this record and the plan disagree, the plan governs.

## Context

One compatibility release must run harness v1 (turn-oriented wrapper and
external v1 endpoints) and harness v2 (ACP RuntimePools) against the same Task
population without inference, fallback, cross-dispatch, or protocol mutation,
and must retire v1 as a separately gated operation.

## Decision

**Ownership.** One controller binary and exactly one controller ownership
scope reconcile agent Tasks. Leader election is mandatory. The global Lease is
`orka-system/orka-agent-execution` and never varies by release or namespace.
Every legacy `03b49a10.orka.ai` Lease is enumerated, acquired in deterministic
order, and continuously renewed as a migration fence recorded in
`AgentExecutionControl.status.ownership`; loss of any fence closes readiness
and stops every mutating runnable. While SQLite is the payload store the
controller runs exactly one Pod, `Recreate` rollout, and a process-lifetime
exclusive filesystem lock; every mutating SQLite-backed endpoint and background
writer is leader-gated. Kubernetes control CRDs and Leases are authoritative
for lifecycle, fences, and CAS; SQLite persists payloads, ledgers, outbox
projections, and artifacts only (extends ADR 0008).

**API surface.** `core.orka.ai/v1alpha1` remains the only Kubernetes API
version; harness v1/v2 are protocol values. A bridge CRD wave installs the
full v1+v2 field superset with variant fields as optional pointers, no
protocol defaults, and discriminator CEL; enforcement admission then ratchets:
new built-in Agents require an immutable `spec.runtime.contractVersion`
(`orka.harness.v1|orka.harness.v2`), `AgentRuntime.spec.contractVersion`
accepts both values and is immutable, and unchanged historical v1 objects
(including legacy Task `spec.agentRuntime.workspace` fields and
`status.harnessRuntime`) round-trip without pruning. A missing selector is
never interpreted as either protocol; `runtime.type: opencode` is never
protocol evidence.

**Binding.** Every executable agent Task gets, before any executor side
effect, a write-once, immutable, uncached compare-if-absent
`status.agentExecutionBinding` that freezes protocol, backend, mode revision,
policy identity, Agent/runtime UIDs, and a content-addressed immutable
execution snapshot of every non-secret executable input. Dispatchers build
requests only from the snapshot. Snapshot bodies are encrypted at rest,
reference-retained, and exported only through a privileged audited operation.
Adoption of pre-existing Tasks consults v2 authoritative stores first, then
durable v1 evidence, and quarantines ambiguity immutably
(`agentExecutionQuarantine`); proven no-route-state deletion records immutable
`UnboundNoExecution`. There is no fallback in either direction, ever.

**Dispatch.** Two isolated leader-gated dispatchers consume durable demand
carrying the binding digest: `HarnessV1Dispatcher` (durable attempt state
machine, wrapper admission ledger closing only pre-first-frame ambiguity,
`SubmittedUnknown`/`OutcomeUnknown` without replay) and the existing
`ACPDispatcher` (unchanged v2 fencing, publication, external-effect, and
`OutcomeUnknown` semantics; RuntimePool side effects only after binding).
External v2 `runtimeRef` dispatch stays fail-closed.

**Sessions.** Every agent Session durably records lineage (namespace UID,
Session UID, contract version, lineage generation, runtime identity, snapshot
digest, provenance) claimed atomically with the Session lease. Implicit
cross-protocol continuation is rejected; migration is an explicit
transcript-bootstrap creating a new Session UID. Terminal settlement is one
atomic order: validate Kubernetes authority → commit SQLite payload plus
inactive outbox → CAS Kubernetes control and release the lease → activate the
projection. Ambiguity settles the Session `ReconciliationBlocked`, never
`Available`.

**Modes and admission.** Backend admission modes (`enabled → closing →
drain-only → disabled`) live in the singleton `AgentExecutionControl` with
UID/generation/modeRevision CAS and durable binding reservations; the
controller-serialized closing barrier is the linearization point. New v1
admission additionally requires an admin-owned `AgentExecutionPolicy` bound by
UID/generation/digest. Fail-closed admission (parameterized
ValidatingAdmissionPolicy plus `failurePolicy: Fail` webhooks) is served by a
separate stateless replicated `orka-admission` Deployment, never by the
singleton controller Pod; narrowly scoped API-server match conditions exempt
only cleanup-safe controller writes. Quarantine and blocked Sessions exit only
through automatic receipt-based recovery or an immutable, fenced, admin-only
`AgentExecutionAdjudication`; original evidence is never rewritten.

**OpenCode.** Legacy v1 OpenCode Agents are preserved for the window
(sealed-inventory adoption only; no new v1 OpenCode bindings); new OpenCode is
v2-only through the managed digest-pinned RuntimePool profile with
provider-qualified model IDs and reviewed limits. v1→v2 OpenCode migration
creates a new Agent UID and new Session lineage; nothing is patched in place.
Omitted versus explicit-empty tool allowlists remain distinct security states
everywhere.

## Consequences

- Protocol choice is always an explicit, durable, immutable fact; every
  executor record carries the binding digest, and cross-dispatch is
  structurally impossible rather than merely avoided.
- The plan's schemas (§6.2–§6.5, §7.7, §9.3), verification matrix (§13),
  rollback checkpoints (§14), and retirement proofs (§15) are binding release
  gates; `scripts/upgrade-orka-crds.sh` remains the v2-only hard-cutover tool
  and is not the coexistence migration path.
- V1 is a compatibility trust tier: no strict-governance claims, no new direct
  publication, credential-free public-read-only new workloads, warning
  events/metrics on every admitted v1 Task.
- The wrapper Pod template is never mutated while active turns exist without a
  completed durable drain.
