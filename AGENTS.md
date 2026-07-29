# AGENTS.md

Orka is a Kubernetes-native task execution platform that manages Jobs and Pods for container tasks and AI agent tasks.

## Non-negotiables

- **Never commit, log, or print credentials** — API keys, tokens, or secrets of any kind. Use Kubernetes Secrets or env vars.
- **No binaries in the repository** — put build artifacts in `bin/` (gitignored) or CI release pipelines.
- **Scope discipline** — implement exactly what was asked, nothing more.
- **Never push to `main`.** Push to the current branch after a change when it is not `main`.
- **Transaction-token integration is fail-closed** — never store raw TxTokens in Task specs/status/logs. Use owner-referenced Secrets for child tokens, safe metadata/digests for audit, subset checks for child scopes, and fail-closed TTS exchanges for outbound scopes.
- **Never edit generated files** (below), and never delete `// +kubebuilder:scaffold:*` comments.

## Generated — do not edit

| Path | Regenerate with |
| --- | --- |
| `config/crd/bases/*.yaml`, `config/rbac/role.yaml` | `make manifests` |
| `manifest_staging/deploy/orka.yaml`, `manifest_staging/charts/orka/**` | `make manifests` |
| `deploy/**`, `charts/orka/**` | `make promote-staging-manifest` (release-preparation flow only) |
| `**/zz_generated.*.go` | `make generate` |
| `PROJECT` | kubebuilder CLI |
| `ui/src/routeTree.gen.ts` | TanStack Router |

## Gotchas

Execution model:

- `runtimeRef` AgentRuntime tasks are remote-runtime tasks — there is no Kubernetes Job/Pod per task. Orka stays the governance plane, brokered tools execute through Orka, and remote adapters receive only harness auth plus safe tool schemas, never downstream production tool credentials.
- Agent CLI runtimes (`codex`, `claude`, `copilot`, `opencode`) run through the `agent-harness-wrapper`. The old per-runtime worker images and entrypoints are gone.
- Harness-wrapper success maps `TurnCompleted` to `AgentRuntimeCompleted` plus terminal task events. Do not expect a worker `ResultSubmitted` event on harness-backed agent tasks.
- Harness wrapper `GET /v1/health` and `GET /v1/capabilities` are intentionally unauthenticated; mutating turn endpoints (`POST /v1/turns`, cancel) require the wrapper bearer token.
- The harness wrapper may emit restricted PodSecurity warnings — it runs as root with limited capabilities for child process/credential setup. Rollout success plus runtime live tests are the source of truth.
- When changing the harness wrapper, run the canonical [live validation checklist](website/docs/guides/cli-harness-wrapper.md#live-validation-checklist).

Workers:

- Worker filesystem is read-only except `/tmp`, `/home/worker`, and `/workspace`.
- AI worker truncates messages on context overflow — keeps system prompt plus newest, drops the middle atomically with structured metadata.
- Built-in AI worker tools: `web_search`, `code_exec`, `file_read`, `web_fetch`, `file_write`.

Memory and coordination:

- Coordination memory tools: `recall_memory`, `remember`, `propose_memory`, `search_transcript`.
- Memory is governance-first: `remember` and `propose_memory` create review proposals, not durable memories.
- Reviewing a memory proposal does not apply it. Use the explicit proposal apply endpoint for accepted `memory` proposals when durable memory should be created.
- Never put secrets, credentials, tokens, raw transcripts, or one-off task status in durable memory.

Auth and telemetry:

- Transaction tokens are accepted via `Txn-Token` by default. `Authorization: Bearer` context-token support is opt-in so ServiceAccount/OIDC auth can coexist.
- Live GitHub OIDC E2E requires GitHub Actions `id-token: write` or `ORKA_GITHUB_OIDC_TOKEN`. Redact JWTs, TxTokens, and request tokens in logs.
- OpenTelemetry GenAI constants are hand-rolled in `internal/tracing/genai` because the GenAI conventions are still Development-stage. Telemetry is enabled with `--enable-telemetry`/`--enable-tracing`, workers honor `ORKA_ENABLE_TELEMETRY`, and prompt/completion content capture stays default-off and fail-closed.

Build:

- `make build` requires UI assets — run `make ui-build` first, or `ensure-ui-embed` creates a stub and the embedded UI won't work.

Helm manifests and release snapshots:

- Helm generator inputs live under `cmd/build/helmify/`; canonical Kubernetes inputs remain under `config/`.
- `make manifests` regenerates the committed next-release outputs in `manifest_staging/deploy/orka.yaml` and `manifest_staging/charts/orka/`. Edit the source inputs, not generated staging files, and commit both source and regenerated output.
- Root `deploy/` and `charts/orka/` are promoted release snapshots. Do not edit them directly; only the release-preparation flow runs `make release-manifest` and `make promote-staging-manifest`. Staging may intentionally be ahead of the root snapshots.
- A pushed `v*` tag packages and publishes the already-reviewed root snapshot. Tag publication must not regenerate or promote manifests.
- Chart CRDs are generated from `config/crd/bases/`. Helm does not update them during `helm upgrade`; apply the CRDs from the exact target chart before upgrading the release.

## Code style

- Structured logging: `log := log.FromContext(ctx); log.Info("msg", "key", val)`
- LLM tool args for nested objects arrive as `map[string]any`, not strings — always type-switch.
- Put model-readable tool constraints in the JSON Schema (`maximum`, `minimum`, `enum`, `default`), not only in description prose, and validate and enforce them at runtime in `Execute`; schema guides the model but is not a runtime trust boundary.

## Build and verify

```bash
make manifests            # After CRD/RBAC/Kustomize or Helm generator input changes
make generate             # After generated Go type input changes
make lint-fix && make test # After any *.go edits
make build                 # Includes UI; see ui-build gotcha above
make docker-build-all      # Controller, AI/general workers, harness wrapper
make deploy IMG=<registry>/orka:tag HARNESS_WRAPPER_IMG=<registry>/agent-harness-wrapper:tag
```

```bash
cd ui && bun run lint && bun run test                    # After UI edits
bash -n scripts/*.sh                                      # After shell script edits
go run github.com/rhysd/actionlint/cmd/actionlint@latest .github/workflows/<workflow>.yml
```

Single Go test: `go test ./internal/api/ -run TestHandlerName -v`.
UI dev server: `cd ui && bun install && bun run dev` (:5173).

Full command reference, CI workflow catalog, and OpenTelemetry development notes:
`website/docs/development/development.md`.

## Skills

| Skill | Use for |
| --- | --- |
| `$autoreview` | Review before commit/land on non-trivial code changes. Repeat until no accepted/actionable findings remain. Skip for trivial/docs-only work, equivalent manual review, or when the human opts out. |
| `$pr-closeout` | After creating or updating an agent-authored PR, drive it to green. Skip when the human opts out, the PR is intentionally draft/WIP, or the blocker is external/human-only. |
| `$kindctl` | Repo/worktree-scoped kind clusters, without touching the global kubeconfig. |
| `$orka-kind-deploy` | Rebuild and redeploy the full local stack into a kind cluster. |
| `$vekil-reverse-proxy-deploy` | Reverse proxy for Anthropic/Gemini/OpenAI-compatible clients. |
| `$agent-sandbox-deploy` | kubernetes-sigs agent-sandbox workspace provider (local/kind eval only). |
| `$agent-substrate-deploy` | Agent Substrate workspace provider (local/kind eval only; owns its own cluster). |
