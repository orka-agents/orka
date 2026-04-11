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

## Project Overview

Orka is a Kubernetes-native task execution platform that manages Jobs and Pods for container tasks and AI agent tasks.

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

Built-in AI worker tools: `web_search` (with DuckDuckGo fallback), `code_exec` (with deny-pattern safety guards for bash), `file_read`, `web_fetch` (URL content extraction), `file_write`.

## Auto-Generated Files — Do NOT Edit

- `config/crd/bases/*.yaml` — regenerate with `make manifests`
- `config/rbac/role.yaml` — regenerate with `make manifests`
- `**/zz_generated.*.go` — regenerate with `make generate`
- `PROJECT` — managed by kubebuilder CLI
- `ui/src/routeTree.gen.ts` — managed by TanStack Router

Do NOT delete `// +kubebuilder:scaffold:*` comments — the CLI injects code at these markers.

## Workflow Principles

- **Scope discipline** — Implement exactly what's asked, nothing more. Don't add nice-to-haves, optional features, or "improvements" that weren't requested. When in doubt, leave it out.
- **Continuous verification** — Run `make lint-fix` and `make test` after each logical phase of work, not just at the end. Catch problems early.

## Code Style & Conventions

### Go
- Structured logging: `log := log.FromContext(ctx); log.Info("msg", "key", val)`
- LLM tool args for nested objects arrive as `map[string]any`, not strings — always type-switch

## Verification

```bash
make manifests generate  # After editing *_types.go or markers
make lint-fix && make test  # After editing any *.go files
cd ui && bun run lint && bun run test  # After editing UI code
```

Single tests: `go test ./internal/api/ -run TestHandlerName -v`

## Key Gotchas

- Worker filesystem is read-only except `/tmp`, `/home/worker`, and `/workspace` — writes elsewhere will fail
- `make build` requires UI assets — run `make ui-build` first, or the `ensure-ui-embed` target creates a stub
- On context length errors, the AI worker truncates messages using a token-budget approach — keeps the first message (system prompt) and newest messages, dropping middle messages atomically. The truncation note includes structured metadata (tool names, file paths, URLs) extracted from dropped blocks so the LLM retains context about prior work
- `code_exec` tool accepts timeout up to 60s — values above 60 are ignored and the 30s default is used
