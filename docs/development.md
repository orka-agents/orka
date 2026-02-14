# Development

## Prerequisites

- Go 1.25+
- Bun (for UI build)
- Docker 17.03+
- kubectl v1.11.3+
- Access to a Kubernetes v1.11.3+ cluster

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
# Run all Go unit tests (uses envtest for K8s API + etcd)
make test

# Lint
make lint
make lint-fix

# E2E tests (uses isolated Kind cluster)
make test-e2e
```

See [Testing](testing.md) for full test structure and patterns.

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
make docker-build-all              # All images

# Push images
make docker-push
make docker-push-claude-worker
make docker-push-copilot-worker
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
