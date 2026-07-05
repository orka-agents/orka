# Fibey bring-your-own agent runtime demo

This demo exercises the first bring-your-own agent runtime slice: Orka registers a namespace-local `AgentRuntime` facade for a remote execution backend, then an `Agent` routes `type: agent` work to it with `spec.runtime.runtimeRef`.

The checked-in backend is a deterministic generic HTTP harness fixture. It advertises `runtimeName: fibey-http-runtime`, supports `orka.harness.v1`, and runs in `observed` tool mode by default. AgentKit Serve and Foundry should plug in by swapping only the backend Service/adapter endpoint and `AgentRuntime` facade, not the Orka workflow.

## Backend facades

| Facade | Backend | Credentials |
| --- | --- | --- |
| `fibey-http-runtime` | Generic mock/self-hosted HTTP runtime | Harness bearer token only |
| `fibey-agentkit-runtime` | AgentKit Serve adapter | Adapter/runtime config only |
| `fibey-foundry-runtime` | Foundry adapter | Adapter Secret; no Orka Tool production credentials |

`fibey-agentkit-runtime` is intentionally observed-only in the checked-in demo: it should show `toolExecutionModes: [observed]`, `supportsCancel: true`, and `supportsRuntimeSessions: true`, with no `brokeredToolClasses` or `supportsContinuation`. AgentKit brokered read/write/coordination exist only for deployments that explicitly enable those conformance-gated profiles.

All facades are namespace-local `AgentRuntime` objects. Remote execution backends do **not** receive production Orka Tool credentials. In brokered mode, remote backends request tools and Orka owns authorization, approvals, idempotency, credential resolution, execution/brokering, events, lineage, and audit.

## Build/load the generic HTTP fixture image for kind

```bash
docker build -t ghcr.io/orka-agents/orka/example-echo-harness:latest -f examples/harness/echo/Dockerfile .
kind load docker-image ghcr.io/orka-agents/orka/example-echo-harness:latest --name <your-kind-cluster>
```

The fixture can run scripted behaviors through `ORKA_REMOTE_HTTP_RUNTIME_BEHAVIOR`:

- `success` — return a final result;
- `read-tool` — emit a brokered read-only tool request and observed result frame;
- `approval-tool` — emit an approval-pending frame and resume on `/v1/turns/{turnID}/continue`;
- `failure` — fail deterministically;
- `timeout` — emit a retryable timeout failure;
- `cancellation` — wait until the turn is cancelled.

The default demo uses `success`/observed mode so it can run without external credentials or brokered-tool infrastructure.

## Apply the demo

```bash
kubectl apply -k examples/fibey-custom-agent-demo
kubectl wait --for=condition=Ready agentruntime/fibey-http-runtime --timeout=60s
kubectl get task fibey-quincy-north-alert -o yaml
```

Expected flow:

1. `AgentRuntime/fibey-http-runtime` reads only a harness token Secret labeled `orka.ai/agent-runtime-auth: "true"`, scoped with `orka.ai/agent-runtime-name`, and endpoint-bound with `orka.ai/agent-runtime-endpoint` before probing `/v1/health` and `/v1/capabilities` and becoming Ready.
2. `Agent/fibey-remote-http` selects the runtime by `runtimeRef`.
3. `Task/fibey-quincy-north-alert` starts a harness turn against the generic HTTP runtime endpoint.
4. The task timeline shows `TurnStarted`, `RuntimeOutput`, and `TurnCompleted` frames mapped into native Orka execution events.

A successful Task should include `status.harnessRuntime.runtimeRefName: fibey-http-runtime`, proving the resolved runtime target was frozen for the accepted turn.

## Swapping backends

To test AgentKit Serve or Foundry, keep the Orka workflow and tool policy the same. Replace only:

- the backend Deployment/Service or external endpoint;
- the harness bearer-token Secret binding;
- the namespace-local `AgentRuntime` facade used by `Agent.spec.runtime.runtimeRef`.

Optional facade manifests are checked in but not included in the default `kustomization.yaml` because they require separately deployed adapters:

For a local/kind AgentKit observed-mode demo with no model credentials, build and
load an AgentKit test-agent image from the AgentKit Serve checkout, then deploy
the offline echo fixture Service used only for readiness/conformance demos:

```bash
# From /path/to/agentkit.serve:
make build-agentkit build-serve build-test-agent AGENT_IMAGE=hello-agent:test
kind load docker-image hello-agent:test --name <your-kind-cluster>

# From this Orka checkout:
kubectl apply -f examples/fibey-custom-agent-demo/secret-agentkit.yaml
kubectl apply -f examples/fibey-custom-agent-demo/agentkit-runtime-offline.example.yaml
kubectl apply -f examples/fibey-custom-agent-demo/agentruntime-agentkit.yaml
kubectl apply -f examples/fibey-custom-agent-demo/agent-agentkit.yaml
kubectl wait --for=condition=Ready agentruntime/fibey-agentkit-runtime --timeout=60s
```

The example deployment sets `AGENTKIT_PROTOCOL=orka`, reads
`AGENTKIT_AUTH_TOKEN` from the Orka client-auth Secret, and sets
`AGENTKIT_ORKA_OFFLINE_ECHO=1` so the AgentRuntime readiness probe and demo task
complete without live provider credentials. Remove `AGENTKIT_ORKA_OFFLINE_ECHO`
and provide normal model/runtime credentials for production AgentKit services.

```bash
# AgentKit Serve observed-mode facade; requires a Service named fibey-agentkit-runtime.
kubectl apply -f examples/fibey-custom-agent-demo/secret-agentkit.yaml
kubectl apply -f examples/fibey-custom-agent-demo/agentruntime-agentkit.yaml
kubectl apply -f examples/fibey-custom-agent-demo/agent-agentkit.yaml
kubectl wait --for=condition=Ready agentruntime/fibey-agentkit-runtime --timeout=60s

# Foundry adapter facade; requires a Service named fibey-foundry-runtime.
# Build/deploy examples/harness/foundry with ORKA_FOUNDRY_* credentials first.
kubectl apply -f examples/fibey-custom-agent-demo/secret-foundry.yaml
kubectl apply -f examples/fibey-custom-agent-demo/agentruntime-foundry.yaml
kubectl apply -f examples/fibey-custom-agent-demo/agent-foundry.yaml
kubectl wait --for=condition=Ready agentruntime/fibey-foundry-runtime --timeout=60s
```

Run the same task against another backend by changing only `spec.agentRef.name`, for example:

```bash
examples/fibey-custom-agent-demo/switch-backend.sh agentkit
examples/fibey-custom-agent-demo/switch-backend.sh foundry
examples/fibey-custom-agent-demo/switch-backend.sh http
```

The script validates the selected `AgentRuntime` and `Agent`, then patches only
the Task's `spec.agentRef.name`.

Brokered mode is used only when the selected runtime advertises brokered capabilities and the task/agent exposes allowed tools. Current AgentKit Serve facades do not advertise brokered mode, so AgentKit-owned tools remain internal to AgentKit and Orka observes only lifecycle/output frames. Orka-owned side-effect tools stay behind Orka brokered governance; production tool credentials are not handed to the remote backend.
