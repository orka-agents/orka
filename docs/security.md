# Security

## Worker Pods

All worker pods run with a hardened security context:

- Non-root user (uid 1000)
- Read-only root filesystem
- All Linux capabilities dropped
- Seccomp profile: RuntimeDefault

### Writable Paths

Only three paths are writable (mounted as EmptyDir volumes):

| Path | Purpose |
|------|---------|
| `/tmp` | Container runtime temporary files |
| `/home/worker` | CLI config/cache (Claude, Copilot, Codex) |
| `/workspace` | Git clone and working directory |

All other filesystem paths are read-only. Workers that write outside these paths will fail.

### Agent Runtime Security Notes

- **Copilot agent worker**: Auto-approves **all** permission requests from the Copilot SDK without filtering. The agent can execute shell commands, file operations, and network requests in autonomous mode with no safeguards beyond the read-only rootfs
- **Claude agent worker**: Sets `HOME=/home/worker` unconditionally, bypassing default HOME permissions
- **Git credentials**: `GIT_TOKEN` and `GITHUB_TOKEN` are set as environment variables, visible to all child processes. If the prompt is user-controlled, the LLM agent could potentially access these via `os.Environ()`
- **Prior task diffs**: Applied via `git apply --check` (dry-run) then `git apply`, but without verifying patch author or integrity. A compromised prior task could inject arbitrary changes

## Controller

The controller runs with:

- Non-root user (uid 65532)
- Read-only root filesystem
- Seccomp profile: RuntimeDefault

## Authentication

- All API endpoints require a Kubernetes ServiceAccount bearer token
- Token validation uses the Kubernetes TokenReview API
- The `orka` CLI extracts tokens from kubeconfig for browser-based login
- **Token caching**: Validated tokens are cached for 60 seconds using SHA256 hashes to avoid repeated TokenReview API calls. Token revocation has up to 60s propagation delay. The cache is in-memory only — not persistent across pod restarts

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
