---
slug: /testing
---

# Testing

Orka has comprehensive test coverage across all packages, including unit tests, integration tests (envtest), end-to-end tests (Kind cluster), and frontend tests.

## Running Tests

```bash
# Run test pipeline (manifests, generate, fmt, vet, then Go tests)
make test

# Run Go tests with coverage report
make test
go tool cover -func=cover.out | grep total

# Run frontend tests
make ui-test                # or: cd ui && bun run test
make ui-test-coverage       # or: cd ui && bun run test:coverage

# Run E2E tests (requires isolated Kind cluster)
make test-e2e

# Run Agent Substrate E2E (requires Docker, Go, git, curl, kind, kubectl, ko, jq)
SUBSTRATE_E2E_EXTENDED=1 bash scripts/agent-substrate-e2e.sh

# Lint
make lint
make lint-fix
make ui-lint
```

## Test Structure

### Go Tests

Tests use **Ginkgo + Gomega** (BDD style) for controller/integration tests and standard Go `testing` for unit tests.

| Package | Test Files | Coverage Areas |
|---------|-----------|----------------|
| `internal/api/` | `handlers_test.go`, `internal_handlers_test.go`, `auth_test.go`, `middleware_test.go`, `pagination_test.go`, `server_test.go`, `openai_compat_test.go` | REST API handlers, internal API handlers, memory/session APIs, authentication, middleware, pagination, OpenAI compatibility |
| `internal/controller/` | `task_controller_test.go`, `agent_controller_test.go`, `tool_controller_test.go`, `session_manager_test.go`, `job_builder_test.go`, `repositoryscan_controller_test.go`, `webhook_test.go` | Reconciliation logic, session management, job building, coordination enforcement, repository scan mapper/finding/patch ingestion |
| `internal/security/` | `security_test.go`, `contracts_test.go` | Repository security artifact contracts, v2 evidence validation, fingerprinting, bounded context manifests, prompt helpers |
| `internal/security/slices/` | `mapper_test.go` | Deterministic review-slice mapper coverage for Go, Node/TypeScript, Python, workflows, scripts, config, path skipping, and stable output |
| `internal/store/sqlite/` | `security_store_test.go` | Repository security store migrations, findings, review slices, dropped finding diagnostics, patch proposals |
| `internal/llm/` | `provider_test.go` | Provider registry |
| `internal/llm/anthropic/` | `provider_test.go` | Anthropic API integration |
| `internal/llm/openai/` | `provider_test.go` | OpenAI API integration |
| `internal/metrics/` | `metrics_test.go` | Prometheus metrics recording |
| `internal/tools/` | `registry_test.go`, memory tool tests, coordination tool tests, PR tool tests, agent-management tool tests, `integration_test.go` | Built-in tool implementations, memory tools, coordination tools, PR tools, agent management tools |
| `internal/worker/` | `tool_executor_test.go` | Custom Tool CRD executor |
| `workers/ai/` | `main_test.go` | AI worker functions |
| `workers/general/` | `main_test.go` | General worker functions |
| `workers/agent/copilot/` | `main_test.go` | Copilot agent worker |
| `workers/agent/claude/` | `main_test.go` | Claude agent worker |
| `workers/agent/codex/` | `main_test.go` | Codex agent worker |

### E2E Tests

End-to-end tests run against a dedicated Kind cluster:

| Test File | Coverage |
|-----------|----------|
| `test/e2e/e2e_test.go` | Core task lifecycle |
| `test/e2e/agent_test.go` | Agent task execution |
| `test/e2e/agent_copilot_test.go` | Copilot runtime |
| `test/e2e/agent_claude_test.go` | Claude runtime |
| `test/e2e/agent_workspace_test.go` | Workspace/git clone |
| `test/e2e/agent_session_test.go` | Session continuity |
| `test/e2e/autonomous_mode_test.go` | Autonomous iterations, max-iteration stop, Plan API, suspend behavior |
| `test/e2e/coordination_advanced_test.go` | `cancel_task`, inter-task messaging, auto-retry, dynamic agent create/delete |
| `test/e2e/pr_workflow_test.go` | PR tool workflow (`create_pull_request`, review/comment/merge) and workspace PR env wiring |
| `test/e2e/api_coverage_test.go` | Sessions, agent update API, single-tool API, auth validation, secrets API, chat delete, non-autonomous plan 404 |
| `test/e2e/chat_advanced_test.go` | JSON chat mode, `agentRef` chat routing, management tools via chat |
| `test/e2e/security_enforcement_test.go` | Non-root execution, read-only filesystem, deny-pattern enforcement, kube-system chat block |
| `test/e2e/agent_advanced_test.go` | Skills ConfigMap wiring, agent resource propagation, session maxMessages behavior |
| `test/e2e/workspace_advanced_test.go` | Advanced workspace settings (`gitSecretRef`, `subPath`, `ref`, fork/PR env vars, session init container) |
| `test/e2e/provider_advanced_test.go` | Provider rate-limit config coverage |
| `test/e2e/live_copilot_proxy_test.go` | Live Orka Provider + `type: ai` path using copilot-proxy as the backend harness, including durable memory recall, proposal governance, and transcript search tool execution |
| `test/e2e/live_chat_api_test.go` | Live chat SSE and JSON transport/session coverage using a proxy-backed Provider |
| `test/e2e/live_anthropic_compat_test.go` | Live Anthropic-compatible `/anthropic/v1/models` and `/anthropic/v1/messages` coverage with default tools-enabled behavior |
| `test/e2e/live_agent_runtime_matrix_test.go` | Live Orka runtime matrix: Codex+GPT, Claude Code+Claude, Copilot+Gemini |
| `.github/workflows/live-agent-sandbox-e2e.yml` / `scripts/live-agent-sandbox-e2e.sh` | Live upstream `agent-sandbox` Kind validation for Orka agent workspace claim, sandbox execution, delete cleanup, retained-session reuse, and token scrubbing using a fake model-free Claude runtime |
| `.github/workflows/security-scan-e2e.yml` / `scripts/security-scan-e2e.sh` | Secret-free repository security scan Kind validation against pinned `sozercan/nodejs-goof` using the real mapper, deterministic fake Codex analyzer, v2 finding ingestion/drop diagnostics, threat-model rejection, idempotent rescan, and HITL no-auto-patch gating |
| `test/e2e/tools_test.go` | Built-in tools (including `web_fetch`, `file_write`) and custom Tool CRD |
| `test/e2e/scheduled_task_test.go` | Cron scheduling, suspend, `concurrencyPolicy: Forbid`, history-limit cleanup |
| `test/e2e/task_lifecycle_test.go` | Timeout/retry/cancel plus session serialization and lock release |

Repository security E2E coverage should include initial deterministic slice creation,
incremental scan behavior, invalid v2 evidence being dropped and visible through API,
validation task persistence, successful verified patch proposals, and patch proposals with
missing or mismatched artifacts staying not ready.

### E2E Key Requirements

- `E2E_OPENAI_API_KEY`: required for LLM-backed tests (AI chat/tasks, coordination, PR workflow orchestration)
- `E2E_ANTHROPIC_API_KEY`: required for Anthropic-specific e2e cases
- `E2E_GITHUB_TOKEN`: required for GitHub/Copilot and live Copilot runtime tests
- `COPILOT_GITHUB_TOKEN`: required by the live `copilot-proxy` workflow for proxy auth
- The live agent sandbox workflow requires Docker, Kind, kubectl, curl, jq, and network access to install the pinned upstream `agent-sandbox` release. It does not require model credentials.
- GitHub Actions `id-token: write` permission: required by the live GitHub OIDC workflow. For local/manual runs of `scripts/live-github-oidc-e2e.sh`, set `ORKA_GITHUB_OIDC_TOKEN` to a valid JWT instead. The same workflow also runs a self-contained `kontxt` TxToken check using an ephemeral key/JWKS fixture, so no external kontxt secret is required.
- `E2E_LIVE_COPILOT_PROXY_BASE_URL` (or `E2E_COPILOT_PROXY_BASE_URL` / `COPILOT_PROXY_BASE_URL`): enables the focused live copilot-proxy spec against a running proxy
- `E2E_LIVE_COPILOT_PROXY_SERVICE_NAMESPACE`, `E2E_LIVE_COPILOT_PROXY_SERVICE_NAME`, `E2E_LIVE_COPILOT_PROXY_SERVICE_PORT`: optional overrides for how the live spec reaches the in-cluster proxy service for `/readyz` and `/v1/models` checks
- Structural e2e tests (job/env/volume assertions) run without external model keys
- Security Scan E2E is secret-free and model-free, but requires Docker plus local toolchain dependencies: Go, `kind`, `kubectl`, `curl`, and `jq`
- Agent Substrate E2E is secret-free, but requires Docker plus local toolchain dependencies: Go, git, curl, `kind`, `kubectl`, `ko`, and `jq`

The live copilot-proxy E2E path runs in a separate workflow and executes the focused live suites for:

- provider-backed `type: ai` tasks, including durable memory/tool execution coverage
- chat SSE/JSON flows via `/api/v1/chat`
- Anthropic-compatible `/anthropic/v1/models` and `/anthropic/v1/messages` flows with the default Orka tool loop enabled
- external agent runtimes across `codex` + GPT, `claude` + Claude, and `copilot` + Gemini

This is an **Orka** live integration suite, not a deep `copilot-proxy` feature suite. The proxy is test harness infrastructure that gives non-Copilot runtimes access to live GPT, Claude, and Gemini models in CI. The only proxy-specific assertions are smoke checks that the harness is alive and usable:

- `/readyz` returns healthy
- `/v1/models` is non-empty
- GPT, Claude, and Gemini model families are present

It bootstraps a fresh Kind cluster, deploys the published multi-arch `docker.io/sozercan/copilot-proxy:latest` image, injects `COPILOT_GITHUB_TOKEN` for proxy auth, requires the live proxy to expose GPT/Claude/Gemini model families, maps that same secret to `E2E_GITHUB_TOKEN` for the Copilot runtime case, and then runs the focused live suites against the in-cluster proxy.

Model selection is endpoint-specific. Provider-backed `type: ai` tasks and `/api/v1/chat` probe the live proxy before choosing an OpenAI-compatible Chat Completions model, because a model can appear in the catalog while still being rejected for that endpoint. The Codex runtime uses GPT models that work with the Responses API, while the Claude and Copilot runtime matrix cases use Claude and Gemini families respectively. Keep these preferences in `test/e2e/helpers_test.go` aligned with the live proxy's allowed models rather than assuming one model family works across every endpoint.

The live agent sandbox workflow (`.github/workflows/live-agent-sandbox-e2e.yml`) runs `scripts/live-agent-sandbox-e2e.sh`. It installs upstream `agent-sandbox` `v0.4.6`, builds the PR controller image, builds a fake Claude worker image that also hosts the sandbox `/execute` and file APIs, builds the pinned upstream sandbox router image, and validates that Orka can run an agent Task inside the claimed sandbox without external model access. The script asserts:

- the outer worker re-execs inside the sandbox with `ORKA_AGENT_SANDBOX_DEPTH=1` and sandbox recursion disabled
- the staged service account token is available to the inner worker while the command runs
- `cleanupPolicy: delete` removes the generated `SandboxClaim`
- `cleanupPolicy: retain` plus `reusePolicy: session` reattaches to the deterministic session claim
- retained workspace state persists across tasks
- staged token files are scrubbed before the retained workspace is left behind

The live GitHub OIDC workflow (`.github/workflows/live-github-oidc-e2e.yml`) runs `scripts/live-github-oidc-e2e.sh` in GitHub Actions with `id-token: write`. It builds the controller from the PR, deploys it to a fresh Kind cluster, configures `ORKA_OIDC_ISSUER=https://token.actions.githubusercontent.com` and the workflow audience, fetches a real GitHub Actions OIDC token, generates a real `kontxt` TxToken against an in-cluster JWKS endpoint, and validates:

- unauthenticated API requests return `401`
- OIDC-authenticated Task creation returns `201`
- the OIDC-created Task response and persisted CR contain `spec.requestedBy` with the GitHub OIDC issuer and a non-empty subject
- the `kontxt`-created Task response and persisted CR contain `spec.requestedBy` with the configured kontxt issuer, subject, and scope-derived roles
- top-level `requestedBy` and nested `spec.requestedBy` client tampering are rejected with `400`
- a tampered `kontxt` TxToken is rejected with `401`

The Agent Substrate workflow (`.github/workflows/agent-substrate-e2e.yml`) is secret-free and runs `scripts/agent-substrate-e2e.sh` against a fresh Kind cluster. It pins the Substrate checkout with `SUBSTRATE_REF`, installs Substrate, initializes the local RustFS snapshot bucket, builds local Orka controller/workspace/worker images, then validates:

- direct Substrate actor create/resume/router/daemon exec/suspend/delete
- Orka `SubstrateActorPool` reconciliation and density reporting
- Orka `Task` execution and result submission with the default Substrate workspace provider
- pooled Orka `Task` placement through `spec.execution.workspace.poolRef`
- MCP actor-backed `Tool` execution through a pooled Substrate actor
- MCP actor reuse across forced Tool reconciles without rebooting an already booted actor
- workspace placement, density, and resume-latency status fields
- delete and retained cleanup when the pinned Substrate runtime completes `runsc delete`
- `WorkspaceCleanupFailed` is tolerated only after the Task result is available, because the pinned Substrate revision can fail `runsc delete` after successful Orka execution in GitHub-hosted kind
- a missing `ActorTemplate` fails predictably
- failure diagnostics include Orka controller logs, worker Job logs, Task YAML, Kubernetes events, and Substrate actor/worker state

Run it locally with:

```bash
PATH="$(go env GOPATH)/bin:$PATH" \
SUBSTRATE_E2E_EXTENDED=1 \
bash scripts/agent-substrate-e2e.sh
```

### Frontend Tests

Frontend tests use **Vitest + Testing Library + MSW**. Coverage thresholds are enforced in `vite.config.ts`.

```bash
cd ui && bun run test:coverage
```

## Testing Patterns

### Table-Driven Tests

```go
tests := []struct {
    name    string
    input   string
    want    string
    wantErr bool
}{
    {"valid", "input", "output", false},
    {"invalid", "bad", "", true},
}
for _, tt := range tests {
    t.Run(tt.name, func(t *testing.T) {
        // test logic
    })
}
```

### Fake Kubernetes Client

```go
scheme := runtime.NewScheme()
corev1alpha1.AddToScheme(scheme)
corev1.AddToScheme(scheme)
client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
```

### HTTP Mocking

```go
server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
    w.WriteHeader(http.StatusOK)
    w.Write([]byte(`{"result": "ok"}`))
}))
defer server.Close()
```

### Fiber Test App

```go
app := fiber.New()
app.Get("/test", handler)
req := httptest.NewRequest(http.MethodGet, "/test", nil)
resp, _ := app.Test(req)
```

### Frontend Test Mocking

```typescript
// Mock zustand persist middleware
vi.mock('zustand/middleware', () => ({ persist: (fn: unknown) => fn }))

// Use test utils with QueryClient wrapper
import { render } from '@/test/test-utils'
```

## Testing with Chat

When testing features via the chat endpoint, use **natural prompts** — the kind a human would actually type. Never reference internal concepts like agent names, tool names, or implementation details. Describe what you want done, not how the system should do it. The chat should infer the right agents, tools, delegation patterns, and cancellation logic on its own.

Good examples:
- "Research the benefits of Kubernetes and write a technical guide based on the findings."
- "What's the best container orchestration tool? Get me an answer as fast as possible."
- "Draft an outline for a blog post about containers and turn it into a full post."
- "Compare microservices vs monoliths from three angles, then synthesize into a recommendation."

Bad examples:
- "Create a coordinator agent and a researcher agent, then delegate two tasks..."
- "Use the send_message tool to send a message to task msg-receiver..."
- "Have three researchers race to answer..." (users don't think in terms of "researchers")
- "Use the first answer and cancel the others." (the system should infer this automatically)
