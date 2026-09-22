# Two teams, one address, more useful work

Payments must count repeated payment events once and fix a double-charge bug.
Inventory needs a useful stock answer and a fix for overlapping orders. Both
teams send requests through one company address. The platform team changes
the saved worker instructions, then the model-routing policy, and checks what
those changes actually produce.

The walkthrough uses the chapter helpers from `demo/lib/demo.sh`. It introduces
an Agent as saved worker instructions, a Task as the record of one piece of
work, and a gateway when the viewer sees it choose a model. The closing link is
<https://orka-agents.github.io/orka/>.

## Prepare on sertac-aks

Use the existing Orka CRDs and shared admission installation, a matching
`bin/orka` CLI, `kubectl`, Python 3 with PyYAML, and `curl`. All cluster commands
select `sertac-aks` explicitly. They do not switch the global context.

```sh
python3 demo/12-efficiency/prepare.py --context sertac-aks \
  --providers-source /Users/sozercan/projects/copilot-proxy/provider-v4.yaml render-safe

python3 demo/12-efficiency/prepare.py --context sertac-aks \
  --providers-source /Users/sozercan/projects/copilot-proxy/provider-v4.yaml apply
```

The first command renders manifests offline with the private classifier address
removed. The second prepares two namespace-scoped Orka installations, two Vekil
gateways, the compatibility router, and the AIKit CPU model. Controllers run in
harness-v2 mode. Setup refuses resources without its ownership label and
requires capacity on the selected model node. Its safe state record is
`bin/efficiency-production/setup.json`.

Setup copies the existing Copilot credential and the configured Jev credential
into demo-owned Secrets without printing them. It verifies fingerprints of the
original provider file, `vekil-system/vekil`, its token-cache PVC, and the original
Copilot Secret before and after each operation. It does not mount or replace
the original cache. Gateway state uses a separate writable directory.

The four team and runtime namespaces use a scoped namespace policy because the
shared admission installation still names a retired webhook path. Setup excludes
only these namespaces from that webhook, protects their harness-v2 claim with
the new policy, and adds the two controller identities to shared admission.
It leaves other namespace and admission rules in place.

Image references are pinned in `prepare.py`. The production images use
`docker.io/sozercan`; builds use the `remote-vm` buildx builder. When rebuilding
Vekil, export a clean commit with `git archive` and build that export. Its
checkout contains a private provider file and its Dockerfile copies the build
context, so the working directory must not be the build context. Provisioning,
image builds, and credential setup stay outside the recorded walkthrough.

## Rehearse and record

Run only one walkthrough at a time. It uses dedicated localhost ports 18090,
18101, 18102, 18111, 18112, and 18120 for cluster connections. Stop an earlier
manual `run.py connect` process before starting the standalone script.

```sh
# Live rehearsal, without recording.
demo/12-efficiency/demo.sh

# A fresh live run, recorded at 100 columns by 28 rows.
demo/record.sh 12-efficiency
```

Every run gets a new private directory under `bin/efficiency-production/`.
`EFFICIENCY_RUN_DIR` can select a different new directory. Existing directories
are refused. The script changes only the two demo gateways' routing modes and
the two demo Agents, creates new Tasks, and retains prior evidence. Its own
connection and resource-sampling processes stop on exit. A failure stops the
walkthrough; inspect the saved responses before trying a fresh run.

The short commands shown in the terminal are real helpers:

- `run ask` submits an authenticated OpenAI-compatible request to the shared
  router, then collects the actual Orka Task, answer, events, and gateway records.
- `run agents` updates the saved instructions. `gateway_mode` changes the two
  demo gateways after their earlier statistics have been captured.
- `view` shows compact excerpts from installed configuration or verified run
  records. Full JSON responses remain beside those views.

The application keeps `platform/coordinator` as its model name. Its hosted
coordinator creates one native AI Task using the team's Agent and returns that
Task's answer. The worker uses the stable `team-assistant` model name through
Vekil's Messages compatibility interface. A public name alone is not evidence
of which model handled a call.

## What the comparison proves

The initial inventory request produces a short sentence. Updating only its
Agent instructions adds structured stock, shortage, and customer-next-action
fields. The business request and application connection remain unchanged.

With those instructions fixed, both measured runs use four identical requests.
The baseline uses hosted GPT-5.5. In the second run, Jev assesses each worker
request and Vekil chooses a configured destination, either Qwen3.5 2B served by
AIKit on cluster CPU or hosted GPT-5.5. Incoming coordination stays hosted.
Routing is not an answer-quality test or an automatic correction mechanism.

Payments must return two unique events totaling 30. Inventory must return
18 available and a shortage of 6, with a customer next action. The proposed
payment and stock fixes run through six and eight checks respectively in real
Orka container Tasks, including overlapping requests, retries, and failure
cases. Model-generated code is restricted to the fixture's operations before
execution. The code, test command, container image, and result are retained.

`evidence.py` checks complete event histories, exactly one new worker Task per
request, unchanged prompts, identical requests and Agent configurations across
the measured runs, and zero worker tool calls. Native workers still expose
built-in memory tools even with an empty configured list; the evidence verifies
they were not used. The verifier joins gateway operation IDs, attempt records,
completion logs, installed destinations, and Orka's normalized usage events.
Counter changes must reconcile with the retained operations. Missing history,
unexpected traffic, restarts, configuration changes, or failed checks stop the
comparison. Both model destinations must actually appear in the routed run.

Orka records these standalone requests under Other team usage. Worker and
coordinator consumption are reconciled with gateway totals and counted once.
Classifier usage is reported separately, including startup checks outside the
measured request intervals. Missing classifier usage remains unavailable.
Reported tokens include cached input, so totals do not represent uncached
computation or dollar cost. The comparison also retains application elapsed
time and sampled model CPU time and working memory, including idle time.

This is a controlled example with fixed requests, not a general model benchmark.
The video makes no preset savings or speed claim. Local CPU work still consumes
capacity, and coordination can dominate the total model usage.

## Verification and production

```sh
bash -n demo/12-efficiency/demo.sh demo/record.sh
python3 -m unittest discover -s demo/12-efficiency -p 'test_*.py'
python3 demo/12-efficiency/evidence.py --run-dir <successful-run-directory>

python3 demo/narrated/prepare.py render 12-efficiency
python3 demo/narrated/resolve.py --prepare-lua \
  bin/narrated-demos/12-efficiency/manifest.json --sandbox
```

The narrated workflow uses the existing Qwen3 TTS Podman service and reference
voice, then creates a new DaVinci Resolve project. See
[`../narrated/README.md`](../narrated/README.md) for assembly, export verification,
and audio and frame checks. Earlier demo recordings remain separate. Generated
casts, audio, video, projects, and evidence stay in ignored output directories.
