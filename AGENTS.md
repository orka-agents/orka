# AGENTS.md

Orka is a Kubernetes-native task execution platform that manages Jobs and Pods for container tasks and AI agent tasks.

## Constraints

- **No secrets** — never commit, log, or print API keys, tokens, or credentials. Use Kubernetes Secrets or env vars.
- **No binaries in repo** — build artifacts go in `bin/` (gitignored) or CI release pipelines.
- **Scope discipline** — implement exactly what's asked, nothing more.
- **Pre-land/pre-commit code changes**: use `$autoreview` until no accepted/actionable findings remain, unless equivalent manual review already done, trivial/docs-only, or user opts out.
- **Git push discipline** — after making a change, push to the current branch when it is not `main`; never push directly to `main`.
- **Post-PR closeout** — after creating or updating an agent-authored PR, use `$pr-closeout` until current CI/review blockers are resolved, unless the human opts out or the PR is intentionally draft/WIP.

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

## PR Closeout

After creating or updating an agent-authored PR, use `$pr-closeout` (`.agents/skills/pr-closeout/SKILL.md`) by default, like `$autoreview` is used before landing. Resolve merge conflicts, fix failing CI, address or push back on unresolved review threads, reply on GitHub and resolve addressed comments, push the non-main PR branch, and repeat until current CI is green and no unresolved actionable review threads remain. Skip only when the human opts out, the PR is intentionally draft/WIP, or the remaining blocker is external/human-only. Do not merge or enable auto-merge unless explicitly asked.

## Build & Test

```bash
make manifests          # Regenerate committed staging manifests and Helm chart
make generate           # Regenerate Go types
make build              # Build (includes UI)
make test               # Run tests
make lint-fix           # Lint and fix
make docker-build-all   # Controller, AI/general workers, harness wrapper image
make deploy IMG=<registry>/orka:tag HARNESS_WRAPPER_IMG=<registry>/agent-harness-wrapper:tag
```

### Helm generation and release snapshots

- Helm generator inputs live under `cmd/build/helmify/`; canonical Kubernetes inputs remain under `config/`.
- `make manifests` regenerates the committed next-release outputs in `manifest_staging/deploy/orka.yaml` and `manifest_staging/charts/orka/`. Edit the source inputs, not generated staging files, and commit both source and regenerated output.
- Root `deploy/` and `charts/orka/` are promoted release snapshots. Do not edit them directly; only the release-preparation flow runs `make release-manifest` and `make promote-staging-manifest`. Staging may intentionally be ahead of the root snapshots.
- A pushed `v*` tag packages and publishes the already-reviewed root snapshot. Tag publication must not regenerate or promote manifests.
- Chart CRDs are generated from `config/crd/bases/`. Helm does not update them during `helm upgrade`; apply the CRDs from the exact target chart before upgrading the release.

UI: `cd ui && bun install && bun run dev` (dev server on :5173). See @website/docs/development/development.md for full commands.

For testing against a local Kubernetes cluster, use the `$kindctl` skill to manage repo/worktree-scoped kind clusters without touching the global kubeconfig.

To stand up a reverse proxy for Anthropic/Gemini/OpenAI-compatible clients, use the `$vekil-reverse-proxy-deploy` skill. When it falls back to GitHub Copilot device-code login, surface the login code and URL to the user and wait for their confirmation before continuing — never complete the login on their behalf.

To stand up an execution-workspace provider on a local kind cluster for evaluation, use the `$agent-sandbox-deploy` skill (kubernetes-sigs agent-sandbox; pairs with `$kindctl` for the cluster and `$orka-kind-deploy` for the controller) or the `$agent-substrate-deploy` skill (Agent Substrate; owns its own gVisor kind cluster, so it is not hosted on a `$kindctl` cluster). Both are local/kind eval only — Orka does not install or manage these providers in production — and both surface the `$vekil-reverse-proxy-deploy` device-code login to the user for confirmation rather than completing it.

## Verification

Run after every change:

```bash
make manifests                   # After CRD/RBAC/Kustomize or Helm generator input changes
make generate                    # After generated Go type input changes
make lint-fix && make test       # After any *.go edits
cd ui && bun run lint && bun run test  # After UI edits
bash -n scripts/*.sh                  # After shell script edits
go run github.com/rhysd/actionlint/cmd/actionlint@latest .github/workflows/<workflow>.yml  # After workflow edits
```

Single test: `go test ./internal/api/ -run TestHandlerName -v`

## Auto-Generated — Do NOT Edit

- `config/crd/bases/*.yaml`, `config/rbac/role.yaml` — `make manifests`
- `manifest_staging/deploy/orka.yaml`, `manifest_staging/charts/orka/**` — `make manifests`
- `deploy/**`, `charts/orka/**` — promoted release snapshots from `make promote-staging-manifest`
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
- Agent CLI runtimes (`codex`, `claude`, `copilot`) run through the `agent-harness-wrapper`; the old per-runtime worker images/entrypoints are gone.
- Harness-wrapper success maps `TurnCompleted` to `AgentRuntimeCompleted` + terminal task events; do not expect a worker `ResultSubmitted` event on harness-backed agent tasks.
- Harness wrapper `GET /v1/health` and `GET /v1/capabilities` are intentionally unauthenticated; mutating turn endpoints (`POST /v1/turns`, cancel) require the wrapper bearer token.
- `runtimeRef` AgentRuntime tasks are remote-runtime tasks; do not expect a Kubernetes Job/Pod per task. Orka remains the governance plane, brokered tools execute through Orka, and remote adapters receive only harness auth plus safe tool schemas, never downstream production tool credentials.
- The harness wrapper may emit restricted PodSecurity warnings because it runs as root with limited capabilities for child process/credential setup; rollout success plus runtime live tests are the source of truth.
- Coordination memory tools: `recall_memory`, `remember`, `propose_memory`, `search_transcript`
- Do not store secrets, credentials, tokens, raw transcripts, or one-off task status in durable memory
- Reviewing a memory proposal does not apply it; use the explicit proposal apply endpoint for accepted `memory` proposals when durable memory should be created
- Kontxt TxTokens are accepted via `Txn-Token` by default; `Authorization: Bearer` context-token support is opt-in so ServiceAccount/OIDC auth can coexist
- Live GitHub OIDC/kontxt E2E requires GitHub Actions `id-token: write` or `ORKA_GITHUB_OIDC_TOKEN`; redact JWTs, TxTokens, and request tokens in logs
- OpenTelemetry GenAI constants are hand-rolled in `internal/tracing/genai`; telemetry is enabled with `--enable-telemetry`/`--enable-tracing`, workers honor `ORKA_ENABLE_TELEMETRY`, and prompt/completion content capture remains default-off/fail-closed
- Harness-wrapper real-world validation should include at least one Codex/Claude task through Vekil, workspace clone/read, fork/checkpoint continuation, cancel/timeout, unsafe workspace URL rejection, and (when a GitHub token is available) branch push + PR creation/cleanup on a temporary branch.
