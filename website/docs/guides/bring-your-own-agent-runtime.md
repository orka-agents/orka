# Bring your own AgentRuntime

Orka can run `type: agent` tasks through a namespace-local `AgentRuntime` facade while Orka remains the API, lifecycle, approval, and tool-governance plane.

```text
Orka Task
  -> Agent.runtime.runtimeRef
    -> AgentRuntime facade
      -> remote execution backend
        -> ToolCallRequested
          -> Orka validates, approves, executes, audits
          -> /v1/turns/{turnID}/continue with ToolCallResult
```

## Core concepts

- **AgentRuntime**: the Orka-facing CRD and protocol contract.
- **Remote execution backend**: the workload runtime or adapter behind the facade.
- **Brokered governance**: remote runtimes request tools; Orka authorizes and executes them.
- **Namespace-local facade**: the `AgentRuntime`, `Agent`, `Task`, and `Tool` objects used by a workflow live in the workflow namespace. The endpoint may route to a Service elsewhere.

## Minimal runtime facade

For an observed-only backend such as current AgentKit Serve Orka mode, advertise
only observed capabilities:

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: AgentRuntime
metadata:
  name: agentkit-runtime
spec:
  contractVersion: orka.harness.v1
  deployment:
    mode: external-endpoint
    endpoint: http://agentkit-runtime.default.svc.cluster.local:8080
  clientAuth:
    bearerTokenSecretRef:
      name: agentkit-runtime-token
      key: token
  capabilities:
    toolExecutionModes: [observed]
    supportsCancel: true
    supportsRuntimeSessions: true
```

The checked-in AgentKit Serve facades report lifecycle/output frames for
AgentKit-owned observed runs. Do not add `brokeredToolClasses` or
`supportsContinuation` to an AgentKit facade unless that deployment enables an
AgentKit brokered profile that has passed the matching Orka conformance probe.

For a backend that has already passed a brokered profile, include only the
profile it actually supports:

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: AgentRuntime
metadata:
  name: support-http-runtime
spec:
  contractVersion: orka.harness.v1
  deployment:
    mode: external-endpoint
    endpoint: http://support-http-runtime.default.svc.cluster.local:8080
  clientAuth:
    bearerTokenSecretRef:
      name: support-http-runtime-token
      key: token
  capabilities:
    toolExecutionModes: [observed, brokered]
    brokeredToolClasses: [read]
    supportsRuntimeSessions: true
    supportsContinuation: true
```

The bearer Secret must be labeled and endpoint-bound:

```yaml
metadata:
  labels:
    orka.ai/agent-runtime-auth: "true"
    orka.ai/agent-runtime-name: support-http-runtime
  annotations:
    orka.ai/agent-runtime-endpoint: http://support-http-runtime.default.svc.cluster.local:8080
```


## Foundry hosted AgentKit over Responses

For AgentKit agents deployed as Foundry hosted agents, use the `examples/harness/foundry-responses` adapter rather than the Assistants/threads adapter. Hosted Responses requests are endpoint-scoped and must not include request-level `tools`; AgentKit must be statically configured with the safe function schemas it may request. Orka still validates every `function_call` against Task policy and Tool CRDs, performs approval/idempotency, executes the tool, and resumes the hosted response with `function_call_output` plus `previous_response_id`.

Advertise only the brokered classes that the hosted AgentKit deployment is statically configured and conformance-tested to request:

Readiness deep-probes each advertised class. Since hosted Responses requests do not carry request-level tools, the AgentKit deployment must also statically expose the probe-only `conformance_read` and/or `conformance_write` schema for those classes. Leave a class unadvertised until that live probe succeeds; local fake-server conformance alone does not satisfy the readiness gate.

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: AgentRuntime
metadata:
  name: foundry-agentkit-responses
spec:
  contractVersion: orka.harness.v1
  deployment:
    mode: external-endpoint
    endpoint: http://foundry-agentkit-responses.default.svc.cluster.local:8080
  clientAuth:
    bearerTokenSecretRef:
      name: foundry-agentkit-responses-token
      key: token
  capabilities:
    toolExecutionModes: [observed, brokered]
    brokeredToolClasses: [read]
    supportsRuntimeSessions: true
    supportsContinuation: true
```

Add `write` only after the hosted AgentKit static write schema and brokered-write conformance pass. The adapter's MVP state is in-memory and fail-safe: duplicate identical continuations are no-ops, conflicting duplicates are rejected, and a restart while waiting for approval returns `turn not found` without sending a hosted continuation.

## Expose a brokered tool

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Tool
metadata:
  name: support-ticket-lookup
spec:
  description: Look up sanitized support ticket evidence.
  brokeredToolClass: read
  parameters:
    type: object
    properties:
      incident:
        type: string
  http:
    url: http://support-tool.default.svc.cluster.local:8080/lookup
    method: POST
```

Then allow it on the Task:

```yaml
spec:
  type: agent
  agentRef:
    name: support-remote-investigator
  agentRuntime:
    allowedTools:
    - support-ticket-lookup
```

Orka sends the remote runtime only the safe schema view. It does not send the HTTP URL or auth Secret refs.

## Approval-gated writes

Use `spec.brokeredToolClass: write` for consequential tools. Orka records `ApprovalRequested`, parks the task with `WaitingForApproval=True`, and resumes the runtime after a human decision:

```bash
orka task approvals support-escalation-demo
orka task approve support-escalation-demo <approval-id>
```

If a brokered write has an unresolved pre-execution ledger entry, Orka returns an outcome-unknown error instead of replaying the write.

## Demos

- `examples/support-escalation-runtime-demo`: brokered read path with no external provider credentials.
- `examples/fibey-custom-agent-demo`: namespace-local backend switching facades.
- `examples/bring-your-own-agent-runtime-demo`: canonical README and security notes.
