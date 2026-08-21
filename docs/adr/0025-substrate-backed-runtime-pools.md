# 25. Substrate-backed ACP RuntimePools without suspension

Date: 2026-08-19

## Status

Accepted. Implements Phase 2 of issue #343 on the workspace-provider-backed
RuntimePool seam established by ADR 0024.

## Context

Agent Substrate hosts workloads as gVisor-isolated Actors placed on WorkerPool
workers, reached exclusively through the atenet-router by logical Host header
(`<actorID>.<actorDNSSuffix>`), and controlled over the public `ateapi.Control`
gRPC API. Three provider facts constrain the design:

1. **`CreateActor` injects nothing.** The request carries only
   `{actorID, templateNamespace, templateName}`; image, command, env, ports,
   worker placement, runsc build, and snapshot location are all defined by an
   `ate.dev/v1alpha1` ActorTemplate — a CR in Orka's own cluster whose
   `env[].valueFrom.secretKeyRef` entries the provider resolves at workload
   build.
2. **Actors have no dialable address or Kubernetes identity.** There is no
   listable Pod, no UID, and no per-instance endpoint; routing is Host-based
   through the router, and the only identity is the actor ID string.
3. **Suspension checkpoints process memory.** The gVisor checkpoint contains
   "memory, sentry state, and filesystem deltas" and lands in provider snapshot
   storage. A running ACP supervisor holds the pool controller token, the HMAC
   capability secret, and the live provider-proxy bearer in process memory, so
   any suspension of a booted supervisor writes live credentials into a
   snapshot — which the execution-workspace contract prohibits. Live
   validation additionally showed the provider builds a **golden snapshot per
   ActorTemplate that carries `snapshotsConfig`** by booting one instance and
   checkpointing it — so a derived template with resolved secret env would
   leak credentials at rest even before Orka creates its own actor.

## Decision

**A Substrate workload backend for workspace-backed RuntimePools**, sharing
every admission, fencing, drain, and recovery barrier with the Deployment and
Agent Sandbox backends, with these Substrate-specific mappings:

- **Derived, controller-owned, credential-free ActorTemplate.** `templateRef`
  on the Task names the operator's *infrastructure* template (required for
  Substrate, unlike agent-sandbox where it is rejected). The pool reconciler
  copies every spec field — including the provider-required `snapshotsConfig` —
  except `containers` from it and injects the single canonical supervisor
  container: the controller-approved immutable runtime image, the exact fence
  environment as literals, and **no credential material of any kind** (no
  Secret references, no `valueFrom`, no bootstrap values). The only
  bootstrap-related field is the public per-pool nonce
  `ORKA_ACP_CREDENTIAL_BOOTSTRAP_NONCE`. Template drift (either the rendered
  container or the copied infrastructure) changes the revision digest and
  triggers the standard drain-then-replace rollout.
- **Post-boot credential bootstrap.** Because the provider golden-snapshots
  every template that carries `snapshotsConfig` (and `snapshotsConfig` is a
  required field), nothing secret may exist in the workload until after the
  real actor boots. The supervisor detects the nonce env, boots into an
  awaiting-bootstrap phase that serves only a minimal `/v2/health`
  (`lifecycle: booting`) and a one-time `PUT /v2/credential-bootstrap`
  endpoint, and blocks until seeded. After the actor boots, the pool
  reconciler seeds the pool controller token, HMAC capability secret, and
  provider-proxy bearer over the router transport, gated by the
  `X-Orka-Credential-Bootstrap-Nonce` header. First write wins; an identical
  repeat is acknowledged (idempotent controller retries); a different payload
  returns 409 and the controller recycles the exact instance rather than
  trusting a workload seeded by another party. The nonce is public per-pool
  entropy stored as a third key of the pool auth Secret — it fences *which*
  workload the controller seeds, it grants nothing. All pool Secrets stay in
  the controller's runtime namespace; the template namespace never holds
  secret material. The seeded values travel over the same cluster-trusted
  channel that carries every subsequent Authorization header, and the
  supervisor consumes them via the existing read-once bootstrap variables.
- **Exact-instance identity.** The rendered environment pins
  `ORKA_ACP_POD_UID=workspace:<sha256(actorID)>`, so the supervisor derives the
  opaque `RuntimeInstanceID = workspace:<sha256(actorID)>.<bootID>` and the
  controller validates the probe against a synthetic instance carrying the
  same identity. Every
  actor lifetime is exactly one fresh boot (`ResumeActor(boot=true)` once,
  recorded in a pool annotation), so a boot ID can never originate from a
  restored snapshot; a supervisor process restart inside the actor changes the
  boot ID and recycles the exact instance, exactly like the Pod paths. This
  opaque Orka-derived fence is the only actor-related value that appears in
  public Task status (`status.execution.runtimeInstanceID`, required for
  fencing, recovery, and artifact authorization); the raw Actor ID, route
  hosts, worker names, snapshot URIs, and every provider-assigned identifier
  stay out.
- **Router transport.** The dispatcher and the pool probe client reach
  Substrate instances through a dedicated transport that dials the router for
  any host under the actor DNS suffix while preserving the logical route host
  as the HTTP Host header (the proven MCP actor pattern), and refuses every
  other host. `ActiveInstance.PodAddress` carries the route host;
  `PodNamespace/PodName` carry the provider worker placement.
- **Credential-free golden snapshots.** `snapshotsConfig` is copied verbatim
  (the provider requires it and uses it for the per-template golden-snapshot
  build). That checkpoint is safe because it captures a waiting,
  credential-free supervisor plus the public nonce — never a seeded one: the
  golden build boots its own instance from the template, which parks in the
  awaiting-bootstrap phase and is checkpointed unseeded. Only the pool's real,
  booted actor is ever seeded.
- **No checkpoint of a live supervisor, ever.** The provider deletes only
  suspended actors, and suspending a live workload checkpoints its memory —
  so teardown is staged: after the standard authenticated drain plus
  persisted quiescence barriers, the controller first destroys the workload's
  memory by deleting its assigned single-workload worker Pod (guarded by the
  provider's `ate.dev/worker-pool` label; the worker Deployment replaces the
  Pod fresh), confirms the Pod is gone, then calls `SuspendActor` purely to
  settle the memoryless actor into the deletable suspended state — with
  nothing left to checkpoint — and finally `DeleteActor`. A pool annotation
  records the in-progress teardown so every reconcile resumes it before any
  other decision. If the provider suspends or snapshots a booted actor
  unilaterally, the pool fails closed: admission closes, the actor is
  recycled, and the replacement boots from scratch. Operators must not enable
  provider-side idle suspension for ACP actor templates.
- **Fail-closed cleanup.** Pool finalization deletes the actor through the
  control API before releasing the finalizer; an unreachable Substrate control
  plane blocks pool deletion rather than leaking a credentialed runtime
  workload. The derived template is removed in the same pass; pool Secrets
  live in the runtime namespace and are swept by the generic child cleanup.
  Stopped idle pools are garbage-collected by the existing dispatcher reaper.

## What remains fail-closed, and why

- **Actor suspend/resume and snapshot restore.** Deliberately unsupported: any
  checkpoint of a booted supervisor captures live credentials (most critically
  the shared provider-proxy bearer, which is not pool-scoped). Warm
  suspend/resume becomes viable only with provider-side credential-safe
  sessions — e.g. short-lived per-session certificates via the
  `SessionIdentity.MintCert` API that exists in the proto but is rejected as
  unsupported today — or a supervisor architecture that provably holds no
  long-lived secrets in memory. Until then, "resume" is recreation: fresh
  actor, fresh boot, session continuity through the existing RuntimeSession
  generation-increment recreation path.
- **Substrate-only Task options** (`boot`, `poolRef`, `snapshot`,
  `hibernation`, `onDetach`, `cleanupPolicy: retain`) remain rejected before
  any workspace or RuntimePool demand exists.
- **SubstrateActorPool warm actors** are not used: pre-created actors would
  share one template (and therefore one credential generation) across pools.

## Operator requirements

- The infrastructure ActorTemplate must exist, be operator-owned, and carry
  valid `workerPoolRef`, `runsc`, and `snapshotsConfig` fields (the provider
  requires a snapshot location); its containers are never executed by ACP
  pools.
- The controller reads ActorTemplates cluster-wide, but writing the derived
  template requires an operator-provided RoleBinding for the controller
  ServiceAccount in the template namespace (`ate.dev` ActorTemplates, full
  CRUD — no Secret access: nothing secret exists in the template namespace).
- Credential-safe teardown additionally requires Pod `get`/`list`/`delete` in
  the provider worker namespace, so the controller can destroy a live
  workload's memory before settling and deleting its actor.
- The referenced WorkerPool is dedicated to Orka ACP runtimes. Before an
  Actor is created or credential-seeded, the controller materializes an
  egress-only default deny plus DNS, controller API, and provider-proxy
  allowlists selecting the upstream `ate.dev/worker-pool` label. The
  controller therefore also needs NetworkPolicy CRUD in the worker namespace;
  finalization retains those policies until the Actor is proven gone.
- The Substrate control plane and router are reachable from the Orka
  controller; the actor's egress must reach the Orka controller API and the
  provider proxy (Vekil). Cross-cluster topologies are not supported: the
  control API, router, and Orka must share a cluster and trust configuration
  (`--substrate-api-ca-file` or the explicit insecure opt-in).
- The provider's workload runtime must grant the ACP supervisor process the
  same narrow capability set as its Kubernetes Pod
  (`CHOWN`, `KILL`, `SETGID`, `SETUID`): the supervisor assigns each session
  tree to a distinct never-reused UID/GID and launches ACP children under that
  identity, and fails closed when the drop is unavailable. The e2e topology
  carries a reviewed atelet patch for this; production providers need an
  equivalent capability policy.
- Provider-side idle suspension must be disabled for ACP actor templates.
- Dispatch is gated by `--substrate-enabled` plus
  `--acp-workspace-dispatch-enabled`; either missing fails closed with the
  reason projected to `Task.status.executionWorkspace`.

## Consequences

- The provider-neutral contract from ADR 0024 now has two live backends behind
  identical barriers; the dispatcher, fencing, recovery, publication, and Task
  API remain unchanged.
- Live E2E promotion (actor → route → execute → drain → replace → cleanup, and
  gVisor compatibility of the supervisor's rlimit/procfs expectations) is
  tracked by issue #343 and exercised on the Substrate kind topology.
