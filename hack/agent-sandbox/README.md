# Agent Sandbox Phase 1 Spike

This directory contains a **local/manual spike only** for validating upstream
`agent-sandbox` compatibility before Orka grows a production adapter.

It does not integrate Orka with `agent-sandbox`, does not vendor upstream client
types, and does not change Orka's existing Kubernetes Job or `code_exec` paths.

## Pinned upstream version

This spike is pinned to upstream `agent-sandbox`:

```text
v0.4.6
```

Install the pinned upstream manifests with:

```bash
kubectl apply -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/v0.4.6/manifest.yaml
kubectl apply -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/v0.4.6/extensions.yaml
```

The spike references these upstream APIs and resources:

```text
extensions.agents.x-k8s.io/v1alpha1
sandboxes.agents.x-k8s.io
sandboxclaims.extensions.agents.x-k8s.io
sandboxtemplates.extensions.agents.x-k8s.io
sandboxwarmpools.extensions.agents.x-k8s.io
```

## What the spike does

`spike.sh` creates a disposable namespace and then:

1. Installs the pinned upstream `agent-sandbox` manifests, unless disabled.
2. Creates a minimal `SandboxTemplate`.
3. Creates a `SandboxClaim` with `shutdownPolicy: Delete`.
4. Runs `echo ok` inside the sandbox pod.
5. Writes and reads a file in `/workspace`.
6. Patches `spec.lifecycle.shutdownTime` to the current UTC time and waits for
   Delete behavior where possible.
7. Creates a second `SandboxClaim` with `shutdownPolicy: Retain`.
8. Writes and reads another file.
9. Patches `spec.lifecycle.shutdownTime` to the current UTC time and prints the
   retained claim/status and remaining resources where possible.
10. Prints:

```bash
kubectl get sandbox,sandboxclaim -A
```

Command execution is intentionally done with `kubectl exec` against the sandbox
pod for this spike. This is not the production Orka adapter design.

The Delete-policy path expects expiration to delete the claim and its Sandbox.
The Retain-policy path expects expiration to keep the claim object for inspection
while the underlying Sandbox/Pod resources are removed by upstream
`agent-sandbox`. This is lifecycle compatibility evidence, not a durable Orka
workspace contract.

## Prerequisites

- A disposable Kubernetes cluster.
- `kubectl` configured for that cluster.
- A default storage class if you want to validate the PVC-backed workspace path.
- Optional runtime classes such as gVisor/Kata if you edit the generated
  `SandboxTemplate` to include `runtimeClassName`.

## Usage

Run:

```bash
hack/agent-sandbox/spike.sh
```

Useful environment variables:

| Variable | Default | Description |
| --- | --- | --- |
| `AGENT_SANDBOX_VERSION` | `v0.4.6` | Must remain `v0.4.6`; other values are refused. |
| `NAMESPACE` | `orka-agent-sandbox-spike` | Namespace for spike resources. |
| `TEMPLATE_NAME` | `orka-spike-template` | Name of the `SandboxTemplate`. |
| `DELETE_CLAIM` | `orka-spike-delete` | Name of the Delete-policy `SandboxClaim`. |
| `RETAIN_CLAIM` | `orka-spike-retain` | Name of the Retain-policy `SandboxClaim`. |
| `INSTALL_AGENT_SANDBOX` | `1` | Set to `0` to skip applying upstream manifests. |
| `WAIT_TIMEOUT` | `180s` | Timeout for rollout and pod readiness waits. |
| `CLEANUP` | `0` | Set to `1` to delete the spike namespace at the end. |

Example without reinstalling upstream manifests:

```bash
INSTALL_AGENT_SANDBOX=0 hack/agent-sandbox/spike.sh
```

Example with cleanup:

```bash
CLEANUP=1 hack/agent-sandbox/spike.sh
```

On rerun, the script removes stale claims with the configured `DELETE_CLAIM` and
`RETAIN_CLAIM` names before creating fresh claims. If `CLEANUP=0`, remove spike
resources manually with:

```bash
kubectl delete namespace orka-agent-sandbox-spike
```

## Scope guardrails

This is intentionally limited to Phase 1 compatibility evidence:

- Do not implement the real Orka adapter here.
- Do not import or vendor upstream `agent-sandbox` client types into Orka packages.
- Do not change Orka worker Job creation.
- Do not change Kubernetes `code_exec`; it must remain ephemeral and hardened.
