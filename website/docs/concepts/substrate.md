---
slug: /substrate
description: "External Agent Substrate ACP workspaces, data-only checkpoints, and native MCP Tools."
---

# Agent Substrate workspaces

ACP workspaces use the separately deployed `orka-workspace-substrate` provider.
It targets unmodified Agent Substrate `v1.0.0`, with source and protocol pinned to
commit `b34fbee742906494b5eb4f1f36987176bf8770a4`. The
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
or client certificate/key files. Actor traffic goes through `atenet-router` over
mTLS with a pod certificate, selected by the `ate-target-actor: <atespace>/<actor>`
header. The provider reloads tokens for each request and certificates for each TLS
handshake. These credentials never enter workload requests, status, templates, or
logs.

| External provider flag | Purpose |
| --- | --- |
| `--native-api-endpoint` | Native TLS control endpoint, usually `api.ate-system.svc:443`. |
| `--native-ca-file` | Server trust bundle. |
| `--native-token-file` | Rotating bearer identity. |
| `--native-cert-file`, `--native-key-file` | Alternative rotating client identity. |
| `--native-router-endpoint` | Router URL, default `https://atenet-router.ate-system.svc:443`. |
| `--native-router-ca-file`, `--native-router-cert-file`, `--native-router-key-file` | Router trust and the pod-certificate identity on the router allowlist. |

Authorization is always enforced. Grant each Orka identity least privilege with
access policies rather than bootstrap ownership: `editor` on the Atespaces it uses
and global `viewer` for worker placement reads. Add the provider's and core's
pod-certificate SPIFFE IDs (`spiffe://cluster.local/ns/<namespace>/sa/<serviceaccount>`)
to `--ingress-allowed-spiffe-ids`; no other Orka workload needs router access.
Namespaces whose Pods call the native API need the `ate.dev/api-access=true` label.

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
  substrate:
    podCertificate:
      enabled: true
providerProxy:
  substrateEgressGateway:
    enabled: true
```

Core reaches each Substrate workspace's supervisor only through the configured
`--substrate-router-url`, using its pod certificate and the attested
`ate-target-actor` target; a provider-reported endpoint that differs from that
router is refused. Without the certificate, Substrate pools stay `Degraded`. Core
sends Substrate v1 providers an egress allowlist naming its provider-auth-proxy
and API origins instead of a NetworkPolicy. Because Actor egress arrives from the
egress gateway Pods, `providerProxy.substrateEgressGateway` admits them to the
token-authenticated provider proxy.

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

Agent Substrate v1 has no direct Actor egress. All Actor traffic leaves through the
installation's egress gateway and is denied unless the Actor's `EgressPolicy`
allows it. Workspace requests carry a host-name egress allowlist instead of a
Kubernetes NetworkPolicy: each rule names `HTTP`, `HTTPS`, or `TLSPassthrough`, a
lowercase FQDN (or a single leftmost `*.` wildcard), and its ports. Bare `*`, IP
addresses, and CIDRs are rejected, and an empty list denies all egress. Core always
adds its in-cluster provider-auth-proxy origin. The provider applies the allowlist
before every native Resume and does not resume if it cannot. `HTTPS` rules are
intercepted by the gateway, so only allowlists with an `HTTPS` rule get the
`egress-mitm.ate.dev` and `system-roots.ate.dev` trust bundles and seven CA
variables. Upstream allows at most 32 environment variables per container and the
Orka supervisor uses 31, so core admits only `HTTP` and `TLSPassthrough` rules for
workspaces today.

The infrastructure ActorTemplate must select exactly one operator-owned
WorkerPool (`ate.dev/v1`, one `gvisor` sandbox class, no `template.resources`).
Each allocation copies its frozen worker image, scheduling, labels, and annotations
into a private single-replica pool whose Pod limits leave room for exactly one
template-sized Actor (the Actor's CPU, and its memory plus the 64Mi gVisor
reserve). Upstream workers report up to `--max-actors` Actors, so the provider
verifies that capacity, the gVisor class, the worker epoch, and that the worker's
Actor assignments contain only its own Actor before Resume, while Ready, and
before teardown. Allocation and instance labels are present in the Pod template at
birth. Later worker replacements never become startup evidence for that request.

Core checks the provider's sealed process evidence and exact worker Pod before
releasing credentials, then verifies the authenticated supervisor boot. Native
worker control registration, certificate management, and storage transfer run
through the native host/control infrastructure. They do not require broad worker
Pod egress exceptions. Native worker and router ingress remain operator-owned;
allow only authorized management callers and Orka runtime routes.

Native Delete carries the observed Actor UID as a precondition; Suspend and
Resume have no caller preconditions, and the router can resume a suspended Actor
on traffic. The provider's Kubernetes journal CAS serializes its own operations
without adding native atomic fencing. Concurrent administrator mutation of
provider-owned Actors, Tags, templates, or pools is outside that ownership
boundary.

## Data-only suspension and cold resume

Derived templates set `snapshotConfig.preferredFidelity: SNAPSHOT_FIDELITY_VOLUMES`
and disable golden snapshots, so snapshots never capture process memory and a
fresh Actor always cold-boots. Only `VOLUMES` snapshots are accepted; `ROOTFS` and
`MEMORY` fail closed. Full-memory restore stays disabled.
The admitted runtime mounts `/durable/orka-workspace` and uses
`ORKA_ACP_DURABLE_WORKSPACE_KEY=shared` for retained workspace data.

After core's authenticated drain, the provider suspends synchronously, verifies a
completed `VOLUMES` snapshot, and copies it into a private native Tag. Upstream
Tags record only the source Actor's name, so it fences the source Actor's UID and
version before and after tagging and checks the Tag's template UID. It
foreground-deletes the private pool and observes every recorded worker Pod UID's
absence before deleting the source Actor or reporting suspension. The operator's source WorkerPool remains
untouched. A snapshot alone is not termination evidence.

Cold resume verifies the retained Tag and frozen ActorTemplate and worker
specification. Generated allocation and instance labels rotate. The worker image
follows the operator WorkerPool's current image, which must be pinned by digest,
so a workspace suspended before a Substrate upgrade resumes on the upgraded
worker. Isolation, the pinned SandboxConfig, scheduling, resources, and operator
labels remain exact, and the journal records every image transition. It starts a
fresh Actor and worker, repeats sealed bootstrap, and receives fresh core
credentials. Durable workspace data survives; process memory does not.

The private pool always names its SandboxConfig: an omitted `configRef` on the
operator pool is pinned to the config that pool has resolved. Each checkpoint
records the snapshot's sandbox runtime. If a restore crashes before the
supervisor boots under a different runtime, the workspace fails with the
`SandboxRuntimeRestoreFailed` reason instead of `InstanceFailed`, and its
checkpoint stays available for explicit recovery.

### Worker eviction

A worker Pod can be terminated while it hosts a running workspace: an upgrade
rolls its WorkerPool, a node drains, or a Spot VM is reclaimed. The worker sends
`SIGTERM` to the supervisor, which then has 30 minutes to be suspended before it
is killed. A supervisor that serves a suspendable workspace does not exit. It
stops admitting sessions and prompts, as it would for a drain, and stays alive
for that window.

Core sees the eviction either through the worker Pod's termination or through
the eviction time the supervisor reports in its drain status. The supervisor
reports it even when a suspension or capacity drain was already running and
keeps that drain's reason. Core closes admission and records the
eviction deadline. It then reuses the normal retirement: an authenticated drain,
settlement of prompts already accepted, and a synchronous data-only suspension.
After suspension the workspace resumes on a new worker. The RuntimePool's
`WorkerEviction` condition reports `EvictionDraining`, then `EvictionSuspended`.

Suspension is authorized only after every accepted prompt settles, and never at
or after the deadline. A prompt that outlives the window is lost with the
instance and is never replayed. The pool then reports `EvictionDeadlineExceeded`,
the workspace fails, and its last checkpoint stays retained. Spot reclaims allow
15 seconds to 2 minutes, so only an idle workspace reliably survives them. A
workspace class without Suspend reports `EvictionNotSuspendable` and loses the
instance.

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
`--substrate-api-*` and `--substrate-router-url` settings apply to this MCP
integration, not the external ACP provider.

```yaml
controller:
  substrate:
    mcpToolsEnabled: true
    podCertificate:
      enabled: true
```

`podCertificate` projects the controller's Substrate pod identity and service
trust bundle. The router admits only allowlisted pod certificates, so MCP Tools
need it (or another podidentity-signed client certificate); a bearer token alone
authenticates control calls but cannot reach Actors. Add the controller's SPIFFE
ID to `--ingress-allowed-spiffe-ids`.

The controller is the only Orka client of the router. AI workers and other
callers invoke a Substrate MCP Tool through the authenticated orka-api proxy
published in `Tool.status.endpoint` (`/api/v1/tools/<name>/mcp?namespace=<ns>`).
The proxy requires `get` on that Tool, derives the Actor only from controller-owned
status, injects the Tool's configured credentials, and never forwards the caller's
Orka credentials. OIDC callers and `outboundAccessPolicyRef` are not supported for
these Tools.

Before the proxy injects a Tool's `http.authSecretRef` credential, it checks that
the caller may use it. A Kubernetes caller, including the AI worker's
ServiceAccount, needs `get` on that Secret in the Tool's namespace; a
transaction-token caller needs the credential-read scope. Other callers receive
`403`. The restricted worker profile needs [a grant](#mcp-tool-credential-grant).

A Tool stands for the one MCP callable that carries its name, so permission to use
a Tool is permission to call only that. The proxy accepts a single JSON-RPC
message per request: `initialize`, `notifications/initialized`, or a `tools/call`
whose `params.name` is the Tool's name. It answers `403` for any other method or
callable and `400` for a batch or a malformed message, and it forwards the
message it checked rather than the caller's bytes.

The caller never sees the values the proxy injects. If the Actor echoes the Tool
credential, a sensitive configured header value or the forwarded transaction
token, the proxy replaces it with `[REDACTED]` in the response body and headers,
also when the value is written with JSON string escapes.

MCP Actors get no egress unless `spec.mcp.substrateActor.egress` allows it, using
`http`, `https` (gateway-intercepted), or `tls` (passthrough) rules with a
lowercase FQDN or leftmost `*.` wildcard and ports. The controller applies the
policy before every resume and reports `EgressPolicyReady`. Intercepted `https`
egress requires the operator's ActorTemplate to trust the `egress-mitm.ate.dev`
bundle. MCP templates must set a `wakeupProbe` on the routable port and disable
golden snapshots when Tools request `boot: true`.
`SubstrateActorPool.spec.templateRef` is immutable and belongs to native MCP Tool
placement. It does not select a Task's ACP workspace.

The controller stops an Actor rather than leave it running under a policy it has
not verified for the Tool's current `spec`. If the policy cannot be applied, or an edit is
rejected before that step (for example an invalid egress rule or a missing auth
Secret), the Tool reports `EgressPolicyReady=False` and the controller stops the
Actor it owns, retrying until it has. It suspends the Actor when the template
keeps data, and the next successful reconcile resumes it. A template that
snapshots `VOLUMES` and mounts no durable directory cannot be suspended by
Substrate, so its Actor is deleted and created again later. Substrate also lets
an Actor outlive its template. When the template cannot be read, the controller
cannot suspend the Actor (Substrate refuses without it), so it replaces the
Actor's egress policy with one that denies all egress and keeps retrying the
stop. While the gateway may still apply the previous rules from its cache (see
below), a running Actor is deleted instead, which discards any data on its
durable volumes; a suspended one is left as it is.

A stored policy is not enforced at once. The Substrate egress gateway caches each
Actor's policy by Actor name for `--egress-policy-cache-ttl` (10s by default) and
does not invalidate the entry when the policy changes or the Actor is replaced.
The controller therefore waits `--substrate-egress-policy-propagation` (14s by
default: the cache TTL plus the 4s a policy fetch may take) before it lets an
Actor run under a policy the gateway may not apply yet. During the wait the Tool
reports `EgressPolicyReady=False` with reason `EgressPolicyPropagating` and is
unavailable. Operators will notice two effects:

- **An egress edit restarts the Actor.** When an edit replaces the stored
  policy, a running Actor is stopped first (suspended, or deleted and created
  again if its template cannot be suspended) and resumed after the wait. Expect
  about 15 seconds of downtime for that Tool and, for a template without a
  durable directory, the loss of whatever the Actor held in memory. Edits that
  leave `egress` unchanged do not restart it.
- **A new Actor's first boot waits.** An Actor's name is derived from its Tool
  or pool slot, and a policy deleted and created again looks like a new one, so
  a new policy can share its Actor name with rules the gateway still holds. The
  controller cannot tell, so an Actor runs no sooner than one wait after its
  policy was created or last changed. That includes a pool's Actors created
  ahead of use: their policy is created when a Tool first claims them.

Set the flag to your gateway's cache TTL plus 4s. Set it to `0` only if the
gateway runs with `--egress-policy-cache-ttl=0`; the controller then neither
waits nor restarts Actors.

### Grant restricted AI workers the Tool credential {#mcp-tool-credential-grant}

Workers used to read a Substrate MCP Tool's `http.authSecretRef` themselves, from
a Secret mounted into the worker or from the Kubernetes API. The proxy now reads
it, and only for a caller allowed to `get` that Secret. A copy mounted into the
worker is no longer used for these Tools.

The default AI worker role can read Secrets, so nothing changes there. With
`workers.ai.rbacProfile: restricted` the worker has no Secret access, and a
Substrate MCP Tool with `authSecretRef` fails with
`403 not authorized to use the tool credential` after the upgrade. Grant the
worker ServiceAccount `get` on that one Secret, in the namespace that holds the
Tool. Do not grant Secret access more broadly: the grant also lets the worker read
that Secret directly.

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: mcp-tool-credential
  namespace: orka-system
rules:
  - apiGroups: [""]
    resources: ["secrets"]
    resourceNames: ["my-mcp-tool-token"] # the Tool's http.authSecretRef.name
    verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: mcp-tool-credential
  namespace: orka-system
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: mcp-tool-credential
subjects:
  - kind: ServiceAccount
    name: orka-ai-worker # <release fullname>-ai-worker
    namespace: orka-system
```

Tools without `authSecretRef` need no grant.

## Upgrades and proof limits

Agent Substrate 1.x minor releases upgrade in place by following the upstream
[upgrade guide](https://github.com/agent-substrate/substrate/blob/v1.0.0/docs/guides/upgrade.md),
with two Orka-specific rules in its worker step:

- Patch only operator-owned WorkerPools, and pin each new `workerImage` by
  digest. Do not patch the provider's private `<actor>-workers` pools, which
  carry the `substrate.workspace.orka.ai` allocation labels. The provider
  refuses to resume on a changed private pool, and suspended workspaces pick up
  the new operator image on their next resume.
- Before upgrading `ate-api-server`, move running workspaces off old workers by
  deleting their private worker Pods (or draining their nodes). Each one drains
  and suspends as described in [Worker eviction](#worker-eviction), then resumes
  on the new image. Wait until no private pool runs the old image.

Agent Substrate cannot upgrade a pre-1.0 installation in place. Before moving to
v1.0.0, retire every workspace, checkpoint, and MCP Actor created under v0.1.0,
then install Substrate v1.0.0 fresh and upgrade the provider and core. Records
from v0.1.0 fail closed with `LegacySubstrateV0State` rather than being adopted.
The `--substrate-actor-dns-suffix` and `--substrate-session-identity-*` settings
were removed and fail startup if set; core now issues each Actor's workspace
credential itself through the sealed bootstrap.

Native Substrate workspaces reach the controller API and the provider proxy
through the egress gateway, by the host names in `--controller-url` and
`--acp-provider-proxy-base-url`. An Actor cannot resolve a short
`<service>.<namespace>.svc` name, so Core refuses one when it admits a native
workspace: the RuntimePool reports `Degraded` with a message naming the flag
and the fully qualified name to use. Charts and kustomize manifests from this
release set `<service>.<namespace>.svc.<cluster domain>` (chart value
`clusterDomain`, default `cluster.local`). If you set either flag yourself to a
short name, change it to the fully qualified one before using Substrate-native
workspaces. Pod-backed workspaces and other workers keep resolving short names.
The same rule applies to the hosts in a Substrate MCP Tool's
`spec.mcp.substrateActor.egress`.

Installations that run AI workers with `workers.ai.rbacProfile: restricted` and
use Substrate MCP Tools with `http.authSecretRef` must
[grant the worker that Secret](#mcp-tool-credential-grant) before upgrading.

Substrate MCP Tools behave differently around egress from this release on. A
Tool becomes available about 14 seconds later than before when its Actor or its
Actor's egress policy is new,
and editing a Tool's `egress` allowlist restarts its Actor, with about 15 seconds
of downtime. Both come from waiting for the Substrate egress gateway to apply a
changed policy; see [Native MCP Tools in core](#native-mcp-tools-in-core) for the
`--substrate-egress-policy-propagation` flag that sets the wait.

Drain all old in-tree ACP allocations and retained records through the original
owner before applying the pruned RuntimePool CRD or starting new core. The startup
gate rejects legacy workspace labels in every state, including `Failed` and
`Deleted`, across all namespaces. It never adopts native allocations or strips
finalizers. See [Upgrading](../operations/upgrading.md#external-workspace-migration).

The [external Substrate proof](https://github.com/orka-agents/orka-workspace/blob/main/scripts/external-substrate-e2e.sh)
installs the pinned v1.0.0 release with `ate-setup` on a dedicated gVisor kind
cluster. It verifies worker sizing, assignments, and the egress allowlist before
native Resume, writes a filesystem marker, captures `VOLUMES` data, observes exact
worker/Actor cleanup, and restores that marker in a fresh process after source
deletion. It proves native provider lifecycle and checkpoint behavior. Its
standalone lane uses fixture Core admission.

The [native Core Task proof](https://github.com/orka-agents/orka-workspace/blob/v0.1.0-alpha.2/hack/external-substrate-e2e/README.md#actual-core-and-deployed-provider-task-proof)
last passed with deployed Core and provider controllers on Substrate v0.1.0 and
must be rerun on v1.0.0. It verifies real fail-closed Task admission, sealed
bootstrap, authenticated Serving, RuntimeSession execution, and a persisted prompt
result. Exact Actor, worker, private pool, Core pool, and credential retirement are
required. The agent is deterministic and makes no external model requests. Neither
native lane proves packet enforcement; full-memory restore remains gated.
