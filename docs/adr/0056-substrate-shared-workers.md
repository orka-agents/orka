# 56. Let opted-in Substrate profiles share workers, with interim termination evidence

Date: 2026-11-18

## Status

Accepted as an interim decision, approved by the project owner. Only for
`SubstrateWorkspaceProfile`s that set `spec.placement.sharedWorkers`, it supersedes:

- the **Single-Actor workers** bullet of ADR 0049;
- the clause "termination is proven by worker Pod UID absence, Actor assignments, and
  the worker epoch" in ADR 0049's **Least privilege** bullet;
- the matching ADR 0031 sentences "Drain the supervisor and exact single-Actor worker
  before snapshot capture" (only its single-Actor requirement) and "Delete and observe
  the exact worker Pod before deleting the source Actor and reporting suspension".

Every other profile keeps ADRs 0031 and 0049 unchanged. ADR 0030's full-memory gate and
ADR 0031's data-only cold path stay in force. The provider side is `orka-workspace`
ADR 0008.

## Context

ADR 0049 runs each Substrate Actor on a private worker sized for exactly that Actor.
The provider proves an Actor stopped by deleting its worker Pod and observing the exact
Pod UID absent. Every cold start and resume therefore waits for a new worker Pod.
kagent and Google AX resume onto workers that are already running.

Agent Substrate v1.0.0 (`b34fbee742906494b5eb4f1f36987176bf8770a4`) is built for shared
workers, but gives no per-Actor proof of sandbox exit:

- Each Actor has its own `runsc` sandbox, cgroup leaf and network namespace. A `VOLUMES`
  checkpoint runs only `runsc fscheckpoint` over durable directories, with no memory
  image, and deactivates the Actor's tunnel first.
- Teardown after a checkpoint is best effort (`_ = terminate()` in
  `cmd/ateworker-gvisor/ckptcgroup.go`). The control plane still marks the Actor
  `SUSPENDED` and releases its assignment.
- Deleting a `SUSPENDED` Actor tears nothing down. A leaked sandbox keeps its memory,
  without a tunnel, until the worker Pod dies.
- A worker restart is strong evidence: a new process registers a higher epoch and the
  control plane crashes the earlier Actors.
- Co-hosted Actors share one privileged worker process, the node's `/var/lib/ate`, and
  their egress mTLS keys. Upstream's threat model states that workers are not a
  boundary between Actors.
- Placement checks no Atespace or tenant. It matches only Worker labels, which are
  copied from WorkerPool metadata labels, so any template whose selector those labels
  satisfy, including an empty one, can land on a worker.
- A worker Pod has a hardcoded 3600 s termination grace, within which the worker drains
  for at most 30 minutes.
- The egress gateway caches an Actor's policy for its configurable
  `--egress-policy-cache-ttl` (default 10 s). Expiry governs only new requests.
- The worker's DNS relay caches the policy for 5 s and keeps a stale policy when a
  refresh fails.
- A checkpoint's `Tunnel.Deactivate` stops ingress and TCP egress and drains their
  streams. It does not stop the DNS relay, which ends only in the later, best-effort
  teardown.

## Decision

### Opt-in and scope

- **A frozen profile field.** Sharing is opt-in per profile through
  `spec.placement.sharedWorkers: {maxActorsPerWorker: 2..16, maxWorkerLifetime: 3h..24h,
  default 24h}`. The field is part of the immutable, hashed profile spec, so opting in
  requires a new profile and changes the frozen profile hash. An absent field keeps
  today's allocation path exactly. The provider's leader sweep adds only a Kubernetes
  list of shared pools.
- **One pool, one scope.** A shared pool serves exactly one workspace namespace and
  Atespace, profile UID and hash, Actor size, and operator WorkerPool. This scope is the
  provider's bookkeeping. It does not by itself keep other tenants off the worker.
- **Pool identity and ledger.** A shared pool carries only a provider-owned label with a
  random journaled value, never the operator pool's labels, and the derived template
  selects only that label. A resourceVersion-protected ledger on the pool lists every
  hosted allocation and Actor UID.
- **A foreign Actor fails closed.** Any assigned Actor that is not in the ledger fails
  observation, and the pool stops admitting and is retired through the strong path.

### Tenancy: a dedicated install only

- **The flag.** Sharing is allowed only when the provider runs with
  `--substrate-dedicated-install` (default false). The operator thereby asserts that no
  tenant other than this Orka creates Actors or ActorTemplates on the Agent Substrate
  install.
- **Without it.** An opted-in profile's allocation is refused, with the error on the
  workspace's `Provisioned` condition, and existing shared pools are strongly retired.
- **Verified even with it.** The provider holds global viewer and lists every
  ActorTemplate. A template whose selector is empty or is satisfied by the pool's labels
  is foreign if it is outside the pool's Atespace, or inside it but not a derived
  template this provider recorded. Any foreign template fails closed: the pool stops
  admitting and is strongly retired. This runs before each claim, before each Resume,
  and on every sweep.
- **Residual risk.** A foreign template or Actor created between two checks can reach a
  shared worker until the next check, the ledger's assignment check, or observation
  detects it. Detection then strongly retires the pool. The checks bound this race; they
  do not prevent it.

### Never shared

These keep the private single-Actor path:

- operator-tainted, quarantined, or `Failed` workspaces;
- restores and recoveries from a public checkpoint (`restoreFrom`);
- workspaces without a `DataOnly` (`VOLUMES`) profile;
- every profile without the field.

The provider also requires gateway injection of the provider-proxy credential, so the
long-lived provider token never enters a shared sandbox. Suspicion is re-checked when a
slot is claimed (the allocation moves to a private worker) and immediately before
Resume (it fails closed). A pool hosting a workspace that becomes tainted, quarantined,
or `Failed` is strongly retired.

### Interim termination evidence

The interim evidence replaces worker-Pod absence on the normal path. All four must
hold:

1. **The Actor is suspended.** It is `SUSPENDED` (and then deleted) at a version after
   the issued suspend, with the expected new `VOLUMES` snapshot generation. A Stop issues
   the same suspend for evidence only.
2. **The fenced worker no longer hosts it.** Its assignment is absent from the same
   `ACTIVE` worker: the same Worker UID and Pod UID, with `registered_epoch` and
   `observed_epoch == registered_epoch` unchanged, and the worker version is re-read
   unchanged.
3. **Everything it could use is revoked.**
   - Its egress policy, including credential injection, is replaced by deny-all and
     verified.
   - `SuspendActor` is issued only after deny-all has been stored for longer than the
     larger of the gateway's declared policy cache and the DNS relay's 5 s cache, plus
     5 s: 15 s by default.
     - The declared cache is the provider's `--substrate-egress-policy-cache-ttl`,
       which operators must set to the gateway's own value. The provider cannot read
       the gateway's value, and upstream offers no enforcement acknowledgement.
     - The checkpoint then deactivates the tunnel, which ends streams admitted earlier.
   - Residual risk:
     - a mis-declared gateway TTL shortens the window;
     - the DNS relay keeps answering by its last fetched policy until teardown;
     - if its refresh failed, that policy is the stale allowlist;
     - for a leaked sandbox, the DNS relay may keep answering until the worker Pod is
       gone.
   - The instance's Orka credentials are made unusable by Core, asynchronously (see the
     next section).
4. **The residue is bounded in time.** `maxWorkerLifetime` is a hard bound on the worker
   Pod, not on when its deletion begins:
   - strong retirement starts the Pod's full 3600 s grace plus 5 minutes before it;
   - admission closes an hour earlier, so a pool admits for `maxWorkerLifetime − 2h05m`
     after birth (55 minutes at the 3h minimum, 21h55m at 24h);
   - every retirement first deletes the worker Pod directly, UID-preconditioned, with
     a grace that ends 5 minutes before the bound (at most 3600 s), and only then the
     pool. A kubelet that has started a longer stop does not shorten it for a later
     delete, as live validation showed, so the first delete must carry the bound; the
     API server never lengthens an in-progress grace, so the pool's cascade cannot
     undo it. Neighbours drain and Core evicts them inside that grace; a retirement
     issued late, after an outage, can leave too little time, and a neighbour that
     cannot checkpoint in time fails without replay with its last checkpoint;
   - a Pod still present 5 minutes before the bound is re-deleted with a one-second
     grace, never zero, so the provider's own delete stays on the kubelet path (other
     deletion paths are an accepted restriction, below). That re-delete is best
     effort for a stop already begun. The bound is enforced only through graceful
     deletion the kubelet honours, with a capped grace: a kubelet restart during a
     stop restarts it with the grace the Pod object then carries (the capped value,
     or one second after escalation, never the original 3600 s), so it can extend
     termination by up to one capped grace, and a node or kubelet failure is outside
     the bound. That is the accepted limit of this hard bound; no stronger mechanism
     exists within the provider's RBAC, which has no node access. Each such delete is
     tied to the retired pool's UID and preconditioned on the Pod's UID, so a pool
     replaced under the same name never loses its Pods;
   - all of this runs in its own provider loop every 10 s, using Kubernetes alone,
     never behind the native sweep (which has a one-minute deadline), and escalation
     never waits for provenance discovery: a pool due by its annotations' bound under
     the 24h ceiling, which provenance can only shorten, is escalated first;
   - unreadable or inconsistent lifetime evidence retires the pool at once;
   - the bound never exceeds the pool's server-set creation time plus the shortest
     lifetime frozen in the journals that recorded it, nor that creation time plus
     24h, so rewriting the pool's own annotations cannot extend it. A pool no journal
     recorded, such as an unused warm pool, is bounded by the lifetime of the immutable,
     hashed profile its scope names, or by 3h if that profile does not match. That
     scope counts only if the pool's immutable name commits to it (a hash of the scope
     in the name); a name committing to another scope is doubt, and a pool named before
     the scheme gets 3h;
   - when the journals cannot be listed, admission (claims and Resumes) closes at
     once. Inside a 60 s outage window, retirement and escalation keep the annotations'
     bound under the 24h ceiling, so one transient failed List never retires or
     escalates a healthy pool. Only if the listing still fails 60 s later does the
     ceiling drop to creation plus 3h, and every shared pool is then strongly
     retired, so one transient API error never evicts every shared Actor. That 60 s limit is independent of the discovery
     budget: the pool List has its own deadline, so a journal read that uses up the
     budget never keeps the pools unlisted, and a failed pool List is retried within
     10 s while a failed journal read keeps admission closed;
   - discovery does not depend on the removable shared label;
   - no failed ledger write can block the deletion.

### Credentials Core owns

Core makes these unusable, asynchronously:

- **The attachment bearer** authenticates requests into the runtime, which the router
  no longer delivers to the sandbox.
- **The pool's controller bearer and capability secret** are accepted by the MCP broker
  only while the RuntimePool's exact active instance is set.
  - The provider reports `Stopped` first. Core clears the active instance on its next
    RuntimePool reconcile, which the workspace watch triggers and the 1 s retirement
    progress requeue bounds while the controller runs. Throughout that interval the
    leaked sandbox has no tunnel and deny-all egress.
  - Core rotates both, by unpublishing and replacing the bound auth Secret, before any
    successor boots.

### Strong fallback

- **When it applies.** The strong path replaces the interim evidence on any doubt:
  - the assignment is still present;
  - the worker's epoch, identity or `ACTIVE` state changes mid-proof;
  - the worker is terminating;
  - a foreign template or Actor appears;
  - any upstream error.

  It also applies to every error on a shared worker after the suspension intent is
  persisted, apart from a retried journal write conflict. That covers an unreadable
  Actor, failed egress revocation, failed template verification, invalid snapshot
  evidence, any Tag-capture failure, and a suspend issued whose effect is unresolved.
  These always drive the pool's deletion and the absence proof to completion, from
  every retirement pass and from the provider's Kubernetes-only bounds loop, even after
  a crash right after the intent was saved or while the native API is down:
  - a persisted strong marker;
  - a hosted journal's recorded failure;
  - a hosted workspace that is tainted, quarantined or `Failed`, including the workspace
    of any unreleased journal that recorded the pool, whatever its ledger entries say;
  - a pool ledger saved as `Retiring` or `Quarantined`;
  - a retirement-intent annotation, which the provider writes when the pool's Delete
    itself fails. Admission refuses such a pool, and every bounds pass retries the Delete.

  Strong triggers are persisted under a fresh bounded context detached from the
  request's deadline. A rejected or timed-out evidence-only Stop suspend is doubt too,
  and so is a Stop that finds its Actor still `SUSPENDING` after that suspend: a Stop
  needs no data, so it never waits for or continues the suspension.
- **No replay.** An unresolved suspend is never replayed against a `RUNNING` Actor. Once
  the worker Pod is proven absent, the instance is reported lost and retires through
  Stop with its last checkpoint.
  - A shared Actor left `SUSPENDING` by a failed SuspendActor (for example a transient
    `FinalizeSuspended` error) is different. The provider continues that same upstream
    suspension, a bounded number of times, before the deleted pool's worker Pod is gone.
    Agent Substrate's workflow is re-entrant and lease-serialized: it reuses the
    persisted snapshot, and atelet answers a checkpoint it already committed. So no new
    checkpoint is taken and no Tag is invented. If the continuation loses that race, the
    instance is lost as above. If every continuation ends without a result, the
    suspension is doubt and takes the strong path.
- **What it does.** The provider closes the pool and deletes it. Core observes the
  terminating worker and drains each co-hosted Actor for data-only suspension inside
  its eviction window (ADR 0046). Every hosted instance then proves termination by exact
  worker Pod UID absence.

### Core needs no code change

- **Core never decides termination for native processes.** It accepts the provider's
  exact, Core-authorized terminal claim, and it never observes Substrate worker Pods to
  decide termination (`externalObservedInstanceTerminated`).
- **The binding already names the Actor and runtime, not the worker Pod.**
  - The private bootstrap binding is to the opaque per-sequence runtime UID
    (`externalNativeRuntimeUID`).
  - The sealed bootstrap is bound to the exact Actor identity (Atespace, name, UID).
  - The worker Pod reference is only an attestation and eviction fence. It stays
    correct when several instances name the same worker, because a Pod replacement
    kills every hosted sandbox. No binding moves to the worker epoch, and nothing
    changes for non-shared profiles.
- **The opt-in needs no new type or version.** Core reads Substrate profiles as
  unstructured data and freezes them by hash. The new field changes that hash without
  any new Core type or `orka-workspace` module version.

## Consequences

- **Faster starts, fewer Pods.** A turn that lands on a shared worker with a free slot
  skips worker birth, and up to 16 Actors of one namespace share one worker Pod.
- **Weaker than ADRs 0031 and 0049.** A per-Actor stop is proven by control-plane
  bookkeeping plus revocation, not by observed sandbox exit. The owner accepted this
  because sharing is opt-in, limited to a dedicated and continuously verified install,
  hard-bounded in time, and revoked.
- **Residual risk: a leaked sandbox.** If teardown failed, that sandbox's memory may
  persist on the node until the worker Pod is gone. That memory includes workspace data
  and the instance's credentials, which Core makes unusable asynchronously as
  described above. The `maxWorkerLifetime` bound holds only while the provider's
  bounds loop can reach the Kubernetes API to delete the pool and re-delete its Pod,
  and while the kubelet honours that deletion. A provider or API server outage that
  spans the retirement deadline extends the leak until both recover; the bounds loop
  then retires the pool on its next pass, because the deadline is read from Kubernetes
  state, not from memory.
- **Residual risk: foreign tenants.** Isolation from other tenants rests on the
  operator's dedicated-install assertion. Verification detects a violation after it
  happens and then strongly retires the pool.
- **Accepted restriction: Pod absence is kubelet-confirmed only for the provider's own
  deletes.** The provider deletes worker Pods only with a non-zero grace. PodGC (for a
  Pod bound to a deleted Node object, such as a partitioned node's) and any force
  delete remove the Pod object without kubelet confirmation. Exact Pod UID absence then
  does not prove the hosted sandboxes stopped. Orka's private-path proof (ADRs 0031 and
  0049), which also rests on exact worker Pod UID absence, has the same limit. Operators
  must not force-delete Substrate worker Pods, and must not delete a partitioned node's
  Node object without first fencing that node.
- **Threat model: writers in the provider's namespaces are operator-equivalent.** A
  principal with WorkerPool or ConfigMap write access there can already disrupt or
  delete workers. Making the lifetime bound tamper-resistant against such a writer is
  defence in depth: it relies only on immutable, server-set data (the pool's name, UID
  and creationTimestamp) and the provider's own journals.
- **Residual risk: worker compromise.** A compromised worker exposes every co-hosted
  Actor of the same namespace and profile.
- **Strong retirements disturb neighbours.** A strong retirement suspends a pool's
  neighbours through the eviction path. A neighbour that cannot drain in time fails
  without replay.
- **Exit criterion.** Replace the interim evidence with an upstream per-Actor
  sandbox-exit attestation when Agent Substrate provides one. Until then, do not widen
  this decision.
- **Live validation is still needed.** Capacity, label propagation, `observed_epoch`,
  egress revocation on a running Actor, and eviction of a strongly retired shared worker
  must be validated live before the opt-in is recommended.
