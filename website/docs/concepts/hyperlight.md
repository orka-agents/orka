---
slug: /hyperlight
description: "Running model-written code in Hyperlight micro-VMs: code_exec for AI Tasks and sandbox_exec for agent Tasks."
---

# Hyperlight micro-VMs

Orka can run the code a model writes in a [Hyperlight](https://github.com/hyperlight-dev/hyperlight)
micro-VM instead of a container. Each run gets a fresh guest with its own
kernel ([hyperlight-unikraft](https://github.com/hyperlight-dev/hyperlight-unikraft)),
restored in milliseconds from a warm snapshot, and the guest reaches no network
and no host file unless Orka grants it. It is disabled by default and fails
closed: a Pod that cannot start a micro-VM returns an error, never a weaker
sandbox.

Two tools use it:

| Tool | Task type | Enabled by | The guest gets |
|------|-----------|------------|----------------|
| `code_exec` | `ai` | `--ai-worker-code-exec-backend=hyperlight` | The code. No network, no host files. |
| `sandbox_exec` | `agent` (built-in ACP runtimes) | `--acp-sandbox-exec` and the tool in the Agent's or Task's allowed tools | The session workspace, read-write at `/workspace`. No network. |

## Requirements

- **Nodes with a hypervisor device**: `/dev/kvm` (KVM, which on a cloud VM needs
  nested virtualization) or `/dev/mshv`.
- **The Hyperlight device plugin**: [hyperlight-on-kubernetes](https://github.com/hyperlight-dev/hyperlight-on-kubernetes)
  advertises `hyperlight.dev/hypervisor` and injects the device with CDI, so
  Pods need no privileges. CDI is on by default in containerd 2.x; containerd
  1.7 needs `enable_cdi = true`. The plugin gives the device to `DEVICE_UID` and
  `DEVICE_GID`; pass that group to the controller with `--hyperlight-device-gid`.
- **The Hyperlight bundle**: `make docker-build-hyperlight-bundle` builds `hluk`
  and the `python`, `node` and `bash` guest images into one image. Pass it with
  `--hyperlight-bundle-image`; an init container copies it into each Pod that
  runs micro-VMs, along with a writable directory for the warm snapshots.

## `code_exec` for AI Tasks

```bash
--ai-worker-code-exec-backend=hyperlight
--hyperlight-bundle-image=<registry>/hyperlight-bundle@sha256:<digest>
--hyperlight-device-gid=<the plugin's DEVICE_GID>
```

Every AI worker Job then requests one `hyperlight.dev/hypervisor`, and its
`code_exec` runs `python`, `javascript` and `bash` in micro-VMs. The controller
pins the backend: `ORKA_CODE_EXEC_BACKEND` and `ORKA_HYPERLIGHT_*` values a Task
or Agent sets, provider- and tenant-scoped ones included, are dropped. Timeouts
and output limits are those of `code_exec` (at most 60 seconds, 64 KiB per
stream). The `code_exec` that the OpenAI- and Anthropic-compatible endpoints run
inside the API server keeps its own backend.

## `sandbox_exec` for agent Tasks

With `--acp-sandbox-exec`, standard RuntimePool Pods get the device and the
bundle, and an agent that allows `sandbox_exec` can run a script in a micro-VM
with its workspace mounted. Turn the provider's own shell off, and that is the
only way it runs a command:

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Agent
metadata:
  name: sandboxed-claude
spec:
  runtime:
    type: claude
    contractVersion: orka.harness.v2
    defaultAllowBash: false
    defaultAllowedTools: [Read, Edit, Write, Glob, Grep, sandbox_exec]
  model:
    name: claude-sonnet-4.6
```

- The supervisor in the runtime Pod serves the tool itself, under the prompt's
  MCP grant; it never reaches the controller broker. Settling or cancelling the
  prompt kills a running micro-VM.
- `hluk` runs as the session's user, which owns the workspace, and only that
  process gets the device's group: the agent's own processes never open the
  hypervisor.
- Arguments: `code`, `language` (`bash`, `python` or `javascript`; `bash` by
  default), and `timeout` in seconds (120 by default, at most 600). The result
  is JSON with `stdout`, `stderr` and `exit_code`, 64 KiB per stream.
- `sandbox_exec` cannot require an Orka approval, which only the broker grants.
- Codex has no way to turn its own shell off (see
  [Troubleshooting](../operations/troubleshooting.md)), so there `sandbox_exec`
  sits beside the provider's shell rather than replacing it.
- Workspace-backed RuntimePools (agent-sandbox, Substrate) run without a
  hypervisor and do not get `sandbox_exec`.

## What runs in the guest

The guest is a Unikraft unikernel with one vCPU, running ordinary Linux
programs:

- **python**: CPython 3.12 with its standard library. The image has no `ssl`,
  `sqlite3` or `subprocess` support.
- **javascript**: Node.js, without `child_process`.
- **bash**: BusyBox, with pipes, `$(...)` and redirects.

There is no `fork()`, so a program that forks fails, and scheduling is
cooperative, so a busy thread holds the vCPU. Guest memory is fixed per runtime
(`python` 256 MiB, `node` 512 MiB, `bash` 128 MiB; `ORKA_HYPERLIGHT_SCRATCH_MB`
overrides it), and exceeding it ends the run.
