---
description: "Shape what an agent knows and can do: system prompts, skills, built-in and custom tools, approvals, and model fallbacks."
---

# Agents, skills, and tools

An Agent is the reusable half of a Task: which model to call, what to tell it, and what it
is allowed to do. This page maps the ways to shape one and links to the details.

## Two kinds of Agent

| | Native Agent | Runtime Agent |
| --- | --- | --- |
| Used by | `type: ai` Tasks | `type: agent` Tasks |
| What runs | Orka's AI worker, calling the model in a loop | A coding-agent CLI: Codex, Claude Code, Copilot CLI, or OpenCode |
| Model access | A [Provider](../reference/resources.md#provider) and its API key Secret | Your model gateway, through the [provider proxy](../operations/provider-proxy.md) |
| Tools | Orka's [built-in tools](../reference/built-in-tools.md) and your custom Tools | The CLI's own tools, plus Orka tools through a per-session broker |
| Key fields | `providerRef`, `model`, `systemPrompt`, `tools`, `skills`, `coordination` | `runtime.type`, `model.name`, `runtime.defaultAllowedTools`, `runtime.defaultAllowBash` |

The [Agent reference](../reference/resources.md#agent) lists every field, and
[Agent runtimes](../concepts/agent-runtimes.md) explains how runtime Agents run.

## Write the system prompt

Set `systemPrompt.inline`, or keep longer prompts in a ConfigMap with `systemPrompt.configMapRef`.
All four built-in runtimes, including OpenCode, accept `systemPrompt`. For an optional persona
kept separate from role instructions, see [Soul](../concepts/soul.md).

Keep the prompt minimal and requirement-focused. A study of repository context files for
coding agents ([arxiv 2602.11988](https://arxiv.org/abs/2602.11988)) found that they do not
reliably help. LLM-generated context files lowered task success by 0.5–2% while raising cost
by 20–23%. Developer-written files raised success by 2.4% on average, a gain that was not
statistically significant, while raising cost by up to 19%. The instructions in them were
followed; repository overviews were not useful. In practice:

- **Include only** tooling commands (build, test, and lint invocations), gotchas the agent
  cannot discover (for example, "the provider Secret key defaults to `api-key`"), and hard
  constraints it cannot infer from the source.
- **Leave out codebase overviews.** Agents find their way around a repository with file
  listing and search tools; an overview adds tokens without speeding that up.
- **Do not repeat** what is already in the repository's docs, README, or code comments.
  Agents follow what they are told, so extra instructions mean more exploration and reasoning
  tokens — up to 22% more in the study — without better outcomes.

## Add skills

A [Skill](../reference/resources.md#skill) is a reusable block of instructions, in the Agent
Skills format, that a native Agent or AI Task lists by name. Runtime Agents do not accept
Skills; put that guidance in their system prompt or the Task prompt. Create a Skill from a
`SKILL.md` file:

```bash
orka skill init ./my-skill --name my-skill --description "My skill"
orka skill validate ./my-skill/SKILL.md
orka skill import ./my-skill/SKILL.md --name my-skill
```

Skills are added to the system prompt on every model call for every Task that uses the
Agent, so each one costs tokens on every request:

- Write instructions ("run `make lint-fix` after changes"), not descriptions ("this project
  uses a Makefile-based build system").
- Split large Skills so each Agent references only what it needs. A research Agent does not
  need a coding-standards Skill.
- Audit Skills regularly and remove instructions the agent already follows by default.

## Give the agent tools

The first four options below are for native Agents, which list tools in `spec.tools`. Runtime
Agents reject `spec.tools`; see [coding-agent tools](#coding-agent-tools).

**Built-in tools.** List names such as `web_search`, `code_exec`, `web_fetch`, `file_read`, and
`file_write` in the Agent's `spec.tools`. Orka adds memory tools to every native Task, and
coordination and GitHub tools to coordinators. [Built-in tools](../reference/built-in-tools.md)
has the full list and parameters.

**Your own HTTP API.** Describe it with a [Tool](../reference/resources.md#tool) resource — a
description and JSON Schema the model sees, plus the URL, method, and authentication Orka uses
— then list the Tool's name in the Agent's `spec.tools`. URLs can take
[path parameters](../reference/resources.md#url-path-interpolation) from the arguments.

**An MCP server.** A Tool can call one reviewed function on an MCP server your team already
runs ([Remote MCP tools](../concepts/remote-mcp.md)), or an MCP server Orka hosts in an
[Agent Substrate actor](../reference/resources.md#mcp-servers-in-substrate-actors).

**Without handing the agent the credential.** An
[outbound access policy](../concepts/outbound-access.md) lets a Tool exchange a token, route
through a trusted gateway, or act with the requesting person's own linked account, while the
credential stays out of the agent's reach.

### Coding-agent tools

Runtime Agents use their CLI's tools — `Read`, `Grep`, `Glob`, `Edit`,
`Bash`, and so on. Set defaults with `runtime.defaultAllowedTools` and
`runtime.defaultAllowBash`, and narrow them per Task with `agentRuntime.allowedTools`,
`disallowedTools`, and `allowBash`.

## Require approval for consequential actions

| When | How |
| --- | --- |
| A native autonomous coordinator calls a custom Tool | List it in `coordination.approvalRequiredTools`. See [Autonomous tasks](autonomous-tasks.md#human-approvals). |
| An external runtime calls an Orka-executed Tool | Declare it in the runtime's `approvalRequiredTools`. See [Human approval](human-approval-v2.md). |
| A tool would write through a person's linked account | Always approval-required. See [Connectors](connectors.md). |

## Survive provider outages

Add [`model.fallbacks`](../reference/resources.md#model-fallbacks) to a native Agent, and Orka
tries the next Provider when one fails with an authentication error, a network error, or
exhausted retries.

## Measure the effect

- With [GenAI telemetry](observability.md#genai-metrics) enabled, track
  `gen_ai.client.operation.duration` and `gen_ai.client.token.usage` to compare latency and
  token use.
- Compare runs with and without a specific prompt or Skill addition, and keep only what
  measurably helps.
- Well-documented repositories benefit least from extra context; spend the effort where the
  repository's own documentation is thin.
- [Usage and PR outcomes](usage-and-pr-outcomes.md) reports tokens per merged pull request
  for repository work.
