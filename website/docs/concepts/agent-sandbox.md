---
slug: /agent-sandbox
description: "External Agent Sandbox workspaces with Orka ACP execution and PVC-backed cold resume."
---

# Agent Sandbox workspaces

Orka runs an ACP RuntimeSession in an upstream Agent Sandbox through the separately
deployed `orka-workspace-sandbox` provider. Core owns Task execution, runtime
credentials, admission, and settlement. The provider owns Sandbox resources and
reports infrastructure identity through the shared `workspace.orka.ai` API.

The external provider is tested against unmodified upstream `agent-sandbox`
`v1.0.3`. Install and operate that backend separately. For an existing v0.5
installation, complete the upstream
[storage migration](https://github.com/kubernetes-sigs/agent-sandbox/blob/v0.5.6/docs/api-migration-guide.md)
before installing v1.

:::tip[Video demo]
Watch [Suspend an agent workspace and keep its files](https://www.youtube.com/watch?v=DyS9JioSRa0).
:::

## Install and select a class

Install the shared CRDs and ownership admission policies, then the provider's
CRDs, RBAC, registration, and Deployment from the
[external provider repository](https://github.com/orka-agents/orka-workspace/blob/main/providers/sandbox/README.md).
Use a digest-pinned provider image. Its two replicas share a provider-specific
leader-election Lease and use a ServiceAccount distinct from Orka core.

An operator creates a `SandboxProviderConfig` and a same-namespace
`SandboxWorkspaceProfile` for an `ExecutionWorkspaceClass`. These kinds belong to
`sandbox.workspace.orka.ai/v1alpha1`. The class references the registered provider
and immutable profile. An ACP class explicitly requires `acp.runtime.v2`; add
`Suspend` to its allowed lifecycle actions only when the profile enables it.

Enable Orka's provider API and ACP workspace dispatch with the required
provenance and class-use admission webhooks. There is no core Agent Sandbox
backend flag. See [workspace configuration](../reference/configuration.md#workspace-providers).

A Task selects only a class:

```yaml
spec:
  type: agent
  execution:
    workspace:
      classRef:
        name: sandbox-coding
      reusePolicy: session
```

Session reuse requires `spec.sessionRef`. Top-level `Task.spec.workspace` remains
the repository contract for verified source access and clean-room publication.
Provider, template, pool, and native resource selectors do not belong in a Task.

## Startup and ownership

Core freezes the class binding and public supervisor request in a dedicated
single-session RuntimePool. The provider creates an isolated SandboxTemplate,
zero-replica SandboxWarmPool, and SandboxClaim from that request. It verifies the
realized Pod, request revision, and allocation identity before reporting startup
evidence. Core independently verifies the Pod and network policy, completes the
sealed bootstrap exchange, and admits the authenticated runtime instance.
Provider readiness alone never authorizes a prompt.

The provider receives no private attachment credentials. Core releases them only
to the attested process and verifies the resulting boot identity. The runtime Pod
has no Kubernetes service-account token or Git publication credential. Core-owned
NetworkPolicies select its admitted labels; the provider cannot move the Pod or
change its execution template and still receive credentials.

The provider advertises ACP runtime allocation and data-only suspension. It does
not advertise generic exec, files, TLS endpoints, pooled capacity, checkpoint
export, or full-memory restore. Unsupported class requirements fail admission.

## Suspension and deletion

A suspend-capable `SandboxWorkspaceProfile` supplies:

```yaml
apiVersion: sandbox.workspace.orka.ai/v1alpha1
kind: SandboxWorkspaceProfile
metadata:
  name: sandbox-data
spec:
  suspend:
    mode: DataOnly
    volume:
      capacity: 1Gi
      accessModes: [ReadWriteOnce]
```

The class must permit interactive session reuse and suspension, and set
`idleTimeout` or `maxLifetime`. A suspended-workspace count cap additionally
requires `maxLifetime`.

The claim uses a dedicated durable workspace PVC rather than warm capacity. Its
StorageClass must support dynamic provisioning and deletion of the backing PV.
Suspension verifies the exact claim, Sandbox, PVC, and PV, puts the Sandbox in
`Suspended` operating mode, and observes the exact Pod UID's absence before
reporting suspension. Process memory and credentials are not retained.

Cold resume creates a new Pod against the retained volume and repeats startup
attestation and credential bootstrap. Delete waits for the runtime Pod and exact
PVC/PV to disappear. Missing journals, changed storage identities, and uncertain
native outcomes fail closed; removing finalizers is not a recovery procedure.

## Upgrades and proof limits

Before installing the pruned RuntimePool CRD or new core binary, drain every
legacy in-tree workspace through its original owner, including retained workspaces
in `Ready`, `Suspended`, `Failed`, and `Deleted` states. The new core startup gate
rejects these records across all namespaces. It does not adopt or clean them.
See [Upgrading](../operations/upgrading.md#external-workspace-migration).

The [standalone Sandbox proof](https://github.com/orka-agents/orka-workspace/blob/main/scripts/external-sandbox-e2e.sh)
writes a non-root filesystem marker, suspends and resumes with a new Pod and
bootstrap nonce while retaining exact PVC/PV identities, then observes deletion.
It proves provider lifecycle and storage behavior. The
[separate fake-provider core proof](https://github.com/orka-agents/orka-workspace/blob/main/hack/external-workspace-e2e/README.md)
proves actual authenticated Orka RuntimeSession and Task execution. The standalone
Sandbox proof does not make that core integration claim. Kind's default CNI does
not prove NetworkPolicy packet enforcement.
