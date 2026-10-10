# Workspace provider authoring contract

Provider adapters watch `workspace.orka.ai/v1alpha1` resources whose immutable
`spec.controllerName` matches the adapter. They import only
`api/v1alpha1`, `sdk`, and `sdk/workspaceagent` from an immutable published
[`orka-workspace`](https://github.com/orka-agents/orka-workspace) module version.
The `sdk` import retains the Go package name `workspaceprovider`. Orka consumes
the same shared module and sources its workspace CRDs from that module.

## External ACP providers

ACP classes may select any independently deployed adapter that supports
`orka.workspace.lifecycle.v1` and `acp.runtime.v2`. Its registered
ServiceAccount must hold the registration-scoped virtual `provider-status`
permission. The class uses the adapter's own configuration/profile kinds; core
resolves and freezes their UIDs, generations, and functional hashes through the
REST mapper. Grant read access to both kinds used in that resolution.

Core admits and attaches the exact materialized workspace before publishing
physical demand. The provider acknowledges attachment without waiting for a
running supervisor. Core then writes a numbered immutable public workload to
the workspace. The provider reports exact instance and request-revision evidence;
core independently checks accessible compute/storage, performs the sealed
bootstrap exchange, and opens admission only after the authenticated v2 probe.
Generation acknowledgement is asynchronous and does not authorize prompt dispatch.

`runtime.native-process` declares a fresh writable container filesystem, exact
process startup evidence, and operator-managed infrastructure/router ingress.
Core freezes that capability before hashing the request, omits Kubernetes scratch
mounts, and publishes only runtime Egress policy. The native supervisor intent
declares UID/GID 0 and its exact capability set. The pinned gVisor backend does not
support Kubernetes seccomp profiles or allowPrivilegeEscalation controls, so Core
omits those fields and its Pod node selector from fresh native requests. Native
placement uses the provider's pinned Linux WorkerPool. Core freezes the native
bootstrap listener at port 80 and omits Kubernetes probes, lifecycle hooks and
termination grace periods; the provider owns Actor readiness and exact retirement.
Native requests use supervisor session-directory defaults and SystemInfo identity,
and omit the Pod namespace and configured Core MCP broker environment overrides.
Pod-backed requests retain their health and shutdown settings,
scratch mounts and Ingress/Egress policy. Existing admitted requests keep their
original layout and cannot switch startup evidence kinds. Native operators must
confine worker and router ingress while permitting Orka's authenticated routes.

Native endpoint ingress requires worker labels whose literal values bind both
the attested allocation ID and instance ID. Providers must use label-safe
identities and birth labels unique to that allocation and incarnation. Before
granting ingress, Core requires that only the exact attested worker Pod matches
the full selector, including during deletion, and re-reads its UID and labels
through the authoritative API. Core freezes its two endpoint policy targets
before creation, verifies exact ownership and permissions on retries, and
deletes only its known policies with UID/resourceVersion preconditions. Cold
resume removes predecessor grants before saving the replacement evidence.
A later workload cannot change those targets within the same pool; retire the
exact pool and create a new one to change endpoint namespaces or permissions.

Fresh native requests freeze an opaque Core runtime identity from the exact pool
UID, workspace UID and workload sequence before admission. Raw provider process
identity remains in the separate startup evidence and sealed bootstrap challenge;
Task runtime fences contain only the opaque identity and supervisor boot ID.
Older native requests that used provider identity cannot admit new Tasks. Their
immutable workload remains available for exact authenticated drain and retirement;
retire those instances before continuing with a fresh opaque identity.

Private runtime credentials stay core-owned. Retirement closes admission and
drains the authenticated instance before the provider receives exact sequence
and instance authorization. A missing provider response retains both ownership
fences and finalizers. Independent checkpoint restore additionally binds the
checkpoint UID, digest, class, and provider revision; the provider must acquire
durable artifact ownership before native creation.

Before creating an external runtime credential Secret, Core saves its exact key,
role, epoch, and full data digest, then binds its Kubernetes UID before bootstrap.
Cleanup uses that durable evidence and UID/resourceVersion preconditions, never
pool labels alone. An issued create whose outcome or UID cannot be established
keeps admission and cleanup closed until exact evidence is recovered. Earlier
external auth credentials can recover from saved name/UID bindings or the exact
bootstrap auth UID. Older provider credentials and unbound auth credentials
without that evidence stay untouched and require operator verification and
cleanup; they cannot block a pool finalizer merely by copying its labels.

Core requires provider registration names to be DNS-compatible Kubernetes label
values of at most 63 characters, including for providers without checkpoint use.
The shared checkpoint policy authorizes the exact registration named by the
protected routing label; a hash or annotation cannot replace it. Longer names
can be stored by Kubernetes but are unsupported for new Core admission.

Core establishes checkpoint routing from the immutable source workspace UID.
If the authoritative API proves that an unrouted source is absent or replaced,
Core records `workspace.orka.ai/checkpoint-routing-failure` as `SourceNotFound`
or `SourceUIDMismatch` and stops routing retries. This is a routing diagnostic;
it does not set the provider's checkpoint phase or claim artifact cleanup.
An existing unrouted checkpoint bound to an unsupported provider name receives
`ProviderNameUnsupported` in that same Core-owned annotation. It acquires no
provider status or retained-artifact ownership. Install a supported registration
and use a new workspace and checkpoint to retry that case. Create a new checkpoint
request from a live exact source to retry source loss.
Deleting sources and API outages remain retryable. Already routed checkpoints
and retained artifacts keep their provider ownership after source deletion.
With `--enable-workspace-provider-api=false`, Core preserves checkpoint requests
and existing routes without assigning a provider or writing routing diagnostics.
Runtime retirement and workspace retention cleanup remain active.

The shared repository contains the [provider installation and retirement
guide](https://github.com/orka-agents/orka-workspace/blob/327a82dfdfcd74a146c4e18747b54d2d13b3e3b5/docs/external-providers.md),
provider-specific prerequisites, and conformance/live proof scripts. External ACP
providers use the generic workspace API and ACP dispatch flags. Legacy ACP
cleanup belongs to the previous release; the removal release retains only pooled
Substrate MCP Tools. Its startup gate names legacy allocations that must retire under their
original owner before cutover; copying provider labels does not migrate them.

The workspace-agent connection contract below applies when an adapter exposes
that protocol. ACP's sealed bootstrap and authenticated harness are separate
capabilities; `acp.runtime.v2` alone does not advertise workspace-agent exec,
reset, files, or TLS endpoints.

## Parameter CRD read aggregation

Orka core resolves a direct `ExecutionWorkspaceClass.spec.parametersRef` to
confirm that the referenced namespaced profile exists before reporting the class
Ready. Orka does not ship wildcard access to adapter-owned API groups.

Each adapter installation must create a ClusterRole with the aggregation label
below and read-only access to the adapter's workspace profile CRD:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: substrate-workspace-profile-reader
  labels:
    workspace.orka.ai/aggregate-to-parameter-reader: "true"
rules:
- apiGroups: ["substrate.workspace.orka.ai"]
  resources: ["substrateworkspaceprofiles"]
  verbs: ["get", "list", "watch"]
```

The Orka installation binds its controller ServiceAccount to an aggregated
`workspace-parameter-reader` ClusterRole. Removing an adapter also removes its
specific read grant without changing Orka core RBAC. Provider configuration and
pool parameter CRDs remain adapter-owned and are not granted through this class
profile reader unless the adapter explicitly requires them for class resolution.

## Workspace-agent process identity and capability contract

Providers that run the workspace agent as a root supervisor must let it launch
task commands as the configured unprivileged identity. For the standard Orka
workspace-agent image, the supervisor runs as UID/GID 0 and task commands run as
UID/GID 1000. The provider runtime must grant the supervisor `CAP_SETUID` and
`CAP_SETGID` in its OCI bounding, permitted, and effective capability sets. A
runtime that mirrors capabilities into the inheritable set must include them
there as well.

These capabilities belong only to the supervisor. Providers must not replace a
failed credential transition by running the task command as root. If the
runtime cannot perform the UID/GID drop, command startup must fail closed. The
unprivileged command must not retain the supervisor's capabilities after the
credential transition and exec. The container root and every executable path
used by task commands must also remain traversable by the configured command
identity; a provider-owned `0700` rootfs directory makes every post-drop exec
fail even when the capability set is correct.

## Workspace-agent connection Secret contract

A ready workspace that exposes the workspace-agent data plane sets
`ExecutionWorkspace.status.connectionSecretRef`. The referenced Secret uses the
versioned public contract in `orka-workspace/sdk`. Adapters should build its `data` map with `EncodeConnectionData`; core and
tests decode it with `ParseConnectionData` before constructing a
`workspaceagent.Client`.

| Secret data key | Required | Meaning |
| --- | --- | --- |
| `protocolVersion` | yes | Must equal `workspace.orka.ai/v1` |
| `endpoint` | yes | Absolute workspace-agent HTTP(S) base URL; HTTPS is required unless the explicit insecure flag is set |
| `controlAuth` | yes | Privileged attachment/scrub/reset bearer value; never copy it into status, events, or logs |
| `ca.crt` | no | PEM CA bundle used to verify a private HTTPS endpoint |
| `hostHeader` | no | Explicit HTTP Host value for provider routers |
| `allowInsecure` | no | Boolean development override for plain HTTP; production adapters omit it |

The adapter owns Secret creation and rotation. `ExecutionWorkspace.status`
contains only the namespaced Secret reference and sanitized endpoint metadata.
Because the workspace agent keeps attachment identity in memory while workspace
files may survive a process restart, a secured agent starts fail-closed. Before
the first attachment after every agent start or restart, the adapter must issue
a full control-authenticated reset using the binding generation reported by
`GET /v1/capabilities`, then use the rotated generation returned by reset for
attachment activation.

The shared lifecycle contract and its provider conformance suite live in
[`orka-workspace`](https://github.com/orka-agents/orka-workspace). Providers must
pass that suite before advertising a supported contract. Workspace-agent
implementations must also preserve the data-plane protocol described above.

## Drain before upgrading from in-tree ACP providers

The external provider API and binaries are installed from the same
`orka-workspace` revision that Orka pins in its Go module. The [shared installation
and compatibility guide](https://github.com/orka-agents/orka-workspace/blob/327a82dfdfcd74a146c4e18747b54d2d13b3e3b5/docs/external-providers.md)
lists the supported Kubernetes and backend versions and the provider-owned schemas.

Before replacing Orka or applying the new RuntimePool CRD, drain every legacy ACP
pool, workspace, and retained artifact with its original release and native
backend. The removal release has no in-tree Sandbox or Substrate ACP cleanup
implementation. Its unconditional startup gate names old-shaped RuntimePools and
every workspace bearing the legacy controller label, including Ready, Suspended,
Failed, and Deleted resources. Persisting a pool after schema pruning does not
bypass the gate. The old owner must remove these objects before cutover; copying
labels or native identifiers never authorizes adoption.

Provider configuration and profiles belong to separate deployments. Install them
and their read-only parameter grants, then enable the generic workspace API and
ACP dispatch with class-use and Task provenance admission. There is no core
Sandbox or native ACP provider flag. Core independently observes exact runtime
Pods and storage and verifies authenticated supervisor admission; a provider's
Ready observation alone is insufficient.

Pooled Substrate MCP Tools remain an explicit in-tree boundary. Their only enable
flag is `--substrate-mcp-tools-enabled`, with native control TLS/authentication
settings. It does not enable ACP allocations. Helm's
`controller.executionWorkspace.workerNamespaces` grants read-only Pod access in
external worker namespaces; compute and worker confinement belong to providers.
