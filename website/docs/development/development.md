---
slug: /development
---

# Development

## Prerequisites

- Go 1.25.3+
- Bun (for UI build)
- Docker 17.03+
- kubectl (version compatible with your cluster)
- Access to a Kubernetes cluster

## Build Commands

```bash
# Generate Go types, the installer manifest, and the Helm staging chart
make generate
make manifests

# Build (includes UI)
make build

# Build CLI only
make build-cli

# Run locally
make run
```

## Helm Chart Generation and Releases

Orka follows Gatekeeper's staged chart flow. The editable Helm generator and static chart inputs live under `cmd/build/helmify/`; canonical Kubernetes resources live under `config/`. Generated and promoted outputs are committed so pull requests and release preparation review the exact manifests that will ship.

| Path | Purpose | Edit directly? |
| --- | --- | --- |
| `cmd/build/helmify/` | Helm generator, Kustomize input, and static chart files | Yes |
| `manifest_staging/deploy/orka.yaml` | Generated next-release installer manifest | No |
| `manifest_staging/charts/orka/` | Generated next-release Helm chart used by CI and upgrade tests | No |
| `deploy/` and `charts/orka/` | Promoted release snapshots | No |

For a normal manifest or chart contribution:

1. Edit `config/` and/or the generator inputs under `cmd/build/helmify/`.
2. Run `make manifests`.
3. Review and commit the source changes together with all changes under `manifest_staging/`.
4. Do not promote the chart in an ordinary feature PR. The root snapshots may intentionally remain at the current release while staging contains the next release.

`make manifests` rebuilds staging from scratch, so direct changes in `manifest_staging/` are clobbered. CI reruns generation and requires a clean diff to detect stale output; run `make manifests` and inspect `git diff` for the same drift check locally.

Release preparation runs the same targets as Gatekeeper's flow:

```bash
make release-manifest NEWVERSION=vX.Y.Z[-beta.N|-rc.N]
make promote-staging-manifest
```

The first target updates release inputs and regenerates staging. The second copies the reviewed staging installer and chart into `deploy/` and `charts/orka/`. Normally `.github/workflows/release-pr.yml` runs both and opens the release-preparation PR. A matching `v*` tag packages and publishes those committed root snapshots; tag workflows do not regenerate or promote manifests.

CRDs are generated from the canonical definitions in `config/crd/bases/`. Chart generation makes them available on fresh install, but Helm does not update them during upgrades. Apply the CRDs from the exact target chart before upgrading the controller, as documented in `charts/orka/README.md`.

## Testing

```bash
# Run test pipeline (manifests, generate, fmt, vet, then Go tests)
make test

# Lint
make lint
make lint-fix

# E2E tests (uses isolated Kind cluster)
make test-e2e
```

See [Testing](testing.md) for full test structure and patterns.

### CI Validation

The repository has additional GitHub Actions workflows in addition to the normal test matrix:

- `Live Copilot Proxy E2E` — exercises live model-backed Orka paths through the copilot-proxy harness.
- `Live Agent Sandbox E2E` — installs the pinned upstream `agent-sandbox` release in Kind, builds the PR controller plus fake Claude/sandbox-runtime and upstream router images, then validates workspace claim, sandbox execution, delete cleanup, retained-session reuse, and token scrubbing without model access.
- `Live GitHub Label Trigger E2E` — builds the PR controller image, deploys it to Kind, configures a generated webhook secret and synthetic runtime Agent, then verifies signed label webhooks create scoped agent Tasks while invalid signatures and duplicate deliveries are handled correctly. This workflow is manual, model-free, and secret-free.
- `Live GitHub OIDC E2E` — builds the PR controller image, deploys it to Kind, authenticates to Orka with a real GitHub Actions OIDC token, then generates a real `kontxt` TxToken against an in-cluster JWKS endpoint. It verifies `spec.requestedBy` stamping for both auth modes, rejects client tampering, and rejects a tampered TxToken.
- `Repository Monitor Smoke` — runs automatically on PRs and pushes touching monitor-relevant Go, CRD/config, worker, or dependency paths. It creates the UI embed stub and runs focused Go tests for monitor store/API/controller behavior, GitHub pull request event queueing, targeted single-PR inventory runs, read-only review task job construction, stdout result forwarding, `create_pr_monitor` repository URL and credential validation, GitHub tool `repo_url` scope enforcement, and PR review marker tooling.
- `Agent Substrate E2E` — installs Agent Substrate and Orka into a fresh Kind cluster, creates Orka-compatible `WorkerPool`/`ActorTemplate` resources, validates direct Substrate actor execution, runs default and pooled Orka Tasks through the Substrate workspace provider, exercises pooled MCP actor-backed Tools, and checks workspace placement/density telemetry. This workflow is secret-free.

Validate workflow/script edits locally before pushing:

```bash
bash -n scripts/live-copilot-proxy-e2e.sh
bash -n scripts/live-agent-sandbox-e2e.sh
bash -n scripts/live-github-label-trigger-e2e.sh
bash -n scripts/live-github-oidc-e2e.sh
bash -n scripts/agent-substrate-e2e.sh
go run github.com/rhysd/actionlint/cmd/actionlint@latest .github/workflows/live-copilot-proxy-e2e.yml
go run github.com/rhysd/actionlint/cmd/actionlint@latest .github/workflows/live-agent-sandbox-e2e.yml
go run github.com/rhysd/actionlint/cmd/actionlint@latest .github/workflows/live-github-label-trigger-e2e.yml
go run github.com/rhysd/actionlint/cmd/actionlint@latest .github/workflows/live-github-oidc-e2e.yml
go run github.com/rhysd/actionlint/cmd/actionlint@latest .github/workflows/repository-monitor-smoke.yml
go run github.com/rhysd/actionlint/cmd/actionlint@latest .github/workflows/agent-substrate-e2e.yml
```

The agent sandbox live script does not require provider credentials or model access. It uses a deterministic fake `claude` CLI in the sandbox runtime image so CI can verify Orka's sandbox plumbing independently from external LLM availability.

The GitHub OIDC live script requires GitHub Actions `id-token: write` or a manual `ORKA_GITHUB_OIDC_TOKEN`; without either, it fails fast before creating a cluster. The `kontxt` portion is self-contained: it generates an ephemeral RSA key/JWKS and TxToken during the run, so no external kontxt service or secret is required.

Run the Agent Substrate E2E locally with:

```bash
PATH="$(go env GOPATH)/bin:$PATH" \
SUBSTRATE_E2E_EXTENDED=1 \
bash scripts/agent-substrate-e2e.sh
```


## OpenTelemetry development

Telemetry is enabled with `--enable-telemetry` (or the legacy alias
`--enable-tracing`) and exported through `OTEL_EXPORTER_OTLP_ENDPOINT`. When the
controller flag is enabled and a worker-reachable OTLP endpoint is configured,
AI worker Jobs receive `ORKA_ENABLE_TELEMETRY=true`, `ORKA_TRACEPARENT`, and the
non-secret standard OTLP environment. Agent-runtime and harness-wrapper
telemetry is explicit opt-in on those workloads; OTLP endpoint variables alone
do not enable Kubernetes worker telemetry. Delegated child Tasks continue the
active parent trace through Task annotations.

GenAI semantic-convention constants live in `internal/tracing/genai` rather than
upstream `semconv` because the GenAI conventions are still Development-stage.
Run focused telemetry tests with:

```bash
go test ./internal/tracing/... ./internal/llm/ ./internal/tools/ ./internal/worker ./workers/ai ./workers/harness/cliwrapper -run 'Tracing|Telemetry|GenAI|ExecuteTool|TraceContext|Traceparent|TaskRun|DelegateTrace' -v
```

The live Kind e2e coverage for collector export lives in
`test/e2e/otel_genai_test.go`. It patches the controller with
`--enable-telemetry`, points it at an in-cluster OpenTelemetry Collector, and
asserts that AI worker Jobs export GenAI model/tool spans and metrics.

Run disabled-telemetry hot-path benchmarks with:

```bash
go test ./internal/llm ./internal/tools ./internal/worker -run '^$' -bench 'Telemetry|Tracing|ExecuteTool|ToolExecutor' -benchmem
```

## UI Development

```bash
make ui-install         # Install UI dependencies (bun)
make ui-dev             # Run UI dev server
make ui-build           # Build UI and copy to embed directory
make ui-lint            # Lint UI code
make ui-test            # Run UI unit tests
make ui-test-coverage   # Run UI tests with coverage
```

## Docker Images

```bash
# Build images
make docker-build                  # Controller image
make docker-build-ai-worker        # AI worker
make docker-build-general-worker   # General worker
make docker-build-harness-wrapper  # Agent CLI harness wrapper (codex/claude/copilot/opencode)
make docker-build-all              # Controller, workers, and harness wrapper

# Push images
make docker-push
make docker-push-ai-worker
make docker-push-general-worker
make docker-push-harness-wrapper
make docker-push-all
```

## Local Development with Kind

```bash
kind create cluster
make docker-build docker-push IMG=<registry>/orka:tag
make docker-build-harness-wrapper docker-push-harness-wrapper HARNESS_WRAPPER_IMG=<registry>/agent-harness-wrapper:tag
make deploy IMG=<registry>/orka:tag HARNESS_WRAPPER_IMG=<registry>/agent-harness-wrapper:tag
```

### Demo Cluster + Recordings

For interactive presentations and asciinema recordings of `hack/demos/`,
a one-shot bootstrap is available:

```bash
make demo-cluster-up      # kind cluster + Orka + kontxt + agent-sandbox
make demo-images          # build + load the kontxt-caller image (Demo 50)
hack/demos/00-preflight.sh
# ... run ./hack/demos/10-chat-pr.sh, 20-..., etc.
make demo-cluster-down
```

The scripts pace themselves via `DEMO_RECORD_PROFILE=presenter|docs|social|hero`
and pick a short or long request body via
`DEMO_REQUEST_PRESET=quiet-flag|readme-fix|vekil-metrics`. See
`hack/demos/RECORDING.md` for the full design.

## Generate Installer YAML

```bash
make build-installer IMG=ghcr.io/orka-agents/orka:latest
```

## Build Gotchas

### UI Embedding

`make build` embeds the React UI into the controller binary via `//go:embed`. The UI must be built first:

```bash
make ui-build    # Build UI and copy to internal/uiembed/dist/
make build       # Now the Go build will succeed
```

If the UI isn't built, the `ensure-ui-embed` Makefile target creates a stub `internal/uiembed/dist/index.html` so the Go build doesn't fail — but the embedded UI won't work.

### CLI Version Injection

`make build-cli` injects Git version info via `-ldflags`:

```bash
make build-cli   # Produces bin/orka with embedded version
```

### Metrics Disabled by Default

The controller's `--metrics-bind-address` defaults to `0` (disabled). Set it explicitly to enable Prometheus metrics:

```
--metrics-bind-address=:8443
```

### HTTP/2 Disabled by Default

HTTP/2 is disabled for metrics and webhook servers due to CVEs ([GHSA-qppj-fm5r-hxr3](https://github.com/advisories/GHSA-qppj-fm5r-hxr3), [GHSA-4374-p667-p6c8](https://github.com/advisories/GHSA-4374-p667-p6c8)). Use `--enable-http2=true` only if needed.

### Leader Election

Leader election ID is hardcoded as `03b49a10.orka.ai`. Multiple controller deployments in the same cluster will coordinate via this ID.
