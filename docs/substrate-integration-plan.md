# Agent Substrate Integration Plan

This plan describes how Orka should integrate with
[agent-substrate/substrate](https://github.com/agent-substrate/substrate) as an
experimental execution workspace backend.

The recommendation is to add Substrate as a second provider behind Orka's
existing workspace abstraction, not as a replacement for Orka's task
orchestration or for the current agent-sandbox integration.

## Executive Summary

Use a wrapper-first integration:

1. Orka creates the normal Kubernetes worker Job.
2. The outer worker reads the resolved execution workspace provider.
3. For `provider: substrate`, the worker creates or reattaches a Substrate Actor.
4. The worker resumes the Actor through Substrate's control API.
5. The worker talks to an Orka workspace daemon inside the Actor.
6. The worker stages the same Orka worker binary and short-lived token files.
7. The daemon executes the inner Orka worker inside the Substrate Actor.
8. The inner worker performs the normal agent lifecycle.
9. The outer worker scrubs token files, then suspends or deletes the Actor.

This keeps Orka's control plane, Task API, result submission, artifact handling,
and agent runtime behavior stable while using Substrate for warm, suspendable,
stateful execution.

## Research Baseline

This plan was checked against:

- Orka's current workspace implementation:
  - `Task.spec.execution.workspace` is agent-sandbox-specific today.
  - `internal/workspace.WorkspaceExecutor` already provides the right provider
    boundary: claim, wait-ready, exec, upload, download, release, delete,
    describe.
  - The controller validates and injects worker env; the worker owns claim,
    readiness, command execution, and cleanup.
  - Task status currently has no workspace lifecycle field.
- Agent Substrate `origin/main` at `b80031d`:
  - public control API is actor lifecycle and listing.
  - `CreateActor` derives an Actor from an `ActorTemplate`.
  - `ResumeActor`, `SuspendActor`, and `DeleteActor` manage state transitions.
  - `DeleteActor` is only valid for suspended actors.
  - `ActorTemplate` requires snapshots config, a `WorkerPoolRef`, and runsc
    download configuration.
  - kind setup is provided by `hack/create-kind-cluster.sh` and
    `hack/install-ate-kind.sh`.
- Kubernetes SIG agent-sandbox `origin/main` at `35b9757`:
  - public Kubernetes objects include `Sandbox`, `SandboxClaim`,
    `SandboxTemplate`, and `SandboxWarmPool`.
  - the Go SDK exposes lifecycle, command, and file operations that Orka already
    adapts into `WorkspaceExecutor`.
  - `SandboxTemplate` has built-in network policy management.

The main architectural difference is therefore: agent-sandbox already supplies
Orka's exec/files workspace surface, while Substrate supplies durable actor
lifecycle plus routing. Orka must provide its own daemon inside Substrate Actors
to regain the exec/files surface.

## Current Relevant Layers

Orka and Substrate operate at different layers:

```text
User or API client
  |
  v
Orka Task API
  - owns task intent, authz, sessions, prompt, provider config, result contract
  |
  v
Orka Task controller
  - validates workspace provider
  - creates Kubernetes worker Job
  - injects resolved workspace env
  |
  v
Outer Orka worker Job
  - runs in Kubernetes as today
  - claims/resumes workspace backend
  - stages inner worker binary and token files
  |
  v
Workspace backend provider
  - agent-sandbox today
  - Substrate after this integration
  |
  v
Substrate Actor runtime
  - Actor lifecycle: create, resume, suspend, delete
  - WorkerPool warm capacity
  - gVisor/runsc checkpoint and restore
  - router/DNS path to active actors
  |
  v
Inner Orka worker process
  - clone/prepare workspace
  - run Codex, Claude, Copilot, or other agent runtime
  - submit result and artifacts back to Orka
```

Substrate is a lower execution substrate. Orka remains the higher-level agent
task system.

## Key Design Decisions

### Provider Selection

The Task selects the provider explicitly:

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Task
metadata:
  name: substrate-agent-task
spec:
  type: agent
  agentRef:
    name: codex-agent
  prompt: "Continue this implementation."
  sessionRef:
    name: substrate-demo
    create: true
  execution:
    workspace:
      enabled: true
      provider: substrate
      templateRef:
        name: orka-codex
        namespace: ate-demo
      reusePolicy: session
      cleanupPolicy: retain
```

Rules:

- `spec.execution.workspace.provider` is the source of truth.
- `provider: substrate` requires Substrate support to be enabled in Orka.
- `provider: agent-sandbox` requires agent-sandbox support to be enabled in Orka.
- If `provider` is omitted, keep current behavior and default to
  the configured default provider, whose built-in compatibility default is
  `agent-sandbox`.
- Do not auto-select based on what is installed in the cluster. Auto-selection
  would make task behavior depend on ambient cluster state and could silently
  move a task between different isolation and persistence models.
- Standard worker execution is not a workspace provider. If
  `workspace.enabled` is false or omitted, the worker runs directly in its
  Kubernetes Job as it does today.

### Wrapper-First Integration

Use the existing worker-wrapper pattern first.

Why:

- It matches the current agent-sandbox architecture.
- It avoids moving task execution into the controller.
- It preserves existing worker auth, result submission, artifact upload, and
  runtime-specific behavior.
- It limits the first implementation to one existing seam:
  `internal/workspace.WorkspaceExecutor`.
- It lets Substrate prove useful without requiring Orka to own Substrate
  installation, scheduling, or storage.

Controller-direct execution can be a later milestone after Substrate is stable
enough and Orka has stronger status/reporting requirements for actor lifecycle.

### Workspace Daemon Requirement

Substrate's public API is actor lifecycle, not a generic exec and file API.

Substrate exposes:

- `CreateActor`
- `ResumeActor`
- `SuspendActor`
- `DeleteActor`
- `GetActor`
- `ListActors`
- `ListWorkers`

It does not currently provide the agent-sandbox-style operations Orka needs:

- execute command
- upload files
- download files
- scrub staged secret files
- report command exit code and output

Therefore, the Substrate `ActorTemplate` used by Orka must run a small Orka
workspace daemon. The daemon is the bridge between Orka's `WorkspaceExecutor`
contract and the process/filesystem inside the Actor.

## Public API Changes

Extend `ExecutionWorkspaceSpec` with a provider field:

```go
// WorkspaceProvider selects the execution workspace backend.
// +kubebuilder:validation:Enum=agent-sandbox;substrate
type WorkspaceProvider string

const (
    WorkspaceProviderAgentSandbox WorkspaceProvider = "agent-sandbox"
    WorkspaceProviderSubstrate    WorkspaceProvider = "substrate"
)

type ExecutionWorkspaceSpec struct {
    Enabled bool `json:"enabled,omitempty"`

    // Provider selects the workspace backend. When omitted, the controller
    // resolves the configured default workspace provider; the built-in
    // compatibility default is agent-sandbox.
    // +optional
    Provider WorkspaceProvider `json:"provider,omitempty"`

    TemplateRef   *WorkspaceTemplateReference `json:"templateRef,omitempty"`
    ReusePolicy   WorkspaceReusePolicy        `json:"reusePolicy,omitempty"`
    CleanupPolicy WorkspaceCleanupPolicy      `json:"cleanupPolicy,omitempty"`
}
```

Do not add a separate `spec.execution.substrate` block for v1. Substrate should
be a workspace provider because Orka already has a backend-neutral execution
workspace contract.

Add provider-neutral workspace status to `TaskStatus`:

```go
type ExecutionWorkspaceStatus struct {
    Provider      WorkspaceProvider             `json:"provider,omitempty"`
    TemplateRef   *WorkspaceTemplateReference   `json:"templateRef,omitempty"`
    Phase         ExecutionWorkspacePhase       `json:"phase,omitempty"`
    Reason        ExecutionWorkspaceReason      `json:"reason,omitempty"`
    ReusePolicy   WorkspaceReusePolicy          `json:"reusePolicy,omitempty"`
    CleanupPolicy WorkspaceCleanupPolicy        `json:"cleanupPolicy,omitempty"`
    Reused        bool                          `json:"reused,omitempty"`
    Message       string                        `json:"message,omitempty"`
    LastUpdateTime *metav1.Time                 `json:"lastUpdateTime,omitempty"`
}

type ExecutionWorkspacePhase string
type ExecutionWorkspaceReason string
```

The status type should not expose provider-native identifiers in v1. In
particular, do not expose Substrate actor IDs, snapshot URIs, worker pod IPs,
daemon URLs, token paths, or handoff credentials.

## Controller Configuration

Keep agent-sandbox config intact. Add a provider-neutral default and Substrate
config beside it:

```text
--execution-workspace-default-provider
--substrate-enabled
--substrate-api-endpoint
--substrate-api-ca-file
--substrate-api-insecure-skip-verify
--substrate-router-url
--substrate-actor-dns-suffix
--substrate-default-template
--substrate-default-template-namespace
--substrate-claim-timeout
--substrate-command-timeout
--substrate-cleanup-policy
```

Environment variables:

```text
ORKA_EXECUTION_WORKSPACE_DEFAULT_PROVIDER
ORKA_SUBSTRATE_ENABLED
ORKA_SUBSTRATE_API_ENDPOINT
ORKA_SUBSTRATE_API_CA_FILE
ORKA_SUBSTRATE_API_INSECURE_SKIP_VERIFY
ORKA_SUBSTRATE_ROUTER_URL
ORKA_SUBSTRATE_ACTOR_DNS_SUFFIX
ORKA_SUBSTRATE_DEFAULT_TEMPLATE
ORKA_SUBSTRATE_DEFAULT_TEMPLATE_NAMESPACE
ORKA_SUBSTRATE_CLAIM_TIMEOUT
ORKA_SUBSTRATE_COMMAND_TIMEOUT
ORKA_SUBSTRATE_CLEANUP_POLICY
```

Recommended defaults:

```text
defaultProvider: agent-sandbox
enabled: false
apiEndpoint: api.ate-system.svc:443
apiCAFile: ""
apiInsecureSkipVerify: false
routerUrl: http://atenet-router.ate-system.svc
actorDnsSuffix: actors.resources.substrate.ate.dev
claimTimeout: 2m
commandTimeout: 30m
cleanupPolicy: delete
```

Helm values:

```yaml
controller:
  executionWorkspace:
    defaultProvider: agent-sandbox
  substrate:
    enabled: false
    apiEndpoint: api.ate-system.svc:443
    apiCAFile: ""
    apiInsecureSkipVerify: false
    routerUrl: http://atenet-router.ate-system.svc
    actorDnsSuffix: actors.resources.substrate.ate.dev
    defaultTemplate: ""
    defaultTemplateNamespace: ""
    claimTimeout: 2m
    commandTimeout: 30m
    cleanupPolicy: delete
```

Do not add `cleanupTimeout` in v1. Workspace cleanup is part of the provider
lifecycle and should use `claimTimeout`; `commandTimeout` remains only for the
inner worker command.

Production Substrate API access should use explicit trust material through
`apiCAFile` or an equivalent mounted Secret. `apiInsecureSkipVerify` is only for
local kind smoke tests and should default to false.

## Controller Validation

When `workspace.enabled: true`:

- Reject unsupported providers.
- Reject `provider: substrate` unless `SubstrateEnabled` is true.
- Reject `provider: agent-sandbox` unless `AgentSandboxEnabled` is true.
- Keep workspace requests limited to `type: agent` Tasks.
- Keep rejecting `Agent.spec.execution.workspace`; v1 only supports
  `Task.spec.execution.workspace`.
- Require `templateRef.name` unless the selected provider has a default
  template.
- Require `sessionRef.name` when `reusePolicy: session`.
- Validate cleanup policy and reuse policy as today.

For Substrate template validation:

- If Orka has the Substrate CRD type registered or uses unstructured clients,
  validate that `ate.dev/v1alpha1 ActorTemplate` exists.
- If `.status.phase` is readable, require or warn that it is ready. Prefer a
  hard failure for v1 because a non-ready ActorTemplate cannot satisfy the task.
- Require Orka compatibility markers before using an ActorTemplate:

  ```yaml
  metadata:
    labels:
      orka.ai/execution-workspace: "true"
      orka.ai/workspace-provider: substrate
    annotations:
      orka.ai/workspace-protocol: http-json-v1
      orka.ai/workspace-daemon-port: "80"
      orka.ai/workspace-staging-root: /app
      orka.ai/agent-runtimes: codex
  ```

- Fail validation if required provider/protocol markers are missing. Runtime
  annotations may be optional at first, but if present they must include the
  requested agent runtime.
- Do not require Orka to create `WorkerPool` or `ActorTemplate` resources.
- Validate lazily per Task. A controller may start when Substrate is disabled or
  temporarily absent; only Tasks selecting Substrate require Substrate resources.

## Status Reporting

Add safe, provider-neutral workspace lifecycle status to Tasks that request an
Execution Workspace:

```yaml
status:
  executionWorkspace:
    provider: substrate
    templateRef:
      name: orka-codex
      namespace: ate-demo
    phase: Retained
    reason: WorkspaceRetained
    reusePolicy: session
    cleanupPolicy: retain
    reused: true
    message: "workspace retained for session reuse"
```

Allowed phases should reuse Orka's existing backend-neutral workspace phases:

```text
Pending
Ready
Released
Retained
Deleted
Failed
```

Common non-failure reasons:

```text
WorkspacePending
WorkspaceClaimed
WorkspaceReady
WorkspaceReleased
WorkspaceRetained
WorkspaceDeleted
```

Status ownership:

- The controller writes validation and attachment-lock failures before Job
  creation.
- The worker writes lifecycle status for claim, readiness, handoff, command, and
  cleanup because those operations happen in the worker-wrapper path.
- The controller should not poll Substrate Actors or agent-sandbox claims to
  infer normal workspace progress.
- Intermediate status update failures should be logged and retried briefly, but
  they should not fail the agent command. The result endpoint remains the source
  of command completion.
- Final Task success still requires the requested cleanup state to be reached.
  If cleanup cannot retain, release, or delete as requested, fail the Task with
  a workspace cleanup reason even if the inner agent command succeeded.

Add a small authenticated internal endpoint beside result submission:

```text
POST /internal/v1/tasks/{namespace}/{taskName}/execution-workspace/status
```

Request shape:

```json
{
  "provider": "substrate",
  "phase": "Ready",
  "reason": "WorkspaceReady",
  "templateRef": {"name": "orka-codex", "namespace": "ate-demo"},
  "reusePolicy": "session",
  "cleanupPolicy": "retain",
  "reused": true,
  "message": "workspace ready",
  "observedAt": "2026-05-20T00:00:00Z"
}
```

The endpoint should use the same worker authentication and namespace check as
`POST /internal/v1/results/{namespace}/{taskName}`. It should reject attempts to
update a Task outside the worker's namespace.

Provider-neutral failure reasons:

```text
WorkspaceValidationFailed
WorkspaceAttachmentLocked
WorkspaceClaimFailed
WorkspaceReadinessFailed
WorkspaceHandoffFailed
WorkspaceCommandFailed
WorkspaceSecretScrubFailed
WorkspaceCleanupFailed
WorkspaceStatusUpdateFailed
```

`WorkspaceStatusUpdateFailed` is normally a worker log/event reason, not a Task
failure reason, unless the same controller/API failure also prevents result
submission or cleanup reporting.

Do not expose provider-native sensitive or low-level details in v1 status:

- raw Substrate snapshot URI
- Workspace Handoff Credential
- TxTokens or credential paths
- full daemon URL
- provider-native worker pod IP
- provider-native snapshot state

Substrate actor ID should remain out of v1 status unless a later debugging need
requires a safe external reference field. Worker logs may include redacted
provider lifecycle messages, but Task status should stay provider-neutral.

## Generic Worker Environment

Add a generic workspace env contract:

```text
ORKA_EXECUTION_WORKSPACE_ENABLED
ORKA_EXECUTION_WORKSPACE_PROVIDER
ORKA_EXECUTION_WORKSPACE_TEMPLATE_NAME
ORKA_EXECUTION_WORKSPACE_TEMPLATE_NAMESPACE
ORKA_EXECUTION_WORKSPACE_CLAIM_NAMESPACE
ORKA_EXECUTION_WORKSPACE_CLAIM_NAME
ORKA_EXECUTION_WORKSPACE_REUSE_POLICY
ORKA_EXECUTION_WORKSPACE_REUSE_KEY
ORKA_EXECUTION_WORKSPACE_CLEANUP_POLICY
ORKA_EXECUTION_WORKSPACE_CLAIM_TIMEOUT_SECONDS
ORKA_EXECUTION_WORKSPACE_COMMAND_TIMEOUT_SECONDS
ORKA_EXECUTION_WORKSPACE_STATUS_ENDPOINT
ORKA_EXECUTION_WORKSPACE_DEPTH
```

Substrate-specific worker env:

```text
ORKA_SUBSTRATE_API_ENDPOINT
ORKA_SUBSTRATE_API_CA_FILE
ORKA_SUBSTRATE_API_INSECURE_SKIP_VERIFY
ORKA_SUBSTRATE_ROUTER_URL
ORKA_SUBSTRATE_ACTOR_DNS_SUFFIX
```

Compatibility:

- Keep existing `ORKA_AGENT_SANDBOX_*` env parsing.
- Prefer the generic env when present.
- Render both the generic env and legacy `ORKA_AGENT_SANDBOX_*` env for
  agent-sandbox during the migration.
- Existing agent-sandbox Tasks should continue to work unchanged.
- Do not add a cleanup-timeout env in v1. Use claim timeout for lifecycle
  cleanup.

## Substrate Workspace Executor

Add `internal/workspace/substrate.go` implementing `WorkspaceExecutor`.

Recommended internal dependencies:

- Generate a small internal gRPC client from Substrate's `ateapi.proto`.
- Do not import the full Substrate Go module into Orka if avoidable. The full
  module pulls newer Kubernetes dependencies and many cloud dependencies that
  Orka does not need at runtime.
- Wrap generated clients behind a small `substrateControlClient` interface for
  tests.
- Use bounded retries for idempotent lifecycle reads and transient control API
  failures. Do not retry non-idempotent daemon command execution by default.
- Use `claimTimeout` for claim, readiness, release, retain, and delete
  operations.
- Use `commandTimeout` only for the inner worker command.

### Identity Mapping

Map Orka workspace identity to Substrate Actor identity:

```text
WorkspaceRef.Namespace -> ActorTemplate namespace or logical provider namespace
WorkspaceRef.ClaimName -> Orka stable claim name
WorkspaceRef.ID        -> Substrate actor_id
```

Actor ID rules:

- Must be DNS-1123 label compatible and no more than 63 characters.
- For `reusePolicy: session`, derive:

  ```text
  orka-s-<sha256(provider, task namespace, template namespace, template name, reuse key)[:32]>
  ```

- For `reusePolicy: none`, derive:

  ```text
  orka-t-<task uid prefix>-<attempt>
  ```

  The controller should resolve and pass this value into worker env. Do not
  derive one-shot actor IDs from only task name because retries and recreated
  Tasks can collide.

### Method Mapping

`Claim`

- Call `GetActor(actor_id)`.
- If found, return reused.
- If not found and `CreateIfMissing`, call `CreateActor`.
- Pass `actor_template_namespace` and `actor_template_name` from
  `TemplateRef`.
- Return `PhasePending` for newly created suspended actors.

`WaitReady`

- Call `ResumeActor(actor_id)`.
- Poll `GetActor(actor_id)` until `STATUS_RUNNING` and actor has an active
  worker location.
- Then call daemon `GET /healthz` through the router until healthy.
- Return `PhaseReady`.

`Exec`

- POST to daemon `POST /v1/exec`.
- Use the Substrate router URL with `Host: <actor-id>.<actorDnsSuffix>`.
- Return stdout, stderr, exit code, truncation flags, started/finished times.

`Upload`

- PUT to daemon `PUT /v1/files`.
- Preserve requested mode where possible.

`Download`

- POST to daemon `POST /v1/files/download`.
- Support explicit path list and recursive download when paths are empty.

`Release`

- If `Retain` is true:
  - call daemon scrub first
  - fail if scrub fails, because retaining a known-unscrubbed snapshot is unsafe
  - call `SuspendActor`
  - return `PhaseRetained`
- If `Retain` is false:
  - call daemon scrub first
  - call `SuspendActor`
  - return `PhaseReleased`

`Delete`

- If actor is running, call daemon scrub first.
- Call `SuspendActor` when needed because Substrate only deletes suspended
  actors.
- Call `DeleteActor`.
- Return `PhaseDeleted`.
- For delete, a scrub failure should not stop a delete attempt. If delete
  succeeds, the requested post-run state was reached. If delete fails after a
  scrub failure, report `WorkspaceCleanupFailed` and include a sanitized message.

`Describe`

- Call `GetActor`.
- Map statuses:

  ```text
  STATUS_RESUMING    -> Pending
  STATUS_RUNNING     -> Ready
  STATUS_SUSPENDING  -> Released
  STATUS_SUSPENDED   -> Retained or Released, based on local request context
  missing actor      -> Deleted
  ```

## Orka Workspace Daemon

Add a new daemon binary:

```text
cmd/orka-workspace-agent
```

The daemon runs inside the Substrate ActorTemplate. It exposes the minimum API
Orka needs to satisfy `WorkspaceExecutor`.

Use HTTP/JSON v1 for the daemon protocol. Substrate control remains gRPC, but
the daemon is Orka-owned and intentionally small; HTTP/JSON keeps tests,
debugging, and compatibility close to Orka's existing internal worker APIs and
agent-sandbox's HTTP command/file surface.

Endpoints:

```text
GET  /healthz
POST /v1/exec
PUT  /v1/files
POST /v1/files/download
POST /v1/scrub
```

Every endpoint except `/healthz` must require the Workspace Handoff Credential,
for example:

```text
Authorization: Bearer <handoff-token>
```

The credential is generated per outer worker handoff, staged only for that
Task, and scrubbed before retain.

`POST /v1/exec` request:

```json
{
  "command": ["sh", "-c", "echo ok"],
  "env": {"KEY": "value"},
  "workDir": "/workspace",
  "stdin": "base64-encoded-optional-stdin",
  "timeoutSeconds": 1800,
  "maxOutputBytes": 2000
}
```

`POST /v1/exec` response:

```json
{
  "stdout": "ok\n",
  "stderr": "",
  "exitCode": 0,
  "stdoutTruncated": false,
  "stderrTruncated": false,
  "startedAt": "2026-05-20T00:00:00Z",
  "finishedAt": "2026-05-20T00:00:01Z"
}
```

Security requirements:

- Allow paths only under `/app`, `/workspace`, `/home/worker`, and `/tmp`.
- Reject path traversal and unsafe symlinks where practical.
- Run as the Workspace Runtime User, not as root.
- Never log env values, file contents, token paths with contents, bearer tokens,
  TxTokens, or full request bodies.
- Default command timeout must be enforced server-side.
- Output must be bounded even if the caller forgets `maxOutputBytes`.
- `/v1/scrub` must remove staged token files before Actor suspension.

Packaging:

- Add the daemon binary to each agent worker image.
- Substrate `ActorTemplate` should run the daemon as the long-lived process.
- The outer worker stages and invokes the runtime-specific Orka worker binary
  through the daemon for each Task.

## Worker Runtime Changes

Refactor `workers/common/agent_runtime.go`:

- Replace `runAgentInSandbox` with `runAgentInWorkspace`.
- Dispatch by `ORKA_EXECUTION_WORKSPACE_PROVIDER`.
- Keep `runAgentInSandbox` as a compatibility wrapper or test helper.
- Reuse the existing staging behavior:
  - worker binary
  - ServiceAccount token file
  - transaction token file
  - context-token subject token file
  - git askpass helper
- Rename shared helpers from sandbox-specific names to workspace-specific names
  where practical.
- Inner worker must disable workspace recursion:

  ```text
  ORKA_EXECUTION_WORKSPACE_ENABLED=false
  ORKA_EXECUTION_WORKSPACE_DEPTH=<previous+1>
  ```

- For agent-sandbox compatibility, also continue setting:

  ```text
  ORKA_AGENT_SANDBOX_ENABLED=false
  ORKA_AGENT_SANDBOX_DEPTH=<previous+1>
  ```
- Outer worker should emit workspace status events after claim, readiness,
  handoff, command completion/failure, scrub, and cleanup.
- Status event submission should be best effort with short retries. It should
  not wrap the inner command result path.

Cleanup behavior:

- `cleanupPolicy: delete` deletes the Substrate Actor after scrub and suspend
  attempts. If scrub fails, still attempt delete.
- `cleanupPolicy: retain` scrubs staged secrets, then suspends the Actor so the
  filesystem and memory snapshot can be reused.
- `cleanupPolicy: retain` fails if scrub fails; do not snapshot known staged
  credentials.
- Cleanup must return an error if the requested final state is not reached. The
  worker should fail the Task with `WorkspaceCleanupFailed` even if the inner
  command succeeded.
- If cleanup policy is unsupported, retain/suspend rather than delete to avoid
  unexpected data loss.

## Security Model

Substrate snapshots memory and filesystem state. That makes token handling more
sensitive than normal one-shot Jobs.

Mandatory rules:

- Never store raw TxTokens, JWTs, provider credentials, Git tokens, or Service
  Account tokens in Task spec/status/logs.
- Stage token files only for the duration of the inner worker execution.
- Before `SuspendActor`, call daemon scrub for all staged token paths.
- If scrub fails, treat `cleanupPolicy: retain` as failed and surface an error
  rather than snapshotting known credentials.
- For `cleanupPolicy: delete`, still scrub first when the Actor is running, then
  suspend and delete.
- Do not expose the daemon publicly. It is an exec surface.
- Recommend NetworkPolicy that allows only Orka worker pods to reach the
  Substrate router for Orka ActorTemplates.
- Do not make Orka's Helm chart manage Substrate NetworkPolicy by default in v1.
  Substrate selectors, router placement, and CNI behavior are installation
  details. Provide example manifests and make the operator own the policy.
- NetworkPolicy is defense in depth. The daemon must still authenticate every
  non-health request with the Workspace Handoff Credential.

Recommended scrub paths:

```text
/app/orka-sa-token
/app/orka-transaction-token
/app/orka-context-subject-token
/app/orka-git-askpass
/app/orka-workspace-handoff-token
```

## Does Substrate Require gVisor?

Current Substrate effectively requires gVisor/`runsc`.

Important distinction:

- Orka's outer worker Job does not need `runtimeClassName: gvisor`.
- Substrate's own runtime path does need `ateom-gvisor` plus a configured
  `runsc` binary.

The `ActorTemplate` contains `spec.runsc`, and `atelet` downloads the matching
binary by URL/hash into `/run/ateom-gvisor/static-files`. Operators do not need
to preinstall `runsc` on every node manually, but nodes must allow Substrate's
privileged runtime components to run.

Substrate runtime modularity beyond gVisor is roadmap material, not the current
implementation target.

## Installation Boundary

Orka should not install Substrate in v1.

Operators must provide:

- Substrate CRDs.
- `ate-system` control plane.
- `atelet` DaemonSet.
- `atenet-router`.
- snapshot storage, such as GCS or S3-compatible storage.
- `WorkerPool` capacity.
- Orka-compatible `ActorTemplate`.

Orka should validate and use that installation. It should not try to mutate
cluster-level Substrate infrastructure.

Provider installation should not participate in default provider selection.
Even if Substrate CRDs and agent-sandbox CRDs are both present, Orka uses
`spec.execution.workspace.provider` or the configured default provider.

## Local Kind Setup

Substrate has a kind path that can be used for Orka development and smoke
testing. This creates a compatible local cluster with Substrate components,
local S3-compatible snapshot storage, and a Substrate `WorkerPool`.

Do not run Substrate's kind setup against an existing kind cluster name unless
it is okay to recreate it. The script deletes and recreates the named cluster.
Use a dedicated cluster name.

The repo now has an automated kind E2E path for this flow:
`scripts/agent-substrate-e2e.sh`, wired into
`.github/workflows/agent-substrate-e2e.yml`. Keep the manual setup below as the
operator/debugging reference; use the script for repeatable validation.

The CI path pins Substrate with `SUBSTRATE_REF`, creates a dedicated kind
cluster and local registry, installs Substrate, initializes the local RustFS
snapshot bucket, builds Orka images, deploys Orka with Substrate enabled, and
runs both Substrate-direct and Orka-mediated task checks.

### 1. Clone Substrate

```bash
git clone https://github.com/agent-substrate/substrate /tmp/agent-substrate-substrate
cd /tmp/agent-substrate-substrate
```

### 2. Create A Dedicated Kind Cluster

```bash
export KIND_CLUSTER_NAME=orka-substrate
hack/create-kind-cluster.sh
```

The script:

- creates a kind cluster
- enables Kubernetes beta APIs needed by Substrate's certificate flow
- configures a local registry
- enables proxy ARP on kind nodes for the gVisor networking path

### 3. Install Substrate Into Kind

```bash
export KIND_CLUSTER_NAME=orka-substrate
hack/install-ate-kind.sh --deploy-ate-system
```

The kind overlay:

- deploys the Substrate control plane
- deploys `atelet`
- deploys `atenet-router`
- deploys ValKey
- deploys RustFS as S3-compatible local snapshot storage
- configures `ATE_STORAGE_BACKEND=s3` for `atelet`

Check health:

```bash
kubectl --context kind-orka-substrate get pods -n ate-system
kubectl --context kind-orka-substrate get svc -n ate-system
kubectl --context kind-orka-substrate get crd | grep ate.dev
```

### 4. Install Orka Into The Same Cluster

From the Orka repo:

```bash
cd /Users/sozercan/projects/orka
make manifests generate
make docker-build-all
kind load docker-image controller:latest --name orka-substrate
kind load docker-image ghcr.io/sozercan/orka/agent-worker-codex:latest --name orka-substrate
kind load docker-image ghcr.io/sozercan/orka/agent-worker-claude:latest --name orka-substrate
kind load docker-image ghcr.io/sozercan/orka/agent-worker-copilot:latest --name orka-substrate
kind load docker-image ghcr.io/sozercan/orka/ai-worker:latest --name orka-substrate
kind load docker-image ghcr.io/sozercan/orka/general-worker:latest --name orka-substrate
```

Then deploy Orka with Substrate support enabled. The exact command depends on
whether the repo uses kustomize or Helm for the local run, but the controller
must receive at least:

```text
--execution-workspace-default-provider=agent-sandbox
--substrate-enabled=true
--substrate-api-endpoint=api.ate-system.svc:443
--substrate-api-insecure-skip-verify=true
--substrate-router-url=http://atenet-router.ate-system.svc
--substrate-actor-dns-suffix=actors.resources.substrate.ate.dev
--substrate-default-template=orka-codex
--substrate-default-template-namespace=ate-demo
```

### 5. Create A Substrate WorkerPool

Example:

```yaml
apiVersion: ate.dev/v1alpha1
kind: WorkerPool
metadata:
  name: orka-agent-pool
  namespace: ate-demo
spec:
  replicas: 2
  ateomImage: ko://github.com/agent-substrate/substrate/cmd/servers/ateom-gvisor
```

In a fully local flow, resolve `ko://` images through Substrate's install
scripts or replace with the image reference pushed or loaded into the kind
cluster.

### 6. Create An Orka-Compatible ActorTemplate

The ActorTemplate must run the Orka workspace daemon as the long-lived process.
The exact image reference should be the Orka agent worker image that contains
`/orka-workspace-agent`.

Example shape:

```yaml
apiVersion: ate.dev/v1alpha1
kind: ActorTemplate
metadata:
  name: orka-codex
  namespace: ate-demo
  labels:
    orka.ai/execution-workspace: "true"
    orka.ai/workspace-provider: substrate
  annotations:
    orka.ai/workspace-protocol: http-json-v1
    orka.ai/workspace-daemon-port: "80"
    orka.ai/workspace-staging-root: /app
    orka.ai/agent-runtimes: codex
spec:
  pauseImage: registry.k8s.io/pause:3.10.2@sha256:f548e0e8e3dc1896ca956272154dde3314e8cc4fde0a57577ee9fa1c63f5baf4
  containers:
  - name: workspace
    image: ghcr.io/sozercan/orka/agent-worker-codex:latest
    command: ["/orka-workspace-agent"]
    ports:
    - containerPort: 80
  workerPoolRef:
    name: orka-agent-pool
    namespace: ate-demo
  snapshotsConfig:
    location: s3://substrate-snapshots/orka-codex/
  runsc:
    amd64:
      url: "gs://gvisor/releases/nightly/2026-05-19/x86_64/runsc"
      sha256Hash: "a397be1abc2420d26bce6c70e6e2ff96c73aaaab929756c56f5e2089ea842b63"
    arm64:
      url: "gs://gvisor/releases/nightly/2026-05-19/aarch64/runsc"
      sha256Hash: "1ba2366ae2efceba166046f51a4104f9261c9cb72c6db8f5b3fe2dc57dea86b9"
```

Notes:

- Keep this manifest as an example. The exact snapshot bucket and runsc URLs
  should match the Substrate version being tested.
- Substrate currently treats ActorTemplates as immutable state roots. Use a new
  template name, such as `orka-codex-v2`, when the daemon or worker image
  changes in an incompatible way.
- Avoid putting provider credentials into ActorTemplate env. Orka should stage
  short-lived credentials per task and scrub them before suspend.

Optional local NetworkPolicy shape:

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: allow-orka-workers-to-substrate-router
  namespace: ate-system
spec:
  podSelector:
    matchLabels:
      app: atenet-router
  policyTypes: ["Ingress"]
  ingress:
  - from:
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: orka-system
      podSelector:
        matchLabels:
          app.kubernetes.io/part-of: orka
    ports:
    - protocol: TCP
      port: 80
```

Treat this as a starting point only. Match the actual labels emitted by the
Substrate install and Orka worker Jobs in the target cluster.

### 7. Smoke Test Substrate Alone

Before testing Orka, verify Substrate can create and resume an Actor:

```bash
kubectl --context kind-orka-substrate -n ate-demo get workerpool
kubectl --context kind-orka-substrate -n ate-demo get actortemplate

go install github.com/agent-substrate/substrate/cmd/kubectl-ate@latest
kubectl ate create actor orka-smoke-1 --template orka-codex --namespace ate-demo
kubectl ate get actor orka-smoke-1
kubectl ate resume actor orka-smoke-1
```

If `kubectl-ate` is run from a local checkout instead:

```bash
cd /tmp/agent-substrate-substrate
go install ./cmd/kubectl-ate
```

### 8. Smoke Test Through Orka

Create a Task that selects Substrate:

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Task
metadata:
  name: orka-substrate-smoke
  namespace: orka-system
spec:
  type: agent
  agentRef:
    name: codex-agent
  prompt: "Print the current working directory and list the workspace root."
  sessionRef:
    name: substrate-smoke
    create: true
  execution:
    workspace:
      enabled: true
      provider: substrate
      templateRef:
        name: orka-codex
        namespace: ate-demo
      reusePolicy: session
      cleanupPolicy: retain
```

Expected behavior:

- Orka creates the outer worker Job.
- Outer worker creates or reattaches a Substrate Actor.
- Substrate resumes the Actor.
- Outer worker uploads the inner Orka worker binary and token files through the
  daemon.
- Inner worker executes the normal agent runtime.
- Outer worker scrubs staged token files.
- Because `cleanupPolicy: retain`, the Actor is suspended and can be reused by
  the next Task in the same session.

Debug commands:

```bash
kubectl --context kind-orka-substrate -n orka-system get tasks
kubectl --context kind-orka-substrate -n orka-system get jobs,pods
kubectl --context kind-orka-substrate -n ate-demo get actortemplate,workerpool
kubectl ate get actor
```

### 9. Run The Automated Kind E2E

The automated script is the preferred local validation entry point:

```bash
PATH="$(go env GOPATH)/bin:$PATH" \
SUBSTRATE_E2E_EXTENDED=1 \
bash scripts/agent-substrate-e2e.sh
```

Prerequisites: Docker, Go, git, curl, `kind`, `kubectl`, `ko`, and `jq`.

Useful overrides:

```bash
KIND_CLUSTER=orka-agent-substrate-e2e-local \
KIND_REGISTRY_NAME=orka-agent-substrate-registry-local \
KIND_REGISTRY_PORT=5019 \
SUBSTRATE_E2E_EXTENDED=1 \
bash scripts/agent-substrate-e2e.sh
```

What it validates:

- Substrate control plane rollout in a fresh kind cluster.
- local RustFS snapshot bucket creation for ActorTemplate golden snapshots.
- Orka-compatible `WorkerPool` and `ActorTemplate` readiness.
- direct Substrate actor create/resume/router/daemon exec/suspend/delete.
- Orka `Task` with default Substrate workspace config and `cleanupPolicy:
  delete`.
- Orka `Task` with `cleanupPolicy: retain` when
  `SUBSTRATE_E2E_EXTENDED=1`.
- missing-template failure path for `provider: substrate`.
- controller and worker logs, Task YAML, Kubernetes events, Substrate actor
  state, and worker state on failure.

The script intentionally uses a minimal CI-only Codex worker image from
`workers/agent/codex/Dockerfile.substrate-e2e` because the worker binary is
uploaded into the Substrate actor during the handoff. Keeping that image small
reduces kind runner storage pressure and makes failures easier to diagnose.

## Production Compatibility Requirements

Substrate requires a compatible runtime pool, not just any Kubernetes cluster.

Minimum requirements:

- privileged `atelet` DaemonSet is allowed
- privileged/root `ateom-gvisor` worker pods are allowed
- hostPath `/run/ateom-gvisor` is allowed
- host ports used by `atelet` are allowed
- nodes support the required gVisor/runsc checkpoint and restore flow
- network path to `atenet-router` works from Orka worker pods
- `atelet` can download `runsc` and workload images
- snapshot storage is configured and reachable
- Substrate control API is reachable from Orka workers

For managed clusters, this likely means a dedicated node pool or namespace with
appropriate security policy. For restricted clusters that disallow privileged
pods or hostPath mounts, Substrate will not be viable without upstream runtime
changes.

## Implementation Phases

### Phase 1: API, Config, And Env Contract

- Add `workspace.provider`.
- Add `status.executionWorkspace`.
- Add provider-neutral workspace phases and reasons.
- Add default workspace provider config, defaulting to `agent-sandbox`.
- Add Substrate controller config and Helm values.
- Add generic workspace env struct in `internal/workerenv`.
- Render generic env for workspace-backed Tasks and legacy env for
  agent-sandbox compatibility.
- Add internal workspace status endpoint.
- Keep old agent-sandbox env support.
- Update generated CRDs with `make manifests generate`.

### Phase 2: Worker Refactor

- Introduce `runAgentInWorkspace`.
- Move staging helpers to provider-neutral names.
- Dispatch to provider-specific `WorkspaceExecutor`.
- Add workspace status event submission.
- Make cleanup return errors when requested final state is not reached.
- Keep agent-sandbox behavior unchanged.
- Add tests for provider dispatch and recursion protection.

### Phase 3: Substrate Executor

- Generate or vendor the minimal Substrate control API client.
- Implement `SubstrateWorkspaceExecutor`.
- Implement actor ID generation.
- Implement explicit Substrate API trust config.
- Implement router HTTP calls to the daemon.
- Validate Orka-compatible ActorTemplate markers.
- Add fake gRPC and fake daemon tests.

### Phase 4: Workspace Daemon

- Add `cmd/orka-workspace-agent`.
- Implement health, exec, upload, download, and scrub endpoints.
- Implement Workspace Handoff Credential auth.
- Add path safety and output bounding.
- Add daemon binary to agent worker images.
- Add daemon unit tests.

### Phase 5: Docs And Local Kind Smoke

- Document Substrate prerequisites.
- Add kind setup instructions.
- Add example WorkerPool, ActorTemplate, and Task.
- Add opt-in smoke test instructions.

### Phase 6: Kind CI E2E

- Add a GitHub Actions workflow that installs Substrate and Orka in one fresh
  kind cluster.
- Keep it self-contained and secret-free.
- Pin the Substrate revision under test with `SUBSTRATE_REF`.
- Validate default delete cleanup, retained cleanup, and missing-template
  failure behavior.
- Emit relevant Kubernetes, Orka, Substrate, worker, and task logs on failure.

## Test Plan

Unit tests:

- provider default preserves agent-sandbox
- explicit default provider config is used when `provider` is omitted
- `provider: substrate` requires Substrate feature gate
- `provider: agent-sandbox` still requires agent-sandbox feature gate
- template defaulting is provider-specific
- Substrate template validation requires Orka compatibility markers
- `reusePolicy: session` requires `sessionRef.name`
- status endpoint rejects cross-namespace updates
- status endpoint persists provider-neutral workspace status
- generic env render/parse round trip
- agent-sandbox env compatibility
- generic env is preferred over legacy env when both are present
- cleanup uses claim timeout, with no cleanup-timeout env
- worker dispatch chooses expected executor
- recursion is detected for generic env and legacy env
- workspace status update failures are logged without failing the inner command
- Substrate actor ID generation is deterministic and DNS-1123 compatible
- Substrate executor maps lifecycle states correctly
- daemon rejects unsafe paths
- daemon rejects unauthenticated exec/upload/download/scrub
- daemon truncates output
- daemon scrub removes staged token files

Integration tests:

- fake Substrate control server plus fake daemon validates `Claim -> WaitReady
  -> Upload -> Exec -> Release`.
- delete path suspends running actors before delete.
- retain path fails if scrub fails.
- delete path still attempts deletion after scrub failure and succeeds if the
  Actor is deleted.
- cleanup failure after successful command fails the Task with
  `WorkspaceCleanupFailed`.

Automated kind E2E:

- run `bash scripts/agent-substrate-e2e.sh`
- install Substrate in a dedicated kind cluster
- initialize local RustFS snapshot storage
- build and push local Orka controller, workspace daemon, and minimal Codex
  worker images
- create WorkerPool and ActorTemplate
- run Substrate-only actor smoke through `kubectl-ate`
- run Orka Task with default `provider: substrate`
- run retained cleanup when `SUBSTRATE_E2E_EXTENDED=1`
- verify missing-template failure is surfaced

Manual kind smoke:

- install Substrate in dedicated kind cluster
- deploy Orka with Substrate enabled
- create WorkerPool and ActorTemplate
- run Substrate-only actor smoke
- run Orka Task with `provider: substrate`
- run second Task with same session and verify actor reuse

## Open Risks

- Substrate is young and some architecture docs are aspirational.
- Actor lifecycle APIs are available, but Orka's exec/files API must be supplied
  by the daemon.
- Substrate snapshots can preserve secrets if scrub is missed or fails.
- ActorTemplate image updates need explicit versioning.
- Pulling the full Substrate module into Orka may cause dependency churn.
- kind is useful for smoke testing, but production viability needs a real
  runtime pool test.

## Recommendation

Proceed only as an experimental provider:

- disabled by default
- explicit `provider: substrate`
- wrapper-first
- daemon-required
- HTTP/JSON Workspace Daemon Protocol
- worker-reported provider-neutral workspace status
- no separate cleanup timeout in v1
- Orka-compatible template markers required
- no automatic Substrate installation by Orka
- kind smoke supported for development
- NetworkPolicy examples documented, not managed by Orka by default
- production support gated on a compatible Substrate runtime pool
