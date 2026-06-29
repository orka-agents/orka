# Fibey custom AgentKit runtime demo

This demo exercises the first BYOA/runtimeRef slice: Orka registers a pre-deployed custom-agent harness endpoint as an `AgentRuntime`, then an `Agent` routes `type: agent` work to it with `spec.runtime.runtimeRef`.

The mock service uses the example Orka harness server as an AgentKit-compatible stand-in. It advertises `runtimeName: fibey-agentkit`, supports `orka.harness.v1`, and runs in `observed` tool mode. Real AgentKit images should expose the same Orka-facing endpoint while owning any standalone or Foundry protocol surfaces outside Orka.

## Build/load the mock image for kind

```bash
docker build -t ghcr.io/sozercan/orka-example-echo-harness:latest -f examples/harness/echo/Dockerfile .
kind load docker-image ghcr.io/sozercan/orka-example-echo-harness:latest --name <your-kind-cluster>
```

## Using a real AgentKit image

The checked-in Deployment is a deterministic echo harness stand-in. To test a real AgentKit image after AgentKit's Orka protocol mode is available, replace the container image in `mock-agentkit-service.yaml` and configure the service with:

```yaml
env:
- name: AGENTKIT_PROTOCOL
  value: orka
- name: AGENTKIT_AUTH_TOKEN
  valueFrom:
    secretKeyRef:
      name: fibey-agentkit-harness-token
      key: token
```

The same Secret is referenced by `AgentRuntime.spec.clientAuth.bearerTokenSecretRef` and must keep these labels and annotations:

```yaml
metadata:
  labels:
    orka.ai/agent-runtime-auth: "true"
    orka.ai/agent-runtime-name: fibey-agentkit
  annotations:
    orka.ai/agent-runtime-endpoint: http://fibey-agentkit.default.svc.cluster.local:8080
```

After applying the manifests, the expected readiness check is:

```bash
kubectl wait --for=condition=Ready agentruntime/fibey-agentkit --timeout=60s
```

A successful Task should produce native Orka timeline events mapped from harness frames, including `AgentRuntimeStarted`, runtime output events, and `AgentRuntimeCompleted`. The Task status should also include `status.harnessRuntime.runtimeRefName: fibey-agentkit`, showing the resolved runtime target was frozen for the accepted turn.

## Apply the demo

```bash
kubectl apply -k examples/fibey-custom-agent-demo
kubectl wait --for=condition=Ready agentruntime/fibey-agentkit --timeout=60s
kubectl get task fibey-quincy-north-alert -o yaml
```

Expected flow:

1. `AgentRuntime/fibey-agentkit` reads only a harness token Secret labeled `orka.ai/agent-runtime-auth: "true"`, scoped with `orka.ai/agent-runtime-name`, and endpoint-bound with `orka.ai/agent-runtime-endpoint` before probing `/v1/health` and `/v1/capabilities` and becoming Ready.
2. `Agent/fibey-custom` selects the runtime by `runtimeRef`.
3. `Task/fibey-quincy-north-alert` starts a harness turn against the mock endpoint.
4. The task timeline shows `TurnStarted`, `RuntimeOutput`, and `TurnCompleted` events.

This is intentionally **observed mode only**. Orka-owned side-effect tools such as work-order dispatch stay separate until brokered tool/approval mode is implemented.
