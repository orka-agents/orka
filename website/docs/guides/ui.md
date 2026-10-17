---
slug: /ui
description: "The dashboard built into the controller: what each page shows and how to reach it."
---

# Web dashboard

Orka includes a web dashboard built into the controller. There is nothing extra to
deploy, but it is off by default: the API and the `orka` CLI work without it, and
a signed-in browser keeps its token in localStorage on the same origin as every API,
so leave it off where nobody needs a browser UI.

## Turn it on

With Helm, set `controller.dashboard.enabled`:

```bash
helm upgrade orka orka/orka --namespace orka-system --reuse-values \
  --set controller.dashboard.enabled=true
```

Without Helm, start the controller with `--dashboard-enabled`, or set its
`ORKA_DASHBOARD_ENABLED` environment variable:

```bash
kubectl -n orka-system set env deployment/orka-controller-manager ORKA_DASHBOARD_ENABLED=true
```

While it is off, paths outside the APIs answer with a JSON 404, `orka dashboard` stops
with an error, and connector consent finishes with `orka connection complete` (see
[Connectors](connectors.md#3-link-an-account)).

:::tip Try it without a cluster
The [dashboard demo](pathname:///demo/) runs in your browser on sample data, with a
guided tour of the main pages. Nothing in it reaches a cluster or a model.
:::

## Open it

Forward the API port if it is not already forwarded, and have a client token ready
(see [Connect to the API](../getting-started.md#connect-to-the-api)):

```bash
kubectl -n orka-system port-forward svc/orka 8080:8080
kubectl -n orka-system create token orka-client
```

Open [http://localhost:8080](http://localhost:8080). The login page asks for a token;
paste the one you just created. From there you can create Tasks, chat with the
orchestrator, watch Tasks run live, and browse sessions and artifacts. If you use the
CLI, `orka dashboard` opens the dashboard already signed in.

## Pages

| Page | Route | What it shows |
|------|-------|---------------|
| Dashboard | `/` | Task, session, agent, and tool counts, and recent Tasks |
| Usage | `/usage` | Model usage and tokens per merged pull request by team. See [Usage and PR outcomes](usage-and-pr-outcomes.md) |
| Chat | `/chat` | The chat orchestrator, with streamed tool calls. See [Interactive chat](chat.md) |
| Monitors | `/monitors` | Repository monitors: runs, pull request and issue queues, workflow timeline, blocked reasons, implementation jobs, and GitHub mutation audit. See [Repository monitors](repository-monitors.md) |
| Security | `/security` | Repository scans, threat models, findings, validation, patch proposals, and remediation pull requests. See [Security scanning](repository-security-scanning.md) |
| Tasks | `/tasks` | Create, monitor, and manage Tasks, with log streaming |
| Task detail | `/tasks/:taskId` | Overview (current activity, progress, delivery, tokens, derived checks, child tasks), Activity (trace and raw events), Approvals, Result and artifacts, Logs, Spec (the Task resource), and Plan for autonomous runs |
| Board | `/kanban` | Tasks as a Kanban board by status |
| Gateways | `/gateways` | Gateway readiness, bindings, per-Session queues, event timelines, and delivery attempts. See [Gateways](gateways.md) |
| Sessions | `/sessions` | Sessions with message counts and token use; each one's transcript and timeline across its Tasks |
| Runtimes | `/runtimes` | RuntimePool capacity and admission, and external AgentRuntime registrations |
| Agents | `/agents` | Agents with their model and tools, and a form to create one |
| Tools | `/tools` | Built-in and custom tools, with their parameter schemas |
| Connectors | `/settings/connectors` | Link your own accounts, such as GitHub, for agents to act as you. See [Connectors](connectors.md) |
| Login | `/login` | Sign in with a token |

## Execution events

Task and session detail pages surface the evented execution backbone (see the
[Execution Events reference](../reference/execution-events.md) for the underlying
APIs):

- **Activity tab, Trace view** (the default) — an explainable, grouped view from
  `GET /tasks/:id/trace`: lifecycle summary, model requests, tool calls, child
  tasks (linkable), workspace, artifacts, approvals, fork provenance, and
  errors/warnings, with a raw-timeline fallback when no structured groups exist.
- **Activity tab, Raw events view** — the semantic execution event stream for a
  task, loaded from `GET /tasks/:id/events` and followed live over
  `GET /tasks/:id/stream`. Events are deduplicated by sequence, grouped by
  category, and filterable by category, severity, and free-text search.
  Redacted/truncated payloads are marked and the raw (already-redacted) payload is
  available behind a disclosure toggle and a copy-JSON action. Live follow stops on
  the terminal `stream_complete` frame.
- **Approvals tab** — pending high-risk approvals from `GET /tasks/:id/approvals`
  with approve/decline actions (optional reason) posted to
  `POST /tasks/:id/approvals/:approvalID/decision`. Buttons disable while a
  decision is in flight; conflicts (decided elsewhere/expired) surface inline.
- **Fork from a checkpoint** — each raw event row offers "Fork from here", which
  opens a dialog seeded with that event's sequence and posts to
  `POST /tasks/:id/fork`. Forked tasks show a provenance card derived from their
  `orka.ai/fork-source-task` and `orka.ai/fork-source-seq` annotations.
- **Session timeline** — session detail aggregates events across the session's
  tasks from `GET /sessions/:id/events` / `GET /sessions/:id/stream`, with each
  row linking to its originating task.

These surfaces hide gracefully (with a clear message) when execution event storage
is not enabled on the server (`501 Not Implemented`).

## Authentication

The UI uses ServiceAccount bearer tokens stored in localStorage:

1. **CLI**: `orka dashboard` creates a 24-hour ServiceAccount token with `kubectl create token` (or uses `--token`) and opens the browser with `#token=<token>`
2. **Manual login**: Paste a ServiceAccount token on the login page
3. **Token creation**: `kubectl create token orka-client -n orka-system`

All API requests include `Authorization: Bearer <token>`.

## Features

- **Dark/light theme**: Toggle with localStorage persistence
- **Namespace selector**: Filter all views by Kubernetes namespace
- **Security workflow**: Manage repository scans, edit threat models, triage findings, validate/reproduce issues, generate patch proposals, and open remediation PRs
- **Skeleton loaders**: Loading states for all list/detail pages
- **Error handling**: Global error boundary, toast notifications, 401 redirect
- **Responsive design**: Mobile-responsive sidebar, tables, and cards
- **Auto-refresh**: TanStack Query `refetchInterval` for live status updates
- **Live event streams**: SSE-backed task/session execution timelines that reconnect from the last sequence and recover history after refresh
- **Cursor pagination**: Kubernetes-style `continue` token pagination

## Working on the dashboard

The dashboard's source, its development server, and its tests live in
[`ui/`](https://github.com/orka-agents/orka/tree/main/ui); see its
[README](https://github.com/orka-agents/orka/blob/main/ui/README.md).
