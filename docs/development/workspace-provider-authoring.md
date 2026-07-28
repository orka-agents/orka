# Workspace provider authoring contract

Provider adapters watch `workspace.orka.ai/v1alpha1` resources whose immutable
`spec.controllerName` matches the adapter. They import only
`api/workspace/v1alpha1`, `pkg/workspaceprovider`, and `pkg/workspaceagent` from
a tagged Orka module version.

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

The local Agent Substrate integration currently applies a reviewed compatibility
patch to the pinned upstream OCI generator because that revision omits these two
capabilities and creates every extracted rootfs with mode `0700`. For the
explicit `/orka-workspace-agent` entrypoint only, the patch grants the set-ID
capabilities and changes the extracted rootfs directory to `0755`; other Actor
containers keep the pinned Substrate capability and rootfs policy. The installer
verifies the exact upstream source blob before applying the patch. Re-pinning
Substrate therefore requires an explicit review of the upstream OCI runtime
contract rather than silently carrying the patch onto changed code.

## Workspace-agent connection Secret contract

A ready workspace that exposes the workspace-agent data plane sets
`WorkspaceObservation.ConnectionSecretRef`. The referenced Secret uses the
versioned public contract in `pkg/workspaceprovider` rather than adapter-specific
keys. Adapters should build its `data` map with `EncodeConnectionData`; core and
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

The shared conformance suite requires a disposable data-plane workspace and
exercises health, capabilities, attachment fencing, idempotent exec, advertised
file operations, revocation, and reset through the public client.
