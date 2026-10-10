---
slug: /substrate
description: "External Agent Substrate ACP workspaces, data-only checkpoints, and native MCP Tools."
---

# Agent Substrate workspaces

ACP workspaces use the separately deployed `orka-workspace-substrate` provider.
It targets unmodified Agent Substrate `v0.1.0`, with source and protocol pinned to
commit `fa6d949685a6318940a9a0195c867c864009b820`. The
[provider installation and protocol pin](https://github.com/orka-agents/orka-workspace/blob/main/providers/substrate/README.md)
live in the external workspace repository. Orka core retains a separate native
integration for MCP Tools; enabling it does not enable ACP workspace allocation.

Substrate owns native Actors, placement, gVisor isolation, and snapshot storage.
The external provider owns its private worker pools and allocation journals.
Core owns Task outcomes, RuntimeSessions, prompt leases, credentials, admission,
and settlement. Reading a dormant Session does not start an Actor.

:::tip[Video demo]
Watch [Checkpoint and restore an agent workspace](https://www.youtube.com/watch?v=jsdRB-0LLAc).
:::

## Install the external ACP provider

Install the native backend independently, then the shared workspace CRDs and
ownership policies and the provider's CRDs, RBAC, registration, and Deployment.
Supply the provider's installation-owned `substrate-native-control` Secret.
Control traffic uses authenticated TLS with a CA bundle and either a bearer token
or client certificate/key files. The provider reloads tokens for each request and
certificates for each TLS handshake. These credentials never enter workload
requests, status, templates, or logs.

| External provider flag | Purpose |
| --- | --- |
| `--native-api-endpoint` | Native TLS control endpoint, usually `api.ate-system.svc:443`. |
| `--native-ca-file` | Server trust bundle. |
| `--native-token-file` | Rotating bearer identity. |
| `--native-cert-file`, `--native-key-file` | Alternative rotating client identity. |
| `--actor-dns-suffix` | Direct Actor route suffix, usually `actors.resources.substrate.ate.dev`. |
| `--native-direct-egress=true` | Acknowledges the native direct-egress prerequisite below. |

Actor, ActorTemplate, Atespace, Worker, and Tag are native API resources. WorkerPool
is a Kubernetes CRD. Create infrastructure templates through the native API, such
as `kubectl ate create actor-template -f`, rather than applying an ActorTemplate
CRD. The profile's `templateRef.namespace` names the native Atespace.

The operator creates a `SubstrateProviderConfig` and immutable
`SubstrateWorkspaceProfile` in `substrate.workspace.orka.ai/v1alpha1`. For example:

```yaml
apiVersion: substrate.workspace.orka.ai/v1alpha1
kind: SubstrateWorkspaceProfile
metadata:
  name: substrate-data
spec:
  templateRef:
    name: workspace-infrastructure
    namespace: team
  suspend:
    mode: DataOnly
```

An administrator-managed `ExecutionWorkspaceClass` references that profile and
the registered provider. Require `acp.runtime.v2` explicitly. Suspension requires
interactive session reuse, an allowed Suspend action, and bounded expiry.

Enable core's provider API and ACP dispatch with the required provenance and
class-use admission webhooks. Core has no Substrate ACP backend or native routing
flag. Give its ServiceAccount read-only Pod access in the worker namespaces so
it can independently verify startup and termination. With Helm:

```yaml
controller:
  executionWorkspace:
    dispatchEnabled: true
    workerNamespaces: [ate-workers]
```

The provider's separate RBAC authorizes its native infrastructure and network
policy operations. See [workspace configuration](../reference/configuration.md#workspace-providers).

A Task selects a class without native selectors:

```yaml
spec:
  type: agent
  execution:
    workspace:
      classRef: {name: substrate-coding}
      reusePolicy: session
```

Top-level `Task.spec.workspace` remains the repository and publication contract.

## Private workers and network confinement

Configure ateapi with `--egress-gateway-address=` before setting the external
provider's `--native-direct-egress=true`. This disables the transparent gateway
so worker NetworkPolicies see the admitted destination IPs. The direct route
`<actor>.<atespace>.<suffix>:80` must reach atenet-router from both provider and
core. The provider cannot independently verify the gateway setting.

The infrastructure ActorTemplate must select exactly one operator-owned
WorkerPool. Each allocation copies its frozen worker image, scheduling, resource
settings, labels, and annotations into a private single-replica pool. The provider
creates the admitted egress policy before that pool. Allocation and instance
labels are present in the Pod template at birth, before any Actor can run. It
pins an active, empty, single-Actor worker and exact Pod UID before native Resume.
Later worker replacements never become startup evidence for that request.

Core checks the provider's sealed process evidence and exact worker Pod before
releasing credentials, then verifies the authenticated supervisor boot. Native
worker control registration, certificate management, and storage transfer run
through the native host/control infrastructure. They do not require broad worker
Pod egress exceptions. Native worker and router ingress remain operator-owned;
allow only authorized management callers and Orka runtime routes.

Use a CNI that enforces NetworkPolicy and avoid additional policies that widen
admitted worker egress. Kind's default CNI cannot prove packet enforcement.
Native Suspend, Resume, and Delete lack caller UID/version preconditions. The
provider's Kubernetes journal CAS serializes its own operations without adding
native atomic fencing. Concurrent administrator mutation of provider-owned Actors,
Tags, templates, or pools is outside that ownership boundary.

## Data-only suspension and cold resume

Derived templates enforce Data/Data/ColdBoot. Full-memory restore stays disabled.
The admitted runtime mounts `/durable/orka-workspace` and uses
`ORKA_ACP_DURABLE_WORKSPACE_KEY=shared` for retained workspace data.

After core's authenticated drain, the provider captures and verifies a Data
snapshot and copies it into a private native Tag. It verifies source identity and
provenance before and after capture, drains exact workers, foreground-deletes the
private pool, and observes every recorded Pod UID's absence before deleting the
source Actor or reporting suspension. The operator's source WorkerPool remains
untouched. A snapshot alone is not termination evidence.

Cold resume verifies the retained Tag and frozen ActorTemplate and worker
specification. Only generated allocation and instance labels may rotate; worker
image, isolation, scheduling, resources, and operator labels remain exact. It
starts a fresh Actor and worker, repeats sealed bootstrap, and receives fresh
core credentials. Durable workspace data survives; process memory does not.

Missing journals, foreign placement, changed storage or infrastructure identities,
and uncertain native outcomes close admission. A lost Resume response is not
permission to replay boot. Preserve journals and checkpoint catalogs for explicit
recovery rather than removing ownership markers or finalizers.

## Checkpoints, forks, and recovery {#checkpoints-forks-and-recovery}

The provider advertises `checkpoint.data` and `restore.cold` alongside ACP runtime
and suspension. A public `ExecutionWorkspaceCheckpoint` carries an opaque data
reference bound to provider, source class/profile, namespace, checkpoint UID, and
content digest. It does not expose native Tags or control credentials.

Export must finish before deleting the source workspace. Import requires live
`use` authorization for the checkpoint and exact compatible target infrastructure.
A fresh target workspace restores data into a new Actor; it never resumes source
process memory or credentials. Immutable checkpoint and template references keep
required provider-owned storage alive until their consumers are gone.

The provider does not advertise pooled ACP capacity, generic exec/files, TLS Actor
endpoints, MCP Service workloads, or full-memory restore.

## Native MCP Tools in core

MCP Tools retain `spec.mcp.substrateActor` and optional `SubstrateActorPool` placement.
Enable only that path with `--substrate-mcp-tools-enabled`. The core
`--substrate-api-*`, router, Actor DNS, and control credential settings apply to
this MCP integration, not the external ACP provider.

```yaml
controller:
  substrate:
    mcpToolsEnabled: true
    apiCredentials:
      existingSecret: substrate-mcp-control
      certKey: tls.crt
      privateKeyKey: tls.key
      caKey: ca.crt
```

For bearer authentication use `apiCredentials.bearerTokenKey` instead of certificate
and private-key keys. Keep verified TLS and Secret projections for rotation.
`SubstrateActorPool.spec.templateRef` is immutable and belongs to native MCP Tool
placement. It does not select a Task's ACP workspace.

## Upgrades and proof limits

Drain all old in-tree ACP allocations and retained records through the original
owner before applying the pruned RuntimePool CRD or starting new core. The startup
gate rejects legacy workspace labels in every state, including `Failed` and
`Deleted`, across all namespaces. It never adopts native allocations or strips
finalizers. See [Upgrading](../operations/upgrading.md#external-workspace-migration).

The [external Substrate proof](https://github.com/orka-agents/orka-workspace/blob/main/scripts/external-substrate-e2e.sh)
uses an actual gVisor worker, verifies confinement labels and policy before native
Resume, writes a filesystem marker, captures data, observes exact worker/Actor
cleanup, and restores that marker in a fresh process after source deletion. It
proves native provider lifecycle and checkpoint behavior. Its standalone lane
uses fixture Core admission.

The [native Core Task proof](https://github.com/orka-agents/orka-workspace/blob/v0.1.0-alpha.2/hack/external-substrate-e2e/README.md#actual-core-and-deployed-provider-task-proof)
also passed with deployed Core and provider controllers. It verifies real
fail-closed Task admission, sealed bootstrap, authenticated Serving on port 80,
RuntimeSession execution, and a persisted prompt result. Exact Actor, worker,
private pool, Core pool, and credential retirement are required. The agent is
deterministic and makes no external model requests. Neither native lane proves
packet enforcement with kind's default CNI; full-memory restore remains gated.
