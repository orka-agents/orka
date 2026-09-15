---
description: "Native tool policy, pinned runner support, diagnostics, and execution tests for built-in ACP runtimes."
---

# Native runner tools

`Agent.spec.runtime.toolPolicy` selects `full` or `restricted` native tool
access for a built-in `orka.harness.v2` runtime. Omitting it preserves the
legacy behavior. The setting belongs to the operator; Tasks, prompts,
repository settings, and agent-facing Agent management tools cannot select it.
See [Agent runtimes](../concepts/agent-runtimes.md#native-tool-policy) for a
full-mode Agent example.

## Pinned runner behavior

Orka approves an exact runner and adapter image. Shared names such as
`WebSearch` describe policy grants; they do not prove that each runner provides
the same tool. The versions below come from
[`internal/acp/pins.go`](https://github.com/orka-agents/orka/blob/main/internal/acp/pins.go).

| Runner and adapter | Policy omitted | Explicit full policy | Explicit restricted policy |
| --- | --- | --- | --- |
| Codex CLI `0.145.0`, ACP `1.1.7` | Keeps the existing unrestricted or legacy read-intent preset. The latter is not an exact native-tool allowlist. | Keeps the native catalog and disables both [native multi-agent backends](https://github.com/openai/codex/blob/25af12f7e61572b0bc18ddb1008be543b91519b0/codex-rs/core/src/config/mod.rs#L1434). There is no standalone native `WebFetch`. | Unsupported. The [pinned ACP runner](https://github.com/agentclientprotocol/codex-acp/blob/307d81018f7cc0c3141ddf71c7532d38310e2cfb/src/AgentMode.ts) cannot remove exact native tools. |
| Claude Code `2.1.217`, SDK `0.3.217`, ACP `0.61.0` | Uses the SDK's native preset when unrestricted and explicit tool lists when narrowed. | Keeps the [SDK preset](https://github.com/agentclientprotocol/claude-agent-acp/blob/c19bddcf7914259d6c15103a2d1580c7371e1d16/src/acp-agent.ts#L5216), disables unsupported tools, ignores project settings, and requires the configured MCP servers. | Projects the exact supported native allowlist and denylist into the SDK. |
| Copilot CLI `1.0.77` | Keeps the existing CLI catalog and fixed exclusions. Narrowed policies use explicit CLI exclusions. | Keeps supported native tools and grants native URL, shell, and write [permissions](https://docs.github.com/en/copilot/reference/cli-command-reference) within Orka's existing boundaries. | Removes denied native tools. Granting native `WebSearch` is unsupported. |
| OpenCode `1.18.9` | Limits native tools to Read, Write, Edit, Bash, Glob, and Grep, with mutation aliases and read-intent restrictions. Native web tools and todos are disabled. | Keeps the supported [native catalog](https://github.com/anomalyco/opencode/blob/4da7bb44c84e013fa53e9c5d02ac753d1435c81a/packages/opencode/src/tool/registry.ts#L58), including fetch, search, and todos. Enables the bundled Exa search client. | Uses explicit permissions. Supports Read, Glob, Grep, Bash, the mutation group, WebFetch, WebSearch, and `todowrite`, subject to workspace and bypass checks. |

Full mode preserves the pinned runner catalog. Orka's shared tool-name list
translates policy grants. The mode does not load new plugins, external MCP
servers, or credentials from a Task or checkout. Runner upgrades still require
reviewed pins and new immutable images.

### Unsupported native features

Native helper agents and schedulers do not create Orka child Tasks, so they
have no Orka ownership or cancellation contract. Use authorized `delegate_task`
and `wait_for_tasks` calls for delegation. Full native access does not grant
either brokered tool.

- Codex disables both native multi-agent backends in explicit modes. Its
  adapter marks repository configuration untrusted while preserving the Orka
  external sandbox and ordinary `AGENTS.md` instructions. Hooks, plugins,
  automatic skill presentation, and skill-triggered MCP installation are
  disabled. Repository MCP entries cannot suppress the authorized Orka broker.
- Claude disables Agent, Task, TaskOutput, TaskStop, Workflow, Monitor,
  CronCreate, CronDelete, CronList, ScheduleWakeup, RemoteTrigger,
  PushNotification, Artifact, EnterWorktree, ExitWorktree, Skill,
  AskUserQuestion, EnterPlanMode, and ExitPlanMode. Background tasks are
  disabled, setting sources are empty, and MCP configuration is strict.
- Copilot excludes `list_agents`, `read_agent`, `skill`, `sql`, `task`, and
  `write_agent`. Built-in MCP servers, custom instructions, experimental
  features, remote execution, and interactive questions remain disabled.
- OpenCode denies `task`, `skill`, `question`, `lsp`, `external_directory`, and
  `doom_loop`. It disables subagents, background subagents, title generation,
  plugin loading, LSP, formatters, snapshots, and sharing. Project configuration
  cannot replace the generated session configuration.

## Configuration rules

Full mode requires `workspace.intent: write`, an effective Bash grant, and a
Task-owned Session. The existing writable-workspace prerequisites also apply:
`workspace.gitRepo` and `workspace.publicationCredentialRef` must be configured
before execution. It rejects all Task `allowedTools` lists and native tool
names in Agent `defaultAllowedTools` or Task `disallowedTools`. An Agent
`defaultAllowedTools` list in full mode grants only Orka tools; `[]` grants
none. Task `disallowedTools` can name only explicitly granted Orka tools and
narrows those brokered grants. Setting Agent
`defaultAllowBash: false` remains incompatible even if a Task requests Bash.

Restricted mode requires `runtime.defaultAllowedTools` or Task
`agentRuntime.allowedTools`. An explicit empty list denies all tools. For an
OpenCode reader:

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Agent
metadata:
  name: opencode-reader
spec:
  runtime:
    type: opencode
    contractVersion: orka.harness.v2
    toolPolicy: restricted
    defaultAllowBash: false
    defaultAllowedTools:
      - Read
      - Glob
  model:
    name: openai/gpt-5.4
```

Use `workspace.intent: read` on its Tasks. To grant native OpenCode web access,
add `WebFetch` or `WebSearch` to the allowlist and configure the required
network access. Keep Bash disabled when denying any file, mutation, or web
operation that a command could perform instead.

OpenCode treats `Write`, `Edit`, and `apply_patch` as one mutation grant because
the runner selects different tools for different models. Allowing one allows
the group; denying one denies the group. Read intent rejects this group and
Bash. It also rejects OpenCode Grep because that tool cannot enforce the
protected-file exclusions applied to Read. An unsupported restricted policy
fails before a provider session starts.

The policy mode, allowed names, denied names, and Bash gate enter the immutable
tool-policy digest. A policy change requires a new Session and a distinct pool
profile. Explicit policies that allow commands reject `sessionRef` and
`execution.workspace.reusePolicy: session`. Freezing a process between prompts
does not terminate its background work; resuming it for another Task would
reuse the previous Task's processes.

Native permissions do not grant Orka MCP authority. The loopback broker still
checks the exact tool, active Task, prompt lease, fences, approvals, and
delegation scope. The mode does not change UID isolation, Task budgets,
workspace validation, credential handling, or clean-room publication.
File-pattern exclusions on a native Read tool do not constrain arbitrary shell
commands. Commands remain bounded by the session's OS identity, private
directories, available credentials, and network policy.

## Web setup

Full mode does not change RuntimePool NetworkPolicy. Egress remains
default-deny. Enabling a runner tool does not establish provider support,
network reachability, authentication, or a successful tool result.

OpenCode uses its [bundled Exa client](https://github.com/anomalyco/opencode/blob/4da7bb44c84e013fa53e9c5d02ac753d1435c81a/packages/opencode/src/tool/mcp-websearch.ts#L4) for native search. Orka sets
`OPENCODE_ENABLE_EXA=true` and `OPENCODE_WEBSEARCH_PROVIDER=exa` in full mode or
when restricted mode grants `WebSearch`. The client calls
`https://mcp.exa.ai/mcp`; Orka injects no search API key. An operator must permit
that destination for a cluster Task to reach it. Native fetch also needs
access to its target. These changes do not permit direct SCM publication or
bypass the authenticated model-provider proxy.

## Diagnostics

`Task.status.execution.toolPolicy` records the admission snapshot for the
frozen policy. It includes the mode, runner, runner version, policy digest, and
feature entries. Each entry has a `name`, `source`, `state`, and `reason`.

| Value | Meaning |
| --- | --- |
| `source: runner` | A native runner feature, such as `web_search`, `file_read`, or commands. |
| `source: orka` | An explicitly authorized tool executed through Orka's broker. |
| `ready` | The approved integration is configured to provide the feature. This is not a record of a successful invocation. |
| `disabled` | The frozen policy denies the feature. |
| `unavailable` | A required dependency is unavailable. See the admission limitation below. |
| `unsupported` | The pinned runner or its Orka integration cannot provide the feature under this contract. |
| `unverified` | The tool is enabled, but required external setup or a real call has not been verified. |

Web features and custom Orka endpoints remain `unverified` at admission. The
snapshot does not promote them to `ready` after a tool result. Inspect the
actual tool call, result, and Task outcome when verifying execution. A tool
name in a prompt or model catalog alone is insufficient evidence.

Approval-required configurations currently fail admission before Orka
publishes this snapshot. Inspect the Task's admission error when the snapshot
is absent; do not expect an `unavailable` feature entry for that rejection.

## Verification

Policy and projection tests cover all four pinned runners. They check legacy
compatibility, full-mode catalog preservation, unsupported restrictions,
equivalent command bypasses, separate broker authority, diagnostics, and
Session/pool identity changes.

```bash
go test ./internal/acp ./internal/harness/v2 ./internal/controller \
  ./workers/acp/supervisor \
  -run 'Test.*(NativeTool|NativePolicy|PermissionRules|FullNative)' -count=1
```

The opt-in
[`TestOpenCodeNativeTools`](https://github.com/orka-agents/orka/blob/main/workers/acp/supervisor/native_tools_integration_test.go)
runs a caller-supplied OpenCode executable matching the pinned version with
production session configuration and the provider proxy. A local Chat
Completions fixture requests the native tools and checks their results. Search
calls the public Exa service when explicitly enabled. It does not use
model-provider account credentials.

```bash
ORKA_TEST_OPENCODE_BIN=/absolute/path/to/opencode \
ORKA_TEST_OPENCODE_SEARCH=1 \
go test ./workers/acp/supervisor -run '^TestOpenCodeNativeTools$' -count=1 -v
```

Without `ORKA_TEST_OPENCODE_BIN`, the test skips. It rejects a reported version
other than `1.18.9`, but does not verify artifact authenticity. Verify the
executable's checksum against the official release before using a run as
qualification evidence. Without `ORKA_TEST_OPENCODE_SEARCH=1`, the public
search case skips.

| Execution check | Evidence for this change |
| --- | --- |
| OpenCode full native fetch | Passed on macOS arm64. The controlled HTTP server received one request, and the runner returned its marker through native tool results and ACP events. |
| OpenCode full native search | Passed on macOS arm64 against public Exa search. The runner returned results containing URLs through its native `websearch` call. |
| OpenCode restricted Read and denied fetch/Bash | Passed on macOS arm64. Read returned the workspace marker; forced fetch and Bash calls returned explicit unavailable tool errors, and the controlled HTTP server received no requests. |
| OpenCode cancellation and cleanup | Passed on macOS arm64 with process retirement. Native ACP cancellation alone left the held fetch body open; retiring the process closed it. |
| Full and restricted supervisor boundaries | Passed on macOS arm64 with a fake ACP child and production Claude policy projection: archive traversal and escaping symlink rejection, relative workspace roots, child credential isolation, private directories, cancellation, provider-authority revocation, and process exit. |
| Full and restricted RuntimePool resources | Passed with the controller test client: resource limits, Pod hardening, and scoped default-deny NetworkPolicy remain in the rendered resources. This does not prove live CNI enforcement. |
| Codex, Claude, and Copilot native execution | Not run for this policy change. Source and configuration tests do not establish live tool success. |
| Linux runtime image, UID isolation, Kubernetes/CNI egress | Not run for this policy change. The host fixture does not establish these deployment properties. |

Before qualifying an image, run the native cases in the target container and
cluster with both permitted and denied egress. Verify denied calls cannot
reach a controlled endpoint through Bash, verify cancellation retires the
process tree, and retain actual tool events and results. Run the existing
[release qualification](release-qualification.md) checks for publication,
approvals, restart, and cleanup alongside those native-tool checks.
