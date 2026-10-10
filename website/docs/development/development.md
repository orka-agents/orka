---
slug: /development
description: "Building, running, and regenerating Orka locally."
---

# Development

## Prerequisites

| Tool | Version | Notes |
| --- | --- | --- |
| Go | 1.26.2 or newer | `go.mod` keeps `go 1.26.2` and prefers `toolchain go1.27.2`. With automatic toolchain selection, Go downloads 1.27.2 when needed. CI builds on 1.27. |
| Bun | current | Builds the React dashboard, which is embedded into the controller binary. Bun 1.4.2 has a [known Windows/WSL setup issue](#windows-bsod-during-ui-dependency-installation); use 1.3.13 there. |
| Docker | with BuildKit | The Dockerfiles use BuildKit syntax. Docker Desktop and any modern Docker Engine have it on by default. |
| kubectl | matching your cluster | |
| A Kubernetes cluster | | [kind](https://kind.sigs.k8s.io/) is fine for development. |

## Build commands

For the local run, replace `/path/outside-the-repository` with a private, writable
directory for the persistent database and snapshot key. `RUN_STORE_PATH` overrides
the controller's `/data/orka.db` default.

```bash
# Generate Go types, the installer manifest, and the Helm staging chart
make generate
make manifests

# Build (includes UI)
make build

# Build CLI only
make build-cli

# Run locally with one persistent AES-256 snapshot key
openssl rand 32 > /path/outside-the-repository/orka-snapshot-key
chmod 600 /path/outside-the-repository/orka-snapshot-key
make run RUN_STORE_PATH=/path/outside-the-repository/orka.db \
  RUN_AGENT_EXECUTION_SNAPSHOT_KEY_FILE=/path/outside-the-repository/orka-snapshot-key
```

## Helm chart generation and releases

Orka uses a staged chart flow. The editable Helm generator and static chart inputs live under `cmd/build/helmify/`; canonical Kubernetes resources live under `config/`. Generated and promoted outputs are committed so pull requests and release preparation review the exact manifests that will ship.

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

Release preparation runs the staged release targets:

```bash
make release-manifest NEWVERSION=vX.Y.Z[-beta.N|-rc.N]
make promote-staging-manifest
```

The Go command in `cmd/build/release/` handles version updates, candidate
verification, qualification dispatch, and publication. Workflows invoke it with
`go run ./cmd/build/release`; `make release-manifest` uses its `update-version`
subcommand before generating staging manifests.

The first target updates release inputs and regenerates staging. The second
copies staging into `deploy/` and `charts/orka/`. **Prepare Release** in
`.github/workflows/release-prepare.yml` runs both on `release-X.Y`, commits the
candidate, and dispatches its checks and publication workflow using
`GITHUB_TOKEN`. It does not create a release-preparation PR or push to `main`.

The workflow builds the release images, then waits for approval in
`release-qualification` before testing the exact packaged
chart and image digests. After qualification, a separate `release` environment
approval permits tagging and publication of the qualified artifacts. See
[release automation and qualification](release-qualification.md)
for environment setup, dispatch, evidence, and retries. Ordinary nightly smoke
and component tests do not satisfy the publication gate.

Core CRDs are generated into `config/crd/bases/`; `config/crd/kustomization.yaml`
selects the APIs packaged in the installer and chart. Shared workspace contracts
and provider-owned APIs are maintained in the
[orka-workspace repository](https://github.com/orka-agents/orka-workspace).
Fake, Sandbox, and Substrate controllers are independently built and deployed.
Helm does not update CRDs during upgrades. Drain legacy in-tree allocations before
applying the pruned RuntimePool schema; see [Upgrading](../operations/upgrading.md#external-workspace-migration).

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

### CI validation

The repository has additional GitHub Actions workflows in addition to the normal test matrix:

- `Agent Runtime E2E` runs on trusted default-branch changes, nightly, or by manual dispatch. It builds the current controller and all four built-in runtime images, bootstraps Kind plus Vekil and the production ACP topology, and executes Codex, OpenCode, Claude, and Copilot RuntimePools against real model providers. It uses the repository's `COPILOT_GITHUB_TOKEN` secret and runs as ordinary CI without a deployment environment.
- `Release Qualification` verifies the candidate chart, recovery, agent execution, Git publication and GitHub API fixture tests, and cleanup. The release workflow dispatches it automatically; environment approval permits model-provider access. GitHub observations use the job token, with no stored Git publication credentials.
- `Live Copilot Proxy E2E` — exercises native `type: ai` and compatibility API paths through an external proxy used as test infrastructure. `Agent Runtime E2E` separately executes the built-in Codex, OpenCode, Claude, and Copilot RuntimePools end to end.
- `Live GitHub OIDC E2E` — builds the PR controller image, deploys it to Kind, authenticates to Orka with a real GitHub Actions OIDC token, and verifies `spec.requestedBy` stamping plus client provenance-tampering rejection.
- `Gateway Live E2E` — runs on relevant pushes and pull requests or by manual dispatch. It creates a fresh Kind cluster, generates disposable TLS and bearer credentials, deploys the TLS reference adapter and deterministic echo `AgentRuntime`, and verifies invalid authentication, accepted and duplicate ingress, runtime-backed Task completion, final delivery, idempotency, and correlation metadata. It is model-free and secret-free and does not use repository or provider credentials.
- `Repository Monitor Smoke` — runs automatically on PRs and pushes touching monitor-relevant Go, CRD/config, worker, or dependency paths. It creates the UI embed stub and runs focused Go tests for monitor store/API/controller behavior, GitHub pull request event queueing, targeted single-PR inventory runs, read-only review task job construction, stdout result forwarding, `create_pr_monitor` repository URL and credential validation, GitHub tool `repo_url` scope enforcement, and PR review marker tooling.
- `Live Connectors E2E` — builds the PR controller, the native AI worker, and the connectors fixture (an OIDC issuer, a fake OAuth provider with short-lived tokens, a resource API, and a scripted model) into Kind, then proves the linked-account lifecycle end to end: a person signs in through the OIDC fixture, links the fake provider (`POST /api/v1/connections`, the consent redirect, and the fragment-carried completion), a native `type: ai` Task created by that person reads through the linked token, its write parks for approval and runs once approved, the second read is served by a refreshed token because the first one expired meanwhile, another person sees no link, and disconnecting revokes the tokens and removes the Connection. It runs the controller with the fixture-only `--connectors-allow-private-endpoints` allowance (a literal acknowledgement value that the controller accepts only together with a plain-http localhost callback base) so the provider may live in the cluster. It is model-free and secret-free.

Validate workflow/script edits locally before pushing:

```bash
bash -n scripts/live-copilot-proxy-e2e.sh
bash -n scripts/agent-runtime-e2e.sh scripts/agent-runtime-kind-e2e.sh scripts/lib/agent-runtime-kind-bootstrap.sh
bash -n scripts/live-github-oidc-e2e.sh
bash -n scripts/live-connectors-e2e.sh
go run github.com/rhysd/actionlint/cmd/actionlint@latest .github/workflows/live-copilot-proxy-e2e.yml
go run github.com/rhysd/actionlint/cmd/actionlint@latest .github/workflows/agent-runtime-e2e.yml
go run github.com/rhysd/actionlint/cmd/actionlint@latest .github/workflows/release-qualification.yml
go run github.com/rhysd/actionlint/cmd/actionlint@latest .github/workflows/live-github-oidc-e2e.yml
go run github.com/rhysd/actionlint/cmd/actionlint@latest .github/workflows/gateway-e2e.yml
go run github.com/rhysd/actionlint/cmd/actionlint@latest .github/workflows/repository-monitor-smoke.yml
go run github.com/rhysd/actionlint/cmd/actionlint@latest .github/workflows/live-connectors-e2e.yml
```

### External workspace local proof

Use both source repositories: [Orka core](https://github.com/orka-agents/orka)
provides the real controller and ACP supervisor; [orka-workspace](https://github.com/orka-agents/orka-workspace)
provides shared contracts, independently deployed providers, and local proof harnesses.
Run cluster commands from the shared repository with its `kindctl` tag; the
scripts preserve the cluster and scoped kubeconfig.

```bash
# Run from the orka-workspace checkout.
scripts/external-workspace-e2e.sh provider
ORKA_CORE_BUILD_RELEASED=1 ORKA_CORE_SOURCE=/path/to/orka-checkout \
  scripts/external-workspace-e2e.sh core
```

The provider lane deploys two fake replicas and checks leader election, ordinary
and foreign-writer rejection, field ownership, exact public Pod identity, and
retirement. The core lane runs an actual authenticated RuntimeSession and Task
using the production supervisor and a deterministic model-free ACP agent. It
requires success, exact process/Pod fences, credential revocation, and physical
termination. This verifies the generic external handoff rather than native backend
conformance or publication.

The separate `scripts/external-sandbox-e2e.sh` and
`scripts/external-substrate-e2e.sh` in the shared repository exercise the installed
native backends. They prove durable data and exact lifetime cleanup; their
standalone lanes do not claim real core credential bootstrap or Task execution.
Default kind CNI checks policy objects without proving packet enforcement.
See the [shared proof instructions](https://github.com/orka-agents/orka-workspace/blob/main/hack/external-workspace-e2e/README.md)
for prerequisites, frozen source/image evidence, and private artifact handling.

The additional `scripts/external-substrate-core-e2e.sh proof` runs an actual Task
through deployed Core and native provider controllers. It verifies authenticated
Serving on port 80, RuntimeSession execution, persisted result, and exact native,
Core pool, and credential retirement. Follow the
[native Core proof instructions](https://github.com/orka-agents/orka-workspace/blob/v0.1.0-alpha.2/hack/external-substrate-e2e/README.md#actual-core-and-deployed-provider-task-proof)
after installing its dedicated backend cluster.

`scripts/external-workspace-upgrade-e2e.sh` uses the same released core image to
prove stock startup rejects retained legacy objects, even after old backend
settings have been pruned by the new CRD. It creates isolated synthetic records,
checks all legacy states, verifies no adoption or credential/compute creation,
and removes only exact synthetic UIDs after passing. It preserves evidence and
resources on failure. Run it only after the final core build has been released;
the shared README documents its required image, frozen source, and old-schema inputs.

The GitHub OIDC live script requires GitHub Actions `id-token: write` or a manual
`ORKA_GITHUB_OIDC_TOKEN`; without either, it fails before creating a cluster.
Transaction-token provider E2E lives in the external integration repository.

## Harness wrapper real-world validation

When changing ACP runtime supervision or broker boundaries, validate them against a live cluster, not only unit tests. Use `scripts/agent-runtime-e2e.sh` for an already deployed cluster, or `scripts/agent-runtime-kind-e2e.sh` to create the same ephemeral Kind/Vekil topology used by CI.

## OpenTelemetry development

Telemetry is enabled with `--enable-telemetry` (or the legacy alias
`--enable-tracing`) and exported through `OTEL_EXPORTER_OTLP_ENDPOINT`. When the
controller flag is enabled and a worker-reachable OTLP endpoint is configured,
AI worker Jobs receive `ORKA_ENABLE_TELEMETRY=true`, `ORKA_TRACEPARENT`, and the
non-secret standard OTLP environment. ACP attempt, RuntimeSession, and
publication spans run in the controller and use its exporter. Managed
RuntimePool supervisors receive non-secret trace exporter settings and continue
W3C context carried in authenticated v2 request headers. Provider children remain
outside this instrumentation. Collector routing is operator-owned; see the
[observability guide](../guides/observability.md#enable-telemetry) for supported
settings and network prerequisites. Delegated child Tasks continue the active
parent trace through Task annotations.

GenAI semantic-convention constants live in `internal/tracing/genai` rather than
upstream `semconv` because the GenAI conventions are still Development-stage.
Run focused telemetry tests with:

```bash
go test ./internal/tracing/... ./internal/llm/ ./internal/tools/ ./internal/worker ./workers/ai ./internal/harness/v2/... ./internal/acp/... ./workers/acp/... -run 'Tracing|Telemetry|GenAI|ExecuteTool|TraceContext|Traceparent|TaskRun|RuntimeSession|Fence' -v
```

The live Kind e2e coverage for collector export lives in
`test/e2e/otel_genai_test.go`. It patches the controller with
`--enable-telemetry`, points it at an in-cluster OpenTelemetry Collector, and
asserts that AI worker Jobs export GenAI model/tool spans and metrics.

Run disabled-telemetry hot-path benchmarks with:

```bash
go test ./internal/llm ./internal/tools ./internal/worker -run '^$' -bench 'Telemetry|Tracing|ExecuteTool|ToolExecutor' -benchmem
```

## UI development

```bash
make ui-install         # Install UI dependencies (bun)
make ui-dev             # Run UI dev server
make ui-build           # Build UI and copy to embed directory
make ui-lint            # Lint UI code
make ui-test            # Run UI unit tests
make ui-test-coverage   # Run UI tests with coverage
```

## Docker images

```bash
# Build images
make docker-build                       # Controller image
make docker-build-ai-worker             # Native AI worker
make docker-build-general-worker        # General worker
make docker-build-acp-codex-runtime      # Immutable Codex ACP runtime
make docker-build-acp-claude-runtime     # Immutable Claude ACP runtime
make docker-build-acp-copilot-runtime    # Immutable GitHub Copilot ACP runtime
make docker-build-acp-opencode-runtime    # Immutable OpenCode ACP runtime
make docker-build-workspace-publisher    # Clean-room Workspace/Publisher
make docker-build-all

# Push images
make docker-push
make docker-push-ai-worker
make docker-push-general-worker
make docker-push-acp-codex-runtime
make docker-push-acp-claude-runtime
make docker-push-acp-copilot-runtime
make docker-push-acp-opencode-runtime
make docker-push-workspace-publisher
make docker-push-all
```

## Local development with Kind

```bash
kind create cluster
make docker-build-all
# Push/load the images, then use immutable runtime digests for deployment.
make deploy \
  IMG='<repo>@sha256:<controller-digest>' \
  ACP_CODEX_RUNTIME_IMG='<registry>/acp-codex@sha256:<digest>' \
  ACP_CLAUDE_RUNTIME_IMG='<registry>/acp-claude@sha256:<digest>' \
  ACP_COPILOT_RUNTIME_IMG='<registry>/acp-copilot@sha256:<digest>' \
  ACP_OPENCODE_RUNTIME_IMG='<registry>/acp-opencode@sha256:<digest>' \
  WORKSPACE_PUBLISHER_IMG='<repo>@sha256:<publisher-digest>'
```

### Demo cluster + recordings

For interactive presentations and asciinema recordings of `hack/demos/`,
a one-shot bootstrap is available:

```bash
make demo-cluster-up      # kind cluster + Orka + agent-sandbox
make demo-images          # build + load demo runtime images
hack/demos/00-preflight.sh
# ... run ./hack/demos/10-chat-pr.sh, 20-..., etc.
make demo-cluster-down
```

The scripts pace themselves via `DEMO_RECORD_PROFILE=presenter|docs|social|hero`
and pick a short or long request body via
`DEMO_REQUEST_PRESET=quiet-flag|readme-fix|vekil-metrics`. See
`hack/demos/RECORDING.md` for the full design.

## Generate installer YAML

The installer manifest is generated into `manifest_staging/deploy/orka.yaml` by
the staged manifest flow:

```bash
make manifests
```

See [Helm Chart Generation and Releases](#helm-chart-generation-and-releases)
for how staging output is promoted into `deploy/` at release time.

## Setup gotchas

### Windows BSOD during UI dependency installation

:::note[Known issue with Bun 1.4.2 on Windows/WSL]

On Windows, Bun 1.4.2 may cause a BSOD while `make ui-install` runs
`bun install`. If this happens, remove Bun 1.4.2 and install the version
currently used by Orka CI (`1.3.13`):

For native Windows Bun, run:

```powershell
& "$env:USERPROFILE\.bun\uninstall.ps1"
iex "& {$(irm https://bun.com/install.ps1)} -Version 1.3.13"
```

For Bun installed inside WSL, run:

```bash
rm -rf "$HOME/.bun"
curl -fsSL https://bun.com/install | bash -s "bun-v1.3.13"
```

Restart the terminal and verify the installed version:

```bash
bun --version
```

The command should report `1.3.13`. Then retry:

```bash
make ui-install
```

If Orka CI moves to a newer Bun version, check the `bun-version` in
`.github/workflows/test.yml` before applying this workaround.

:::

## Build gotchas

### UI embedding

`make build` embeds the React UI into the controller binary via `//go:embed`. The UI must be built first:

```bash
make ui-build    # Build UI and copy to internal/uiembed/dist/
make build       # Now the Go build will succeed
```

If the UI isn't built, the `ensure-ui-embed` Makefile target creates a stub `internal/uiembed/dist/index.html` so the Go build doesn't fail — but the embedded UI won't work.

### CLI version injection

`make build-cli` injects Git version info via `-ldflags`:

```bash
make build-cli   # Produces bin/orka with embedded version
```

### Metrics disabled by default

The controller's `--metrics-bind-address` defaults to `0` (disabled). Set it explicitly to enable Prometheus metrics:

```
--metrics-bind-address=:8443
```

### HTTP/2 disabled by default

HTTP/2 is disabled for metrics and webhook servers due to CVEs ([GHSA-qppj-fm5r-hxr3](https://github.com/advisories/GHSA-qppj-fm5r-hxr3), [GHSA-4374-p667-p6c8](https://github.com/advisories/GHSA-4374-p667-p6c8)). Use `--enable-http2=true` only if needed.

### Leader election

Leader election ID is hardcoded as `03b49a10.orka.ai`, and its Lease is stored
in the controller's required non-empty watch namespace. Static `harness-v1` and
`harness-v2` installations use different watched namespaces and therefore
different Leases; they do not coordinate ownership of one Task population.
