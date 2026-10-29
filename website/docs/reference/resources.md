---
slug: /resources
description: "Every Orka custom resource you write, field by field: Tasks, Agents, Providers, Tools, and the rest."
---

# Resource reference

This page documents the custom resources you create to use Orka. Controller flags and Helm
values are in [Configuration](configuration.md), and HTTP endpoints are in the
[API reference](api-reference.md).

| Resource | Group | What it is |
| --- | --- | --- |
| [Task](#task) | `core.orka.ai` | One unit of work: a container command, a native AI prompt, or a coding-agent session. |
| [Agent](#agent) | `core.orka.ai` | Reusable configuration: model, prompt, tools, skills, coordination, and optionally a [runtime](#agent-with-runtime). |
| [Provider](#provider) | `core.orka.ai` | A model backend and the Secret that holds its API key. |
| [Skill](#skill) | `core.orka.ai` | Reusable instructions injected into an Agent's system prompt. |
| [Tool](#tool) | `core.orka.ai` | A custom tool backed by an HTTP endpoint or an MCP server. |
| [OutboundAccessPolicy](#outboundaccesspolicy) | `core.orka.ai` | How Tools reach an external API without handing agents the credential. |
| [ConnectorProvider](#connectorprovider) and [Connection](#connection) | `core.orka.ai` | An OAuth service people can link, and one person's link to it. |
| [RepositoryScan](#repositoryscan) | `core.orka.ai` | A repository to scan for security findings. |
| [RepositoryMonitor](#repositorymonitor) | `core.orka.ai` | A repository whose pull requests and issues agents review, implement, and repair. |
| [SubstrateActorPool](#substrateactorpool) | `core.orka.ai` | A pool of Agent Substrate actors for MCP Tools. |
| AgentHostTemplate and AgentHost | `core.orka.ai` | An operator's definition of a long-lived, single-user agent host, and one user's host from it. See [Agent hosts](../operations/agent-hosts.md). |
| AgentRuntime | `core.orka.ai` | An external agent runtime registration. See [Bring your own agent runtime](../guides/bring-your-own-agent-runtime.md). |
| GatewayClass, Gateway, GatewayBinding | `gateway.orka.ai` | Connections to chat apps and other agents. See [Gateways](../guides/gateways.md) and the [Gateway API](gateway-api.md). |
| ExecutionWorkspaceClass and related resources | `workspace.orka.ai` | Sandboxed execution workspaces from separately installed providers. See [Workspace providers](configuration.md#workspace-providers). |

The controller creates and owns several more resources, such as RuntimePools and
PromptAttempts. Read them when debugging, but do not edit them. [Architecture](../concepts/architecture.md#custom-resources)
lists them.

## Task

One unit of work: a container command, a native AI prompt, or a coding-agent session. The
example shows most fields at once; a real Task uses a handful.

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Task
metadata:
  name: my-task
spec:
  type: ai  # or "container" or "agent"
  agentRef:
    name: my-agent
  prompt: "Analyze the latest Kubernetes security best practices"
  execution:
    runtimeClassName: gvisor
    nodeSelector:
      sandbox-runtime: gvisor
  sessionRef:
    name: my-session
    create: false  # default: false
    append: true
    maxMessages: 50
  priority: 500
  timeout: 5m
  retryPolicy:
    maxRetries: 3
    backoffMultiplier: 2
    initialDelay: 10s
  webhookURL: "https://example.com/webhook"
  # Scheduled/recurring task fields (optional)
  schedule: "0 */6 * * *"      # Cron expression
  timeZone: "America/New_York" # IANA timezone
  concurrencyPolicy: Forbid    # Allow or Forbid concurrent runs
  startingDeadlineSeconds: 100  # Deadline for starting missed scheduled runs (default: 100)
  suspend: false
  successfulRunsHistoryLimit: 3
  failedRunsHistoryLimit: 1
```

| Field | Task types | Description |
| --- | --- | --- |
| `type` | all | `ai`, `agent`, or `container`. Required. |
| `agentRef` | `ai`, `agent` | The [Agent](#agent) to use. |
| `prompt` | `ai`, `agent` | What to do. |
| `image`, `command`, `args` | `container` | The container to run. See [Container tasks](../guides/container-tasks.md). |
| `env` | `ai`, `container` | Environment variables for the worker container. Coding-agent pools do not inherit them. |
| `ai` | `ai` | Settings for a Task without an Agent, or overrides for one: `providerRef`, `model`, `systemPrompt`, `temperature`, `maxTokens`, `skills`, and `tools`. |
| `workspace` | `agent`, `container` | The git repository to work in. See [Workspace](#workspace). |
| `agentRuntime` | `agent` | Per-Task overrides of the Agent's runtime defaults: `maxTurns`, `allowedTools`, `disallowedTools`, and `allowBash`. |
| `sessionRef` | `ai`, `agent` | Conversation continuity: `name`, `create` (default `false`), `append` (default `true`), and `maxMessages` (default `50`). Immutable after creation. |
| `timeout` | all | Maximum wall-clock duration. Coding-agent Tasks default to `30m`, measured from Task creation, including queueing and runtime admission. |
| `priority` | all | Queue priority from 0 to 1000, default `500`. Higher runs first. |
| `retryPolicy` | all | `maxRetries` (default `0`), `backoffMultiplier` (default `2`), and `initialDelay`. |
| `webhookURL` | all | URL called when the Task completes. |
| `resources` | `ai`, `container` | CPU and memory requests and limits for the worker Pod. Coding-agent Tasks reject custom resources; their pool profile sets them. |
| `execution` | `ai`, `container`; `workspace` only for `agent` | Runtime class and placement. See [Execution](#execution). |
| `secretRef` | `ai`, `container` | A Secret for the Task. Container Tasks do not receive it by default; see [Container tasks](../guides/container-tasks.md#other-fields). |
| `schedule`, `timeZone`, `concurrencyPolicy`, `startingDeadlineSeconds`, `successfulRunsHistoryLimit`, `failedRunsHistoryLimit` | all | Turn the Task into a cron template. See [Scheduled tasks](../guides/scheduled-tasks.md). |
| `suspend` | all | Pauses a schedule or an autonomous coordinator without deleting it. |
| `priorTaskRef` | `ai`, `container` | Apply a completed Task's diff before starting. Set by the `prior_task` tool parameter; coding-agent runtimes do not accept it. |
| `requestedBy`, `transaction` | all | The verified caller and transaction metadata. The API server sets them; clients cannot. |

### Workspace

For `type: agent`, repository and delivery policy belong at top-level `spec.workspace`:

```yaml
workspace:
  intent: write
  gitRepo: https://github.com/example/project.git
  branch: main
  readCredentialRef:
    name: project-source-read
  publicationGitRepo: https://github.com/example/project.git
  publicationReadCredentialRef:
    name: project-target-read
  publicationCredentialRef:
    name: project-target-write
  forgeCredentialRef:
    name: project-forge
  pushBranch: orka/example-change
  prBaseBranch: main
  createPR: true
```

| Field | Type | Default | Description |
| --- | --- | --- | --- |
| `intent` | string | `read` for agent Tasks | `read` or `write`. Immutable for an active attempt. |
| `gitRepo` | string | empty | Credential-free source repository URL. Embedded credentials, query strings, and fragments are rejected. |
| `sourceRepository` | object | empty | Optional canonical provider/ID source identity. |
| `branch` / `ref` | string | empty | Source branch or exact ref/commit/tag. When both are empty, the Publisher resolves and freezes the repository's advertised default branch before execution. |
| `readCredentialRef.name` | string | empty | Secret used only by the clean-room source clone/read operation. |
| `publicationGitRepo` | string | empty | Credential-free publication repository URL. |
| `publicationRepository` | object | empty | Optional canonical provider/ID publication identity. |
| `publicationReadCredentialRef.name` | string | empty | Target-read Secret used only for publication preflight and independent verification. |
| `publicationCredentialRef.name` | string | empty | Target-write Secret used only for the exact branch compare-and-swap push. |
| `forgeCredentialRef.name` | string | empty | Forge API Secret used only for pull-request reconciliation; required when `createPR` is true. |
| `subPath` | string | empty | Repository subdirectory exposed as the workspace root. |
| `pushBranch` | string | generated for write Tasks | Publication branch. Orka-generated names use full Task or Session identity entropy. |
| `prBaseBranch` | string | empty | Pull-request base branch. |
| `prTitle` | string | prompt's first nonblank line | Exact pull-request title, up to 256 characters. Empty uses the default, which trims whitespace and truncates to 256 characters. An empty or whitespace-only prompt uses `Orka publication generation N`. Nonempty whitespace-only titles are rejected. |
| `prBody` | string | publisher summary and Task namespace/name | Pull-request body, up to 32,768 characters. The publisher appends the publication generation and reconciliation markers to custom and default bodies. Reserved Orka reconciliation comments are rejected. |
| `createPR` | boolean | `false` | Reconcile a pull request only after verified branch publication. Requires `intent: write`. |

The four credential references are distinct roles: `readCredentialRef` is source-read only,
`publicationReadCredentialRef` is target preflight and verification only,
`publicationCredentialRef` is target-write only, and `forgeCredentialRef` is
pull-request reconciliation only. All four are same-namespace Secret references. The
controller freezes each selected Secret version for the attempt, and the credential broker
releases it only to the Workspace/Publisher for the exact operation. None enters the coding
agent's process tree or Task status.

`prTitle` and `prBody` apply only when `createPR: true`; supplying them alone does not
request a pull request. Orka rejects secret-like titles and bodies before publication,
including prompt-derived titles. Explicit overrides wait for a publisher with pull-request
presentation support before prompt admission.

The durable attempt is reported in `status.execution`, and workspace validation and
publication in `status.delivery`: publication ID, repository identities, branch, starting,
remote, tree, and commit SHAs, artifact digest, and optional pull request receipt. A Task is
not delivered merely because the model reports success; require a terminal verified delivery
outcome. [Agent runtimes](../concepts/agent-runtimes.md#delivery-status) lists the outcomes.

### Execution

Native `ai` and container Tasks support `spec.execution` for worker Pod runtime selection
and placement. Built-in ACP agent Tasks do not accept per-Task placement or custom
resources; reviewed RuntimePool profiles own those settings. They can request an
execution workspace through `spec.execution.workspace` when the dispatch and provider
gates are enabled. See [Execution workspace requests](#execution-workspace-requests).

```yaml
execution:
  runtimeClassName: gvisor
  nodeSelector:
    sandbox-runtime: gvisor
  tolerations:
    - key: sandbox-runtime
      operator: Equal
      value: gvisor
      effect: NoSchedule
  affinity:
    nodeAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        nodeSelectorTerms:
          - matchExpressions:
              - key: sandbox-runtime
                operator: In
                values: ["gvisor"]
```

| Field | Type | Description |
|-------|------|-------------|
| `runtimeClassName` | string | Selects a Kubernetes `RuntimeClass` such as `gvisor` or `kata-qemu` |
| `nodeSelector` | map[string]string | Restricts native worker Pods to nodes with matching labels |
| `tolerations` | list | Allows native worker Pods onto tainted runtime-specific node pools |
| `affinity` | object | Adds Kubernetes affinity or anti-affinity rules for native worker Pods |
| `workspace` | object | Class-only execution-workspace request. `classRef` selects an admitted external provider and immutable profile for a dedicated ACP RuntimeSession. Repository access uses top-level `Task.spec.workspace`. |

Resolution order:

- `Agent.spec.execution` provides defaults for tasks that reference the Agent
- `Task.spec.execution` overrides Agent defaults
- `runtimeClassName` is a scalar override
- `nodeSelector`, `tolerations`, and `affinity` replace Agent defaults when they are set on the Task

#### Execution workspace requests

`Task.spec.execution.workspace.classRef` selects an administrator-managed class for
a dedicated workspace-backed ACP RuntimeSession. Dispatch requires the workspace
provider API, `--acp-workspace-dispatch-enabled`, required admission webhooks, live
class-use authorization, and an admitted external provider with the required
contracts and capabilities. Optional `reusePolicy`, `workspaceSlot`, and `onDetach`
must satisfy the frozen class lifecycle. Direct provider, template, pool, boot,
snapshot, and cleanup selectors are removed. Rejections appear in
`Task.status.executionWorkspace` before compute demand. Top-level
`Task.spec.workspace` remains the verified source/publication contract.

See [Agent Sandbox Workspaces](../concepts/agent-sandbox.md), [Agent Substrate Workspaces](../concepts/substrate.md), and ADRs 0024/0025 for the provider-neutral contract.

## Agent

Reusable configuration that Tasks point at. An Agent is either **native** — it names a
Provider and model and runs on Orka's AI worker — or a **runtime** Agent that runs a
coding-agent CLI. A runtime Agent sets `spec.runtime` and must not set `providerRef`; see
[Agent with runtime](#agent-with-runtime). Fields marked "native Agents only" make a runtime
Agent's Tasks fail before a session starts.

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Agent
metadata:
  name: researcher-agent
spec:
  providerRef:
    name: anthropic-prod
  execution:
    runtimeClassName: kata-qemu
    nodeSelector:
      sandbox-runtime: kata
  model:
    temperature: 0.7
    maxTokens: 4096
  systemPrompt:
    inline: "You are a research specialist..."
  tools:
    - name: web_search       # a built-in tool
    - name: catalog-search   # a custom Tool resource
  skills:
    - name: skill-researcher
  session:
    maxMessages: 50
  coordination:
    enabled: true
    autonomous: true          # Enable autonomous loop mode
    maxIterations: 20         # Max loop iterations (0 = unlimited)
    allowedAgents:
      - name: coder-agent
    maxConcurrentChildren: 5
    maxDepth: 3
```

| Field | Description |
| --- | --- |
| `providerRef` | The [Provider](#provider) for a native Agent. Mutually exclusive with `runtime`. |
| `model` | `name` (optional when the Provider sets `defaultModel`), `temperature` (0–2), `maxTokens`, `contextWindow`, and [`fallbacks`](#model-fallbacks). |
| `systemPrompt` | `inline` text, or `configMapRef` to read it from a ConfigMap key. |
| `tools` | Native Agents only. Tools the Agent may call: [built-in tools](built-in-tools.md) and custom [Tool](#tool) resources by name. An entry with `enabled: false` is skipped. Runtime Agents use `runtime.defaultAllowedTools` instead. |
| `skills` | Native Agents only. [Skills](#skill) whose content is added to the system prompt. |
| `coordination` | Native Agents only. Lets the Agent delegate to other Agents. See [Coordination](#coordination). |
| `runtime` | Makes this a coding-agent Agent. See [Agent with runtime](#agent-with-runtime). |
| `execution` | Native Agents only. Default runtime class and placement for the Agent's worker Pods. See [Execution](#execution). |
| `resources` | Native Agents only. Default CPU and memory for the Agent's worker Pods. |
| `session` | Session defaults, such as `maxMessages`. |
| `secretRef` | A Secret injected into native worker Pods as environment variables. Runtime Agents must not set it. |
| `ttlAfterLastTask` | Delete the Agent this long after its last Task completes. Unset keeps it. |

### Coordination

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | `false` | Enable agent-to-agent coordination tools |
| `autonomous` | bool | `false` | Enables autonomous loop mode. When true, the controller re-creates Jobs in a loop instead of marking the task as Succeeded |
| `maxIterations` | int32 | `0` | Limits the number of autonomous loop iterations. Only used when `autonomous` is true. `0` means unlimited |
| `approvalRequiredTools` | list | `[]` | Custom Tool CRD names that require human approval before execution in enabled autonomous coordination mode. Built-in tools, including `request_approval`, are rejected |
| `allowedAgents` | list | `[]` | List of agent names this agent is allowed to delegate to |
| `maxConcurrentChildren` | int32 | `5` | Maximum number of concurrent child tasks |
| `maxDepth` | int32 | `3` | Maximum delegation depth |

Enabling coordination adds Orka's coordination, memory, GitHub pull request, agent
management, and planning tools to the Agent. [Built-in tools](built-in-tools.md#who-gets-which-tools)
lists exactly which tools, and [Multi-agent coordination](../guides/multi-agent-coordination.md)
shows how to use them.

### Model fallbacks

Fallback providers are tried automatically when the primary provider fails, for example on
an authentication error, an outage, or rate limiting. Configure them in `spec.model.fallbacks`:

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Agent
metadata:
  name: resilient-agent
spec:
  providerRef:
    name: my-openai
  model:
    name: gpt-6-astra
    fallbacks:
      - providerRef: my-anthropic
        model: claude-opus-5
      - providerRef: my-azure-openai
        model: gpt-6-astra
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `providerRef` | string | Yes | Name of a Provider CRD to fall back to |
| `model` | string | No | Model to use with this provider. If empty, uses the provider's `defaultModel` |

How fallbacks work:

1. The primary provider is tried first with automatic retries (exponential backoff on 429/5xx errors).
2. If the primary provider fails with an auth error (401/403), network error, or exhausts all retries, the first fallback provider is tried.
3. Each fallback provider also gets automatic retries.
4. If all providers fail, the last error is returned.

Fallbacks are only supported on Agent-based tasks; Agent-less tasks get retries only. Each
fallback provider needs its own Provider with a valid Secret reference. Rate-limited
providers (429 responses) are temporarily cooled down and skipped in subsequent requests.
Streaming requests fail over only on the initial connection; mid-stream failures are not
retried.

## Agent with runtime

Agent configuration for the supported built-in coding-agent runtimes: Claude, Codex,
Copilot, and OpenCode. Runtime Agents do not reference provider Secrets; their pools reach
your model gateway through Orka's authenticated [provider proxy](../operations/provider-proxy.md).

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Agent
metadata:
  name: claude-agent
spec:
  model:
    name: claude-opus-5
  systemPrompt:
    inline: "You are a senior software engineer."
  runtime:
    type: claude         # or "codex" / "copilot" / "opencode"
    contractVersion: orka.harness.v2
    defaultMaxTurns: 50
    defaultAllowBash: true
    defaultAllowedTools:
      - Read
      - Write
      - Edit
      - Bash
```

| Field | Description |
| --- | --- |
| `model.name` | Required. A model your gateway lists. |
| `runtime.type` | `codex`, `claude`, `copilot`, or `opencode`. |
| `runtime.contractVersion` | `orka.harness.v2`. |
| `runtime.defaultMaxTurns` | Default turn limit per Task, 1–1000. |
| `runtime.defaultAllowedTools` | Tools the CLI may use by default, such as `Read`, `Grep`, `Glob`, `Edit`, and `Bash`. |
| `runtime.defaultAllowBash` | Whether shell use is allowed by default. Defaults to `true`. Codex requires `true`. |
| `runtime.defaultReasoningEffort` | `none`, `low`, `medium`, `high`, `xhigh`, or `max`. Runtimes reject values they do not support: `none` is Codex-only and `max` is Claude-only. |
| `runtime.runtimeRef` | Selects an external [AgentRuntime](../guides/bring-your-own-agent-runtime.md) instead of a built-in type. |
| `runtime.toolboxes` | Built-in types only. Read-only [toolbox](../guides/toolboxes.md) images whose tool folders are added to the agent's `PATH`. |

All four built-in harness v2 runtimes support `spec.systemPrompt` and the optional
[`spec.soul`](../concepts/soul.md) persona source. Copilot and OpenCode receive
protected native instruction files outside the repository workspace. OpenCode
model IDs use literal `provider/model` form, for example `openai/gpt-6-astra`, and
`spec.model.contextWindow` and `spec.model.maxTokens` are both required positive reviewed
ceilings, with `contextWindow > maxTokens`. They are part of the immutable runtime
profile; Orka does not discover or guess them from a mutable model catalog.

Operator-owned runtimes outside the built-in set can use `orka.harness.v2`
`AgentRuntime` registration and conformance. A current-generation ready,
strict-governed registration can be selected through `runtime.runtimeRef`;
Orka freezes and revalidates its endpoint, profile, authentication authority,
and observed runtime identity for dispatch.

A Task that uses a runtime Agent sets `type: agent`, a [workspace](#workspace), and
optional `agentRuntime` overrides:

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Task
metadata:
  name: code-review
spec:
  type: agent
  agentRef:
    name: claude-agent
  prompt: "Review the code in this repo for security issues. Do not modify files."
  workspace:
    intent: read
    gitRepo: "https://github.com/example/repo.git"
    branch: main
    # readCredentialRef:
    #   name: repository-read
    # subPath: "services/api"
  agentRuntime:
    maxTurns: 100
    allowBash: true
    allowedTools:
      - Read
      - Write
      - Edit
      - Bash
      - Glob
      - Grep
```

[Agent runtimes](../concepts/agent-runtimes.md) explains how these Tasks run.

## Provider

A model backend and the Secret that holds its API key. Native AI Agents, chat, and the
compatibility APIs use Providers. Coding-agent runtimes do not; they reach models through the
[provider proxy](../operations/provider-proxy.md).

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Provider
metadata:
  name: anthropic-prod
spec:
  type: anthropic  # or "openai", "azure-openai"
  secretRef:
    name: anthropic-secret
    key: api-key
  baseURL: ""  # optional custom endpoint for proxies
  defaultModel: claude-opus-5
  # Azure-specific (only for type: azure-openai)
  # azure:
  #   deploymentName: my-deployment
  #   apiVersion: "2024-02-15-preview"
```

| Field | Default | Description |
| --- | --- | --- |
| `type` | required | `anthropic`, `openai`, or `azure-openai`. Use `anthropic` or `openai` with a `baseURL` for any compatible endpoint. |
| `secretRef.name` / `secretRef.key` | key `api-key` | The Secret and key holding the API key. Required, even for an endpoint that ignores it. |
| `baseURL` | provider default | A custom endpoint, such as a gateway or an in-cluster model server. |
| `defaultModel` | empty | Model used when the Agent or Task does not name one. |
| `azure.deploymentName` | required for Azure | The Azure OpenAI deployment. |
| `azure.apiVersion` | `2024-02-15-preview` | The Azure OpenAI API version. |

For `type: openai`, Orka tries the Responses API first and switches to Chat Completions when
the endpoint does not support it.

## Skill

Reusable instructions in the Agent Skills format. Skills listed on
an Agent or AI Task are added to its system prompt on every model call, so keep them short.
`orka skill init`, `validate`, and `import` create Skills from `SKILL.md` files.

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Skill
metadata:
  name: skill-researcher
  labels:
    orka.ai/category: "research"
spec:
  displayName: "Research Methodology"
  description: "Structured research workflow and source validation guidance"
  version: "1.0.0"
  author: "platform-team"
  tags: ["research", "analysis"]
  content:
    inline: |
      # Research Skill
      Use primary sources and cite references.
    files:
      templates/checklist.md: |
        - [ ] Validate source credibility
        - [ ] Cross-check key claims
  # source tracks where a skill was imported from (for updates)
  # source:
  #   github: "anthropics/skills"
  #   skillName: "researcher"
status:
  phase: Ready
  contentHash: sha256:...
```

## Tool

A custom tool that agents call by name. A Tool calls a plain HTTP endpoint or an MCP server.

:::tip[Video demo]
Watch [Allow stock checks but block purchasing](https://www.youtube.com/watch?v=1vDI6PxhmfY).
:::

This example uses a placeholder catalog API. Replace its URL and Secret reference with your
service's values.

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Tool
metadata:
  name: catalog-search
spec:
  description: "Search a product catalog"
  parameters:
    type: object
    properties:
      query:
        type: string
        description: "Search query"
    required: ["query"]
  http:
    url: "https://catalog.example.com/search"
    method: POST
    timeout: 30s
    authSecretRef:
      name: catalog-api-key
      key: api-key
    authInject: body     # "header" (Bearer token) or "body" (JSON key)
    authBodyKey: api_key # JSON key name when authInject=body
```

| Path | Type | Values/default | Notes |
|------|------|----------------|-------|
| `spec.description` | string | required | Description shown to the LLM. |
| `spec.parameters` | JSON Schema | empty | Tool argument schema in function-calling format. Remote MCP Tools require a reviewed object schema. |
| `spec.brokeredToolClass` | string | empty | `read`, `write`, or `coordination`. Required to expose the Tool to external runtimes through brokered governance; `write` Tools get approval and exact-argument checks. |
| `spec.http.url` | string | required for plain HTTP tools | Endpoint called by workers. May contain [`{{param}}` placeholders](#url-path-interpolation). MCP actor-backed tools may omit it because Orka uses `status.endpoint`. |
| `spec.http.method` | string | default `POST`; allowed `GET`, `POST`, `PUT`, `PATCH`, `DELETE` | HTTP method for plain HTTP tools. MCP tools use `POST`. |
| `spec.http.headers` | map | empty | Static headers sent with the request. Reserved token propagation headers cannot be overridden when outbound TxToken propagation is enabled. |
| `spec.http.timeout` | duration | default `30s` | Per-call request timeout. |
| `spec.http.authSecretRef` | Secret key selector | empty | Secret value used as the auth token. Cannot coexist with a direct OutboundAccessPolicy. |
| `spec.http.outboundAccessPolicyRef.name` | string | empty | Same-namespace [OutboundAccessPolicy](#outboundaccesspolicy) required to be Accepted with ResolvedRefs. |
| `spec.http.authInject` | string | default `header`; allowed `header`, `body` | `header` sends `Authorization: Bearer <token>`. `body` injects the token into the JSON request body and is invalid for MCP tools. |
| `spec.http.authBodyKey` | string | empty | JSON key used when `authInject: body`. |
| `spec.mcp.remote.url`, `spec.mcp.remote.toolName` | string | empty | Call one reviewed function on an independently operated MCP server. See [Remote MCP tools](../concepts/remote-mcp.md). |
| `spec.mcp.workspace.classRef.name`, `spec.mcp.workspace.port` | object, integer | empty | Reserved for running the MCP server in a Service-mode execution workspace. Not supported yet: a Tool that sets it fails reconciliation. |
| `spec.mcp.path` | string | `/mcp` | HTTP path exposed by the MCP server inside the actor. |
| `spec.mcp.substrateActor.templateRef.name` | string | required | Substrate `ActorTemplate` hosting the MCP server. |
| `spec.mcp.substrateActor.templateRef.namespace` | string | Tool namespace or configured default | Namespace containing the actor template. |
| `spec.mcp.substrateActor.poolRef.name` | string | empty | Optional `SubstrateActorPool` for actor placement and reuse. The pool template must match the MCP actor template. |
| `spec.mcp.substrateActor.poolRef.namespace` | string | Tool namespace | Namespace containing the referenced actor pool. |
| `spec.mcp.substrateActor.boot` | boolean | `false` | Boots the actor from scratch on first resume; later reconciles reuse an already booted actor. |
| `status.available` | boolean | false | Whether the controller can reach the resolved endpoint. |
| `status.endpoint` | string | empty | Resolved non-secret endpoint used by workers. For MCP actor-backed tools this is the Substrate router endpoint. |
| `status.actor` | object | empty | Safe actor metadata, including provider, actor ID, route host, resolved template, and pool reference. |

A Tool selects at most one MCP backend: `remote` or `substrateActor`.

### MCP servers in Substrate actors

MCP actor-backed Tools require Substrate support on the controller. Orka creates or reuses the
Substrate actor, waits for the MCP endpoint, stores it in `status.endpoint`, and workers call
the MCP tool through JSON-RPC `tools/call` using the Tool name as the MCP tool name.

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Tool
metadata:
  name: repo-inspector
spec:
  description: "Inspect repository metadata through an MCP server"
  parameters:
    type: object
    properties:
      message:
        type: string
    required:
      - message
  mcp:
    path: /mcp
    substrateActor:
      templateRef:
        name: orka-mcp
        namespace: ate-demo
      poolRef:
        name: mcp-substrate-pool
      boot: true
```

`mcp.path` defaults to `/mcp`, `poolRef` is optional, and `boot` only affects the first actor
resume. For transport authentication, set `spec.http.authSecretRef`, keep `authInject` as
`header` or omit it, and omit `http.url`: MCP call arguments are forwarded to the server as
tool input, so `authInject: body` is only valid for plain HTTP tools.

### URL path interpolation

Tool CRD URLs can contain `{{paramName}}` placeholders that are replaced with parameter values at runtime. Interpolated values are URL path-escaped, and the matching parameters are removed from the request body. This is useful for REST APIs that require path parameters.

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Tool
metadata:
  name: github-merge-pr
spec:
  description: "Merge a GitHub pull request"
  parameters:
    type: object
    properties:
      owner:
        type: string
      repo:
        type: string
      pull_number:
        type: integer
      merge_method:
        type: string
        enum: [merge, squash, rebase]
    required: [owner, repo, pull_number]
  http:
    url: "https://api.github.com/repos/{{owner}}/{{repo}}/pulls/{{pull_number}}/merge"
    method: PUT
    authSecretRef:
      name: github-token
      key: token
    authInject: header
```

In this example, `owner`, `repo`, and `pull_number` are interpolated into the URL path and removed from the JSON body. Only `merge_method` is sent in the request body.

## OutboundAccessPolicy

`OutboundAccessPolicy` is namespaced and selects exactly one adapter. Direct mode performs RFC 8693/RFC 7523 exchange and injects a validated Bearer resource credential. Gateway mode dials a trusted Kubernetes Service while preserving the original Tool authority, path, query, method, body, and protocol headers. Connection mode injects the requesting person's linked-account credential for a `ConnectorProvider`.

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: OutboundAccessPolicy
metadata:
  name: resource-api
  namespace: orka-system
spec:
  direct:
    grant: TokenExchange
    tokenEndpoint:
      url: https://identity.example.test/oauth/token
    subject:
      source: TransactionToken
    scopes: [api.read]
    requestedTokenType: urn:ietf:params:oauth:token-type:access_token
    expectedIssuedTokenType: urn:ietf:params:oauth:token-type:access_token
```

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: OutboundAccessPolicy
metadata:
  name: github-as-me
  namespace: orka-system
spec:
  connection:
    providerRef:
      name: github
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `spec.connection.providerRef.name` | string | required | Same-namespace `ConnectorProvider`. `ResolvedRefs` is True only while the provider is Accepted. |
| `spec.connection.output` | object | `Authorization: Bearer` | Header and prefix for the injected credential. `Txn-Token` is forbidden. |

Connection mode resolves at call time, in the controller only. A `readOnly` Connection hides the provider's write-class tools from the agent, and connector write tools are always approval-required. For native `type: ai` Tasks the worker Pod never runs such a tool itself: it calls `POST /internal/v1/tasks/:namespace/:taskName/connector-tools/:tool` with its ServiceAccount token, and the controller verifies the caller is the Task's current worker, that the arguments satisfy the Tool's declared JSON Schema, that the tool is connector-backed and enabled for the Task, and that the policy frozen into the Job at dispatch is honored: the Job carries the tool list, the approval-required set, the spec digest of every dispatched connector tool, and the Connection bindings (`ORKA_CONNECTOR_TOOL_DIGESTS`, `ORKA_CONNECTION_BINDINGS`). A tool whose definition changed since dispatch, a write-class tool the frozen policy does not cover, or a Task whose `status.connectionBindings` no longer match the Job's is refused, and a tool the policy covers executes only with an approved approval (`approvalId`) that binds this tool's configuration, the connection-mode policy's injection configuration, and the Connection frozen for its policy (so a Job re-created after the decision against a re-linked account or a changed policy needs a fresh approval), these exact arguments, and is claimed atomically for a single execution. Every failure before a provider request hands the claim back; a claimed call is recorded in the durable external-effect ledger under its claim with the frozen Connection digest, using the Tool's own request timeout as its lease, and a worker that lost the response receives the committed result again (`replayed: true`) without a second request. Results larger than 256 KiB are retained as a digest-and-size receipt and cannot be replayed. The effect record is reserved before the claim is recorded, and a spent claim is reconciled from it: a record still Pending is moved to Failed (which fences out any request that has not started its call) and the claim is handed back; an in-flight record whose lease lapsed settles as outcome unknown (`502`) and the approval stays spent; a live in-flight record is `409 still executing`. It then executes the call against the Connection frozen into `status.connectionBindings` at Job creation, which a recovered Job always reads back from its own environment. The Task's verified `spec.requestedBy` selects the person, the Connection identity frozen into the Task's execution snapshot at dispatch must still match the live Connection (UID, generation, and grant sequence, so a re-link of the same Connection object needs a re-dispatch), the policy object checked against the dispatched configuration must be the one that resolves, the Connection must be Ready for its current generation, and the Tool URL must be HTTPS without `authSecretRef`. A token that expires within 60 seconds is refreshed once per Connection at a time and the rotated material written back to custody. A provider that rejects the refresh marks the Connection `Revoked` and shreds its custody; an expired token with no refresh token marks it `Expired`. Any other condition fails the call with no fallback to Task Secrets, environment credentials, or other people's Connections. Worker Pods have no credential source and refuse connection-mode policies.

Policy status contains only `observedGeneration`, `Accepted`, and `ResolvedRefs`. Secret references are key-specific and same-namespace. Cross-namespace Service refs require exact controller allowlist entries. See [Outbound Access Policies](../concepts/outbound-access.md).

## ConnectorProvider

`ConnectorProvider` is the operator-owned catalog entry for one third-party service people may link through OAuth. It is namespaced, reconciled only when the controller runs with `--connectors-enabled`, and carries public OAuth client settings plus the tools the service offers. Only the client secret lives in a Secret.

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: ConnectorProvider
metadata:
  name: github
  namespace: orka-system
spec:
  displayName: GitHub
  oauth:
    authorizeURL: https://github.com/login/oauth/authorize
    tokenURL: https://github.com/login/oauth/access_token
    clientID: Iv1.example-client-id
    clientSecretRef:
      name: github-connector-oauth
      key: clientSecret
    scopes:
      read: [read:user]
      write: [repo]
  tools:
    - name: list_pull_requests
      class: read
      source: Builtin
    - name: create_pull_request
      class: write
      source: Builtin
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `spec.displayName` | string | object name | Shown to people in the dashboard and CLI. |
| `spec.oauth.authorizeURL` | string | required | Absolute HTTPS authorization endpoint. Private, loopback, link-local, and cluster-internal hosts are rejected, and so are single-label names (which Pod DNS search domains complete to cluster Services) and non-canonical numeric hosts. Hosts must be ASCII: write an internationalized name in its punycode (`xn--`) form. The same host rules, and a 2048-byte length bound, apply to every endpoint and HTTP tool URL. |
| `spec.oauth.tokenURL` | string | required | Absolute HTTPS token endpoint used for the code exchange and refresh. |
| `spec.oauth.revocationURL` | string | empty | Optional RFC 7009 endpoint called best-effort on disconnect for the committed credential. It must be on the token endpoint's host and port, so tokens are only sent back to their issuer; GitHub has no such endpoint, so a GitHub provider leaves it empty. Orka never revokes material it did not commit: a token from a consent nobody completed is deleted and left to expire, because it may belong to a different person's grant. |
| `spec.oauth.clientID` | string | required | Public OAuth client identifier, printable ASCII, at most 256 bytes. |
| `spec.oauth.clientSecretRef` | Secret key selector | required | Same-namespace Secret holding the client secret. |
| `spec.oauth.clientAuthentication` | `ClientSecretBasic` \| `ClientSecretPost` | `ClientSecretBasic` | How the client secret is presented to the token endpoint. |
| `spec.oauth.pkce` | bool | `true` | Enable RFC 7636 code verification. |
| `spec.oauth.scopes.read` / `.write` | []string | empty | Scopes requested for `readOnly` Connections, and additionally for `readWrite`. Entries must be RFC 6749 scope tokens (printable ASCII without spaces, quotes, or backslashes) of at most 256 bytes; the read and write sets together must encode to at most 2048 bytes. |
| `spec.oauth.additionalAuthorizeParameters` | map | empty | Static authorize query parameters, each value at most 512 bytes and 2048 bytes encoded in total. Reserved OAuth fields and credential-like names (`token`, `secret`, `api_key`, `assertion`, and similar) are rejected; the spec is public configuration. Endpoint URLs may carry a query of at most 1024 bytes, but not credential-like parameters or reserved OAuth fields (including `code_verifier`). An HTTP tool URL may use any query name that is not credential-like, such as `state=open`. |
| `spec.tools[].name` | string | required | Tool name exposed to agents. Unique within the provider. |
| `spec.tools[].class` | `read` \| `write` | required | Write tools are hidden from `readOnly` Connections and require approval. |
| `spec.tools[].source` | `Builtin` \| `HTTP` | required | `Builtin` names an existing Orka tool. `HTTP` carries a curated definition in `http`. |
| `spec.tools[].description`, `.parameters`, `.http` | | | HTTP tools only. `parameters` must be a JSON Schema whose root declares `type: object` and that resolves in full, including nested property schemas. `http.url` must be HTTPS and is the exact destination, without template placeholders; static headers are limited to 128-byte names, 1024-byte values, and 4096 bytes in total; `Authorization`, `Cookie`, `Host`, `Txn-Token`, and hop-by-hop headers (`Connection`, `Keep-Alive`, `Te`, `Trailer`, `Upgrade`, and similar) are reserved, and credential-like header names (`X-Api-Key`, `X-Auth-Token`, and similar) are rejected: the linked account is the only credential. |

Status contains only `observedGeneration`, `Accepted`, and `ResolvedRefs`. `Builtin` declarations are checked against the controller's built-in tool registry and the connector built-in catalog (the GitHub tools listed in the [Connectors guide](../guides/connectors.md#built-in-github-tools)): a misspelled name, a built-in that would ignore the linked credential, or a class other than the one the catalog fixes for the tool is rejected.

## Connection

`Connection` is one person's linked account with one `ConnectorProvider`. The API server creates it from the caller's verified OIDC or context-token identity; `spec.subject` and `spec.providerRef` are immutable. Deleting the Connection is the disconnect. Status never carries token material.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `spec.subject.issuer` | string | required | Identity issuer that verified the subject. Immutable. |
| `spec.subject.subject` | string | required | Issuer-scoped stable subject. Immutable. |
| `spec.providerRef.name` | string | required | Same-namespace `ConnectorProvider`. Immutable. |
| `spec.mode` | `readOnly` \| `readWrite` | `readOnly` | `readOnly` hides the provider's write tools and requests only read scopes. |
| `status.state` | string | | `Pending`, `Ready`, `Expired`, `Revoked`, or `Error`. |
| `status.grantedScopes` | []string | | Scopes the provider reported at consent time. |
| `status.linkedAt`, `.expiresAt`, `.lastRefreshTime` | time | | Link, access-token expiry, and refresh timestamps. |
| `status.grantSequence` | integer | | The grant custody assigned to the linked material; it rises with every committed consent and never repeats. Authority frozen or approved under one grant does not survive a re-link. |
| `status.consent.providerUID`, `.authorityDigest` | string | | The `ConnectorProvider` UID and a non-secret digest of everything the last consent authorized a token to reach: the provider's OAuth client identity (client ID, secret reference, authentication method, endpoints, static authorize parameters) and the curated HTTP tool destinations (name, class, URL, method, static headers). Built-in declarations (name, class, and their fixed `https://api.github.com` audience) are part of it too. Changing any of these asks for consent again. |

Conditions are `ProviderResolved` and `ScopesGranted` (set by the controller; the latter compares `status.grantedScopes` with the scopes the current mode and provider require, so widening the mode or a provider requiring more scopes projects `Pending` with reason `ConsentRequired` without erasing the consent, and narrowing restores readiness; it also requires `status.consent` to match the current provider, so a replaced provider or a changed OAuth client asks for consent again instead of refreshing or revoking the held token against a different authority) and `Ready` (set by the consent and refresh paths). A Connection is usable only when `Ready` is True and both controller conditions are True for the current generation. See [ADR 0033](https://github.com/orka-agents/orka/blob/main/docs/adr/0033-user-connectors.md) for the design.

## RepositoryScan

Repository security scan configuration. A `RepositoryScan` is namespace-scoped and tells Orka which repository to scan, how to schedule incremental scans, and which Agents should perform analysis and remediation.

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: RepositoryScan
metadata:
  name: example-repo
  namespace: orka-system
spec:
  provider: github
  repoURL: "https://github.com/example/app"
  owner: example
  repository: app
  branch: main
  ref: "v1.2.3"                 # optional tag, branch, or commit SHA checkout override
  subPath: "services/api"       # optional monorepo scope
  gitSecretRef:                  # optional for private repositories
    name: github-credentials
  forkRepo: "https://github.com/example/app-security-fork" # optional remediation fork
  prBaseBranch: main             # optional PR base branch override
  schedule: "0 2 * * *"         # optional cron expression for incremental scans
  timeZone: "UTC"               # optional IANA time zone
  historyDays: 30                # optional initial history window
  validationMode: light          # off, light, or full
  validationMaxFindingsPerRun: 8 # optional auto-validation task cap for light mode
  validationMinSeverity: medium  # optional auto-validation severity threshold
  validationMinConfidence: medium # optional auto-validation confidence threshold
  customScanInstructionsRef:     # optional ConfigMap-backed additive scanner instructions
    name: repo-security-policy
    key: policy                  # optional; defaults to policy
  falsePositivePolicyRef:        # optional ConfigMap-backed false-positive policy
    name: repo-security-policy
    key: false-positives
  analysisAgentRef:
    name: security-reviewer
  patchAgentRef:                 # optional; defaults to the analysis agent when omitted
    name: security-patcher
  maxFindingsPerRun: 25
  suspend: false                 # pause scheduled incremental scans when true
```

**Spec fields:**

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `provider` | string | No | Source control provider. `github` is the supported v1 provider and default. |
| `repoURL` | string | Yes | Repository URL to scan. |
| `owner` | string | No | Repository owner or organization. Inferred from `repoURL` when omitted. |
| `repository` | string | No | Repository name. Inferred from `repoURL` when omitted. |
| `branch` | string | No | Base branch to scan. Defaults to the literal `main` when omitted (not resolved from the repository's actual default branch). Set this explicitly for repositories whose default branch is not `main`. |
| `ref` | string | No | Specific git ref, tag, or commit SHA to check out for scan tasks. When `ref` is set and `branch` is omitted, scan workspaces check out the ref directly instead of forcing `main`; PR remediation still uses `prBaseBranch` or `main` unless `branch` is set. |
| `subPath` | string | No | Optional subdirectory to scan in a monorepo. |
| `gitSecretRef` | LocalObjectReference | No | Compatibility source-read Secret for scan Tasks; `readCredentialRef` takes precedence when both are set. Never used for publication. |
| `readCredentialRef` | LocalObjectReference | For patches | Source clone/read Secret. Required, together with the three publication roles below, before any patch proposal or remediation PR. |
| `publicationReadCredentialRef` | LocalObjectReference | For patches | Target-repository read Secret used only for publication preflight and independent verification. |
| `publicationCredentialRef` | LocalObjectReference | For patches | Target-repository write Secret used only for the exact compare-and-swap branch push. |
| `forgeCredentialRef` | LocalObjectReference | For patches | Forge API Secret used for controller-side forge reads and PR upkeep: fetching the published remediation commit to derive and verify patch evidence, and reconciling/decorating the remediation pull request. Never mounted into any Task. The four patch roles must reference pairwise-distinct Secrets. |
| `forkRepo` | string | No | Writable fork repository URL for patch proposal branches and remediation PRs. |
| `prBaseBranch` | string | No | Pull request base branch for remediation. Defaults to `branch` when omitted. |
| `schedule` | string | No | Cron expression for scheduled incremental scans. |
| `timeZone` | string | No | IANA time zone used by `schedule`. |
| `historyDays` | int32 | No | How far back the initial scan should inspect repository history. |
| `validationMode` | string | No | Validation aggressiveness: `off`, `light`, or `full`. Defaults to `light`. |
| `validationMaxFindingsPerRun` | int32 | No | Maximum automatic validation tasks to enqueue per scan run in `light` mode. |
| `validationMinSeverity` | string | No | Minimum severity eligible for automatic validation. Defaults are mode-dependent. |
| `validationMinConfidence` | string | No | Minimum confidence eligible for automatic validation. Defaults are mode-dependent. |
| `customScanInstructionsRef` | PolicyConfigMapKeyRef | No | Same-namespace ConfigMap key containing additive scanner instructions. The ConfigMap must opt in with `orka.ai/security-policy: "true"` as a label or annotation. |
| `falsePositivePolicyRef` | PolicyConfigMapKeyRef | No | Same-namespace ConfigMap key containing additive false-positive policy text. The ConfigMap must opt in with `orka.ai/security-policy: "true"` as a label or annotation. |
| `analysisAgentRef` | AgentReference | Yes | Agent used for repository scan runs and threat model generation. |
| `patchAgentRef` | AgentReference | No | Agent used for patch proposal runs. |
| `maxFindingsPerRun` | int32 | No | Bounds accepted scan findings per run after validation and deterministic dropped-finding filters. |
| `suspend` | bool | No | Pauses scheduled incremental scans while preserving the scan configuration. |

`PolicyConfigMapKeyRef` uses `name` plus optional `key`; when `key` is omitted, Orka reads the `policy` key. Policy ConfigMap values are capped at 32 KiB and rejected if they look like they contain secrets, tokens, private keys, or credentials. Custom policy text is additive only; it cannot disable Orka's default evidence, no-secret, or finding-quality rules.

**Status fields:**

`status.phase`, `status.lastScanID`, `status.lastScanTaskName`, `status.lastSuccessfulScanAt`, `status.lastObservedHeadSHA`, `status.lastProcessedCommit`, `status.threatModelVersion`, `status.findingCounts`, and `status.conditions` summarize the latest scan lifecycle and open findings. Dynamic scan runs, threat models, findings, and patch proposals are stored by the controller and surfaced through the security API/UI rather than embedded directly in the CRD status.

## RepositoryMonitor

Durable automation for one GitHub repository. A `RepositoryMonitor` is namespace-scoped and tells Orka which repository and branch to watch, which Agents review pull requests and work on issues, which credentials each role may use, and which labels and schedules control the work. [Repository monitors](../guides/repository-monitors.md) and [Issue-to-PR automation](../guides/issue-to-pr-automation.md) explain the workflows.

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: RepositoryMonitor
metadata:
  name: example-app
  namespace: orka-system
spec:
  provider: github
  repoURL: "https://github.com/example/app"
  owner: example                 # optional; inferred from repoURL
  repository: app                # optional; inferred from repoURL
  branch: main
  readCredentialRef:             # optional for private repositories or higher API rate limits
    name: repo-monitor-source-read
  schedule: "*/30 * * * *"      # optional cron expression
  timeZone: "UTC"               # optional IANA time zone
  suspend: false
  targets:
    pullRequests:
      enabled: true
      includeDrafts: false
      maxPerRun: 20
  agents:
    reviewer:
      name: repo-reviewer
  review:
    event: COMMENT              # legacy task input only; does not publish to GitHub
    staleReviewTTL: 24h
    exactEventEnabled: true     # queue exact-head runs from signed PR webhooks
    publish:
      enabled: true             # default false; controller-owned GitHub side effect
      mode: summary_with_inline_findings
      event: COMMENT            # V1 only supports neutral COMMENT reviews
      postPassed: false
      postNeedsChanges: true
      postNeedsHuman: true
      postSecuritySensitive: false
      sameHeadPolicy: skip
      inline:
        enabled: true
        minPriority: P2
        maxComments: 10
  policy:
    protectedLabels:
      - security-sensitive
    pauseLabels:
      - orka:pause
  validation:
    image: ghcr.io/example/app-validation@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa # must contain /bin/sh and the repository's validation tools
```

**Spec fields:**

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `provider` | string | No | Source control provider. `github` is the supported v1 provider and default. |
| `repoURL` | string | Yes | Credential-free GitHub repository root URL to monitor, such as `https://github.com/owner/repo`, `https://github.com/owner/repo.git`, or `git@github.com:owner/repo.git`. Pull request, issue, branch/tree, blob/file, commit, query-string, fragment, non-GitHub, HTTP, and embedded-credential URLs are rejected. |
| `owner` | string | No | Repository owner or organization. Inferred from `repoURL` when omitted. |
| `repository` | string | No | Repository name. Inferred from `repoURL` when omitted. |
| `branch` | string | No | Base branch used for pull request inventory. Defaults to `main`. |
| `gitSecretRef` | LocalObjectReference | No | Backward-compatible source-read Secret containing `token`, `password`, or `GITHUB_TOKEN`. `readCredentialRef` takes precedence when both are set. Never used for publication or forge writes. |
| `readCredentialRef` | LocalObjectReference | No | Source clone/read Secret, used only by the clean-room workspace boundary. |
| `publicationReadCredentialRef` | LocalObjectReference | For write workflows | Target-repository read Secret used only for publication preflight and independent verification. |
| `publicationCredentialRef` | LocalObjectReference | For write workflows | Target-repository write Secret used only for the exact compare-and-swap branch push. |
| `forgeCredentialRef` | LocalObjectReference | For write workflows, label triggers, and review publishing | GitHub API Secret used only for controller-owned forge reads and mutations. The four roles must reference pairwise-distinct Secrets. |
| `schedule` | string | No | Cron expression for scheduled monitor runs. |
| `timeZone` | string | No | IANA time zone used by `schedule`. |
| `suspend` | bool | No | Pauses scheduled monitor runs while preserving the monitor configuration. |
| `targets.pullRequests.enabled` | bool | No | Enables pull request monitoring. Defaults to `true`; set `false` for an issue-only monitor. |
| `targets.pullRequests.includeDrafts` | bool | No | Select draft pull requests for review when true. Defaults to false. |
| `targets.pullRequests.maxPerRun` | int32 | No | Maximum selected PRs per run. Defaults to `20`; allowed range is `1` to `100`. |
| `targets.issues.enabled` | bool | No | Enables issue inventory and the issue workflow. Defaults to `false`. |
| `targets.issues.maxPerRun` | int32 | No | Maximum selected issues per run, `1` to `100`. |
| `targets.issues.includeLabels` / `excludeLabels` | list | No | Restrict issue inventory to issues with any of these labels, or exclude issues with them. |
| `triggers.github.labels.enabled` | bool | No | Accept `orka:*` label commands from signed GitHub webhooks. |
| `triggers.github.labels.consumeCommandLabels` | bool | No | Remove an accepted command label after durable intake. |
| `triggers.github.labels.requireActorPermission` | string | No | Minimum GitHub permission of the person applying a command label: `write` (default), `maintain`, or `admin`. |
| `triggers.github.labels.issues.implement` | string | No | Label that starts the issue workflow. Defaults to `orka:implement`. |
| `agents.reviewer` | AgentReference | When PR monitoring is enabled | Agent for read-only pull request review. It must use a built-in `claude`, `codex`, or `opencode` runtime and omit `spec.secretRef`; the runtime proxy supplies model credentials. |
| `agents.triager`, `agents.researcher`, `agents.planner` | AgentReference | No | Agents for read-only issue triage, research, and planning. |
| `agents.implementer` | AgentReference | No | Agent for issue implementation, a write workflow. |
| `agents.repairer` | AgentReference | No | Agent for pull request repair, a write workflow. |
| `issueWorkflow.triage.enabled`, `.research.enabled`, `.planning.enabled`, `.implementation.enabled` | bool | No | Enable each issue phase. Each defaults to on when its Agent is configured and a command requests it. |
| `issueWorkflow.implementation.requirePlan` | bool | No | Require a current ready plan before implementation. Defaults to `true`. |
| `issueWorkflow.implementation.branchPrefix` | string | No | Prefix for implementation push branches. Defaults to `orka/issue`. |
| `issueWorkflow.implementation.maxActive` | int32 | No | Concurrently active implementation jobs per monitor. Defaults to `2`. |
| `issueWorkflow.implementation.maxAttemptsPerIssue` | int32 | No | Implementation attempts per issue. Defaults to `2`. |
| `issueWorkflow.implementation.maxChangedFiles` | int32 | No | Files allowed in one implementation patch. Defaults to `12`. |
| `issueWorkflow.implementation.allowedPaths` | list | No | Path globs or prefixes, such as `api/**`, that implementation patches may touch. |
| `repair.enabled` | bool | No | Enables automatic, bounded pull request repair. Requires `agents.repairer` and the publication credentials. |
| `repair.maxRepairsPerPR` / `maxRepairsPerHead` | int32 | No | Repair limits. Default `5` per pull request and `2` per head. |
| `repair.maxValidationRetries` | int32 | No | Validation retries for one repair job. Default `2`. |
| `review.requireGreenCI` | bool | No | Wait for green CI before selecting a pull request for review. |
| `review.event` | string | No | Legacy/default review event value included in review task input. It does not publish to GitHub; use `review.publish.event`. Defaults to `COMMENT`. |
| `review.publish.enabled` | bool | No | Enables controller-owned GitHub pull request review publishing. Defaults to `false`. |
| `review.publish.mode` | string | No | `summary_only` or `summary_with_inline_findings`. Inline comments are only attempted for changed RIGHT-side diff lines. |
| `review.publish.event` | string | No | GitHub review event submitted by the controller. V1 only supports neutral `COMMENT` reviews; `APPROVE` and `REQUEST_CHANGES` are rejected. |
| `review.publish.postPassed` | bool | No | Post clean/passed reviews when true. Defaults to `false`. |
| `review.publish.postNeedsChanges` | bool | No | Post `needs_changes` reviews when true. Defaults to `true`. |
| `review.publish.postNeedsHuman` | bool | No | Post `needs_human` reviews when true. Defaults to `true`. |
| `review.publish.postSecuritySensitive` | bool | No | Allow public publishing of `security_sensitive` results. Defaults to `false`; sensitive findings are skipped by default. |
| `review.publish.sameHeadPolicy` | string | No | Duplicate policy for the same monitor, PR, and head SHA. V1 only supports `skip`. |
| `review.publish.inline.enabled` | bool | No | Enables inline GitHub review comments when `mode` is `summary_with_inline_findings`. |
| `review.publish.inline.minPriority` | string | No | Lowest priority eligible for inline comments (`P0`-`P3`). Defaults to `P2`; lower-priority findings remain in the summary. |
| `review.publish.inline.maxComments` | int32 | No | Max inline comments per GitHub review. Defaults to `10`, allowed range `0` to `50`. |
| `review.staleReviewTTL` | duration | No | Re-review an unchanged head after the previous accepted review is older than this duration. |
| `review.exactEventEnabled` | bool | No | Queue exact-head monitor runs from signed GitHub pull request webhook events when true. |
| `policy.protectedLabels` | list | No | PR labels that block automated review selection. |
| `policy.pauseLabels` | list | No | PR labels that pause monitor automation for that item. |
| `validation.image` | string | No | Container image for isolated pull request validation. The reviewer inspects the checked-out code and selects one shell command; Orka fixes the image and exact read-only PR head. The image must contain `/bin/sh` and every tool the repository may require. |

When `validation.image` is set, the reviewer must call `run_validation` once and wait for its child Task to finish before returning a `passed` verdict. A missing, failed, malformed, or stale validation Task blocks `passed` and merge-ready state. Orka records the image and status with the review and retains only a SHA-256 digest of the selected command in its durable validation binding. Validation stdout and stderr are suppressed so repository or fixture secrets cannot enter Pod logs or result storage. The command runs against a read-only checkout with deny-all ingress and egress, so the image must already contain every required tool and dependency. If a Go repository needs `golangci-lint`, for example, use a Go-based image that includes dependencies and `golangci-lint`. The same rule applies to offline Terraform, Azure CLI, or other repository-specific checks. Maintainers normally set this image once per `RepositoryMonitor`; commands, args, credentials, and network access are not part of the monitor configuration.

Before upgrading a monitor that uses the former `validation.mode` and `validation.commands` fields, replace them with `validation.image`. The CRD retains the old fields so the controller can detect existing policies, but it does not execute them. A non-empty legacy command list puts the monitor in `Error` with reason `LegacyValidationCommandsUnsupported`; Orka does not silently disable validation. Apply the updated CRD before upgrading the controller, then update each affected monitor with a digest-pinned image and remove the legacy fields.

`targets.commits` is rejected until commit inventory is implemented. Review tasks check out the exact PR head and receive generated read-only context files under `/workspace/.git/orka/`: `pr-review.md`, `pr-review.files`, and `pr-review.diff`. GitHub publishing, branch pushes, PR creation, label consumption, and readiness publication are controller-owned and audited through mutation records; read-only agents never receive the GitHub mutation token. GitHub's per-PR auto-merge setting owns merging. Orka never enables auto-merge or calls the merge endpoint; when it is disabled, the PR stays open and ready.

**Status fields:**

`status.phase`, `status.lastRunID`, `status.lastRunTime`, `status.lastSuccessfulRunTime`, `status.observedGeneration`, `status.openPullRequests`, `status.pendingReviews`, `status.activeRepairs`, `status.blockedItems`, `status.mergeReadyItems`, and `status.conditions` summarize the monitor lifecycle and current queue. Dynamic runs, PR items, review records, repair records, command events, and audit events are stored by the controller and surfaced through the monitor API/UI rather than embedded directly in CRD status.

See [Repository Monitors](../guides/repository-monitors.md) for the workflow, API examples, and current limits.

## SubstrateActorPool

`SubstrateActorPool` is an operator-owned pool of deterministic Substrate actors for native MCP Tools and density reporting. It does not place ACP Tasks. Enable it with `--substrate-mcp-tools-enabled`.

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: SubstrateActorPool
metadata:
  name: codex-substrate-pool
spec:
  templateRef:
    name: orka-codex
    namespace: ate-demo
  workerPoolRef:
    name: orka-workers
    namespace: ate-demo
  targetActors: 4
  precreateActors: true
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `templateRef.name` | string | required | Substrate `ActorTemplate` used for pool members. |
| `templateRef.namespace` | string | Pool namespace | Native Substrate Atespace containing the `ActorTemplate`. |
| `workerPoolRef.name` | string | empty | Optional Substrate `WorkerPool` used for capacity and density reporting. |
| `workerPoolRef.namespace` | string | Pool namespace | Namespace containing the `WorkerPool`. |
| `targetActors` | integer | `0` | Desired stateful actor count, capped at `1000`. References from Tools require at least `1`. |
| `precreateActors` | boolean | `false` | Pre-create deterministic warm actors up to `targetActors`. |

`spec.templateRef` is immutable. Orka records the first accepted native template
