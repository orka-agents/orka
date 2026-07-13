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
- **Namespace-local facade**: the `AgentRuntime`, `Agent`, `Task`, and `Tool` objects used by a workflow live in the workflow namespace. TLS endpoints may be external. Insecure HTTP endpoints must name a selector-backed, non-`ExternalName` Service in that same namespace.

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
    transportSecurity: insecure-cluster-local-http
  clientAuth:
    bearerTokenSecretRef:
      name: agentkit-runtime-token
      key: token
  capabilities:
    toolExecutionModes: [observed]
    supportsCancel: true
    supportsRuntimeSessions: true
```

Set `transportSecurity` explicitly for new manifests. An unmarked omission is
treated as `tls`; it never opts a new object into plaintext HTTP. For upgrades,
the supported CRD helper handles schemas that predate the field as well as the
legacy read-time default, publishes the omission-safe target schema, and then
marks only pre-transition stored omissions. The controller backfills marked
HTTPS objects to `tls` or marked HTTP objects to
`insecure-cluster-local-http` only for a validated same-namespace Kubernetes
Service with a non-empty selector. Direct IPs, cross-namespace Services,
selectorless Services, and `ExternalName` Services are rejected. Use the
unambiguous `service.namespace.svc.<cluster-domain>` Service FQDN shown above
(`cluster.local` is the Kubernetes default).

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
    transportSecurity: insecure-cluster-local-http
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
