# Help the customer. Fix the cause.

A customer clicked Pay again after checkout froze and saw two charges. Support
needs a factual acknowledgement and the order reference. Payments engineering
needs to fix overlapping retries. These two teams use saved Orka Agents in one
installation on `sertac-aks`.

The walkthrough starts with the scenario and the instructions that define each
job. It runs both jobs with hosted GPT-5.5, introduces a local model, enables
Vekil's semantic router, and repeats the same work. The recording uses real
`orka`, `kubectl`, Git, and Podman commands. Node runs the payment tests inside
an isolated container.

The support Agent uses the Orka Provider named `semantic-router`. The coding
Agent uses a Codex runtime under `orka.harness.v2`; its authenticated model proxy
also forwards to Vekil. These are separate connection mechanisms. An ACP Agent's
`providerRef` does not select its model proxy.

Jev classifies new work as lightweight or powerful. Vekil
maps that choice to Qwen3.5 2B, served by AIKit on cluster CPUs, or hosted
GPT-5.5. Related tool calls retain the selected model while the agent finishes
the job. The video describes local capacity as hardware you already pay for and
operate. Actual routing records establish the destination of each call.

## Preparation

Requirements are the existing Orka CRDs and admission installation, a matching
`bin/orka`, `kubectl`, Python with PyYAML, a running Podman machine, Git, and `jq`.
Use the existing demo installation prepared by `prepare.py`. For a new
installation, see that command's help and safe manifest rendering. Do not
reapply the original controller template over the persistent runtime setup.

```sh
python3 demo/12-efficiency/runtime_setup.py render-safe
python3 demo/12-efficiency/runtime_setup.py --context sertac-aks apply
python3 demo/12-efficiency/story.py setup
```

Runtime setup changes only the owned `team-payments` installation. It prepares
the coding runtime's authenticated model proxy, repository egress proxy, and
Publisher. Before replacing the controller, it copies a consistent SQLite
backup and existing artifacts onto an owned persistent volume and verifies the
copy. The repository credential stays in Kubernetes Secrets and outside the
agent process. Setup verifies that existing shared services, the original
Copilot credential, and the private provider source are unchanged.

The sample lives on `sozercan/orka-demo-inventory`, branch
`demos/duplicate-checkout`, at the revision pinned in `story.py`. Its source is
also under `sample/payments/`. Six regression tests cover overlapping retries,
completed retries, independent checkouts, and failure recovery. The starting
implementation deliberately fails two tests. Both runs start at that same
revision, and each publishes only `payments/charge.mjs` to a fresh demo branch.
No pull request is created.

Setup pulls the official Node 24 image pinned in `story.py`. Independent checks
mount only the payment source directory read-only. The container runs as an
unprivileged user, with no network, dropped capabilities, a read-only root
filesystem, resource limits, and a 60-second deadline. Generated code never
runs with the recording host's credentials or access to its evidence files.

Provisioning, credentials, and builds stay outside the walkthrough. All images
are pinned. New builds use the `remote-vm` builder and `docker.io/sozercan`.
Never use the private Vekil working directory as a Docker build context.

The coding image adds the policy-model settings used by Vekil's Codex launcher.
Its model catalog names `team-assistant` and uses text and standard function
tools. Hosted web search, freeform tools, and remote compaction are disabled
because the semantic policy route does not support them. The original Codex
binary, ACP adapter, and Orka v2 supervisor remain in the pinned base image.
The catalog uses short base instructions, and Codex's own subagent feature is
disabled for this single-worker job. Orka supplies the saved Agent instructions.
Build this small configuration layer from its isolated, credential-free context:

```sh
docker buildx build --builder remote-vm --platform linux/amd64 \
  --tag docker.io/sozercan/orka-acp-codex-runtime:efficiency-policy-20260922-r3 \
  --push demo/12-efficiency/runtime
```

`runtime_setup.py` pins the resulting image digest. Keep it identical for both
runs. This is preparation for the demo, outside the recorded walkthrough.

## Rehearsal and recording

Stop an earlier manual `run.py connect` process before starting. Run one
walkthrough at a time. The existing connection helper uses dedicated localhost
ports 18090, 18101, 18102, 18111, 18112, and 18120.

```sh
demo/12-efficiency/demo.sh

demo/record.sh 12-efficiency
```

The recording is asciicast v3 at 100 columns by 28 rows. Waits are compressed.
Archive an existing `demo/casts/12-efficiency.cast` before recording because the
recorder replaces that file. Each run gets a fresh evidence directory under
`bin/efficiency-production/`. `EFFICIENCY_RUN_DIR` can choose a new directory.
The script refuses an existing run directory, retains previous Tasks and
published branches, and stops its own connection and sampling processes on exit.

`story.py` runs offscreen to prepare manifests, retain raw responses, and check
the records. It is not a new user CLI. Configuration excerpts and comparison
files displayed with `cat` are derived from the actual deployment and run.
The visible `orka` commands call `bin/orka` with prepared server, namespace, and
short-lived authentication flags. They do not alter the presenter's CLI config,
`HOME`, or kubeconfig.

## Evidence

The support reply must contain two sentences, acknowledge the reported double
charge, request an order reference, and avoid claiming a completed refund or
investigation. The native worker must make zero tool calls. Native workers
still expose memory tools, so an empty configured tool list alone proves
nothing about tool use.

The coding Agent edits the real repository and runs its tests. The recording
also independently checks the published branch and reruns the unchanged six
tests. Both runs must change only the payment implementation and start from the
pinned revision. This is a small in-memory concurrency example, not a payment
processor or a claim about deduplication across processes.

Gateway snapshots, operation IDs, attempt records, completion logs, configured
destinations, and physical counters must reconcile. The routed run requires
actual classifier decisions and both model destinations. Tool continuations must
retain the hosted destination selected earlier in the same Task. Their calls
count toward model usage and do not count as new classifier calls. Vekil bounds
the context supplied to its classifier. Its aggregate truncation flag covers
background instructions and older messages as well as the current request.
The coding route view reports that flag when set. The demo establishes the
recorded destination and tested outcome, without claiming the classifier saw
the entire coding context or attributing the tier to one particular signal.
Fallback, failed model calls, changed instructions, restarts during a measured
interval, missing history, or failing checks stop the walkthrough.

The comparison includes reported model tokens, classifier requests and usage,
Task elapsed time, and sampled CPU and memory use. Classifier startup checks are
reported outside the Task intervals. Missing measurements remain unavailable.
Orka's usage report must reconcile wherever it reports consumed tokens. The
current Codex runtime leaves its attempt count unavailable there, with the
explicit gap `No consumed-token counts reported`. The demo retains that gap as
unavailable, verifies the native support measurement, and uses the gateway's
complete physical-call records for both jobs in the token comparison. It never
substitutes gateway numbers into missing Orka measurements or adds overlapping
counts together. Standalone Tasks appear under other team usage. Token totals
include cached input; they do not establish dollar savings. Local computation
also consumes capacity.

## Verification and video production

```sh
bash -n demo/12-efficiency/demo.sh
python3 -m unittest discover -s demo/12-efficiency -p 'test_*.py'
python3 demo/12-efficiency/story.py --run-dir <successful-run-directory> verify

python3 demo/narrated/prepare.py render 12-efficiency
```

See [`../narrated/README.md`](../narrated/README.md) for reference-voice synthesis,
Resolve assembly, and export checks. Give the replacement manifest a new
`output_name`, such as `12-efficiency-customer-story`, before assembly so the
previous project's staged media and exports remain available. The final video
ends with <https://orka-agents.github.io/orka/>.
