# AGENTS.md

Orka is a Kubernetes-native task execution platform that manages Jobs and Pods for container tasks and AI agent tasks.

## Constraints

- **No secrets** — never commit, log, or print API keys, tokens, or credentials. Use Kubernetes Secrets or env vars.
- **No binaries in repo** — build artifacts go in `bin/` (gitignored) or CI release pipelines.
- **Scope discipline** — implement exactly what's asked, nothing more.

## Build & Test

```bash
make manifests          # Regenerate CRDs (after editing *_types.go or markers)
make generate           # Regenerate Go types
make build              # Build (includes UI)
make test               # Run tests
make lint-fix           # Lint and fix
make docker-build-all   # All Docker images
make deploy IMG=<registry>/orka:tag
```

UI: `cd ui && bun install && bun run dev` (dev server on :5173). See @docs/development.md for full commands.

## Verification

Run after every change:

```bash
make manifests generate          # After *_types.go or marker edits
make lint-fix && make test       # After any *.go edits
cd ui && bun run lint && bun run test  # After UI edits
bash -n scripts/*.sh                  # After shell script edits
go run github.com/rhysd/actionlint/cmd/actionlint@latest .github/workflows/<workflow>.yml  # After workflow edits
```

Single test: `go test ./internal/api/ -run TestHandlerName -v`

## Auto-Generated — Do NOT Edit

- `config/crd/bases/*.yaml`, `config/rbac/role.yaml` — `make manifests`
- `**/zz_generated.*.go` — `make generate`
- `PROJECT` — kubebuilder CLI
- `ui/src/routeTree.gen.ts` — TanStack Router

Do NOT delete `// +kubebuilder:scaffold:*` comments.

## Code Style

- Structured logging: `log := log.FromContext(ctx); log.Info("msg", "key", val)`
- LLM tool args for nested objects arrive as `map[string]any`, not strings — always type-switch
- Memory features are governance-first: `remember` and `propose_memory` create review proposals, not durable memories

## Gotchas

- Worker filesystem is read-only except `/tmp`, `/home/worker`, and `/workspace`
- `make build` requires UI assets — run `make ui-build` first (or `ensure-ui-embed` creates a stub)
- AI worker truncates messages on context overflow — keeps system prompt + newest, drops middle atomically with structured metadata
- `code_exec` timeout max is 60s — values above are ignored (30s default used)
- Built-in AI worker tools: `web_search`, `code_exec`, `file_read`, `web_fetch`, `file_write`
- Coordination memory tools: `recall_memory`, `remember`, `propose_memory`, `search_transcript`
- Do not store secrets, credentials, tokens, raw transcripts, or one-off task status in durable memory
- Reviewing a memory proposal does not apply it; create durable memory explicitly via the API until an apply flow exists
- Live GitHub OIDC/kontxt E2E requires GitHub Actions `id-token: write` or `ORKA_GITHUB_OIDC_TOKEN`; redact JWTs, TxTokens, and request tokens in logs
