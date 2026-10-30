# Policy surfaces agents run into

This page lists each policy an Orka agent can run into. For each one it covers four
things:

- where the policy is enforced;
- what the agent can learn before it acts;
- what the agent sees when the policy refuses an action;
- whether the action could be checked ahead of time.

It describes current behavior. The contract for changing that behavior is
[ADR 0038](../adr/0038-agent-policy-discovery.md).

**Not covered: [agent hosts](../../website/docs/operations/agent-hosts.md).**

- Orka admits and runs an agent host, and pins its template's policy: egress, allowed
  models and approval settings.
- But Orka doesn't mediate the host's turns, so no refusal from inside a host passes
  through Orka.

"Checkable ahead of time" means two things:

- the answer can come from the enforcement point itself, or from the same frozen data it
  uses;
- it does **not** mean Orka performs the check today.

## Summary

| Surface | Enforced by | Known before acting | What the agent sees when refused | Checkable ahead of time |
| --- | --- | --- | --- | --- |
| Brokered tool policy (ACP) | Supervisor MCP proxy, then the controller broker | Partly: `tools/list` gives names, descriptions and input schemas | JSON-RPC error `-32001` "MCP tool call is not authorized" | Yes, exactly, from the frozen tool policy |
| Approval-required brokered tools (ACP, AgentKit and Foundry runtimes) | Controller broker | No; `tools/list` doesn't mark them | An `isError` result with an approval code and plain guidance; the Task shows `WaitingForApproval` | Yes, from the frozen approval policy; the outcome needs a person |
| Broker session checks and supervisor failures: fence, policy digest, operation capability, prompt state, connection binding, transport, deadlines, capacity, argument size | Controller broker and supervisor MCP proxy | No | JSON-RPC error `-32002` "MCP broker call failed" for nearly every cause; `-32003` or HTTP 429 at capacity; `-32600` for a request body over the size limit | No; these describe session state, not policy |
| Provider-native tools (`Read`, `Bash`, `WebFetch` and others) | Runtime session configuration, plus ACP permission requests Orka resolves | Through the runtime's own tool list | Runtime-specific | Yes, from the frozen tool policy |
| Workspace intent and publication | Filesystem permissions for read intent; the controller and the Publisher | Only by reading the Task spec | A filesystem error for a write under read intent; a Task-level failure for publication | Yes, from the frozen execution plan |
| Orka API authorization in built-in tools: namespace scope, API authorization checks including context-token scopes, Task creation limits | Orka API and the tools | No | A tool result with `permission_denied`, `authorization_failed` or `limit_reached`, plus a message and suggestion. `code_exec`'s resource authorization is the exception: an unclassified "resource authorization: …" error. | Yes |
| Per-namespace active-Task cap (`--max-tasks-per-namespace`) | Controller | No | No refusal. A child Task over the cap doesn't start; it's requeued every 10 seconds, with a `NamespaceTaskLimitReached` event. | Yes, from the controller setting and the current count |
| Linked accounts (Connections) used by built-in tools | The tools, through the controller's linked-account resolver | No | Depends on the tool. A bound Connection that can't be used fails the call closed instead of falling back. Through the ACP broker, an unavailable binding is the 502 behind `-32002`. | Partly, from the Task's frozen Connection binding |
| Child transaction-token scopes | Subset check and token exchange when a child Task is created | No | An unclassified tool error | Partly: the subset rule can be checked, but the exchange itself can still refuse |
| Kubernetes RBAC for the identity Orka acts as | API server | No | Through the shared classifier: a tool result with `permission_denied` and "Check RBAC permissions". On some paths: the raw error, unclassified. | Yes: a SubjectAccessReview for that identity |
| Kubernetes admission: ValidatingAdmissionPolicy, MutatingAdmissionPolicy, Gatekeeper, Kyverno, Orka's own policies, and built-in plugins such as ResourceQuota and NamespaceLifecycle | API server | No | Through the shared classifier: a 403 denial (Gatekeeper, built-in plugins) becomes `permission_denied` "Check RBAC permissions"; a ValidatingAdmissionPolicy without a `reason`, or a webhook that sets no status code, becomes `internal_error`. On some paths: the raw error. Warnings are dropped. | Yes: server-side dry-run for the same identity and object |
| Network egress from ACP RuntimePools, Kubernetes `code_exec` Jobs and validation Tasks | Kubernetes NetworkPolicy (default deny for RuntimePools), plus runtime enforcement where installed | No, though the fixed rules are documented | A connection failure or timeout that looks like an outage | Fixed rules: yes. Runtime enforcement policies: no |
| Filesystem layout: read-only root filesystems; private per-session paths and UIDs for ACP children | Pod security context and the ACP supervisor | No | An ordinary filesystem error, such as `EROFS` or `EACCES` | Yes; the layout is fixed |
| Model and provider route (ACP) | The supervisor's per-session provider proxy, checked against the immutable session profile | No | The runtime's provider call fails; what the model sees depends on the runtime | Yes, from the session profile |
| Outbound transaction tokens for HTTP Tool calls (native AI workers) | Token exchange before the call | No | An unclassified tool error ("Error executing tool: …") | Partly, like child scopes |
| OutboundAccessPolicy on an HTTP Tool (ADR 0011) | The outbound access resolver, in the native worker and the ACP broker | No | An unclassified error, "resolve outbound access policy: …": "Error executing tool: …" in native workers, `-32002` through the ACP broker | Partly, from the referenced policy and the Task's frozen Connections |
| Native AI worker tools (`type: ai`) | Each tool's implementation | From the tool list | A structured tool result for most refusals; free text "Error executing tool: …" for Go errors | Depends on the tool |
| Native approval-required tools (`type: ai`) | The worker's approval gate | No | Plain-text tool results, such as "approval requested for X (approvalID Y); parked until a human decides" | Yes, from the Agent's configuration |
| Task-level failures | Controller | No | For ACP prompts, `FailedEvent{stopReason, code, message, retryable}`; for other Task kinds, a Failed Task | Not applicable |

## Details

### Brokered tools (ACP)

**Where the policy comes from.**

- For built-in runtimes, these inputs become an `MCPToolPolicy`
  (`internal/harness/v2/mcp.go`):
  - the Task's `agentRuntime.allowedTools`, `disallowedTools` and `allowBash`;
  - the Agent's `runtime.defaultAllowedTools` and `runtime.defaultAllowBash`.
- `normalizeACPRuntimeToolPolicy` then adjusts the lists for the provider kind and the
  workspace intent.
- Before planning, connector tools are adjusted:
  - write tools are hidden for read-only links, and for runtimes that can't honor
    approvals;
  - visible write tools are added to the approval policy.
- For external AgentRuntimes, the policy comes from the registration's
  `capabilities.mcpPolicy`, and the Task's `allowedTools` must match it exactly.
- The digest is frozen into the RuntimePool profile.
- The supervisor receives the frozen digests as `ORKA_ACP_TOOL_POLICY_DIGEST` and
  `ORKA_ACP_APPROVAL_POLICY_DIGEST`.
- Each tool descriptor carries an `Effect`, either `read_only` or `consequential`. The
  read-only built-ins are listed in `readOnlyBrokeredTools` in
  `internal/controller/acp_mcp_policy.go`.

**What the agent can learn first.**

- `tools/list` (`mcpProxySession.listTools` in `workers/acp/supervisor/mcp_proxy.go`)
  returns each brokered tool's name, description and input schema.
- It doesn't say which tools are read-only or need approval.
- The MCP `initialize` response carries no `instructions`.

**What a refusal looks like.**

- `authorizeCall` refuses a call for one of two reasons:
  - the prompt's authority is inactive;
  - the tool isn't an allowed *brokered* tool. That includes provider-native tools such
    as `Bash`: they can be in the policy, but they can't be called over MCP.
- Both reach the agent as the same JSON-RPC error: `-32001` "MCP tool call is not
  authorized".
- A model can name any MCP tool, so this path is reachable in ordinary sessions.

**Capacity limits.**

- A session with too many calls in flight gets `-32003` "MCP session is at capacity".
- When every slot of the proxy-wide connection limit is taken, the proxy answers HTTP 429
  "MCP proxy is at capacity". That's before any JSON-RPC response exists.
- Tool arguments are limited to 256 KiB (`MaxMCPArgumentsBytes`), and the proxy reads at
  most that plus 64 KiB of request body.
  - Arguments between the two limits fail local validation and get `-32002`.
  - A larger body fails to decode and gets `-32600` "invalid MCP JSON-RPC request", with
    no request ID.

**Broker refusals all look the same.**

- The broker answers with distinct statuses:
  - 400 for a malformed or expired request, or one that doesn't match its pool identity;
  - 401 when pool authentication fails;
  - 410 for a stale fence or policy;
  - 403 for a failed operation capability or an inactive prompt;
  - 409 for an approval call that conflicts with its stored action or a previous call;
  - 502 for an unavailable connection binding or a failed tool;
  - 503 when the broker or approval storage is unavailable, including after an approved
    call that may have run ("the original call must not be repeated").
- Some tool failures come back as 200 `isError` results that the broker shapes itself:
  - `code: invalid_arguments` for a registry argument rejection;
  - `code: tool_execution_failed` for a custom tool's failure or a read-only tool's
    request deadline.
- The supervisor's broker client (`workers/acp/supervisor/mcp_broker_client.go`) collapses
  every non-200 response into one error.
- The proxy then returns `-32002` "MCP broker call failed".
- The same `-32002` covers failures that start in the supervisor:
  - a local digest, validation or signing failure before the request is sent;
  - a broker transport failure;
  - a broker response that fails to decode or validate;
  - the call's deadline expiring;
  - the prompt's authority being revoked while the call is in flight.

**Why this matters to the model.** The MCP specification says clients *should* pass tool
execution errors (`isError`) to the model, but only *may* pass protocol errors. None of
the JSON-RPC errors above is a tool execution error. Only the 200 `isError` results are.

### Approval-required brokered tools

**Which runtimes support them.**

- `MCPApprovalPolicy.requiredTools` lists the tools that need approval.
- Approval-required brokered tools are supported only on AgentKit and Foundry runtimes
  (`runtimeSupportsMCPApprovals`).
- On the built-in runtimes, connector write tools are hidden instead.

**What the agent sees.**

- The broker holds the call until a person decides.
- It then returns a tool result shaped like `{isError, code, approvalID, error}`
  (`internal/controller/acp_mcp_approval.go`).
- The codes are:
  - `approval_declined`;
  - `approval_expired`;
  - `approval_cancelled`;
  - `approval_stale`;
  - `tool_outcome_unknown` ("The tool may have run. Do not repeat it automatically.");
  - `tool_failed_without_effect`.
- Among the refusals the broker produces itself, this is the only one that gives the agent
  a code and retry guidance.
- A built-in tool's own `ChatToolResult` refusal also reaches the agent as an `isError`
  result, carrying its `errorType` and `suggestion`.

### Provider-native tools

**Enforcement.** Each runtime's session configuration enforces policy for its own
built-in tools.

**ACP permission requests.** When a runtime asks permission over ACP, Orka resolves the
request against the frozen policy:

- `codexMCPPermissionToolName` and `canonicalPermissionToolName`, in
  `workers/acp/supervisor/mapper.go`;
- `openCodeBrokeredPermissions`, in `workers/acp/supervisor/env.go`.

**What the model sees.** What the model receives when a permission is rejected depends on
the runtime.

### Workspace intent and publication

**Read intent.** A read-intent workspace isn't writable by the provider child, so a write
fails with an ordinary filesystem error.

**Publication.** The controller validates the Task's publication constraints and applies
them in two places.

- **Delta limits.** These bound the workspace delta Orka collects from the session
  (`acpWorkspaceDeltaLimits` in `internal/controller/acp_dispatcher.go`):
  - `maxChangedFiles`;
  - `allowedPaths`;
  - `denyRepositoryControlPaths`;
  - `rejectBinaryFiles`;
  - `rejectSecretLikeContent`.
- **Publication limits.** These govern clean-room publication itself
  (`internal/controller/acp_publication.go`):
  - `expectedRemoteSHA`;
  - `createPR`.

These come from the frozen Task spec, but the agent isn't told about them. If the delta or
the publication fails, the agent or the caller sees a Task-level failure only after the
turn.

### Orka API authorization in built-in tools

Built-in tools return a `ChatToolResult` (`internal/tools/registry.go`) shaped like
`{success, data, error, errorType, suggestion}`:

| Refusal | `errorType` |
| --- | --- |
| Namespace scope check | `permission_denied` |
| Orka API authorization checks, including context-token scopes (`chatToolAuthorizationError`) | `authorization_failed` |
| Task creation limit for the turn | `limit_reached` |

**The caller's own authorization.** Chat and the compatibility API run tools under the
caller's own authorization (`authorizeExternalToolContext` in
`internal/api/external_tool_authorization.go`).

**`code_exec` resources.** For the Kubernetes `code_exec` backend, that authorization also
covers the temporary resources (`AuthorizeCodeExecResources`). Before creating anything,
the backend checks create and delete permission for every object in the set. A refusal
comes back as an unclassified "resource authorization: …" error.

### Linked accounts

**How Connections are used.** Built-in tools that the controller runs for a person can use
that person's linked account (a Connection) (`LinkedAccountCredentials` in
`internal/tools/registry.go`).

**The rules.**

- A Connection that's bound but can't be used now makes the tool fail closed, instead of
  falling back to its own credentials.
- For GitHub tools, `RequireGitHubTaskCredentials` turns off the controller-wide
  repository and credential fallback.
- `list_connections` is gated by `AuthorizeConnectorRead`.
- Outside a Task, a linked account can be scoped only by a Task this turn created.
- A later linked call refuses a Task whose workspace changed after the turn chose it.

**What the agent sees.** The refusal's shape depends on the tool. Through the ACP broker,
a consequential call whose Connection binding can't be named is refused with a 502 before
anything is reserved, and the agent sees `-32002`.

### Child transaction-token scopes

**The policy.** A child Task's transaction-token scopes must be a subset of its parent's,
and the token exchange fails closed.

**What the agent sees.** The Task creation tools return the subset check failure and any
exchange failure as Go errors, not as classified results. The agent gets an unclassified
tool failure.

### Kubernetes RBAC and admission

**The shared classifier.** Many built-in tools classify Kubernetes errors with
`classifyChatK8sErr` (`internal/tools/chat_helpers.go`):

- Every 403 becomes `permission_denied`, with "Check RBAC permissions".
- Every error the function doesn't recognize becomes `internal_error`.

**Paths that skip it.** Some paths return the raw error instead, and those refusals reach
the agent unclassified:

- **`delegate_task`,** which the ACP broker offers, wraps a failure to create the child
  Task as "failed to create child task: …".
  - A Gatekeeper or ValidatingAdmissionPolicy denial on that create fails the tool.
  - It reaches an ACP agent as `-32002`.
- **`create_container_task`'s coordinator path,** which only native AI workers reach.
  Reading the parent Task and the child transaction checks both return raw errors, which
  the agent sees as "Error executing tool: …". The same tool's chat path uses the shared
  classifier.

**Why admission denials are mislabelled.**

- Gatekeeper's admission webhook denies with 403 Forbidden, so its denials look like RBAC
  failures.
- A ValidatingAdmissionPolicy without a `reason` denies with 422 Invalid, so it becomes
  `internal_error`.
- A webhook chooses its own status code. When it sets none, the API server returns 400, so
  that denial also becomes `internal_error`.
- The API server's message names the source either way:
  - `admission webhook "<name>" denied the request: …`;
  - `ValidatingAdmissionPolicy '<policy>' with binding '<binding>' denied request: …`, or
    the same without the binding. It appears after the usual
    `<resource> "<name>" is forbidden: ` wrapper, and again in `details.causes`.
- Built-in admission plugins also deny with 403, without either prefix. Examples:
  - ResourceQuota ("exceeded quota");
  - NamespaceLifecycle, for a namespace that is being terminated.

  So a quota on Tasks reaches the agent as "Check RBAC permissions" through tools that use
  the shared classifier.
- LimitRange validates only Pods and PersistentVolumeClaims. PodSecurity enforces only on
  Pods, and only warns on a Job. Built-in tools don't create Pods or PVCs directly: `code_exec`
  creates a Job. If the Job's Pod is rejected, the agent sees a Job that can't start, plus
  at most a create-time warning, not a refusal.
- An RBAC denial's message comes from the authorizer: "… cannot `<verb>` resource …".
- Orka's own local authorization refusals are 403s too, such as the compatibility API's
  "not authorized". They carry neither the authorizer's form nor an admission form.

**Orka's own policies without a `reason`.** These guard controller-owned objects, so
agents don't normally reach them:

- `config/admission/gateway_task_protection.yaml`;
- `config/admission/acp_workspace_lease_protection.yaml`;
- `config/admission/namespace_mode_protection.yaml`.

**Warnings** from admission aren't passed back to the agent.

**Checking ahead of time.**

- Orka already uses SubjectAccessReview for several of its own authorization decisions.
- It doesn't use server-side dry-run anywhere.
- A dry run with the identity that will make the request would evaluate the whole
  admission chain without saving anything.

### Network egress

**ACP RuntimePools.**

- Runtime Pods have no Kubernetes RBAC.
- Egress is default-deny, except to the provider auth proxy and the controller's MCP
  broker. See the [security model](../../website/docs/concepts/security.md).

**Runtime enforcement.** Where it's installed, runtime enforcement such as Gatekeeper
Runtime can also block connections, file access or programs inside the container.

**What a block looks like.** A blocked connection looks the same as an outage: a
connection failure or a timeout.

**Native and container workers.** Ordinary native and container worker Pods get no
NetworkPolicy by default. Two kinds of Pod get one at runtime:

- the Jobs the Kubernetes `code_exec` backend creates, unless it's turned off for the
  scope (`buildKubernetesCodeExecNetworkPolicy` in
  `internal/tools/code_exec_kubernetes.go`);
- repository-monitor validation Tasks
  (`internal/controller/repositorymonitor_validation_runtime.go`).

A connection those block also looks like an outage.

### Fixed runtime controls

These come from the [security model](../../website/docs/concepts/security.md). An agent
can hit them, but nothing describes them to it.

**Filesystems.**

- Native workers and ACP runtime Pods run with a read-only root filesystem.
- Native workers can write only under `/tmp`, `/home/worker` and `/workspace`.
- ACP children run under distinct, non-reused UIDs, with private `HOME`, `TMPDIR`, XDG and
  workspace paths.
- A write anywhere else fails with an ordinary filesystem error.

**Model and provider route.**

- In ACP, the supervisor's per-session provider proxy checks every provider route and the
  model against the immutable session profile.
- A request for a different model or route is refused.
- The runtime sees its provider call fail, and what the model sees depends on the runtime.

**Outbound transaction tokens.**

- When a native AI worker calls an HTTP Tool, it can first exchange its subject token for
  an operation-scoped transaction token.
- A refused exchange is returned as a Go error, so the agent sees "Error executing
  tool: …".

**Outbound access policies.**

- An HTTP Tool can reference an OutboundAccessPolicy (`spec.http.outboundAccessPolicyRef`).
  See [ADR 0011](../adr/0011-vendor-neutral-transaction-and-outbound-access.md).
- The native worker (`applyOutboundAccessPolicy` in `internal/worker/tool_executor.go`)
  and the ACP broker resolve that policy before the call. The policy can refuse it, or
  require credential injection over HTTPS.
- A refusal is returned as a Go error ("resolve outbound access policy: …"):
  - "Error executing tool: …" in a native worker;
  - `-32002` through the ACP broker.

### Native AI workers

**Built-in tools.**

- Most built-in tools return a `ChatToolResult`.
- When a tool returns a Go error, the worker sends the model the free text "Error
  executing tool: …" (`workers/ai/main.go`).

**Approval-required tools.**

- The worker's approval gate (`workers/ai/approval_gate.go`) enforces the tools an Agent's
  coordination settings mark as approval-required.
- It answers with plain-text tool results. Examples:
  - "approval requested for X (approvalID Y); parked until a human decides";
  - "Not executed because … request approval again".

### Task-level outcomes

**Failures.** A failed execution records `FailedEvent{stopReason, code, message,
retryable}` (`internal/harness/v2/event_types.go`).

**Waiting for approval.** A Task waiting for a person shows the `WaitingForApproval`
condition, which isn't a failure.
