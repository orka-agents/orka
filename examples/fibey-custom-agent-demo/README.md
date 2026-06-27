# Fibey custom AgentKit runtime demo

This demo exercises the first BYOA/runtimeRef slice: Orka registers a pre-deployed custom-agent harness endpoint as an `AgentRuntime`, then an `Agent` routes `type: agent` work to it with `spec.runtime.runtimeRef`.

The mock service uses the example Orka harness server as an AgentKit-compatible stand-in. It advertises `runtimeName: fibey-agentkit`, supports `orka.harness.v1`, and runs in `observed` tool mode. Real AgentKit images should expose the same Orka-facing endpoint while owning any standalone or Foundry protocol surfaces outside Orka.

## Build/load the mock image for kind

```bash
docker build -t ghcr.io/sozercan/orka-example-echo-harness:latest -f examples/harness/echo/Dockerfile .
kind load docker-image ghcr.io/sozercan/orka-example-echo-harness:latest --name <your-kind-cluster>
```

## Apply the demo

```bash
kubectl apply -k examples/fibey-custom-agent-demo
kubectl wait --for=condition=Ready agentruntime/fibey-agentkit --timeout=60s
kubectl get task fibey-quincy-north-alert -o yaml
```

Expected flow:

1. `AgentRuntime/fibey-agentkit` reads only a harness token Secret labeled `orka.ai/agent-runtime-auth: "true"` (and optionally scoped with `orka.ai/agent-runtime-name`) before probing `/v1/health` and `/v1/capabilities` and becoming Ready.
2. `Agent/fibey-custom` selects the runtime by `runtimeRef`.
3. `Task/fibey-quincy-north-alert` starts a harness turn against the mock endpoint.
4. The task timeline shows `TurnStarted`, `RuntimeOutput`, and `TurnCompleted` events.

This is intentionally **observed mode only**. Orka-owned side-effect tools such as work-order dispatch stay separate until brokered tool/approval mode is implemented.
