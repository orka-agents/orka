---
slug: /configuration
---

# Configuration

## Custom Resources

### Task

The core work unit. Supports container commands, AI agent prompts, or external agent CLI runtimes.

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

### Agent

Reusable agent configurations with model settings, tools, skills, and optional agent-to-agent coordination.

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
    - name: web-search
    - name: github-search
  skills:
    - name: skill-researcher
  session:
    persistence: configmap # configmap, pvc, or none
    ttl: 24h
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

**Coordination fields:**

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | `false` | Enable agent-to-agent coordination tools |
| `autonomous` | bool | `false` | Enables autonomous loop mode. When true, the controller re-creates Jobs in a loop instead of marking the task as Succeeded |
| `maxIterations` | int32 | `0` | Limits the number of autonomous loop iterations. Only used when `autonomous` is true. `0` means unlimited |
| `approvalRequiredTools` | list | `[]` | Custom Tool CRD names that require human approval before execution in enabled autonomous coordination mode. Built-in tools, including `request_approval`, are rejected |
| `allowedAgents` | list | `[]` | List of agent names this agent is allowed to delegate to |
| `maxConcurrentChildren` | int32 | `5` | Maximum number of concurrent child tasks |
| `maxDepth` | int32 | `3` | Maximum delegation depth |

**Auto-injected coordination tools** (when `enabled: true`):

`delegate_task`, `wait_for_tasks`, `create_container_task`, `cancel_task`, `send_message`, `check_messages`, `recall_memory`, `remember`, `propose_memory`, `search_transcript`, `create_pull_request`, `list_pull_requests`, `check_pr_review_marker`, `check_pull_request_ci`, `merge_pull_request`, `auto_merge_pull_request`, `review_pull_request`, `post_review_comment`, `create_agent`, `delete_agent`, `update_plan`

When `autonomous: true`, `request_approval` is also injected so the worker can park the task after an explicit human approval request.

**Opt-in coordination tools** (require explicit `spec.tools[]` entries on the Agent):

`list_issues`, `get_issue`, `comment_on_issue`

**PR review marker environment:**

Prompt-orchestrated PR monitors use `check_pr_review_marker` to produce and detect hidden review markers. These variables are read by the worker Task that runs the tool:

| Environment variable | Description |
|----------------------|-------------|
| `ORKA_PR_REVIEW_MARKER_SECRET` | Optional stable HMAC key for PR review marker signatures. Use a Kubernetes Secret or another secret injection path. |
| `ORKA_PR_REVIEW_MARKER_PREVIOUS_SECRETS` | Optional comma-separated previous marker keys accepted during rotation. |
| `ORKA_PR_REVIEW_MARKER_TRUSTED_AUTHOR` | Optional GitHub login trusted for legacy marker compatibility. When omitted, Orka resolves the authenticated GitHub user for the Task credential. |

### RepositoryScan

Repository security scan configuration. A `RepositoryScan` is namespace-scoped and tells Orka which repository to scan, how to schedule incremental scans, and which Agents should perform analysis and remediation.

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: RepositoryScan
metadata:
  name: example-repo
  namespace: default
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
| `gitSecretRef` | LocalObjectReference | No | Secret containing credentials for private repository access. |
| `forkRepo` | string | No | Writable fork repository URL for patch proposal branches and remediation PRs. |
| `prBaseBranch` | string | No | Pull request base branch for remediation. Defaults to `branch` when omitted. |
| `schedule` | string | No | Cron expression for scheduled incremental scans. |
| `timeZone` | string | No | IANA time zone used by `schedule`. |
| `historyDays` | int32 | No | How far back the initial scan should inspect repository history. |
| `validationMode` | string | No | Validation aggressiveness: `off`, `light`, or `full`. Defaults to `light`. |
| `analysisAgentRef` | AgentReference | Yes | Agent used for repository scan runs and threat model generation. |
| `patchAgentRef` | AgentReference | No | Agent used for patch proposal runs. |
| `maxFindingsPerRun` | int32 | No | Bounds scan output volume. |
| `suspend` | bool | No | Pauses scheduled incremental scans while preserving the scan configuration. |

**Status fields:**

`status.phase`, `status.lastScanID`, `status.lastScanTaskName`, `status.lastSuccessfulScanAt`, `status.lastObservedHeadSHA`, `status.lastProcessedCommit`, `status.threatModelVersion`, `status.findingCounts`, and `status.conditions` summarize the latest scan lifecycle and open findings. Dynamic scan runs, threat models, findings, and patch proposals are stored by the controller and surfaced through the security API/UI rather than embedded directly in the CRD status.

### RepositoryMonitor

Durable GitHub pull request monitor configuration. A `RepositoryMonitor` is namespace-scoped and tells Orka which repository and branch to inspect, which Claude runtime Agent should review selected PR heads, and which labels or scheduling rules should control review selection.

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: RepositoryMonitor
metadata:
  name: example-app
  namespace: default
spec:
  provider: github
  repoURL: "https://github.com/example/app"
  owner: example                 # optional; inferred from repoURL
  repository: app                # optional; inferred from repoURL
  branch: main
  gitSecretRef:                  # optional for private repositories or higher API rate limits
    name: repo-monitor-github
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
        onlyChangedLines: true
  policy:
    protectedLabels:
      - security-sensitive
    pauseLabels:
      - orka:pause
  validation:
    mode: changed               # off, changed, or full
    commands:
      - make test
```

**Spec fields:**

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `provider` | string | No | Source control provider. `github` is the supported v1 provider and default. |
| `repoURL` | string | Yes | Credential-free GitHub repository root URL to monitor, such as `https://github.com/owner/repo`, `https://github.com/owner/repo.git`, or `git@github.com:owner/repo.git`. Pull request, issue, branch/tree, blob/file, commit, query-string, fragment, non-GitHub, HTTP, and embedded-credential URLs are rejected. |
| `owner` | string | No | Repository owner or organization. Inferred from `repoURL` when omitted. |
| `repository` | string | No | Repository name. Inferred from `repoURL` when omitted. |
| `branch` | string | No | Base branch used for pull request inventory. Defaults to `main`. |
| `gitSecretRef` | LocalObjectReference | No | Git Secret containing `token`, `password`, or `GITHUB_TOKEN` for GitHub API access and same-repository PR checkout. This is separate from the reviewer Agent's runtime credential Secret. |
| `schedule` | string | No | Cron expression for scheduled monitor runs. |
| `timeZone` | string | No | IANA time zone used by `schedule`. |
| `suspend` | bool | No | Pauses scheduled monitor runs while preserving the monitor configuration. |
| `targets.pullRequests.enabled` | bool | No | Enables pull request monitoring. Currently this must be true or omitted. |
| `targets.pullRequests.includeDrafts` | bool | No | Select draft pull requests for review when true. Defaults to false. |
| `targets.pullRequests.maxPerRun` | int32 | No | Maximum selected PRs per run. Defaults to `20`; allowed range is `1` to `100`. |
| `agents.reviewer` | AgentReference | Yes | Claude runtime Agent used for read-only PR review tasks. The Agent must reference a Secret in the monitor namespace with `ANTHROPIC_API_KEY` or `ANTHROPIC_FOUNDRY_API_KEY`. |
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
| `review.publish.inline.onlyChangedLines` | bool | No | Restricts inline comments to changed RIGHT-side diff lines. V1 treats this as true. |
| `review.staleReviewTTL` | duration | No | Re-review an unchanged head after the previous accepted review is older than this duration. |
| `review.exactEventEnabled` | bool | No | Queue exact-head monitor runs from signed GitHub pull request webhook events when true. |
| `policy.protectedLabels` | list | No | PR labels that block automated review selection. |
| `policy.pauseLabels` | list | No | PR labels that pause monitor automation for that item. |
| `validation.mode` | string | No | Validation mode included in review task input. Defaults to `changed`; allowed values are `off`, `changed`, and `full`. |
| `validation.commands` | list | No | Validation commands included in review task input for the reviewer. |

`targets.issues`, `targets.commits`, `review.requireGreenCI`, repair, and automerge fields are present for the broader monitor API shape, but the current controller rejects issue/commit targets and `review.requireGreenCI`; repair and automerge are not active workflows in this implementation slice. Review tasks check out the exact PR head and receive generated read-only context files under `/workspace/.git/orka/`: `pr-review.md`, `pr-review.files`, and `pr-review.diff`. GitHub publishing, when enabled, happens later in the controller from the structured review result; the LLM never receives the GitHub mutation token and cannot choose the GitHub event.

**Status fields:**

`status.phase`, `status.lastRunID`, `status.lastRunTime`, `status.lastSuccessfulRunTime`, `status.observedGeneration`, `status.openPullRequests`, `status.pendingReviews`, `status.activeRepairs`, `status.blockedItems`, `status.mergeReadyItems`, and `status.conditions` summarize the monitor lifecycle and current queue. Dynamic runs, PR items, review records, repair records, command events, and audit events are stored by the controller and surfaced through the monitor API/UI rather than embedded directly in CRD status.

See [Repository Monitors](../guides/repository-monitors.md) for the workflow, API examples, and current limits.

### Execution

Tasks and Agents both support `spec.execution` for harness wrapper pod runtime selection and placement. Agent Tasks can also set `Task.spec.execution.workspace` to request experimental workspace-backed execution through upstream `agent-sandbox` or Agent Substrate; see [Agent Sandbox Workspaces](agent-sandbox.md) and [Substrate Execution Workspaces](substrate.md).

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
| `nodeSelector` | map[string]string | Restricts harness wrapper pods to nodes with matching labels |
| `tolerations` | list | Allows harness wrapper pods onto tainted runtime-specific node pools |
| `affinity` | object | Adds Kubernetes affinity or anti-affinity rules for harness wrapper pods |
| `workspace` | object | Experimental execution workspace request under `Task.spec.execution.workspace`. Use only on `type: agent` Tasks. |

Resolution order:

- `Agent.spec.execution` provides defaults for tasks that reference the Agent
- `Task.spec.execution` overrides Agent defaults
- `runtimeClassName` is a scalar override
- `nodeSelector`, `tolerations`, and `affinity` replace Agent defaults when they are set on the Task

#### Execution Workspace Requests

`Task.spec.execution.workspace` is alpha support for durable, claimable agent workspaces backed by an existing upstream `agent-sandbox` or Agent Substrate installation. When `workspace.enabled: true`, the Task controller validates the request, resolves defaults, and passes workspace settings to the harness wrapper turn. Orka still creates the outer Kubernetes worker Job; the worker wrapper claims and waits for the upstream workspace, runs the configured agent runtime inside it, and then deletes or retains/releases the workspace according to `cleanupPolicy`.

This field is distinct from `Task.spec.agentRuntime.workspace`, which configures the git checkout prepared for the agent runtime inside the current execution environment.

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Task
metadata:
  name: coding-agent-task
spec:
  type: agent
  agentRef:
    name: claude-agent
  prompt: "Continue implementing the feature in this session."
  sessionRef:
    name: feature-123
    create: true
  execution:
    runtimeClassName: gvisor
    workspace:
      enabled: true
      provider: agent-sandbox
      templateRef:
        name: coding-agent
      reusePolicy: session
      cleanupPolicy: retain
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | boolean | `false` | Enables experimental workspace-backed execution. When false or omitted, sandbox settings are not propagated. |
| `provider` | string | Controller default provider, defaulting to `agent-sandbox` | Workspace backend: `agent-sandbox` or `substrate`. |
| `templateRef.name` | string | Controller default template, if configured | Workspace template name. Required when enabled unless the controller has a default template. |
| `templateRef.namespace` | string | Task namespace | Namespace containing the workspace template. Orka propagates this value in its worker environment/request identity. |
| `reusePolicy` | string | `none` | Reuse behavior: `none` or `session`. `session` requires `spec.sessionRef.name`. |
| `cleanupPolicy` | string | Controller default cleanup policy, defaulting to `delete` | Cleanup behavior after execution: `delete` or `retain`. |
| `boot` | boolean | `false` | Substrate only. Boots the actor from scratch on first resume. |
| `poolRef.name` | string | empty | Substrate only. Places the workspace on a `SubstrateActorPool`; pooled workspaces currently require `cleanupPolicy: delete`. |
| `poolRef.namespace` | string | Task namespace | Substrate only. Namespace containing the referenced pool. |
| `snapshot` | object | empty | Substrate only, reserved. Non-empty restore/checkpoint settings are currently rejected. |
| `hibernation` | object | empty | Substrate only, reserved. `processMode: resident` is currently rejected. |

See [Agent Sandbox Workspaces](agent-sandbox.md) and [Substrate Execution Workspaces](substrate.md) for validation rules, controller flags, execution flow, and operational limitations.

#### SubstrateActorPool

`SubstrateActorPool` is an operator-owned pool of deterministic Substrate actors for pooled Task placement, MCP actor-backed Tools, and density reporting.

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
  targetWorkers: 2
  precreateActors: true
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `templateRef.name` | string | required | Substrate `ActorTemplate` used for pool members. |
| `templateRef.namespace` | string | Pool namespace | Namespace containing the `ActorTemplate`. |
| `workerPoolRef.name` | string | empty | Optional Substrate `WorkerPool` used for capacity and density reporting. |
| `workerPoolRef.namespace` | string | Pool namespace | Namespace containing the `WorkerPool`. |
| `targetActors` | integer | `0` | Desired stateful actor count, capped at `1000`. References from Tasks or Tools require at least `1`. |
| `targetWorkers` | integer | `0` | Intended physical worker budget. `targetActors` may exceed this value to express oversubscription. |
| `precreateActors` | boolean | `false` | Pre-create deterministic warm actors up to `targetActors`. |

### Provider Fallback Chain

You can configure fallback providers that are automatically tried when the primary provider fails (e.g., due to auth errors, provider outages, or rate limiting). Fallbacks are configured on the Agent CRD's `spec.model.fallbacks` field.

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Agent
metadata:
  name: resilient-agent
spec:
  providerRef:
    name: my-openai
  model:
    name: gpt-4o
    fallbacks:
      - providerRef: my-anthropic
        model: claude-sonnet-4-20250514
      - providerRef: my-azure-openai
        model: gpt-4o
```

#### How fallbacks work

1. The primary provider is tried first with automatic retries (exponential backoff on 429/5xx errors).
2. If the primary provider fails with an auth error (401/403), network error, or exhausts all retries, the first fallback provider is tried.
3. Each fallback provider also gets automatic retries.
4. If all providers fail, the last error is returned.

#### Fallback fields

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `providerRef` | string | Yes | Name of a Provider CRD to fall back to |
| `model` | string | No | Model to use with this provider. If empty, uses the provider's `defaultModel` |

#### Notes

- Fallbacks are only supported on Agent-based tasks. Agent-less tasks get retries only.
- Each fallback provider must have its own Provider CRD with a valid secret reference.
- Rate-limited providers (429 responses) are temporarily cooled down and skipped in subsequent requests.
- Streaming requests are retried/failed over only on the initial connection — mid-stream failures are not retried.

### Agent (with Runtime)

Agent configuration for external CLI runtimes (Claude Code CLI, GitHub Copilot CLI, or Codex CLI).

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Agent
metadata:
  name: claude-agent
spec:
  secretRef:
    name: claude-credentials
  model:
    name: "claude-sonnet-4-20250514"
  systemPrompt:
    inline: "You are a senior software engineer."
  runtime:
    type: claude         # or "copilot" / "codex"
    defaultMaxTurns: 50
    defaultAllowBash: true
    defaultAllowedTools:
      - Read
      - Write
      - Edit
      - Bash
```

Agent runtime tasks reference an Agent with `runtime` configured:

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Task
metadata:
  name: code-review
spec:
  type: agent
  agentRef:
    name: claude-agent
  prompt: "Review the code in this repo for security issues"
  agentRuntime:
    workspace:
      gitRepo: "https://github.com/example/repo.git"
      branch: main
      # gitSecretRef:
      #   name: git-credentials
      # subPath: "services/api"
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

### Skill

Reusable skill definitions (Agent Skills standard) that are referenced by Agents and AI Tasks.

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
  #   context7: false
status:
  phase: Ready
  contentHash: sha256:...
```

### Tool

Custom tool definitions for agents. Tools can call plain HTTP endpoints or MCP servers hosted in durable Substrate actors. Plain HTTP tools require `http.url` and support header-based or body-based auth injection.

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Tool
metadata:
  name: tavily-search
spec:
  description: "Search the web for current information"
  parameters:
    type: object
    properties:
      query:
        type: string
        description: "Search query"
    required: ["query"]
  http:
    url: "https://api.tavily.com/search"
    method: POST
    timeout: 30s
    authSecretRef:
      name: tavily-secret
      key: api-key
    authInject: body     # "header" (Bearer token) or "body" (JSON key)
    authBodyKey: api_key # JSON key name when authInject=body
```

MCP actor-backed tools can also set `http.authSecretRef` for transport auth.
For these tools, `http.url` may be omitted because Orka uses the resolved actor
endpoint from Tool status. MCP transport auth must use header injection;
`authInject: body` is only valid for plain HTTP tools because MCP call arguments
are forwarded to the MCP server as tool input.

Example MCP actor-backed Tool:

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

MCP actor-backed Tools require `mcp.substrateActor.templateRef.name`. `mcp.path`
defaults to `/mcp`, `poolRef` is optional, and `boot` only affects the first
actor resume. `spec.http` may be omitted unless the resolved actor endpoint
needs transport auth settings; when `spec.http` is present for MCP auth only,
omit `http.url`.

#### URL Path Interpolation

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

### Provider

LLM provider configuration with credentials. Supports Anthropic, OpenAI, and Azure OpenAI.

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
  defaultModel: claude-sonnet-4-20250514
  rateLimit:
    requestsPerMinute: 60
    tokensPerMinute: 100000
  # Azure-specific (only for type: azure-openai)
  # azure:
  #   deploymentName: my-deployment
  #   apiVersion: "2024-02-15-preview"
```

## Helm Chart

Key configuration values for the Helm chart:

| Parameter | Default | Description |
|-----------|---------|-------------|
| `controller.replicas` | `1` | Controller replicas |
| `controller.image.repository` | `ghcr.io/sozercan/orka` | Controller image |
| `controller.watchNamespace` | `""` | Namespace scope (empty = cluster-wide) |
| `controller.enforceNamespaceIsolation` | `true` | Restrict namespace-bound API callers and default Helm RBAC to their namespace |
| `controller.apiPort` | `8080` | REST API port |
| `controller.metricsPort` | `8081` | Metrics endpoint port |
| `controller.healthPort` | `8082` | Health probe port |
| `controller.logLevel` | `info` | Log level (debug/info/warn/error) |
| `controller.agentSandbox.enabled` | `false` | Enable experimental workspace-backed execution for agent Tasks that set `execution.workspace` |
| `controller.agentSandbox.routerUrl` | `""` | Optional upstream agent-sandbox router base URL used for workspace claims |
| `controller.agentSandbox.defaultTemplate` | `""` | Default agent-sandbox `SandboxWarmPool` name when a Task omits `templateRef.name` |
| `controller.agentSandbox.warmPoolPolicy` | `disabled` | Legacy compatibility setting: `disabled` or `template`; v0.5 claims use `SandboxWarmPool` references |
| `controller.agentSandbox.namespaceStrategy` | `task` | Sandbox resource namespace strategy: `task` or `controller` |
| `controller.agentSandbox.claimTimeout` | `2m` | Timeout for workspace claim and readiness operations |
| `controller.agentSandbox.commandTimeout` | `30m` | Timeout for agent runtime execution inside the sandbox |
| `controller.agentSandbox.cleanupPolicy` | `delete` | Default workspace cleanup policy: `delete` or `retain` |
| `workers.ai.image.repository` | `ghcr.io/sozercan/orka/ai-worker` | AI worker image |
| `workers.general.image.repository` | `ghcr.io/sozercan/orka/general-worker` | General worker image |
| `service.type` | `ClusterIP` | Service type |
| `crds.install` | `true` | Install CRDs |
| `crds.keep` | `true` | Keep CRDs on uninstall |
| `monitoring.enabled` | `false` | Enable Prometheus ServiceMonitor |
| `client.create` | `true` | Create client ServiceAccount for API access |
| `client.name` | `orka-client` | Client ServiceAccount name |
| `client.namespace` | `""` | Client ServiceAccount namespace override. Empty defaults to `controller.watchNamespace` when namespace isolation is enforced and `watchNamespace` is set, otherwise the release namespace. |

Context-token flags can also be configured through Helm under
`controller.contextToken`. For example:

```yaml
controller:
  contextToken:
    profile: kontxt
    issuer: https://issuer.example.com
    audience: orka
    headers: Txn-Token
    authzMode: enforce
    scopes:
      taskCreate: orka:tasks:create
      providerUse: orka:providers:use
      toolUse: orka:tools:use
      monitorRead: orka:monitors:read
      monitorWrite: orka:monitors:write
      monitorOperate: orka:monitors:operate
    tts:
      url: https://tts.example.com
      audience: orka-workers
      timeout: 5s
      tokenSource: serviceAccount
      childScope: orka:tasks:create
      outboundScope: orka:tools:use
      childTokenTTL: 5m
      toolTokenTTL: 2m
```

The Helm keys mirror the controller flags: for example,
`controller.contextToken.jwksUrl` renders `--context-token-jwks-url`,
`controller.contextToken.scopes.secretRead` renders
`--context-token-secret-read-scopes`,
`controller.contextToken.scopes.monitorRead` renders
`--context-token-monitor-read-scopes`, and
`controller.contextToken.tts.toolTokenTTL` renders
`--context-token-tool-token-ttl`.

See [charts/orka/values.yaml](https://github.com/sozercan/orka/blob/main/charts/orka/values.yaml) for the full list.

## Controller Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--api-port` | `8080` | REST API server port |
| `--watch-namespace` | `""` | Namespace to watch (empty = all) |
| `--enforce-namespace-isolation` | `false` | Restrict users to their ServiceAccount's namespace |
| `--max-tasks-per-namespace` | `0` | Max active tasks per namespace (0 = unlimited) |
| `--agent-sandbox-enabled` | `ORKA_AGENT_SANDBOX_ENABLED` env or `false` | Enable experimental workspace-backed execution for agent Tasks that set `execution.workspace` |
| `--agent-sandbox-router-url` | `ORKA_AGENT_SANDBOX_ROUTER_URL` env or `""` | Optional upstream agent-sandbox router base URL used for workspace claims |
| `--agent-sandbox-default-template` | `ORKA_AGENT_SANDBOX_DEFAULT_TEMPLATE` env or `""` | Default agent-sandbox `SandboxWarmPool` name when a Task omits `templateRef.name` |
| `--agent-sandbox-warm-pool-policy` | `ORKA_AGENT_SANDBOX_WARM_POOL_POLICY` env or `disabled` | Legacy compatibility setting: `disabled` or `template`; v0.5 claims use `SandboxWarmPool` references |
| `--agent-sandbox-namespace-strategy` | `ORKA_AGENT_SANDBOX_NAMESPACE_STRATEGY` env or `task` | Sandbox resource namespace strategy: `task` or `controller` |
| `--agent-sandbox-claim-timeout` | `ORKA_AGENT_SANDBOX_CLAIM_TIMEOUT` env or `2m` | Timeout for workspace claim and readiness operations |
| `--agent-sandbox-command-timeout` | `ORKA_AGENT_SANDBOX_COMMAND_TIMEOUT` env or `30m` | Timeout for agent runtime execution inside the sandbox |
| `--agent-sandbox-cleanup-policy` | `ORKA_AGENT_SANDBOX_CLEANUP_POLICY` env or `delete` | Default workspace cleanup policy: `delete` or `retain` |
| `--controller-url` | `""` | Base URL workers use to reach the controller API (e.g., `http://orka-api.orka-system.svc:8080`). Required for worker result callbacks and session transcript fetching |
| `--oidc-issuer` | `ORKA_OIDC_ISSUER` env or `""` | OIDC issuer URL for external API bearer token validation. Requires `--oidc-audience` when set |
| `--oidc-audience` | `ORKA_OIDC_AUDIENCE` env or `""` | Expected OIDC audience for external API bearer tokens. Requires `--oidc-issuer` when set |
| `--oidc-jwks-url` | `ORKA_OIDC_JWKS_URL` env or `""` | Optional JWKS URL. When empty, Orka discovers it from the issuer metadata |
| `--oidc-allowed-subjects` | `ORKA_OIDC_ALLOWED_SUBJECTS` env or `""` | Required comma-separated OIDC subject allowlist patterns when OIDC is enabled |
| `--oidc-namespace` | `ORKA_OIDC_NAMESPACE` env or `default` | Namespace assigned to authorized OIDC callers for namespace isolation |
| `--context-token-profile` | `ORKA_CONTEXT_TOKEN_PROFILE` env or `""` | Context-token profile for external API requests. Currently supports `kontxt` |
| `--context-token-issuer` | `ORKA_CONTEXT_TOKEN_ISSUER` env or `""` | Context-token issuer URL. Requires `--context-token-profile` and `--context-token-audience` when set |
| `--context-token-audience` | `ORKA_CONTEXT_TOKEN_AUDIENCE` env or `""` | Expected context-token audience. Requires `--context-token-profile` and `--context-token-issuer` when set |
| `--context-token-jwks-url` | `ORKA_CONTEXT_TOKEN_JWKS_URL` env or `""` | Optional context-token JWKS URL. For `kontxt`, defaults to `<issuer>/.well-known/jwks.json` |
| `--context-token-headers` | `ORKA_CONTEXT_TOKEN_HEADERS` env or `""` | Comma-separated context-token header locations. Use `Header` for raw tokens or `Header:Scheme` for scheme-prefixed tokens. The `kontxt` default is `Txn-Token` |
| `--context-token-authz-mode` | `ORKA_CONTEXT_TOKEN_AUTHZ_MODE` env or `""` | Context-token authorization mode: `off`, `audit`, or `enforce`. Empty defaults to `off` |
| `--context-token-task-create-scopes` | `ORKA_CONTEXT_TOKEN_TASK_CREATE_SCOPES` env or `""` | Comma-separated scopes authorizing Task creation. Defaults to `orka:tasks:create` |
| `--context-token-task-read-scopes` | `ORKA_CONTEXT_TOKEN_TASK_READ_SCOPES` env or `""` | Comma-separated scopes authorizing Task reads and related data. Defaults to `orka:tasks:get` |
| `--context-token-task-list-scopes` | `ORKA_CONTEXT_TOKEN_TASK_LIST_SCOPES` env or `""` | Comma-separated scopes authorizing Task listing. Defaults to `orka:tasks:list` |
| `--context-token-task-delete-scopes` | `ORKA_CONTEXT_TOKEN_TASK_DELETE_SCOPES` env or `""` | Comma-separated scopes authorizing Task deletion. Defaults to `orka:tasks:delete` |
| `--context-token-tool-read-scopes` | `ORKA_CONTEXT_TOKEN_TOOL_READ_SCOPES` env or `""` | Comma-separated scopes authorizing Tool reads. Defaults to `orka:tools:read` |
| `--context-token-tool-use-scopes` | `ORKA_CONTEXT_TOKEN_TOOL_USE_SCOPES` env or `""` | Comma-separated scopes authorizing Orka-managed chat/OpenAI/Anthropic tool execution. Defaults to `orka:tools:use` |
| `--context-token-provider-use-scopes` | `ORKA_CONTEXT_TOKEN_PROVIDER_USE_SCOPES` env or `""` | Comma-separated scopes authorizing chat/OpenAI/Anthropic model-provider use and model listing. Defaults to `orka:providers:use` |
| `--context-token-secret-read-scopes` | `ORKA_CONTEXT_TOKEN_SECRET_READ_SCOPES` env or `""` | Comma-separated scopes authorizing Secret metadata reads. Defaults to `orka:secrets:read` |
| `--context-token-agent-read-scopes` | `ORKA_CONTEXT_TOKEN_AGENT_READ_SCOPES` env or `""` | Comma-separated scopes authorizing Agent reads. Defaults to `orka:agents:read` |
| `--context-token-agent-write-scopes` | `ORKA_CONTEXT_TOKEN_AGENT_WRITE_SCOPES` env or `""` | Comma-separated scopes authorizing Agent writes. Defaults to `orka:agents:write` |
| `--context-token-memory-read-scopes` | `ORKA_CONTEXT_TOKEN_MEMORY_READ_SCOPES` env or `""` | Comma-separated scopes authorizing memory reads. Defaults to `orka:memory:read` |
| `--context-token-memory-write-scopes` | `ORKA_CONTEXT_TOKEN_MEMORY_WRITE_SCOPES` env or `""` | Comma-separated scopes authorizing memory writes. Defaults to `orka:memory:write` |
| `--context-token-session-read-scopes` | `ORKA_CONTEXT_TOKEN_SESSION_READ_SCOPES` env or `""` | Comma-separated scopes authorizing session reads. Defaults to `orka:sessions:read` |
| `--context-token-session-write-scopes` | `ORKA_CONTEXT_TOKEN_SESSION_WRITE_SCOPES` env or `""` | Comma-separated scopes authorizing session writes/deletes. Defaults to `orka:sessions:write` |
| `--context-token-security-read-scopes` | `ORKA_CONTEXT_TOKEN_SECURITY_READ_SCOPES` env or `""` | Comma-separated scopes authorizing security scan reads. Defaults to `orka:security:read` |
| `--context-token-security-write-scopes` | `ORKA_CONTEXT_TOKEN_SECURITY_WRITE_SCOPES` env or `""` | Comma-separated scopes authorizing security scan creates, updates, deletes, and other mutations. Defaults to `orka:security:write` |
| `--context-token-monitor-read-scopes` | `ORKA_CONTEXT_TOKEN_MONITOR_READ_SCOPES` env or `""` | Comma-separated scopes authorizing repository monitor reads. Defaults to `orka:monitors:read` |
| `--context-token-monitor-write-scopes` | `ORKA_CONTEXT_TOKEN_MONITOR_WRITE_SCOPES` env or `""` | Comma-separated scopes authorizing repository monitor create, update, and delete operations. Defaults to `orka:monitors:write` |
| `--context-token-monitor-operate-scopes` | `ORKA_CONTEXT_TOKEN_MONITOR_OPERATE_SCOPES` env or `""` | Comma-separated scopes authorizing repository monitor manual runs. Defaults to `orka:monitors:operate` |
| `--context-token-skill-read-scopes` | `ORKA_CONTEXT_TOKEN_SKILL_READ_SCOPES` env or `""` | Comma-separated scopes authorizing Skill reads. Defaults to `orka:skills:read` |
| `--context-token-skill-write-scopes` | `ORKA_CONTEXT_TOKEN_SKILL_WRITE_SCOPES` env or `""` | Comma-separated scopes authorizing Skill writes. Defaults to `orka:skills:write` |
| `--context-token-tts-url` | `ORKA_CONTEXT_TOKEN_TTS_URL` env or `""` | kontxt TTS base URL for optional token exchange/replacement |
| `--context-token-tts-audience` | `ORKA_CONTEXT_TOKEN_TTS_AUDIENCE` env or `""` | Audience requested from kontxt TTS exchanges |
| `--context-token-tts-timeout` | `ORKA_CONTEXT_TOKEN_TTS_TIMEOUT` env or `""` | Timeout for kontxt TTS exchanges. Defaults to `5s` when TTS is enabled |
| `--context-token-tts-token-source` | `ORKA_CONTEXT_TOKEN_TTS_TOKEN_SOURCE` env or `""` | Subject token source for TTS exchanges: `serviceAccount`, `incoming`, or `none`. Defaults to `serviceAccount` when TTS is enabled |
| `--context-token-subject-token-type` | `ORKA_CONTEXT_TOKEN_SUBJECT_TOKEN_TYPE` env or `""` | Subject token type for worker-side TTS exchanges. Workers default to TxToken subject tokens when empty |
| `--context-token-child-scope` | `ORKA_CONTEXT_TOKEN_CHILD_SCOPE` env or `""` | Scope workers request for child delegated TxTokens when TTS is configured |
| `--context-token-outbound-scope` | `ORKA_CONTEXT_TOKEN_OUTBOUND_SCOPE` env or `""` | Scope workers request for outbound HTTP Tool TxTokens when TTS is configured |
| `--context-token-child-token-ttl` | `ORKA_CONTEXT_TOKEN_CHILD_TOKEN_TTL` env or `""` | Requested TTL for child delegation TxTokens. Defaults to `5m` when TTS is enabled |
| `--context-token-tool-token-ttl` | `ORKA_CONTEXT_TOKEN_TOOL_TOKEN_TTL` env or `""` | Requested TTL for outbound tool TxTokens. Defaults to `2m` when TTS is enabled |
| `--task-provenance-admission-enabled` | `ORKA_TASK_PROVENANCE_ADMISSION_ENABLED` env or `false` | Enable validating admission that rejects untrusted direct Kubernetes Task writes to Orka-managed provenance fields (`spec.requestedBy`, `spec.transaction`, and transaction metadata labels/annotations) |
| `--task-provenance-admission-trusted-users` | `ORKA_TASK_PROVENANCE_ADMISSION_TRUSTED_USERS` env or controller ServiceAccount usernames | Comma-separated Kubernetes usernames trusted to set Orka-managed Task provenance fields |
| `--task-provenance-admission-trusted-service-accounts` | `ORKA_TASK_PROVENANCE_ADMISSION_TRUSTED_SERVICE_ACCOUNTS` env or `orka-ai-worker` | Comma-separated ServiceAccount names trusted in the target Task namespace to set Orka-managed Task provenance fields for child Task creation |
| `--ai-worker-image` | `ghcr.io/sozercan/orka/ai-worker:latest` | AI worker container image |
| `ORKA_HARNESS_WRAPPER_ENDPOINT` | unset | Required controller environment variable for agent Tasks; points at the CLI harness wrapper HTTP endpoint. |
| `ORKA_HARNESS_WRAPPER_BEARER_TOKEN_FILE` | unset | Optional controller token file for authenticated wrapper endpoints. |

| `--general-worker-image` | `ghcr.io/sozercan/orka/general-worker:latest` | General worker container image |
| `--store-backend` | `sqlite` | Storage backend (sqlite) |
| `--store-path` | `/data/orka.db` | Path to SQLite database file |
| `--chat-enabled` | `true` | Enable the chat endpoint |
| `--chat-provider` | `""` | Default Provider CRD name for chat |
| `--chat-model` | `""` | Default model for chat |
| `--chat-max-iterations` | `50` | Max tool execution loops per chat request |
| `--chat-max-duration` | `30m` | Max wall-clock time per chat request |
| `--chat-tool-timeout` | `60s` | Max time for single tool execution |
| `--chat-max-concurrent` | `10` | Max concurrent chat sessions |
| `--chat-max-tasks-per-turn` | `5` | Max tasks created per chat turn |
| `--chat-max-session-size` | `512000` | Soft limit for session size before truncation (bytes) |
| `--leader-elect` | `false` | Enable leader election |
| `--metrics-bind-address` | `0` | Metrics endpoint address |
| `--health-probe-bind-address` | `:8081` | Health probe address |
| `--metrics-secure` | `true` | Serve metrics via HTTPS |
| `--enable-http2` | `false` | Enable HTTP/2 for metrics and webhook servers |
| `--enable-telemetry` / `--enable-tracing` | `false` | Enable OpenTelemetry traces and metrics (requires worker-reachable OTLP endpoint for worker telemetry) |

### Agent Sandbox Controller Settings

Agent sandbox settings are disabled by default. When enabled, the controller validates `Task.spec.execution.workspace`, resolves/defaults the effective `SandboxWarmPool` and workspace settings, injects the resolved settings into harness wrapper turns, and the worker wrapper owns upstream sandbox claim, execution, and cleanup. Settings can be supplied as flags, environment variables, or Helm values:

| Flag | Environment variable | Helm value | Default |
|------|----------------------|------------|---------|
| `--agent-sandbox-enabled` | `ORKA_AGENT_SANDBOX_ENABLED` | `controller.agentSandbox.enabled` | `false` |
| `--agent-sandbox-router-url` | `ORKA_AGENT_SANDBOX_ROUTER_URL` | `controller.agentSandbox.routerUrl` | empty |
| `--agent-sandbox-default-template` | `ORKA_AGENT_SANDBOX_DEFAULT_TEMPLATE` | `controller.agentSandbox.defaultTemplate` | empty |
| `--agent-sandbox-warm-pool-policy` | `ORKA_AGENT_SANDBOX_WARM_POOL_POLICY` | `controller.agentSandbox.warmPoolPolicy` | `disabled` |
| `--agent-sandbox-namespace-strategy` | `ORKA_AGENT_SANDBOX_NAMESPACE_STRATEGY` | `controller.agentSandbox.namespaceStrategy` | `task` |
| `--agent-sandbox-claim-timeout` | `ORKA_AGENT_SANDBOX_CLAIM_TIMEOUT` | `controller.agentSandbox.claimTimeout` | `2m` |
| `--agent-sandbox-command-timeout` | `ORKA_AGENT_SANDBOX_COMMAND_TIMEOUT` | `controller.agentSandbox.commandTimeout` | `30m` |
| `--agent-sandbox-cleanup-policy` | `ORKA_AGENT_SANDBOX_CLEANUP_POLICY` | `controller.agentSandbox.cleanupPolicy` | `delete` |

Supported values are `disabled` or `template` for the legacy warm pool policy setting, `task` or `controller` for namespace strategy, and `delete` or `retain` for cleanup policy. `task` defaults sandbox claims to the Task namespace; `controller` defaults them to the controller namespace when discoverable, and explicit `templateRef.namespace` values are honored as the claim/warm-pool namespace. See [Agent Sandbox Workspaces](agent-sandbox.md) for examples, live smoke-test steps, and limitations.

When this feature is enabled, harness wrapper pods need RBAC for the upstream sandbox API: create/delete/patch `sandboxclaims`, read `sandboxtemplates`, `sandboxwarmpools`, and `sandboxes`, create `pods/portforward`, and read `endpointslices`. The Helm chart and generated worker RBAC include these permissions; custom deployments must include equivalent rules for the worker ServiceAccount.

### External API OIDC Authentication

ServiceAccount bearer token authentication is always available. To allow external callers such as GitHub Actions to authenticate directly with OIDC JWTs, configure issuer, audience, an explicit subject allowlist, and the namespace assigned to OIDC callers:

```bash
--oidc-issuer=https://token.actions.githubusercontent.com
--oidc-audience=orka-ci
--oidc-allowed-subjects=repo:my-org/my-repo:ref:refs/heads/main
--oidc-namespace=ci
```

The same settings can be supplied with environment variables:

```bash
ORKA_OIDC_ISSUER=https://token.actions.githubusercontent.com
ORKA_OIDC_AUDIENCE=orka-ci
ORKA_OIDC_ALLOWED_SUBJECTS=repo:my-org/my-repo:ref:refs/heads/main
ORKA_OIDC_NAMESPACE=ci
# Optional; when omitted, Orka discovers the JWKS URL from the issuer metadata.
ORKA_OIDC_JWKS_URL=https://token.actions.githubusercontent.com/.well-known/jwks
```

OIDC validation requires RS256-signed JWTs with matching `iss` and `aud`, valid time claims, a non-empty `sub`, and a `sub` value that matches `--oidc-allowed-subjects`. Wildcards `*` and `?` are supported in allowlist patterns; use the narrowest GitHub Actions subject for the trusted repository, branch, environment, or workflow. Authorized OIDC callers are bound to `--oidc-namespace` (or `default` when omitted) so namespace isolation can reject requests for other namespaces. When an OIDC-authenticated caller creates a Task, Orka records the verified identity in `spec.requestedBy`. Clients cannot set `requestedBy` themselves.

### External API Context-Token Authentication

Orka can also authenticate external API requests with generic transaction/context tokens. The built-in `kontxt` profile validates RS256-signed JWTs with JOSE header `typ: txntoken+jwt`, matching `iss` and `aud`, valid time claims, a non-empty `sub`, and the required `kontxt` claims `iat`, `txn`, `scope`, and `req_wl`.

For a newcomer-friendly setup and smoke test, see [Kontxt quickstart: use Kubernetes identity to call Orka without long-lived tokens](../guides/kontxt-quickstart.md).

Enable the profile by configuring the profile, issuer, and audience:

```bash
--context-token-profile=kontxt
--context-token-issuer=https://issuer.example.com
--context-token-audience=orka-api
```

The same settings can be supplied with environment variables:

```bash
ORKA_CONTEXT_TOKEN_PROFILE=kontxt
ORKA_CONTEXT_TOKEN_ISSUER=https://issuer.example.com
ORKA_CONTEXT_TOKEN_AUDIENCE=orka-api
# Optional for kontxt; when omitted, Orka uses <issuer>/.well-known/jwks.json.
ORKA_CONTEXT_TOKEN_JWKS_URL=https://issuer.example.com/.well-known/jwks.json
```

By default, the `kontxt` profile reads raw transaction tokens from the `Txn-Token` header:

```bash
curl -H "Txn-Token: $TXN_TOKEN" https://orka.example.com/api/v1/tasks
```

To customize token locations, set `--context-token-headers` or `ORKA_CONTEXT_TOKEN_HEADERS` to a comma-separated list. Use `Header` for raw token headers and `Header:Scheme` for scheme-prefixed headers. For example, keep the default `Txn-Token` header and explicitly opt in to `Authorization: Bearer` context-token support:

```bash
--context-token-headers=Txn-Token,Authorization:Bearer
```

`Authorization: Bearer` remains the default location for Kubernetes ServiceAccount and OIDC JWT authentication. Context-token bearer authentication is only attempted when `Authorization:Bearer` is explicitly configured and the bearer JWT has `typ: txntoken+jwt`; other bearer tokens continue through the standard OIDC or Kubernetes TokenReview flow. When an external context-token caller creates a Task, Orka records the verified subject and issuer in immutable `spec.requestedBy` and records safe transaction metadata in immutable `spec.transaction`, transaction labels, and transaction annotations. Clients cannot set `requestedBy` or `transaction` themselves.

Optional authorization is controlled by `--context-token-authz-mode` / `ORKA_CONTEXT_TOKEN_AUTHZ_MODE`. In `audit` mode, Orka logs safe authorization failures and allows the request. In `enforce` mode, Orka rejects context-token callers that lack the configured operation scope or violate signed `tctx` constraints. Task creation can be constrained by `tctx.namespace`, `tctx.taskType`, `tctx.agent`, `tctx.allowedAgents`, workspace `tctx.repo`/`tctx.branch`/`tctx.ref`, and `tctx.allowedTools`. Chat, OpenAI-compatible, and Anthropic-compatible model calls require the provider-use scope (default `orka:providers:use`) and honor `tctx.namespace`, `tctx.provider`, `tctx.allowedProviders`, `tctx.model`, and `tctx.allowedModels`. When Orka-managed server-side tools are exposed to those endpoints, they also require the tool-use scope (default `orka:tools:use`) and honor `tctx.allowedTools`. Security scan read/list/get endpoints require the security-read scope (default `orka:security:read`), and security scan create/update/delete and mutation endpoints require the security-write scope (default `orka:security:write`). Repository monitor read endpoints require the monitor-read scope (default `orka:monitors:read`), monitor create/update/delete endpoints require the monitor-write scope (default `orka:monitors:write`), and manual monitor runs require the monitor-operate scope (default `orka:monitors:operate`). Repository monitor access can also be constrained by `tctx.namespace`, `tctx.repo`, `tctx.branch`, `tctx.agent`, and `tctx.allowedAgents`. The raw TxToken is never logged or persisted in Task specs/status.

### Kontxt TTS Exchange and Propagation

Configure `--context-token-tts-url` / `ORKA_CONTEXT_TOKEN_TTS_URL` when workers should exchange a mounted subject token for child or outbound replacement TxTokens. Delegation tools require `ORKA_CONTEXT_TOKEN_SUBJECT_TOKEN_FILE` and `ORKA_CONTEXT_TOKEN_CHILD_SCOPE`; HTTP Tool calls can use `ORKA_CONTEXT_TOKEN_OUTBOUND_SCOPE` or fall back to the current transaction scope. Child scopes are fail-closed: Orka rejects a requested child scope that is not already present in the parent transaction scopes before it creates the child Task.

Successful delegation exchanges store the raw child TxToken only in an owner-referenced Kubernetes Secret and annotate the child Task with the Secret name. The controller mounts that Secret into the child worker and sets `ORKA_TRANSACTION_TOKEN_FILE` / `ORKA_CONTEXT_TOKEN_SUBJECT_TOKEN_FILE` so deeper delegation and downstream Tool calls can continue the same transaction with configured child/outbound scopes.

### Task Provenance Admission Hardening

The REST API rejects client-supplied `requestedBy` and `transaction` fields and stamps verified provenance itself. To also protect direct Kubernetes `Task` CRD writes, enable the optional validating admission webhook:

```bash
--task-provenance-admission-enabled=true
```

The webhook denies untrusted `CREATE` or `UPDATE` requests that set or modify Orka-managed provenance fields: `spec.requestedBy`, `spec.transaction`, `orka.ai/transaction-*` labels/annotations, `orka.ai/context-token-profile`, and the child token Secret annotation. By default, trusted writers are the Orka controller ServiceAccount usernames in the controller namespace and the `orka-ai-worker` ServiceAccount name in the target Task namespace; override them with `--task-provenance-admission-trusted-users` and `--task-provenance-admission-trusted-service-accounts`.

Admission deployment is opt-in. To install the manifests, uncomment the `[WEBHOOK]` resource and patch in `config/default/kustomization.yaml`, provide a `webhook-server-cert` TLS Secret for the manager, and set the webhook `caBundle` (or configure certificate-manager CA injection) before applying the webhook configuration. The bundled webhook manifest defaults to `failurePolicy: Ignore`; switch it to `Fail` only after webhook TLS and availability are configured.

## Prometheus Metrics

Orka registers the following Prometheus metrics on the controller-runtime registry. Enable monitoring with the Helm chart:

```yaml
monitoring:
  enabled: true
  interval: 30s
```

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `orka_api_requests_total` | Counter | `endpoint`, `method`, `status` | Total API requests (status bucketed as `2xx`/`4xx`/`5xx`) |
| `orka_api_request_duration_seconds` | Histogram | `endpoint`, `method` | API request latency in seconds |
| `orka_skills_loaded_total` | Counter | `skill`, `namespace` | Skills loaded by namespace and name |
| `orka_store_db_size_bytes` | Gauge | — | Size of the SQLite database file in bytes |
| `orka_context_token_auth_total` | Counter | `profile`, `result` | Context-token authentication attempts |
| `orka_context_token_authorization_total` | Counter | `action`, `result`, `reason` | Context-token authorization decisions (allow/deny/audit) |
| `orka_context_token_tts_exchange_total` | Counter | `result`, `reason` | kontxt TTS token-exchange attempts |
| `orka_context_token_tts_exchange_duration_seconds` | Histogram | `result`, `reason` | kontxt TTS token-exchange latency in seconds |

Context-token metrics are described in more detail in [Kontxt TxToken integration](kontxt.md#observability). All context-token labels use low-cardinality values only.

## OpenTelemetry telemetry

Orka supports opt-in OpenTelemetry traces and GenAI metrics for debugging,
performance analysis, and backend cost/latency dashboards. Telemetry is
disabled by default and uses OpenTelemetry no-op providers until enabled.

### Enabling telemetry

Add the `--enable-telemetry` flag to the controller and configure an OTLP
collector endpoint. The legacy `--enable-tracing` alias enables the same traces
and metrics:

```yaml
args:
  - --enable-telemetry
env:
  - name: OTEL_EXPORTER_OTLP_ENDPOINT
    value: "jaeger-collector.observability.svc:4317"
  - name: OTEL_EXPORTER_OTLP_INSECURE
    value: "true"
```

| Flag / Environment Variable | Default | Description |
|------------------------------|---------|-------------|
| `--enable-telemetry` / `--enable-tracing` | `false` | Enable OpenTelemetry traces and metrics |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | SDK default `localhost:4317` | OTLP collector endpoint for traces and metrics |
| `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` | unset | Trace-specific OTLP endpoint |
| `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` | unset | Metrics-specific OTLP endpoint |
| `OTEL_EXPORTER_OTLP_PROTOCOL` | SDK default gRPC | Set to `http/protobuf` for OTLP/HTTP collectors |
| `OTEL_EXPORTER_OTLP_TRACES_PROTOCOL` / `OTEL_EXPORTER_OTLP_METRICS_PROTOCOL` | unset | Signal-specific exporter protocol overrides |
| `OTEL_EXPORTER_OTLP_INSECURE` and signal-specific insecure vars | SDK default | Disable TLS for in-cluster/dev collectors that require it |
| `OTEL_TRACES_SAMPLER` / `OTEL_TRACES_SAMPLER_ARG` | SDK default | Standard OpenTelemetry sampler configuration |

Controller-local defaults such as `localhost:4317` are valid only for the
controller process. AI worker Jobs receive telemetry enablement only when the
controller has a non-loopback, worker-reachable OTLP endpoint. The controller
copies non-secret OTLP endpoint/protocol/insecure/timeout/compression settings
to AI worker Pods and intentionally does not copy OTLP headers, certificate or
client-key env vars, `OTEL_RESOURCE_ATTRIBUTES`, or baggage.

Harness-wrapper and agent-runtime worker telemetry is explicit opt-in. Set
`ORKA_ENABLE_TELEMETRY=true` and OTLP exporter env on those workloads when you
want their process-local `task.run` spans exported.

### Instrumented Components

| Tracer | Span | Attributes |
|--------|------|------------|
| `orka.api` | HTTP/API middleware spans | HTTP request/route/status metadata |
| `orka.chat` | `chat.request`, `chat.tool_loop.iteration` | session metadata; `chat.iteration`, `orka.tenant`, requested model, tool-call count |
| `orka.worker` / `orka.harness` | `task.run` | `orka.task.id`, `orka.task.namespace`, `orka.tenant`, `orka.agent.name` when known |
| `orka.agent` | `agent.step` | iteration, requested model/provider, tool-call count, Orka task metadata |
| `orka.gen_ai` | `chat {model}` | `gen_ai.*` provider/model/token metadata and `error.type` |
| `orka.gen_ai` | `execute_tool {tool.name}` | `gen_ai.tool.*`, `orka.tool.name`, `orka.tool.kind`, `orka.tool.result.size_bytes`, parent/child task fields for delegation |
| `orka.controller` | `task.reconcile` | task name, namespace, type, and propagated trace context |

Use `orka.task.id` to find a Task trace, `orka.tool.name` to find specific tool
executions, and `orka.parent_task.id` / `orka.child_task.id` to follow delegated
children. Tool spans do not include raw arguments or result bodies.

### Example: Jaeger Setup

```bash
# Deploy Jaeger all-in-one (development only)
kubectl create namespace observability
kubectl apply -n observability -f https://raw.githubusercontent.com/jaegertracing/jaeger-operator/main/examples/simplest.yaml

# Configure the controller
kubectl set env deployment/orka-controller \
  OTEL_EXPORTER_OTLP_ENDPOINT=jaeger-collector.observability.svc:4317
```

## Context Engineering Best Practices

Research on LLM agent context files ([arxiv 2602.11988](https://arxiv.org/abs/2602.11988)) shows that verbose context hurts more than it helps: LLM-generated context files reduce task success rates by 0.5–2% while increasing inference costs by 20–23%. Even developer-written files yield only marginal improvements (~4%) with similar cost increases. The guidelines below translate these findings into practical advice for Orka's `systemPrompt`, `Skill`, and `Agent` configuration.

### Writing Effective System Prompts

Keep Agent `systemPrompt` content **minimal and requirement-focused**:

- **Include only**: tooling commands (build/test/lint invocations), non-discoverable gotchas (e.g., "provider secret key defaults to `api-key`"), and hard constraints the agent cannot infer from source code.
- **Avoid codebase overviews** — agents discover project structure efficiently on their own through file listing and search tools. Overviews add tokens without improving navigation speed.
- **Don't duplicate** information already present in `website/docs/`, `README`, or inline code comments. Redundant instructions increase reasoning token usage (14–22% more) without improving outcomes.

### Writing Effective Skills

Skills are prepended to the system prompt on **every LLM call** for every task that uses the parent Agent. Each Skill directly increases per-request token cost.

- Keep Skill `content` concise and **action-oriented** — write instructions ("run `make lint-fix` after changes"), not descriptions ("this project uses a Makefile-based build system").
- Split large Skills so Agents only reference the ones they need. A research Agent doesn't need a coding-standards Skill.
- Regularly audit Skill content and remove instructions the agent follows by default.

### Monitoring Recommendations

- Track `orka_task_duration_seconds` and LLM token metrics — more instructions ≠ better outcomes.
- A/B test agent performance with and without specific `systemPrompt` or `Skill` content to validate that each addition provides measurable benefit.
- Well-documented repositories benefit least from additional context; focus context engineering effort on repos with limited existing documentation.
