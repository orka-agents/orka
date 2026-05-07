# Development

## Prerequisites

- Go 1.25.3+
- Bun (for UI build)
- Docker 17.03+
- kubectl (version compatible with your cluster)
- Access to a Kubernetes cluster

## Build Commands

```bash
# Generate CRD manifests and Go types
make generate
make manifests

# Build (includes UI)
make build

# Build CLI only
make build-cli

# Run locally
make run
```

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

### Live CI Validation

The repository has two live GitHub Actions E2E workflows in addition to the normal test matrix:

- `Live Copilot Proxy E2E` — exercises live model-backed Orka paths through the copilot-proxy harness.
- `Live GitHub OIDC E2E` — builds the PR controller image, deploys it to Kind, authenticates to Orka with a real GitHub Actions OIDC token, verifies `spec.requestedBy` stamping, and rejects client tampering.

Validate workflow/script edits locally before pushing:

```bash
bash -n scripts/live-github-oidc-e2e.sh
go run github.com/rhysd/actionlint/cmd/actionlint@latest .github/workflows/live-github-oidc-e2e.yml
```

The GitHub OIDC live script requires GitHub Actions `id-token: write` or a manual `ORKA_GITHUB_OIDC_TOKEN`; without either, it fails fast before creating a cluster.

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
make docker-build-claude-worker    # Claude agent worker
make docker-build-copilot-worker   # Copilot agent worker
make docker-build-codex-worker     # Codex agent worker
make docker-build-ai-worker        # AI worker
make docker-build-general-worker   # General worker
make docker-build-all              # All images

# Push images
make docker-push
make docker-push-claude-worker
make docker-push-copilot-worker
make docker-push-codex-worker
make docker-push-all
```

## Local Development with Kind

```bash
kind create cluster
make docker-build docker-push IMG=<registry>/orka:tag
make deploy IMG=<registry>/orka:tag
```

## Generate Installer YAML

```bash
make build-installer IMG=ghcr.io/sozercan/orka:latest
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
