# AGENTS.md

Orka is a Kubernetes-native task execution platform that manages Jobs and Pods for container tasks and AI agent tasks.

## Constraints

- **No secrets** — never commit, log, or print API keys, tokens, or credentials. Use Kubernetes Secrets or env vars.
- **No binaries in repo** — build artifacts go in `bin/` (gitignored) or CI release pipelines.
- **Scope discipline** — implement exactly what's asked, nothing more.
- **Pre-land/pre-commit code changes**: use `$autoreview` until no accepted/actionable findings remain, unless equivalent manual review already done, trivial/docs-only, or user opts out.
- **Git push discipline** — after making a change, push to the current branch when it is not `main`; never push directly to `main`.

## Continuous Review

For non-trivial code changes, run `$autoreview` (`.agents/skills/autoreview/SKILL.md`) before final/commit/ship and keep going until there are no accepted/actionable findings, unless the change is trivial/docs-only, equivalent manual review already happened, or the human opts out.

- Treat review output as advisory: verify every finding against the real code path before changing code.
- If review-triggered fixes change code, rerun focused tests and rerun `$autoreview`.
- Format before review when formatting can move line locations; focused tests and review may run in parallel only after formatting is stable.

## GitHub PRs & Issues

Before creating or updating a GitHub PR or issue body for agent-authored work, read `$agent-transcript` (`.agents/skills/agent-transcript/SKILL.md`) if available and check for a safe local session transcript.

- If a high-confidence transcript is found, ask exactly: "Include a redacted agent transcript? It helps reviewers and can make the PR easier to prioritize. I can open a local preview first."
- Never upload raw logs. Include an `## Agent Transcript` section only after human approval and only with a sanitized, scoped transcript.
- Drop system/developer prompts, reasoning, raw tool outputs, env, cookies, tokens, auth URLs, secrets, broad local paths, and unrelated session turns.
- If no safe transcript exists or the human declines, continue without a transcript and do not add a placeholder section.

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

UI: `cd ui && bun install && bun run dev` (dev server on :5173). See @website/docs/development/development.md for full commands.

For testing against a local Kubernetes cluster, use the `$kindctl` skill to manage repo/worktree-scoped kind clusters without touching the global kubeconfig.

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
- Kontxt integration is fail-closed: never store raw TxTokens in Task specs/status/logs; use owner-referenced Secrets for child tokens, safe metadata/digests for audit, subset checks for child scopes, and fail-closed TTS exchanges for outbound scopes.

## Gotchas

- Worker filesystem is read-only except `/tmp`, `/home/worker`, and `/workspace`
- `make build` requires UI assets — run `make ui-build` first (or `ensure-ui-embed` creates a stub)
- AI worker truncates messages on context overflow — keeps system prompt + newest, drops middle atomically with structured metadata
- `code_exec` timeout max is 60s — values above are ignored (30s default used)
- Built-in AI worker tools: `web_search`, `code_exec`, `file_read`, `web_fetch`, `file_write`
- Coordination memory tools: `recall_memory`, `remember`, `propose_memory`, `search_transcript`
- Do not store secrets, credentials, tokens, raw transcripts, or one-off task status in durable memory
- Reviewing a memory proposal does not apply it; use the explicit proposal apply endpoint for accepted `memory` proposals when durable memory should be created
- Kontxt TxTokens are accepted via `Txn-Token` by default; `Authorization: Bearer` context-token support is opt-in so ServiceAccount/OIDC auth can coexist
- Live GitHub OIDC/kontxt E2E requires GitHub Actions `id-token: write` or `ORKA_GITHUB_OIDC_TOKEN`; redact JWTs, TxTokens, and request tokens in logs
