# Orka demos

Recorded terminal sessions, each built around one scenario a viewer can follow
without knowing Orka first. Every demo opens with the situation, shows Orka
doing one legible thing, and ends on a payoff you can verify outside the
terminal: a pull request, an object that survived deletion, a refusal.

| | Demo | Shows |
|---|---|---|
| 1 | [`01-chat-to-pr`](01-chat-to-pr) | Maya asks for a health check in Claude Code. Orka runs the agents on her team's cluster and ends with a reviewed, CI-green pull request. No model key ever reaches her laptop. |
| 2 | [`02-agent-sandbox`](02-agent-sandbox) | A workspace that sleeps. Maya's agent works today; the kubernetes-sigs Agent Sandbox is suspended (no Pod, only a disk) and wakes tomorrow with the same identity and her files still on it. |
| 3 | [`03-agent-substrate`](03-agent-substrate) | A save point for an agent. Priya's audit runs as a gVisor Actor on Agent Substrate, sleeps with every worker free, and comes back from a checkpoint after the workspace is deleted. |
| 4 | [`04-security-scan`](04-security-scan) | Findings that arrive as pull requests. Priya registers an old app; Orka writes a threat model, checks its most severe findings, and opens the fix she picks as a pull request. |
| 5 | [`05-two-teams`](05-two-teams) | Alice and Bob use one AI URL. Their tokens pick their teams' separate Orka installations, and the wrong door stays shut at the router and at the installation. Uses the compatibility router from PR #604. |
| 8 | [`08-agent-to-agent`](08-agent-to-agent) | Sam's order-desk app asks the inventory team's agent for advice over A2A, retries without duplicate work, and continues the conversation in one Session. |
| 9 | [`09-governed-tools`](09-governed-tools) | Jordan's assistant checks a supplier's stock, then tries to buy. Gateway logs and the supplier's own receipts show which request got through. |
| 10 | [`10-reviewed-memory`](10-reviewed-memory) | One agent proposes a warehouse note. Jordan reviews and publishes it, and a fresh agent answers from it with the trail on record. |
| 11 | [`11-fibey-approval`](11-fibey-approval) | Fibey investigates a pump alert and proposes an inspection. Lee, the shift lead, approves it, and the work-order receipt returns to the same waiting Task. |
| 12 | [`12-efficiency`](12-efficiency) | Dana's platform team runs twenty customer replies and one fix twice: on a hosted model, then with Vekil choosing between hosted and local CPU models. Compare checked outcomes, elapsed time, and estimated cost. |

Projects 6 and 7 are hackathon videos with their own instructions and are
excluded from the narrated standalone series. See [`narrated/`](narrated/) for
the scenario-led voiceover and DaVinci Resolve workflow for demos 01–05 and
08–12. The standalone recorder discovers only directories containing an
executable `demo.sh`.

The scripts are plain bash. `demo/lib/demo.sh` types commands the way a
person would, and every command the viewer sees is the command that ran.
Narration is in the script next to the command it explains, so re-recording
after a change keeps the two in sync.

The demos share one company. Maya (developer) and Jordan (warehouse) are on
the inventory team, Priya is on security, Alice is on payments, Sam runs the
order desk, Lee is a shift lead, and Dana is on the platform team. Every demo
follows the same shape: a two-line situation, chapters named for what happens,
one green check per chapter stating what was just proven, an evidence table
built from the objects the demo queried, and the install command.

## Rules the scripts follow

These keep the recordings readable for someone who has never seen Orka.

- Five nouns on screen: Provider, Agent, Task, Session, Publisher. Host objects
  (Sandbox, Actor) are shown but not taught. Tool appears only in 09 and 11.
- Orka objects are shown with the `orka` CLI, not with helpers: `task list
  --watch` follows the work, `task status` answers "did it finish and where
  did the change go", `task events --type ... --tail 1` shows what an agent
  said last, `task approvals` shows what is waiting for a person, and every
  `get` prints a short field list, and `task approvals --watch` waits for the
  moment a Task asks a person. Build the CLI from a checkout that includes
  [#672](https://github.com/orka-agents/orka/pull/672) and
  [#677](https://github.com/orka-agents/orka/pull/677) or newer.
- The typed command is shorter than its output. What is left for helpers is
  outside Orka: GitHub, the supplier, the host. Anything that needs `jq`,
  `sed`, `awk`, or `column` is a shell function with a plain name, defined at
  the top of the script and announced once with `helpers_note`. `head` may cap
  long text on camera.
- Objects are shown as a few fields, never a full status dump. `orka task
  status` prints Task, Phase, Delivery, and Publication branch.
- Model answers are capped by the prompts, so `orka task result` is shown
  whole; `head` or `tail` caps the few that may run long.
- Prompts contain no apologies for the runtime. They say what to do, not what
  the sandbox lacks.
- Narration is the bright text; commands are grey.

## Before recording

- The demo client identity is `ORKA_CLIENT_SA` (default `orka-client`). To
  show a person's name in reviewer and approver fields (demos 10 and 11),
  create a ServiceAccount with that name and the same RoleBinding as
  `orka-client`, then set `ORKA_CLIENT_SA` before recording.
- Demo 12 refuses to start the routed batch until Vekil reports the Jev
  classifier preflight ready, so a recording never shows hosted fallback by
  accident. Make sure the classifier is reachable before recording.
- Demos 02 and 03 still get pull request and branch names from the Publisher's
  defaults; a Task-level title arrives with
  [#632](https://github.com/orka-agents/orka/issues/632).

## Prepare the new walkthroughs

Demos 08 through 11 have been recorded against the prepared AKS installation.
Each opens with a business situation and explains new terms beside the action
that uses them. The terminal captures compress quiet waits; the narrated
Resolve edits add scenario openings, explanations, and closing links.

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

Demo 12 has its own `sertac-aks` preparation and saves complete run evidence in
`bin/efficiency-production/`. Follow its README for the two team installations,
Vekil and Jev routing, and AIKit's Qwen3.5 2B CPU model. Its script keeps the
original provider configuration and Copilot credential intact.

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
original five demos. Demos 08 through 12 keep their records and check only their
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
