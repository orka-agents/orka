# Support escalation bring-your-own AgentRuntime demo

This non-Fibey scenario validates the generic bring-your-own agent runtime story in a support escalation domain.

The workflow is intentionally the same shape as the Fibey demo:

```text
Task/support-escalation-demo
  -> Agent/support-remote-investigator
    -> AgentRuntime/support-http-runtime
      -> generic HTTP remote execution backend
        -> Orka-brokered Tool/support-ticket-lookup
```

Security invariant: the remote runtime does not receive support-system credentials. It can request `support-ticket-lookup`; Orka validates policy, executes the Tool CRD, records events, and returns only the brokered result.

## Run

Build/load the generic HTTP fixture image first:

```bash
docker build -t ghcr.io/sozercan/orka-example-echo-harness:latest -f examples/harness/echo/Dockerfile .
kind load docker-image ghcr.io/sozercan/orka-example-echo-harness:latest --name <your-kind-cluster>
```

Apply the demo:

1. Create a per-cluster runtime bearer Secret named `support-http-runtime-token`
   with data key `token`, label `orka.ai/agent-runtime-auth: "true"`, label
   `orka.ai/agent-runtime-name: support-http-runtime`, and annotation
   `orka.ai/agent-runtime-endpoint: http://support-http-runtime.default.svc.cluster.local:8080`.
   Generate the bearer value outside the repository; do not commit it.
2. Apply the demo:

   ```bash
   kubectl apply -k examples/support-escalation-runtime-demo
   kubectl wait --for=condition=Ready agentruntime/support-http-runtime --timeout=60s
   kubectl get task support-escalation-demo -o yaml
   ```

The checked-in `Tool/support-ticket-lookup` points at the included mock `support-tool` service. Replace that service with a real read-only support lookup service for a live demo. The AgentRuntime and Orka task flow remain unchanged when swapping backends or domains.

## Brokered write variant

To exercise approval-gated writes in this domain, add a write-class Tool such as `support-escalate-case`, include it in `Task.spec.agentRuntime.allowedTools`, and set the fixture behavior to `approval-tool`. Orka will emit `ApprovalRequested`, execute the write Tool only after approval, and continue the remote runtime with the approved/declined result.
