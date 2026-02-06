# Mercan Frontend UI — Implementation Plan

## Problem Statement

Mercan is a Kubernetes-native task execution platform with a full REST API but no web UI. Users must interact via `kubectl` or direct API calls. We need a modern frontend dashboard for managing tasks, sessions, agents, and tools.

## Approach

Build a React SPA in `ui/` using Bun as the runtime. For development, Vite dev server runs on port 5173 and proxies `/api/*` to the Go API on port 8080. For production, Vite builds static assets into `ui/dist/`, which the Go binary embeds via `//go:embed` and serves at `/`.

Auth follows the KubeAIRunway pattern: a `mercan login` CLI command extracts the OIDC token from kubeconfig and opens the browser with the token. The UI stores it in localStorage and sends it as a Bearer token with every API request.

### Reference Projects
- **OpenClaw**: Inspected for general UI/UX patterns (control plane UI, chat, settings, navigation)
- **KubeAIRunway**: Inspected for K8s-specific patterns (auth flow, embedding, API client structure, sidebar layout)

## Tech Stack

| Layer              | Technology             |
|--------------------|------------------------|
| Runtime            | Bun 1.2+               |
| Frontend           | React 19               |
| Build Tool         | Vite 6                 |
| Styling            | Tailwind CSS 4          |
| UI Primitives      | shadcn/ui (Radix-based) |
| State Management   | Zustand 5              |
| Data Fetching      | TanStack Query 5       |
| Routing            | TanStack Router        |
| Schema Validation  | Zod 3                  |
| Icons              | Lucide React           |
| Testing            | Vitest + Testing Library|

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

## Directory Structure

```
ui/
├── index.html
├── package.json
├── tsconfig.json
├── tsconfig.node.json
├── vite.config.ts
├── tailwind.config.ts          (Tailwind CSS 4)
├── components.json             (shadcn/ui config)
├── public/
│   └── favicon.svg
├── src/
│   ├── main.tsx                (app entry)
│   ├── app.tsx                 (root component with providers)
│   ├── index.css               (Tailwind imports)
│   ├── routeTree.gen.ts        (TanStack Router generated)
│   ├── lib/
│   │   ├── api-client.ts       (fetch wrapper with auth)
│   │   ├── utils.ts            (cn() helper, etc.)
│   │   └── constants.ts        (API base URL, etc.)
│   ├── schemas/
│   │   ├── task.ts             (Zod schemas for Task API)
│   │   ├── session.ts          (Zod schemas for Session API)
│   │   ├── agent.ts            (Zod schemas for Agent API)
│   │   └── tool.ts             (Zod schemas for Tool API)
│   ├── stores/
│   │   ├── auth.ts             (Zustand: token, user info)
│   │   └── ui.ts               (Zustand: sidebar, theme, namespace)
│   ├── hooks/
│   │   ├── use-tasks.ts        (TanStack Query hooks for tasks)
│   │   ├── use-sessions.ts     (TanStack Query hooks for sessions)
│   │   ├── use-agents.ts       (TanStack Query hooks for agents)
│   │   ├── use-tools.ts        (TanStack Query hooks for tools)
│   │   └── use-auth.ts         (auth state hook)
│   ├── components/
│   │   ├── ui/                 (shadcn/ui primitives: button, card, table, badge, etc.)
│   │   ├── layout/
│   │   │   ├── root-layout.tsx (sidebar + header + content)
│   │   │   ├── sidebar.tsx     (navigation sidebar)
│   │   │   └── header.tsx      (namespace selector, user, theme)
│   │   ├── tasks/
│   │   │   ├── task-list.tsx
│   │   │   ├── task-detail.tsx
│   │   │   ├── task-create-form.tsx
│   │   │   ├── task-status-badge.tsx
│   │   │   └── task-result-viewer.tsx
│   │   ├── sessions/
│   │   │   ├── session-list.tsx
│   │   │   ├── session-detail.tsx
│   │   │   └── transcript-viewer.tsx
│   │   ├── agents/
│   │   │   ├── agent-list.tsx
│   │   │   └── agent-detail.tsx
│   │   ├── tools/
│   │   │   ├── tool-list.tsx
│   │   │   └── tool-detail.tsx
│   │   └── dashboard/
│   │       ├── overview.tsx
│   │       ├── stats-cards.tsx
│   │       └── recent-tasks.tsx
│   └── routes/
│       ├── __root.tsx          (root route with layout)
│       ├── index.tsx           (dashboard)
│       ├── tasks/
│       │   ├── index.tsx       (task list)
│       │   ├── new.tsx         (create task)
│       │   └── $taskId.tsx     (task detail)
│       ├── sessions/
│       │   ├── index.tsx       (session list)
│       │   └── $sessionId.tsx  (session detail)
│       ├── agents/
│       │   ├── index.tsx       (agent list)
│       │   └── $agentId.tsx    (agent detail)
│       ├── tools/
│       │   ├── index.tsx       (tool list)
│       │   └── $toolName.tsx   (tool detail)
│       └── login.tsx           (token auth page)
└── dist/                       (Vite build output, gitignored)
```

## Todos

### Phase 1: Project Scaffolding
1. **scaffold-ui-project** — Initialize `ui/` with Bun, Vite 6, React 19, TypeScript. Create `package.json`, `vite.config.ts`, `tsconfig.json`, `index.html`, `src/main.tsx`.
2. **setup-tailwind** — Install and configure Tailwind CSS 4 with `index.css`.
3. **setup-shadcn** — Initialize shadcn/ui with `components.json`. Install base components: button, card, table, badge, input, dialog, select, dropdown-menu, sheet, toast, tabs, separator, scroll-area, skeleton.
4. **setup-routing** — Install TanStack Router. Create file-based route tree: `__root.tsx`, `index.tsx`, and route stubs for tasks, sessions, agents, tools, login.
5. **setup-state** — Install Zustand 5. Create auth store (token, user) and UI store (sidebar collapsed, theme, selected namespace).
6. **setup-tanstack-query** — Install TanStack Query 5. Create `QueryClientProvider` in app root.
7. **setup-zod-schemas** — Install Zod 3. Create schemas matching the Go API types: Task, Session, Agent, Tool with all fields.

### Phase 2: API Layer & Auth
8. **api-client** — Create `lib/api-client.ts`: fetch wrapper that reads token from Zustand auth store, injects `Authorization: Bearer <token>` header, handles errors, supports pagination.
9. **auth-flow** — Implement login page (`routes/login.tsx`): token input field (paste SA token), localStorage persistence, redirect to dashboard on success. Support `#token=...` hash fragment for CLI login flow.
10. **auth-guard** — Create route guard: redirect to `/login` if no token in store. Protect all routes except `/login`.

### Phase 3: Layout & Navigation
11. **root-layout** — Build sidebar + header layout in `__root.tsx`. Sidebar: Dashboard, Tasks, Sessions, Agents, Tools (with Lucide icons). Header: namespace selector dropdown, theme toggle (dark/light), user info.
12. **theme-system** — Implement dark/light theme toggle using Tailwind's `dark:` classes and localStorage persistence.
13. **namespace-selector** — Dropdown in header to filter all views by Kubernetes namespace. Store in Zustand UI store.

### Phase 4: Dashboard
14. **dashboard-page** — Overview page at `/`: stats cards (total tasks, running, succeeded, failed), recent tasks list with status badges, active sessions count, registered agents/tools counts.

### Phase 5: Task Management
15. **task-hooks** — TanStack Query hooks: `useTaskList(namespace, limit)`, `useTask(id, namespace)`, `useTaskResult(id, namespace)`, `useCreateTask()`, `useDeleteTask()`.
16. **task-list-page** — Table view of tasks with columns: name, type, phase, namespace, age. Filterable by status. Pagination via `continue` token. Click row → detail page.
17. **task-create-page** — Form to create a task: name, namespace, type selector (container/ai/agent), conditional fields based on type (image+command for container, provider+model+prompt for AI, agentRef+prompt for agent). Submit via mutation.
18. **task-detail-page** — Detail view: metadata, spec, status conditions, phase badge. Tabs: Overview, Result (rendered markdown or code), Logs (placeholder for future streaming).

### Phase 6: Session Management
19. **session-hooks** — TanStack Query hooks: `useSessionList(namespace, limit)`, `useSession(id, namespace)`, `useDeleteSession()`.
20. **session-list-page** — Table view: name, namespace, message count, token usage (input/output), active task, age. Click → detail.
21. **session-detail-page** — Detail view with transcript viewer. Parse JSONL transcript and render as a chat-like message list (role + content). Show token stats.

### Phase 7: Agents & Tools
22. **agent-hooks** — TanStack Query hooks: `useAgentList(namespace)`, `useAgent(name, namespace)`.
23. **agent-list-page** — Card grid of agents: name, model, provider, active task count. Click → detail.
24. **agent-detail-page** — Full agent config view: model settings, system prompt, tools, skills, runtime config, coordination settings, status conditions.
25. **tool-hooks** — TanStack Query hooks: `useToolList(namespace)`, `useTool(name, namespace)`.
26. **tool-list-page** — Table view: name, namespace, builtin badge, description, available status.
27. **tool-detail-page** — Full tool spec: description, JSON Schema parameters, HTTP config (url, method, headers).

### Phase 8: Go Embedding & Build Integration
28. **go-embed** — Add `//go:embed ui/dist/*` to a new `internal/api/static.go` file. Update Fiber server to serve embedded SPA files at `/`, with fallback to `index.html` for client-side routing.
29. **makefile-targets** — Add Makefile targets: `ui-install` (bun install), `ui-dev` (bun run dev), `ui-build` (bun run build), `ui-lint` (bun run lint). Update `build` target to depend on `ui-build`. Update `docker-build` to include UI build step.
30. **dockerfile-update** — Update Dockerfile with a Bun build stage: install deps, run Vite build, copy `ui/dist/` into the Go build context for embedding.

### Phase 9: CLI Login Command
31. **cli-login** — Add `mercan login` subcommand to `cmd/`: extract OIDC/SA token from kubeconfig, open browser with `http://localhost:8080/#token=<token>`. Similar to KubeAIRunway's approach.

### Phase 10: Polish & Testing
32. **loading-states** — Add skeleton loaders for all list/detail pages using shadcn Skeleton component.
33. **error-handling** — Global error boundary, toast notifications for API errors, 401 → redirect to login.
34. **responsive-design** — Mobile-responsive sidebar (sheet on mobile), responsive tables, responsive cards.
35. **vitest-setup** — Configure Vitest with Testing Library. Write tests for: API client, Zod schemas, auth store, at least one component per resource type.

## Notes

- **Tailwind CSS 4** uses the new CSS-first configuration with `@theme` directive instead of `tailwind.config.js`. Verify shadcn/ui compatibility with Tailwind v4 at implementation time.
- **TanStack Router** uses file-based routing with code generation (`routeTree.gen.ts`). Run `tsr generate` or the Vite plugin during dev.
- The Go API already has CORS enabled (`AllowOrigins: *`), so the Vite dev proxy is for convenience, not necessity.
- Task log streaming is not fully implemented in the API yet — the logs tab will be a placeholder.
- Session transcripts are JSONL format stored in ConfigMaps — the transcript viewer should parse and render this.
- Consider adding auto-refresh (polling via TanStack Query's `refetchInterval`) for task list and detail pages to show live status updates.
- The `continue` token for pagination follows Kubernetes conventions — the UI should implement cursor-based pagination, not page numbers.
