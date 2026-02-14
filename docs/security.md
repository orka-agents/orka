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
- The `orka` CLI extracts tokens from kubeconfig for browser-based login

## Secret Management

- LLM API keys and tool credentials are stored in Kubernetes Secrets
- Secrets are referenced via `secretRef` in CRD specs and mounted as read-only volumes
- Secrets are never logged, stored in task specs, or exposed in API responses

## Namespace Scoping

- Controller can be scoped to a single namespace with the `--watch-namespace` flag
- Chat endpoint blocks operations in `kube-system` and `kube-public` namespaces
- The embedded UI is served over the same port as the API (no separate attack surface)

## Multi-Tenancy

Orka supports soft multi-tenancy using Kubernetes namespaces as tenant boundaries.

### Namespace Isolation

Enable `--enforce-namespace-isolation` to restrict users to their ServiceAccount's namespace:

- API requests are rejected (403) if the target namespace differs from the caller's SA namespace
- Tasks cannot reference Agents or Providers in other namespaces (cross-namespace `agentRef.namespace` and `providerRef.namespace` are rejected)
- Workers cannot submit results to namespaces other than their own
- All access denials are logged with caller identity and IP address

### Per-Namespace Task Limits

Use `--max-tasks-per-namespace` to cap the number of active (Pending/Running) tasks per namespace. Tasks exceeding the limit are requeued with backoff. Set to `0` (default) for unlimited.

### Recommended Production Configuration

For multi-tenant deployments, enable both isolation and limits:

```
--enforce-namespace-isolation=true
--max-tasks-per-namespace=50
--watch-namespace=""
```

### Data Isolation

All stored data (results, sessions, plans) is keyed by namespace in SQLite. Queries always filter by namespace. The controller, API server, and workers all enforce namespace boundaries at their respective layers.

### Audit Logging

Security-relevant events are logged at the API layer:

- Authentication failures (missing/invalid tokens)
- Namespace access denials (isolation violations, watch-namespace mismatches)
- Cross-namespace worker access attempts
