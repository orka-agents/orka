---
title: Govern kagent agents
description: Run kagent Agents under Orka's dispatch, lifecycle, and audit, or translate them into native Orka Agents.
---

# Govern kagent agents

`orka agent import kagent` brings [kagent](https://github.com/kagent-dev/kagent)
Agents under Orka. It works in one of two ways for each Agent:

- **Wrap.** kagent keeps running the Agent. An Orka Agent fronts it through
  the [kagent runtime adapter](https://github.com/orka-agents/orka-runtime-kagent),
  an external `orka.harness.v2` AgentRuntime. Orka owns how work reaches the
  Agent, its identity and lifecycle, and the audit record.
- **Translate** (`--translate`). The import converts the Agent into a native
  Orka Agent, with no kagent at run time. It maps only the system prompt, the
  model, and MCP tools you map to Orka Tools, and refuses everything else.

Supported kagent builds:

| kagent | API | Status |
| --- | --- | --- |
| v0.10.3, Go runtime | `kagent.dev/v1alpha2` | supported |
| `1eb89fb1` (v1.0.0-alpha13+31) with Substrate fork v0.5.0-alpha2 | `api.kagent.dev/v1alpha3` | experimental until kagent v1.0.0 |

The adapter refuses turns on any other kagent controller build, unless the
operator verified it with the adapter's release gate and listed its commit
(`--accept-kagent-commit`).

## Observed and governed

The import classifies each wrapped Agent from its kagent objects:

| | Observed | Governed |
| --- | --- | --- |
| Dispatch, lifecycle, cleanup | Orka | Orka |
| Tool calls | kagent runs them; Orka sees names and status | Orka runs them through the adapter's relay |
| Model calls | kagent calls its provider | Orka serves them through the relay |
| Orka approvals | no | yes, for the tools you mark |
| Token usage | as kagent reports it | as the relay meters it |
| Output token limit | no | `--max-output-tokens` |

An Agent is governed only when every model and tool endpoint points at the
relay, and the import can prove from kagent's objects that nothing else runs
on the Agent's behalf. Anything it cannot prove keeps the Agent observed, and
the dry run says why. An Agent that uses kagent's own approval prompts is
refused in both modes, because a wrapped Agent cannot answer them.

## What wrapped Agents do not get

- **Repository work.** The adapter sends kagent only the prompt text.
  `Task.spec.workspace`, workspace deltas, and clean-room publication do not
  reach a wrapped Agent. Translate an Agent that changes code.
- **Per-Task configuration.** As for every external runtime, the registered
  profile is authoritative. A change to the model, prompt, or tools is a new
  kagent revision and a [re-pin](#deploy-and-re-pin).
- **Connector-backed Tools.** External runtimes refuse them. Linked GitHub
  built-ins work when the AgentRuntime registers them, with write tools
  approval-required.
- **Orka's model limits.** The supervisor never sees kagent's model calls. A
  governed Agent's relay bounds output with `--max-output-tokens`.

## Install prerequisites

kagent ships without NetworkPolicies, and its default authentication accepts
any caller's user ID. Before wrapping Agents, apply the policies in
[Prepare your kagent install](https://github.com/orka-agents/orka-runtime-kagent/blob/main/docs/kagent-install.md)
for your kagent version. At minimum, admit only kagent's controller to
Substrate's actor router (v1) and to kagent's database, and, for v0, admit
only the controller and the adapter to Agent pods. List those components with
`--unreachable`: the adapter refuses turns while it can connect to one.

## Deploy and re-pin

You need the `orka` CLI and access to the cluster; nothing is built.
`activate` reaches the Orka API like other `orka` commands: through the
server you configure or pass with `--server`, or else by port-forwarding
the API Service with `kubectl`. Each
runtime runs Orka's kagent runtime image, by default the one released with
your CLI (`ghcr.io/orka-agents/orka/acp-kagent-runtime:<version>`), and
mounts its Agent's `kagent.json` from a ConfigMap. The adapter refuses a
configuration whose digest the runtime's profile does not pin. A development
build of the CLI has no released image, so pass `--image`; each `main` commit
publishes `ghcr.io/orka-agents/orka/acp-kagent-runtime:sha-<full-commit>`.

The adapter pins the exact Agent it wraps: kagent v1's compiled revision, or
kagent v0's config hash and image digest. Changing kagent's Helm
`runtimeDefaults`, editing the Agent or its ModelConfig, or upgrading kagent
changes the pin, and the adapter then refuses turns until the Agent is
re-pinned. Orka cannot change a registered AgentRuntime's profile in place, so
each pin gets its own runtime, named `<agent>-kagent-<digest>`:

1. Export the Agents and their related objects, and write a bundle:

   ```bash
   orka agent import kagent -f export.yaml --dry-run --bundle-dir bundle \
     --control-plane-url http://kagent-controller.kagent.svc:8083 \
     --relay-url 'http://{name}-kagent-relay.orka-system.svc:8092' \
     --model-upstream-url <model base URL> --model-upstream-model <model> \
     --unreachable kagent-postgresql.kagent.svc:5432
   ```

   The dry run reports each Agent as observed or governed, and why. A
   governed Agent's tool policy lists the tools its bindings name, plus any
   you add with `--allowed-tool`. If a binding names none, kagent calls
   whatever the relay lists, so name the Orka tools it may call with
   `--allowed-tool`. Mark the ones that need an Orka approval with
   `--approval-required-tool`.

2. Deploy and register the runtime of every Agent in the export:

   ```bash
   orka agent-runtime kagent deploy bundle/<agent> --context <context> -n orka-system
   ```

   It waits until Orka reports the AgentRuntime Ready. A governed Agent also
   needs `--model-upstream-token-file`, Orka's credential for the model
   upstream. A runtime keeps the credential and the image it was deployed
   with: to change either, re-pin under a new `--runtime-revision`. Use
   `--image` for another runtime image, and `--controller-namespace` when the
   Orka controller runs in another namespace than the runtimes.

   Orka's conformance check sends a prompt that calls the relay, so deploy
   moves a governed Agent's relay to the new runtime first, and back if it
   fails before the runtime is Ready. Until you activate the new runtime,
   governed turns on the old one fail their tool and model calls. A governed
   Agent's relay Service and NetworkPolicy may serve only that Agent and its
   kagent Agent, so deploy refuses ones another Agent's deploy created, and
   ones it did not create. To let the Agent use such objects, label them
   `orka.ai/kagent-agent=<agent>` and annotate them
   `orka.ai/kagent-source='<kagent apiVersion> <kagent namespace>/<kagent name>'`,
   such as `'kagent.dev/v1alpha2 kagent/probe'`, the value the Orka Agent
   carries.

3. Point the Orka Agent at the new runtime, with the import flags that wrote
   the bundle, minus `--dry-run`, after `--`:

   ```bash
   orka agent-runtime kagent activate bundle/<agent> --context <context> -n orka-system -- \
     -f export.yaml <the other import flags>
   ```

   The command passes `--bundle-dir` itself, and `-n`: the import creates the
   Orka Agents in the runtimes' namespace. It reaches the Orka API like the
   import does, through kubeconfig's current context, so when `--context`
   names another one, also pass that cluster's Orka API with `--server` and
   `--token`. Pass the Orka API and token flags before `--`, where the
   command checks them. The import switches every Agent
   in the export, and refuses unless all of their runtimes are Ready, and
   unless these flags pin exactly the bundles you deployed. `deploy` records
   each runtime's AgentRuntime UID in its bundle (`registration.uid`), and the
   import refuses a runtime with another UID, such as one in another cluster.
   Activate also refuses while the relay of a governed Agent in the bundle
   directory selects another runtime; deploying that bundle again moves it
   back.

4. Retire the Agent's older runtimes. Orka drains each one first, so this
   waits until their sessions are done:

   ```bash
   orka agent-runtime kagent retire bundle/<agent> --context <context> -n orka-system
   ```

   It refuses while a governed Agent's relay selects another runtime. If it
   stops before it is done, run it again: it also finds runtimes whose
   registration is gone but whose objects are left.

Re-pinning is the same four steps. Each command holds a lock per Agent it
changes, the ConfigMap `<agent>-kagent-lock`, so runs for one Agent never
overlap. A run that was killed leaves its lock; delete the ConfigMap once no
run is left.

The supervisor reads Orka's controller epoch once, when it starts, and Orka
marks a runtime with an older epoch not Ready. The registration therefore
opts into `deployment.kubernetesRecovery`: after an Orka controller restart,
Orka drains each runtime and restarts its supervisor with the new epoch, with
no step on your part. See
[Bring your own AgentRuntime](bring-your-own-agent-runtime.md#kubernetes-recovery).

To manage the objects yourself, for example from GitOps,
`orka agent-runtime render-kagent` renders each part of a runtime.
