# Execution Workspace Provider, Pool, and Hosted Tool Implementation Plan

**Date:** July 16, 2026  
**Target repository:** `github.com/orka-agents/orka`

## Summary

Replace Orka’s direct Agent Sandbox and Substrate integration with a provider-neutral `workspace.orka.ai` control plane and a versioned `orka-workspace-agent` data plane.

The implementation covers:

- Interactive Task workspaces.
- Persistent service workspaces for MCP-backed Tools.
- Generic warm/capacity pools.
- Provider/class version pinning and draining.
- Attachment fencing and credential rotation.
- Separation of Task execution outcome from workspace cleanup.
- Additive migration of existing Task, Tool, Helm, and pool configurations.
- Independent Substrate and Agent Sandbox adapter repositories.
- Eventual removal of provider SDKs, protos, flags, and RBAC from Orka core.
- OpenSandbox is explicitly excluded; the contract must remain extensible enough to support it later.

## Locked Architectural Decisions

- New API group: `workspace.orka.ai/v1alpha1`.
- Provider adapter control plane: Kubernetes reconcilers watching generic workspace CRDs.
- Data plane: versioned HTTP protocol implemented by `orka-workspace-agent`.
- Provider identity and provider-specific profile references are immutable.
- Functional `ExecutionWorkspaceClass` specs are immutable; changes use a new class.
- Provider adapters never write `Task` or `Tool` status directly.
- Orka core owns `Task` and `Tool` status; one provider adapter owns each `ExecutionWorkspace` status.
- Exclusive attachment uses a Lease plus an enforced attachment epoch and rotated bearer token.
- Task completion waits only for attachment revocation, not full workspace disposal.
- Cleanup failure never replays a successful Task; affected workspaces become quarantined.
- Task users select classes, never provider names, templates, pools, endpoints, or provider versions.
- Classes are namespaced; providers are cluster-scoped.
- Class use requires an explicit `use` authorization check.
- Initial capacity enforcement uses object-count `ResourceQuota` and provider queues; Kueue integration is deferred.
- Remote `AgentRuntime` workspace use remains unsupported initially; brokered workspace operations are the first future remote mode.
- Adapter repositories:
  - `orka-agents/orka-workspace-provider-substrate`
  - `orka-agents/orka-workspace-provider-agent-sandbox`
- Substrate extraction includes interactive workspaces, service/MCP actors, and actor pools from the start.

---

## Public API and Interface Changes

### New `workspace.orka.ai/v1alpha1` resources

#### `ExecutionWorkspaceProvider` — cluster-scoped

Represents one configured provider installation.

Key fields:

```yaml
spec:
  controllerName: substrate.workspace.orka.ai/v1
  parametersRef:
    group: substrate.workspace.orka.ai
    kind: SubstrateProviderConfig
    name: production
  lifecycleState: Active # Active, Draining, Disabled
  requiredContracts:
    - workspace.orka.ai/v1
  usagePolicy:
    allowedNamespaceSelector: {}
status:
  observedGeneration: 1
  adapter:
    version: 1.0.0
    digest: sha256:...
  backend:
    version: ...
    apiVersions: [...]
  supportedFeatures: [...]
  lastHeartbeat: ...
  conditions: [...]
```

Immutable:

- `controllerName`
- `parametersRef`
- `requiredContracts`

Mutable:

- `lifecycleState`
- `usagePolicy`

#### `ExecutionWorkspacePool` — namespaced

Represents provider-managed warm or reusable capacity.

```yaml
spec:
  providerRef:
    name: substrate-prod
  parametersRef:
    group: substrate.workspace.orka.ai
    kind: SubstratePoolParameters
    name: coding-pool
  capacity:
    minReady: 4
    maxSize: 100
status:
  available: 3
  allocated: 8
  suspended: 20
  total: 31
  conditions: [...]
```

Immutable:

- `providerRef`
- `parametersRef`

Mutable:

- `capacity`

#### `ExecutionWorkspaceClass` — namespaced

Represents a user-selectable environment and policy.

Exactly one provisioning source:

```yaml
spec:
  providerRef: ...
  parametersRef: ...
```

or:

```yaml
spec:
  poolRef:
    name: coding-pool
```

Additional fields:

```yaml
spec:
  mode: Interactive # Interactive, Service
  requiredFeatures: [...]
  allowedReuseScopes: [None, Session]
  lifecycle:
    defaultOnDetach: Suspend
    allowedOnDetach: [Suspend, Delete]
    detachTimeout: 2m
    idleTimeout: 30m
    maxLifetime: 24h
    deletionPolicy:
      providerResources: Delete
      persistentVolumes: Retain
      checkpoints: Delete
```

The complete functional spec is immutable.

#### `ExecutionWorkspace` — namespaced, controller-created

Represents one concrete provider-bound environment.

```yaml
spec:
  mode: Interactive
  classBinding:
    name: secure-coding-v1
    uid: ...
    generation: 1
    profileHash: sha256:...
  providerBinding:
    name: substrate-prod
    uid: ...
    generation: 2
  sessionRef:
    name: feature-123
    uid: ...
  slot: default
  desiredState: Ready
  lifecycle: ...
  attachment:
    taskRef:
      name: implement-api
      uid: ...
    epoch: 42
    tokenSHA256: ...
    tokenSecretRef:
      name: ws-attachment-42
    expiresAt: ...
  service:
    ports: []
status:
  observedGeneration: 3
  state: Attached
  externalID: ...
  attachedEpoch: 42
  providerBinding:
    contractVersion: workspace.orka.ai/v1
    adapterVersion: 1.0.0
    adapterDigest: sha256:...
    backendAPIVersion: ...
  endpoints: []
  disposition:
    compute: Pending
    accessCredentials: Active
    ephemeralSecrets: Pending
    workspaceData: Retained
    persistentVolumes: Retained
    checkpoints: Pending
    providerResources: Pending
  conditions: [...]
```

Immutable after provisioning starts:

- Mode
- Class/provider binding
- Session reference and slot
- Resolved lifecycle/deletion policy

Mutable:

- Desired state
- Attachment
- Requested service endpoints

### Existing core API additions

#### Task

Additive `v1alpha1` shape:

```yaml
spec:
  execution:
    workspace:
      classRef:
        name: secure-coding-v1
      reusePolicy: session
      workspaceSlot: default
      onDetach: Suspend
```

Add:

- `TaskPhaseFinalizing`
- Immutable `status.executionOutcome`
- Generic workspace reference/status conditions
- Retry guard: a successful recorded execution outcome is never retried

Legacy provider/template/pool fields remain served during migration.

#### Tool

Additive MCP hosting shape:

```yaml
spec:
  mcp:
    path: /mcp
    workspace:
      classRef:
        name: mcp-service-v1
      port: 8080
```

Legacy `mcp.substrateActor` remains supported during migration.

### Provider-specific APIs

Substrate adapter repository:

- `SubstrateProviderConfig`
- `SubstrateWorkspaceProfile`
- `SubstratePoolParameters`

Agent Sandbox adapter repository:

- `AgentSandboxProviderConfig`
- `AgentSandboxWorkspaceProfile`
- `AgentSandboxPoolParameters`

Orka core treats all provider-specific parameter references as typed object references and does not import these Go types after extraction.

### Shared public packages retained in Orka

- `api/workspace/v1alpha1`
- `pkg/workspaceprovider`
- `pkg/workspaceagent`

External adapter repositories pin a tagged Orka module version and may import only these public packages, never `internal/` or core controller packages.

---

# Implementation Phases

## Phase 0 — Documentation and ADR Lock

### Work

Create the master implementation document and five ADRs:

1. Execution workspace domain model and resource ownership.
2. CRD control plane plus workspace-agent data plane.
3. Attachment fencing, bearer-token rotation, and transport security.
4. Task execution outcome versus workspace finalization.
5. Provider/class versioning, draining, pools, and service workspaces.

The lifecycle ADR explicitly supersedes ADR 0006 as the target architecture while retaining wrapper-first as historical/transitional context.

Document:

- Conversation Session versus RuntimeSession versus ExecutionWorkspace.
- Status-writer ownership.
- Provider/class/workspace scopes.
- Interactive versus Service modes.
- Pool relationship.
- Exact state transitions and condition meanings.
- Deletion disposition guarantees.
- Remote AgentRuntime exclusion.
- Legacy migration and removal policy.

### Exit criteria

- All five ADRs have `Accepted` status.
- Every CRD field described below is assigned an owner and mutability rule.
- Every state transition has an identified reconciler.
- No unresolved “TBD” remains for v1 behavior.
- Existing runtime behavior is unchanged.
- Documentation review includes controller, worker/harness, security, Tool/MCP, and provider maintainers.

---

## Phase 1 — Workspace API Group and Contract Packages

### Work

Convert the Kubebuilder project to a multi-group layout using supported Kubebuilder commands; do not hand-edit `PROJECT`.

Add the four `workspace.orka.ai/v1alpha1` CRDs and register them with the manager scheme.

Add schema validation:

- Immutable class spec.
- Immutable provider identity.
- Immutable pool provider/parameters.
- Immutable concrete workspace binding.
- Exactly one of class direct-provider configuration or pool reference.
- Service ports only for Service mode.
- Attachments only for Interactive mode.
- Valid lifecycle/deletion policy combinations.
- `onDetach` override must be allowed by the class.
- Session reuse requires a Task session.
- Provider and class references cannot cross their allowed scope.

Add public contract packages:

- Feature/profile constants.
- Condition and state constants.
- Typed object and immutable binding references.
- Status patch helpers.
- Provider controller predicate keyed by `controllerName`.
- Conformance interfaces without provider implementation.

Add controller RBAC only for the new generic resources.

### Exit criteria

- `make manifests generate` produces valid CRDs and deepcopy code.
- `make lint-fix && make test` passes.
- Envtest proves all immutability and one-of validations.
- Round-trip serialization tests cover every CRD.
- A provider/class/pool/workspace can be manually created and read.
- No provider resource is created and no existing Task behavior changes.
- Orka core still builds without importing new adapter repositories.
- Public packages have no imports from `internal/`.

---

## Phase 2 — `orka-workspace-agent` Protocol v1

### Work

Replace the private daemon protocol with `pkg/workspaceagent`.

Add:

```text
GET    /v1/health
GET    /v1/capabilities
PUT    /v1/control/attachment
DELETE /v1/control/attachment/{epoch}
POST   /v1/exec
GET    /v1/exec/{operationID}
POST   /v1/exec/{operationID}/cancel
PUT    /v1/files
POST   /v1/files/download
POST   /v1/scrub
POST   /v1/reset
```

Every DTO carries an explicit protocol version.

Attachment security:

- Core generates a random 32-byte token.
- Raw token exists only in a Kubernetes Secret mounted into the worker.
- SHA-256 hash, epoch, expiry, Task UID, and workspace UID are sent through the privileged attachment-control endpoint.
- Workspace agent stores current attachment state only in memory or tmpfs.
- Data requests must carry workspace UID, epoch, and bearer token.
- Stale epoch or token is rejected.
- Detach clears the active epoch before scrub/reset.

Execution safety:

- Caller supplies `operationID`.
- Duplicate operation IDs return the original state/result.
- Timed-out commands are queried before retry.
- Explicit cancellation supported.
- Bounded operation-result retention.
- Upload uses path and digest idempotency.
- Scrub/reset are idempotent.

Transport:

- TLS is supported with endpoint and CA data supplied through a connection Secret.
- Plain HTTP is rejected unless `--allow-insecure-workspace-transport` is set.
- No bearer token is logged or returned in errors.

### Exit criteria

- Unit tests cover successful activation, revocation, expiry, and epoch rollover.
- A stale worker token cannot exec, upload, download, scrub, reset, or cancel.
- Duplicate operation IDs never execute a command twice.
- Timeout followed by status lookup produces a deterministic outcome.
- Cancelled operations reach a terminal cancelled state.
- TLS integration test validates a generated CA/certificate chain.
- Fuzz tests cover request decoding, path validation, and bounded payloads.
- Secret-looking values do not appear in logs or protocol errors.
- Existing workspace-agent tests migrate to the public protocol package.
- `make lint-fix && make test` passes.

---

## Phase 3 — Core Workspace Coordinator and Fake Provider

### Work

Add core reconcilers for:

- Provider heartbeat freshness and usability evaluation.
- Class resolution and explicit `use` authorization.
- Workspace identity and immutable binding.
- Attachment epoch and Secret lifecycle.
- Workspace finalization and quarantine.
- Pool reference validation.
- Generic Tool and Task workspace projection.

Use a direct `SubjectAccessReview` for the `use` verb on the referenced class.

Add a fake/reference provider controller in Orka:

- Controller name `fake.workspace.orka.ai/v1`.
- Supports Interactive and Service modes.
- Supports generic pools.
- Runs the real workspace-agent image.
- Exercises all provider SDK and conformance helpers.
- Is available only in tests/dev manifests.

Workspace identity:

- Session reuse: namespace + Session UID + workspace slot + class UID.
- No session: Task UID.
- Service workspace: owning Tool UID + class UID.

Add provider conformance tests for:

- Provider registration and heartbeat.
- Provision and observe.
- Attach/detach.
- Suspend/resume.
- Delete and disposition.
- Service endpoints.
- Pool allocation.
- Adapter restart and reconciliation.
- Idempotent create after ambiguous response.

### Exit criteria

- Manually creating a fake provider, class, pool, and workspace reaches Ready.
- Interactive attachment reaches `Attached=True` and executes through workspace-agent.
- Service workspace publishes a usable test endpoint.
- Pool counts and allocation status are accurate.
- Provider heartbeat expiry blocks new workspaces.
- Active → Draining rejects new workspaces but permits existing reattachment and cleanup.
- Disabled permits cleanup/delete only.
- Deleting a referenced provider is blocked by finalizer.
- Orphan and ambiguous-create tests do not create duplicate external resources.
- No Task or Tool integration is enabled yet.
- Envtest, unit tests, and fake-provider E2E pass.

---

## Phase 4 — Task and Harness Integration

### Work

Add class-based Task resolution and controller-first provisioning.

Flow:

1. Resolve and authorize class.
2. Find/create concrete ExecutionWorkspace.
3. Wait for workspace `DataPlaneReady=True`.
4. Increment attachment epoch and create token Secret.
5. Wait for provider `Attached=True` for that epoch.
6. Create harness worker Job with only workspace connection and attachment Secret.
7. Run the agent through workspace-agent.
8. Store execution outcome/result.
9. Transition Task to Finalizing.
10. Revoke attachment and delete worker Job/Secret.
11. Mark Task terminal.
12. Let workspace finalization continue asynchronously.

Task completion:

- Add `Finalizing`.
- Detach timeout defaults to 2 minutes and is class-controlled.
- Successful outcome disables retry permanently.
- Detach timeout deletes Job and token Secret, quarantines workspace, and terminates the Task with degraded conditions.
- Full scrub/suspend/delete never changes a successful execution outcome.
- Failed execution retries only after old attachment is revoked; uncertain workspaces are quarantined or replaced.

Update `wait_for_tasks`, task APIs, webhook delivery, events, and status rendering for Finalizing and degraded completion.

Remove provider-native claim/release/delete actions from the new worker path.

### Exit criteria

- Fake-provider Task E2E reaches Succeeded through the new path.
- Session-scoped second Task reuses the same workspace with a higher epoch.
- A successful result followed by scrub failure is not rerun.
- A stale first Task cannot mutate a workspace after the second Task attaches.
- Detach timeout completes the Task with degraded status and quarantines the workspace.
- A quarantined workspace is never selected for reuse.
- Failed execution respects retry policy without retaining stale attachment authority.
- Cancellation revokes attachment before Task cancellation becomes terminal.
- Parent `wait_for_tasks` returns correct result and degraded/finalization metadata.
- Existing Tasks without classRef still use the legacy path unchanged.
- Harness runtime no longer rejects class-based workspace requests.
- Focused controller, worker, harness, and E2E tests pass.

---

## Phase 5 — Generic Pools and MCP Service Workspaces

### Work

Add generic pool behavior:

- Class may directly reference provider parameters or a pool.
- Pool adapter owns capacity reconciliation.
- Core reports provider-independent available/allocated/suspended/total counts.
- Capacity exhaustion produces `Admitted=False/CapacityUnavailable`, not provider failure.
- ResourceQuota object counts limit total namespaced workspaces.

Add generic service workspaces:

- Tool with `mcp.workspace.classRef` creates a Service-mode ExecutionWorkspace.
- Tool specifies port and MCP path.
- Provider publishes sanitized endpoint information.
- Tool controller performs existing SSRF-safe health checks.
- Tool status reports class/workspace reference and endpoint, not actor/provider-native IDs.
- Tool deletion relies on owned workspace deletion/finalization.

Service workspaces do not use Task attachment credentials. Provider lifecycle control remains authenticated separately.

### Exit criteria

- Fake provider prewarms capacity and allocates from a pool.
- Pool resize up/down preserves leased workspaces.
- Pool deletion waits for or safely drains allocations.
- Namespace ResourceQuota rejects workspace creation above the hard count.
- MCP test Tool becomes Available through a Service workspace.
- Tool deletion deletes or disposes its workspace according to policy.
- Tool endpoint status contains no provider-native actor identifiers or credentials.
- Task and Tool can use the same pool without allocation collision.
- Legacy `SubstrateActorPool` and `mcp.substrateActor` behavior remains unchanged.
- Generic pool/service controller tests and E2E pass.

---

## Phase 6 — External Substrate Provider Repository

### Work

Create `orka-agents/orka-workspace-provider-substrate`.

Implement provider-specific CRDs and controllers:

- `SubstrateProviderConfig`
- `SubstrateWorkspaceProfile`
- `SubstratePoolParameters`
- Provider/Class/Pool/Workspace reconciliation for `substrate.workspace.orka.ai/v1`

Move or reimplement behind the generic contract:

- Substrate API trust and CA validation.
- Session identity configuration.
- Actor creation/reattachment.
- Workspace-agent bootstrap and routing.
- Running/suspend/delete lifecycle.
- Actor-template compatibility checks.
- Worker/actor density and pool reconciliation.
- MCP service endpoint publication.
- Actor lease and cleanup behavior.
- Provider/backend version observation.
- Breaking-version compatibility checks.

Support all generic modes from the first release:

- Interactive workspace.
- Service/MCP workspace.
- Pool-backed workspace/service.
- Active/Draining/Disabled.
- Attachment fencing.
- Quarantine/delete.
- Suspend/resume.

Ship independent:

- Go module.
- Image.
- Helm chart/Kustomize bundle.
- Release workflow.
- SBOM/signature.
- Compatibility matrix.
- Conformance and live E2E workflows.

### Exit criteria

- Adapter passes the shared provider and workspace-agent conformance suites.
- Live Substrate Task E2E covers cold run, session reuse, suspend/resume, cancellation, timeout, stale-token rejection, and cleanup.
- Live MCP E2E hosts a real MCP server through Service mode.
- Generic pool E2E covers precreate, allocation, capacity pressure, resize, and deletion.
- Two adapter/backend versions can run side-by-side under distinct provider resources.
- Draining migration routes new workspaces to v2 while existing v1 workspaces remain usable.
- Provider credentials never appear in Orka worker Jobs or generic resource status.
- Orka’s new path contains no Substrate-specific branches.
- Existing direct Substrate path remains available only for compatibility.

---

## Phase 7 — External Agent Sandbox Provider Repository

### Work

Create `orka-agents/orka-workspace-provider-agent-sandbox`.

Implement:

- `AgentSandboxProviderConfig`
- `AgentSandboxWorkspaceProfile`
- `AgentSandboxPoolParameters`
- Provider/Class/Pool/Workspace reconciliation for `agent-sandbox.workspace.orka.ai/v1`

Interactive profile requirements:

- Primary container runs `orka-workspace-agent`.
- Harness wrapper and required CLI runtimes are installed.
- Writable workspace/home/tmp/app paths exist.
- Per-Task tokens/config are delivered after claim, never through `SandboxClaim.env`, preserving warm-pool allocation.
- Template supports TLS endpoint publication.
- Running/Suspended operating modes map to generic lifecycle.

Service profile requirements:

- Provider can expose declared service ports.
- MCP server template reaches Ready and publishes endpoint.
- Workspace-agent may be a control sidecar but is not assumed to share another container’s root filesystem.

Pool behavior maps generic capacity onto SandboxWarmPool without exposing warm-pool names to Tasks/Tools.

### Exit criteria

- Adapter passes the same shared conformance suite as Substrate.
- Live Task E2E covers cold claim, warm claim, session reuse, suspend/resume, cancellation, stale-token rejection, and cleanup.
- Task-specific attachment setup does not force a warm claim into the cold path.
- Live Service/MCP E2E publishes and calls a real MCP endpoint.
- Generic pool metrics match upstream warm-pool state.
- Agent Sandbox upstream API migration/version mismatch marks the provider incompatible before new allocation.
- Existing direct Agent Sandbox path remains available only for compatibility.
- No provider credentials appear in worker Jobs or generic status.

---

## Phase 8 — Additive Legacy Translation

### Work

Keep current v1alpha1 Task, Agent, Tool, Helm, and `SubstrateActorPool` fields working.

Add a compatibility materializer:

- Normalize effective legacy settings using current defaulting rules.
- Create deterministic managed provider-profile objects, classes, and pools using unstructured provider-specific CRs.
- Name resources from a stable hash of normalized legacy settings.
- Label generated resources `workspace.orka.ai/legacy-generated=true`.
- Patch resolved `classRef` onto legacy Task/Agent/Tool specs while preserving legacy fields.
- Record translation status and deprecation warning.
- Garbage-collect unreferenced generated classes/profiles after a 24-hour grace period.
- Mirror legacy `SubstrateActorPool` into `ExecutionWorkspacePool` and provider parameters.
- Legacy Tool actor settings materialize a Service class/workspace.
- Existing Orka Helm provider settings bootstrap matching provider/config resources during the compatibility window.

Validation permits classRef plus legacy fields only when the migration controller has marked the object translated.

### Exit criteria

- Existing Agent Sandbox Task manifests run unchanged through the new provider contract.
- Existing Substrate Task manifests run unchanged through the new provider contract.
- Existing `mcp.substrateActor` Tools run through generic Service workspaces.
- Existing `SubstrateActorPool` manifests produce equivalent generic pool behavior.
- Session reuse produces the same logical workspace identity before and after translation.
- Translation is idempotent across controller restarts.
- Generated resources are not duplicated.
- Deprecation warnings are visible through status/events/CLI.
- Legacy and new manifests are covered in both provider E2E workflows.
- Existing direct execution can be disabled while translated legacy manifests still pass.

---

## Phase 9 — API, CLI, UI, Documentation, and Operations

### Work

REST/API surfaces:

- List/get providers, classes, pools, and workspaces.
- Admin actions: drain/enable/disable provider, delete/quarantine workspace, inspect disposition.
- Capability endpoint indicates workspace-provider API availability.

CLI:

```text
orka workspace provider list|get|drain|enable|disable
orka workspace class list|get
orka workspace pool list|get
orka workspace list|get|delete|quarantine
```

Task/Tool views display:

- Class and workspace reference.
- Generic state and conditions.
- Attachment/finalization state.
- Pool/capacity.
- Provider adapter/backend version for operators.
- No credentials or raw provider endpoints.

UI:

- Generic workspace state in Task and Tool details.
- Provider/class/pool/workspace operator pages.
- Quarantine and cleanup warnings.
- Legacy deprecation warnings.
- Feature-gated rendering when API unavailable.

Metrics:

- Workspace operation count/duration by operation/result.
- State transitions.
- Provision, resume, attach, detach, and cleanup latency.
- Quarantine/orphan counts.
- Attachment auth failures by bounded reason.
- Provider readiness and heartbeat freshness.
- Pool available/allocated/suspended capacity.
- Legacy translation count/failure.

No workspace UID, Task UID, external ID, endpoint, or token appears in metric labels.

Documentation:

- Concepts and domain glossary.
- Provider authoring guide.
- Provider conformance guide.
- Class/pool administration.
- Task and Tool migration.
- Security and credential boundaries.
- Upgrade/draining runbook.
- Quarantine/orphan runbook.

### Exit criteria

- CLI and REST tests cover all read/admin actions and authorization.
- UI lint/tests pass and no provider-specific field is required for rendering generic workspaces.
- Operators can diagnose pending, capacity-blocked, unhealthy, quarantined, draining, and cleanup-failed states without controller logs.
- All metrics pass low-cardinality review.
- Documentation contains runnable manifests for both adapters and both workspace modes.
- Website build passes.

---

## Phase 10 — Remove Direct Provider Coupling from Orka Core

### Work

After both external adapters and legacy translation are production-ready, remove from Orka core:

- Agent Sandbox SDK dependency and scheme registration.
- Substrate proto/client packages.
- Direct provider factories.
- Agent Sandbox/Substrate controller flags and worker environment variables.
- Provider-specific Task and Tool controller branches.
- Direct `SubstrateActorPoolReconciler`.
- Provider-specific lease labels/annotations and cleanup paths.
- Provider-specific RBAC.
- Provider-specific Helm deployment configuration.
- Direct workspace status switching/defaulting.

Retain:

- `workspace.orka.ai` APIs.
- Provider SDK/conformance packages.
- Workspace-agent protocol/client/server.
- Fake provider.
- Temporary legacy materializers.
- Generic Task/Tool/Pool/Workspace controllers.

Orka CI pins released adapter image/chart versions and installs them as external dependencies for live integration workflows.

### Exit criteria

- `go.mod` has no Agent Sandbox or Substrate provider dependency.
- `cmd/main.go` registers no provider-native API schemes.
- Core RBAC has no provider-native resources.
- Controller and worker images contain no provider-native client code.
- Both live provider workflows install adapters from their independent repositories/releases.
- Task, Tool, pool, migration, version-skew, and cleanup E2E remain green.
- Uninstalling one adapter affects only its provider resources.
- Core build/test succeeds when neither provider is installed.

---

## Phase 11 — v1beta1 API Cutover and Legacy Removal

### Work

Introduce `core.orka.ai/v1beta1` Task, Agent, and Tool APIs:

- Workspace selection uses classRef only.
- MCP hosted Tool selection uses workspace class only.
- Direct provider/template/pool/boot/snapshot/hibernation fields are absent.
- Substrate-specific Tool fields are absent.
- Legacy `SubstrateActorPool` is deprecated and no longer created by new clients.

Before storage-version cutover:

- Migration controller materializes and patches classRef onto every legacy object.
- Migration report identifies zero legacy-only active objects.
- Completed historical Tasks are patched safely without triggering execution.
- Conversion round trips preserve classRef and terminal status.
- v1alpha1 remains served for one compatibility release.
- New v1alpha1 legacy writes receive warnings and are materialized immediately.
- v1beta1 becomes storage version only after migration preconditions pass.

After the compatibility window:

- Remove legacy materializers.
- Stop serving deprecated pool and direct-provider fields according to the project’s API deprecation policy.

### Exit criteria

- Conversion webhook round-trip tests pass for Task, Agent, and Tool.
- Storage-version migration succeeds on fixture clusters containing legacy objects.
- No completed Task is rerun due to migration-induced generation changes.
- v1beta1 clients cannot author provider-specific workspace settings.
- v1alpha1 compatibility tests remain green during the supported window.
- Migration CLI/controller reports all legacy objects resolved before storage cutover.
- Removal release has an explicit upgrade and rollback runbook.

---

## Phase 12 — Default-On Rollout and Production Hardening

### Feature gates

Initial:

```text
--enable-workspace-provider-api=false
--enable-controller-first-workspaces=false
--enable-workspace-legacy-translation=true
--allow-insecure-workspace-transport=false
```

Progression:

1. API CRDs/controllers default off.
2. Fake-provider/internal CI enabled.
3. External adapter canaries enabled by namespace/class.
4. Controller-first path default on for classRef Tasks/Tools.
5. Legacy translation default on.
6. New API default on.
7. Direct provider path removed.
8. v1beta1 storage cutover.

### Production validation

Required scenarios:

- Cold and warm interactive execution.
- Session reuse with epoch rollover.
- Cross-runtime conversation with compatible shared workspace.
- Cancellation and timeout.
- Outcome-unknown command recovery.
- Successful result plus cleanup failure.
- Quarantine and administrator recovery.
- Provider crash/restart.
- Orka controller crash/restart.
- Data-plane restart.
- Provider draining and side-by-side upgrade.
- Backend API incompatibility.
- Pool pressure and resize.
- Service/MCP lifecycle.
- Namespace authorization denial.
- ResourceQuota rejection.
- Secret redaction and token rotation.
- Unsafe endpoint/TLS rejection.
- Legacy manifest translation.
- Adapter uninstall with active workspaces.
- Force-orphan audit path.

### Exit criteria

- All shared unit, envtest, integration, fake-provider, and live-provider suites pass.
- `make manifests generate`, `make lint-fix`, and `make test` pass.
- UI lint/tests and website build pass.
- Modified workflows pass actionlint.
- Provider adapters pass conformance against every supported backend version.
- Security review finds no provider credentials in Task/Tool specs, statuses, events, worker environments, logs, or metrics.
- Stale attachment tokens are mechanically rejected.
- A successful Task cannot be replayed because of workspace finalization failure.
- Cleanup/quarantine alerts and runbooks are exercised.
- Adapter upgrade and rollback are demonstrated without migrating active workspace bindings.
- Orka operates normally with zero, one, or both adapters installed.

---

## Cross-Phase Test Matrix

### API/schema

- Immutability and one-of validation.
- Defaulting and conversion.
- Reference scope and authorization.
- Feature requirement/subset validation.
- Provider/class/pool/workspace serialization.

### State machines

- Provider Active/Draining/Disabled.
- Workspace provision/attach/detach/suspend/delete/quarantine.
- Task Running/Finalizing/terminal.
- Pool scale/allocation/drain.
- Service workspace ready/unhealthy/deleted.

### Security

- Class `use` authorization.
- Namespace provider policy.
- TLS enforcement.
- Token secrecy.
- Epoch fencing.
- Secret revocation before scrub.
- Cross-namespace denial.
- Provider credential isolation.
- SSRF-safe service endpoint handling.

### Reliability

- Idempotent provisioning.
- Ambiguous external create.
- Duplicate operation ID.
- Controller/adapter/data-plane restart.
- Status conflict/reconciliation.
- Finalizer timeout and force-orphan.
- Provider version drift.
- Capacity queue behavior.

### Compatibility

- New class manifests.
- Legacy Task/Agent manifests.
- Legacy Tool actor manifests.
- Legacy SubstrateActorPool.
- v1alpha1/v1beta1 conversion.
- Side-by-side provider revisions.

---

## Assumptions and Explicit Non-Goals

- OpenSandbox is not implemented or documented as a supported provider in this plan.
- Cross-provider checkpoint restore is not supported.
- Shared concurrent workspace attachment is not supported; v1 is exclusive single-writer.
- Remote AgentRuntime direct workspace access is not supported.
- Provider installation remains operator-managed; Orka does not install providers in production.
- Kueue integration, SPIFFE/mTLS identity, secure erase, and cross-namespace grants are future capability profiles.
- Existing RuntimeSession persistence remains separate from ExecutionWorkspace persistence.
- Provider-specific parameter CRDs are owned by their adapter repositories.
- Provider adapter source moves directly into independent repositories after the common API/conformance foundation is stable; no new long-lived provider implementation is added to Orka core.
