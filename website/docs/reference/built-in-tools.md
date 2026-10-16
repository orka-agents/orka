---
slug: /built-in-tools
description: "Every tool Orka gives agents: which Tasks get it, how to turn it on, and its parameters."
---

# Built-in tools

Orka ships tools that agents can call without any extra infrastructure. This page lists all
of them, which Tasks receive them, and their parameters. To give agents your own tools, see
[Agents, skills, and tools](../guides/agents-skills-and-tools.md).

## Who gets which tools

Native `type: ai` Tasks run Orka's AI worker, which builds its tool list from three sources:

1. **Tools you select.** Names listed in the Agent's `spec.tools` (entries with
   `enabled: false` are skipped) and in the Task's `spec.ai.tools`. A name can be a
   [core tool](#core-tools), an [opt-in GitHub tool](#github-tools), or a custom
   [`Tool`](resources.md#tool) resource.
2. **Tools Orka adds automatically.**
   - Every `type: ai` Task gets the [memory tools](#memory-tools).
   - An Agent with `coordination.enabled: true` gets the [coordination tools](#coordination-tools),
     the [GitHub pull request tools](#github-tools), [agent management](#agent-management-tools),
     and `update_plan`. With `coordination.autonomous: true` it also gets `request_approval`.
   - A delegated child Task gets `send_message` and `check_messages`. So does a child that runs
     on a built-in coding-agent runtime.
   - A Task that came in through a [gateway](../guides/gateways.md) can get `reply_in_conversation`.
3. **Nothing else.** The annotation `orka.ai/disable-coordination-tool-injection: "true"` on a
   Task turns off the automatic coordination, approval, and child-messaging tools. Memory tools
   stay, and tools you selected explicitly still apply.

Coding-agent Tasks (`type: agent`) use their CLI's own tools, such as `Read`, `Grep`, and
`Bash`, controlled by the Agent's `runtime.defaultAllowedTools` and the Task's
`agentRuntime` overrides. Orka-executed tools reach them through a per-session MCP broker that
rechecks the Task's policy on every call. See [Agent runtimes](../concepts/agent-runtimes.md).

When transaction-token authorization restricts `tctx.allowedTools`, the grant must cover the
automatically added tools as well as the selected ones. See
[Restricted tool grants](../concepts/transaction-tokens.md#restricted-tool-grants).

## Core tools

Select these by name in `spec.tools` or `spec.ai.tools`. The OpenAI- and Anthropic-compatible
endpoints also offer the first five to clients.

| Tool | Description | Parameters |
|------|-------------|------------|
| `web_search` | Search the web using a configured search API or DuckDuckGo | `query` (required), `limit` (default 5) |
| `code_exec` | Execute code in a sandboxed environment | `language` (python/javascript/bash), `code`, `timeout` (default 30s, clamped to 60s) |
| `file_read` | Read files from the workspace | `path`, `offset`, `limit` (max 1MB) |
| `web_fetch` | Fetch and extract URL content | `url` (required), `max_chars` (default 50000), `raw` |
| `file_write` | Write or append files in workspace paths | `path` (required), `content` (required), `mode` (`write`/`append`), `create_dirs` |
| `request_approval` | Park the Task until a person approves or declines one exact custom-tool call. Added automatically for autonomous coordinators; see [human approval](../guides/autonomous-tasks.md#human-approvals). | `action`, `targetTool` (a custom Tool name), `targetArguments` (the exact argument object) (required); `riskSummary`, `severity` (`warning`/`critical`) |

## Memory tools

Every `type: ai` Task receives these. `remember` and `propose_memory` create proposals for a
person to review; they never change durable memory directly. See [Memory](../concepts/memory.md).

| Tool | Description | Parameters |
|------|-------------|------------|
| `recall_memory` | Recall durable namespace-scoped memories | `query`, `tags`, `task_name`, `agent_name`, `source`, `limit`, `include_disabled` |
| `remember` | Submit a durable memory proposal for review | `content` (required); `title`, `description`, `tags`, `agent_name` |
| `propose_memory` | Submit a memory-adjacent governance proposal | `title` (required); `type`, `skill_name`, `description`, `content`, `patch`, `agent_name` |
| `search_transcript` | Search prior session transcripts | `query` (required); `session_name`, `exclude_session_name`, `roles`, `limit`, `max_snippet_length` |

## Coordination tools

Added for Agents with `coordination.enabled: true`. They are not returned by
`GET /api/v1/tools`. [Multi-agent coordination](../guides/multi-agent-coordination.md) explains
how to use them together.

| Tool | Description | Parameters |
|------|-------------|------------|
| `delegate_task` | Delegate a subtask to another agent | `agent`, `prompt` (required); `taskNamespace`, `agentNamespace`, `priority`, `timeout`, `workspace`, `maxTurns`, `allowBash`, `prior_task`, `feedback`, `auto_retry`, `max_retries` |
| `wait_for_tasks` | Wait for delegated tasks to complete | `tasks` (required), `timeout` (default 10m) |
| `create_container_task` | Create a child container task | `image`, `command`/`args`; `workspace`, `prior_task`, `namespace`, `timeout`, `priority`, `schedule` |
| `cancel_task` | Cancel a running child task | `task_name` (required); `namespace`, `reason` |
| `send_message` | Send a message to a sibling task | `to_task` (required, or `*` to broadcast), `content` (required) |
| `check_messages` | Check for messages from sibling tasks | `mark_read` (boolean, default true) |
| `update_plan` | Update the autonomous execution plan | `summary`, `plan_document` (required); `progress_pct`, `goal_complete` |

### `delegate_task`

Creates a child Task for the named Agent and returns `{"taskName": "<name>", "status": "created"}`.
The child is `type: agent` when the target Agent has `spec.runtime`, and `type: ai` otherwise.

| Parameter | Description |
| --- | --- |
| `agent` | Name of the Agent to delegate to. Required. |
| `prompt` | The child Task's prompt. Required. |
| `taskNamespace` | Namespace for the child Task. Defaults to the parent's. |
| `agentNamespace` | Namespace of the target Agent. Defaults to `taskNamespace`. (`namespace` is the legacy form that sets both.) |
| `priority` | 0–1000. Defaults to the parent's priority. |
| `timeout` | Child Task timeout, such as `20m`. |
| `workspace` | Repository settings for a coding-agent child, below. |
| `maxTurns`, `allowBash` | Per-Task runtime overrides for a coding-agent child. |
| `prior_task`, `feedback` | Apply a completed Task's diff first and prepend review feedback to the prompt. Native children only. |
| `auto_retry`, `max_retries` | Report structured failure details for the coordinator to act on; see [retrying failed children](../guides/multi-agent-coordination.md#retry-failed-children). `max_retries` defaults to 2. |

`workspace` takes the same fields as a Task [workspace](resources.md#workspace), with Secret
names as plain strings:

| Field | Description |
| --- | --- |
| `gitRepo` | Credential-free HTTPS source URL. Required for write intent. |
| `branch` / `ref` | Source branch, or an exact commit, tag, or `refs/heads/...` ref. With neither, the repository's default branch is resolved and frozen. |
| `intent` | `read` (default) or `write`. Publication fields require `write`. |
| `readCredentialRef` | Secret for clone and read. |
| `publicationGitRepo` | Credential-free HTTPS repository that receives the branch. |
| `publicationReadCredentialRef` | Secret for target preflight and verification. |
| `publicationCredentialRef` | Secret for the branch push. Required for write intent. |
| `forgeCredentialRef` | Secret for the pull request API. Required when `createPR` is true. |
| `pushBranch` | Publication branch. |
| `prBaseBranch` | Pull request base branch. Required when `createPR` is true. |
| `prTitle`, `prBody` | Pull request text, up to 256 and 32,768 characters. |
| `createPR` | Reconcile a pull request after verified publication. |

Keep the four credential roles on distinct Secrets.

`delegate_task` and chat's `create_agent_task` both accept optional `workspace.prTitle` and
`workspace.prBody`. When requesting a pull request, the agent creating the Task should write
descriptive text from the request before execution, not from the executing sandbox's output.
Omitting either field or supplying an empty string keeps its fallback. Metadata alone does not
enable `createPR` or write intent. Nonempty whitespace-only titles are invalid. Both fields
reject secret-like text, and bodies reject reserved publisher reconciliation comments.
Validation also applies when `createPR` is false.

With `prior_task`, the child copies the prior Task's workspace configuration when none is
given, and Orka tracks the loop with two labels: `orka.ai/iteration` (1, 2, 3, ...) and
`orka.ai/iteration-group` (shared by every iteration of the same logical task). Built-in
coding-agent runtimes do not accept `prior_task`, and external `runtimeRef` runtimes reject
it. For those, have the next agent read the published branch instead; see
[code, review, and publish loops](../guides/multi-agent-coordination.md#code-review-and-publish-loops).

### `wait_for_tasks`

Polls the named children every five seconds until each is `Succeeded`, `Failed`, or
`Cancelled`, or until `timeout` passes, then returns their results. Results are read from
the controller, and any `diff` field is stripped so it does not flood the coordinator's context.

```json
{
  "completed": true,
  "results": [
    {"task": "name", "agent": "agent", "phase": "Succeeded", "result": "..."},
    {"task": "name", "agent": "agent", "phase": "Failed", "result": "error: ..."}
  ]
}
```

A child delegated with `auto_retry: true` that failed carries `failureDetails` with its
message, `retryCount`, and `maxRetries`. The tool does not retry it; see
[retrying failed children](../guides/multi-agent-coordination.md#retry-failed-children).

### `cancel_task`

Asks the controller to cancel a child. The controller checks that the caller is the
parent's current worker, that the child carries the protected parent link (so Task
provenance admission is required), and that the child is `Pending` or `Running`. Children
running on a coding-agent runtime are refused. The child then completes as `Cancelled`
through the normal path, which deletes its worker Job and releases any session lock.
The tool returns `{"taskName": ..., "status": "cancellation requested"}`, or the child's
phase when it has already finished.

### `send_message` and `check_messages`

Siblings — children of the same coordinator — can message each other through the
controller. `to_task: "*"` broadcasts to every sibling except the sender. `check_messages`
returns unread messages addressed to the Task or broadcast to its siblings, and marks them
read unless `mark_read` is false. Messages are deleted with the Tasks that own them.

## GitHub tools

Coordinator Agents get the pull request tools automatically. The issue tools, `list_issues`,
`get_issue`, and `comment_on_issue`, need an explicit `spec.tools` entry. A person can also
let these tools act through their own linked account; see [Connectors](../guides/connectors.md).

Every GitHub tool that accepts `repo_url` checks it against the current Task's workspace
repository or its signed transaction repository context. A mismatch fails before Orka reads
a credential or calls GitHub. Tools that accept `task_name` read the repository and
credentials from that Task's workspace: tools that write to GitHub (`create_pull_request`,
`post_review_comment`, `comment_on_issue`, `merge_pull_request`, `auto_merge_pull_request`)
use its `forgeCredentialRef` and never fall back to
another credential, and read tools use its `readCredentialRef`.

### `create_pull_request`

Creates a pull request from a branch that Orka independently verified for a completed agent
Task. It calls the GitHub API with the child Task's `workspace.forgeCredentialRef`, or the
requester's linked account, and fails if neither is available; it never falls back to the push
or read credential. The forge credential never reaches the coding-agent runtime. Require a terminal verified delivery receipt before calling it: a model
saying it pushed is not evidence.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `task_name` | string | yes | Name of the completed child Task whose workspace and delivery receipt identify the publication repository/branch |
| `head_branch` | string | yes | Branch containing the changes (the `pushBranch` from the coder task) |
| `base_branch` | string | yes | Target branch to merge into (e.g. `main`) |
| `title` | string | yes | Pull request title |
| `body` | string | no | Pull request body in Markdown |

### `check_pull_request_ci`

Checks GitHub CI status for a pull request without merging it.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `pr_number` | integer | yes | GitHub pull request number to inspect |
| `task_name` | string | no | Name of the child task whose workspace config has the repo and git credentials |
| `repo_url` | string | no | GitHub repository URL, scope-checked as above |
| `wait_timeout` | string | no | Maximum time to wait for pending checks, e.g. `30m`. Empty means one immediate check |
| `poll_interval` | string | no | Delay between checks while waiting, e.g. `30s`. Defaults to `30s` when waiting |

```json
{
  "status": "failed",
  "pr_number": 42,
  "head_sha": "abc123",
  "checks_passed": false,
  "checks_failed": true,
  "checks_pending": false,
  "checks_details": "lint (conclusion=failure)",
  "wait_timed_out": false,
  "attempts": 1,
  "message": "one or more CI checks failed"
}
```

`status` is one of `passed`, `failed`, `pending`, `no_checks`, `closed`, `merged`, or `unknown`.

### `review_pull_request`

Fetches a pull request's unified diff, per-file patches with change statistics, and metadata
(title, body, author, branches).

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `pr_number` | integer | yes | GitHub pull request number to review |
| `task_name` | string | no | Name of the child task whose workspace config has the repo and git credentials |
| `repo_url` | string | no | GitHub repository URL, scope-checked as above |

```json
{
  "pr_title": "feat: add auth middleware",
  "pr_body": "Adds JWT-based authentication...",
  "pr_author": "dev-bot",
  "base_branch": "main",
  "head_branch": "feature/jwt-auth",
  "diff": "diff --git a/auth.go ...",
  "files": [
    {"filename": "auth.go", "status": "added", "additions": 42, "deletions": 0, "patch": "..."}
  ],
  "status": "fetched"
}
```

### `post_review_comment`

Posts a review with a verdict and optional line comments.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `pr_number` | integer | yes | GitHub pull request number |
| `body` | string | yes | Top-level review body text |
| `event` | string | yes | Review verdict: `APPROVE`, `REQUEST_CHANGES`, or `COMMENT` |
| `comments` | array | no | Line-level comments, each with `path` (relative to the repository root), `line` (new-file line number), and `body` |
| `task_name` | string | no | Name of the child task whose workspace config has the repo and git credentials |
| `repo_url` | string | no | GitHub repository URL, scope-checked as above |

Returns the `review_id`, `status: "submitted"`, and the review's `html_url`.

### `merge_pull_request` and `auto_merge_pull_request`

`merge_pull_request` checks CI once and merges only when every check passed. Otherwise it
returns `{"merged": false, "checks_passed": false, "message": "CI checks have not all passed: ..."}`
without failing. `auto_merge_pull_request` instead polls checks every 30 seconds until they
pass, fail, or the pull request closes, and tolerates transient GitHub errors (429 and 5xx).

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `task_name` | string | yes | Name of the child task whose workspace config has the repo and git credentials |
| `pr_number` | integer | yes | GitHub pull request number to merge |
| `merge_method` | string | no | `merge`, `squash`, or `rebase` (defaults to `squash`) |
| `commit_title` | string | no | Custom merge commit title |
| `commit_message` | string | no | Custom merge commit message |
| `timeout` | string | no | `auto_merge_pull_request` only: maximum wait, e.g. `30m` or `1h` (defaults to `30m`) |

`auto_merge_pull_request` reports an `outcome`: `merged`, `ci_failed` (returned at once with
`checks_details`), `closed`, `already_merged`, or `timeout`.

### `list_pull_requests` and `list_issues`

List open pull requests (number, title, author, branches, labels, URL) or open issues
(number, title, body truncated to 500 characters, labels, author). `list_issues` returns only
unassigned issues by default.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `task_name` | string | no | Name of a task whose workspace config has the repo and git credentials |
| `repo_url` | string | no | GitHub repository URL, scope-checked as above |
| `per_page` | integer | no | Results per page (default 30, max 100) |
| `page` | integer | no | Page number (default 1) |
| `unassigned_only` | boolean | no | `list_issues` only: return issues with no assignee (default `true`) |

At least one repository source must be available: `task_name`, the current Task, or `ORKA_GIT_REPO`.

### `get_issue` and `comment_on_issue`

`get_issue` fetches an issue's title, body, labels, assignees, state, and first page of
comments. `comment_on_issue` posts a Markdown comment.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `issue_number` | integer | yes | GitHub issue number |
| `body` | string | `comment_on_issue` only | Comment text |
| `task_name` | string | no | Name of a task whose workspace config has the repo and git credentials |
| `repo_url` | string | no | GitHub repository URL, scope-checked as above |

### `check_pr_review_marker`

Checks whether a pull request already has an Orka review marker for its current head, so a
scheduled reviewer does not review the same head twice.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `pr_number` | integer | yes | GitHub pull request number to inspect |
| `task_name` | string | no | Name of a task whose workspace config has the repo and git credentials |
| `repo_url` | string | no | GitHub repository URL, scope-checked as above |
| `head_sha` | string | no | PR head SHA to check. If omitted, the tool fetches the current PR head SHA. |

The tool returns a hidden marker to include unchanged in the later `post_review_comment`
body. It binds the repository, pull request number, and head SHA:

```html
<!-- orka:pr-review repo=owner/repo pr=123 head_sha=abc123 sig=... -->
```

Marker signatures do not depend on the live GitHub token, so they survive token rotation.
These variables are read by the worker Task that runs the tool:

| Environment variable | Description |
|----------------------|-------------|
| `ORKA_PR_REVIEW_MARKER_SECRET` | Optional stable HMAC key for marker signatures. Use a Kubernetes Secret or another secret injection path; never store it in the repository. |
| `ORKA_PR_REVIEW_MARKER_PREVIOUS_SECRETS` | Optional comma-separated previous keys, accepted until reviews signed with them have aged out. |
| `ORKA_PR_REVIEW_MARKER_TRUSTED_AUTHOR` | Optional GitHub login trusted for legacy markers and markers signed before a dedicated secret was configured. When omitted, Orka resolves the authenticated GitHub user for the Task credential. |

### `create_pr_monitor`

Creates a scheduled `type: ai` Task that reviews one repository's pull requests on a cron
schedule. It is offered by chat and its management tools rather than to every coordinator.
[Repository monitors](../guides/repository-monitors.md) are the durable replacement: they keep
per-PR runs, items, review records, and audit events that this tool does not.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `name` | string | yes | Human-readable monitor name. The created Task receives an Orka-generated name. |
| `repo_url` | string | yes | Credential-free GitHub repository root URL, such as `https://github.com/owner/repo`, `https://github.com/owner/repo.git`, or `git@github.com:owner/repo.git`. Stored in the created Task's `spec.workspace.gitRepo`. |
| `schedule` | string | yes | Cron schedule for the monitor Task. |
| `agent_ref` | string | yes | AI Agent for the monitor Task. It must have coordination enabled and autonomous coordination disabled. |
| `namespace` | string | no | Namespace for the monitor Task. Defaults to the current task namespace. |
| `provider_ref` | string | no | Optional Provider reference for the scheduled AI Task. |
| `gitSecretRef` | string | no | Secret mapped to the created Task's `spec.workspace.readCredentialRef`. |
| `per_page` | integer | no | Maximum open PRs to scan per run. Defaults to `30`, maximum `100`. |
| `review_event` | string | no | `COMMENT`, `APPROVE`, or `REQUEST_CHANGES`. Defaults to `COMMENT`. |
| `prompt` | string | no | Additional instructions appended to the generated monitor prompt. |

Pull request, issue, branch/tree, blob/file, commit, query-string, fragment, non-GitHub,
HTTP, and embedded-credential URLs are rejected before the Task is created, so a copied
browser URL cannot widen the monitor's scope. Without `gitSecretRef`, Orka looks for
`git-credentials`, `github-credentials`, `copilot-token`, `github-token`, and `git-token` in
the target namespace; the Secret must contain a non-empty `token`, `password`, or
`GITHUB_TOKEN` key.

The created Task receives only `list_pull_requests`, `check_pr_review_marker`,
`check_pull_request_ci`, `review_pull_request`, and `post_review_comment`, and its prompt
tells it to pass the same `repo_url` to each. Because those tools are scoped to the Task's
repository, the monitor cannot use its credential on another repository by changing an
argument.

## Agent management tools

Coordinators can create the specialists they need at runtime; see the
[self-bootstrapping example](https://github.com/orka-agents/orka/tree/main/examples/self-bootstrapping).

| Tool | Description | Parameters |
|------|-------------|------------|
| `create_agent` | Create an Agent at runtime | `name`, `provider`, `model` (required); `systemPrompt` (role instructions), `soul` (optional persona source), `tools`, `coordination` |
| `delete_agent` | Delete an Agent | `name` (required), `namespace` |

## Tools for specific features

| Tool | Where it appears | Read more |
| --- | --- | --- |
| `create_ai_task`, `create_agent_task`, `create_container_task`, `check_task_progress`, `fetch_task_output`, `wait_for_task`, `cancel_task`, `list_agents`, `list_tools`, `list_tasks`, plus on-demand `create_agent`, `update_agent`, `delete_agent`, `create_tool`, `delete_tool`, `delete_session` | The chat orchestrator | [Interactive chat](../guides/chat.md#available-tools) |
| `list_connections` | Chat, compatibility-API coordinator mode, and coding-agent runtimes, when connectors are enabled | [Connectors](../guides/connectors.md#ask-what-is-linked-list_connections) |
| `reply_in_conversation` | Tasks created by a gateway whose adapter supports interim messages | [Replying while the agent continues](../operations/gateways.md#replying-while-the-agent-continues) |
| `run_validation` | Pull request reviewers of a repository monitor with `spec.validation.image` | [Review workspace context](../guides/repository-monitors.md#review-workspace-context) |
