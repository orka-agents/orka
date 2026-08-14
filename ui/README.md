# Orka Web UI

The dashboard embedded into the Orka controller binary. It covers the public
REST surface end to end: tasks (create incl. full spec + YAML mode, timeline,
trace, approvals, artifacts, plan, children, fork), chat, sessions, agents,
providers, tools, skills, durable memory + proposal review, repository
monitors, security scanning, runtime fabric (RuntimePools, external
AgentRuntimes, substrate actor pools), gateways, and system status.

## Stack

React 19 · TypeScript · Vite 6 · TanStack Router (file-based, generated
`src/routeTree.gen.ts` — do not edit) · TanStack Query · Zustand · Tailwind 4 ·
radix-ui · zod · js-yaml · Vitest + Testing Library + MSW.

## Development

```bash
bun install
bun run dev        # dev server on :5173, /api proxied to :8080
bun run lint
bun run test
bun run test:coverage
bun run build      # tsc -b && vite build
```

From the repo root, `make ui-build` builds and copies the bundle into
`internal/uiembed/dist/` for `//go:embed`.

## Conventions

- One schema module per API area in `src/schemas` (zod); canonical
  execution-event shapes live in `execution-event.ts`.
- Hooks in `src/hooks` wrap the API client (`src/lib/api-client.ts`); list
  and detail calls pass the selected namespace from the UI store.
- Optional backend capabilities (event store, memory store, artifact store,
  plans) return 501 — render a "not enabled" state, never an error loop.
- Deep specs (AgentRuntime, RepositoryMonitor, Tool, full TaskSpec) are edited
  through the shared YAML `ManifestEditor`; the server remains the validation
  authority.
- Never render secret values: pickers submit Secret names/keys only.
- Colocated `*.test.tsx` per component; MSW default handlers in
  `src/test/mocks/handlers.ts`.
