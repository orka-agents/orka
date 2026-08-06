# Orka Harness v1/v2 Coexistence Plan

**Status:** Proposed — Revision 4, second review incorporated
**Prepared:** August 6, 2026
**Repository baseline:** `acp` at `77bdc4db9fc6b02d266b770300c8d7fbd18fcfba`
**Comparison baseline:** `origin/main` at `21f8ef15923e505eb89252be9a043f57fb2f2ef0`

## 1. Executive decision

Phase 0 must first choose among blue/green replacement, a zero-active-state in-place bridge, and full active coexistence. This document specifies the full coexistence design if that option is justified; it is not authorization to begin implementation before the strategic gate passes.

If full coexistence is selected, build a time-bounded compatibility release that supports both harness protocols through:

- one controller binary;
- one explicitly owned controller scope;
- one dual-compatible CRD surface;
- two isolated execution dispatchers;
- one immutable, write-once execution binding for every executable agent Task;
- one immutable, non-secret execution snapshot referenced by that binding;
- one atomic Session-lineage and Session-settlement model.

```text
TaskReconciler
  ├─ ensure Task finalizer
  ├─ create/adopt immutable Task execution binding and snapshot
  ├─ v1 binding → HarnessV1Dispatcher → harness-wrapper or external v1 endpoint
  ├─ v2 binding → ACPDispatcher → RuntimePool/RuntimeSession
  └─ route-aware terminal settlement, cancellation, recovery, and finalization
```

Do not:

- infer v1 or v2 from a missing built-in Agent selector;
- run existing v1, v2, or dual controllers against overlapping Task populations;
- automatically fall back from v2 to v1;
- mutate an in-flight Task or Session from one protocol to the other;
- reconstruct a bound request from mutable live Agent, AgentRuntime, ConfigMap, Skill, Tool, or policy state;
- represent v1 as satisfying v2 workspace, credential, fencing, publication, or duplicate-prevention guarantees;
- replay an ambiguously submitted v1 turn;
- delete a v2 RuntimeSession before publication finalization or explicit abandonment;
- rely on Helm rollback, CR YAML export, or object counts as a sufficient recovery mechanism.

Until this compatibility release and its preflight tooling exist, use separate clusters or non-overlapping control planes for simultaneous v1 and v2 operation.

## 2. Validated baseline and preconditions

### 2.1 Repository baseline

The following assumptions were verified against the stated repository baselines:

- The current `acp` branch is an intentional v2-only hard cutover and now includes built-in OpenCode on the managed ACP v2 RuntimePool path.
- `origin/main` contains the v1 turn-oriented harness-wrapper path.
- Focused v1 and v2 API, controller, harness, and worker tests pass independently.
- The v1 and v2 data-plane workloads have distinct Kubernetes resource names and can physically coexist.
- The existing controllers cannot safely run concurrently:
  - in the same namespace they compete for the same leader-election Lease;
  - in different namespaces they can acquire different Leases and reconcile the same cluster-scoped Task population.
- The current v2 CRD prunes v1-only fields when applied over v1 objects.
- A single structural `AgentRuntime` CRD containing both field sets, discriminated by CEL, is feasible without a conversion webhook only if variant-specific OpenAPI required/default semantics are removed.
- Kubernetes ACP control CRDs and coordination Leases are authoritative for lifecycle, fences, mutation ownership, and status `resourceVersion` CAS. SQLite is the payload store for transcripts, SessionTurns, deferred outbox projections, cleanup payloads, and artifacts; SQLite control/epoch rows are not authoritative.
- The current SQLite schema still creates the legacy `runtime_sessions` table, although the v1 store implementation was removed.
- Current v1 Tasks and v2 Tasks are not cross-version resumable by the opposite controller.
- The built-in runtime selector has the same unversioned shape in v1 and v2, and neither baseline has a protocol selector.
- `runtime.type: opencode` exists in both baselines and is therefore not protocol evidence: `origin/main` routes it through the v1 wrapper, while current `acp` routes it through an ACP v2 RuntimePool.
- Legacy v1 and current v2 OpenCode Agent configuration shapes are incompatible. V1 permits legacy model IDs, Agent-level system prompts, and provider Secrets; v2 requires a provider-qualified `provider/model`, reviewed positive `contextWindow` and `maxTokens`, no `providerRef` or provider Secret, no Agent-level system prompt, and no reasoning-effort override.
- The current v2 OpenCode RuntimePool profile pins model token limits and the digest-pinned OpenCode adapter/runtime image. A missing OpenCode image fails closed and has no legacy fallback.
- The current hard-cutover inventory does not fully gate on active built-in v1 Tasks, active wrapper turns, unbound Tasks, or internal Task producers.
- The v1 wrapper keeps active turns and consumed-turn tombstones in process memory; they do not survive wrapper restart.
- Current Session storage does not contain protocol/runtime lineage.
- Helm does not upgrade CRDs in `charts/orka/crds`, and the production Kustomize path excludes CRDs.

### 2.2 Deployment preconditions

Before the dual controller may start:

1. Identify the source installation as one of:
   - verified v1 baseline;
   - verified v2 baseline;
   - already mixed or unknown.
2. Stop all existing controller writers and internal/external Task producers.
3. Establish one fixed controller ownership scope and one fixed leader-election namespace.
4. Take verified, identity-preserving backups appropriate to the intended rollback mode.
5. Apply the dual-compatible CRDs through an explicit CRD lifecycle step.
6. Explicitly classify every existing built-in Agent as v1 or v2; `runtime.type: opencode` must be classified from source-release/UID evidence, never from the runtime type alone.
7. Classify or quarantine every agent Task that is nonterminal, deleting, finalizing, carries the Task finalizer, has route-specific records, or has pending cleanup, plus every referenced Session.
8. Verify that no overlapping controller Deployment or release can watch the same Task population.

An already mixed or unknown installation must fail closed until operator review. A cluster that already applied the v2-only CRD cannot recover previously pruned v1 fields without a pre-pruning backup or another authoritative source.

### 2.3 Migration-strategy go/no-go

Before approving the coexistence ADR or implementation budget, choose one strategy:

1. **Blue/green drain and fresh v2 installation**
   - lowest implementation and long-term support cost;
   - accepts a maintenance window and no preservation of in-flight runtime state;
   - recreates Agents explicitly, including the required new-UID OpenCode migration.
2. **Zero-active-state in-place bridge**
   - freezes every producer and drains v1 completely;
   - preserves compatible Kubernetes objects, history, and backups;
   - applies bridge CRDs and enables v2 without concurrent v1/v2 execution.
3. **Full active coexistence**
   - implements the remainder of this document;
   - preserves eligible in-flight work and Session continuity and permits same-cluster canaries;
   - carries the highest build, verification, operational, and retirement cost.

The decision record must quantify:

- affected clusters and customers;
- active-turn duration and volume;
- Session-continuity and in-flight preservation requirements;
- allowable downtime and maintenance windows;
- consequential external-effect and rollback requirements;
- external v1 runtime and legacy OpenCode usage;
- regulatory, audit, and historical-retention requirements;
- implementation cost, ongoing dual-stack support cost, and retirement date.

Full active coexistence proceeds only when same-cluster canaries, preserved in-flight work, Session continuity, near-zero downtime, or another signed-off requirement justifies its incremental cost. Install count alone is not decisive; one high-value installation may be sufficient when continuity is mandatory.

Selecting blue/green replacement or the zero-active-state bridge terminates and supersedes this full-coexistence plan. The selected option must produce its own reduced implementation plan, verification matrix, rollback procedure, and option-specific definition of done; the full-coexistence definition in section 18 no longer applies.

## 3. Goals

1. Preserve eligible v1 built-in and external runtime workflows during v2 stabilization.
2. Preserve in-flight legacy v1 work without replaying ambiguous consequential effects.
3. Permit explicit v2 canaries in the same controller ownership scope.
4. Freeze the selected execution protocol and executable configuration for the full Task lifetime, including retries and cleanup.
5. Preserve all still-present v1 objects without additional field pruning or forced destructive migration.
6. Preserve v2 fencing, `OutcomeUnknown`, workspace governance, external-effect, and publication semantics.
7. Establish protocol/runtime lineage atomically for every conversation Session used by an agent runtime.
8. Allow v1 admission to transition through `enabled`, `drain-only`, and `disabled` using a durable, revisioned control object.
9. Provide a measurable migration from v1 admission to v2 admission without silently changing existing Tasks or Sessions.
10. Remove v1 admission, data plane, code, and API fields as separate, explicitly gated operations.
11. Keep the compatibility window time-bounded, with an owner, target retirement release, and maximum duration decided before implementation begins.
12. Preserve legacy v1 OpenCode during the compatibility window while admitting new OpenCode v2 Agents only through the managed ACP RuntimePool profile.

## 4. Non-goals

- Making v1 and v2 behavior identical.
- Adding automatic protocol translation for active RuntimeSessions.
- Treating transcript bootstrap as continuation of the same runtime lineage.
- Enabling external v2 Task dispatch unless its existing support boundary is deliberately completed.
- Adding new general-purpose v1 features beyond safety mechanisms required for coexistence, such as durable admission deduplication and drain visibility.
- Claiming strict governance for v1 observed-mode runtimes.
- Supporting transparent replay after an ambiguous consequential v1 side effect.
- Reusing v1 Git publication as the implementation of v2 publication.
- Restoring data already pruned by a previously applied incompatible CRD without a backup.
- Supporting multiple controller replicas while SQLite remains the required single-process payload store behind Kubernetes-authoritative control records.
- Treating ordinary CR YAML export/import as a UID-preserving disaster-recovery mechanism.
- Mutating one OpenCode Agent in place from the v1 configuration contract to the v2 contract; migration creates a new explicitly v2 Agent and a new runtime lineage.

## 5. Safety model

### 5.1 Single active controller owner

“One controller binary” is insufficient. Exactly one controller ownership scope may reconcile a Task population.

Requirements:

- Use one well-known leader-election Lease for all dual-controller releases that can watch the same Tasks. The default namespace is `orka-system` and the default Lease name is `orka-agent-execution`; neither may vary by Helm release name or installation namespace.
- Treat the current `03b49a10.orka.ai` Lease identity as a migration fence. Migration inventory must enumerate every such Lease and every namespace containing a legacy controller release.
- Before opening/migrating SQLite or enabling a mutating runnable, acquire the new global Lease and then acquire every discovered or pre-created legacy Lease in deterministic namespace/name order. Record the complete fence set in `AgentExecutionControl.status.ownership`.
- Continuously renew the legacy Lease fence set throughout coexistence. Loss/replacement of a fenced Lease, discovery of a new unclassified legacy Lease/controller, or inability to complete the overlap scan closes readiness and stops every mutating runnable.
- Detect and reject any old or new controller running with leader election disabled. A process without election cannot be fenced by either Lease; stop its Pods and revoke its write-capable ServiceAccount RBAC before migration.
- Retire the legacy Lease bridge only after legacy manifests/GitOps sources and writer RBAC are removed, no old Pod/image has appeared for two Lease durations and two reconciliation periods, and v1 execution/cleanup retirement is complete. Retain the Lease objects as annotated migration tombstones until a separately reviewed ownership-cleanup release.
- Leader election is mandatory. The compatibility chart and standalone production validation must reject `controller.leaderElect=false`; shipped deployment paths already enable election, so this is enforcement and transition fencing rather than a new steady-state behavior.
- Add startup preflight that detects overlapping controller releases, Deployments, Pods, and watch scopes.
- Fail startup or readiness when overlap cannot be disproved or ownership cannot be renewed.
- Acquire and verify the complete global-plus-legacy ownership fence set before opening or migrating SQLite or serving a mutating SQLite-backed API.
- With SQLite, require exactly one controller Pod, a `Recreate` rollout, and a process-lifetime exclusive filesystem lock adjacent to the database. Helm rendering and production deployment validation must reject `replicas != 1`.
- All SQLite-backed mutation endpoints and background writers must be ownership/leader gated and stop when ownership is lost.
- `HarnessV1Dispatcher`, `ACPDispatcher`, outbox projection, drain coordination, and cleanup runnables must require leader election.
- Migration and repair tools must acquire the same ownership Lease or require every controller Pod to be scaled to zero.

### 5.2 Source-aware Agent classification

Built-in Agents must gain an explicit protocol selector:

```yaml
spec:
  runtime:
    type: codex
    contractVersion: orka.harness.v2
```

Rules:

- There is no dual-schema default.
- Missing selector is not interpreted as either v1 or v2 by the dual controller.
- A source-release-aware migration stamps all existing built-in Agents:
  - verified v1 source release → stamp v1;
  - verified v2 source release → stamp v2;
  - mixed or unknown source → require explicit operator classification.
- New built-in Agents must specify the selector.
- `spec.runtime.contractVersion` is immutable once set.
- Existing declarative manifests must be updated before admission reopens.
- Dual-controller readiness requires zero unclassified built-in Agents.

For `runtimeRef`, derive the protocol from the referenced `AgentRuntime`; do not duplicate it on the Agent.

OpenCode-specific rules:

- `runtime.type: opencode` is supported by both protocols during coexistence and is never sufficient to select one.
- A v1-source OpenCode Agent is stamped v1 and retains its legacy configuration only for v1 execution and cleanup.
- A v2-source OpenCode Agent is stamped v2 and must satisfy the reviewed ACP OpenCode profile.
- Migrating v1 OpenCode to v2 requires a new Agent UID, explicit v2 selector, provider-qualified model ID, reviewed model limits, and a new Session/runtime lineage. Do not patch the legacy Agent in place.

### 5.3 Compatibility policy

New v1 admission requires an explicit cluster-admin-owned `AgentExecutionPolicy`. The policy is namespaced to the trust domain but writable only by administrators.

It must define at least:

- whether new v1 bindings are allowed;
- allowed built-in runtime types;
- whether public read-only workspaces are allowed;
- whether trusted observed-mode external runtimes are allowed;
- allowed brokered tool classes;
- retry eligibility;
- prohibited credential and publication fields;
- required network-isolation profile.

The Task binding records the policy UID, generation, digest, and evaluation result. A later policy change cannot broaden an existing binding.

### 5.4 Fail-closed admission serving topology

Do not serve the mandatory fail-closed coexistence admission boundary from the singleton SQLite controller Pod.

Use a separate stateless `orka-admission` Deployment with:

- at least two replicas;
- rolling updates and a PodDisruptionBudget;
- no SQLite mount, controller reconciliation, or runtime-dispatch responsibility;
- a dedicated ServiceAccount and NetworkPolicy;
- managed TLS Secret and CA-bundle injection;
- readiness that proves certificates, handlers, and policy/config caches are usable before the Pod enters Service endpoints.

Installation order:

1. deploy the admission Service, certificate issuer/injection, and stateless replicas;
2. wait for at least two ready endpoints and verify an AdmissionReview smoke test;
3. create `AgentExecutionControl` and install the parameterized binding/classification/adjudication `ValidatingAdmissionPolicy` and bindings with deny-on-missing-parameter behavior;
4. install `failurePolicy: Fail` webhook configurations for the remaining identity/context rules;
5. only then enable bridge classification, binding protection, or the dual controller.

Pure object-transition invariants remain in CRD CEL. Use a parameterized `ValidatingAdmissionPolicy` for every `absent → present` execution binding, including controller writes. The policy requires the exact control UID, generation, and mode revision and an effectively enabled backend; missing or stale parameters deny the write.

Admission-policy parameter caches are not the linearization point for a drain cutoff. The authoritative cutoff is the controller-owned two-phase `enabled → closing → drain-only` barrier in section 9.3, including durable binding reservations and closure proof. The admission policy is defense in depth and rejects observed stale revisions; it does not replace the serialized controller barrier.

Agent/AgentRuntime contract classification and adjudication resolution-reference writes also remain under API-server enforcement for trusted identities. They are not bypassed merely because the caller is the controller or migrator.

API-server-side webhook `matchConditions` may exclude the exact controller, migration, and adjudication-controller ServiceAccounts only for narrowly defined cleanup-safe writes that cannot create a binding, change a contract, broaden authority, or append an adjudication resolution reference—for example terminal projection recovery and finalizer maintenance. Handler-level trust alone does not prevent circular failure when a webhook endpoint is unavailable. All other matching writes fail closed.

Webhook availability is independent from controller readiness. Controller startup, cleanup, and recovery must not require a write that is routed to an unavailable webhook. Certificate/bootstrap failure keeps migration and untrusted writes closed but does not prevent already-authorized controller cleanup.

### 5.5 Operator adjudication and reconciliation

Quarantine evidence and `ReconciliationBlocked` state remain immutable. Resolution uses a separate namespaced `AgentExecutionAdjudication` record rather than clearing or editing the original evidence.

Each adjudication records:

- exact Task, Session, and namespace UIDs;
- expected subject `resourceVersion`/domain version and evidence-closure watermark;
- binding, quarantine, blocked-state, and evidence digests;
- requested one-way action;
- independent receipt/evidence references and digests;
- verified requester identity, justification, creation time, and expiry when applicable;
- controller-applied operation ID/digest, subject-side immutable resolution reference, resulting subject version, and terminal result.

Allowed actions are fail-closed and do not authorize replay or protocol mutation. They may:

- confirm a proven terminal v1 or v2 outcome;
- authorize cleanup of v1, v2, or both discovered lineages;
- confirm `UnboundNoExecution` from independently verified evidence;
- permanently abandon an unprovable effect as `OutcomeUnknown`;
- bootstrap a new Session lineage from a reconciled canonical transcript.

The adjudication spec is immutable, admin-only, and subject to fail-closed admission. Status is controller-owned and idempotent. The controller applies it only when the subject UID, expected resource/domain version, evidence digest, and evidence-closure watermark still match. Applying a decision CAS-appends an immutable resolution reference to the Task or Session control record; finalization consumes only an `Applied` adjudication referenced by that exact subject-side record. Newly discovered evidence or a changed subject version makes the adjudication `Superseded`. Applying a decision emits Kubernetes events and durable audit records. Conflicting adjudications are rejected by Task/Session UID, evidence closure, and operation CAS.

Provide API, CLI, and UI workflows to list evidence, create an adjudication, observe application, and identify unresolved records. Automatic narrow recovery, such as publication receipt reconciliation, runs before requiring operator adjudication.

Unresolved quarantine, blocked Sessions, and pending adjudications block v1 retirement only when they contain, may conceal, or can authorize v1 lineage or v1 cleanup. Purely v2 blocked/adjudication state remains platform cleanup work but does not keep the v1 wrapper or v1 execution plane installed.

## 6. Core invariants

### 6.1 Finalizer before execution planning

Before binding or executor-specific side effects:

1. the Task finalizer must be present;
2. the Task must not be deleting;
3. the controller must own the active controller scope;
4. the selected backend mode must be read from an uncached, durable revisioned control object.

If deletion begins before binding, the Task may be classified only for cleanup. It must never dispatch new work.

### 6.2 Task binding and immutable execution snapshot

Every executable agent Task must have a controller-owned execution binding that is:

- written once before executor-specific side effects;
- immutable for the Task lifetime;
- preserved across retries;
- keyed by Task UID and immutable referenced-object UIDs;
- linked to an immutable, content-addressed execution snapshot;
- free of raw credentials;
- authoritative for routing, recovery, cancellation, terminal settlement, and finalization.

Scheduled parent Tasks are templates, not executable attempts. They do not receive an execution binding. Each generated child Task receives its own binding and snapshot.

Proposed status shape:

```yaml
status:
  agentExecutionBinding:
    schemaVersion: 1
    mode: execute # execute, cleanup-only
    contractVersion: orka.harness.v1
    backend: harness-wrapper # harness-wrapper, runtime-pool, external-endpoint
    provenance: newly-bound # newly-bound, legacy-adopted, legacy-cleanup-only
    bindingDigest: sha256:...
    task:
      namespaceUID: 00000000-0000-0000-0000-000000000000
      uid: 00000000-0000-0000-0000-000000000000
      boundSpecGeneration: 1
    backendControl:
      name: cluster
      uid: 00000000-0000-0000-0000-000000000000
      generation: 7
      modeRevision: 12
      admittedMode: enabled
    policy:
      name: compatibility
      uid: 00000000-0000-0000-0000-000000000000
      generation: 3
      digest: sha256:...
    agent:
      namespace: default
      name: coder
      uid: 00000000-0000-0000-0000-000000000000
      generation: 4
    snapshot:
      id: task-uid/sha256:...
      digest: sha256:...
      schemaVersion: 1
    runtimeType: codex
    runtimeRef:
      name: optional
      uid: optional-only-for-legacy-cleanup
      generation: optional-only-for-legacy-cleanup
    runtimeProfileDigest: optional-sha256:...
    runtimeProfileDigestSchemaVersion: 1
    boundAt: "2026-08-04T00:00:00Z"
```

The immutable execution snapshot contains every non-secret executable input, including:

- resolved system prompt and Skill content;
- model/runtime configuration, including reviewed OpenCode context/output limits when applicable;
- Task runtime overrides;
- tool, MCP, approval, and native-tool policy;
- retry policy and duplicate-safety classification;
- workspace repository identity, intent, and safety policy;
- runtime endpoint identity and capability profile;
- relevant Agent and AgentRuntime configuration.

Credentials remain references only: role, Secret namespace/name/key, Secret UID, and resourceVersion. Raw credential values never enter the snapshot, binding, Task status, logs, events, or metrics.

For Agent `defaultAllowedTools` and Task `allowedTools`, omission and an explicit empty array are distinct security states. Omission may select reviewed runtime defaults; `[]` means deny all. Classification, custom serialization, immutable snapshots, admission, status patches, and migration must preserve this distinction exactly.

Dispatchers build requests only from the snapshot. Live Agent, AgentRuntime, ConfigMap, Skill, Tool, and policy reads are audit checks, not request-construction inputs.

Execution snapshots contain resolved prompts, Skill content, repository identities, endpoint metadata, and policy configuration. They remain sensitive even though raw credentials and TxTokens are prohibited.

Snapshot lifecycle requirements:

- encrypt snapshot bodies at rest and restrict access to controller and explicitly authorized recovery identities;
- treat snapshot IDs/digests as integrity references, not access authorization;
- expose only metadata and abbreviated digests through ordinary Task status, logs, events, metrics, CLI, API, and UI;
- require a privileged audited operation for snapshot-body export;
- retain a snapshot while referenced by a binding, attempt, Session lineage, finalizer, quarantine, adjudication, or retained recovery checkpoint;
- garbage-collect only after all references are terminal and the configured audit/backup retention period expires;
- include encrypted snapshot records in backup/restore and verify binding-to-snapshot referential integrity;
- apply retention, access, export, and deletion policy independently of Task CR deletion.

### 6.3 Write-once binding CAS

`persistAgentExecutionBinding` must implement compare-if-absent, not a generic retrying status update:

```text
GET Task through uncached APIReader
if Task UID changed or deletionTimestamp is set:
    stop; never dispatch
if binding exists:
    exact match → success
    mismatch → permanent BindingConflict; never overwrite
if binding absent:
    optimistic-lock status patch absent → candidate
```

Additional enforcement:

- Add Task-status CEL allowing only `absent → present`, then exact equality.
- Add equivalent immutability for the snapshot reference and binding provenance.
- Copy the binding digest into every demand, attempt, turn, Session lease, publication, and cleanup record.
- Immediately before the first executor side effect, re-read the Task and backend-control object uncached and verify Task UID, binding digest, control UID/generation/mode revision, and `deletionTimestamp == nil`.
- A stale dispatcher must be unable to process demand created for another binding.

### 6.4 Legacy adoption and quarantine

Legacy adoption is a bounded migration operation, not a permanent inference rule.

Before adoption:

- freeze external ingress and every internal Task producer;
- stop old controller writers;
- capture a sealed inventory of Task UIDs and resourceVersions;
- apply dual-compatible CRDs;
- classify Agents explicitly.

For each pre-existing Task, query evidence in this order:

1. **V2 authoritative evidence:** any `status.execution`, `status.delivery`, PromptAttempt, RuntimePool reservation, SessionControl/SessionTurn, Publication, BranchClaim, ExternalEffect, outbox, or artifact record keyed to the Task UID.
2. **V1 authoritative evidence:** durable v1 demand/attempt/turn/frame/terminal records, legacy runtime-session rows, `status.harnessRuntime`, and deterministic turn annotations only when corroborated by controller-owned durable state, wrapper-ledger evidence, or trustworthy audit provenance.
3. **Task-local configuration:** only when authoritative evidence proves no prior executor-specific state and the referenced Agent has explicit protocol classification.

Rules:

- Any v2 authoritative record adopts v2, even when Task status lacks runtime identity.
- Any v1 authoritative record adopts v1.
- Legacy annotations alone are never sufficient adoption evidence, even when internally consistent.
- Both evidence sets produce immutable `Quarantined/Ambiguous` disposition.
- No evidence plus a deleting Task produces `UnboundNoExecution` cleanup disposition.
- Never infer a historical Agent or AgentRuntime UID from a current object with the same name.
- Legacy records lacking enough UID/configuration evidence become `legacy-cleanup-only`: they may be observed, cancelled, or settled, but never retried, continued, or satisfied by a recreated runtime.
- Deleting Tasks run the same side-effect-free adoption logic before route-aware cleanup.
- Legacy harness annotations become controller-owned through fail-closed admission.
- Disable legacy adoption after the sealed inventory completes.

When authoritative inventory proves that a deleting or terminal-cleanup Task has no route-specific state, persist an immutable no-execution disposition:

```yaml
status:
  agentExecutionNoExecution:
    schemaVersion: 1
    state: UnboundNoExecution
    migrationInventoryID: coexistence-2026-08-04
    evidenceDigest: sha256:...
    recordedAt: "2026-08-04T00:00:00Z"
```

This disposition permits common cleanup only. It cannot be converted into an executable binding, and its exact value is immutable once recorded.

Mixed, contradictory, or unprovable route evidence produces an immutable quarantine record:

```yaml
status:
  agentExecutionQuarantine:
    schemaVersion: 1
    reason: MixedV1V2Evidence
    migrationInventoryID: coexistence-2026-08-04
    v1EvidenceDigest: sha256:...
    v2EvidenceDigest: sha256:...
    recordedAt: "2026-08-04T00:00:00Z"
```

Quarantined Tasks:

- admit no new submission, continuation, retry, or publication;
- retain all potentially relevant cleanup handlers;
- attempt only idempotent observation/cancellation/settlement;
- remain blocked until explicit operator adjudication or verified cleanup of all lineages.

### 6.5 Attempt identity and v1 state machine

Keep attempt-specific state separate from the lifetime binding.

V1 must use a durable attempt aggregate with CAS transitions:

```text
Prepared
  -> Submitting
      -> Rejected
      -> SubmittedUnknown
          -> Rejected | Accepted | OutcomeUnknown
      -> Accepted
          -> Running
          -> CancelRequested | Settling
          -> Succeeded | Failed | Cancelled | OutcomeUnknown
```

The aggregate records:

- Task UID;
- binding and snapshot digests;
- attempt number;
- request digest;
- turn ID;
- runtime session ID;
- correlation ID;
- controller epoch/fenced claim;
- last event sequence;
- submission state;
- backend instance/endpoint identity;
- auth Secret name/key/UID/resourceVersion;
- cancellation request and settlement state;
- terminal receipt digest;
- duplicate-safety and retry classification.

Rules:

- Persist `Submitting` before writing `StartTurn`.
- A definitive pre-submission or ledger-backed non-acceptance becomes durable `Rejected`; it records that no executor accepted the request and is the only submission-state path eligible for safe resend/retry.
- Any request whose non-acceptance cannot be proved becomes `SubmittedUnknown`.
- Do not resend an ambiguously submitted request to an external v1 runtime.
- The built-in wrapper must persist an admission ledger keyed by Task UID, attempt, turn ID, and request digest across restart before automatic recovery may resend.
- `CancelAccepted` is nonterminal; cleanup waits for a terminal frame or authoritative settlement receipt.
- Ambiguous accepted work becomes terminal `OutcomeUnknown` unless authoritative lookup proves its outcome.
- Retry is allowed only after definitive pre-submission rejection or a definitive retryable terminal failure and only when the immutable snapshot classifies the workload as duplicate-safe.

The existing persisted-frame journal remains the cross-restart backstop after the first mapped frame is durable. A positive persisted-frame lookup proves that the deterministic turn was accepted and ran and therefore suppresses another `StartTurn`.

The durable wrapper admission ledger closes only the gaps the frame journal cannot cover:

- admission before the first frame is persisted;
- acceptance when the response is lost;
- idempotent handling of the same turn ID and request digest;
- permanent rejection of the same turn ID with a different digest;
- durable admission-close and drain inventory;
- terminal or explicit `OutcomeUnknown` receipts surviving wrapper restart.

The ledger does not duplicate frame storage, transcript persistence, or frame deduplication. Recovery checks evidence in this order:

1. durable terminal or `OutcomeUnknown` ledger receipt;
2. persisted mapped frames;
3. durable ledger-backed non-acceptance → `Rejected`;
4. durable accepted/running ledger state plus authoritative runtime lookup;
5. otherwise `SubmittedUnknown`, followed by `OutcomeUnknown` when non-acceptance cannot be proved.

Absence from the process-local registry or absence of persisted frames is never proof of non-acceptance.

For v2, retain the current PromptAttempt, fence, Session, publication, external-effect, and `OutcomeUnknown` state machines and bind every record to the lifetime binding digest.

### 6.6 No fallback

Never route a v2 Task to v1 because of:

- disabled or drain-only v2 admission;
- missing RuntimePool image;
- capacity pressure;
- invalid profile;
- `OutcomeUnknown`;
- stale fence;
- workspace modification;
- credential change;
- publication conflict;
- unsupported provider-native tool policy;
- missing live Agent or AgentRuntime after binding.

Never route a v1 Task to v2 because the wrapper or external v1 endpoint is unavailable, restarted, drained, or no longer supports the selected runtime.

### 6.7 Session lineage and atomic terminal settlement

Every conversation Session used by an agent runtime must durably record:

- namespace UID;
- Session UID;
- contract version;
- runtime lineage generation;
- runtime type or AgentRuntime UID;
- configuration/snapshot digest;
- lineage creation provenance.

Lineage establishment or verification must occur atomically with Session lease acquisition. Two concurrent first-use Tasks cannot establish different protocols. A same-name namespace or Session recreation must not attach to old durable runtime state.

A non-empty pre-existing Session is never silently treated as unclaimed. If its prior protocol cannot be proved, it is quarantined. `lineageGeneration` is independent of the Session lease generation and any v2 RuntimeSession generation; it changes only through an explicit named migration.

Cross-version continuation must either:

1. fail with explicit incompatibility; or
2. run a named migration that creates a new Session UID and lineage from the canonical transcript.

It must never silently reuse opposite-protocol runtime state.

Normal terminal completion—not only Kubernetes deletion—must expose one atomic visibility point for:

1. validating Session UID, Task UID, attempt, binding digest, and lineage;
2. appending the assistant result or a terminal outcome marker;
3. setting Session availability to `Available` or `ReconciliationBlocked`;
4. releasing the exact mutation/common lease;
5. finalizing the route-specific turn/attempt receipt;
6. enqueueing the Task terminal projection.

For v2 write Tasks, publication, external-effect, and finalize-or-abandon prerequisites must be durably classified before this Session settlement commits. Publication ambiguity therefore commits the Session as `ReconciliationBlocked`, never `Available`.

Kubernetes ACP control CRDs and coordination Leases are authoritative; SQLite is the payload persistence side of the cross-store protocol. Terminal settlement uses this idempotent order:

1. validate the Kubernetes-authoritative Task, PromptAttempt, SessionControl, BranchClaim, controller epoch, object UIDs/generations, resourceVersions, exact lease, and route-specific receipts;
2. atomically commit the SQLite-owned SessionTurn, transcript or terminal marker, finalization digest, and inactive deferred-outbox payload;
3. CAS-update the Kubernetes-authoritative control records and release the exact Session lease;
4. activate the deferred SQLite outbox projection only after the Kubernetes CAS succeeds;
5. recover from every committed boundary without replaying transcript, publication, or runtime effects.

A SQLite payload commit alone must not make a Session reusable, release control ownership, or expose terminal Task status. A Kubernetes control transition must not become visible before its required payload and deferred projection are durable.

`OutcomeUnknown`, publication ambiguity, or unresolved v1 submission ambiguity sets the Session to `ReconciliationBlocked`. Neither dispatcher may continue the Session until explicit reconciliation, migration, or deletion.

The Task deletion finalizer is an idempotent recovery path for terminal settlement, not the primary normal-completion path.

## 7. API and CRD design

### 7.1 Keep one Kubernetes API version

Continue serving/storing `core.orka.ai/v1alpha1`. Harness v1/v2 are protocol values, not Kubernetes API versions.

Do not introduce a second Kubernetes API version solely for harness v2. That would require conversion and increase migration risk without resolving protocol coexistence.

Use two controlled API waves:

1. **Bridge CRDs:** contain the complete v1/v2 field superset, remove conflicting defaults, preserve unchanged historical values, and temporarily allow one-time absent-to-explicit classification while all execution admission remains closed.
2. **Enforcement admission/schema:** reject new missing selectors and new/changed legacy workspace fields after classification completes. The bridge superset remains installed throughout coexistence and in-place rollback.

Helm and production Kustomize do not perform this transition automatically; the bridge CRDs are a separate mandatory release operation.

### 7.2 Built-in Agent selector

Add `contractVersion` to built-in runtime configuration:

```go
// +kubebuilder:validation:Enum=orka.harness.v1;orka.harness.v2
ContractVersion *AgentRuntimeContractVersion `json:"contractVersion,omitempty"`
```

Rules:

- required for new built-in Agents;
- no default in the dual schema;
- immutable once set;
- backfilled explicitly for every stored Agent before dual-controller readiness;
- omitted only on unchanged stored objects during the bounded bridge migration, when execution admission remains closed;
- fail-closed admission rejects creation of a new built-in Agent without an explicit selector even during the bridge wave.

### 7.3 `AgentRuntime` discriminated union

Change `AgentRuntimeContractVersion` to accept both values:

```go
// +kubebuilder:validation:Enum=orka.harness.v1;orka.harness.v2
```

During coexistence, represent `spec.contractVersion` as an optional pointer so omission can be detected and explicitly classified during the bridge wave. Final admission requires it, but the stored Go type remains able to distinguish omission from either protocol value.

Preserve existing JSON paths and combine child fields:

```go
type AgentRuntimeClientAuth struct {
    BearerTokenSecretRef           *LegacySecretRef `json:"bearerTokenSecretRef,omitempty"`
    ControllerBearerTokenSecretRef *SecretKeyRef    `json:"controllerBearerTokenSecretRef,omitempty"`
    OperationCapabilitySecretRef   *SecretKeyRef    `json:"operationCapabilitySecretRef,omitempty"`
}
```

All variant-specific fields must be OpenAPI-optional pointers without unconditional `+Required` markers. CEL on the discriminator must:

- require the v1 auth/capability shape for `orka.harness.v1`;
- preserve v1’s historically optional capability semantics;
- require the v2 auth/profile/limits/governance shape for `orka.harness.v2`;
- reject mixed shapes;
- make `contractVersion` immutable;
- prohibit removing fields required by the selected variant.

The dual schema has no `contractVersion` default. The bridge schema may temporarily accept omission only to support a controlled one-time absent-to-explicit backfill while writers are stopped. Field omission is never protocol evidence, and final admission requires an explicit value.

`AgentRuntimeCapabilitiesSpec` and observed status include both field sets. Variant-specific observed fields are written only by the matching probe implementation.

Restore real JSON serialization for:

```yaml
status:
  observedAuthRefResourceVersion:
```

The current Go-only `json:"-"` field is insufficient for stored v1 status.

Endpoint and Secret policy must preserve the stronger current controls:

- no loopback endpoints outside tests;
- same-namespace Service endpoints for in-cluster HTTP;
- HTTPS for external endpoints;
- Secret label/runtime/endpoint binding;
- no credentials in URLs.

### 7.4 Task compatibility and validation ratcheting

Restore and retain both status surfaces:

```yaml
status:
  harnessRuntime: ...             # v1 compatibility
  execution: ...                  # v2
  delivery: ...                   # v2
  agentExecutionBinding: ...      # authoritative route
  agentExecutionNoExecution: ...  # proven common-cleanup-only disposition
  agentExecutionQuarantine: ...   # ambiguous route evidence
  agentExecutionResolutionRef: ... # immutable reference to an Applied adjudication
```

Restore legacy Task fields required to decode and round-trip stored objects, including:

- `spec.agentRuntime.workspace`;
- `workspace.gitSecretRef`;
- `workspace.forkRepo`.

Prefer a separate `LegacyAgentWorkspaceConfig` Go type at the historical JSON path so current v2-only `WorkspaceConfig` CEL does not apply unconditionally to stored v1 values. These fields are compatibility-read surfaces, not new authority surfaces.

Rules:

- `Task.spec.workspace` is the only repository surface for newly created agent Tasks.
- New or modified Tasks must not introduce legacy workspace credentials or direct-publication fields.
- Mixed legacy and v2 credential/publication shapes are rejected.
- Historically valid unchanged v1 values must remain status-updatable even when they violate current v2-only URL policy.
- Use transition CEL to grandfather unchanged legacy values, or keep only protocol-neutral URL validation in the shared schema and enforce protocol-specific rules after binding.
- After binding, every execution-affecting agent Task field is immutable. This includes env, SecretRefs, retry policy, resources/placement, priorTaskRef, workspace, runtime overrides, prompt, Agent reference, Session reference, and timeout.

Because CRD validation cannot dereference the selected Agent or policy, enforce reference-dependent rules during binding resolution and again against the frozen snapshot before first side effect.

### 7.5 OpenCode contract-specific Agent validation

The dual schema and controller must distinguish legacy v1 OpenCode from managed v2 OpenCode. `runtime.type: opencode` alone does not select validation semantics.

| Field or behavior | OpenCode v1 | OpenCode v2 |
| --- | --- | --- |
| Execution | harness wrapper | managed ACP RuntimePool |
| Model identity | historical model ID | literal `provider/model` |
| `contextWindow` / `maxTokens` | optional legacy fields | required positive; context greater than output |
| Agent `systemPrompt` | historically allowed | prohibited |
| Agent `secretRef` | historically used for provider endpoint/key | prohibited |
| `providerRef` / `model.provider` | prohibited for runtime-backed agent Tasks | prohibited |
| Reasoning effort / fallbacks | legacy v1 behavior | prohibited |
| Provider credential authority | trusted wrapper configuration | controller-authenticated provider proxy |

Bridge and v1 rules:

- Preserve unchanged stored v1 OpenCode Agents and allow unrelated `/status` updates.
- Do not apply current v2-only OpenCode CEL or `ValidateOpenCodeAgentSpec` unconditionally to a v1-selected or still-unclassified bridge object.
- V1 OpenCode may retain the historical model, `systemPrompt`, and Secret-backed wrapper configuration only under the v1 compatibility policy and immutable execution snapshot.
- New or changed v1 OpenCode configuration remains subject to the v1 compatibility restrictions; it never gains v2 governance claims.

V2 rules apply only to an explicit v2 binding and require:

- a provider-qualified `spec.model.name` whose first nonempty path segment is the provider identity and whose remaining path is the opaque model identity; nested model paths are valid and preserved semantically;
- positive reviewed `spec.model.contextWindow` and `spec.model.maxTokens`, with context greater than output;
- no `spec.providerRef`, provider `secretRef`, or `model.provider`;
- no Agent-level `systemPrompt`;
- no runtime reasoning-effort override or model fallback;
- only supported temperature semantics;
- a digest-pinned OpenCode ACP runtime image and profile whose digest includes model limits, adapter artifacts, tool policy, and provider identity.

A v1-to-v2 OpenCode migration creates a new Agent and new Session/runtime lineage. It never reuses the legacy Agent UID, wrapper runtime session, or provider Secret path.

### 7.6 Status exclusivity and immutability

Add status CEL and controller tests that enforce:

- `agentExecutionBinding` is write-once and immutable;
- `agentExecutionNoExecution` is write-once and immutable and permits common cleanup only;
- `agentExecutionResolutionRef` and the equivalent SessionControl resolution reference are append-once, immutable, and must name an `Applied` adjudication with matching subject/evidence/operation digests;
- a v1 binding cannot acquire new v2 execution/delivery state;
- a v2 binding cannot acquire new v1 harness state;
- legacy pre-binding objects may temporarily contain one historical status surface;
- mixed historical state moves only into quarantine, never directly into a binding;
- terminal outcome/receipt fields remain immutable once recorded;
- quarantine and blocked-state evidence are never cleared or rewritten by adjudication; resolution is represented by a separate immutable adjudication record plus an immutable subject-side resolution reference.

Audit all Task status writers and use scoped optimistic patches so unrelated controllers cannot clear or replace binding fields.

### 7.7 `AgentExecutionAdjudication`

Add a namespaced, admin-authored reconciliation resource:

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: AgentExecutionAdjudication
metadata:
  name: task-uid-evidence-digest
  namespace: default
spec:
  taskRef:
    name: task-name
    uid: 00000000-0000-0000-0000-000000000000
  sessionRef:
    name: optional-session
    uid: optional-session-uid
  expectedState:
    subjectResourceVersion: "12345"
    subjectDomainVersion: 7
    evidenceClosureWatermark: sha256:...
  quarantineDigest: sha256:...
  blockedStateDigest: optional-sha256:...
  action: CleanupBoth # ConfirmV1Outcome, ConfirmV2Outcome, CleanupV1, CleanupV2, CleanupBoth, MarkNoExecution, AbandonOutcomeUnknown, BootstrapNewLineage
  evidenceDigests:
    - sha256:...
  justification: independently verified operator evidence
  requestedBy: verified-user@example.com
status:
  state: Applied # Pending, Applying, Applied, Rejected, Superseded
  operationID: ...
  operationDigest: sha256:...
  resultingSubjectResourceVersion: "12346"
  resolutionRefDigest: sha256:...
  observedAt: "2026-08-06T00:00:00Z"
```

Rules:

- spec is immutable and create-only for cluster administrators or a dedicated adjudicator role; admission verifies `requestedBy` against the authenticated caller;
- admission records verified requester identity and rejects missing or stale Task/Session UIDs, expected subject versions, evidence-closure watermarks, and evidence digests;
- actions are one-way and never authorize prompt replay, publication replay, or protocol mutation;
- controller status application is idempotent and fenced by exact subject version, evidence closure, evidence digest, and operation digest;
- application CAS-appends an immutable subject-side resolution reference that route-aware finalization must verify;
- competing or stale adjudications become `Superseded`; conflicting current adjudications are rejected;
- the original binding, quarantine, no-execution, and blocked-state evidence remain immutable;
- application emits Kubernetes events and durable audit records;
- automatic receipt-based recovery runs before operator adjudication is required.

Provide separate viewer, adjudicator, adjudication-controller, and cluster-admin break-glass roles. Human adjudicators may create records and read evidence but cannot patch Task status, finalizers, bindings, quarantine, or Session availability directly.

Provide CLI and UI workflows to inspect evidence, create adjudications, track application, and list unresolved records. `AbandonOutcomeUnknown` and permanent retirement require typed confirmation and cluster-admin break-glass authorization.

### 7.8 Generated artifacts

After API changes:

```bash
make manifests generate
```

Do not edit generated CRDs, RBAC, Helm CRDs, or deepcopy files directly.

## 8. Controller architecture

### 8.1 Preflight and classification stage

Before either dispatcher starts admitting work:

1. verify singleton controller ownership;
2. verify SQLite single-Pod constraints;
3. verify the dual CRDs and schema annotations;
4. verify zero unclassified built-in Agents;
5. complete the sealed legacy Task adoption sweep;
6. complete Session lineage classification;
7. quarantine ambiguity;
8. verify every pre-existing Task requiring execution or cleanup ownership—including deleting and terminal-cleanup Tasks—is bound, quarantined, cancelled, or explicitly classified with no executor state.

Controller readiness remains false until the preflight completes.

### 8.2 Binding stage

Refactor executable agent Task planning into:

1. `resolveAgentExecutionCandidate` — pure resolution and validation, no durable writes or runtime side effects.
2. `persistAgentExecutionSnapshot` — idempotently store the immutable non-secret snapshot by Task UID and digest.
3. `persistAgentExecutionBinding` — uncached compare-if-absent status CAS.
4. `verifyBoundExecution` — uncached re-read of Task, mode revision, snapshot, and deletion state.
5. `persistExecutorDemand` — route-specific durable demand containing the binding digest.

Only after all five steps may executor-specific work begin.

### 8.3 `HarnessV1Dispatcher`

Move v1 stream execution out of Task reconciliation.

Responsibilities:

- consume persisted v1 demand only when its binding digest matches the Task;
- acquire an epoch-fenced dispatcher claim;
- implement the durable v1 attempt state machine;
- call `StartTurn` only from durable `Submitting` state;
- reconcile `SubmittedUnknown` without unsafe replay;
- stream/poll v1 events and persist sequence progress;
- execute only policy-approved v1 brokered continuations;
- perform atomic Session terminal settlement;
- project terminal Task state;
- apply v1 retry policy from the frozen snapshot;
- cancel and settle v1 turns;
- preserve cleanup support while v1 admission is disabled.

The dispatcher accepts only Tasks bound to `orka.harness.v1` and requires leader election.

### 8.4 `ACPDispatcher`

Keep the current v2 dispatcher and add explicit Task binding and binding-digest checks. Built-in Codex, Claude, Copilot, and OpenCode all use this managed RuntimePool path when explicitly bound to v2.

The dispatcher accepts only Tasks bound to `orka.harness.v2` and requires leader election.

Move RuntimePool creation/scaling after durable snapshot and binding. Preserve current PromptAttempt, fence, Session, publication, external-effect, outbox, artifact, and `OutcomeUnknown` invariants.

For built-in OpenCode v2, the RuntimePool profile requires:

- `providerKind: opencode`;
- the exact provider-qualified model identity;
- reviewed `modelLimits.context` and `modelLimits.output`, with context greater than output;
- the OpenCode CLI, ripgrep, and ACP schema artifact digests;
- a separately configured digest-pinned OpenCode runtime image;
- model limits and artifact identities included in the canonical profile/pool digest.

Changing the model limits, adapter/artifact digest, provider identity, or image creates a new RuntimePool identity and requires drain-and-replace. It must not mutate an existing pool or continue an existing Session under the changed profile. `modelLimits` are mandatory for built-in OpenCode pools and remain optional for generic/external v2 profiles unless that runtime contract requires them.

External v2 dispatch remains fail-closed until intentionally completed.

### 8.5 Path-specific validation

Run common validation first:

- executable Task versus scheduled template;
- Task type and Agent existence;
- exactly one of built-in type or `runtimeRef`;
- explicit built-in contract selector;
- namespace/reference policy;
- execution-affecting Task immutability;
- controller-owned compatibility policy;
- transaction-token prohibition for agent Tasks unless a separately reviewed integration is implemented.

Then run selected-path validation. OpenCode validation is selected by the immutable contract binding; the v2 OpenCode validator must never reject or reinterpret a v1-bound legacy OpenCode Agent.

V1 validation:

- rejects mixed legacy/v2 workspace shapes;
- rejects direct publication for new bindings;
- rejects every Git, publication, and forge credential for new bindings; new v1 workspaces are credential-free and public only;
- rejects consequential retries;
- rejects provider-native or observed-mode write behavior unless the workload is an explicitly trusted isolated exception;
- freezes any adopted legacy credential Secret UID/key/resourceVersion;
- marks ambiguous legacy publication work cleanup-only and non-retryable;
- permits legacy OpenCode shape only for a sealed-inventory legacy-adopted v1 binding and frozen snapshot; new v1 OpenCode binding is rejected.

V2 validation continues to reject:

- arbitrary Task env;
- built-in Agent/Task Secret credential delivery;
- legacy workspace credentials;
- unsupported retry policy;
- unsupported native-tool restrictions;
- unsupported execution placement/resources;
- invalid OpenCode model identity, model limits, provider/Secret delivery, system prompt, reasoning effort, fallback, or missing digest-pinned runtime image;
- any failure path that would invoke v1.

### 8.6 Route-aware terminal settlement and deletion finalization

Normal completion and deletion recovery use the same route-aware ordering. Atomic Session terminal settlement from section 6.7 occurs only after the route-specific execution, publication, and external-effect prerequisites have been durably classified.

Deletion finalization starts by reading the immutable binding, no-execution disposition, or quarantine disposition.

Correct v2 order:

1. close outstanding demand and settle or cancel prompt execution;
2. retrieve and durably classify the terminal output or receipt;
3. drive delivery, Publication, BranchClaim, external-effect, outbox, and artifact prerequisites to terminal state;
4. send runtime publication finalization or explicit abandonment while the exact RuntimeSession exists;
5. complete or idempotently recover atomic Session terminal settlement and lease release;
6. delete the task-scoped RuntimeSession and record its cleanup digest;
7. reclaim executor-specific records and artifact identities;
8. perform common result/event/Job cleanup;
9. remove the finalizer.

V1 order:

1. observe or request cancellation of the exact turn;
2. wait for terminal settlement or classify irreducible ambiguity as `OutcomeUnknown`;
3. finalize v1 Session/turn records atomically;
4. reclaim v1 attempt and runtime-session records;
5. perform common cleanup;
6. remove the finalizer.

For quarantined Tasks, retain both cleanup paths until every discovered lineage is terminal or explicitly adjudicated.

ACP cleanup must never run merely because `task.spec.type == agent`.

### 8.7 Retry behavior

- Preserve the lifetime protocol binding and snapshot across every v1 retry.
- Do not clear binding or snapshot references when clearing attempt state.
- Preserve v2's current no-Task-retry model.
- Reject v1 retry for consequential, observed-mode, non-idempotent, credential-bearing, publication-capable, or ambiguously submitted workloads.
- A legacy cleanup-only binding never retries or continues.
- `OutcomeUnknown` is terminal and never enters generic retry logic.

### 8.8 Internal Task producers

Drain and inventory must account for every producer, including:

- Gateway ingress and queued Gateway events;
- scheduled parent Tasks;
- RepositoryMonitors and RepositoryScans;
- webhook-triggered Tasks;
- delegated child Tasks;
- approval and continuation queues.

Stopping external ingress alone does not close admission. Before a drain cutoff, freeze all producers or prove that every queued item has been bound or cancelled under the intended protocol.

## 9. Deployment design

### 9.1 Single controller process with explicit ownership

Build one controller binary containing:

- v1 protocol client and conformance code;
- v1 durable attempt store and dispatcher;
- v2 protocol and dispatcher;
- both AgentRuntime probe implementations;
- binding, snapshot, classification, and quarantine logic;
- route-aware terminal settlement and finalization.

Deployment requirements:

- exactly one controller Pod while SQLite is used;
- one fixed global leader-election namespace/Lease plus the continuously renewed legacy Lease fence set across upgrades;
- leader election enabled and non-optional;
- preflight rejection of overlapping releases/watch scopes;
- the complete global-plus-legacy ownership fence set acquired before SQLite open/migration or mutating API startup;
- `Recreate` controller rollout plus a process-lifetime exclusive SQLite filesystem lock;
- no old and new controller Pods simultaneously opening the same SQLite store;
- all mutating APIs and background writers ownership/leader gated;
- all old Pods stopped before schema migration or restore.

SQLite backups must use the online backup API or a quiesced WAL checkpoint, clean database close, and volume snapshot. Copying only the main database file while committed data may remain in WAL is prohibited.

Do not deploy current v1 and v2 controllers side by side against the same Task population.

### 9.2 Separate data planes

Retain distinct resources.

V1:

- harness-wrapper Deployment;
- Service;
- ServiceAccount;
- bearer Secret;
- v1 wrapper image;
- durable wrapper admission ledger on a dedicated PVC;
- explicit admission-close, list/status, and drain APIs restricted to the controller.

V2:

- RuntimePools;
- provider proxy;
- SCM egress proxy;
- publisher;
- runtime namespace;
- ACP runtime images, including the digest-pinned OpenCode runtime image.

Use separate Secrets, ServiceAccounts, namespaces or trust-domain placement, and NetworkPolicies.

The v1 wrapper must have:

- default-deny ingress;
- narrowly scoped controller ingress;
- explicitly documented provider/SCM egress;
- no ambient Git publication credential;
- one wrapper per trust domain when isolation cannot otherwise be proven.

### 9.3 Durable backend modes

Represent backend modes and ownership in one controller-owned singleton control object:

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: AgentExecutionControl
metadata:
  name: cluster
  namespace: orka-system
spec:
  backends:
    v1:
      desiredMode: enabled # enabled, drain-only, disabled
    v2:
      desiredMode: disabled
status:
  observedGeneration: 1
  ownership:
    leaseNamespace: orka-system
    leaseName: orka-agent-execution
    leaseUID: 00000000-0000-0000-0000-000000000000
    controllerEpoch: 7
    legacyLeaseFences:
      - namespace: legacy-orka
        name: 03b49a10.orka.ai
        uid: 00000000-0000-0000-0000-000000000000
        resourceVersion: "12345"
  backends:
    v1:
      effectiveMode: enabled
      modeRevision: 12
      admissionClosedAt: null
    v2:
      effectiveMode: disabled
      modeRevision: 4
```

The control UID, generation, and per-backend `modeRevision` form the admission revision. Recreating the control object is not a normal mode transition; a UID change forces cleanup-only behavior until operator reconciliation.

Semantics:

- `enabled`: admit new bindings and process existing state;
- `closing`: internal transition state; reject new binding reservations, settle every reservation created under the prior enabled revision, and prove the cutoff closure;
- `drain-only`: reject new bindings, but resume, cancel, settle, and finalize state proven admitted before the cutoff;
- `disabled`: reject new bindings and new execution, while cleanup/recovery code remains registered. Readiness fails if residual executable or cleanup state is discovered.

Every binding operation first creates a controller-owned durable reservation keyed by Task UID and control UID/generation/mode revision. The Task binding CAS settles that reservation. Mode closure uses one serialized protocol under the complete ownership fence set:

1. CAS the effective backend mode from `enabled` to `closing` and record the closing revision;
2. stop creation of new binding reservations;
3. recover or settle every reservation created under the prior enabled revision;
4. perform repeated uncached Task/reservation inventory until no new prior-revision binding appears and every reservation is terminal;
5. verify the admission policy/webhook observes the closing revision and denies representative new binding writes;
6. CAS to `drain-only`, record `admissionClosedAt` and the cutoff inventory digest, and only then declare the cutoff effective.

The controller is the only identity permitted to create binding reservations or Task bindings. The parameterized admission policy is defense in depth; it is not trusted as a strongly consistent external-object read. Dispatch and first executor reservation require a settled pre-cutoff binding reservation. Any binding lacking one, carrying a stale revision, or appearing after `admissionClosedAt` is quarantined and blocks drain completion.

Static Helm values may bootstrap desired mode but are not the linearizable admission authority.

### 9.4 Wrapper lifecycle and upgrade drain

The historical wrapper uses `Recreate`, `emptyDir`, process-local active turns, and no blocking turn drain. Therefore:

- Do not mutate the wrapper Pod template while active v1 turns exist unless durable admission/turn state and blocking drain have been implemented.
- Roll controller and wrapper independently.
- Reuse the exact existing wrapper image digest during initial controller coexistence upgrade when preserving active turns.
- Add an out-of-band pre-upgrade drain command or Job.
- Deployment mutation must not begin until the durable drain marker is `Completed`; timeout aborts the release.
- An accepted-but-unprovable v1 turn becomes `OutcomeUnknown`; it is never replayed merely to complete an upgrade.

### 9.5 CRD-first release waves

Helm and the production Kustomize path do not automatically upgrade CRDs. The compatibility release requires explicit waves:

1. freeze GitOps/CRD field managers;
2. freeze Task producers and controller writers;
3. take verified backups;
4. apply the exact dual-compatible CRDs with one designated field manager;
5. wait for CRDs to become `Established`;
6. run server-side dry-runs and live status round-trip tests against stored fixtures;
7. explicitly stamp built-in Agents and classify Tasks/Sessions;
8. deploy one dual controller with the source protocol enabled and the other protocol disabled;
9. complete adoption and verify readiness;
10. enable canary admission separately.

Retain the dual superset CRDs as the rollback bridge. Do not narrow the schema while any dual, v1, or v2 object depends on it.

`scripts/upgrade-orka-crds.sh` remains a v2-only hard-cutover gate and is not the coexistence migration tool. It currently treats live OpenCode Agents as delete-and-recreate blockers. The coexistence bridge instead preserves existing Agent UIDs, retains both OpenCode shapes, and stamps the contract explicitly; use separate coexistence migration tooling.

### 9.6 Build and release assets

Restore and retain:

- `workers/harness/**`;
- `cmd/orka-agent-harness-wrapper/**`;
- v1 Go module dependencies;
- v1 Docker build/push targets;
- wrapper Helm and Kustomize assets;
- wrapper image digest plumbing;
- release/security scanning for both image families;
- `ACP_OPENCODE_RUNTIME_IMG` build, push, render, digest validation, and release plumbing;
- durable wrapper admission-ledger migrations and tests.

Update CI that currently rejects wrapper template presence.

### 9.7 Admission service lifecycle

Deploy `orka-admission` independently from the controller and runtime data planes.

Operational requirements:

- two or more stateless replicas with rolling update and PodDisruptionBudget;
- dedicated Service, ServiceAccount, certificate Secret, CA injection, NetworkPolicy, and readiness probes;
- no SQLite, runtime credentials, controller Lease ownership, or mutable execution state;
- webhook configurations installed only after ready endpoints and CA validation succeed;
- parameterized API-server admission for every binding/classification/resolution-reference write, including trusted callers; webhook `matchConditions` bypass trusted identities only for cleanup-safe operations;
- `failurePolicy: Fail` for all protected untrusted writes;
- upgrade and certificate-rotation tests proving that untrusted writes remain fail closed while controller cleanup continues;
- uninstall ordering that removes or disables webhook configurations before deleting the last admission endpoint.

A failed admission rollout blocks migration and untrusted writes but must not strand already-bound cleanup or create a controller startup dependency cycle.

## 10. Security policy

V1 is a compatibility trust tier, not a strict-governance tier.

A fail-closed admission boundary must permit only the exact controller, migration, and adjudication-controller identities to perform their field-specific authorized mutations. Binding creation remains subject to the parameterized backend-control admission policy even for the controller. Task editors may request deletion but may not forge execution provenance, change contracts, append resolution references, or bypass cleanup. Admission protection must use `failurePolicy: Fail` and fail closed during the coexistence window.

### 10.1 Transaction tokens

Unless a separate security review completes a protocol-specific integration:

- reject `Task.spec.transaction` for both v1 and v2 agent Tasks;
- never place raw TxTokens in wrapper/external-runtime requests, env, Task spec/status, annotations, logs, events, metrics, results, or snapshots;
- any future child token must be TTS-minted, scope-subset checked, short-lived, and stored in an owner-referenced Secret;
- every outbound credential exchange must fail closed on audience, scope, TTL, subject, actor, and operation context.

### 10.2 V1-eligible new workloads

Eligible only under an explicit bound compatibility policy:

- pure prompting with no workspace and duplicate-tolerant effects;
- public repository analysis only when the workspace is technically mounted read-only, Bash/native write tools are disabled, and network policy prevents direct SCM publication;
- local diff generation without publication only in a trusted isolated wrapper profile;
- external v1 runtimes using brokered read-only tools, no workspace credentials, and no observed write tools;
- other v1-only profiles in a trusted isolated environment with no claim of strict governance.

If read-only behavior cannot be technically enforced, classify the runtime as trusted/non-governed rather than calling it read-only.

Legacy v1 OpenCode does not qualify for the public read-only workspace lane because the historical adapter pre-approves file mutation. New v1 OpenCode bindings are prohibited. Only sealed-inventory, pre-existing v1 OpenCode Agents/Tasks may be adopted for compatibility execution or cleanup. Adopted v1 OpenCode remains non-publication and non-retryable after ambiguity and never claims v2 governance.

### 10.3 V2-only workloads

Require v2 for:

- managed OpenCode execution outside an explicitly trusted legacy compatibility environment;
- private repository access requiring one-operation credential isolation;
- immutable/read-only workspace proof;
- branch push, fork publication, CAS update, or PR creation;
- path allowlists or repository-control-path denial;
- binary or secret-like content rejection;
- strict duplicate prevention or non-idempotent external effects;
- prompt-scoped governed Tool CRD execution;
- transaction-token integration, if later enabled;
- workflows relying on BranchClaims, independent verification, or durable publication outcome classification.

For OpenCode v2 read intent, the effective native policy must deny Bash, `apply_patch`, `edit`, `write`, and Grep even when requested by the Agent or Task. Read and Glob may remain when allowed, and separately authorized brokered/MCP tools may remain alongside that native surface. Provider access comes only through the controller-authenticated proxy, and the qualified model identity plus reviewed token limits are frozen into the profile and binding.

### 10.4 V1 restrictions

For new v1 bindings:

- require an explicit admin-owned compatibility policy;
- disable direct publication;
- reject legacy and v2 publication credentials;
- reject consequential retries;
- reject mixed workspace/credential shapes;
- prefer one wrapper per trust domain;
- prevent claims of v2 strict workspace governance;
- emit a warning event and metric for every admitted v1 Task;
- bind the policy identity/digest into the Task binding.

For adopted in-flight legacy publication Tasks:

- preserve only the exact frozen legacy endpoint/workspace/credential identity;
- prohibit retry after ambiguity;
- never translate a v1 direct-publication result into v2 delivery success;
- require operator review when terminal publication outcome cannot be proved.

## 11. Observability

Add low-cardinality metrics with enumerated reason codes:

- `orka_agent_tasks_total{contract_version,backend,outcome}`;
- `orka_agent_tasks_active{contract_version,backend}`;
- `orka_agent_binding_failures_total{reason}`;
- `orka_agent_binding_conflicts_total{reason}`;
- `orka_agent_quarantined_active{reason}`;
- `orka_agent_outcome_unknown_total{contract_version,reason}`;
- `orka_agent_cancellations_total{contract_version,result}`;
- `orka_agent_cleanup_pending{contract_version,stage}`;
- `orka_agent_cleanup_oldest_seconds{contract_version,stage}`;
- `orka_agent_v1_admissions_total{runtime_class}`;
- `orka_agent_v1_sessions_active`;
- `orka_agent_unbound_tasks_active{intended_contract}`;
- `orka_agent_mode_revision{contract_version,mode}`;
- `orka_agent_session_lineage_conflicts_total{reason}`;
- `orka_agent_adjudications_total{action,result}`;
- `orka_agent_adjudications_pending{reason}`.

`runtime_class` and `reason` must be bounded enums, not runtime names or free-form messages.

Expose the binding, provenance, mode revision, policy digest, and quarantine state in:

- Task CLI table/JSON output;
- Task API responses;
- Kubernetes events;
- audit/event stream metadata;
- UI Task/Session detail surfaces for contract/backend, abbreviated binding digest, backend mode revision, quarantine/no-execution state, blocked Session state, adjudication progress, and snapshot metadata/retention state.

The UI and ordinary CLI/API responses expose snapshot metadata and digests only. They do not display snapshot bodies, resolved prompts, Skill content, or sensitive Secret-reference metadata by default.

Never label metrics with Task, Session, user, repository, endpoint, or Secret identifiers.

## 12. Migration and implementation phases

### Phase 0 — Safety contract and ownership

- [ ] Complete a migration-strategy ADR comparing blue/green replacement, zero-active-state in-place bridge, and full active coexistence.
- [ ] Record quantitative continuity, downtime, rollback, support-window, installation-criticality, and cost requirements.
- [ ] Obtain named product, operations, and engineering sign-off for the selected and funded strategy.
- [ ] Rescope later phases when full active coexistence is not selected.
- [ ] Record the coexistence architecture in an ADR when full coexistence is selected.
- [ ] Define the controller ownership scope and fixed leader-election namespace.
- [ ] Enforce one controller Pod with SQLite.
- [ ] Define compatibility-window owner, target retirement release, and maximum duration.
- [ ] Define `AgentExecutionPolicy`, `AgentExecutionControl`, and `AgentExecutionAdjudication` ownership/RBAC.
- [ ] Define the separate replicated admission-service topology, CA/bootstrap sequence, API-server match conditions, and minimum supported Kubernetes version for the chosen admission APIs.
- [ ] Define adjudication decisions, evidence requirements, retention, automatic-recovery precedence, and break-glass policy.
- [ ] Inventory all legacy Lease namespaces, controller ServiceAccounts/RBAC, GitOps sources, and leader-election-disabled deployments.
- [ ] Define binding, snapshot lifecycle, quarantine, adjudication, v1 attempt, and Session-lineage schemas.
- [ ] Decide whether external v2 dispatch is in or out.
- [ ] Record the resolved OpenCode policy: legacy v1 OpenCode remains supported during coexistence, while new OpenCode uses the managed ACP v2 RuntimePool profile.

**Exit:** one migration strategy is explicitly selected and funded; remaining phases are scoped to that strategy; and the reviewed API, state-machine, security, ownership, authority, and rollback contracts contain no unresolved ambiguity.

### Phase 1 — Dual schema and storage foundation

- [ ] Generate the complete proposed Agent, AgentRuntime, and Task bridge CRDs before committing to the API design.
- [ ] Apply them to real clusters at the minimum and current supported Kubernetes versions.
- [ ] Verify structural-schema validity, CRD establishment, CEL compilation/static cost, server-side dry-run, and API/etcd size headroom.
- [ ] Run both-baseline create, update, status-update, patch, and server-side-apply fixtures, including representative maximum-shape objects.
- [ ] Make any CEL-cost, schema-size, pruning, or historical-object failure a Phase 1 blocker.
- [ ] Implement `AgentExecutionPolicy`, `AgentExecutionControl`, and `AgentExecutionAdjudication` API types, status, RBAC, and admission rules.
- [ ] Implement the discriminated `AgentRuntime` union with no default and conditional CEL.
- [ ] Restore v1 status and Task workspace fields with validation ratcheting.
- [ ] Add contract-aware OpenCode Agent validation that preserves legacy v1 objects and enforces the reviewed v2 shape only for explicit v2 bindings.
- [ ] Preserve omitted versus explicit-empty Agent/Task tool allowlists through bridge serialization and snapshots.
- [ ] Add required immutable built-in Agent contract selector.
- [ ] Add immutable Task binding and quarantine status types.
- [ ] Add durable immutable execution-snapshot storage.
- [ ] Add Session-lineage columns/records and atomic CAS support.
- [ ] Add durable v1 attempt and wrapper admission-ledger schemas.
- [ ] Regenerate manifests and deepcopy code.
- [ ] Add real stored-object fixtures from both baselines.
- [ ] Test `/status` updates for historically valid v1 objects.

**Exit:** the complete bridge CRDs are accepted by real supported clusters with documented CEL and size headroom; both-baseline fixtures survive create/update/status/SSA without pruning; mixed objects and contract/binding mutation fail; and storage migrations and rollback tests pass.

### Phase 2 — Explicit classification and legacy adoption

- [ ] Implement source-release detection and operator confirmation.
- [ ] Stamp every existing built-in Agent explicitly, including source-aware classification of every OpenCode Agent.
- [ ] Stamp or verify every existing AgentRuntime contract explicitly.
- [ ] Update samples and declarative manifests with separate v1 and v2 OpenCode examples; v1-to-v2 migration creates a new Agent.
- [ ] Scale legacy controllers to zero, revoke legacy writer RBAC, acquire and record the complete legacy Lease fence set, then freeze producers and capture sealed Task inventory.
- [ ] Query all v1 and v2 authoritative evidence by Task UID.
- [ ] Adopt exact single-protocol evidence.
- [ ] Create cleanup-only legacy bindings where identity is incomplete.
- [ ] Quarantine mixed/ambiguous state.
- [ ] Classify existing Sessions and block ambiguous lineage.
- [ ] Run adoption for deleting and finalizing Tasks.
- [ ] Disable annotation-based adoption after the sweep.

**Exit:** zero unclassified built-in Agents; every execution- or cleanup-relevant Task, including deleting/finalizing Tasks, has a binding, immutable no-execution disposition, cancellation, or quarantine; and zero referenced Sessions remain unclassified.

### Phase 3 — Binding, snapshot, and common Session settlement

- [ ] Implement pure candidate resolution.
- [ ] Persist immutable snapshots by Task UID/digest.
- [ ] Implement uncached compare-if-absent binding CAS.
- [ ] Add binding CEL and status-writer audit.
- [ ] Bind mode and policy UID/generation/digest.
- [ ] Add binding-digest checks to every durable record.
- [ ] Implement atomic Session lineage plus lease acquisition.
- [ ] Implement protocol-neutral cross-store terminal Session settlement using Kubernetes authority and SQLite payload/outbox ordering.
- [ ] Implement `AgentExecutionAdjudication`, automatic-recovery-before-adjudication, admin RBAC, audit events, and CLI/API workflows.
- [ ] Add binding, deletion, Agent recreation, concurrent first-use, quarantine, blocked-Session, and adjudication tests.

**Exit:** no Agent/AgentRuntime/config mutation, controller restart, deletion race, or status conflict can change an executor or executable request.

### Phase 4 — Restore safe v1 execution plane

- [ ] Restore v1 protocol/client/conformance packages.
- [ ] Restore wrapper worker and binary.
- [ ] Restore required module dependencies.
- [ ] Characterize the existing persisted-frame recovery backstop and document every v1 submission crash window.
- [ ] Implement a durable wrapper admission ledger limited to pre-first-frame ambiguity, idempotent admission, drain inventory, and durable terminal/unknown receipts.
- [ ] Implement fenced `HarnessV1Dispatcher` and v1 attempt state machine.
- [ ] Move turn identity from mutable annotations into durable state.
- [ ] Restore external v1 `runtimeRef` cleanup/execution within policy.
- [ ] Restore legacy built-in OpenCode wrapper execution behind sealed-inventory, legacy-adopted v1 bindings only; prohibit new v1 OpenCode admission.
- [ ] Add `SubmittedUnknown`/`OutcomeUnknown` behavior.
- [ ] Add settlement-aware cancellation.
- [ ] Add durable drain and wrapper no-rollout gate.
- [ ] Add recovery tests for pre-write rejection, acceptance-before-response, acceptance-before-first-frame, post-first-frame restart, and terminal-before-Task-status projection.

**Exit:** v1 Tasks run, recover, cancel, settle, retry only when proven safe, and delete without entering ACP cleanup or replaying ambiguous work.

### Phase 5 — Route-aware v2 integration

- [ ] Gate ACP queueing and dispatch by immutable v2 binding/digest.
- [ ] Move RuntimePool side effects after snapshot and binding.
- [ ] Gate ACP finalization by binding.
- [ ] Correct publication-finalization-before-RuntimeSession-deletion ordering.
- [ ] Preserve current fence, publication, external-effect, outbox, artifact, and `OutcomeUnknown` behavior.
- [ ] Preserve the merged OpenCode ACP profile, model-limit digesting, provider-proxy authority, native-tool normalization, and read-only policy semantics behind explicit v2 bindings.
- [ ] Ensure external v2 remains rejected unless explicitly enabled.

**Exit:** v1 and v2 Tasks run concurrently without shared execution paths, cross-cleanup, or weakened v2 publication semantics.

### Phase 6 — Deployment and required compatibility CI

- [ ] Restore wrapper Helm/Kustomize resources.
- [ ] Add v1 NetworkPolicy and trust-domain placement.
- [ ] Enforce SQLite single-Pod deployment.
- [ ] Add fixed controller ownership/leader-election namespace and explicit retirement/fencing of every `03b49a10.orka.ai` legacy Lease holder.
- [ ] Add durable backend modes and revision checks.
- [ ] Deploy the separate replicated stateless `orka-admission` service, certificate/CA bootstrap, API-server match conditions, PodDisruptionBudget, and fail-closed smoke tests.
- [ ] Add CLI and UI surfaces for binding, quarantine, no-execution, blocked Session, adjudication, backend mode, and snapshot metadata.
- [ ] Restore v1 image targets and release artifacts.
- [ ] Retain OpenCode ACP image build/push/render/digest validation and release scanning.
- [ ] Update CI rules that prohibit wrapper resources.
- [ ] Implement explicit CRD-first apply waves.
- [ ] Add coexistence-specific migration tooling; do not reuse the v2-only `scripts/upgrade-orka-crds.sh` hard-cutover path.
- [ ] Add required fresh-install and both-direction upgrade workflows.
- [ ] Add backup/restore and rollback drills.

**Exit:** one release installs both planes with distinct identities/credentials, singleton ownership, required compatibility CI, and a rehearsed release procedure.

### Phase 7 — Canary and admission migration

- [ ] Start from source protocol enabled and other protocol disabled.
- [ ] Enable explicit v2 canaries only after classification/adoption completes, including an OpenCode v2 canary with reviewed model limits.
- [ ] Run defined canary soak and safety SLOs.
- [ ] Freeze all v1 Task producers before closure.
- [ ] CAS v1 from `enabled` to internal `closing` and stop new binding reservations.
- [ ] Bind, cancel, or quarantine every pre-closing unbound Task and settle every prior-revision binding reservation.
- [ ] Resolve or explicitly retain every pending v1-related adjudication and run repeated uncached Task/reservation inventory passes.
- [ ] Verify admission enforcement observes `closing` and rejects representative new binding writes.
- [ ] Transition v1 to `drain-only` only after closure proof, then record the durable cutoff revision/time and inventory digest.

**Exit:** no post-cutoff v1 binding can be created; v1 backlog and cleanup age are measurable and within declared limits.

### Phase 8 — Staged v1 retirement

- [ ] Admission off: no new v1 bindings or v1-producing queues.
- [ ] Execution drained: zero `Prepared`, `Submitting`, `SubmittedUnknown`, `Accepted`, `Running`, `CancelRequested`, or `Settling` v1 attempts and zero unsettled accepted/running/cancel-requested/unknown wrapper-ledger entries.
- [ ] Cleanup drained: zero v1-related pending cancellation, output retrieval, Session settlement, unresolved quarantine/blocked state, pending adjudication, or finalizer work.
- [ ] Data plane removed: wrapper removed only after durable quiescence proof.
- [ ] Code retained as cleanup/read compatibility until stored state no longer requires it.
- [ ] API fields removed only through a separately planned storage/API migration after retention and archival requirements are satisfied.

**Exit:** no active, cleanup-relevant, resumable, unresolved adjudication, or stored object requires v1 execution semantics; historical retention has an explicit archival/removal disposition.

## 13. Verification matrix and release gates

### 13.1 Required workflow lanes

Release must depend on a compatibility workflow containing:

1. v1-only fresh install;
2. v2-only fresh install;
3. dual fresh install;
4. active v1 → dual upgrade;
5. active v2 → dual upgrade;
6. mixed execution plus controller leader restart;
7. wrapper restart before first persisted frame;
8. CRD-first Helm upgrade;
9. CRD-first production Kustomize deployment;
10. in-place rollback within compatible dual releases;
11. coordinated backup/restore disaster drill;
12. legacy v1 OpenCode plus managed v2 OpenCode coexistence and migration-with-new-Agent lane;
13. fail-closed admission install, certificate bootstrap/rotation, replica rollout, endpoint loss, and uninstall ordering;
14. legacy `03b49a10.orka.ai` Lease holder plus new-ownership transition, including election-disabled-controller rejection;
15. cross-store terminal settlement crash injection at every Kubernetes/SQLite/outbox boundary;
16. quarantine and `ReconciliationBlocked` automatic recovery, adjudication, conflicting-decision rejection, and retirement inventory;
17. full bridge-CRD real-cluster CEL/static-cost, size-headroom, and maximum-shape fixture lane.

### 13.2 API/CRD

- existing v1 AgentRuntime survives CRD application and `/status` update;
- existing v2 AgentRuntime survives CRD application and `/status` update;
- historically valid v1 workspace URLs and fields round-trip unchanged;
- valid explicit v1 and v2 objects are accepted;
- unclassified new built-in Agents are rejected;
- mixed auth/capability shapes are rejected;
- contract and binding mutation are rejected;
- v1 status round-trips;
- v2 Task immutability remains enforced;
- applying rollback bridge CRDs does not narrow stored schema;
- a stored v1 OpenCode Agent with legacy model/systemPrompt/Secret shape survives bridge-CRD application and `/status` update;
- a v2 OpenCode Agent requires provider-qualified model identity and reviewed positive model limits;
- v2-only OpenCode validation is conditional on explicit v2 selection and does not reject v1-selected legacy objects;
- omitted and explicit-empty Agent/Task tool allowlists remain distinct through bridge serialization, snapshots, and migration;
- full bridge CRDs establish on all supported Kubernetes versions with recorded CEL/static-cost and size headroom;
- `AgentExecutionAdjudication` spec and original quarantine/no-execution evidence are immutable, and stale evidence digests are rejected.

### 13.3 Binding and adoption

- v1-source Agents are explicitly stamped v1;
- v2-source Agents are explicitly stamped v2;
- current v2 unversioned Agents are never silently classified v1;
- v1 and v2 OpenCode Agents with the same `runtime.type` are classified from source/UID evidence, not from the type string;
- unknown-source OpenCode Agents are quarantined rather than inferred from their fields or available image;
- v1-to-v2 OpenCode migration creates a new Agent UID and does not mutate the legacy Agent;
- PromptAttempt-without-Task-status adopts v2;
- v1 retry state with cleared annotations still adopts v1 from durable evidence;
- user-forged or annotation-only legacy evidence does not force adoption;
- deleting legacy Tasks receive cleanup-only adoption;
- proven no-route-state deletion records immutable `UnboundNoExecution` and performs common cleanup only;
- Agent/AgentRuntime delete-recreate with same name never satisfies an old UID;
- binding conflict never overwrites an existing binding;
- `enabled → closing` stops new durable binding reservations, settles all prior-revision reservations, and produces a stable cutoff inventory before `drain-only`;
- missing or stale `AgentExecutionControl` parameters deny every observed new binding, including controller writes;
- a binding appearing after `admissionClosedAt`, lacking a settled pre-cutoff reservation, or carrying a stale revision is quarantined and blocks drain completion;
- every executor record contains the binding digest;
- ambiguous both-state objects enter quarantine;
- automatic receipt-based recovery runs before adjudication;
- adjudication actions cannot change the bound protocol or authorize replay and are applied exactly once by subject version/evidence-closure/operation digest;
- finalization consumes only an immutable subject-side resolution reference to an `Applied` adjudication;
- newly discovered evidence or a changed subject version makes a pending adjudication `Superseded`.

### 13.4 Runtime and recovery

- v1 and v2 Tasks run concurrently;
- controller restart with active v1 Task;
- wrapper restart before and after first persisted frame, proving the frame journal suppresses replay after the first frame and the admission ledger closes only the pre-frame gap;
- definitive pre-submission or ledger-backed non-acceptance becomes durable `Rejected` and is the only safe submission retry/resend path;
- ambiguous v1 submission becomes `OutcomeUnknown` without replay;
- settlement-aware v1 cancellation;
- controller restart with queued v2 Task;
- accepted v2 prompt on takeover becomes `OutcomeUnknown` as designed;
- timeout and deletion for both protocols;
- backend closing/drain-only blocks new reservations, proves a linearized cutoff, and completes only pre-cutoff work;
- stale mode revision cannot create a late binding or reservation;
- v1 OpenCode executes only through `HarnessV1Dispatcher` and v2 OpenCode only through `ACPDispatcher`;
- absent, placeholder, or mismatched OpenCode ACP image/profile fails closed without v1 fallback;
- changing OpenCode model limits, artifacts, provider identity, or image creates a new RuntimePool identity and cannot continue the prior Session;
- OpenCode v2 read-policy defeats requested Bash, mutation aliases, and Grep;
- qualified and nested OpenCode model IDs preserve the provider prefix and opaque model remainder through binding, authorization, and profile digesting, and the proxy forwards the exact remainder on the selected provider route;
- OpenCode v2 native-tool normalization, continuation, cancellation, timeout, and restart cases pass.

### 13.5 Sessions

- atomic concurrent first-use chooses exactly one lineage;
- v1 Session continues on v1;
- v2 Session continues on v2;
- implicit cross-version continuation is rejected;
- explicit transcript-bootstrap migration creates a new Session UID/lineage;
- cross-store terminal settlement validates Kubernetes authority, commits SQLite payload/outbox, CAS-updates Kubernetes control/releases the lease, and only then activates projection;
- crash after each committed boundary resumes without replay;
- `OutcomeUnknown` blocks continuation;
- independently verified recovery or immutable adjudication can resolve a blocked Session without clearing original evidence;
- Session deletion settles both route-specific records.

### 13.6 Security

- v1 and v2 credentials are resolved independently;
- no raw credential or TxToken appears in Task spec/status/annotations/logs/events/results/snapshots;
- transaction-bearing agent Tasks remain rejected unless separately enabled;
- v1 cannot claim strict workspace governance;
- new v1 direct publication is disabled;
- mixed legacy/v2 credential shapes are rejected;
- v2 child receives no Git publication credential;
- v2 publication and independent verification remain intact;
- v2 rejection never invokes v1;
- policy identity/digest is frozen into the binding;
- admission Service outage blocks untrusted protected writes but does not block API-server-excluded cleanup-safe controller operations;
- controller binding creation is never excluded from the parameterized API-server admission policy;
- admission certificate bootstrap and rotation complete before `failurePolicy: Fail` configurations become active;
- OpenCode v2 receives provider access only through the controller proxy and no Agent/provider Secret enters the child;
- OpenCode v1 credentials remain confined to the legacy compatibility path and never satisfy v2 credential-isolation claims.

### 13.7 Deployment and ownership

- Helm lint/template with v1-only, v2-only, and dual bootstrap modes;
- Helm rejects SQLite with multiple controller replicas;
- second release in another namespace cannot acquire overlapping ownership;
- the mutating ownership barrier remains closed until the dual controller holds the global Lease and complete legacy `03b49a10.orka.ai` fence set, with no other holder and every election-disabled controller stopped;
- separate admission replicas remain available through controller `Recreate` rollout and their own rolling update;
- Kustomize build for dual mode;
- CRD apply is a separate required wave;
- RBAC covers both planes without broadening runtime child permissions;
- wrapper and RuntimePool NetworkPolicies coexist;
- image digests are required for release deployment, including `ACP_OPENCODE_RUNTIME_IMG` when OpenCode v2 is enabled;
- wrapper rollout is blocked while active turns exist;
- upgrade drain timeout aborts release mutation.

### 13.8 Required safety SLOs

Before widening canary traffic:

- zero binding mutations;
- zero cross-dispatches;
- zero v2-to-v1 fallbacks;
- 100% of executor side effects preceded by a durable matching binding and snapshot;
- zero unexplained or unclassified `OutcomeUnknown` events;
- zero cleanup items older than the declared cleanup deadline;
- zero unclassified Agents, Tasks, or referenced Sessions;
- zero unresolved admission bootstrap/certificate errors;
- zero stale legacy Lease holders or election-disabled controllers;
- zero open binding reservations and zero post-cutoff bindings at the drain transition;
- zero conflicting or unapplied adjudications beyond the declared operator deadline;
- at least three consecutive green compatibility workflow runs;
- a minimum 72-hour canary soak with at least 100 terminal Tasks per enabled protocol, Session continuation on both protocols, cancellation and timeout cases, controller ownership takeover, and one wrapper drain/restart.

## 14. Rollback and disaster recovery

### 14.1 Rollback checkpoints

Define these checkpoints:

1. **Pre-compatibility mutation:** no Agent classification, Session-lineage migration, binding, snapshot, or dual durable record exists.
2. **Dual schema/classification applied:** CRDs and explicit selectors exist, but no dual binding/snapshot/lineage mutation has admitted execution.
3. **Dual execution active:** any v1 or v2 Task has a dual binding, snapshot, migrated Session lineage, or new durable attempt record.
4. **External effects possible:** any prompt, publication, or external effect may have been accepted.

Rollback to a pre-coexistence v1-only or v2-only binary is allowed only at a specifically tested checkpoint. “No v2 Task exists” is not sufficient if the dual controller already migrated v1 state.

### 14.2 In-place rollback

An in-place rollback:

- preserves all Kubernetes objects and UIDs;
- retains the dual superset CRDs;
- preserves controller/publisher PVCs;
- rolls only to a compatibility binary proven to understand the existing binding/snapshot/store versions;
- requires completed drain markers before Pod replacement;
- validates referential integrity before reopening admission.

Do not narrow CRDs during in-place rollback.

### 14.3 Full disaster restore

A full restore requires a coordinated recovery point containing:

- identity-preserving Kubernetes control-plane/etcd snapshot;
- controller SQLite/PVC snapshot including WAL consistency;
- immutable execution-snapshot records;
- controller artifact storage;
- publisher PVC/journal/prepared bundles;
- wrapper admission-ledger PVC;
- relevant Secrets and runtime objects;
- a backup manifest recording controller epoch, control UID/generation, mode revision, namespace/object UIDs, store schema versions, and snapshot timestamps.

If UID preservation is unavailable, use an application-aware restore tool that:

- remaps or terminalizes every UID-bound record;
- never resumes work under recreated same-name identities;
- creates a new monotonic restore epoch;
- classifies unprovable prompt/publication/external-effect outcomes as `OutcomeUnknown`;
- prevents replay.

CR YAML inventory and count comparison are audit aids, not a restore mechanism.

### 14.4 Rollback sequence after execution admission

1. stop external ingress and every internal Task producer;
2. for each enabled backend, CAS `enabled → closing`, stop new durable binding reservations, and capture the closing revision;
3. capture a sealed pre-cutoff inventory and bind, cancel, record `UnboundNoExecution`, or quarantine every pre-closing unbound Task;
4. recover or settle every prior-revision binding reservation and perform repeated uncached Task/reservation inventory until closure proof succeeds;
5. verify admission enforcement observes `closing`, then CAS each backend to `drain-only` and record `admissionClosedAt` plus the cutoff inventory digest;
6. require drain result `Completed`, not `TimedOut`;
7. settle v1 and v2 attempts, Sessions, publications, external effects, outbox, and artifacts;
8. require zero active/submitting/ambiguous v1 turns or classify them permanently unknown without replay;
9. stop all controller and publisher writers;
10. take or restore one coordinated identity-preserving recovery point;
11. restore only a binary/CRD/store combination proven compatible with that recovery point;
12. validate referential integrity across Task, binding reservation, binding, execution snapshot, Agent, AgentRuntime, namespace, Session, RuntimePool, PromptAttempt, Publication, BranchClaim, ExternalEffect, outbox, artifact, and Secret identities;
13. reopen producers and ingress only after readiness and compatibility checks pass.

## 15. Inventory and retirement proof

Inventory is a closed-world proof, not a single Task count.

It must include:

- unbound agent Tasks and their explicit intended protocol;
- v1/v2 bound Pending, Running, Finalizing, and cleanup Tasks;
- durable binding reservations for both protocols, including open, settled, rejected, and orphan reservations keyed by control revision;
- durable v1 attempt states;
- active wrapper turns and admission-ledger records;
- v1 runtime-session rows;
- v2 PromptAttempts, RuntimeSessions, Publications, BranchClaims, ExternalEffects, outbox, and artifacts;
- Session lineage, locks, cleanup intents, blocked Sessions, and their automatic-recovery/adjudication state;
- queued/claimed Gateway events and deliveries;
- scheduled parent Tasks;
- RepositoryMonitors/Scans and webhook queues;
- delegated child producers;
- Agents, AgentRuntimes, GatewayBindings, and OpenCode usage split by explicit v1/v2 contract and configuration shape;
- immutable execution snapshots, retention/GC state, and orphan snapshot records;
- Pending/Applying/Applied/Rejected/Superseded adjudications and unresolved quarantine evidence, classified as v1-related, mixed/possibly-v1, or v2-only;
- admission policy/webhook configuration digests, certificate readiness, and expected replica availability;
- terminal historical Tasks that still require v1 schema fields, separated from active/cleanup state.

The inventory report records the control UID/generation/mode revision, Kubernetes list resourceVersions, durable-store schema/version watermark, binding-reservation store watermark and open-reservation count, wrapper ledger generation, publisher journal watermark, and a digest of the complete report. Unknown, unreadable, orphaned, or contradictory state counts as nonzero state.

Before declaring zero:

1. freeze all producers;
2. record a durable cutoff mode revision and timestamp;
3. settle or reject every pre-cutoff binding reservation and bind, cancel, or quarantine all pre-cutoff Tasks;
4. perform at least two identical Task/reservation inventory passes separated by more than the maximum producer poll interval and claim lease;
5. verify no record newer than the cutoff introduced v1 work;
6. verify cleanup age and finalizer state;
7. verify through the durable ledger and authenticated listing that every wrapper turn is terminal or explicitly `OutcomeUnknown` and that zero unsettled turns remain before shutdown or removal.

Distinguish:

- admission state;
- active execution state;
- cleanup-relevant state;
- resumable Session state;
- historical terminal/API-retention state.

The v1 data plane may be removed after active and cleanup-relevant state reaches zero. Historical terminal Tasks may retain v1 fields until retention or archival completes; they do not require the wrapper to remain running.

## 16. Known risks

| Risk | Required mitigation |
| --- | --- |
| Full coexistence cost is not justified by continuity requirements | Phase 0 three-option strategy ADR with quantitative product/operations/engineering sign-off |
| SQLite is treated as control authority | Kubernetes CRDs/Leases remain authoritative; SQLite is payload/outbox persistence only |
| Fail-closed webhook shares the singleton controller lifecycle | Separate replicated stateless admission deployment, CA readiness, API-server match conditions, and tested install/uninstall ordering |
| Quarantine or blocked Session has no safe exit | Immutable `AgentExecutionAdjudication` API, admin RBAC, audit, automatic recovery first, and retirement inventory |
| New Lease does not fence the legacy controller Lease | Enumerate and retire every `03b49a10.orka.ai` holder and reject election-disabled controllers before acquiring new ownership |
| Bridge CRD exceeds CEL or API-size limits | Early real-cluster feasibility spike with both-baseline and maximum-shape fixtures |
| Execution snapshot leaks or outlives governance policy | Encryption, least-privilege access, audited export, reference-aware retention/GC, and backup integrity |
| Missing Agent selector silently downgrades current v2 Agents | Source-aware explicit backfill; no default; zero unclassified Agents |
| `runtime.type: opencode` exists in both protocols | Explicit contract binding and source/UID classification; never infer from runtime type |
| V2 OpenCode validation rejects stored v1 OpenCode | Contract-aware validation and bridge ratcheting; preserve unchanged v1 objects |
| V1 OpenCode is patched in place to v2 shape | Require a new Agent UID and new Session/runtime lineage |
| OpenCode ACP image or reviewed model limits are missing | Fail closed on v2 with no v1 fallback |
| Partial v2 state is adopted as v1 | Query all authoritative stores by Task UID before Agent resolution |
| User forges legacy annotations | Fail-closed admission and sealed migration inventory |
| Binding changes after optimistic conflict | Uncached compare-if-absent CAS plus status CEL |
| Mutable Agent/config changes executable request | Immutable content-addressed execution snapshot |
| V1 request is accepted but response is lost | Persisted-frame backstop after first frame; narrowly scoped durable admission ledger before it; no unsafe resend |
| Cancellation accepted but not settled | Nonterminal cancel state and authoritative terminal receipt |
| Session silently crosses protocols | Atomic lineage claim with Session lease |
| Session lock is released before transcript/receipt | Atomic normal terminal Session settlement |
| V2 RuntimeSession deleted before publication finalization | Correct finalization ordering and focused fault tests |
| Current CRD prunes v1 fields | Explicit dual-CRD wave before controller; backup prerequisite |
| Current shared CEL rejects historical v1 object | Validation ratcheting/transition CEL and stored-object status tests |
| Two controllers race across namespaces | Fixed ownership scope and leader-election namespace |
| Multiple Pods write SQLite | Enforce one controller Pod and stop all writers during migration |
| Drain-only races old leader | Durable control object and UID/generation/mode-revision CAS |
| Admission parameter cache observes stale enabled mode | Two-phase closing barrier, durable binding reservations, closure proof, and quarantine of any late binding |
| Wrapper rollout loses active turns | Completed out-of-band drain; durable ledger; no active-turn rollout |
| Inventory misses queued producers | Freeze and inventory Gateways, schedules, monitors, webhooks, delegation |
| Helm rollback leaves incompatible state | Retain dual CRDs and use checkpointed in-place rollback |
| YAML restore recreates UIDs | Coordinated etcd/PVC restore or application-aware rekey/terminalization |
| External v2 assumed available | Keep dispatch fail-closed and document registration-only state |
| V1 broad credentials bypass v2 publication | Round-trip-only legacy fields; explicit policy; no new direct publication |
| TxToken enters v1 env/request | Explicit agent-task rejection and fail-closed TTS rules |

## 17. Primary code areas

API and generated surfaces:

- `api/v1alpha1/agent_runtime_crd_types.go`
- `api/v1alpha1/agent_types.go`
- `api/v1alpha1/task_types.go`
- `api/v1alpha1/task_runtime_types.go`
- `api/v1alpha1/runtime_pool_types.go`
- new execution policy/control/adjudication API types
- generated CRDs, RBAC, deepcopy, and Helm CRDs via `make manifests generate`

Controller and admission:

- `internal/controller/agent_execution_plan.go`
- `internal/controller/opencode_runtime_validation.go`
- `internal/controller/acp_runtime_profile.go`
- `internal/controller/runtime_pool_controller.go`
- `internal/controller/acp_agent_configuration.go`
- `internal/controller/acp_mcp_policy.go`
- `internal/api/context_token_authorization.go`
- `internal/tools/agent_model_update.go`
- `internal/controller/task_controller.go`
- `internal/controller/agent_runtime_controller.go`
- restored `internal/controller/harness_wrapper.go`
- restored `internal/controller/harness_broker.go`
- new `internal/controller/harness_v1_dispatcher.go`
- new binding/snapshot/adoption/quarantine code
- `internal/controller/acp_task_queue.go`
- `internal/controller/acp_dispatcher.go`
- `internal/controller/acp_recovery.go`
- `internal/controller/acp_task_finalizer.go`
- Session control, lineage, settlement, and cleanup code
- Task provenance/finalizer/harness-state admission
- new `cmd/orka-admission/**` and coexistence admission handlers
- `config/webhook/**`, webhook/VAP policies, CA/bootstrap, and match-condition tests
- adjudication controller, evidence validation, audit, and automatic-recovery integration

Runtime and durable stores:

- restored `internal/harness/**`
- retained `internal/harness/v2/**`
- restored `workers/harness/**`
- retained `workers/acp/**`
- `internal/acp/runtime_defaults.go` and `internal/acp/pins.go`
- `workers/acp/images/opencode/**`
- `workers/acp/supervisor/env.go` and `workers/acp/supervisor/provider_proxy.go`
- OpenCode supervisor permission and model-limit handling
- `workers/publisher/**`
- `internal/store/sqlite/**`
- new immutable execution-snapshot store, encryption, access-control, retention, backup, and garbage-collection code/tests
- restored v1 runtime-session store
- new v1 attempt/admission-ledger store

Deployment and release:

- `cmd/main.go`
- restored `cmd/orka-agent-harness-wrapper/**`
- `Makefile`, including `ACP_OPENCODE_RUNTIME_IMG`
- `charts/orka/**`, including admission Deployment/Service/PDB/certificate resources
- `cmd/build/helmify/static/**`
- `config/harness-wrapper/**`
- `config/acp-workload/**`
- CRD apply and source-classification scripts
- `scripts/render-acp-runtime-images.sh`
- inventory, drain, backup, restore, and integrity-check scripts
- release and live E2E workflows

Authority and cross-store settlement:

- `docs/adr/0008-runtime-session-internal-store-first.md`
- `internal/store/kube/session_turn.go`
- `internal/store/outbox_persistence.go`
- Kubernetes-control/SQLite-payload fault-injection and recovery tests

UI:

- `ui/src/schemas/task.ts`
- `ui/src/schemas/session.ts`
- Task list/detail execution-state components
- Session detail and reconciliation-state components
- binding, quarantine, no-execution, blocked-state, adjudication, backend-mode, and snapshot-metadata hooks/tests

## 18. Definition of done

The coexistence milestone is complete only when:

- Phase 0 explicitly selects and funds full active coexistence over the blue/green and zero-active alternatives;
- one explicitly owned controller process safely manages v1 and v2 Tasks concurrently;
- SQLite cannot be opened by multiple controller Pods or overlapping releases;
- every built-in Agent has an explicit immutable protocol selector;
- legacy v1 and managed v2 OpenCode are both supported only through their explicit bindings, and `runtime.type: opencode` is never protocol evidence;
- v1-to-v2 OpenCode migration requires a new Agent UID and new Session/runtime lineage;
- v2 OpenCode preserves reviewed model-limit digesting, provider-proxy credential isolation, native-tool policy normalization, read-intent mutation/Bash/Grep denial, and digest-pinned image admission;
- omitted and explicit-empty OpenCode tool allowlists remain distinct security states through classification, snapshotting, and execution;
- every executable agent Task has an immutable binding and execution snapshot before side effects;
- binding CAS and CEL make replacement impossible;
- valid stored v1 and v2 objects survive the dual CRD and status updates without pruning;
- legacy adoption consults all authoritative stores, never trusts annotations alone, and includes deleting Tasks;
- authoritative no-route-state decisions are durably recorded as immutable `UnboundNoExecution` dispositions;
- ambiguous objects are quarantined rather than guessed;
- automatic recovery and immutable adjudication provide an audited, idempotent exit for quarantined Tasks and blocked Sessions without mutating original evidence;
- Agent/AgentRuntime/config mutation cannot reroute or alter bound execution;
- v1 ambiguous submission never causes automatic replay;
- v1 and v2 dispatchers enforce executor exclusivity through binding digests;
- Session lineage includes namespace UID, is claimed atomically, and prevents implicit cross-version reuse or same-name namespace attachment;
- terminal Session settlement follows Kubernetes-authoritative control, SQLite payload/outbox commit, Kubernetes CAS/lease release, and deferred projection activation in that order;
- v2 publication finalization occurs before RuntimeSession deletion;
- route-aware cleanup is proven under completion, deletion, cancellation, timeout, restart, and takeover;
- v1 cannot satisfy or claim v2 strict-governance requirements;
- no v2 failure or rejection falls back to v1;
- raw credentials and TxTokens never enter unsafe surfaces;
- backend modes are durable and revisioned;
- the separate replicated admission service remains available through controller rollouts, fails untrusted writes closed, and cannot deadlock controller cleanup;
- every legacy `03b49a10.orka.ai` Lease holder and election-disabled controller is fenced before new ownership begins;
- bridge CRDs pass real-cluster CEL/static-cost and size-headroom gates;
- execution snapshots satisfy encryption, access, retention, garbage-collection, backup, and audited-export requirements;
- v1 admission can enter drain-only while cleanup remains operational;
- all Task producers are included in drain and inventory closure;
- required fresh-install, both-direction upgrade, mixed-mode, restart, and restore workflows pass repeatedly;
- safety SLOs and the canary soak gate pass;
- in-place rollback and coordinated disaster restoration have been rehearsed;
- operators can produce separate zero-admission, zero-execution, zero-cleanup, and zero-resumable-v1-state proofs before staged removal.
