# ADR 0012: Use generic CRDs for control and a versioned workspace agent for data

Date: 2026-07-16

## Status

Accepted. This ADR supersedes ADR 0006 as the target architecture. ADR 0006 remains the historical wrapper-first migration decision for the legacy provider paths.

## Context

Wrapper-first provider execution preserved the existing worker path, but it made a Task worker responsible for provisioning, attachment, cleanup, and status. That prevents a workspace from becoming ready before a worker Job, makes service workspaces awkward, gives retries too much provider authority, and couples the core image to provider clients.

## Decision

Provider adapters are Kubernetes reconcilers watching generic `workspace.orka.ai` resources. An adapter selects only providers whose immutable `spec.controllerName` matches its controller identity. It reconciles provider configuration, pools, and concrete workspaces; it never receives Task or Tool credentials and never writes Task or Tool status.

The in-workspace data plane is `orka-workspace-agent`, implementing explicit protocol `workspace.orka.ai/v1` over HTTP:

- `GET /v1/health`
- `GET /v1/capabilities`
- `PUT /v1/control/attachment`
- `DELETE /v1/control/attachment/{epoch}`
- `POST /v1/exec`
- `GET /v1/exec/{operationID}`
- `POST /v1/exec/{operationID}/cancel`
- `PUT /v1/files`
- `POST /v1/files/download`
- `POST /v1/scrub`
- `POST /v1/reset`

Every request and response DTO carries the protocol version. Callers choose operation IDs. Repeating an operation ID returns the original state/result and never executes the operation twice. A timeout is outcome-unknown until the caller queries that ID. Operation results are retained for a bounded period. Upload idempotency is path plus digest. Scrub and reset are idempotent.

The adapter publishes sanitized endpoints and a connection Secret reference. TLS is required for workspace-agent transport. Plain HTTP is accepted only when the Orka controller is explicitly started with `--allow-insecure-workspace-transport`; the default is false. CA material and privileged provider lifecycle credentials are delivered through the connection Secret, never status.

The controller-first Task sequence is:

1. Resolve and authorize a class.
2. Find or create a concrete workspace.
3. Wait for `DataPlaneReady=True`.
4. Create a fenced attachment.
5. Wait for the adapter to report that epoch attached.
6. Create the worker Job with only connection and attachment Secrets.
7. Execute through workspace-agent.
8. Record immutable execution outcome.
9. Revoke attachment and remove worker credentials.
10. Mark the Task terminal; workspace cleanup continues separately.

The workspace-agent health and capability endpoints may be unauthenticated for provider readiness. Attachment-control endpoints require provider/control-plane authentication. Data endpoints require the active workspace UID, epoch, and attachment bearer token.

## State ownership

Core drives `desiredState` and attachment intent. The adapter drives observed workspace state:

- `Pending -> Provisioning -> Ready` for initial provisioning.
- `Ready|Suspended -> Attaching -> Attached` for an attachment epoch.
- `Attached -> Detaching -> Ready|Suspended|Deleting` after revocation.
- `Ready|Suspended -> Suspending -> Suspended` for suspension.
- Any non-deleted state -> `Deleting -> Deleted` for disposal.
- Any state with uncertain isolation or failed mandatory cleanup -> `Quarantined`.
- A recoverable provider error may report `Failed` with conditions; core does not infer provider-native state.

Each transition is reconciled by the provider adapter in response to core-owned intent. Orka's Task/Tool controllers only project the observation.

## Consequences

- Orka core no longer needs provider SDKs after legacy extraction.
- Interactive Tasks and persistent MCP services use the same lifecycle contract.
- Provider retries are Kubernetes reconciliation, not Task replay.
- External adapters can pin the public Orka API and conformance packages without importing `internal/`.
