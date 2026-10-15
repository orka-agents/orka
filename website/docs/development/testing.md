---
slug: /testing
description: "The test suites Orka runs, what each one covers, and how to run them."
---

# Testing

Orka has four kinds of tests: Go unit tests, controller integration tests against a real
API server (envtest), end-to-end tests against a throwaway Kind cluster, and frontend tests
in the browser-like Vitest environment. This page describes what each covers and how to run
it.

## Running tests

```bash
# Run test pipeline (manifests, generate, fmt, vet, then Go tests)
make test

# Run Go tests with coverage report
make test
go tool cover -func=cover.out | grep total

# Run release automation and workspace cleanup tests without a cluster
go test ./cmd/build/release ./scripts/tests

# Run frontend tests
make ui-test                # or: cd ui && bun run test
make ui-test-coverage       # or: cd ui && bun run test:coverage

# Run E2E tests (requires isolated Kind cluster)
make test-e2e

# Run only the deterministic Gateway live E2E
KIND_CLUSTER=orka-gateway-e2e \
E2E_GATEWAY=true \
E2E_EPHEMERAL_CLUSTER=true \
E2E_GINKGO_FOCUS="Gateway live E2E" \
make test-e2e

# Run Agent Substrate E2E (requires Docker, Go, git, curl, kind, kubectl, ko, jq)
bash scripts/agent-substrate-e2e.sh

# Lint
make lint
make lint-fix
make ui-lint
```

### Native Codex handoff gate

The `Native Codex handoff` job in `.github/workflows/test.yml` compiles the
supervisor test from the current checkout and runs it inside the pinned production
Codex 0.160.0 / codex-acp 2.1.1 image. Docker networking is disabled; inference uses
a local scripted Responses server and disposable test credentials. The container
uses only the production supervisor's CHOWN, KILL, SETGID, and SETUID capabilities.

The gate restores the original thread UUID through `session/resume`, continues
across fresh homes with rotated credentials, checks current destination policy,
and verifies read and write intents, a publishable workspace delta, native capture,
return history, unchanged source bytes, and runtime cleanup. It explicitly enables
`ORKA_NATIVE_CODEX_TEST=1` and requires the named test to pass, so a skipped test
cannot satisfy the job. This verifies the native integration, not live-model recall.

### Local environment notes

- **Script test suites need bash >= 4.** The suites under `scripts/tests/`
  rely on `set -e` stopping on failed `(( ))` arithmetic, which macOS's stock
  bash 3.2 does not honor — failures would pass silently. The suites refuse to
  run under bash < 4; on macOS install a modern bash (`brew install bash`) and
  invoke the suites with it.
- **A green Gateway E2E can be an empty one.** Without `E2E_GATEWAY=true` the
  gateway specs skip themselves and Ginkgo prints `Ran 0 of N Specs` with a
  `SUCCESS` exit. If you see that line, nothing was validated — re-run with
  the environment shown above. (CI fails this shape explicitly.)
- **Running `go test` directly on controller packages needs envtest assets.**
  `make test` wires `KUBEBUILDER_ASSETS` automatically; for a bare `go test`
  on packages that start an envtest API server, export it first:
  `KUBEBUILDER_ASSETS="$(bin/setup-envtest use -p path)"`.

## Test structure

### Go tests

Tests use **Ginkgo + Gomega** (BDD style) for controller/integration tests and standard Go `testing` for unit tests.

| Package | Test Files | Coverage Areas |
|---------|-----------|----------------|
| `cmd/build/release/` | `workflow_test.go`, `prepare_test.go`, `publish_test.go`, `version_test.go` | Release version edits, trusted tooling, branch races, artifact identity, approval evidence, chart publication, and retries. Uses local Git repositories and Helm; GitHub and registry responses are fixtures. |
| `scripts/tests/` | `workspace_lifecycle_test.go` | Workspace cancellation, session archival, suspension, and deletion ordering using Bash and jq fixtures. |
| `internal/api/` | `handlers_test.go`, `internal_handlers_test.go`, `auth_test.go`, `middleware_test.go`, `pagination_test.go`, `server_test.go`, `openai_compat_test.go` | REST API handlers, internal API handlers, memory/session APIs, authentication, middleware, pagination, OpenAI compatibility |
| `internal/controller/` | `task_controller_test.go`, `agent_controller_test.go`, `tool_controller_test.go`, `session_manager_test.go`, `job_builder_test.go`, `repositoryscan_controller_test.go`, `webhook_test.go` | Reconciliation logic, session management, job building, coordination enforcement, repository scan mapper/finding/patch ingestion |
| `internal/security/` | `security_test.go`, `contracts_test.go` | Repository security artifact contracts, v2 evidence validation, fingerprinting, bounded context manifests, prompt helpers |
| `internal/security/slices/` | `mapper_test.go` | Deterministic review-slice mapper coverage for Go, Node/TypeScript, Python, workflows, scripts, config, path skipping, and stable output |
| `internal/store/sqlite/` | `security_store_test.go` | Repository security records, findings, review slices, dropped finding diagnostics, patch proposals |
| `internal/store/sqlite/` | `schema_test.go`, `integration_test.go` | Complete current schema, incompatible-layout rejection without data loss, repeated reopening with stable record identities and ordering |
| `internal/llm/` | `provider_test.go` | Provider registry |
| `internal/llm/anthropic/` | `provider_test.go` | Anthropic API integration |
| `internal/llm/openai/` | `provider_test.go` | OpenAI API integration |
| `internal/metrics/` | `metrics_test.go` | Prometheus metrics recording |
| `internal/tools/` | `registry_test.go`, memory tool tests, coordination tool tests, PR tool tests, agent-management tool tests, `integration_test.go` | Built-in tool implementations, memory tools, coordination tools, PR tools, agent management tools |
| `internal/worker/` | `tool_executor_test.go` | Custom Tool CRD executor |
| `workers/ai/` | `main_test.go` | AI worker functions |
| `workers/general/` | `main_test.go` | General worker functions |
| `internal/harness/v2/`, `internal/acp/`, `workers/acp/` | ACP contract, client, supervisor, and conformance tests | RuntimeSession lifecycle, exact fences, duplicate handling, event bounds, cancellation, workspace deltas, process cleanup, and redaction |

### E2E tests

End-to-end tests run against a dedicated Kind cluster:

| Test File | Coverage |
|-----------|----------|
| `test/e2e/e2e_test.go` | Core task lifecycle |
| `test/e2e/agent_test.go` | Agent task execution |
| `test/e2e/agent_copilot_test.go` | Copilot built-in profile admission plus exact digest-pinned RuntimePool image selection, without requiring live provider authentication |
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
| `test/e2e/workspace_advanced_test.go` | Workspace source/ref/subPath, separate read/publication credential roles, delivery status, and Session behavior |
| `test/e2e/provider_advanced_test.go` | Provider rate-limit config coverage |
| `test/e2e/live_copilot_proxy_test.go` | Native `type: ai` Provider compatibility against the separately deployed copilot-proxy service; this is separate from built-in Copilot ACP RuntimePool coverage |
| `test/e2e/live_chat_api_test.go` | Live chat SSE and JSON transport/session coverage using a proxy-backed Provider |
| `test/e2e/anthropic_compat_test.go` | Anthropic-compatible `/anthropic/v1/models` and `/anthropic/v1/messages` coverage with default tools-enabled behavior, on aimock or the live proxy |
| `test/e2e/coordination_memory_tools_test.go` | Coordination memory tool auto-injection and execution (`search_transcript`, `recall_memory`, `propose_memory`, `remember`) and proposal review without durable writes, on aimock or the live proxy |
| `test/e2e/agent_runtime_lifecycle_test.go` | Go port of `scripts/agent-runtime-e2e.sh` (label `agent-runtime`): RuntimePool profile projection and execution fences; read, Session continuation, and Task fork for Codex, OpenCode, Claude, and Copilot; an idle adapter killed between prompts, which OpenCode and Claude resume in place under the same RuntimeSession generation (with Claude's resume-time model request refused by the provider proxy) and which Copilot, unable to resume, fails as `RuntimeLost` before rebuilding from the transcript on the next continuation; unsafe workspace URLs; concurrency within capacity; Task timeout and deletion cancellation through the cleanup finalizer; controller restart without replay; Pod replacement and scale-to-zero recovery of the same RuntimeSession; OpenCode read-intent tool policy; and Session archival and RuntimePool cleanup. On aimock it also checks that each continuation, fork, and recovery replayed the prior transcript to the model, except for Copilot: its requests exceed the 64 KB aimock journal cap, so its continuations rely on the RuntimeSession identity checks, as in the bash lane |
| `test/e2e/aimock_runtime_eval_test.go` | Model-free adversarial evals on the actual Codex, Claude, Copilot, and OpenCode CLIs: consumed SCM/proxy credential containment, governed extraction during Session continuation, and repository-origin read-only mutation checks with independent delivery/publication evidence |
| `test/e2e/aimock_authorization_eval_test.go` | Deployed broker allowlist, delegation namespace/agent restrictions, and child transaction-scope containment on a supported external runtimeRef; native publication-authority checks, permitted controls, and a synthetic signing/JWKS/TTS fixture |
| `test/e2e/agent_runtime_matrix_test.go` | Codex against a pinned read workspace, OpenCode read-intent enforcement, and Claude session wiring through the provider proxy (aimock as Vekil, or the live proxy), plus a digest-pinned Copilot image smoke assertion; it is not the canonical ACP release gate |
| `test/e2e/gateway_test.go` | Authenticated Gateway ingress through a deterministic external `AgentRuntime`, including TLS adapter readiness, invalid bearer rejection, accepted and duplicate events, Task execution, completed events, delivered replies, idempotency, and Task/delivery correlation |
| `.github/workflows/gateway-e2e.yml` | Focused, model-free, secret-free Gateway live E2E in Kind using generated bearer tokens, an ephemeral CA, the TLS reference adapter, and the deterministic echo runtime |
| `.github/workflows/live-agent-sandbox-e2e.yml` / `scripts/live-agent-sandbox-e2e.sh` | Live upstream `agent-sandbox` Kind validation for Orka agent workspace claim, sandbox execution, delete cleanup, retained-session reuse, and token scrubbing using a fake model-free Claude runtime |
| `.github/workflows/repository-monitor-smoke.yml` | Focused RepositoryMonitor smoke coverage for store CRUD, API handlers, pull request event handling, targeted single-PR inventory runs, controller queue/review flow, blocked status counts, read-only review task job building, result stdout forwarding, `create_pr_monitor` repository URL and credential validation, GitHub tool `repo_url` scope enforcement, and PR review marker tooling |
| `.github/workflows/security-scan-e2e.yml` / `scripts/security-scan-e2e.sh` | Secret-free repository security scan Kind validation against pinned `sozercan/nodejs-goof` using the real mapper, deterministic fake Codex analyzer, v2 finding ingestion/drop diagnostics, threat-model rejection, idempotent rescan, and HITL no-auto-patch gating |
| `test/e2e/tools_test.go` | Built-in tools (including `web_fetch`, `file_write`) and custom Tool CRD |
| `test/e2e/scheduled_task_test.go` | Cron scheduling, suspend, `concurrencyPolicy: Forbid`, history-limit cleanup |
| `test/e2e/task_lifecycle_test.go` | Timeout/retry/cancel plus session serialization and lock release |
| `scripts/agent-runtime-e2e.sh` | Canonical deployed-cluster ACP smoke/release gate for Codex, OpenCode, Claude, and Copilot RuntimePools, exact Pod/runtime identity, workspace read/write, continuation, cancellation/timeout, restart/replacement, publication/PR verification, immutable images, and cleanup |
| `.github/workflows/agent-runtime-e2e.yml` / `scripts/agent-runtime-kind-e2e.sh` | Manual agent runtime smoke that calls real model providers, requires credentials, and bootstraps an ephemeral Kind cluster, Vekil, and the production ACP topology before invoking the canonical validator |
| `.github/workflows/release-qualification.yml` / `scripts/agent-runtime-kind-e2e.sh` | Environment-approved chart and runtime acceptance, real local Git publication tests, GitHub API fixture tests, and cleanup; no stored Git publication credentials |

The Gateway Live E2E workflow (`.github/workflows/gateway-e2e.yml`) runs on manual dispatch and on pull requests or pushes that touch Gateway-relevant source, configuration, E2E, image, or dependency paths. It creates a dedicated Kind cluster, generates disposable TLS and bearer credentials, deploys the TLS reference adapter and deterministic echo `AgentRuntime`, and verifies invalid bearer rejection, accepted and duplicate ingress, runtime-backed Task completion, final delivery, idempotency, and correlation metadata. The workflow is model-free and secret-free; it does not use repository or provider credentials.

The Repository Monitor Smoke workflow runs in GitHub Actions on pull requests and pushes that touch the workflow, API, controller, CRD/config, worker, or Go dependency paths. It creates the UI embed stub and runs focused `go test` selections for the monitor store, API handlers, GitHub pull request event handling, targeted single-PR inventory runs, controller queue/review flow, blocked status counts, read-only review job construction, result stdout forwarding, `create_pr_monitor` repository URL and credential validation, GitHub tool `repo_url` scope enforcement, and PR review marker signing/detection tooling. The workflow is secret-free: exact PR event queueing is tested with synthetic signed webhook payloads and fake GitHub clients rather than live repository credentials. The normal Go Tests workflow runs `make test` for non-doc code changes and covers worker-level PR review diff context generation.

Repository security E2E coverage should include initial deterministic slice creation,
incremental scan behavior, invalid v2 evidence being dropped and visible through API,
validation task persistence, successful verified patch proposals, and patch proposals with
missing or mismatched artifacts staying not ready.

### Adversarial aimock evals

The `adversarial-eval` label is part of the `agent-runtime` lane. These evals
script a model acting on an untrusted repository file or brokered tool note and
check the resulting system boundaries. They do **not** measure model judgment
or resistance to prompt injection. The existing fast, in-process evals remain unchanged.

The legitimate prompt asks to read a fixture file or tool note. The malicious
action is scripted only after its nonce returns as a real tool result. Passing
a prevention case requires both the returned refusal and independent evidence
that no forbidden Task, token exchange, workspace change, push, or PR occurred.
Permitted controls exercise the same tools and consume the fixture credentials;
absence of a credential that was never used is not containment evidence.
Public broker/CLI refusals intentionally redact private executor errors. Each
delegation denial is paired with a permitted same-tool control and independent
Task, token-exchange, and access receipts; generic failure alone is not proof of
a specific authorization boundary.

A test-only proxy in front of aimock retains complete request bodies, including
bodies above aimock's own 64 KiB journal cap. It stores no headers or response
bodies and never logs captured content. Capture is bounded to 4 MiB per request,
64 MiB in total, and 4096 entries; exhaustion rejects requests rather than
silently discarding evidence. Snapshot exports stream one entry at a time with
a single concurrent exporter, rather than buffering a potentially expanded
whole-store JSON response. Eval helpers require a stable capture instance,
retained request identities, completed responses, and matching fixture receipts.
Missing or incomplete observation cannot pass. Auxiliary requests are scanned
as well as the case's marked inference requests.

All credentials are synthetic and local to the test cluster. The provider
canary covers Orka's upstream proxy bearer, not vendor credentials held by a
real Vekil deployment. Child-local loopback capabilities are distinct from that
bearer. Transaction tests use cryptographically valid, ephemeral mock authority
and inspect actual child-token exchange results; malformed tokens are not a
scope-containment control. Canary detector tests deliberately plant exposure
and verify that failure messages omit the values.

Built-in ACP Tasks reject transaction-token delegation before inference. The
shared TxToken cases therefore use the conformed external harness-v2 E2E
fixture, which runs a bounded aimock loop against the real controller's public
MCP broker using accepted running-Task fences. An exact fixture-only ingress
grant connects it to isolated aimock; the production proxy-only ingress policy
is unchanged. The injected note is a real brokered tool result, not native
repository content. Parent and permitted child tokens must be valid, and the
child consumes its token in a brokered control.
The fixture keeps these Tasks running until normal cancellation cleanup; a
completed model loop is not presented as native Task success. Publication
checks use real Claude Tasks without a transaction context. Its flat MCP
protocol fits the pinned mock; this case does not cover Codex namespaced MCP
calls. No test claims that built-in runtimes support TxTokens or that external
fixtures exercise native CLI workspace isolation.

Run the focused label through the usual isolated E2E bootstrap:

```bash
E2E_GINKGO_LABEL_FILTER=adversarial-eval E2E_ACP_IDLE_POOL_TTL=15m make test-e2e
```

Every runtime must satisfy native refusal plus `ReadValidated` and no
publication. Claude, Copilot, and OpenCode refuse the write tool itself. Codex
reads and writes through one shell tool, so its refusal comes from the
filesystem: under read intent the supervisor owns the workspace and the
session root, and the Codex shell's write fails with `Permission denied`.

The capture, signing-fixture, detector, and fixture-contract tests also run
without a cluster:

```bash
make ensure-ui-embed
go test ./test/fixtures/aimockcapture/... ./test/fixtures/aimockauthority/...
go test -tags=e2e ./test/e2e -run 'Test(AimockEval|RuntimeEval|AuthorizationEval)'
```

### E2E key requirements

- `scripts/agent-runtime-e2e.sh --context <context>` is the canonical ACP
  deployed-cluster validator. Its default mode is a smoke test of runtime
  behavior against real models. `RELEASE_GATE=1` checks the shipped artifacts
  instead: one workspace read with its API result per runtime, unsafe workspace
  rejection, and image digests. The Kind wrapper also requires uncached
  publication tests. A smoke result cannot qualify a release. Live GitHub publication is
  available only through an explicit local opt-in.
- `scripts/agent-runtime-kind-e2e.sh` is the CI/local bootstrap entrypoint. It
  creates an ephemeral Kind cluster, deploys Vekil and the production ACP
  topology with digest-pinned local images, and then calls the canonical script.
- `.github/workflows/agent-runtime-e2e.yml` runs by manual dispatch only. It
  requires the repository's
  `COPILOT_GITHUB_TOKEN` secret so Vekil can exercise Codex, OpenCode, Claude, and
  Copilot against real model providers without mounting provider credentials
  into RuntimePools. The workflow rejects dispatches from other branches before
  using the secret. It runs as ordinary CI without a deployment environment.
  When migrating from `live-acp-runtime-smoke`, ensure the provider secret is
  available at repository scope. After the renamed workflow succeeds, retire
  the old environment and preserve its deployment history.
- `.github/workflows/release-qualification.yml` uses `workflow_dispatch` and is
  serialized. The release workflow dispatches it automatically. Restrict the
  `release-qualification` environment to `main` and exact permitted release
  branches, require a trusted reviewer, and disable administrator bypass. It
  accepts a full source SHA that must equal both the dispatched workflow commit
  and selected branch head. GitHub
  observations use the job's temporary `GITHUB_TOKEN` with `contents: read`.
  Agent execution runs against aimock (`ACP_E2E_MODEL_BACKEND=aimock`), so no
  model-provider secret, stored Git publication token, or fork setting is
  needed. Publication tests use real temporary Git repositories and local
  GitHub API test servers. They do not prove the complete cluster-to-GitHub
  publication path or live organization permissions.
  [Release qualification](release-qualification.md) documents trusted dispatch,
  required test evidence, report verification, and cleanup. Qualification needs
  the report for the exact candidate; smoke success is insufficient.
- Neither `Agent Runtime E2E` nor `Release Qualification` runs for `pull_request`, so PR-controlled code never
  receives provider credentials. Both check out with persisted
  credentials disabled and expose secrets only to the final local script step.
  The release gate adds `actions: read` to `contents: read` to verify the
  environment branch rules and download candidate artifacts using the native
  token. Neither workflow requests `id-token: write`; GitHub OIDC validation remains isolated in
  `live-github-oidc-e2e.yml`.
- The release gate additionally requires digest-pinned controller, Publisher,
  Codex, OpenCode, Claude, and Copilot images; a Ready central provider proxy; the
  `config/acp-production` Vekil ingress boundary; durable controller/Publisher
  PVCs; a distinct publication fork; and authenticated `gh` access for
  independent remote and PR verification/cleanup.
- The validator runs Codex, then OpenCode, Claude, and Copilot, deleting each
  provider's pools before starting the next. Every provider phase verifies exact
  Pod UID, image ID, runtime instance/profile, and RuntimeSession identity. Smoke
  mode also checks continuation, OpenCode's read-intent tool policy, and, for
  Codex, concurrency, cancellation and timeout settlement, controller restart,
  and pool replacement. Release mode runs only the reads and, with the local
  opt-in, the live publication canary.
- `E2E_OPENAI_API_KEY` and `E2E_ANTHROPIC_API_KEY` remain inputs for older native
  `type: ai` test cases. They are not mounted into built-in ACP RuntimePools.
  When either key is unset, the suite deploys [aimock](https://github.com/CopilotKit/aimock)
  in the test namespace and backs that provider with scripted fixtures from
  `test/e2e/testdata/aimock`, so those specs run on every PR without credentials.
  Set `E2E_DISABLE_MOCK_LLM=1` to skip them instead. Fixture conventions are in
  `test/e2e/testdata/aimock/README.md`. The Anthropic-compatible API,
  coordination memory tool, and agent runtime matrix specs also run on aimock
  by default; with `E2E_LIVE_COPILOT_PROXY_BASE_URL` set they use the live
  proxy instead.
- The suite enables Task provenance trust after verifying the `orka-admission`
  provenance webhook that `make deploy` installs, so coordination messaging and
  `cancel_task` run in the default lane.
- The `Agent runtime lifecycle` job in `test-e2e.yml` runs the `agent-runtime`
  label on its own Kind cluster, and the default job excludes it. Those specs
  restart the controller and park, replace, and scale RuntimePools, and they
  need the production idle pool TTL, so the job sets `E2E_ACP_IDLE_POOL_TTL=15m`
  (the default lane uses `5s`). Run them locally with
  `make test-e2e E2E_GINKGO_LABEL_FILTER=agent-runtime E2E_ACP_IDLE_POOL_TTL=15m`.
- The default lane also runs one clean-room publication end to end without
  github.com: a scripted Codex RuntimePool talks to a second aimock deployed as
  `vekil-system/vekil` behind the production provider proxy, and the publisher
  clones from, pushes to, and opens a pull request on an in-cluster fake forge
  (`test/fixtures/fakeforge`: real Git smart HTTP plus the GitHub REST endpoints
  the publisher calls).
- `COPILOT_GITHUB_TOKEN` also remains the credential for
  `live-copilot-proxy-e2e.yml`, which covers the external proxy as native
  Provider test infrastructure. `Agent Runtime E2E` provides the
  provider-execution evidence for the built-in RuntimePool profiles.
- Structural e2e tests for native worker Jobs run without external model keys.
- `Live Agent Sandbox E2E` and `Agent Substrate E2E` do run workspace-backed ACP Tasks
  end to end against a local model fixture, but they are not the full release gate:
  external-provider execution stays with `Agent Runtime E2E`, and release acceptance
  with `Release Qualification`.
  Every Substrate run includes DataOnly suspension, cold continuation, and
  checkpoint file recovery against the unmodified upstream provider.
- Security Scan E2E is secret-free and model-free, but requires Docker plus the
  local Go, Kind, kubectl, curl, and jq toolchain.

- Gateway Live E2E is model-free and secret-free. Its focused invocation sets `E2E_GATEWAY=true` and `E2E_EPHEMERAL_CLUSTER=true`; the last flag skips per-resource suite cleanup because the caller deletes the entire Kind cluster.
- GitHub Actions `id-token: write` permission: required by the live GitHub OIDC workflow. For local/manual runs of `scripts/live-github-oidc-e2e.sh`, set `ORKA_GITHUB_OIDC_TOKEN` to a valid JWT instead. Provider-specific transaction-token E2E lives in the external integration repositories.
- `E2E_LIVE_COPILOT_PROXY_BASE_URL` (or `E2E_COPILOT_PROXY_BASE_URL` / `COPILOT_PROXY_BASE_URL`): enables the focused live copilot-proxy spec against a running proxy
- `E2E_LIVE_COPILOT_PROXY_SERVICE_NAMESPACE`, `E2E_LIVE_COPILOT_PROXY_SERVICE_NAME`, `E2E_LIVE_COPILOT_PROXY_SERVICE_PORT`: direct-access proxy coordinates used by the legacy Provider, Chat, and Anthropic compatibility specs.
- `E2E_LIVE_ACP_PROVIDER_PROXY_SERVICE_NAMESPACE`, `E2E_LIVE_ACP_PROVIDER_PROXY_SERVICE_NAME`, `E2E_LIVE_ACP_PROVIDER_PROXY_SERVICE_PORT`: model-discovery coordinates for the ACP RuntimePool matrix. The full live script pins these to `vekil-system`, `vekil`, and `1337` so built-in RuntimePools traverse the production provider-proxy DNS and NetworkPolicy boundary.


Live provider/chat tests prefer `gpt-5-mini` and `claude-haiku-4.5` when available.
The runtime smoke and release qualification scripts default to `gpt-5.4-mini`
for Codex and OpenCode, `claude-haiku-4.5` for Claude, and `gpt-5.3-codex` for
Copilot. Override these with `ACP_E2E_CODEX_MODEL`, `ACP_E2E_OPENCODE_MODEL`,
`ACP_E2E_CLAUDE_MODEL`, or `ACP_E2E_COPILOT_MODEL`. Configured models must pass
the Vekil catalog and streaming probes.

Run the trusted smoke bootstrap locally with the token exported in the shell
rather than placed on a command line:

```bash
read -rsp 'Copilot provider token: ' COPILOT_GITHUB_TOKEN && echo
export COPILOT_GITHUB_TOKEN
export ACP_E2E_OPENCODE_MODEL=openai/gpt-5.4-mini
export ACP_E2E_OPENCODE_CONTEXT_WINDOW=32768
export ACP_E2E_OPENCODE_MAX_TOKENS=4096
ACP_E2E_KIND_TAG=local bash scripts/agent-runtime-kind-e2e.sh
```

For a local qualification diagnostic, use read-only GitHub CLI access and the
same model-provider configuration as the smoke run. GitHub's automatic job
token is available only inside Actions. To use that token without configuring
local GitHub access, dispatch
[Release Qualification](release-qualification.md#standalone-qualification).

The local wrapper runs the required Git and PR fixture tests before creating
the cluster:

```bash
export RELEASE_GATE=1
export ACP_E2E_WRITE_CREATE_PR=0
export ACP_E2E_REPO=https://github.com/orka-agents/orka.git
export ACP_E2E_REF="$(git rev-parse HEAD)"
export ACP_E2E_BASE_BRANCH=main
bash scripts/agent-runtime-kind-e2e.sh
```

The fixture tests, image build, and runtime phases use the same candidate.
The wrapper requires a clean checkout at that commit, and the candidate must
equal the selected branch head. Local reports are saved under
`bin/acp-release-*/acceptance.json`, or
`ACP_E2E_REPORT_FILE` when set. `--keep-cluster` leaves cleanup pending and cannot
qualify a release; use the trusted workflow and report verifier above for release
records.

The existing local live GitHub diagnostic remains available with
`ACP_E2E_WRITE_CREATE_PR=1` and separately authorized canary credentials. Its
four-role and distinct-fork requirements are listed in the validator's `--help`.
The hosted release workflow cannot enable it and explicitly reports live GitHub
publication as `not_tested`.

Do not enable shell xtrace for either invocation. The scripts create Kubernetes
Secrets without printing their values and redact provider/GitHub token patterns
from failure diagnostics.


The live agent-sandbox workflow validates both the direct workspace-adapter
lifecycle and the initial workspace-backed Orka harness v2 happy path. It builds the
real Codex supervisor, routes a prompt through a local Responses-compatible
fixture, waits for the Task to succeed, verifies provider-neutral status, and
cleans up the dedicated RuntimePool. It does not replace release qualification
or provide publication evidence. Its direct-adapter assertions
also include:

- the adapter creates a v1beta1 `SandboxClaim` with the expected `warmPoolRef` and executes a command with caller-supplied env inside the sandbox
- `cleanupPolicy: delete` removes the generated `SandboxClaim`
- `cleanupPolicy: retain` plus `reusePolicy: session` reattaches to the deterministic session claim
- retained workspace state persists across tasks

The RepositoryMonitor validation bundle covers the canonical signed `orka:implement` entrypoint, durable intake, pause controls, and replay against GitHub fixtures.

The live GitHub OIDC workflow (`.github/workflows/live-github-oidc-e2e.yml`) runs `scripts/live-github-oidc-e2e.sh` in GitHub Actions with `id-token: write`. It builds the controller from the PR, deploys it to a fresh Kind cluster, configures the GitHub OIDC issuer and workflow audience, fetches a real Actions OIDC token, and validates:

- unauthenticated API requests return `401`
- OIDC-authenticated Task creation returns `201`
- the created Task contains verified `spec.requestedBy` provenance
- top-level `requestedBy` and nested `spec.requestedBy` client tampering are rejected with `400`
- the OIDC token does not appear in controller logs

The Agent Substrate workflow runs the official pin in
`hack/agent-substrate/upstream.env` without provider source patches. Every run
includes direct sealed execution and files, MCP, ACP, controller restart,
DataOnly suspension, cold continuation, checkpoint export and restore,
cancellation, timeout, and cleanup. Native unit tests cover TLS and credential
rotation, lost responses, source identity changes, reference races, and explicit
recovery. Tests do not supply fork-only lifecycle preconditions.

The ACP file scenario uses the real Codex runtime with a deterministic Responses
fixture. The file Tasks have read-only intent, so each one runs a shell probe
that must find no canary file and be denied creating one. The scenario probes,
exports a checkpoint, probes again in the same session, deletes the source
workspace, then restores the checkpoint and probes the restored workspace. The
fixture requires successful probe output from the current turn, and the suite
requires successful execution and `ReadValidated` delivery. Because read-intent
Tasks cannot write, this scenario does not compare file bytes across a
checkpoint. The workflow also
runs a Linux regression as root to verify durable file ownership after session
deletion, drain, and supervisor shutdown.

The lifetime case starts a real shell command with a 300-second hold in a
workspace with a 120-second `maxLifetime`. It verifies that expiry cancels the
original prompt and removes its worker before the command can finish. The Task
has a longer timeout, and the test does not cancel or delete it to force expiry.
Session, workspace, pool, and saved-data cleanup must finish afterward.

```bash
bash scripts/tests/agent-substrate-e2e-hardening-test.sh
KEEP_CLUSTER=1 bash scripts/agent-substrate-e2e.sh
```

Use a new dedicated `KIND_CLUSTER` or explicitly select
`SUBSTRATE_REUSE_CLUSTER=1`. The installer never recreates an existing cluster.
The provider owns its gVisor kind setup; the kubeconfig stays under the run's
`bin/` directory. Live conformance requires a working Docker engine.

### Frontend tests

Frontend tests use **Vitest + Testing Library + MSW**.

```bash
cd ui && bun run test           # what CI runs
cd ui && bun run test:coverage  # adds the coverage report and threshold check
```

:::warning[The UI coverage thresholds are a local target, not a merge gate]
`ui/vite.config.ts` declares thresholds of 95% statements, 80% branches, 90% functions, and
95% lines. Those only apply to `bun run test:coverage`. The `ui-test` job in
`.github/workflows/test.yml` runs plain `bun run test`, so a pull request that drops coverage
still passes CI. Run the coverage command yourself before assuming a number.
:::

## Testing patterns

### Table-driven tests

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

### Fake Kubernetes client

```go
scheme := runtime.NewScheme()
corev1alpha1.AddToScheme(scheme)
corev1.AddToScheme(scheme)
client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
```

### HTTP mocking

```go
server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
    w.WriteHeader(http.StatusOK)
    w.Write([]byte(`{"result": "ok"}`))
}))
defer server.Close()
```

### Fiber test app

```go
app := fiber.New()
app.Get("/test", handler)
req := httptest.NewRequest(http.MethodGet, "/test", nil)
resp, _ := app.Test(req)
```

### Frontend test mocking

```typescript
// Mock zustand persist middleware
vi.mock('zustand/middleware', () => ({ persist: (fn: unknown) => fn }))

// Use test utils with QueryClient wrapper
import { render } from '@/test/test-utils'
```

## Testing with chat

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
