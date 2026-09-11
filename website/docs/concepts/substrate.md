---
slug: /substrate
description: "Agent Substrate execution, data-only suspension, checkpoints, and cold restore."
---

# Agent Substrate workspaces

Orka runs direct workspaces, MCP servers, and built-in ACP runtimes on the
unmodified [Agent Substrate](https://github.com/agent-substrate/substrate)
provider. The supported source and protocol are pinned together in
`hack/agent-substrate/upstream.env`. Provider forks, local patches, and
fork-specific `ActorSnapshot` APIs are not part of this integration.

Substrate owns placement, gVisor isolation, snapshot storage, and physical
workers. Orka owns Task outcomes, durable Sessions and transcripts, prompt
leases, cancellation, runtime admission, workspace data references, and
publication. Reading a dormant Session does not start an Actor.

## Native provider setup

Actor, ActorTemplate, Atespace, Worker, and Tag are native ate-api resources.
Only infrastructure such as WorkerPool is a Kubernetes CRD. Create an
infrastructure template through `kubectl ate create actor-template -f`, with
`metadata.atespace` and `metadata.name`, rather than applying an ActorTemplate
CRD. In Orka's `templateRef`, `namespace` names the native Atespace.

ACP dispatch requires `--substrate-enabled` and
`--acp-workspace-dispatch-enabled`, plus the direct egress configuration below.
Class-backed suspension and checkpoint
restore also require `--enable-workspace-provider-api`, Task provenance and
workspace-use admission, and the matching CRDs and webhooks.

Configure these controller connection settings:

| Setting | Purpose |
| --- | --- |
| `--substrate-api-endpoint` | Native TLS gRPC endpoint, usually `api.ate-system.svc:443`. |
| `--substrate-api-ca-file` | Server trust bundle. |
| `--substrate-api-cert-file` and `--substrate-api-key-file` | Client PEM identity, reloaded at each TLS handshake. A projected PodCertificate bundle may supply both paths. |
| `--substrate-api-bearer-token-file` | Alternative to mTLS, reloaded for every RPC. Choose one authentication method. |
| `--substrate-router-url` | In-cluster workload router URL. |
| `--substrate-actor-dns-suffix` | Usually `actors.resources.substrate.ate.dev`. |

The native route is `name.atespace.<suffix>`. Identical Actor names in different
Atespaces remain distinct. An explicit local insecure-TLS option exists, but
the bundled installer uses verified server trust and projected client identity.

Helm can mount control credentials from an existing Secret in the release namespace:

```yaml
controller:
  substrate:
    enabled: true
    directEgressEnabled: true
    apiCredentials:
      existingSecret: substrate-control
      certKey: tls.crt
      privateKeyKey: tls.key
      caKey: ca.crt
    workerNamespaces: [ate-workers]
```

For bearer authentication, select `apiCredentials.bearerTokenKey` instead of
the certificate and private-key keys. Verified TLS requires `apiCredentials.caKey`
for either authentication method, or `apiCAFile` when mounting credentials yourself.
Secret projections support rotation.
`workerNamespaces` grants Pod cleanup and NetworkPolicy management only in the
listed provider namespaces. Keep both settings while disabling Substrate
admission so existing workspaces can still finish cleanup.

Keep that configuration until retained checkpoint catalogs and template journals
are collected, even after the last RuntimePool is gone. Controller startup checks
those records in the controller namespace when Substrate admission is disabled.
Their cleanup watches run even when the optional public checkpoint CRD is
absent; public checkpoint and restore capabilities remain unavailable until
that API is installed and its reconciliation is enabled.

The infrastructure template must select exactly one WorkerPool and specify a
gVisor `sandboxConfig`, resource limits, and snapshot storage. The controller
compiles separate immutable native templates for ACP. It preserves admitted
placement and storage settings and supplies the pinned runtime image, process
capabilities, readiness probe, durable volume, and public bootstrap material.
Native revisions and their ownership are recorded in private controller
ConfigMaps. Unused revisions are collected after journal and checkpoint
references disappear.

Back up the controller's lifecycle ConfigMaps along with its Kubernetes state.
An initialized pool with a missing journal closes admission and blocks deletion
until the original journal is restored or an operator completes recovery.
Existing legacy pools with lifecycle records are not automatically migrated.
Finish their cleanup using the previous controller before upgrading.

MCP Tool IDs that can be attributed to a recorded template or validated current
binding migrate to `name.atespace` without replacing the Actor or its ownership
lease. Orka rechecks endpoint readiness after migration. Historical cleanup IDs
without enough template provenance remain blocked rather than guessing an Atespace.

The WorkerPool must be dedicated to Orka ACP workloads. Orka needs Pod
get/list/delete and NetworkPolicy access in its namespace. It confines worker
egress before delivering credentials. Cross-cluster placement is unsupported.

Configure the provider's ateapi with `--egress-gateway-address=` to use direct
egress, and use a CNI that enforces Kubernetes NetworkPolicies. The provider
sets this mode at boot, so drain and remove existing ACP Actors before changing
it. Then acknowledge
that configuration with Orka's `--substrate-direct-egress-enabled=true`,
`ORKA_SUBSTRATE_DIRECT_EGRESS_ENABLED=true`, or Helm's
`controller.substrate.directEgressEnabled: true`. Orka cannot inspect this
server setting through the native API. The acknowledgement defaults to false
and closes ACP admission before Actor creation or credential delivery. Providers
also withhold their capability advertisement until it is enabled. Disabling it
still permits drain, suspension, and deletion with the existing credentials.

The pinned provider's default transparent gateway hides Actor destinations from
worker NetworkPolicies, and its Envoy handler does not enforce destination
policies. That mode is unsupported for ACP confinement. Direct egress is a
provider-wide setting, so use a dedicated provider instance if other workloads
require the gateway. The bundled local installer changes only this supported
deployment argument on its dedicated cluster and leaves provider source intact.
Direct workspace and MCP admission do not require Orka's acknowledgement flag.

The router's request timeout must cover the longest supported operation; the
local suite sets `--route-timeout=30m` and uses Envoy info logging. Longer router
shutdown survival also needs a suitable drain timeout and Pod termination grace.

For direct commands with an explicit daemon timeout, the client allows up to
five additional seconds to collect the final exit status. A caller context can
impose a tighter overall deadline or cancel the wait. Timeout conformance requires
the daemon's exit code 124; connection or authentication failures do not count.

Direct workspaces using SessionIdentity recover their installed handoff JWT
after executor recreation through a signed, sealed bootstrap request. The reply
is encrypted for that request and the exact challenged process. Recovery does
not replace the credential, write it to controller state, or replay workspace
commands. An unseeded process returns an authenticated empty result before the
client mints a new JWT. Bootstrap signing credentials are required for recovery.

Direct workspace and MCP deletion use upstream `DeleteActor(anyState=true)` to
terminate workloads without creating snapshots. This also permits cleanup of
stateless MCP Actors and failed suspension attempts. ACP teardown continues to
require its controller-owned journal and worker termination proof.

Upstream currently authenticates control clients but does not implement native
resource authorization/RBAC. Treat its control API, router, template operators,
and worker namespace as a trusted infrastructure boundary. Kubernetes `use`
authorization protects Orka workspace and checkpoint selection; it does not add
RBAC to Substrate. Concurrent external mutation of Orka-owned Actors or Tags is
unsupported.

## Execution and cold suspension

`Task.spec.workspace` remains the repository configuration. Clone/read and
publication credentials stay in their existing workspace and publisher
boundaries. They never enter the ACP process tree.

A class-backed Task selects its execution environment independently:

```yaml
spec:
  type: agent
  agentRef: {name: coding}
  sessionRef: {name: work-session, create: true, append: true}
  execution:
    workspace:
      classRef: {name: substrate-coding}
      reusePolicy: session
      onDetach: Suspend
```

The class uses the reserved adapter `acp.workspace.orka.ai/runtime-pool`, a
Substrate `RuntimeProviderConfig`, and a `RuntimeWorkspaceProfile` containing:

```yaml
spec:
  substrate:
    templateRef: {namespace: team, name: coding-infrastructure}
    suspend: {mode: DataOnly}
```

The class must allow Session reuse and Suspend, set an idle timeout or maximum
lifetime, and use Delete deletion policies. See [configuration](../reference/configuration.md)
for the complete class resources.

Each workspace binds a dedicated single-session RuntimePool. On detach:

1. Orka closes admission and authenticates a quiescent supervisor drain.
2. It verifies the exact Actor, worker, and immutable template. The template
   uses `Data` for pause and commit and `ColdBoot` for data restore. Only the
   durable workspace directory participates; runtime credentials, child
   process roots, and process memory remain ephemeral.
3. It drains the single-Actor worker to stop new placement, requests the Data
   snapshot, and captures an independent native Tag. Tag UID, source Actor
   UID/version, original template UID, and observed Data scope are verified.
4. It deletes the exact worker Pod and observes its absence before deleting
   the source Actor and reporting the workspace Suspended.

A successful snapshot alone is never proof that the workload stopped.
Continuation creates a new Actor from the retained Tag using its original
immutable template. Orka then CAS-updates the suspended Actor to the next
compatible template and cold-boots it. The supervisor generates a process-local
X25519 challenge; Orka encrypts and signs bootstrap credentials for that Actor
and challenge before authenticating the new boot. Actor, Pod, boot identity,
and runtime credentials all change.

The native provider has no caller UID/version preconditions on Suspend, Resume,
or Delete. Orka's ConfigMap CAS protects its own operation journal, not provider
mutations. Random non-reused names, immutable template and Tag identities,
explicit creation intents, exact workload deletion, and fresh admission checks
support this cold-only path. An uncertain boot or previously admitted prompt
is not automatically replayed. Full-memory restore remains prohibited by ADR
0030; ADR 0031 replaces the earlier fork-specific DataOnly requirement.

Actor scale-to-zero does not imply WorkerPool Pod scale-to-zero. Upstream
currently provides one Actor slot per worker. Worker capacity and autoscaling
remain operator responsibilities.

`SubstrateActorPool.spec.templateRef` is immutable. Orka persists the first
accepted native ActorTemplate UID in `status.templateUID`, even when the pool
has no Actors. MCP Tools wait for this binding and reject a different native
template UID before acquiring an Actor lease. Create another pool to change
the template or Atespace, or to use a template recreated under the same name.
The original pool retains responsibility for cleaning up its Actors.

## Checkpoints, forks, and recovery

Export a completed checkpoint from an idle suspended workspace:

```yaml
apiVersion: workspace.orka.ai/v1alpha1
kind: ExecutionWorkspaceCheckpoint
metadata:
  name: before-refactor
  namespace: team
spec:
  workspaceRef:
    name: workspace-name
    uid: WORKSPACE_UID
```

Creation requires Kubernetes `use` on the source ExecutionWorkspace. Export
waits for suspension and never interrupts an attached Task. When Ready, the
checkpoint exposes an immutable digest, class revision, and timestamp, without
native identifiers or storage URLs. Its private reference keeps the Data Tag
and original template alive after source workspace deletion.

If the source workspace disappears before Orka acquires that private reference,
export fails with phase `Failed` and reason `SourceMissing`. Temporary read
errors leave the checkpoint `Pending` with reason `SourceUnavailable` so Orka
can retry.

A fresh Task or Session restores the exact accepted reference:

```yaml
spec:
  execution:
    workspace:
      classRef: {name: substrate-coding}
      reusePolicy: session
      restoreFrom:
        name: before-refactor
        uid: CHECKPOINT_UID
        digest: sha256:CHECKPOINT_DIGEST
```

Restore requires `use` on the checkpoint and the class. Namespace, class and
provider revisions, runtime profile/image, and durable layout must match.
The target acquires its own durable reference before Actor creation. Deleting
the public checkpoint cannot invalidate a restore that already acquired data.
Deleting it before acquisition can make a queued restore fail. Task acceptance
alone does not retain checkpoint data.
Continuation Tasks in that restored Session must preserve the original
`restoreFrom` binding. To branch from another checkpoint, create a new Session.

The Task fork API accepts `executionCheckpoint` with the same name/UID/digest.
`afterSeq` selects conversation history, not filesystem state. A fork gets an
independent workspace and, for Session reuse, its own Session. Without an
explicit execution checkpoint it starts with fresh workspace data.

After a failed restore or lost Actor, set `recoverLastCheckpoint: true` on a
new export to accept the last verified checkpoint explicitly. Later work may be
missing. Recovery starts a fresh workspace; it does not retry an uncertain
source Task. With `onDetach: Suspend`, Orka stops the failed attempt's compute
and retains the failed source workspace when it still owns a verified native
checkpoint. The source cannot accept another Task. It continues to count
against suspended-workspace quota and remains subject to the class's idle and
maximum lifetime limits. Export before those limits expire, or before deleting
the source. `onDetach: Delete` still deletes the source and releases its data.
Removing all pool and public references eventually collects the Tag, its
catalog, and unused templates.

## Diagnostics and verification

`go run ./cmd/orka-substrate-doctor --atespace team --template coding-infrastructure`
uses the `ORKA_SUBSTRATE_API_*` environment settings to check authenticated
native connectivity, Tag inventory, template storage/placement, and worker
readiness. It reports adapter capabilities separately from observed checks and
states that native lifecycle preconditions are absent. It does not identify the
server build or prove execution and streaming compatibility.

Run `bash hack/demos/cluster/install-substrate.sh` for local fixture-backed
conformance on a dedicated gVisor kind cluster. This retains a scoped kubeconfig
under `bin/` and does not modify the default kubeconfig. The source must match
the official pin exactly. Existing clusters require explicit reuse; the
installer does not destroy them to recreate the environment.

The suite exercises native authentication, direct sealed execution and files,
MCP execution, ACP Tasks, controller restart, cold continuation, independent
checkpoint restore, runtime-loss recovery after Task settlement, cancellation,
timeout, and cleanup. Protocol/TLS and
fault-injection tests additionally cover lost responses, source replacement,
Tag provenance, reference races, and explicit recovery. Local provider
conformance and the PR workflow must pass before treating an upgraded pin as
validated. A doctor pass or unit fixture pass is not live execution evidence.
