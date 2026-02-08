# Security

## Worker Pods

All worker pods run with a hardened security context:

- Non-root user (uid 1000)
- Read-only root filesystem
- All Linux capabilities dropped
- Seccomp profile: RuntimeDefault

## Controller

The controller runs with:

- Non-root user (uid 65532)
- Read-only root filesystem
- Seccomp profile: RuntimeDefault

## Authentication

- All API endpoints require a Kubernetes ServiceAccount bearer token
- Token validation uses the Kubernetes TokenReview API
- The `mercan` CLI extracts tokens from kubeconfig for browser-based login

## Secret Management

- LLM API keys and tool credentials are stored in Kubernetes Secrets
- Secrets are referenced via `secretRef` in CRD specs and mounted as read-only volumes
- Secrets are never logged, stored in task specs, or exposed in API responses

## Namespace Scoping

- Controller can be scoped to a single namespace with the `--watch-namespace` flag
- Chat endpoint blocks operations in `kube-system` and `kube-public` namespaces
- The embedded UI is served over the same port as the API (no separate attack surface)
