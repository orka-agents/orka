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
| `sandbox_exec` | `agent` (built-in ACP runtimes) | `--acp-sandbox-exec` and the tool in the Agent's or Task's allowed tools | The session workspace at `/workspace`, read-only unless the session has write intent. No network. |

## Requirements

- **Nodes with a hypervisor device**: `/dev/kvm` (KVM, which on a cloud VM needs
  nested virtualization) or `/dev/mshv`. The bundle drives KVM by default; for
  nodes with only `/dev/mshv`, build it with
  `make docker-build-hyperlight-bundle HYPERLIGHT_BUNDLE_FEATURES=mshv`.
- **The Hyperlight device plugin**: [hyperlight-on-kubernetes](https://github.com/hyperlight-dev/hyperlight-on-kubernetes)
  advertises `hyperlight.dev/hypervisor` and injects the device with CDI, so
  Pods need no privileges. CDI is on by default in containerd 2.x; containerd
  1.7 needs `enable_cdi = true`. Orka does not install it; see
  [Installing the device plugin](#installing-the-device-plugin).
- **The Hyperlight bundle**: this opt-in image is operator-built and is not
  published by Orka release or development-image automation. Build it with
  `make docker-build-hyperlight-bundle HYPERLIGHT_BUNDLE_IMG=<your-registry>/hyperlight-bundle:<tag>`,
  then publish it with `docker push <your-registry>/hyperlight-bundle:<tag>`.
  It contains `hluk` and the `python`, `node` and `bash` guest images. Pass it with
  `--hyperlight-bundle-image`; an init container copies it into each Pod that
  runs micro-VMs, where it is mounted read-only, next to a separate volume for
  the warm snapshots. `hluk` uses that cache only while no other user can write
  to it; otherwise every run boots cold.

### Installing the device plugin

The device plugin is node infrastructure, like a GPU device plugin, and the
cluster operator installs it. Upstream has no versioned releases; its images
are tagged by commit. Orka's Hyperlight E2E lane tests commit `fc71b45`:

```bash
REV=fc71b4501d23977fcc54f7be144d884fc8210667
kubectl label node <node> hyperlight.dev/enabled=true
curl -fsSL "https://raw.githubusercontent.com/hyperlight-dev/hyperlight-on-kubernetes/$REV/deploy/manifests/device-plugin.yaml" |
  IMAGE=ghcr.io/hyperlight-dev/hyperlight-device-plugin@sha256:8e8b788c2327e272317c456c85c8296922ccb8b9b70e5b866451bec11ed02472 \
  DEVICE_COUNT=16 DEVICE_UID=0 DEVICE_GID=<gid> \
  envsubst '${IMAGE} ${DEVICE_COUNT} ${DEVICE_UID} ${DEVICE_GID}' | kubectl apply -f -
kubectl -n hyperlight-system rollout status daemonset/hyperlight-device-plugin
```

- The DaemonSet runs only on nodes labeled `hyperlight.dev/enabled=true`. Label
  the nodes that have the hypervisor device.
- `DEVICE_GID` is the group that owns the device in Pods. Use a nonzero group
  and pass the same value to `--hyperlight-device-gid`. With `DEVICE_UID=0`,
  workload users reach the device only through that group, which Orka adds only
  to `hluk`.
- `DEVICE_COUNT` is how many Pods on a node can hold the device at once, not a
  count of physical devices. Each Hyperlight-enabled AI worker Pod and
  RuntimePool Pod holds one while it runs, so set it above the number of those
  Pods a node can run.
- The published image is `linux/amd64` only.

The plugin is ready when each labeled node advertises the resource:

```bash
kubectl get nodes -l hyperlight.dev/enabled=true \
  -o custom-columns='NODE:.metadata.name,HYPERVISOR:.status.allocatable.hyperlight\.dev/hypervisor'
```

On kind, the plugin container fails to start: its read-only host `/dev` mount
leaves the container runtime no place for `/dev/termination-log`. Set the
container's `terminationMessagePath` to a path under `/tmp`.
`scripts/hyperlight-kind-e2e.sh` shows the full kind setup; it builds the image
from the same commit.

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
- In sandbox-enabled runtimes, the supervisor owns the session root. The
  session can traverse it and write its workspace, home, temporary and XDG
  directories, but cannot replace the workspace entry with a symlink.
- `hluk` runs as the session's user and group, which can read the workspace
  (and write it in a write-intent session), and only that process gets the
  device's group: the agent's own processes never open the hypervisor.
- Only a write-intent session's guests can change the workspace. Any other
  session gets it read-only, so a guest's write fails at once with a read-only
  file system error rather than at the workspace check after the turn.
- A runtime Pod runs one `sandbox_exec` guest at a time, including across
  sessions. A call waits for the guest before it starts, at most as long as its
  timeout and never past prompt cancellation; the timeout then bounds its own
  run. A call that never got the guest reports that nothing ran.
- A call can take 21 minutes: up to 600 s waiting for the guest, then up to
  600 s running. In a session that can call `sandbox_exec`, Orka makes Claude,
  Codex and OpenCode wait that long for an MCP tool call; their own limits are
  60 s (Claude and OpenCode) and 300 s (Codex). Meanwhile it sends a client that
  accepts an event stream a comment every 30 s, since OpenCode drops a response
  that is silent for 300 s.
- Arguments: `code`, `language` (`python`, `javascript` or `bash`; `python` by
  default), and `timeout` in seconds (120 by default, at most 600). The result
  is JSON with `stdout`, `stderr` and `exit_code`, 64 KiB per stream.
- `sandbox_exec` cannot require an Orka approval, which only the broker grants.
- With native Bash allowed, `sandbox_exec` is supplemental; enabling Hyperlight
  never silently removes native commands. With `defaultAllowBash: false` and
  `sandbox_exec` granted, Claude and OpenCode enforce the command-tool exclusion.
  Claude also disables filesystem command hooks and ignores unmanaged MCP
  configuration while retaining `CLAUDE.md` and trusted adapter callbacks.
- Codex supports a narrow command-isolated policy: grant its full non-Bash native
  surface (`Read`, `Write`, `Edit`, `Glob`, `Grep`, `WebSearch`, `WebFetch`) plus
  `sandbox_exec`, and set `defaultAllowBash: false`. Existing read-intent policy
  remains supported. Other arbitrary Codex native-tool restrictions still fail
  closed. The supervisor pins native command execution, filesystem hooks,
  plugins, subagents and host code mode off at startup and per thread; project
  configuration is untrusted and unmanaged user MCP servers are disabled.
  Current-workspace `AGENTS.override.md`/`AGENTS.md` instructions are bounded to
  32 KiB and snapshotted into the private Codex home on creation/resume. The
  snapshot rejects symlink files and remains visible without loading
  command-bearing project configuration.
- The pinned Copilot CLI cannot enforce this exclusive mode with verified
  immutable hook and unmanaged-MCP controls. Orka rejects `sandbox_exec` plus native-Bash denial
  before dispatch. Ordinary Copilot policies and supplemental sandbox execution
  with native Bash allowed are unchanged.
- Workspace-backed RuntimePools (agent-sandbox, Substrate) run without a
  hypervisor and do not get `sandbox_exec`.
- External `AgentRuntime` registrations cannot grant `sandbox_exec`; Orka
  rejects the MCP policy at registration and dispatch.
- A runtime without sandbox execution enabled rejects session creation if
  its frozen tool policy exposes `sandbox_exec`, before starting the provider.
  Remove the tool from the allowlist when using a workspace-backed pool or
  when `--acp-sandbox-exec` is disabled.

## What runs in the guest

The guest is a Unikraft unikernel with one vCPU, running ordinary Linux
programs:

- **python**: CPython 3.12 with its standard library. The image has no `ssl`,
  `sqlite3` or `subprocess` support.
- **javascript**: Node.js, without `child_process`.
- **bash**: the BusyBox `hush` shell, not GNU bash, which needs `fork()`. It
  has pipes, `$(...)`, redirects and background jobs, but its loops are slow.

There is no `fork()`, so a program that forks fails, and scheduling is
cooperative, so a busy thread holds the vCPU. Every file operation in a guest
goes through the host, so reading many files is much slower than on the host:
a scan of a few thousand files takes seconds, ten or more times its native time.
Use `sandbox_exec` to compute, and native read tools to search a repository. Guest memory is fixed per runtime
(`python` 256 MiB, `node` 512 MiB, `bash` 128 MiB), and exceeding it ends the run.
Controller-created Pods pin these sizes. `ORKA_HYPERLIGHT_SCRATCH_MB` only
overrides them when running the runner directly outside those Pods. A run that
prints more than 64 MiB is stopped.

Hyperlight-enabled Pods add 512 MiB for one guest to the worker's memory request
and any nonzero memory limit. A limit-only worker keeps Kubernetes' matching
request default before the addition. An absent limit stays unbounded; an
explicit zero limit is removed so it cannot conflict with the positive request.
The default AI Job therefore requests 1 GiB and has a 2.5 GiB limit; a standard
ACP RuntimePool requests 1 GiB and has a 4.5 GiB limit. Task and Agent resource
settings describe the worker budget before this addition. Size that budget for
the provider, host-side output buffers and workload; the guest reservation is
not a guarantee against every OOM. Non-Hyperlight Pods are unchanged.

## Continuous integration

The `Hyperlight E2E` workflow builds the pinned production bundle and runs real
KVM guests without LLM credentials. It tests cold boot and warm snapshots,
`code_exec` in a non-root read-only container, and the ACP `sandbox_exec` MCP
path with distinct session identities. Checks cover workspace persistence,
read-only workspaces, host-file and environment isolation, output limits,
timeouts, cancellation, and Python and JavaScript script compatibility.

The guest-only job uses the `hyperlight_e2e` Go build tag on Linux with the
bundle at `/opt/orka/hyperlight` and access to `/dev/kvm`.

A second job runs the `hyperlight`-labeled Kubernetes E2E specs with aimock.
Only the model is mocked: the controller, AI worker, OpenCode runtime, device
plugin, bundle init container and KVM guests are real. It checks that:

- an AI Task's `code_exec` guest identity, stdout/stderr and network-policy
  evidence reach aimock's request journal;
- an OpenCode Task with native Bash disabled and a read-intent workspace uses
  `sandbox_exec`: the first guest reads the cloned repository and each of its
  writes fails with `EROFS`, and a second guest finds the repository unchanged;
- withholding the bundle returns an unavailable-tool result to the model,
  without changing the pinned backend or executing a host fallback. The model
  acknowledges this error, so Task success alone is not execution evidence.

The specs also inspect the actual worker/runtime Pods for device allocation,
image digests, a successfully completed bundle init container, and separate
read-only bundle and writable cache mounts. Run this lane on a Linux KVM host
with `scripts/hyperlight-kind-e2e.sh` (use the VM for local runs). It creates
and tears down a fresh repo-scoped kindctl cluster and requires aimock with no
live credentials. Both jobs fail if KVM is missing; unavailable virtualization
never becomes a passing skip in the dedicated lane.

### Harness command-isolation verification

The KVM/aimock deployment lane checks AI `code_exec`, OpenCode and Claude
`sandbox_exec`, unavailable-backend rejection without host fallback, and
fail-closed admission of unsupported Copilot exclusive mode. It verifies actual
model-facing command catalogs and guest results, not canned final replies.
The Claude spec's guest tries to plant workspace hook and MCP configuration
during a read-intent prompt, and the read-only mount must refuse each file.

`scripts/hyperlight-codex-e2e.sh` separately exercises the pinned Codex/ACP
runtime against a private Responses namespace fixture and real Hyperlight guests.
It checks native-command rejection, unmanaged project/user MCP exclusion,
`AGENTS.md` visibility, fresh-guest isolation, workspace persistence, and delivery
of real guest results back to the model. Pinned aimock does not emit these
namespace calls, so this is runtime conformance, not a claimed Codex
Orka/Kubernetes deployment E2E. Neither lane needs live provider credentials.
