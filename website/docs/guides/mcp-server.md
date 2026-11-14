---
slug: /mcp-server
description: "Let IDE agents such as Claude Code, Cursor, and VS Code delegate Tasks to Orka Agents over MCP."
---

# MCP server

Orka can serve a [Model Context Protocol](https://modelcontextprotocol.io) endpoint
so an agent in your editor can hand work to an Orka Agent, follow it, answer its
approval requests, and read its result. The agent in your editor plans; the Orka
Agent runs in the cluster with the cluster's credentials, workspaces, and policies.

The endpoint is a thin client of the public REST API. Each tool call replays the
equivalent REST request inside the API server with **your** credentials, so it can
never do more than you could do with `curl`. There is no separate MCP permission
model to configure.

## Enable the endpoint

The endpoint is off by default. Start the controller with:

```bash
--mcp-server-enabled
```

or set `ORKA_MCP_SERVER_ENABLED=true`. It is served at `POST /mcp` on the API port
(`8080`). `--mcp-max-concurrent` (default `16`) bounds concurrent MCP requests per API
replica. Long waits (`wait_for_task`, `tasks/result`) give up their request slot and
use a separate pool of half as many wait slots (at least one), so waits never block
approvals, cancellations, or other requests. Excess requests and waits receive HTTP 429
with `Retry-After`, or a busy error for a wait.

A request body may be at most 1 MiB, measured after any `Content-Encoding` (`gzip`,
`deflate`, `br`, or `zstd`) is removed; a compressed body that expands past the limit
is refused with HTTP 413 without being expanded.

The endpoint is a stateless [Streamable HTTP](https://modelcontextprotocol.io/specification/2025-06-18/basic/transports)
server. It answers every request with one JSON body, issues no `Mcp-Session-Id`, and
opens no server-sent event stream; `GET /mcp` returns 405. It supports protocol
versions `2025-06-18`, `2025-11-25`, and the sessionless `2026-07-28` revision
(`server/discover` and per-request `_meta`). It does not accept JSON-RPC batches, so
it does not negotiate `2025-03-26`, the one revision that requires them; `initialize`
with that version is answered with `2025-11-25`.

Mirrored headers are validated against the JSON-RPC body and never used for
authorization. A `2026-07-28` request must send `MCP-Protocol-Version` (equal to its
`_meta` version), `Mcp-Method`, and, for `tools/call`, `Mcp-Name` (plain or
`=?base64?…?=`); a missing or mismatched header is `400` with error `-32020`. A
client that declares the Tasks extension must also send `Mcp-Name` equal to
`params.taskId` on `tasks/get`, `tasks/update`, and `tasks/cancel`. An
unsupported version is `400` with `-32022` and the supported list, and a method the
server does not implement is `404` for a `2026-07-28` request. Any `Mcp-Session-Id`
is ignored. The in-process REST replay forwards only your credential headers
(`Authorization`, `x-api-key`, configured context-token headers) and request labels
(`Host`, `User-Agent`, trace context); no other header you send reaches the REST route.

## Authenticate

Use the same credentials as the REST API:

- a Kubernetes ServiceAccount token in `Authorization: Bearer <token>`;
- an OIDC token in `Authorization: Bearer <token>` when OIDC is configured; or
- a transaction token in `Txn-Token: <token>` (or the configured context-token header).

For a quick start, mint a short-lived token for the `orka-client` ServiceAccount and
port-forward the API:

```bash
export ORKA_TOKEN="$(kubectl -n orka-system create token orka-client --duration 8h)"
kubectl -n orka-system port-forward svc/orka 8080:8080
```

The endpoint does not implement MCP OAuth discovery; configure the header in your
client. Requests with a browser `Origin` header must match `ORKA_CORS_ALLOWED_ORIGINS`
when that allow-list is set. With the endpoint enabled, the CORS allow-list also accepts
the `Mcp-Protocol-Version`, `Mcp-Method`, and `Mcp-Name` request headers.

## Configure your client

### Claude Code

```bash
claude mcp add --transport http orka http://localhost:8080/mcp \
  --header "Authorization: Bearer $ORKA_TOKEN"
```

Or check a project-scoped `.mcp.json` into the repository and keep the token in
your environment:

```json
{
  "mcpServers": {
    "orka": {
      "type": "http",
      "url": "https://orka.example.com/mcp",
      "headers": { "Authorization": "Bearer ${ORKA_TOKEN}" }
    }
  }
}
```

### Cursor

In `~/.cursor/mcp.json` or the project's `.cursor/mcp.json`:

```json
{
  "mcpServers": {
    "orka": {
      "url": "https://orka.example.com/mcp",
      "headers": { "Authorization": "Bearer ${env:ORKA_TOKEN}" }
    }
  }
}
```

### VS Code (GitHub Copilot)

In `.vscode/mcp.json`. VS Code prompts for the token once and stores it securely:

```json
{
  "inputs": [
    { "type": "promptString", "id": "orka-token", "description": "Orka API token", "password": true }
  ],
  "servers": {
    "orka": {
      "type": "http",
      "url": "https://orka.example.com/mcp",
      "headers": { "Authorization": "Bearer ${input:orka-token}" }
    }
  }
}
```

To use a transaction token instead, send `"Txn-Token": "<token>"` in `headers` and
omit `Authorization`.

## Tools

Every tool takes an optional `namespace`. Namespace resolution, `--watch-namespace`,
and `--enforce-namespace-isolation` behave exactly as for the REST route.

| Tool | What it does | REST route it replays |
| --- | --- | --- |
| `list_agents` | List Agents (name, runtime, model, tools, readiness) | `GET /api/v1/agents` |
| `get_agent` | Describe one Agent | `GET /api/v1/agents/:name` |
| `create_task` | Run a prompt on an Agent, with an optional `workspace` and `session` | `POST /api/v1/tasks`; `GET /api/v1/agents/:name` only when `taskType` is omitted; `GET /api/v1/tasks/:id` only when it returns an MCP task |
| `get_task` | Status, pending approvals, plan progress, delivery and pull request | `GET /api/v1/tasks/:id`; `GET /api/v1/tasks/:id/approvals` while running |
| `list_tasks` | List Tasks, optionally by label selector | `GET /api/v1/tasks` |
| `wait_for_task` | Wait up to 50 seconds for a Task to finish or need a decision | `GET /api/v1/tasks/:id`, `GET /api/v1/tasks/:id/approvals` |
| `cancel_task` | Stop a Task by deleting it | `DELETE /api/v1/tasks/:id` |
| `list_pending_approvals` | Show a Task's approval requests | `GET /api/v1/tasks/:id/approvals` |
| `decide_approval` | Record a human's approve or decline decision | `POST /api/v1/tasks/:id/approvals/:approvalID/decision` |
| `get_task_result` | Read the result text in chunks | `GET /api/v1/tasks/:id/result` |
| `list_task_artifacts` | List uploaded artifacts | `GET /api/v1/tasks/:id/artifacts` |
| `get_task_artifact` | Read a text artifact in chunks | `GET /api/v1/tasks/:id/artifacts/:filename` |

`create_task` builds the same request as `POST /api/v1/tasks` with `type`,
`prompt`, `agentRef`, and the optional `timeout`, `workspace` (Task
`spec.workspace`), and `session` (`spec.sessionRef` with `append: true`, which
continues an existing Orka Session and records the Task's turns in it). Give `taskType` (`agent` for Agents with a CLI runtime, `ai` for
native Agents) to skip the Agent lookup; otherwise the caller also needs `get` on the
Agent. Omit `name` to generate one starting with `mcp-`.

`cancel_task` deletes the Task, because deletion is the public API's only way to stop
one. Its result and artifacts become unavailable. Read anything you need first.

### Approvals need a human

`decide_approval` records the decision under your identity, exactly like
`orka task approve`. It is annotated as destructive so clients ask before running it,
and its description tells the model to call it only after you have decided. Keep
tool confirmation on for it in your client. A model that may approve its own
delegated actions defeats the purpose of [human approval](human-approval-v2.md).

Clients that support the MCP Tasks extension can instead show the request to you
directly; see below.

## Long-running Tasks

`create_task` returns as soon as the Task exists. Use `wait_for_task` in a loop, or
use MCP Tasks if your client supports them. The server advertises both shapes:

- The **`io.modelcontextprotocol/tasks` extension.** When a request's `_meta`
  client capabilities declare the extension, `create_task` returns a task
  (`resultType: "task"`). `tasks/get` reports its status; once it completes it
  includes the `create_task` result. The extension reserves `failed` for JSON-RPC
  errors, so a failed Task is `completed` with an `isError: true` result, and a
  cancelled Task carries no result. While a human decision is pending the status is
  `input_required` and `inputRequests` holds one form elicitation per pending
  approval, keyed by approval ID, asking for `decision` (`approve` or `decline`)
  and an optional `reason`. Answer with `tasks/update`: `accept` records the chosen
  decision, `decline` declines the request, and `cancel` leaves it pending. Each
  answer is recorded through the REST decision route under your identity.
  `tasks/cancel` deletes the Task.
- **2025-11-25 task-augmented calls.** A `tools/call` for `create_task` with
  `params.task` returns `{ "task": { ... } }`. `tasks/get` returns the task,
  `tasks/result` waits up to 50 seconds for the result and otherwise asks you to
  poll again, and `tasks/cancel` deletes the Task. Answer approvals with
  `decide_approval`; the server sends no elicitation requests on this path. Only
  `create_task` supports task augmentation: `params.task` on any other tool is
  refused with `-32601` before the tool runs. A task-augmented `create_task` that
  creates no task answers with a JSON-RPC error, not a tool result. `params.task`
  is part of the 2025-11-25 revision only; a `2026-07-28` request that carries it is
  answered as if it did not.

In both shapes the task ID is returned only once `tasks/get` resolves it, so you can
poll at once. The server checks by reading the new Task under your identity; a
caller who may create Tasks but not read them still gets the ID. If the new Task
cannot be read within five seconds, the call returns an error that names the Task
instead of an ID (an error result under the extension, a JSON-RPC error for a
2025-11-25 call): read it with `get_task` rather than creating it again.

A result whose REST response is larger than 8 MiB cannot be read through MCP. A
finished task still reports the Task, with `resultUnavailable` naming the REST route
to read the result from, and `get_task_result` returns an error saying the same.

| Orka Task | MCP task status |
| --- | --- |
| `Pending`, `Scheduled`, `Running`, `Finalizing` | `working` |
| Running with a pending approval request | `input_required` |
| `Succeeded` | `completed` |
| `Failed` | `failed`; `completed` with an `isError` result under the extension |
| `Cancelled`, or being deleted | `cancelled` |

A task ID names the Task's namespace and name and pins its UID. A later Task created
with the same name is a different MCP task, and the old ID reports "task not found".
Result reads and cancellation carry the UID to the REST route (`?uid=`), so they never
return or delete a later Task that reuses the name.
MCP keeps no state of its own: every `tasks/*` call re-reads the Task with your
credentials, so a controller or API restart changes nothing. A finished task is never
reported without its output because a result read failed: `tasks/get` and
`tasks/result` return an error you can retry instead. `tasks/list` is not supported; use `list_tasks`.

## Output safety

Tool results are summaries, not raw API objects. Task summaries omit environment
variables, Secret references, transaction metadata, and runtime identifiers.
Prompts, status messages, approval details, results, and artifacts pass through the
same credential redaction Orka uses for durable memory, which removes bearer and
`Txn-Token` headers, JWTs, well-known API token shapes, credential assignments, and
signed URL parameters. Redaction is best-effort pattern matching, not a guarantee.

Results and artifacts are returned in chunks of at most 256 KiB (32 KiB by default).
The whole text is redacted before it is split, so a credential that crosses a chunk
boundary is still removed; `offset` and `nextOffset` refer to the redacted text.
Binary artifacts are refused; download them through the REST API. Lists return at
most 50 items per page.

## Authorization

`POST /mcp` itself only authenticates. Each tool call is authorized by the route it
replays: the same SubjectAccessReviews for ServiceAccount callers, the same OIDC
subject and namespace policy, the same transaction-token scopes and `tctx`
constraints, the same Gateway and workspace-class checks, and the same refusal
before any effect. A denied call returns a tool error with the REST status, for
example `Orka API returned 403: ...`. See the
[authorization reference](../reference/api-authorization.md#mcp-tools) for the
permissions behind each tool.

Each replay is also recorded by the API's request logging and metrics under its REST
path, so audits see the same routes as for direct API use.
