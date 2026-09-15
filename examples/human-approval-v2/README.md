# Human approval for v2 tools

This example reads a simulated pump inventory, proposes one work order, and
waits for a person to approve it in Orka. The work-order endpoint only records
a receipt in SQLite. It does not contact another service or change equipment.

Use the same [Task](task.yaml) with either a direct AgentKit Microsoft Agent
Framework runtime or AgentKit hosted behind `agent-runtime-foundry`.
[acceptance.py](acceptance.py) submits real Orka Tasks and reviews through the
normal API. It checks the simulator's execution count as well as the Task's
stored outcome.

| Tool | Policy | Observable effect |
| --- | --- | --- |
| `read-inventory` | Automatic | One counted inventory read |
| `create-work-order` | Human approval required | One simulated receipt after approval, zero before it |

The runner creates a fresh `runID` for each scenario. The simulator retains
every invocation, including duplicates, so a repeated action cannot disappear
behind an idempotent fixture response.

## Prerequisites

- Builds of Orka, AgentKit, and Foundry with brokered tool approval support.
  A direct runtime must use the Microsoft Agent Framework ACP adapter.
- Deliberate approval qualification of each exact adapter, model, and frozen
  configuration, including the hosted AgentKit version behind Foundry. Approval
  support defaults off; a provider name or a new supervisor image does not
  qualify an older adapter.
- A prepared Orka installation using `--controller-mode=harness-v2`, persistent
  execution-event storage, and the updated AgentRuntime CRD that permits
  nonempty `approvalRequiredTools`. The watched namespace must have
  `orka.ai/controller-mode: harness-v2`.
- Two separately provisioned external runtimes named
  `human-approval-agentkit-runtime` and `human-approval-foundry-runtime`.
  Each must pass current Orka conformance with the profile prepared below.
  Keep capacity for at least two concurrent prompts in each runtime.
- A private file containing an Orka API bearer for a reviewer allowed to create,
  read, update, and delete these Tasks. The cancellation check also needs
  Kubernetes permission to patch Task finalizers. The runner never prints the
  token or retries a mutation after a lost response.
- Python 3.11+, Go, `kubectl`, and `jq`. Preparing hosted schemas and running
  local runtime tests also uses `uv` from the AgentKit checkout.

The deployment contract remains the
[external v2 runtime contract](../../website/docs/guides/bring-your-own-agent-runtime.md).
Provision its controller authentication, operation capability, current epoch,
instance identity, private session storage, and isolated child process exactly
as for any other strict runtime. Keep provider credentials outside the child.
Do not add Agent or Task configuration overrides to a `runtimeRef` Agent.

| Bound | Required configuration |
| --- | --- |
| Human review | 600 seconds maximum |
| Tool execution after approval | 240 seconds maximum, independent of review time |
| Enclosing MCP call | 900 seconds |
| Direct AgentKit child | Supervisor injects `AGENTKIT_MCP_TIMEOUT=900` for this policy |
| Hosted AgentKit response state | `AGENTKIT_FOUNDRY_RESPONSE_STATE_TTL_SECONDS=1800` |
| Foundry hosted-session idle timeout | At least 1800 seconds on the pinned hosted version |
| Example Task | 20 minutes including model work, review, execution, and continuation |

These settings do not extend a cancelled Task or an expired prompt lease.
Check the hosted AgentKit `/readiness` response for
`foundryResponses.stateTtlSeconds: 1800` and verify the actual Foundry version's
idle timeout. Preserve pending hosted state across process restarts with
`AGENTKIT_FOUNDRY_RESPONSE_STATE_FILE` on private persistent storage.

## Prepare the simulator and tools

Run commands from the Orka checkout. For a local kind installation, use its
worktree-scoped wrapper for all Kubernetes access. These commands assume the
cluster and watched namespace already exist.

```bash
DEMO="$PWD/examples/human-approval-v2"
OUT="$PWD/bin/human-approval-v2"
KINDCTL="$PWD/.agents/skills/kindctl/bin/kindctl"
APPROVAL_NAMESPACE=your-harness-v2-namespace
APPROVAL_CONTEXT="$("$KINDCTL" kubectl config current-context)"
mkdir -p "$OUT"
```

The simulator can run as a local process when the controller can reach it:

```bash
python3 "$DEMO/simulated_tools.py" --host 127.0.0.1 --port 8099 \
  --state "$OUT/calls.sqlite"
```

For an in-cluster simulator, use [simulator.yaml.example](simulator.yaml.example).
Set `APPROVAL_PYTHON_IMAGE` to an available Python 3.11+ image by digest before
rendering. It needs only the Python standard library. Keep this service in the
isolated test namespace; it intentionally has no authentication.

```bash
: "${APPROVAL_PYTHON_IMAGE:?Set an immutable Python image reference}"
case "$APPROVAL_PYTHON_IMAGE" in *@sha256:*) ;; *) exit 1 ;; esac

"$KINDCTL" kubectl -n "$APPROVAL_NAMESPACE" create configmap human-approval-tools \
  --from-file=simulated_tools.py="$DEMO/simulated_tools.py" \
  --dry-run=client -o json > "$OUT/simulator-config.json"
"$KINDCTL" kubectl -n "$APPROVAL_NAMESPACE" apply -f "$OUT/simulator-config.json"
"$KINDCTL" kubectl -n "$APPROVAL_NAMESPACE" create --dry-run=client \
  --validate=false -f "$DEMO/simulator.yaml.example" -o json |
  jq --arg image "$APPROVAL_PYTHON_IMAGE" \
    '(.items[] | select(.kind == "Deployment") | .spec.template.spec.containers[0].image) = $image' \
    > "$OUT/simulator.json"
"$KINDCTL" kubectl -n "$APPROVAL_NAMESPACE" apply -f "$OUT/simulator.json"
"$KINDCTL" kubectl -n "$APPROVAL_NAMESPACE" rollout status deployment/human-approval-tools --timeout=120s
"$KINDCTL" kubectl -n "$APPROVAL_NAMESPACE" apply -f "$DEMO/tools.yaml" -f "$DEMO/agents.yaml"
```

If using a controller-reachable host process instead, replace the two
`spec.http.url` values in a copy of [tools.yaml](tools.yaml) before applying it.
The controller must reach that exact simulator. A separate local counter does
not prove what the controller executed.

## Prepare and pin each backend

Use local checkouts with brokered tool approval support:

```bash
AGENTKIT_DIR=/path/to/agentkit
FOUNDRY_DIR=/path/to/agent-runtime-foundry
```

For direct AgentKit, build [agentkitfile.yaml.example](agentkitfile.yaml.example)
with the source-built AgentKit frontend and Microsoft Agent Framework adapter.
Set the frontend and adapter build arguments to their immutable image digests.
The baked `/agent/agent.yaml` must contain no direct `tools` or `brokeredTools`.
The ACP supervisor supplies this Task's MCP tools at session creation.
Compose that image with the current Orka supervisor using
`workers/acp/images/agentkit/Dockerfile`, passing `AGENTKIT_RUNTIME_IMAGE` by
digest and the same image digest as `AGENTKIT_ADAPTER_DIGEST`.

For hosted AgentKit, generate the safe schemas from the same Tool definitions.
Merge those schemas into [agent-hosted.yaml.example](agent-hosted.yaml.example):

```bash
uv run --directory "$AGENTKIT_DIR/runtimes/common" agentkit-brokered-tools \
  "$DEMO/tools.yaml" -o "$OUT/brokered-tools.yaml"
uv run --directory "$AGENTKIT_DIR/runtimes/common" python - \
  "$DEMO/agent-hosted.yaml.example" "$OUT/brokered-tools.yaml" "$OUT/hosted-agent.yaml" <<'PY'
import pathlib, sys, yaml
agent = yaml.safe_load(pathlib.Path(sys.argv[1]).read_text())
agent.update(yaml.safe_load(pathlib.Path(sys.argv[2]).read_text()))
pathlib.Path(sys.argv[3]).write_text(yaml.safe_dump(agent, sort_keys=False))
PY
```

Bake that `hosted-agent.yaml` as `/agent/agent.yaml` into a current AgentKit
brokered Responses image. Run `agentkit-foundry-brokered --config
/agent/agent.yaml --host 0.0.0.0 --port 8088` with
`AGENTKIT_FOUNDRY_BROKERED_MODEL_LOOP=1`. Set the state lifetime and idle timeout
from the table above. Use a concrete hosted agent version, then fill in the
public target fields in [foundry-acp.json](foundry-acp.json) before building the
Foundry ACP source image. Keep `toolSchemaMode: provider-static`.

Give the Foundry broker
`ORKA_FOUNDRY_BROKER_AGENTKIT_CONTINUATION_PROOF` and the hosted AgentKit process
`AGENTKIT_FOUNDRY_BROKERED_CONTINUATION_PROOF` through Secret-backed environment
variables with the same value. Keep that proof out of images, Tasks, and the
supervisor/ACP child. The hosted gateway must preserve the authenticated
continuation body extension. Follow the companion
[Foundry runtime contract](https://github.com/orka-agents/agent-runtime-foundry/blob/main/docs/harness-v2.md)
for the broker sidecar and private ownership ledger.

Compose the Foundry source image with the current Orka supervisor using
`workers/acp/images/foundry/Dockerfile`. Pass `FOUNDRY_RUNTIME_IMAGE` by digest
and the same source image digest as `FOUNDRY_ADAPTER_DIGEST`.

Generate each profile independently. `DIRECT_AGENT_CONFIG` must name the exact
baked `/agent/agent.yaml` extracted from the direct AgentKit image.
`FOUNDRY_CONFIG` must name the exact baked `/agent/foundry.json`. The renderer
rejects an Agentkitfile in place of the baked ABI and uses Orka's existing
canonical digest functions.

```bash
go run ./examples/human-approval-v2/profile --provider agentkit \
  --adapter-digest "${AGENTKIT_RUNTIME_IMAGE##*@}" --config "$DIRECT_AGENT_CONFIG" \
  > "$OUT/agentkit-profile.json"
go run ./examples/human-approval-v2/profile --provider foundry \
  --adapter-digest "${FOUNDRY_RUNTIME_IMAGE##*@}" --config "$FOUNDRY_CONFIG" \
  > "$OUT/foundry-profile.json"
```

For each new deployment, use its generated `supervisorEnv` as the public profile
environment and copy its `profile` and `mcpPolicy` into the new AgentRuntime's
`spec.capabilities`. Keep the rest of the complete
[registration sample](../../config/samples/core_v1alpha1_agentruntime.yaml)
consistent with that deployed runtime's endpoint, authentication, instance,
limits, and governance claims. Both generated policies contain:

```yaml
allowedTools: [create-work-order, read-inventory]
disallowedTools: []
allowBash: false
approvalRequiredTools: [create-work-order]
```

Use these inputs before the first Task binds. Do not patch the approval list
on an already-bound profile or copy one backend's overall profile digest to
the other. Tool, approval, configuration, and overall profile digests must
match the effective configuration.

For an operator-qualified combination, set the supervisor environment variable
`ORKA_ACP_BROKERED_TOOL_APPROVAL_PROFILE_DIGEST` to that backend's generated
`.profile.digest`. The profile renderer deliberately omits this opt-in. For
initial qualification, enable it only in the isolated test deployment, run the
counted acceptance checks below, and record the tested source revisions, image
digests, exact configuration, and hosted version. Use that evidence before
enabling the same profile elsewhere.

The supervisor validates this opt-in against its computed RuntimeProfile. A
malformed or different digest, or a provider other than AgentKit or Foundry,
prevents startup. Without it, the supervisor advertises no brokered approval
support, and approval-required sessions are rejected. Changing the adapter,
model, or configuration needs a new qualification of the resulting profile.

Wait for both registrations to become current and Ready. Each selected
supervisor's `GET /v2/capabilities` must report
`provider.supportsBrokeredToolApprovals: true` and
`provider.supportsPermissions: false`. `supportsAgentSessionConfiguration`
must be false or absent. The runner verifies these claims, the generated
profile, and the eventual Task binding before testing a review.

## Run the API acceptance checks

Expose the prepared Orka API, each supervisor's public probes, and the counted
simulator at local URLs using worktree-scoped port forwards. For example, run
the simulator forward in another terminal and keep it open:

```bash
"$KINDCTL" kubectl -n "$APPROVAL_NAMESPACE" port-forward service/human-approval-tools 18099:8099
```

Set `ORKA_API_URL`, `AGENTKIT_CAPABILITIES_URL`, and `FOUNDRY_CAPABILITIES_URL`
to the corresponding reachable base URLs. `ORKA_API_TOKEN_FILE` names the
private reviewer token file, with no header prefix. The base URLs contain no
credentials. For a non-kind installation, use its explicit context and scoped
kubeconfig instead of the wrapper.

```bash
run_acceptance() {
  "$KINDCTL" exec -- python3 "$DEMO/acceptance.py" \
    --context "$APPROVAL_CONTEXT" --namespace "$APPROVAL_NAMESPACE" \
    --backend "$1" --profile-json "$OUT/$1-profile.json" \
    --api-url "$ORKA_API_URL" --api-token-file "$ORKA_API_TOKEN_FILE" \
    --capabilities-url "$2" --simulator-url http://127.0.0.1:18099 \
    --report "$OUT/$1-acceptance.json"
}
run_acceptance agentkit "$AGENTKIT_CAPABILITIES_URL"
run_acceptance foundry "$FOUNDRY_CAPABILITIES_URL"
```

Each run checks all of the following:

- One inventory lookup finishes automatically and the work-order execution
  count stays zero while a real approval is pending.
- An independent Task finishes during that wait. The approve case waits at
  least 125 seconds, beyond the former direct MCP timeout.
- Approval executes the action once and returns its receipt to the original
  Task. Repeating the decision cannot execute it again; a competing decline is
  rejected.
- Decline, Task cancellation, and the actual 600-second review expiry leave
  the action count at zero. A later approve request is rejected through the API.

Allow at least 13 minutes per backend for the default review and expiry waits,
plus model work. `--scenarios approve,decline,cancel` skips the ten-minute expiry
case during iteration. Record that limited scenario list when reporting a run.
Shortening `--hold-seconds` also stops proving compatibility beyond the old
120-second timeout.

To inspect a review manually, open the Task's approval panel in Orka. Confirm
the exact tool, argument preview, task, and expiry before deciding. The same
review is available from `GET /api/v1/tasks/{name}/approvals?namespace={ns}`;
decisions use `POST /api/v1/tasks/{name}/approvals/{id}/decision?namespace={ns}`
with `{"decision":"approve"}` or `{"decision":"decline"}`. Approval and tool
execution have separate outcomes. An approved call can still fail or have an
unknown result.

## Check runtime recovery

Run a fresh case with `--scenarios recovery`, the same connection arguments,
and a separate report file. When the runner prints `Recovery ready`, restart
only the selected disposable runtime Deployment using its existing recovery
procedure. Retain the simulator database, Task, supporting authentication,
and Foundry broker ledger. Reconnect port forwards if their target Pod changed.

The runner requires Orka to observe a different supervisor boot ID, waits for
the original attempt to settle, and then attempts to approve the old review.
That request must fail and the work-order execution count must remain zero.
It does not resubmit the Task or manufacture a continuation. A runtime that has
not established replacement authority does not pass this check.

Hosted AgentKit process recovery within the same remote session is a separate
compatibility check. Run the companion saved-state tests below. A lost state
file must fail without replaying the action. A local test does not prove that
the public Foundry gateway preserves the continuation proof; the real API run
against the configured hosted version is still required.

## Local checks without a deployment

These commands check the fixture and run actual runtime processes against
counted local HTTP services. They do not create Orka Tasks or establish cloud
gateway compatibility.

```bash
python3 -m unittest discover -s examples/human-approval-v2 -p 'test_*.py' -v
go test ./examples/human-approval-v2/profile ./internal/admission \
  -run 'Test(Shipped|Documented)ManifestsDecodeStrictly' -count=1

AGENTKIT_TEST_APPROVAL_WAIT_SECONDS=121 \
  uv run --directory "$AGENTKIT_DIR/runtimes/microsoft-agent-framework" --extra dev \
  pytest -q tests/test_orka_approvals.py -k http_mcp
uv run --directory "$AGENTKIT_DIR/runtimes/common" --extra dev \
  pytest -q tests/test_foundry_approvals.py

(
  cd "$FOUNDRY_DIR"
  AGENTKIT_SOURCE_DIR="$AGENTKIT_DIR" \
  AGENTKIT_PYTHON="$AGENTKIT_DIR/runtimes/common/.venv/bin/python" \
    go test ./internal/broker -run TestBrokerAgentKitHostedIntegration -count=1 -v
)
```

The runner retains successful and declined Tasks and writes only identities,
decision outcomes, counts, and timings to its report. It deletes the cancelled
Task after observing settlement. On a failed run, inspect the listed Tasks
before starting another one. A cancelled Task may retain the runner's
`human-approval-v2.orka.ai/acceptance-observer` finalizer for inspection; remove
only that finalizer after its execution has settled. Keep the tool database
until the execution counts and any unknown outcome have been resolved.
