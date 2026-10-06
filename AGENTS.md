# AGENTS.md

Orka is a Kubernetes-native task execution platform that manages Jobs and Pods for container tasks and AI agent tasks.

## Non-negotiables

- **Never commit, log, or print credentials** — API keys, tokens, or secrets of any kind. Use Kubernetes Secrets or env vars.
- **No binaries in the repository** — put build artifacts in `bin/` (gitignored) or CI release pipelines.
- **Scope discipline** — implement exactly what was asked, nothing more. Report incidental, out-of-scope findings rather than fixing them, unless necessary for the requested outcome.
- **Never push to `main`.** Push to the current branch after a change when it is not `main`.
- **Transaction-token integration is fail-closed** — never store raw TxTokens in Task specs/status/logs. Use owner-referenced Secrets for child tokens, safe metadata/digests for audit, subset checks for child scopes, and fail-closed TTS exchanges for outbound scopes.
- **Never edit generated files** (below), and never delete `// +kubebuilder:scaffold:*` comments.

## Review before landing

For non-trivial code changes, run `$autoreview` (`.agents/skills/autoreview/SKILL.md`) before final/commit/ship and keep going until there are no accepted/actionable findings, unless the change is trivial/docs-only, equivalent manual review already happened, or the human opts out.

- Treat review output as advisory: verify every finding against the real code path before changing code.
- If review-triggered fixes change code, rerun focused tests and rerun `$autoreview`.
- Format before review when formatting can move line locations; focused tests and review may run in parallel only after formatting is stable.

## PR Closeout

After creating or updating an agent-authored PR, use `$pr-closeout` (`.agents/skills/pr-closeout/SKILL.md`) by default, like `$autoreview` is used before landing. Resolve merge conflicts, fix failing CI, address or push back on unresolved review threads, reply on GitHub and resolve addressed comments, push the non-main PR branch, and repeat until current CI is green and no unresolved actionable review threads remain. Skip only when the human opts out, the PR is intentionally draft/WIP, or the remaining blocker is external/human-only. Do not merge or enable auto-merge unless explicitly asked.

## Build & Test

```bash
make manifests          # Regenerate CRDs (after editing *_types.go or markers)
make generate           # Regenerate Go types
make build              # Build (includes UI)
make test               # Run tests
make lint-fix           # Lint and fix
make docker-build-all   # Controller, AI/general workers, ACP runtimes, publisher
make deploy IMG=<repo>@sha256:<digest> ACP_CODEX_RUNTIME_IMG=<repo>@sha256:<digest> ACP_CLAUDE_RUNTIME_IMG=<repo>@sha256:<digest> ACP_COPILOT_RUNTIME_IMG=<repo>@sha256:<digest> ACP_OPENCODE_RUNTIME_IMG=<repo>@sha256:<digest> WORKSPACE_PUBLISHER_IMG=<repo>@sha256:<digest>
```

UI: `cd ui && bun install && bun run dev` (dev server on :5173). See `website/docs/development/development.md` for full commands.

For testing against a local Kubernetes cluster, use the `$kindctl` skill to manage repo/worktree-scoped kind clusters without touching the global kubeconfig.

To stand up a reverse proxy for Anthropic/Gemini/OpenAI-compatible clients, use the `$vekil-reverse-proxy-deploy` skill. When it falls back to GitHub Copilot device-code login, surface the login code and URL to the user and wait for their confirmation before continuing — never complete the login on their behalf.

External ACP provider APIs, binaries, and scoped local proof scripts live in the `orka-workspace` repository. Install the native backend separately. Pooled Substrate MCP Tools remain in Orka behind `--substrate-mcp-tools-enabled`; that flag does not admit ACP allocations. Drain every legacy ACP allocation under the previous release before upgrading its CRDs or controller; see `docs/development/workspace-provider-authoring.md`.

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
- `manifest_staging/deploy/orka.yaml`, `manifest_staging/charts/orka/**` — `make manifests`
- `deploy/**`, `charts/orka/**` — `make promote-staging-manifest` (release-preparation only)
- `**/zz_generated.*.go` — `make generate`
- `PROJECT` — kubebuilder CLI
- `ui/src/routeTree.gen.ts` — TanStack Router

## Code Style

- Use `orka.ai` and its subdomains for Orka domains, Kubernetes API groups, and label/annotation prefixes. Use `ai.orka.*` for reverse-domain identifiers such as Docker labels.
- Structured logging: `log := log.FromContext(ctx); log.Info("msg", "key", val)`
- LLM tool args for nested objects arrive as `map[string]any`, not strings — always type-switch
- Put model-readable tool constraints in JSON Schema (`maximum`, `minimum`, `enum`, `default`), then validate and enforce them again in `Execute`; schema is guidance, not a runtime trust boundary
- Memory features are governance-first: `remember` and `propose_memory` create review proposals, not durable memories

## Gotchas

- Worker filesystem is read-only except `/tmp`, `/home/worker`, and `/workspace`
- Go builds embed `internal/uiembed/dist`: `make build` builds the UI itself and `make vet`/`lint`/`test` create a stub, but in a fresh checkout run `make ensure-ui-embed` before raw `go build`/`go vet`/`go test`
- AI worker truncates messages on context overflow — keeps system prompt + newest, drops middle atomically with structured metadata
- `code_exec` timeout is clamped to 60s — a larger caller value becomes 60s, not an error. 30s is the default only when the caller supplies none
- Built-in AI worker tools: `web_search`, `code_exec`, `file_read`, `web_fetch`, `file_write`, `request_approval`, and gateway-only `reply_in_conversation` (also registered in the production ACP broker). Reply accepts only `content`, requires authenticated durable gateway origin and permitted tool/transaction policy, and does not stop the normal tool loop. External ACP profiles must explicitly opt in before session freezing. Only the first five tools are proxied through the OpenAI/Anthropic compatibility endpoints (`builtinProxyTools`)
- Built-in agent runtimes (`codex`, `claude`, `copilot`, `opencode`) use only the `orka.harness.v2` ACP RuntimePool path; there is no per-Task Job or legacy fallback.
- Execution workspaces (`Task.spec.execution.workspace.classRef`) require `--acp-workspace-dispatch-enabled` and `--enable-workspace-provider-api`, use separately deployed providers, bind a dedicated single-session `acp-ws-*` RuntimePool, and fail closed on unsupported options, including pooled classes, harness-v1, and retention past workspace deletion. For any change touching workspace admission, RBAC, runtime auth, checkpoints, or cleanup:
  - Class identity is frozen in the execution snapshot; Task attachment is exclusive and epoch-fenced; provider-native identifiers never enter Task status.
  - Checkpoint export needs `use` on the source workspace; `restoreFrom` needs `use` on the checkpoint.
  - Suspension preserves data only, never process memory. Core verifies exact Pod absence or the current-generation native-process termination for its authorized instance; resume cold-boots with rotated bootstrap credentials.
  - Read `docs/adr/README.md` and ADRs 0024–0031 first (0031 supersedes parts of 0027/0030).
- `Task.spec.workspace` is the only agent repository surface. Keep clone/read credentials in `readCredentialRef` and publication/forge credentials in `publicationCredentialRef`; neither enters the ACP process tree.
- RuntimePools are controller-owned, digest-pinned, scale-to-zero resources. Only `Serving` + `Accepting` admits new RuntimeSessions; drain/finalization must complete before replacement or scale-down.
- Safe v2 probes are `GET /v2/health` and `GET /v2/capabilities`; status and all mutations require controller authentication plus operation-scoped authorization and exact fences.
- External v2 `runtimeRef` dispatch freezes and revalidates the AgentRuntime UID, generation, profile, endpoint, authentication Secret versions, and observed runtime instance. External v2 sessions receive no per-Task `AgentConfiguration`; the registered profile digest remains authoritative. Harness v1 registrations and dispatch remain available when explicitly enabled.
- ACP runtime Pods run the supervisor as root with narrowly added process/identity capabilities; ACP children use distinct non-reused UIDs/GIDs, private session trees, and no Git credentials.
- Coordination memory tools: `recall_memory`, `remember`, `propose_memory`, `search_transcript`
- Do not store secrets, credentials, tokens, raw transcripts, or one-off task status in durable memory
- Reviewing a memory proposal does not apply it; use the explicit proposal apply endpoint for accepted `memory` proposals when durable memory should be created
- Kontxt TxTokens are accepted via `Txn-Token` by default; `Authorization: Bearer` context-token support is opt-in so ServiceAccount/OIDC auth can coexist
- Live GitHub OIDC/kontxt E2E requires GitHub Actions `id-token: write` or `ORKA_GITHUB_OIDC_TOKEN`; redact JWTs, TxTokens, and request tokens in logs
- OpenTelemetry GenAI constants are hand-rolled in `internal/tracing/genai`; telemetry is enabled with `--enable-telemetry`/`--enable-tracing`, workers honor `ORKA_ENABLE_TELEMETRY`, and prompt/completion content capture remains default-off/fail-closed
- ACP real-world validation should include Codex, Claude, and OpenCode through Vekil, Copilot image/profile admission (plus live execution when provider auth is available), workspace clone/read, Session continuation, cancellation/timeout, unsafe workspace rejection, controller restart, pool replacement, clean-room branch publication, PR reconciliation, and cleanup.
