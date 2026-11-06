# 39. Move to Agent Substrate v1.0.0

Date: 2026-11-06

## Status

Accepted. Supersedes the direct-egress, Actor DNS routing, worker capacity, and
`DrainWorker` termination evidence in ADR 0031, and the Substrate-minted session
identity in the core workspace executor. The data-only cold path of ADR 0031 and the
full-memory restore gate of ADR 0030 stay in force.

## Context

Orka core and the external `orka-workspace` provider pinned Agent Substrate v0.1.0.
Upstream v1.0.0 (commit `b34fbee742906494b5eb4f1f36987176bf8770a4`) renamed the
protocol to `substrate.ateapi.v1` and changed what Orka depended on:

- `ActorIdentity.MintJWT` is gone. Only the `egress_gateway` system role may call
  `Control.MintActorJwt`, and that role can also read every Actor in every Atespace.
  Not even a global owner can mint.
- An empty egress gateway address now means no TCP egress, not direct egress. All
  Actor egress leaves through the egress gateway under a per-Actor `EgressPolicy`
  that matches host names and ports only.
- The router selects Actors by the `ate-target-actor: <atespace>/<actor>` header,
  ignores `Host`, and serves only mTLS clients whose SPIFFE ID is on an exact-match
  allowlist. Any listed client may reach any Actor.
- Workers report `--max-actors` capacity (default 1000), which no WorkerPool field
  sets. A template without a worker selector may land on any pool.
- Authorization is always enforced; `DrainWorker` needs global owner.
- Snapshot content scopes became fidelity levels (`VOLUMES`, `ROOTFS`, `MEMORY`).
  The per-call boot flag, `current_actor_template_uid`, and Tag
  `source_actor_uid` were removed. Golden snapshots are on unless a template
  disables them.
- Pre-1.0 installations cannot be upgraded in place.

kagent and Google AX, the other public v1 integrations, mint no actor credentials,
rely on the router allowlist alone for inbound calls, run as global owners, and do
not enforce single-Actor workers.

## Decision

- **Per-Actor credential.** The controller generates a random per-Actor bearer and
  delivers it through the existing sealed bootstrap, bound to the exact Actor UID and
  worker Pod, alongside router mTLS. The `--substrate-session-identity-*` and
  `--substrate-actor-dns-suffix` settings are removed and fail startup if set.
- **Router access.** Orka's controller identity is the only allowlisted router
  client. Substrate MCP Tool calls from workers, the ACP broker, and the chat API go
  through an authenticated Orka endpoint that authorizes the Tool and forwards with
  `ate-target-actor`; callers never reach the router directly.
- **Egress.** Default deny. An Orka host-name allowlist (FQDN, leftmost-label
  wildcard only, no bare `*`, explicit HTTP, HTTPS, or TLS passthrough and ports) is
  compiled into the Actor's `EgressPolicy` before every resume. Compilation or apply
  failure blocks the resume.
- **Data-only snapshots.** Templates use `preferredFidelity: VOLUMES` with golden
  snapshots disabled. Only `VOLUMES` counts as data-only; `ROOTFS` and `MEMORY`
  fail closed. Suspends are synchronous; deletes carry the observed Actor UID. A
  boot-from-scratch request requires an Actor without guest state whose template
  disables golden snapshots.
- **Single-Actor workers.** A private pool is sized so exactly one template-sized
  Actor fits by CPU or memory, and the provider verifies the worker's Actor
  assignments. No upstream worker image is modified.
- **Least privilege.** Orka identities receive Atespace editor and global viewer
  through access policies, never bootstrap ownership. `DrainWorker` is no longer
  used; termination is proven by worker Pod UID absence, Actor assignments, and the
  worker epoch.
- **Legacy state.** Retire every v0.1.0 workspace, checkpoint, and MCP Actor, then
  install Substrate v1.0.0 fresh. Legacy records fail closed.

## Consequences

Workspace credentials no longer depend on an upstream minting path, and Actor
ingress has one authenticated, authorizing entry point. Actors lose arbitrary
egress; operators must list every destination, including package registries.
Single-Actor placement depends on worker sizing plus assignment checks rather than
an upstream capacity field. Existing v0.1.0 installations need a drain and
reinstall. Live gVisor conformance must be rerun before the pin is trusted.
