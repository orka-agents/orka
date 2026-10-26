# Guarded native AI and ACP examples

This directory contains two workloads: native AI, shared by standalone Orka and
Kaimahi, and standalone Orka's read-only ACP review. Both require filesystem and
network enforcement together on the selected physical container.
`native-policy.json` is shared by the two native journeys; `acp-policy.json`
describes the different ACP profile. `requirement.json` is an intentionally
incomplete envelope until the caller materializes the selected complete policy.
Neither installation nor an Agent declaration supplies a Task's policy.
The ordinary unprotected samples are unchanged.

## Prerequisites

Use a compatible controller and `/worker-enforced` image, with the shared
Guardd addon installed and enrolled through the supported chart. Keep the
management, watched Task and ACP runtime namespaces separate. See
[installation and coverage](../../../docs/development/container-enforcement.md).

Configure a Ready native AI Provider and its normal result access first.
`native-agent.json` references a namespace-local Provider named `local`; change
that name to your existing Provider if necessary. This example does not create
an endpoint, select a paid model, download weights or put credentials in a Task.
The Provider's configured default model is used.

## Materialize one complete policy

Copy `native-policy.json` for either native journey, or `acp-policy.json` for
ACP, to `policy.materialized.json`. Replace every symbolic address with the
reviewed literal IPv4 address from your installation and verify every port.
The model port `8000` is illustrative, not an automatically discovered endpoint.
Use the actual Service and EndpointSlice destinations, including every selected
backend: service/NAT traffic needs both the original socket destination and the
packet destination. Remove unused tuples deliberately; add actual required
destinations before submission. No component adds grants afterward.

| Profile | Explicit runtime destinations |
| --- | --- |
| Native AI | Controller result/control API, configured model endpoint, DNS resolver UDP/TCP, Kubernetes API Service and endpoint. |
| Managed ACP | Controller callback/artifact API, controller and node health-probe reply destinations, runtime-local loopback proxies, provider-auth proxy Service/backend, DNS resolver UDP/TCP. |

The ACP controller and node health source `"*"` TCP port rules permit replies to
their ephemeral client ports. Determine the actual health source from node/CNI
routing and packet observations; it is not necessarily the node's advertised IP.
Without that explicit reply ceiling, kernel enforcement can prevent kubelet
health checks from completing and kubelet may restart the runtime.
Each rule also permits outbound TCP to **all** ports on its exact IP: this is a
transport ceiling, not a direction, URL or hostname restriction. If that ceiling
is incompatible with your policy, refuse this profile rather than silently
broadening the policy. Likewise an allowed proxy IP does not independently
restrict its upstream hostnames. Optional telemetry, external tools, MCP and
other destinations are not silently exempted. The example does not enable them.
An IP change requires a new complete policy and binding, not a map edit.

The ACP runtime's provider and MCP proxy listeners bind explicit IPv4 loopback
addresses with ephemeral ports. Its `127.0.0.1` TCP `"*"` rule permits both the
child's proxy connections and replies to ephemeral local client ports. It
authorizes every TCP service on that loopback address, not just those proxy
routes; ordinary runtime authentication still applies. This is an explicit
profile requirement, never an automatic loopback exemption. Enabling the local
MCP proxy listener does not authorize an external MCP destination.

The supervisor's control and loopback proxy listeners explicitly use ordinary
TCP, not the Go/platform default of Multipath TCP on supported Linux hosts.
MPTCP is a distinct socket protocol and is outside this provider's qualified
TCP/UDP ceiling. Enabling it implicitly would not establish protection of its
additional kernel-created subflows. This transport choice adds no policy grants
and does not change the control listener's dual-stack address binding.

The native filesystem rule denies mutation of the existing `/workspace` output
tree, which the no-repository AI profile supplies. It does not assume a worker
home mount exists. The ACP rule denies mutation of the existing `/home/worker`
tree; per-session writable state lives separately under `/sessions`. That rule
alone does **not** make an ACP checkout immutable. The read-only workspace
profile's own protection is separate and must not be attributed to Guardd.
Both policies leave other filesystem paths, including `/tmp`, writable.

Guardd captures existing inode identities before binding. Descendant symlinks
are protected objects; their external destinations are not implicitly protected.
The rule applies to the governed container cgroup and its descendants, not to
every privileged writer on the node. Do not infer checkout immutability from a
denied write to `/home/worker`, or inject new fixtures into an already bound tree
and count them as originally protected content.

Encode the reviewed complete document before submission, using only the Python
standard library. This authoring step canonicalizes JSON; the resulting policy
bytes then travel unchanged through either client and the runtime:

```sh
python3 - <<'PY'
import base64
import json
from pathlib import Path

policy = json.loads(Path("policy.materialized.json").read_text())
raw = json.dumps(policy, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode()
requirement = json.loads(Path("requirement.json").read_text())
requirement["policy"] = base64.b64encode(raw).decode()
Path("requirement.materialized.json").write_text(json.dumps(requirement, indent=2) + "\n")
PY
```

Review the decoded document and envelope together. Both ordered capability
lists must remain identical. Unreplaced addresses, missing paths, incompatible
policies and incomplete envelopes refuse; they never select an unprotected
fallback. Unlisted IP egress is denied. IPv6, raw/packet sockets and unsupported
transports are denied, not granted implicitly. This does not protect other Pods
or separately launched `code_exec` Jobs.

## Standalone Orka

Run from this directory, after reviewing the complete requirement and configuring
the Agent's Provider reference. Set `TASK_NAMESPACE` to the installation's watched
Task namespace, not its management namespace. `jq` only copies the complete
envelope; it does not interpret or rewrite the decoded policy bytes.

```sh
kubectl -n "$TASK_NAMESPACE" create -f native-agent.json
kubectl -n "$TASK_NAMESPACE" wait --for=condition=Ready agent/enforced-hello --timeout=60s
jq --slurpfile requirement requirement.materialized.json \
  '.spec.enforcement = $requirement[0]' native-task.json > native-task.materialized.json
kubectl -n "$TASK_NAMESPACE" create -f native-task.materialized.json
kubectl -n "$TASK_NAMESPACE" get task enforced-hello -o yaml
```

Do not submit `native-task.json` directly: its deliberately empty policy fails
admission instead of silently starting an unprotected Task. Materialize it
first. `create` does not adopt an existing Agent or resubmit an existing Task.
For another invocation, choose a new Task name before submission; do not alter
an admitted Task's requirement.

Read the answer through the normal authenticated Task result API. A completed
answer of `4` establishes model execution, not filesystem or network protection.
`EnforcementReady=True` plus the matching current-generation
`status.enforcementAdmission` records historical admission for the exact
requirement/container binding. Pending, refused or uncertain admission is not
permission to run. Cleanup and retirement remain Orka/provider responsibilities.

Independent release qualification checks both an allowed `/tmp`
write/read/unlink, EACCES on the otherwise-writable protected directory, and
actual allowed/denied TCP/UDP traffic inside that exact container cgroup.
No syscall probe is installed or run
automatically for adopters.

The network profile also denies foreign Unix-domain brokers. An ordinary CRI
`exec` bootstrap can therefore fail after activation while synchronizing with
its host-side runtime. Do not add a broker exemption just to run a probe.
Qualification starts probes before activation or uses a privileged observer
that enters the independently verified container namespaces and cgroup
directly, drops to the workload's credentials, and checks that physical cgroup.
Such host access is trusted observation, not workload authority or an escape
from the stated privileged-host threat-model exclusion.

## Kaimahi

Use Kaimahi's normal native authoring, evaluation and digest Deploy workflow
to obtain the live Agent; do not use the standalone `kubectl` Agent-creation
command as a substitute for that workflow. Opt into the same bundled addon
through `kmx orka install --enforcement` on the supported candidate. Then run:

```sh
kmx run --agent hello-world-agent --namespace "$TASK_NAMESPACE" \
  --prompt "What is 2+2? Answer with the single number." \
  --enforcement-file requirement.materialized.json
```

Use your deployed Agent's actual name and selected Kubernetes context.
Use the same materialized native requirement for both native journeys.
Kaimahi forwards these same requirement bytes on its new native Task; there is
no Agent policy inheritance or second Guardd installer. The answer remains on
stdout, while safe enforcement observations go to stderr. `task result` reads
the existing answer without another inference request. See Kaimahi's installation
and native Task documentation for candidate archive/image selection and access.

## Standalone Orka ACP review

Configure the native OpenCode runtime image and the controller's approved model
proxy route first. Replace `openai/your-configured-model` in `acp-agent.json`
with that route's actual model ID. Review the context/output limits against the
selected model's contract; an arbitrary small model is not qualified merely
because it answers arithmetic. Do not pass model credentials in these resources.

The historical filesystem-only review used Qwen3-8B revision
`b968826d9c46dd6066d109eabc6255188de91218`, native 32768 context, thinking mode
with vLLM 0.10.2's `qwen3` reasoning parser, and 4096 output tokens. It reused
verified existing weights and a configured OpenAI-compatible endpoint.
The example neither installs that server nor downloads a model automatically.
Earlier nonthinking/smaller-model answers and a tool-loop failure were not
accepted; their native filesystem successes were not semantic review passes.
The current combined-policy two-file review is not semantically qualified on
that model: completed answers omitted the Go-call evidence or confused the
supervisor with upstream OpenCode. Thinking mode and a sufficient context
window are not a guarantee of a correct review.

A later combined-policy lab run used official
`Qwen/Qwen3-Coder-30B-A3B-Instruct` revision
`b2cff646eb4bb1d68355c01b18ae02e7cf42d120`, native BF16 and chat template,
with vLLM 0.10.2's `qwen3_coder` tool parser, automatic tool choice, no reasoning
parser, and the same 32768/4096 limits. The configured route name is not proof
of the underlying checkpoint; qualification checked the server's model inputs
and loaded dtype independently. Its actual four answers were verified against
the pinned sources and satisfied the useful two-file provenance review under
the complete combined policy. Independent observations checked network and
filesystem behavior in the same admitted physical container.

That answer did not include all requested command excerpts/build flags, and
the recorded tool events do not establish the additional post-read grep pass
requested by the Task. The original useful-review acceptance and this failed
experimental instruction-following criterion are separate. Do not claim
complete prompt compliance, a broad code-quality audit, or universal model
support. Earlier wrong answers remain failed; model execution success alone
does not qualify another model or installation.

After materializing the approved model configuration:

```sh
kubectl -n "$TASK_NAMESPACE" create -f acp-agent.json
kubectl -n "$TASK_NAMESPACE" wait --for=condition=Ready agent/enforced-review --timeout=60s
jq --slurpfile requirement requirement.materialized.json \
  '.spec.enforcement = $requirement[0]' acp-task.json > acp-task.materialized.json
kubectl -n "$TASK_NAMESPACE" create -f acp-task.materialized.json
kubectl -n "$TASK_NAMESPACE" get task enforced-review -o yaml
```

Materialize `acp-policy.json`, not the native profile's policy, before this
submission. As with the native template, the empty policy in `acp-task.json`
deliberately refuses until materialized. The workspace is the public Orka checkout pinned
in the Task; publishing is disabled, bash is disabled, and the configured
native tools are read/glob/grep. The provider boundary still covers the entire
runtime container, not just an adapter process or a logical session.
Read the answer through the normal authenticated Orka Task result API.

The review asks four short source-supported answers, without a rigid output
schema, and requests a separate grep verification pass after the initial reads.
That instruction is model guidance, not an enforced runtime/tool-call gate.
Code excerpts may span physical lines; a normalized continued command
must be labelled as explanation rather than an exact quotation. Negative
compilation conclusions need positive artifact-flow evidence. Successful tool
calls or a correctly formatted answer are not semantic acceptance:
independently check the two executable paths, the startup function, and the
complete build, download, extraction, installation and final-image copy chains.

The earlier filesystem-only accepted review correctly identified Orka's
`/usr/local/bin/orka-acp-runtime`, its Go `main` source, and the Dockerfile's
entrypoint; it identified upstream `/opt/opencode/bin/opencode` and explained
the downloaded native-package extraction rather than claiming a local OpenCode
source build. Its cited locations were checked against the exact pinned files.
Kernel-map, physical-container and actual allowed/denied filesystem syscall
observations were independent of that answer. This historical result is not
acceptance of the final combined policy. An Agent's Ready condition, a native AI answer
or an arithmetic ACP check does not substitute for either review or enforcement
qualification.

Compatible concurrent sessions share the one physical container binding under
Orka's ordinary capacity. Finishing or cancelling one consumer does not retire
that binding. A different complete policy or an unenforced Task cannot borrow
the container. Provider communication uncertainty holds new work; it does not
expire existing controls. Combined-policy end-user acceptance must qualify the
exact final materialized bytes and both control classes; historical
filesystem-only results are not substitutes. Do not extend the boundary to
other Pods or claim continuous assurance against privileged host manipulation.
