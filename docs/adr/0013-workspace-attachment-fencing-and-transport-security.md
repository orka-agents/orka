# ADR 0013: Fence workspace attachments with epochs, rotated tokens, and TLS

Date: 2026-07-16

## Status

Accepted.

## Context

A reusable workspace can outlive a Task worker. Deleting a Job or expiring a Lease alone does not mechanically prevent a stale process from calling the workspace data plane. Reuse therefore requires an enforced authorization generation, credential rotation, and revocation ordering.

## Decision

Interactive attachment is exclusive and uses both a Kubernetes Lease and a workspace-agent-enforced attachment epoch.

For every attachment attempt Orka core:

1. Acquires or renews the workspace attachment Lease for the Task UID.
2. Atomically increments the workspace epoch.
3. Generates 32 random bytes with a cryptographically secure RNG.
4. Stores the raw bearer token only in a namespaced, owner-referenced Kubernetes Secret.
5. Writes only `sha256:<hex>`, epoch, expiry, Task UID, workspace UID, and Secret reference into generic control-plane state.
6. Asks the provider's privileged control path to activate that attachment.
7. Starts the worker only after the workspace status reports the same enforced epoch.

Every data request carries:

- protocol version,
- workspace UID,
- attachment epoch,
- `Authorization: Bearer <token>`.

The workspace agent stores only the active attachment record in memory or tmpfs. It hashes the supplied bearer token and compares it in constant time with the active digest. A missing, expired, stale-epoch, wrong-workspace, or wrong-token request is rejected before dispatch. The rule applies to exec, operation status, cancellation, upload, download, scrub, and reset.

Because workspace files may outlive the agent process while the attachment record does not, a secured agent requires a full control-authenticated reset before its first attachment after every process start or restart. Reset is permitted in this initially unbound state, removes every task-writable root, and rotates the binding generation before activation can succeed.

Revocation ordering is strict:

1. Disable the active epoch in workspace-agent.
2. Observe attachment revocation in workspace status.
3. Delete the worker Job and attachment Secret and release the Lease.
4. Run scrub/reset/suspend/delete according to policy.

If revocation cannot be confirmed before the class detach timeout, core deletes the Job and Secret, marks the workspace quarantined, and permits the Task to terminate with degraded finalization. The quarantined workspace is never selected for reuse.

TLS is required between trusted control-plane/worker clients and workspace-agent. Connection Secrets carry endpoint and CA data. Plain HTTP is rejected unless the explicit development-only insecure flag is enabled. Tokens are never included in URLs, errors, structured logs, events, status, or metrics. Authentication failures use bounded reason labels only.

Service workspaces do not use Task attachment tokens. Provider lifecycle/control credentials remain separate and are never mounted into Task workers.

## Consequences

- A stale first Task cannot mutate a workspace after a later epoch attaches.
- Credential deletion is defense in depth; the data plane itself enforces revocation.
- Cleanup failures do not require keeping Task credentials alive.
- Concurrent shared attachment is intentionally unsupported in v1.
- SPIFFE/mTLS workload identity may replace bearer transport later without changing epoch semantics.
