# Orka demos

Recorded terminal sessions, each built around one scenario a viewer can follow
without knowing Orka first. Every demo opens with the situation, shows Orka
doing one legible thing, and ends on a payoff you can verify outside the
terminal: a pull request, an object that survived deletion, a refusal.

| | Demo | Shows |
|---|---|---|
| 1 | [`01-chat-to-pr`](01-chat-to-pr) | Claude Code pointed at the cluster instead of a vendor. One prompt becomes Agents, Tasks, a review, and a CI-green pull request. No model key leaves the cluster. |
| 2 | [`02-agent-sandbox`](02-agent-sandbox) | A workspace that sleeps. One Session, two requests, one kubernetes-sigs Agent Sandbox: it is suspended between the requests (no Pod, only a disk) and wakes with the first request's work still on it. |
| 3 | [`03-agent-substrate`](03-agent-substrate) | A save point for an agent. An audit runs as a gVisor Actor on Agent Substrate; between requests every worker is free, a follow-up boots a fresh Actor from the kept data, and a checkpoint restores after the workspace is deleted. |
| 4 | [`04-security-scan`](04-security-scan) | Findings that arrive as pull requests. A legacy app is scanned into a threat model and validated findings; a person picks one and Orka opens the fix. |
| 5 | [`05-two-teams`](05-two-teams) | Two teams, two namespaces, two Orka installations, one shared AI URL. The caller's token picks the team; cross-team requests are refused. Uses the compatibility router from PR #604. |
| 8 | [`08-agent-to-agent`](08-agent-to-agent) | An order desk asks the inventory team's agent for help, retries the request, and retrieves the customer reply after the message adapter is replaced. |
| 9 | [`09-governed-tools`](09-governed-tools) | An assistant checks a supplier's stock, then attempts a purchase. Gateway logs and supplier receipts show which request got through. |
| 10 | [`10-reviewed-memory`](10-reviewed-memory) | One assistant proposes a return procedure. A person accepts and applies it, then a fresh agent uses the saved note. |
| 11 | [`11-fibey-approval`](11-fibey-approval) | Fibey investigates a pump alert and proposes an inspection. A person approves the work order, and its receipt returns to the same waiting Task. |

Projects 6 and 7 are edited video projects with their own instructions. The
standalone recorder discovers only directories containing an executable `demo.sh`.

The scripts are plain bash. `demo/lib/demo.sh` types commands the way a
person would, and every command the viewer sees is the command that ran.
Narration is in the script next to the command it explains, so re-recording
after a change keeps the two in sync.

## Prepare the new walkthroughs

Demos 08 through 11 have scripts and setup instructions. They have not been
recorded. Each opens with a business situation and explains new terms
beside the action that uses them. They target about four minutes with waiting
compressed; timing still needs a full rehearsal with the prepared model service.

Start with the existing demo cluster and build its matching CLI with
`make build-cli`. Follow each demo's README for its additional setup, then run
the walkthrough directly to rehearse without recording:

```sh
demo/setup/agent-to-agent.sh
demo/08-agent-to-agent/demo.sh

# agentgateway must be installed; the demo 09 README covers its pinned installer.
demo/setup/governed-tools.sh
demo/09-governed-tools/demo.sh

demo/setup/reviewed-memory.sh
demo/10-reviewed-memory/demo.sh

# Prepare a Fibey runtime with human approval first; see demo 11's README.
demo/setup/fibey-approval.sh
demo/11-fibey-approval/demo.sh
```

Demos 08 through 11 default to a local kind context. For a prepared remote
cluster, put its exact context name and a single scoped kubeconfig in a separate
environment file, then select that file with `ORKA_DEMO_ENV` for setup, rehearsal,
and recording:

```sh
# Contents of your local AKS demo environment file.
export KUBECONFIG=/absolute/path/to/sertac-aks.kubeconfig
export DEMO_KUBE_CONTEXT=sertac-aks
export ORKA_NAMESPACE=orka-demos-v2
```

Set the API and controller settings in that file for the prepared installation.
The scripts reject a context mismatch and never switch contexts. Demo 08 also
requires a pushed, digest-pinned `DEMO_A2A_IMAGE` on a remote cluster; its README
describes that path.

The new demos save complete responses in ignored `demo/setup/state/` run
directories and stop if their checks fail. They use fresh request IDs and keep
prior work records. Demo 10 requires an empty active memory list for a meaningful
comparison; its README explains how to disable only the note from a previous run.
Its reader's event history must show no tool calls. The demo checks that behavior;
the current worker still exposes memory and transcript-search tools.

Demo 09 shows an operation that stays closed. Demo 11 shows an operation that
can proceed after a person's decision. It uses the human-approval work from
[#589](https://github.com/orka-agents/orka/pull/589) and a prepared external
Fibey runtime. Its receipt service creates only simulated work orders.

## Watching

Each chapter is an asciicast v3 marker, so a demo can be stepped through
rather than sat through:

```sh
asciinema play --pause-on-markers demo/casts/01-chat-to-pr.cast
```

* `space` resumes from a marker
* `]` skips to the next chapter
* `.` steps one frame
* `ctrl+c` quits

`demo/casts/` is generated output and is not checked in.

## Re-recording

The original five demos run against one kind cluster that carries Orka, Agent Substrate on
gVisor, kubernetes-sigs Agent Sandbox, and a real model proxy. Build it once:

```sh
SOURCE_KUBECONFIG=~/.kube/kind/<cluster-with-an-authenticated-vekil>.kubeconfig \
  demo/setup/cluster-up.sh
cp demo/setup/env.sh.example demo/setup/env.sh   # then adjust paths
```

`cluster-up.sh` layers the existing installers under `hack/demos/cluster/`;
read its header for the order and why. `vekil-auth.sh` copies an existing
GitHub Copilot login between clusters so no demo needs an interactive login.

Demo 5 needs two more namespace-scoped installations and the router. Build a
controller image from a checkout that includes PR #604 (it ships
`/compat-router`), then:

```sh
CONTROLLER_IMAGE=<registry>/orka/controller@sha256:<digest> demo/setup/two-teams.sh
```

`two-teams.sh` also registers the team controllers with the cluster's shared
admission webhook; without that, their Task status updates are refused.

Then record:

```sh
demo/record.sh                     # every demo
demo/record.sh 02-agent-sandbox    # one

# Optional: animated GIFs next to the casts (needs agg)
demo/render.sh                     # every cast
demo/render.sh 02-agent-sandbox    # one
```

`record.sh` records at 100x28 with idle time capped at two seconds and converts
the chapter sentinels into marker events. It runs `demo/reset.sh` before the
original five demos. Demos 08 through 11 keep their records and check only their
own run. Recording remains a separate, explicit command.

The demos open real pull requests against
[`sozercan/orka-demo-inventory`](https://github.com/sozercan/orka-demo-inventory),
a small Go service that exists for this purpose, and scan the
[`sozercan/nodejs-goof`](https://github.com/sozercan/nodejs-goof) fork.

## Layout

```
demo/
  lib/demo.sh        typed-command helpers, chapters, colours, Orka plumbing
  lib/markers.py     sentinel -> asciicast v3 marker events
  setup/             cluster bootstrap, vekil auth copy, platform resources
  NN-*/demo.sh       the script that gets recorded
  NN-*/manifests/    what that demo applies
  casts/             recording output (gitignored)
```
