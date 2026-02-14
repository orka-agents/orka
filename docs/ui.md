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
│   Browser   │────▶│   Go API Server │────▶│   Kubernetes     │
│  (React)    │◀────│   (Fiber, :8080) │◀────│   API Server     │
└─────────────┘     └─────────────────┘     └──────────────────┘
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

| Page | Route | Description |
|------|-------|-------------|
| Dashboard | `/` | Overview with task/session/agent/tool counts and recent tasks |
| Tasks | `/tasks` | Create, monitor, and manage tasks with log streaming |
| Task Detail | `/tasks/:taskId` | Task metadata, spec, status, result viewer, logs |
| Create Task | `/tasks/new` | Form with type selector (container/AI/agent) and conditional fields |
| Sessions | `/sessions` | Browse sessions with message count and token stats |
| Session Detail | `/sessions/:sessionId` | Transcript viewer with chat-like message rendering |
| Agents | `/agents` | Card grid of agents with model and tool info |
| Agent Detail | `/agents/:agentId` | Full agent configuration view |
| Create Agent | `/agents/new` | Agent creation form |
| Tools | `/tools` | Table of built-in and custom tools |
| Tool Detail | `/tools/:toolName` | Tool spec with JSON Schema parameters |
| Chat | `/chat` | Interactive chat with SSE streaming and tool execution |
| Login | `/login` | Token input for ServiceAccount authentication |

## Authentication

The UI uses ServiceAccount bearer tokens stored in localStorage:

1. **CLI login**: `orka login` extracts the OIDC token from kubeconfig and opens the browser with `#token=<token>`
2. **Manual login**: Paste a ServiceAccount token on the login page
3. **Token creation**: `kubectl create token orka-client -n orka-system`

All API requests include `Authorization: Bearer <token>`.

## Features

- **Dark/light theme**: Toggle with localStorage persistence
- **Namespace selector**: Filter all views by Kubernetes namespace
- **Skeleton loaders**: Loading states for all list/detail pages
- **Error handling**: Global error boundary, toast notifications, 401 redirect
- **Responsive design**: Mobile-responsive sidebar, tables, and cards
- **Auto-refresh**: TanStack Query `refetchInterval` for live status updates
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
│   ├── stores/
│   │   ├── auth.ts              # Zustand: token, user info
│   │   ├── chat.ts              # Zustand: chat state
│   │   └── ui.ts                # Zustand: sidebar, theme, namespace
│   ├── hooks/                   # TanStack Query hooks per resource
│   ├── components/
│   │   ├── ui/                  # shadcn/ui primitives
│   │   ├── layout/              # Sidebar, header, root layout
│   │   ├── tasks/               # Task list, detail, create form
│   │   ├── sessions/            # Session list, detail, transcript
│   │   ├── agents/              # Agent list, detail, create form
│   │   ├── tools/               # Tool list, detail
│   │   ├── chat/                # Chat interface
│   │   └── dashboard/           # Overview, stats cards
│   ├── routes/                  # File-based TanStack Router routes
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
