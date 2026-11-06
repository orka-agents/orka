# ADR 0040: Serve a control-plane MCP server by replaying REST routes

Date: 2026-11-06

## Status

Accepted. Adds an opt-in `POST /mcp` endpoint on the API port
(`--mcp-server-enabled`) that IDE agents use to delegate Tasks, track them, answer
approvals, and read results. It changes no existing route or permission.

## Context

People increasingly work through an agent in their editor: Claude Code, Cursor, or
GitHub Copilot in VS Code. Those agents speak the Model Context Protocol. Handing a
long or credentialed job to an Orka Agent from there meant leaving the editor for
the CLI or dashboard.

The REST API already carries every rule that matters for this: TokenReview, OIDC,
and transaction-token authentication; a per-route SubjectAccessReview inventory
that fails closed; transaction-token scope and `tctx` checks inside handlers;
namespace resolution and isolation; Gateway and workspace-class checks; Task
creation validation; and the approval decision rules (Task and run binding, first
decision wins, refusal for finished or deleting Tasks). Any second path to the
same resources risks diverging from one of them, and a divergence would be a
privilege escalation.

MCP clients also expect long-running work to be modelled as MCP Tasks, including
an `input_required` state when a human must answer.

## Decision

- The MCP endpoint only authenticates. Its route policy is identity-only, like
  `/auth/whoami`.
- Every tool call and every `tasks/*` method is executed by building the public
  REST request it corresponds to and running it through the API server's own
  Fiber handler in process, with the caller's original request headers minus the
  MCP transport headers. The full middleware chain and the route's authorization
  run again; the MCP layer holds no Kubernetes client, store handle, or service
  identity, and adds no permission. Each tool documents the routes it replays,
  and tests require the same SubjectAccessReviews, in the same order, as the
  direct REST request.
- The server is stateless Streamable HTTP with single JSON responses: no MCP
  session, no server-initiated requests, no SSE. An MCP task ID encodes the
  Task's namespace, name, and UID; it is re-read and UID-checked on every call.
  Orka Task phases map to MCP task statuses; a running Task with a pending
  approval is `input_required`.
- Both the `io.modelcontextprotocol/tasks` extension (with approvals offered as
  form elicitations and answered through `tasks/update`) and 2025-11-25
  task-augmented `tools/call` are served from the same mapping.
- Cancellation is `DELETE /api/v1/tasks/:id`, the only public way to stop a Task.
  No new cancellation route is added.
- Tool results are projections, not raw objects: they omit environment values,
  Secret references, transaction metadata, and runtime identifiers. Text fields,
  results, and artifacts pass through `redact.SensitiveText`; results and
  artifacts are redacted whole before being chunked (at most 256 KiB per call).
- Concurrency is bounded per replica (`--mcp-max-concurrent`), with bounded waits
  (at most 50 seconds) that may hold at most half of the slots, matching how chat
  bounds its concurrency.

## Consequences

- The MCP surface cannot drift from REST authorization, and any future REST
  hardening applies to MCP automatically. The cost is a second pass through the
  middleware per tool call; TokenReview results are already cached, so this is
  mostly in-memory work.
- Logs and metrics show each replay under its REST path, which keeps audits
  uniform but means MCP traffic is not separately labelled.
- An LLM holding a person's token can call `decide_approval` as that person. The
  tool is marked destructive and its description requires an explicit human
  decision, and clients that support the Tasks extension can show the approval to
  the person instead, but Orka cannot tell a model's call from a person's. Teams
  that need a hard separation should give IDE agents a token without
  `tasks/approvals` update.
- Cancelling deletes the Task, so its result and artifacts disappear with it.
- The endpoint does not implement MCP OAuth discovery or `tasks/list`; clients
  configure a bearer or transaction-token header, and use `list_tasks`.
