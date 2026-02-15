# CLAUDE.md

This file provides guidance to Claude Code when working with code in this repository.

## CRITICAL: Security

**NEVER leak API keys, secrets, credentials, or sensitive data.** This includes:
- Never commit secrets to version control
- Never log or print API keys, tokens, or passwords
- Never include secrets in error messages or responses
- Always use Kubernetes Secrets or environment variables for sensitive data

## CRITICAL: No Binaries in Repo

**NEVER commit compiled binaries to the repository.** Build artifacts belong in `bin/` (which is gitignored) or CI release pipelines — not in version control.

## CRITICAL: Fix Pre-existing Issues

When you encounter pre-existing bugs, failing tests, or broken CI — **fix them**. Do not skip or ignore issues just because they existed before your change. Leave the codebase better than you found it.

## Project Overview

Orka is a Kubernetes-native task execution platform. A controller manages Jobs and Pods for incoming task requests, supporting container tasks, AI agent tasks with LLM integration, and external agent CLI runtimes (Copilot, Claude Code). CRDs: Task, Agent, Tool, Provider. See @docs/architecture.md for full details.

## Build & Development Commands

```bash
make manifests          # Generate CRD manifests (run after editing *_types.go or markers)
make generate           # Generate Go types
make build              # Build (includes UI)
make test               # Run tests
make lint-fix           # Lint and fix
make docker-build-all   # Docker images (controller + workers)
make deploy IMG=<registry>/orka:tag
```

UI: `cd ui && bun install && bun run dev` (dev server on :5173). See @docs/development.md for full commands.

## Auto-Generated Files — Do NOT Edit

- `config/crd/bases/*.yaml` — regenerate with `make manifests`
- `config/rbac/role.yaml` — regenerate with `make manifests`
- `**/zz_generated.*.go` — regenerate with `make generate`
- `PROJECT` — managed by kubebuilder CLI
- `ui/src/routeTree.gen.ts` — managed by TanStack Router

Do NOT delete `// +kubebuilder:scaffold:*` comments — the CLI injects code at these markers.

## Code Style & Conventions

### Go
- Structured logging: `log := log.FromContext(ctx); log.Info("msg", "key", val)`
- Idempotent reconciliation — re-fetch before updates (`r.Get` before `r.Update`)
- Owner references for garbage collection (`SetControllerReference`)
- RBAC markers above reconciler methods, then `make manifests`
- Table-driven tests; `fake.NewClientBuilder()` for K8s; `httptest.NewServer` for HTTP
- LLM tool args for nested objects arrive as `map[string]any`, not strings — always type-switch

### TypeScript (UI)
- React 19 + TanStack Router + TanStack Query + Zustand + shadcn/ui + Tailwind CSS 4
- Zod schemas matching Go API types (`ui/src/schemas/`)
- See @docs/ui.md for details

## Verification

```bash
make manifests generate  # After editing *_types.go or markers
make lint-fix && make test  # After editing any *.go files
cd ui && bun run lint && bun run test  # After editing UI code
```

Single tests: `go test ./internal/api/ -run TestHandlerName -v`

## Key Gotchas

- Provider secret key defaults to `api-key` — if your K8s Secret uses a different key, set `secretRef.key`
- Worker filesystem is read-only except `/tmp`, `/home/worker`, and `/workspace` — writes elsewhere will fail
- `make build` requires UI assets — run `make ui-build` first, or the `ensure-ui-embed` target creates a stub
- Fallback providers are resolved at Job build time, not worker runtime — provider changes after Job creation don't take effect
- On context length errors, the AI worker naively truncates messages to ~50% — oldest context is lost first
- Copilot agent worker auto-approves all permission requests — no tool filtering in autonomous mode
- OpenAI provider auto-detects API mode: tries Responses API first, falls back to Chat Completions on 404/405
- Auth tokens are cached for 60s (SHA256 hash) — token revocation has up to 60s propagation delay
- `code_exec` tool silently caps timeout at 60s — values above 60 fall back to the 30s default

## Documentation

See @docs/ for detailed documentation:
- @docs/architecture.md — System design, components, key patterns, project structure
- @docs/multi-agent-coordination.md — Coordination tools, delegation, controller enforcement, PR workflows
- @docs/autonomous-tasks.md — Autonomous task execution and planning loops
- @docs/api-reference.md — REST API endpoints (public and internal)
- @docs/chat.md — Chat endpoint, tools, SSE streaming
- @docs/configuration.md — CRD configuration reference
- @docs/development.md — Build, test, and development setup
- @docs/testing.md — Test structure, patterns, and chat testing guidelines
- @docs/security.md — Security model, worker hardening, multi-tenancy
- @docs/agent-runtimes.md — Claude Code CLI and Copilot CLI runtime configuration
- @docs/openai-compat.md — OpenAI-compatible API proxy
- @docs/cicd-integration.md — CI/CD integration patterns
- @docs/getting-started.md — Installation and quick start
- @docs/ui.md — Web dashboard architecture
