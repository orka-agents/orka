---
slug: /ui
---

# Web Dashboard

Orka includes a built-in React web dashboard embedded into the controller binary. No separate frontend deployment is needed.

## Tech Stack

| Layer | Technology |
|-------|-----------|
| Runtime | Bun 1.2+ |
| Frontend | React 19 |
| Build Tool | Vite 6 |
| Styling | Tailwind CSS 4 |
| UI Primitives | shadcn/ui (Radix-based) |
| State Management | Zustand 5 |
| Data Fetching | TanStack Query 5 |
| Routing | TanStack Router (file-based) |
| Schema Validation | Zod 3 |
| Icons | Lucide React |
| Testing | Vitest + Testing Library |

## Architecture

```
┌─────────────┐     ┌─────────────────┐     ┌──────────────────┐
│   Browser   │────▶│  Go API Server  │────▶│   Kubernetes     │
│   (React)   │◀────│ (Fiber, :8080)  │◀────│   API Server     │
└─────────────┘     └────────┬────────┘     └──────────────────┘
                             │
                      ┌──────┴──────┐
                      │ //go:embed  │
                      │  ui/dist/*  │
                      └─────────────┘

Development:
  Vite (:5173) --proxy /api/*--> Go API (:8080)

Production:
  Go binary serves ui/dist/ at "/" and API at "/api/*"
```

## Pages

The sidebar groups destinations by depth: **Operate** (day-to-day work),
**Automation** (standing repository automation), **Registry** (resources agents
are built from), **Fabric** (runtime and gateway infrastructure), and
**Platform** (system status).

| Page | Route | Description |
|------|-------|-------------|
| Dashboard | `/` | Overview counts, phase distribution, recent tasks, and a cross-task "waiting for approval" inbox |
| Chat | `/chat` | Interactive chat with SSE streaming and tool execution; New Chat cancels and deletes the previous server-side session |
| Tasks | `/tasks` | Create, monitor, and manage tasks with log streaming |
| Task Detail | `/tasks/:taskId` | Metadata, requester/transaction provenance, schedule facts, result viewer, logs, execution timeline, trace, approvals, plan, and children (execution graph + API-resolved child table); fork provenance when forked |
| Create Task | `/tasks/new` | Type selector (container/AI/agent) with schedule, env, retry, webhook, session, AI sampling/tools/skills, agent runtime overrides, workspace + clean-room policies — plus a full-spec YAML mode |
| Board / Kanban | `/kanban` | Kanban board for task status and work-in-progress tracking |
| Live | `/live` | Live agent grid for active task execution and status updates |
| Monitors | `/monitors` | Repository monitor inventory with manual runs |
| Monitor Detail | `/monitors/:monitorId` | PR/issue queues, command actions, workflow timeline, monitor events, implementation jobs with patch preview, GitHub mutation audit, and spec editing |
| Create Monitor | `/monitors/create/new` | RepositoryMonitor creation form |
| Security | `/security` | Repository scan inventory with scan status, finding counts, and manual scan actions |
| New Repository Scan | `/security/new` | Form for creating a RepositoryScan from provider, URL, branch, schedule, and analysis agent |
| Repository Security Detail | `/security/:repoId` | Threat model editor, scan history, finding tables, recommended remediation, spec editing, and repository delete |
| Security Finding Detail | `/security/findings/:findingId` | Evidence, validation status, patch proposal, dismissal/reopen, and remediation PR actions |
| Sessions | `/sessions` | Browse sessions with message count and token stats |
| Session Detail | `/sessions/:sessionId` | Transcript viewer plus an aggregated execution timeline across the session's tasks |
| Agents | `/agents` | Card grid of agents with model and tool info |
| Agent Detail | `/agents/:agentId` | Full agent configuration view with full-spec YAML editing |
| Create Agent | `/agents/new` | Agent creation form; built-in ACP runtimes are stamped with `contractVersion: orka.harness.v2`, and registered Providers are offered via `providerRef` |
| Providers | `/providers` | LLM Provider inventory (type, default model, readiness) |
| Provider Detail | `/providers/:providerName` | Provider configuration and status with edit and delete |
| Create Provider | `/providers/new` | Provider creation backed by a credentials Secret picker |
| Tools | `/tools` | Table of built-in and custom tools with manifest-based tool creation |
| Tool Detail | `/tools/:toolName` | Tool spec with JSON Schema parameters, spec editing, and delete for custom tools |
| Skills | `/skills` | Skill inventory (version, tags, phase) |
| Skill Detail | `/skills/:skillName` | Rendered SKILL.md content served by the API, bundled file list, edit, and delete |
| Create Skill | `/skills/new` | Skill authoring form (metadata + inline SKILL.md) |
| Memory | `/memory` | Durable memory browser (filters, enable/disable, soft delete, edit) and the proposal review inbox — accept/reject records a decision; applying an accepted proposal is the explicit step that creates memory |
| Runtimes | `/runtimes` | RuntimePool capacity/admission (read-only), external AgentRuntime registration/edit/removal, and substrate actor pool management |
| Gateways | `/gateways` | Gateway switchboard: bindings, session queues, event ledger, delivery outbox with manual retry, and cluster GatewayClasses |
| Gateway Detail | `/gateways/:gatewayId` | Adapter boundary, observed capabilities, and per-gateway ledgers |
| System | `/system` | Readiness checks, capability badges (chat, memory store), chat orchestrator limits, compat endpoint connect info, and the model catalog |
| Login | `/login` | Token input for ServiceAccount authentication |

## Execution events

Task and session detail pages surface the evented execution backbone (see the
[Execution Events reference](../reference/execution-events.md) for the underlying
APIs):

- **Timeline tab** — the semantic execution event stream for a task, loaded from
  `GET /tasks/:id/events` and followed live over `GET /tasks/:id/stream`. Events
  are deduplicated by sequence, grouped by category, and filterable by category,
  severity, and free-text search. Redacted/truncated payloads are marked and the
  raw (already-redacted) payload is available behind a disclosure toggle and a
  copy-JSON action. Live follow stops on the terminal `stream_complete` frame.
- **Trace tab** — an explainable, grouped view from `GET /tasks/:id/trace`:
  lifecycle summary, model requests, tool calls, child tasks (linkable), workspace,
  artifacts, approvals, fork provenance, and errors/warnings, with a raw-timeline
  fallback when no structured groups exist.
- **Approvals tab** — pending high-risk approvals from `GET /tasks/:id/approvals`
  with approve/decline actions (optional reason) posted to
  `POST /tasks/:id/approvals/:approvalID/decision`. Buttons disable while a
  decision is in flight; conflicts (decided elsewhere/expired) surface inline.
- **Fork from a checkpoint** — each timeline row offers "Fork from here", which
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

1. **CLI login**: `orka login` extracts the OIDC token from kubeconfig and opens the browser with `#token=<token>`
2. **Manual login**: Paste a ServiceAccount token on the login page
3. **Token creation**: `kubectl create token orka-client -n orka-system`

All API requests include `Authorization: Bearer <token>`.

## Features

- **Dark/light theme**: Toggle with localStorage persistence
- **Namespace switcher**: Free-text namespace entry with recent history and the token's own namespace as suggestions (there is no list-namespaces API)
- **Identity popover**: The verified caller identity from `GET /auth/whoami`, including transaction-token metadata when present
- **Security workflow**: Manage repository scans, edit threat models, triage findings, validate/reproduce issues, generate patch proposals, and open remediation PRs
- **Skeleton loaders**: Loading states for all list/detail pages
- **Error handling**: Global error boundary, toast notifications, 401 redirect
- **Responsive design**: Mobile-responsive sidebar, tables, and cards
- **Auto-refresh**: TanStack Query `refetchInterval` for live status updates
- **Live event streams**: SSE-backed task/session execution timelines that reconnect from the last sequence and recover history after refresh
- **Cursor pagination**: Kubernetes-style `continue` token pagination

## Development

```bash
# Install dependencies
make ui-install    # or: cd ui && bun install

# Run dev server (port 5173, proxies /api to :8080)
make ui-dev        # or: cd ui && bun run dev

# Build for production
make ui-build      # or: cd ui && bun run build

# Run tests
make ui-test       # or: cd ui && bun run test

# Run tests with coverage
make ui-test-coverage  # or: cd ui && bun run test:coverage

# Lint
make ui-lint       # or: cd ui && bun run lint
```

## Directory Structure

```
ui/
├── index.html
├── package.json
├── vite.config.ts
├── components.json              # shadcn/ui config
├── src/
│   ├── main.tsx                 # App entry
│   ├── app.tsx                  # Root component with providers
│   ├── index.css                # Tailwind imports
│   ├── routeTree.gen.ts         # TanStack Router generated
│   ├── lib/
│   │   ├── api-client.ts        # Fetch wrapper with auth
│   │   └── utils.ts             # cn() helper
│   ├── schemas/                 # Zod schemas matching Go API types
│   │   ├── agent.ts
│   │   ├── chat.ts
│   │   ├── security.ts          # RepositoryScan, findings, scan runs, patch proposals
│   │   ├── session.ts
│   │   ├── task.ts
│   │   └── tool.ts
│   ├── stores/
│   │   ├── auth.ts              # Zustand: token, user info
│   │   ├── chat.ts              # Zustand: chat state
│   │   └── ui.ts                # Zustand: sidebar, theme, namespace
│   ├── hooks/                   # TanStack Query hooks per resource
│   │   ├── use-agents.ts
│   │   ├── use-chat.ts
│   │   ├── use-security.ts      # Repository security API hooks
│   │   ├── use-sessions.ts
│   │   ├── use-tasks.ts
│   │   └── use-tools.ts
│   ├── components/
│   │   ├── ui/                  # shadcn/ui primitives
│   │   ├── layout/              # Sidebar, header, root layout
│   │   ├── tasks/               # Task list/detail, Kanban board, live agent grid
│   │   ├── events/              # Execution event timeline, trace, severity primitives
│   │   ├── security/            # Repository scans, threat models, findings, patch proposals
│   │   ├── sessions/            # Session list, detail, transcript
│   │   ├── agents/              # Agent list, detail, create form
│   │   ├── tools/               # Tool list, detail
│   │   ├── chat/                # Chat interface
│   │   └── dashboard/           # Overview, stats cards
│   ├── routes/                  # File-based TanStack Router routes
│   │   ├── index.tsx            # Dashboard
│   │   ├── chat.tsx
│   │   ├── kanban.tsx           # Board / Kanban
│   │   ├── live.tsx             # Live agent grid
│   │   ├── security/
│   │   │   ├── index.tsx        # /security
│   │   │   ├── new.tsx          # /security/new
│   │   │   ├── $repoId.tsx      # /security/:repoId
│   │   │   └── findings/
│   │   │       └── $findingId.tsx # /security/findings/:findingId
│   │   ├── tasks/
│   │   ├── sessions/
│   │   ├── agents/
│   │   └── tools/
│   └── test/                    # Test utilities and setup
└── dist/                        # Vite build output (gitignored)
```

## Embedding

The UI is embedded into the Go binary via `//go:embed`:

```go
// internal/uiembed/embed.go
//go:embed all:dist
var distFS embed.FS

func FS() fs.FS { ... }
```

The Fiber server serves embedded static assets at `/` with fallback to `index.html` for client-side routing.
