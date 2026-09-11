# Orka Harness v1/v2 Isolated Coexistence Plan

**Status:** Accepted — Revision 8
**Prepared:** August 7, 2026
**Supersedes:** Revision 7

## 1. Decision

Harness v1 and harness v2 may run on the same Kubernetes cluster only as two
independent Orka installations with disjoint namespace ownership. Each
controller starts in exactly one static mode:

- `harness-v1` serves the legacy turn-oriented wrapper contract;
- `harness-v2` serves the ACP RuntimePool and RuntimeSession contract.

There is no `dual`, `auto`, or `harness-v1-drain` mode. A running installation
does not switch modes. Tasks, Agents, AgentRuntimes, and Sessions do not migrate
between modes, and neither controller falls back to the other protocol.

```text
one Kubernetes cluster

  v1 release                         v2 release
  -----------------------------      -----------------------------
  release/watch namespace:           release/watch namespace:
    orka-v1-system                      orka-v2-system
  mode: harness-v1                   mode: harness-v2
  own Lease/PVC/Secrets/Service      own Lease/PVC/Secrets/Service
  harness wrapper                    ACP RuntimePools/runtime namespace

                 shared, platform-owned CRD schemas
```

The shared Kubernetes API server, cluster nodes, and centrally managed CRD
schemas are infrastructure coupling. They are not execution-plane coupling.

## 2. Why this replaces active coexistence

The previous plan put both protocols behind one controller and one Task
population. Making that safe required dynamic backend modes, route bindings,
classification, binding reservations, quarantine, adjudication, global Lease
fencing, and cross-protocol retirement state.

Namespace isolation removes the ambiguous ownership that those mechanisms
addressed. Protocol choice is a property of an installation and its namespace,
not a per-Task routing decision. Consequently, this plan does not introduce:

- `AgentExecutionControl`;
- `AgentExecutionPolicy`;
- `AgentExecutionAdjudication`;
- dynamic `enabled`, `closing`, `drain-only`, or `disabled` backend modes;
- cross-protocol Task binding, classification, quarantine, or adjudication;
- transcript-bootstrap migration or cross-protocol Session lineage.

Harness-specific safety remains within each path. In particular, ACP v2 keeps
its RuntimeSession fencing, prompt-attempt, publication, external-effect, and
`OutcomeUnknown` rules. Harness v1 must not replay an ambiguously accepted
turn.

## 3. Required invariants

### 3.1 Static controller identity

Every controller process requires:

```text
--controller-mode=harness-v1|harness-v2
--watch-namespace=<one non-empty namespace>
--leader-elect=true
```

`ORKA_CONTROLLER_MODE` is the environment equivalent of
`--controller-mode`. Invalid or omitted modes fail startup. Cluster-wide watch
scope fails startup.

The watched namespace must carry the matching administrative claim:

```yaml
metadata:
  labels:
    orka.ai/controller-mode: harness-v1 # or harness-v2
```

A missing or mismatched label fails startup. The label is an installation
identity, not a runtime switch. Changing it in place is unsupported; create a
new namespace and installation instead.

Namespace bootstrap is fail-closed and write-once with respect to that claim:

- if the namespace is absent, create it with `orka.ai/controller-mode` in the
  same Kubernetes API write, before creating Secrets or workloads;
- if it already exists, proceed only when its exact name and mode claim match;
  same-mode reuse preserves the claim and any unrelated labels;
- never adopt or relabel an existing unlabeled or opposite-mode namespace;
- if another installer wins the create race, reread the namespace and proceed
  only when the resulting identity is an exact same-mode match.

Canonical script-based installs enforce this contract through
`scripts/lib/ensure-static-mode-namespace.sh`. Additional namespace metadata
may converge only after an atomic test confirms that the mode claim is still
unchanged.

The ordinary leader-election ID may be the same in both installations because
each Lease lives in its controller's distinct watched namespace. There is no
cluster-global harness ownership Lease and no legacy-Lease acquisition bridge.

### 3.2 Disjoint ownership

The two releases must have:

- different Helm release names;
- different controller release namespaces;
- different, non-empty watched namespaces;
- different controller Services and API endpoints;
- different ServiceAccounts, Secrets, RBAC bindings, and leader-election
  Leases;
- different SQLite databases and PVCs;
- different worker and harness data-plane resources;
- for v2, a runtime namespace not used by another Orka installation;
- NetworkPolicies that do not admit traffic from the other execution plane.

The packaged Helm and Kustomize installations use the controller release
namespace as the watched namespace. This keeps the controller, workload
objects, namespaced RBAC, Lease, and data plane inside one ownership boundary.
The v2 runtime namespace remains separate. Custom packaging must preserve the
same isolation if it separates the controller and watched namespaces.

Neither controller may list, watch, mutate, finalize, or clean up namespaced
objects in the other controller's watch namespace. RBAC is the enforcement
boundary; cache configuration alone is insufficient.

Cluster-scoped CRDs and admission resources have one designated platform
owner. Cluster-scoped reconcilers, including gateway and workspace-provider
infrastructure, are owned by the `harness-v2` installation and are not started
by `harness-v1`. Common admission protections are installed once. No two Helm
releases independently own the same cluster-scoped admission object.

### 3.3 One protocol per installation

In `harness-v1` mode:

- agent Tasks may use only the v1 contract;
- the legacy dispatcher and wrapper client are enabled;
- ACP RuntimePool dispatch, runtime reconciliation, and ACP cleanup are not
  execution alternatives;
- cluster-scoped gateway/workspace controllers are disabled.

In `harness-v2` mode:

- agent Tasks may use only the v2 contract;
- the ACP dispatcher and RuntimePool/RuntimeSession controllers are enabled;
- the v1 dispatcher and wrapper path are not started;
- missing ACP capacity or configuration fails closed without v1 fallback.

Native AI and container Task paths remain governed by their existing
namespace-scoped contracts. They do not authorize cross-namespace access.

An Agent or AgentRuntime contract selector, where retained to discriminate the
shared CRD schema, must match the controller mode. It does not override the
mode or select a second dispatcher. `runtime.type: opencode` is never protocol
evidence.

### 3.4 No migration or shared continuation

The following operations are unsupported:

- changing an installation from `harness-v1` to `harness-v2` in place, or the
  reverse;
- reusing a controller database, PVC, runtime store, or wrapper ledger under
  the other mode;
- changing a Task, Agent, or AgentRuntime protocol to make existing work run
  through the other installation;
- continuing a v1 Session in v2 or a v2 Session in v1;
- importing a transcript as continuation of the original Session lineage;
- allowing one controller to cancel, finalize, publish, or clean up work owned
  by the other.

An operator may create a new object in the other installation, but it is new
work with a new namespace, UID, Session lineage, attempt identity, and external
effect history. Copying prompts or non-secret configuration does not preserve
execution identity.

The static `harness-v2` release also does not adopt a pre-static, implicit-v2
controller installation in place. Those installations can have accepted ACP
attempts without the immutable execution authority required by this design.
Settle or retire that installation, preserve its state under its existing
owner, and install static `harness-v2` as a new release and namespace.

### 3.5 Harness v1 recovery boundaries

The v1 compatibility installation keeps request execution authority separate
from terminal-settlement authority:

- submitting, replaying, or recovering terminal output requires the frozen
  provider `CredentialRefs` used by the original request;
- acknowledging an already durable settlement reconstructs only the frozen
  wrapper endpoint/authentication client and the stored turn, request-digest,
  and terminal-receipt fences; it does not reread provider credentials;
- wrapper authentication rotation still fails closed against the frozen
  Secret UID, resourceVersion, and key.

Frame polling is bounded by the immutable request deadline. Persisted or
wrapper-ledger terminal evidence always wins first. Once the deadline passes,
the controller durably enters `CancelRequested`, retries `CancelTurn`, and
drains brokered tool-call reservations without starting new effects. If no
authoritative terminal evidence appears within the bounded cancellation
settlement window, the attempt becomes `OutcomeUnknown`; it is never replayed.

Deterministic frame identity, sequence, approval, continuation, frozen-tool,
and input-authority violations become `ProtocolViolation`/`OutcomeUnknown`.
Transport, event-journal, Kubernetes-read, and external-effect-store errors
remain retryable because they do not prove a permanent protocol violation.

The built-in wrapper advertises and enforces `MaxConcurrentTurns=1`. Controller
startup and Helm validation therefore require exactly one harness v1 dispatcher
worker; parallel dispatch is rejected as an unsupported configuration.

## 4. Shared API and CRD contract

CRDs are cluster-scoped even when their custom resources are namespaced. One
platform owner therefore installs and upgrades a schema bundle that can store
both the supported v1 and v2 object shapes without pruning either.

Requirements:

- keep one Kubernetes API version unless an independently justified API
  migration introduces conversion;
- preserve all v1 and v2 fields needed during the compatibility window;
- use structural schema and contract-specific validation where the two shapes
  differ;
- do not default an omitted protocol based on runtime type or observed state;
- verify stored v1 and v2 fixtures round-trip through spec and status updates;
- apply CRD upgrades once, before either release that requires them;
- install every additional Orka release with `--skip-crds` or the equivalent
  GitOps ownership rule.

The shared schema is not permission for mixed execution. Controller mode,
namespace claim, and RBAC determine which installation may act on an object.

The three `AgentExecution*` CRDs from the superseded design are not part of
this architecture. Test clusters that already installed unreleased versions of
those CRDs must remove their instances before deleting the definitions; CRD
deletion is cluster-wide and destructive to instances of that kind.

## 5. Deployment topology

### 5.1 Platform-owned wave

Before installing either controller:

1. back up existing CRDs and custom resources;
2. apply the reviewed v1/v2-compatible CRD bundle through the designated
   platform owner;
3. wait for every CRD to become `Established`;
4. verify maximum-shape v1 and v2 fixtures and status updates on a real
   supported Kubernetes version;
5. install shared admission resources once, after their serving endpoints are
   ready.

Helm does not update files from `crds/` during `helm upgrade`. Every upgrade
that changes schemas requires the explicit CRD-first wave.

### 5.2 Harness v1 release

The v1 release is an isolated compatibility installation. It requires:

- `controller.mode: harness-v1` (rendering
  `--controller-mode=harness-v1`);
- a dedicated, labeled, non-empty watch namespace;
- the reviewed digest-pinned wrapper image and its private Service;
- separate wrapper bearer-auth and rotatable TLS Secrets, plus a dedicated
  durable ledger;
- its own controller store, backups, ServiceAccount, and API endpoint;
- no ACP RuntimePool data plane.

Wrapper upgrades still require a successful wrapper drain before changing its
Pod template. That drain protects v1 turn state; it is not a third controller
mode and does not open migration to v2.

The bearer Secret is immutable while v1 work exists because bindings freeze
its UID and resourceVersion. Certificate renewal changes only the separate TLS
Secret and follows the same drained Pod-template rollover; it must never mutate
the bearer authority as a side effect.

### 5.3 Harness v2 release

The v2 release is a fresh installation. It requires:

- `controller.mode: harness-v2` (rendering
  `--controller-mode=harness-v2`);
- a dedicated, labeled, non-empty watch namespace;
- its own controller and runtime namespaces;
- digest-pinned ACP runtime images;
- the authenticated provider proxy, SCM proxy, and clean-room Publisher where
  the selected workflow requires them;
- its own controller store, backups, ServiceAccount, and API endpoint;
- no harness v1 wrapper data plane.

New producers select the v2 endpoint and namespace explicitly. Existing v1
objects are not copied or adopted.

An older controller that implicitly enabled ACP but did not declare
`--controller-mode=harness-v2` is not an upgrade source. Helm and the canonical
direct-Kustomize deployment preflight reject that in-place transition before
mutating workloads. Once a release already declares the exact static mode and
watch namespace, ordinary same-mode upgrades remain supported.

### 5.4 Cross-plane references

User-authored Task, Agent, AgentRuntime, Provider, Session, Tool, Skill, and
credential references stay within the installation's owned namespace unless a
separate API contract explicitly defines a platform-owned reference. A
reference into the other execution plane is rejected, not proxied.

V2 controller-owned RuntimePool resources may live in its configured runtime
namespace. That is an internal v2 relationship and does not give v1 any access
to the runtime namespace.

## 6. Rollout

1. Inventory the current v1 installation, including active wrapper turns,
   Tasks, Sessions, producers, stores, Secrets, and backups.
2. Atomically establish a dedicated v1 watch namespace with the `harness-v1`
   claim; reject any preexisting namespace without that exact identity.
3. Upgrade the v1 installation to the static-mode compatibility release
   without changing its protocol or object identities. Follow the v1 wrapper
   drain procedure for any wrapper Pod-template change.
4. Apply the shared CRD bundle through the single platform owner.
5. Atomically create a distinct v2 watch namespace with the `harness-v2` claim
   and create a distinct v2 runtime namespace; never adopt or relabel an
   existing unlabeled or opposite-mode watch namespace.
6. Install the v2 release with a unique name, endpoint, RBAC, storage, and
   `harness-v2` mode.
7. Prove with RBAC and runtime tests that neither controller can observe or
   mutate the other's namespace.
8. Run v2 canaries as newly created v2 Agents, Tasks, and Sessions.
9. Route new producers to the v2 endpoint. Leave existing v1 work on v1.

At no point does rollout patch v1 objects into v2 objects or run both modes in
one controller.

## 7. V1 drain and retirement

There is no dynamic drain mode. Draining is an operational procedure:

1. stop v1 API ingress and every internal and external v1 Task producer;
2. revoke or suspend permissions that can create new v1 agent Tasks;
3. record a cutoff time and inventory all active, queued, finalizing, and
   cleanup-relevant v1 work;
4. allow proven v1 work to finish on the v1 installation;
5. cancel work only through v1 and preserve `OutcomeUnknown` where acceptance
   cannot be disproved;
6. repeat uncached inventory until no active turn, Task, Session settlement,
   finalizer, or wrapper-ledger cleanup remains;
7. back up retained v1 history and stores;
8. uninstall v1 workloads and revoke v1 credentials;
9. remove v1 PVCs or historical fields only under a separate reviewed
   retention and data-destruction decision.

The v2 release continues independently throughout v1 retirement.

## 8. Rollback and recovery

Rollback changes where future work is submitted; it does not move existing
work.

If a v2 rollout must stop:

- stop new submissions to the v2 endpoint;
- let accepted v2 work settle, cancel it through v2, or retain its existing
  unknown-outcome classification;
- submit any replacement work as new v1 work only if the v1 installation is
  still intentionally open;
- retain the shared superset CRDs while either object shape exists.

Each installation has its own coordinated recovery point. A recovery must pair
that installation's Kubernetes identities with its controller database, PVCs,
ledgers, artifacts, and Secrets. Never restore a v1 data set into v2 or a v2
data set into v1. Plain YAML export does not preserve UIDs and is not sufficient
to resume UID-bound execution.

## 9. Verification and release gates

### 9.1 Configuration

- missing, empty, `dual`, `auto`, `harness-v1-drain`, and unknown modes fail;
- an empty watch namespace fails;
- a missing or mismatched namespace mode label fails;
- fresh bootstrap creates the namespace and mode claim in one write before any
  Secret or workload write;
- exact same-mode namespace reuse is idempotent and does not rewrite the mode
  claim;
- unlabeled and opposite-mode namespaces are rejected without mutation;
- a namespace create race succeeds only after rereading an exact same-mode
  identity;
- leader election is required and its Lease is in the watched namespace;
- mode-incompatible wrapper or ACP configuration fails rendering or startup;
- implicit or legacy v2 controllers are rejected as in-place static-v2 upgrade
  sources;
- changing the namespace claim does not cause a running installation to adopt
  opposite-mode work.

### 9.2 Isolation

- v1 and v2 releases use different namespaces, SAs, Leases, PVCs, Services,
  Secrets, and endpoints;
- each controller receives `Forbidden` when attempting reads or writes in the
  other watch namespace;
- v1 does not start ACP or cluster-scoped gateway/workspace reconcilers;
- v2 does not start the v1 dispatcher;
- each controller restart, upgrade, and uninstall leaves the other healthy;
- no cluster-scoped admission resource is multiply owned.

### 9.3 API compatibility

- stored v1 and v2 Agent, AgentRuntime, Task, and Session fixtures survive the
  shared CRD apply and status update;
- contract-specific invalid combinations are rejected;
- a contract that conflicts with controller mode fails closed;
- no `AgentExecutionControl`, `AgentExecutionPolicy`, or
  `AgentExecutionAdjudication` CRD is installed;
- deleting one Helm release does not delete shared CRDs or the other release's
  objects.

### 9.4 Runtime behavior

- a v1 Task executes only through the wrapper;
- a v2 Task executes only through ACP RuntimePools;
- unavailable v2 capacity never falls back to v1;
- the shipped v1 controller and wrapper both enforce a single concurrent turn;
- a same-name object in the other namespace is unrelated and cannot continue
  the original Task or Session;
- cross-plane cancellation, cleanup, publication, and Session continuation are
  rejected;
- v1 wrapper and v2 controller restart tests preserve their own protocol's
  duplicate and unknown-outcome invariants.
- v1 settlement acknowledgement survives provider-credential removal while
  wrapper-auth rotation remains fail-closed;
- v1 request deadlines durably request cancellation, stop new brokered effects,
  and reach `OutcomeUnknown` only after the bounded settlement window;
- deterministic v1 frame-authority violations terminalize as protocol
  violations while transport and durable-store failures remain retryable.

### 9.5 Retirement

- v1 producers and create permissions are closed before the drain inventory;
- repeated inventory proves zero active and cleanup-relevant v1 state;
- removing v1 workloads and credentials does not change v2 readiness or work;
- retained historical v1 objects remain readable under the shared schema.

## 10. Definition of done

This replacement plan is complete when:

1. the controller accepts exactly the two static modes, requires a matching
   non-empty namespace claim, and deployment paths establish that claim
   atomically without adopting or relabeling an existing namespace;
2. v1 and v2 run in disjoint namespaces with enforced RBAC, storage, Lease,
   Service, Secret, and network boundaries;
3. mode-specific controller registration makes cross-dispatch impossible;
4. the shared CRD bundle round-trips supported v1 and v2 shapes without the
   three `AgentExecution*` CRDs;
5. Helm and Kustomize deployment paths document one CRD owner and distinct
   release/watch/runtime namespaces;
6. no supported workflow migrates or continues a Task, AgentRuntime, or
   Session across protocols;
7. the two-release, rollback, and v1-retirement verification gates pass on a
   real cluster;
8. ADR 0018 and operator documentation describe the shipped behavior.
