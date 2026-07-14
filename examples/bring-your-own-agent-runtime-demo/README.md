# Bring-your-own AgentRuntime demo

This package is the canonical entry point for the provider-neutral remote-runtime demo. It points at the runnable manifests used by the current repo-owned implementation and keeps the story independent of any one backend.

```text
Orka API + governance plane
  -> namespace-local AgentRuntime facade
    -> remote execution backend adapter/runtime
      -> Orka-brokered Tool CRDs
```

## What is runnable from this repository

| Scenario | Path | Credentials | Brokered tools |
| --- | --- | --- | --- |
| Fibey observed + backend switch facades | `../fibey-custom-agent-demo` | Harness bearer token only for generic HTTP; adapter credentials for optional backends | Optional; default observed |
| Support escalation brokered read | `../support-escalation-runtime-demo` | Harness bearer token only | `support-ticket-lookup` read tool |

AgentKit Serve adapter changes are intentionally not in this repository. Foundry/AgentKit samples are namespace-local facades that point at operator-provided adapter Services.

## Run the generic HTTP demo

Build and load the reference generic HTTP harness fixture:

```bash
docker build -t ghcr.io/orka-agents/orka/example-echo-harness:latest -f examples/harness/echo/Dockerfile .
kind load docker-image ghcr.io/orka-agents/orka/example-echo-harness:latest --name <kind-cluster>
```

Create the runtime bearer Secret out of band; do not commit real values. The Secret must contain a data key named `token`, must be labeled `orka.ai/agent-runtime-auth=true`, and must be annotated with the exact runtime endpoint:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: support-http-runtime-token
  labels:
    orka.ai/agent-runtime-auth: "true"
    orka.ai/agent-runtime-name: support-http-runtime
  annotations:
    orka.ai/agent-runtime-endpoint: http://support-http-runtime.default.svc.cluster.local:8080
stringData:
  token: <generated-placeholder>
```

Apply and run:

```bash
kubectl apply -k examples/support-escalation-runtime-demo
kubectl wait --for=condition=Ready agentruntime/support-http-runtime --timeout=60s
kubectl get task support-escalation-demo -o yaml
```

Expected flow:

1. `AgentRuntime/support-http-runtime` explicitly opts into cluster-local HTTP, validates the same-namespace selector-backed Service, then probes `/v1/health`, `/v1/capabilities`, observed turn conformance, and the advertised brokered read profile.
2. `Task/support-escalation-demo` starts a remote turn through the namespace-local facade.
3. The runtime emits `ToolCallRequested` for `support-ticket-lookup`.
4. Orka validates `Task.spec.agentRuntime.allowedTools`, loads the same-namespace `Tool`, resolves any downstream credentials inside Orka, executes it, records brokered events, and returns a `ToolCallResult` via `/v1/turns/{turnID}/continue`.
5. The runtime completes and Orka stores the result.

## Approval-gated write variant

To exercise a write tool, add a `Tool` with `spec.brokeredToolClass: write`, include it in `Task.spec.agentRuntime.allowedTools`, and run the generic fixture with:

```yaml
env:
- name: ORKA_REMOTE_HTTP_RUNTIME_BEHAVIOR
  value: approval-tool
- name: ORKA_REMOTE_HTTP_RUNTIME_WRITE_TOOL_NAME
  value: <write-tool-name>
```

The expected UX is unchanged across backends:

```bash
orka task approvals <task>
orka task approve <task> <approval-id>
orka task result <task>
```

Orka, not the remote runtime, creates the canonical `ApprovalRequested` event and executes the approved tool exactly once unless a prior execution ledger entry has an unknown outcome, in which case it fails closed instead of replaying a consequential side effect.

## Backend switching

Use `../fibey-custom-agent-demo/switch-backend.sh` to patch only `Task.spec.agentRef.name` between namespace-local facades:

```bash
examples/fibey-custom-agent-demo/switch-backend.sh http
examples/fibey-custom-agent-demo/switch-backend.sh foundry
```

The workflow, Tool CRDs, approval UX, and task/result APIs remain Orka-owned. Remote backends receive safe tool schemas and scoped turn metadata only; they do not receive downstream Tool credentials.

## Troubleshooting

| Symptom | Likely cause | Fix |
| --- | --- | --- |
| `AgentRuntime` Ready=False mentioning `orka.ai/agent-runtime-auth` | bearer Secret missing opt-in label | add `orka.ai/agent-runtime-auth: "true"` |
| Ready=False mentions `transportSecurity` or HTTPS | an HTTP endpoint omitted the explicit cluster-local opt-in, used a cross-namespace/external target, or points at an unsafe Service | use HTTPS, or set `insecure-cluster-local-http` only for a same-namespace selector-backed non-`ExternalName` Service |
| Ready=False endpoint binding error | Secret annotation does not match `spec.deployment.endpoint` | update `orka.ai/agent-runtime-endpoint` |
| Ready=False brokered class missing | runtime did not advertise a required `brokeredToolClasses` value | fix adapter capabilities or narrow `spec.capabilities` |
| Task fails `tool not allowed` | remote requested a tool not in `Task.spec.agentRuntime.allowedTools` | add the intended tool or reject the backend behavior |
| Task waits for approval | write tool requested and no human decision exists | use `orka task approvals` then approve/decline |
| Outcome unknown for write tool | controller saw a pre-execution ledger entry without terminal result | inspect downstream idempotency target; do not blindly replay |
| Result data truncated in parent summary | structured data exceeded `wait_for_tasks` bounds | store large payloads as artifacts and pass references |
