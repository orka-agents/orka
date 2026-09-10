# Set up Fibey for Kubernetes and Azure AI Foundry

Use this guide if you manage the Kubernetes cluster and Azure AI Foundry
project. It prepares the two agent services used in the [demo walkthrough](README.md).
Complete deployment and registration before running the demo.

Fibey runs in Kubernetes for the `agentkit` option and in Azure AI Foundry for
the `foundry` option. Each option includes a service that connects the agent to
Orka. This guide calls that service a runtime. Registering a runtime tells Orka
where to send work and how to authenticate the connection.

The runtime includes Orka's supervisor, which accepts work from Orka and manages
the agent process. The deployed container starts the supervisor automatically.
For the Foundry option, a bridge translates requests for the hosted agent, and
a lifecycle broker sends them and keeps a persistent record of remote work.

## Prepare the build

Build one Fibey AgentKit image and use it for both hosting options. The commands
record each image's digest, an identifier for its exact contents, so both
deployments use the same version. These commands use the `remote-vm` builder and
`linux/amd64`. They publish images but do not deploy workloads.

Use AgentKit source containing commit `c9a18070363b9eded0be8ca826d8fc5365afc828`
and Foundry runtime source containing
`57988dbe9d6b9a5ed2a39ce10b3d5abc8a68a7c7`, or successors with the same contracts.
The Orka checkout must contain the external-v2 integration from PR #487, and
the controller must run with `--controller-mode=harness-v2`. Its watched
namespace must have the label `orka.ai/controller-mode: harness-v2`.
Configure the source directories, a registry you can push to, and your Docker
Buildx builder. Replace the example paths and names as needed:

```bash
ORKA_DIR="$PWD"
AGENTKIT_DIR="$HOME/projects/agentkit.feat-orka-harness-v2"
FOUNDRY_DIR="$HOME/projects/agent-runtime-foundry.feat-harness-v2"
FIBEY_DEMO="$ORKA_DIR/examples/fibey-custom-agent-demo"
FIBEY_BUILD_DIR="$(mktemp -d)"
FIBEY_REGISTRY=docker.io/sozercan
FIBEY_BUILDER=remote-vm
```

## Build the shared AgentKit image

[agentkitfile.yaml.example](agentkitfile.yaml.example) selects Microsoft Agent
Framework, an OpenAI-compatible model, and the Fibey instructions. Change the
model and endpoint to ones available to your hosted container before building.
The file names `OPENAI_API_KEY` but contains no credential value. Keep direct
tools, `brokeredTools`, and context providers absent for this shared demo.

Build the frontend and framework adapter from the required AgentKit source.
Capture each published digest so a remote builder can resolve the exact inputs:

```bash
docker buildx build --builder "$FIBEY_BUILDER" --platform linux/amd64 \
  -t "$FIBEY_REGISTRY/agentkit:fibey-v2" --provenance=false --push \
  --metadata-file "$FIBEY_BUILD_DIR/frontend.json" "$AGENTKIT_DIR"
FIBEY_FRONTEND="$FIBEY_REGISTRY/agentkit@$(jq -er '."containerimage.digest"' "$FIBEY_BUILD_DIR/frontend.json")"

docker buildx build --builder "$FIBEY_BUILDER" --platform linux/amd64 \
  -f "$AGENTKIT_DIR/runtimes/microsoft-agent-framework/Dockerfile" \
  -t "$FIBEY_REGISTRY/agentkit-serve-maf:fibey-v2" --provenance=false --push \
  --metadata-file "$FIBEY_BUILD_DIR/adapter.json" "$AGENTKIT_DIR"
FIBEY_ADAPTER="$FIBEY_REGISTRY/agentkit-serve-maf@$(jq -er '."containerimage.digest"' "$FIBEY_BUILD_DIR/adapter.json")"

docker buildx build --builder "$FIBEY_BUILDER" --platform linux/amd64 \
  -f "$FIBEY_DEMO/agentkitfile.yaml.example" \
  --build-arg BUILDKIT_SYNTAX="$FIBEY_FRONTEND" \
  --build-arg adapter="$FIBEY_ADAPTER" \
  -t "$FIBEY_REGISTRY/fibey-agentkit:v2" --provenance=false --push \
  --metadata-file "$FIBEY_BUILD_DIR/agent.json" "$FIBEY_DEMO"
FIBEY_IMAGE="$FIBEY_REGISTRY/fibey-agentkit@$(jq -er '."containerimage.digest"' "$FIBEY_BUILD_DIR/agent.json")"
```

## Run Fibey in Kubernetes

Build a container that includes Fibey and Orka's supervisor:

```bash
docker buildx build --builder "$FIBEY_BUILDER" --platform linux/amd64 \
  -f "$ORKA_DIR/workers/acp/images/agentkit/Dockerfile" \
  --build-arg AGENTKIT_RUNTIME_IMAGE="$FIBEY_IMAGE" \
  --build-arg AGENTKIT_ADAPTER_DIGEST="${FIBEY_IMAGE##*@}" \
  -t "$FIBEY_REGISTRY/orka-acp-agentkit:fibey-v2" --provenance=false --push \
  --metadata-file "$FIBEY_BUILD_DIR/direct.json" "$ORKA_DIR"
FIBEY_DIRECT_IMAGE="$FIBEY_REGISTRY/orka-acp-agentkit@$(jq -er '."containerimage.digest"' "$FIBEY_BUILD_DIR/direct.json")"
```

Deploy `FIBEY_DIRECT_IMAGE` with Deployment and Service name
`fibey-agentkit-runtime`, one replica, `strategy.type: Recreate`, and container
name `supervisor`. Drain the registered instance and settle its active Tasks
before replacing it.
The container starts `orka-acp-runtime`, the supervisor, and sets
`ORKA_ACP_PROVIDER=agentkit`. The supervisor starts AgentKit automatically using
the Agent Client Protocol, or ACP, and supplies its local model proxy and MCP
server. MCP is the protocol used for tool access; this demo permits no tools.
`AGENTKIT_PROTOCOL=orka` selects v1 and must not be used for this service.

Follow the [AgentKit v2 deployment contract](https://github.com/sozercan/agentkit/blob/c9a18070363b9eded0be8ca826d8fc5365afc828/docs/orka.md#harness-v2-byo-runtime)
and Orka's [supervisor image requirements](../../workers/acp/images/README.md#runtime-contract).
Supply the `ORKA_ACP_*` model, profile, epoch, instance, pool UUID/generation,
trust namespace, and Secret-mounted authentication files. Configure the Orka
MCP/artifact endpoints and upstream model proxy as documented there. A writable
`/sessions` volume and the supervisor's process/identity permissions are required;
the child runs under a separate UID/GID. The composed image alone is not a
configured v2 service.

## Host the same AgentKit agent in Foundry

Build [Dockerfile.foundry](Dockerfile.foundry) with the AgentKit repository as
its context. It copies AgentKit's real hosted wrapper onto the same Fibey image:

```bash
docker buildx build --builder "$FIBEY_BUILDER" --platform linux/amd64 \
  -f "$FIBEY_DEMO/Dockerfile.foundry" \
  --build-arg AGENTKIT_IMAGE="$FIBEY_IMAGE" \
  -t "$FIBEY_REGISTRY/fibey-agentkit-foundry:v2" --provenance=false --push \
  --metadata-file "$FIBEY_BUILD_DIR/hosted-agent.json" "$AGENTKIT_DIR"
FIBEY_HOSTED_IMAGE="$FIBEY_REGISTRY/fibey-agentkit-foundry@$(jq -er '."containerimage.digest"' "$FIBEY_BUILD_DIR/hosted-agent.json")"
```

Deploy this digest as a concrete Foundry Hosted Agent version. Use the
[AgentKit hosted-agent deployment example](https://github.com/sozercan/agentkit/blob/c9a18070363b9eded0be8ca826d8fc5365afc828/test/foundry-hosted-agent/README.md#deploy-to-foundry-with-azd)
and its `foundry.agent.yaml.example`, selecting the `responses` protocol and
port `8088`. Configure the model credential at deployment time. The wrapper
uses the actual AgentKit framework and baked config, not a mock model.

The wrapper relies on Foundry's Entra-authenticated ingress. Keep it behind
that ingress. Ordinary `agentkit-serve --protocol foundry` with a public bind
requires an additional AgentKit bearer; the current Foundry broker does not
inject that bearer. The dedicated wrapper avoids that incompatible extra
authentication layer. Its `/responses` output is non-streaming JSON, which the
Foundry ACP bridge accepts.

Record the project endpoint, agent name, and exact version returned by your
deployment. Do not use `latest` as the version. Provision the Foundry broker's
Azure identity with access to that target using the deployment's normal identity
configuration.

## Build the Foundry connection service

This service runs in Kubernetes and connects Orka to the Fibey agent you hosted
in Foundry. It includes the supervisor, the ACP bridge, and the lifecycle broker
described above.

Copy [foundry-acp.json](foundry-acp.json) into the Foundry build context and edit
its public target fields and model to match the deployment. This file contains
no Azure credentials. Its exact bytes will be part of the v2 profile:

```bash
cp "$FIBEY_DEMO/foundry-acp.json" "$FOUNDRY_DIR/examples/fibey-acp.json"
# Edit $FOUNDRY_DIR/examples/fibey-acp.json with the deployed target before building.

docker buildx build --builder "$FIBEY_BUILDER" --platform linux/amd64 \
  -f "$FOUNDRY_DIR/Dockerfile.acp" \
  --build-arg FOUNDRY_CONFIG=examples/fibey-acp.json \
  -t "$FIBEY_REGISTRY/fibey-foundry-acp:v2" --provenance=false --push \
  --metadata-file "$FIBEY_BUILD_DIR/foundry-source.json" "$FOUNDRY_DIR"
FIBEY_FOUNDRY_SOURCE="$FIBEY_REGISTRY/fibey-foundry-acp@$(jq -er '."containerimage.digest"' "$FIBEY_BUILD_DIR/foundry-source.json")"

docker buildx build --builder "$FIBEY_BUILDER" --platform linux/amd64 \
  -f "$ORKA_DIR/workers/acp/images/foundry/Dockerfile" \
  --build-arg FOUNDRY_RUNTIME_IMAGE="$FIBEY_FOUNDRY_SOURCE" \
  --build-arg FOUNDRY_ADAPTER_DIGEST="${FIBEY_FOUNDRY_SOURCE##*@}" \
  -t "$FIBEY_REGISTRY/orka-acp-foundry:fibey-v2" --provenance=false --push \
  --metadata-file "$FIBEY_BUILD_DIR/foundry-runtime.json" "$ORKA_DIR"
FIBEY_FOUNDRY_IMAGE="$FIBEY_REGISTRY/orka-acp-foundry@$(jq -er '."containerimage.digest"' "$FIBEY_BUILD_DIR/foundry-runtime.json")"
```

Use a single-replica Deployment named `fibey-foundry-runtime` with
`strategy.type: Recreate` and these containers, following the
[Foundry v2 process configuration](https://github.com/orka-agents/agent-runtime-foundry/blob/57988dbe9d6b9a5ed2a39ce10b3d5abc8a68a7c7/docs/harness-v2.md#process-configuration):

| Container | Image | Process |
| --- | --- | --- |
| `supervisor` | `FIBEY_FOUNDRY_IMAGE` | Default `orka-acp-runtime` entrypoint, with `ORKA_ACP_PROVIDER=foundry` |
| `broker` | `FIBEY_FOUNDRY_SOURCE` | `/agent-runtime-foundry --protocol broker --config /agent/foundry.json` |

The supervisor needs the same classes of v2 bootstrap configuration as the
direct backend. Point `ORKA_ACP_PROVIDER_PROXY_BASE_URL` at
`http://127.0.0.1:8091/v1`; its provider token file holds the broker bearer.
The broker listens only on loopback and owns a private persistent volume.
Only the broker receives Azure Workload Identity or another refreshable
`DefaultAzureCredential` configuration. The broker's state must survive both
container and Pod replacement and remain inaccessible to the ACP child.

Expose the supervisor as `fibey-foundry-runtime`.

## Register the deployed backends

Create the two `AgentRuntime` registrations as part of backend provisioning,
following the [external runtime contract](../../website/docs/guides/bring-your-own-agent-runtime.md#strict-governed-registration)
and its complete registration sample. The deployment must pin the exact
identity, profile, protocol limits, and governance claims of each service:

| Registration | `providerKind` | `adapterName` | Adapter digest | Agent configuration digest |
| --- | --- | --- | --- | --- |
| `fibey-agentkit-runtime` | `agentkit` | `agentkit-serve-acp` | Digest of the Fibey AgentKit source image, before supervisor composition | SHA-256 of the exact baked `/agent/agent.yaml` bytes |
| `fibey-foundry-runtime` | `foundry` | `foundry-serve-acp` | Digest of the configured Foundry ACP source image, before supervisor composition | SHA-256 of the exact baked `/agent/foundry.json` bytes |

Both profiles use read intent, credential role `operator-managed`, credential
scope `external-runtime`, and resource class `external`. The model must match
the baked configuration. Keep all MCP tool and approval lists empty and
`allowBash: false`. The runtime's `/v2/capabilities` must advertise
`supportsAgentSessionConfiguration: false`; that is an HTTP capability, not an
additional field in the AgentRuntime CRD.

Do not hash the Agentkitfile as the AgentKit configuration digest. AgentKit
renders that input into a different `/agent/agent.yaml` file. The overall
profile digest must use the v2 contract's canonicalization.

Provision separate controller-bearer and operation-capability Secrets for each
registration, with values of at least 32 bytes. Both Secrets need the exact
registration name and endpoint bindings described in the external runtime
contract. Mount them into the corresponding supervisor. Keep model credentials
separate; only the Foundry broker receives Azure identity and its private
ownership ledger.

Pin each registration to its supervisor lifetime's instance ID and configure
`ORKA_ACP_CONTROLLER_EPOCH` with the current controller epoch. Handle epoch and
instance changes through the external runtime lifecycle. Do not replace a
Foundry lifetime or delete its ledger while remote ownership remains unresolved.

Wait for the actual `AgentRuntime` registrations to become Ready before running
the demo. `GET /v2/health` and `GET /v2/capabilities` are safe public probes;
readiness also requires authenticated status and Orka's conformance checks.
A ready hosted container alone does not establish that the bridge can
authenticate or settle remote work.

## Demo settings and limits

The demo sends one prompt per Task, uses `workspace.intent: read`, and has no
tools or repository. The Task selects `agentRuntime.allowedTools: []`.
Bash denial belongs to the registered MCP policy. External v2 Tasks must omit
`allowBash` entirely; setting it to `false` is still an unsupported runtime
override.

Do not enable AgentKit `brokeredTools` for the Foundry option at the companion
revisions listed above. After a hosted tool call, AgentKit requires a
continuation proof, authorization attached to the returned tool result, in
`function_call_output`. The Foundry v2 broker does not forward that proof.
The direct AgentKit ACP tool path is separate. Hosted tool support needs a
compatible authorization mechanism and end-to-end validation before the
shared tool policy can change.

## Optional: host the Orka supervisor in Foundry

The standard setup hosts Fibey in Foundry and keeps Orka's supervisor in
Kubernetes. To host the supervisor itself in Foundry, follow the Foundry
runtime's [hosted-v2 package](https://github.com/orka-agents/agent-runtime-foundry/blob/57988dbe9d6b9a5ed2a39ce10b3d5abc8a68a7c7/docs/foundry-hosted-v2.md).
That adds another Hosted Agent and a Kubernetes `--protocol hosted-gateway`
process. Register the gateway Service as `fibey-foundry-runtime`; the Agents,
Task, and submit commands stay the same. The hosted lifetime has bounded
WebSocket connections and needs drain/retirement before replacement.
Register this setup using the
[external runtime contract](../../website/docs/guides/bring-your-own-agent-runtime.md#strict-governed-registration)
before running the demo.

## Verify the demo's v2 connection

The demo script checks that the selected runtime is Ready for its current
registration generation, instance, and profile before creating a Task. It then
checks the Task's immutable binding, the record of which Agent and AgentRuntime
Orka selected. It reports success before the agent finishes its answer. Each
run creates a new Task; it never patches an existing Task or reuses a Session
across hosting options.

After running the [demo](README.md), inspect the execution details using the
context and namespace you set there:

```bash
kubectl --context="$FIBEY_CONTEXT" -n "$FIBEY_NAMESPACE" get \
  task/fibey-quincy-agentkit-01 task/fibey-quincy-foundry-01 -o json |
  jq '.items[] | {
    name: .metadata.name,
    phase: .status.phase,
    contract: .status.agentExecutionBinding.contractVersion,
    backend: .status.agentExecutionBinding.backend,
    runtime: .status.execution.agentRuntimeName,
    runtimeUID: .status.execution.agentRuntimeUID,
    instance: .status.execution.runtimeInstanceID,
    attempt: .status.execution.attempt,
    outcome: .status.execution.outcome,
    reason: .status.execution.reason,
    delivery: .status.delivery.state,
    resultRef: .status.resultRef
  }'
```

Expect `orka.harness.v2`, backend `external-endpoint`, the selected registration
name and UID, and `execution.outcome: Succeeded`. `execution.runtimePoolName`
should be absent. `status.harnessRuntime` belongs to v1 and is not evidence for
these runs. This verifies inference, runtime selection, and execution identity.
It does not validate hosted tool governance, conversation continuation, or
recovery after an ambiguous remote operation.

`resultRef.available: true` means Orka has stored an answer. Read the answer
with `orka task result`, using the server, namespace, and credentials configured
in the [walkthrough](README.md#3-read-and-compare-the-answers).

## Troubleshoot a run

If creation, binding, or waiting fails, inspect that exact Task before another
submission. A create error can still leave a Task running. Never reuse an
existing Task name.

`execution.outcome: OutcomeUnknown` means Orka cannot confirm the outcome of
remote execution. Keep the Task, finalizers, supporting Secrets, and Foundry
ledger until normal cleanup establishes retirement. Do not automatically retry
the run or remove these resources to clear the error.

## Check the example locally

Run these checks from the Orka checkout:

```bash
bash scripts/tests/fibey-v2-demo-test.sh
kubectl kustomize examples/fibey-custom-agent-demo
go test ./internal/admission -run 'Test(Shipped|Documented)ManifestsDecodeStrictly' -count=1
```

These checks validate the example files and the submission script without a
cluster, Azure, or a model. Successful live runs still require the configured
hosted endpoint and provider credentials.
