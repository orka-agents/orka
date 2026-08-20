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
   snapshot — which the execution-workspace contract prohibits.

## Decision

**A Substrate workload backend for workspace-backed RuntimePools**, sharing
every admission, fencing, drain, and recovery barrier with the Deployment and
Agent Sandbox backends, with these Substrate-specific mappings:

- **Derived, controller-owned ActorTemplate.** `templateRef` on the Task names
  the operator's *infrastructure* template (required for Substrate, unlike
  agent-sandbox where it is rejected). The pool reconciler copies every spec
  field except `containers` from it and injects the single canonical supervisor
  container: the controller-approved immutable runtime image, the exact fence
  environment as literals, and read-once bootstrap secret references. Epoch-
  scoped pool Secrets are created in the template's namespace so the provider
  can resolve them. Template drift (either the rendered container or the copied
  infrastructure) changes the revision digest and triggers the standard
  drain-then-replace rollout.
- **Read-once bootstrap secrets.** Provider workspaces have no Secret mounts,
  so the supervisor accepts `ORKA_ACP_{CONTROLLER_TOKEN,CAPABILITY_SECRET,
  PROVIDER_TOKEN}_BOOTSTRAP` environment variables as read-once alternatives to
  the file paths (file always wins; the variable is unset immediately after the
  single read, mirroring the workspace agent's bootstrap-token pattern). The
  values necessarily transit the provider's workload build — the provider
  materializes the execution environment and can read anything inside it by
  construction; the containment boundary is that the tokens are pool-scoped,
  epoch-scoped, Orka-minted, and never suspended into snapshots.
- **Exact-instance identity.** The rendered environment pins
  `ORKA_ACP_POD_UID=actor:<actorID>`, so the supervisor derives
  `RuntimeInstanceID = actor:<actorID>.<bootID>` and the controller validates
  the probe against a synthetic instance carrying the same identity. Every
  actor lifetime is exactly one fresh boot (`ResumeActor(boot=true)` once,
  recorded in a pool annotation), so a boot ID can never originate from a
  restored snapshot; a supervisor process restart inside the actor changes the
  boot ID and recycles the exact instance, exactly like the Pod paths.
- **Router transport.** The dispatcher and the pool probe client reach
  Substrate instances through a dedicated transport that dials the router for
  any host under the actor DNS suffix while preserving the logical route host
  as the HTTP Host header (the proven MCP actor pattern), and refuses every
  other host. `ActiveInstance.PodAddress` carries the route host;
  `PodNamespace/PodName` carry the provider worker placement.
- **No suspension, ever.** The backend never calls `SuspendActor`. Scale-down
  and rollout use the standard authenticated drain plus persisted quiescence
  barriers and then `DeleteActor` directly — not the legacy
  scrub-suspend-delete executor flow. If the provider suspends or snapshots a
  booted actor unilaterally, the pool fails closed: admission closes, the
  actor is deleted, and the replacement boots from scratch. Operators must not
  enable provider-side idle suspension for ACP actor templates.
- **Fail-closed cleanup.** Pool finalization deletes the actor through the
  control API before releasing the finalizer; an unreachable Substrate control
  plane blocks pool deletion rather than leaking a credentialed runtime
  workload. The derived template and template-namespace Secrets are removed in
  the same pass, and stopped idle pools are garbage-collected by the existing
  dispatcher reaper.

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
  valid `workerPoolRef`, `runsc`, and (if used) `snapshotsConfig` fields; its
  containers are never executed by ACP pools.
- The Substrate control plane and router are reachable from the Orka
  controller; the actor's egress must reach the Orka controller API and the
  provider proxy (Vekil). Cross-cluster topologies are not supported: the
  control API, router, and Orka must share a cluster and trust configuration
  (`--substrate-api-ca-file` or the explicit insecure opt-in).
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
