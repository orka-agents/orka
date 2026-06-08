---
slug: /substrate
---

# Substrate Execution Workspaces

Orka can run agent Tasks inside
[Agent Substrate](https://github.com/agent-substrate/substrate) Actors through
the experimental `substrate` execution workspace provider.

Substrate is not a replacement for Orka's Task API, controller, worker Jobs,
result submission, or artifact handling. Orka still owns the high-level agent
task lifecycle. Substrate provides the lower-level warm, suspendable, stateful
execution environment for Tasks that opt into an execution workspace.

The Substrate provider is disabled by default. Enable it only in clusters where
Substrate is already installed and where an Orka-compatible `ActorTemplate` is
available.

## How It Works

The integration uses Orka's existing workspace wrapper flow:

```text
Task
  -> Orka controller validates workspace settings
  -> Orka controller creates the normal worker Job
  -> outer worker claims or creates a Substrate Actor
  -> outer worker resumes the Actor
  -> outer worker stages the Orka worker binary and handoff token
  -> Orka workspace daemon inside the Actor starts the inner worker
  -> inner worker runs the agent runtime and reports results to Orka
  -> outer worker scrubs staged token files
  -> outer worker suspends or deletes the Actor
```

This keeps the provider boundary behind `internal/workspace.WorkspaceExecutor`.
The controller resolves and validates provider configuration, while the worker
owns actor claim, readiness, command execution, staging, and cleanup.

Substrate's public control API is actor lifecycle oriented. It does not provide
the command execution and file transfer surface that Orka workers need, so Orka
requires a small workspace daemon inside each compatible Substrate Actor.

## When To Use It

Use `provider: substrate` for agent workloads that benefit from a durable,
suspendable execution environment:

- session-scoped workspaces that should stay warm between Tasks
- agent runs that need filesystem state preserved by Substrate snapshots
- local and CI validation of Orka's Substrate workspace behavior

Use the default Kubernetes worker execution path when a Task does not need a
durable execution workspace. Use `provider: agent-sandbox` when the cluster is
configured for Kubernetes SIG agent-sandbox instead of Substrate.

## Requirements

The cluster must provide:

- Substrate CRDs and control plane.
- A running Substrate router, normally `atenet-router`.
- A snapshot store accepted by the Substrate installation, such as the local
  RustFS bucket used by the kind E2E.
- A Substrate `WorkerPool` for Orka Actors.
- An Orka-compatible Substrate `ActorTemplate`.
- Worker nodes that can run Substrate's gVisor based runtime path.

Orka does not install or operate Substrate in production. Install and validate
Substrate separately, then configure Orka to use it.

## Controller Configuration

Enable the provider on the Orka controller:

```bash
--execution-workspace-default-provider=substrate
--substrate-enabled=true
--substrate-api-endpoint=api.ate-system.svc:443
--substrate-router-url=http://atenet-router.ate-system.svc
--substrate-actor-dns-suffix=actors.resources.substrate.ate.dev
--substrate-default-template=orka-codex
--substrate-default-template-namespace=ate-demo
--substrate-bootstrap-token-secret-name=orka-substrate-bootstrap
--substrate-bootstrap-token-secret-key=token
--substrate-claim-timeout=2m
--substrate-command-timeout=30m
--substrate-cleanup-policy=delete
```

The same values can be provided through environment variables:

| Flag | Environment variable | Default |
| --- | --- | --- |
| `--execution-workspace-default-provider` | `ORKA_EXECUTION_WORKSPACE_DEFAULT_PROVIDER` | `agent-sandbox` |
| `--substrate-enabled` | `ORKA_SUBSTRATE_ENABLED` | `false` |
| `--substrate-api-endpoint` | `ORKA_SUBSTRATE_API_ENDPOINT` | `api.ate-system.svc:443` |
| `--substrate-api-ca-file` | `ORKA_SUBSTRATE_API_CA_FILE` | empty |
| `--substrate-api-insecure-skip-verify` | `ORKA_SUBSTRATE_API_INSECURE_SKIP_VERIFY` | `false` |
| `--substrate-router-url` | `ORKA_SUBSTRATE_ROUTER_URL` | `http://atenet-router.ate-system.svc` |
| `--substrate-actor-dns-suffix` | `ORKA_SUBSTRATE_ACTOR_DNS_SUFFIX` | `actors.resources.substrate.ate.dev` |
| `--substrate-default-template` | `ORKA_SUBSTRATE_DEFAULT_TEMPLATE` | empty |
| `--substrate-default-template-namespace` | `ORKA_SUBSTRATE_DEFAULT_TEMPLATE_NAMESPACE` | empty |
| `--substrate-bootstrap-token-secret-name` | `ORKA_SUBSTRATE_BOOTSTRAP_TOKEN_SECRET_NAME` | empty |
| `--substrate-bootstrap-token-secret-key` | `ORKA_SUBSTRATE_BOOTSTRAP_TOKEN_SECRET_KEY` | `token` when a secret name is set |
| `--substrate-claim-timeout` | `ORKA_SUBSTRATE_CLAIM_TIMEOUT` | `2m` |
| `--substrate-command-timeout` | `ORKA_SUBSTRATE_COMMAND_TIMEOUT` | `30m` |
| `--substrate-cleanup-policy` | `ORKA_SUBSTRATE_CLEANUP_POLICY` | `delete` |

When Substrate is enabled, the controller requires explicit API trust
configuration. Use `--substrate-api-ca-file` in production. Reserve
`--substrate-api-insecure-skip-verify=true` for local smoke tests such as the
kind E2E.

The controller also requires a bootstrap token Secret reference. Worker Jobs use
that Secret to authenticate the first handoff-token upload to a fresh or resumed
workspace daemon. Create a Secret with the configured name and key in every Task
namespace that will run Substrate-backed workers, and provide the same token to
the Substrate `ActorTemplate` daemon environment.

If `--execution-workspace-default-provider=substrate` is set, Tasks may omit
`spec.execution.workspace.provider` and still use Substrate. If the default is
left as `agent-sandbox`, each Substrate Task must set `provider: substrate`.

## Task API

Substrate is selected through the existing execution workspace field:

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Task
metadata:
  name: substrate-agent-task
spec:
  type: agent
  agentRef:
    name: codex-agent
  prompt: "Run make test and summarize the result."
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

Workspace fields:

- `enabled`: must be `true` to use any durable execution workspace.
- `provider`: set to `substrate`, or omit it when the controller default is
  `substrate`.
- `templateRef.name`: Substrate `ActorTemplate` name. It may be omitted only
  when the controller has `--substrate-default-template`.
- `templateRef.namespace`: Substrate `ActorTemplate` namespace. It defaults to
  the Task namespace unless the controller has a provider-specific default.
- `reusePolicy`: `none` creates a fresh Actor. `session` reuses the
  session-scoped Actor and requires `spec.sessionRef.name`.
- `cleanupPolicy`: `delete` removes the Actor after the Task. `retain` scrubs
  staged secrets and suspends the Actor.
- `boot`: asks Substrate to boot the actor from scratch on first resume instead
  of relying on the provider's default snapshot behavior.
- `poolRef`: places the Task on an operator-managed `SubstrateActorPool`.
  Pooled workspaces currently require `cleanupPolicy: delete`; the controller
  rejects pooled `retain` until workspace reset is available.
- `snapshot`: reserved for explicit provider snapshot restore/checkpoint
  settings. Non-empty snapshot settings are currently rejected.
- `hibernation`: reserved for resident process reuse. `processMode: resident`
  is currently rejected.

The controller rejects `provider: substrate` when Substrate support is disabled,
when the provider name is unknown, when a required template is missing, or when
the referenced `ActorTemplate` is not Orka-compatible.

## Actor Pools

`SubstrateActorPool` is an Orka CRD for operator-owned actor pool capacity. A
pool points at one Substrate `ActorTemplate`, optionally records the intended
Substrate `WorkerPool`, and reports sanitized density telemetry.

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: SubstrateActorPool
metadata:
  name: codex-substrate-pool
  namespace: default
spec:
  templateRef:
    name: orka-codex
    namespace: ate-demo
  workerPoolRef:
    name: orka-workers
    namespace: ate-demo
  targetActors: 4
  targetWorkers: 2
  precreateActors: true
```

Pool fields:

- `templateRef`: Substrate `ActorTemplate` used for all actors in the pool.
- `workerPoolRef`: optional Substrate `WorkerPool` the pool targets for
  capacity and density reporting.
- `targetActors`: desired stateful actor count, capped at `1000`.
- `targetWorkers`: intended physical worker budget. `targetActors` may exceed
  this value to express oversubscription.
- `precreateActors`: asks the controller to create deterministic warm actors up
  to `targetActors`; Substrate may suspend actors when workers are full.

Tasks opt into a pool with `spec.execution.workspace.poolRef`:

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Task
metadata:
  name: pooled-substrate-agent-task
spec:
  type: agent
  agentRef:
    name: codex-agent
  prompt: "Run make test and summarize the result."
  execution:
    workspace:
      enabled: true
      provider: substrate
      templateRef:
        name: orka-codex
        namespace: ate-demo
      poolRef:
        name: codex-substrate-pool
      boot: true
```

The pool and Task/Tool template references must resolve to the same
`ActorTemplate`. When namespace isolation is enforced, cross-namespace `poolRef`
values are rejected.

## Workspace Status

Task status exposes a provider-neutral workspace lifecycle:

```yaml
status:
  executionWorkspace:
    provider: substrate
    templateRef:
      name: orka-codex
      namespace: ate-demo
    phase: Deleted
    reason: WorkspaceDeleted
    reusePolicy: session
    cleanupPolicy: delete
    reused: false
    placement:
      workerNamespace: ate-demo
      workerPool: orka-workers
      workerPodName: orka-workers-0
    density:
      workerCount: 1
      actorCount: 2
      runningActorCount: 1
      suspendedActorCount: 1
      actorsPerWorker: "2.00"
    resumeLatency: 2.3s
    message: workspace deleted
```

Possible phases are `Pending`, `Ready`, `Released`, `Retained`, `Deleted`, and
`Failed`.

The status surface is intentionally sanitized. It must not expose Substrate actor
IDs, snapshot URIs, worker pod IPs, daemon URLs, staged token paths, request
tokens, or raw provider credentials.

`placement`, `density`, and `resumeLatency` are best-effort visibility fields.
They are safe for status and telemetry, but they should not be used as stable
provider-native identifiers.

## ActorTemplate Requirements

An Orka-compatible Substrate `ActorTemplate` must run the Orka workspace daemon
as the long-lived actor process. The controller validates the following labels
and annotations:

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

The daemon port must match the daemon container's literal
`ORKA_WORKSPACE_AGENT_LISTEN_ADDR` value, or the daemon default `:8080` when
that environment variable is omitted. The pinned Substrate router currently
forwards actor HTTP traffic to port `80`, so Orka agent worker images grant the
workspace daemon `cap_net_bind_service` while still running it as a non-root
user. The staging root is currently required to be `/app`, because the worker
handoff stages the inner worker binary and secret scrub paths under that
directory.
Use the Orka agent worker image for the runtime, not the daemon-only image; the
ActorTemplate container must include `/orka-workspace-agent`, the selected CLI,
and normal workspace tools such as `git`.
The controller also validates that the workspace-daemon ActorTemplate container
defines `ORKA_WORKSPACE_BOOTSTRAP_TOKEN`. Prefer `valueFrom.secretKeyRef` when
the deployed Substrate version propagates Kubernetes env sources into the actor
runtime. The pinned Substrate revision used by the kind E2E propagates literal
env values only, so the CI template uses a generated, ephemeral literal value
that matches the worker Job Secret.

Example shape:

```yaml
apiVersion: ate.dev/v1alpha1
kind: WorkerPool
metadata:
  name: orka-workers
  namespace: ate-demo
spec:
  replicas: 1
  ateomImage: registry.example.com/ateom-gvisor:tag
---
apiVersion: ate.dev/v1alpha1
kind: ActorTemplate
metadata:
  name: orka-codex
  namespace: ate-demo
  labels:
    orka.ai/execution-workspace: "true"
    orka.ai/workspace-provider: substrate
  annotations:
    orka.ai/agent-runtimes: codex
    orka.ai/workspace-daemon-port: "80"
    orka.ai/workspace-protocol: http-json-v1
    orka.ai/workspace-staging-root: /app
spec:
  pauseImage: registry.k8s.io/pause:3.10.2
  containers:
    - name: workspace
      image: registry.example.com/orka/agent-worker-codex:tag
      command:
        - /orka-workspace-agent
      env:
        - name: ORKA_WORKSPACE_AGENT_LISTEN_ADDR
          value: ":80"
        - name: ORKA_WORKSPACE_HANDOFF_TOKEN_FILE
          value: /app/orka-workspace-handoff-token
        - name: ORKA_WORKSPACE_BOOTSTRAP_TOKEN
          value: "${ROTATED_BOOTSTRAP_TOKEN}"
      ports:
        - containerPort: 80
  workerPoolRef:
    name: orka-workers
    namespace: ate-demo
  snapshotsConfig:
    location: gs://ate-snapshots/orka-codex/
  runsc:
    amd64:
      url: gs://gvisor/releases/nightly/2026-05-19/x86_64/runsc
      sha256Hash: a397be1abc2420d26bce6c70e6e2ff96c73aaaab929756c56f5e2089ea842b63
    arm64:
      url: gs://gvisor/releases/nightly/2026-05-19/aarch64/runsc
      sha256Hash: 1ba2366ae2efceba166046f51a4104f9261c9cb72c6db8f5b3fe2dc57dea86b9
```

Use images and `runsc` artifacts that match the target environment. The example
above mirrors the local E2E shape; production installations should pin and
mirror artifacts according to their own supply-chain policy.

## MCP Actor-Backed Tools

Tools can also be backed by a durable Substrate actor that serves MCP over HTTP.
The Tool controller creates or reuses the actor, waits for the MCP endpoint to
be reachable through the Substrate router, and publishes the resolved endpoint
and actor metadata in Tool status.

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Tool
metadata:
  name: repo-inspector
  namespace: default
spec:
  description: "Inspect repository metadata through an MCP server"
  parameters:
    type: object
    properties:
      message:
        type: string
    required:
      - message
  mcp:
    path: /mcp
    substrateActor:
      templateRef:
        name: orka-mcp
        namespace: ate-demo
      poolRef:
        name: mcp-substrate-pool
      boot: true
```

`mcp.path` defaults to `/mcp`. `mcp.substrateActor.templateRef.name` is
required, and `poolRef` is optional. If a pool is set, the pool template must
match the MCP actor template. `boot: true` boots the actor from scratch only on
the first resume; forced reconciles should not restart an already booted MCP
actor.

MCP tools may omit `spec.http` entirely. If the actor endpoint also needs
transport auth, set `spec.http.authSecretRef` and use header injection. Body
auth injection is invalid for MCP tools because call arguments are forwarded to
the MCP server as tool input.

## Security Model

Substrate Actors can preserve memory and filesystem state, so staged credential
handling is strict:

- The outer worker stages only a short-lived handoff token for the inner worker.
- The first handoff-token upload is authenticated with
  `ORKA_WORKSPACE_BOOTSTRAP_TOKEN`, which must match the worker Job bootstrap
  Secret and must not be the token being uploaded.
- The daemon reads the handoff token from the configured file path.
- The daemon removes the bootstrap token from its process environment before
  launching task commands.
- The outer worker calls the daemon scrub endpoint before retaining or deleting
  an Actor.
- `cleanupPolicy: retain` scrubs staged secrets before suspending the Actor.
- `cleanupPolicy: delete` scrubs staged secrets, suspends the Actor, then
  deletes it.
- Task status and logs must not contain raw tokens, credentials, snapshot URIs,
  or provider-native routing details.

Do not place API keys, long-lived credentials, or source-control tokens directly
in an `ActorTemplate`. Use Kubernetes Secrets and Orka's existing runtime secret
mechanisms for Task execution. The bootstrap token is control-plane credential
material; keep the worker-side value in a Secret, rotate it like other cluster
credentials, and ensure the ActorTemplate daemon receives the same value through
the most restrictive env mechanism supported by the deployed Substrate version.

## Local Kind Validation

The repository includes a secret-free kind E2E for Substrate:

```bash
PATH="$(go env GOPATH)/bin:$PATH" \
SUBSTRATE_E2E_EXTENDED=1 \
bash scripts/agent-substrate-e2e.sh
```

Required local tools:

- Docker
- Go
- git
- curl
- `kind`
- `kubectl`
- `ko`
- `jq`

The script:

1. Creates an isolated kind cluster and local registry.
2. Clones the configured Substrate revision.
3. Installs Substrate into kind.
4. Initializes the local RustFS snapshot bucket.
5. Builds Orka controller, workspace daemon, and worker images.
6. Creates an Orka-compatible Substrate `WorkerPool` and `ActorTemplate`.
7. Exercises direct Substrate actor create, resume, router, daemon exec,
   suspend, and delete.
8. Deploys Orka with Substrate enabled.
9. Creates Orka `SubstrateActorPool` resources for agent and MCP actors.
10. Runs Orka Tasks through default and pooled Substrate workspaces.
11. Executes an MCP Tool backed by a pooled Substrate actor and verifies forced
    reconciles do not reboot the MCP actor.
12. Validates Task result submission, workspace placement/density telemetry,
    cleanup status, and missing-template failure.

The pinned Substrate revision can fail `runsc delete` after an Orka Task has
already produced its result in GitHub-hosted kind. The E2E treats that as a
known upstream cleanup failure only when `status.resultRef.available=true` and
`status.executionWorkspace.reason=WorkspaceCleanupFailed`; direct Substrate
actor suspend/delete is still validated separately.

Useful overrides:

```bash
KIND_CLUSTER=orka-agent-substrate-e2e
KIND_REGISTRY_NAME=orka-agent-substrate-registry
KIND_REGISTRY_PORT=5001
SUBSTRATE_REPO=https://github.com/agent-substrate/substrate.git
SUBSTRATE_REF=main
SUBSTRATE_E2E_EXTENDED=1
KEEP_CLUSTER=1
SUBSTRATE_BOOTSTRAP_TOKEN_SECRET_NAME=orka-substrate-bootstrap
SUBSTRATE_BOOTSTRAP_TOKEN_SECRET_KEY=token
```

Set `KEEP_CLUSTER=1` when you want to inspect the cluster after a failure.

## GitHub Actions

`.github/workflows/agent-substrate-e2e.yml` runs the same script in CI. The
workflow is secret-free and is intended to catch regressions in:

- Orka controller validation for Substrate workspaces
- worker Job environment injection
- SubstrateActorPool reconciliation and density reporting
- Substrate actor lifecycle handling
- pooled Task placement
- MCP Tool execution through a Substrate actor
- MCP actor boot-once reuse across forced reconciles
- workspace daemon command execution
- workspace placement, density, and resume latency status
- secret scrub and cleanup behavior
- task status updates for `Deleted`, `Retained`, and validation failures

Validate workflow changes locally with:

```bash
bash -n scripts/agent-substrate-e2e.sh
go run github.com/rhysd/actionlint/cmd/actionlint@latest .github/workflows/agent-substrate-e2e.yml
```

## Troubleshooting

Start with Orka resources:

```bash
kubectl get task -A
kubectl -n default get task <task-name> -o yaml
kubectl -n default get substrateactorpool
kubectl -n default get tool
kubectl -n orka-system logs deployment/orka-controller-manager --tail=200
kubectl -n default get jobs,pods
kubectl -n default logs job/<worker-job-name> --all-containers=true
```

Then inspect Substrate:

```bash
kubectl -n ate-system get pods
kubectl -n ate-demo get workerpool,actortemplate
kubectl -n ate-demo get actortemplate <template-name> -o yaml
kubectl -n ate-system logs deployment/atenet-router --tail=200
kubectl -n ate-system logs deployment/api --tail=200
```

When the Substrate CLI plugin is available, actor and worker state is also
useful:

```bash
kubectl ate get actors
kubectl ate get workers
```

Common failures:

- `execution workspace provider "substrate" requires substrate to be enabled`:
  set `--substrate-enabled=true` on the controller.
- `substrate workspace bootstrap token secret name is required`: set
  `--substrate-bootstrap-token-secret-name` and create that Secret in the Task
  namespace and `ActorTemplate` namespace.
- `execution workspace templateRef.name is required`: set
  `spec.execution.workspace.templateRef.name` or configure
  `--substrate-default-template`.
- `ActorTemplate ... not found`: create the template in the referenced
  namespace or fix `templateRef.namespace`.
- `ActorTemplate ... missing label/annotation`: add the Orka compatibility
  metadata described above.
- `ActorTemplate ... is not Ready`: inspect Substrate `WorkerPool`, snapshot
  config, image pulls, and `runsc` configuration.
- `substrate actor poolRef ... not found`: create the referenced
  `SubstrateActorPool` or fix `spec.execution.workspace.poolRef`.
- `poolRef does not support cleanupPolicy "retain"`: omit `cleanupPolicy` or
  set it to `delete` for pooled workspaces.
- Task reaches `Failed` with `WorkspaceReadinessFailed`: inspect actor state,
  router logs, workspace daemon logs, and DNS suffix configuration.
- Task reaches `Failed` with `WorkspaceCleanupFailed` after
  `resultRef.available=true`: the command completed and result submission
  succeeded, but Substrate failed while checkpointing or deleting the actor.
  Inspect `atelet` and `ateom-gvisor` logs for `runsc delete` errors.
- MCP Tool remains unavailable: inspect `status.error`, verify
  `mcp.substrateActor.templateRef`, check that the actor template is `Ready`,
  and confirm the MCP server responds on `mcp.path` through the Substrate router.
- Cleanup stalls: verify the actor can reach `Suspended`; Substrate deletes only
  suspended Actors.

The E2E script prints failure diagnostics for controller logs, worker Job logs,
Task YAML, Kubernetes events, and Substrate actor/worker state.

## Limitations

Substrate support is experimental and disabled by default. Production users
should validate their Substrate installation, runtime artifacts, snapshot store,
networking, and security posture independently before routing important work
through this provider.

Current boundaries:

- Orka does not install Substrate.
- Orka does not manage Substrate `WorkerPool` capacity.
- Orka does not expose provider-native Substrate identifiers in Task status.
- Orka requires the workspace daemon in compatible ActorTemplates.
- gVisor/runsc support is a Substrate installation requirement for the current
  runtime path.
- `cleanupPolicy: retain` preserves Actor state after secret scrub and suspend;
  operators are responsible for lifecycle and cost management of retained
  Actors.
