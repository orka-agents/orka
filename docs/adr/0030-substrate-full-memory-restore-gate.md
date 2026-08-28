# 30. Gating credential-safe Substrate full-memory restore

Date: 2026-08-22

## Status

Accepted. Defines the security gate issue #423 requires before Orka may ever
restore a Substrate `Full` snapshot of an ACP runtime. The gate is closed;
ADRs 0027/0028 define the only admitted `DataOnly` mode. Executing that mode
also requires immutable provider snapshot proof and an atomic resume fence.

## Context

A Substrate `Full` snapshot contains process memory, root filesystem changes,
and durable data, and a valid Full snapshot always restores its own guest
state. A restored ACP supervisor would therefore retain:

- controller, capability, and provider-proxy bearer credentials;
- model, MCP, repository, publication, transaction, and child-process
  credentials;
- a stale RuntimeSession and boot identity;
- in-flight prompts, tool calls, publications, and network requests, plus
  buffered responses whose acceptance state is unknown;
- authority that can be duplicated by cloning a snapshot or restoring it more
  than once.

## Decision

Full-memory restore is expressed as one explicit, hard-closed gate:
`substrateFullMemoryRestoreGateOpen()` returns false and is the single future
opening point. Independent of the gate, the mode is rejected at every layer:

- the profile CRD enum admits only `DataOnly`, and the class resolver rejects
  any other mode with the explicit gate message;
- frozen class bindings and pool bindings carrying a non-DataOnly mode fail
  canonical verification and the suspend-capability checks;
- the deployed derived template's snapshot policy is re-proven to be exactly
  `Data`/`Data`/`ColdBoot` at both the suspension and the resume boundary, so
  a template swapped to `Full` while an actor is quiescent or suspended can
  neither checkpoint nor restore it;
- provider acceptance must return an immutable `ActorSnapshot` UID/version,
  source Actor UID/version, and observed `Data` content scope; Orka stores only
  a digest of that proof and never backfills it for older suspended pools;
- data restore uses a separate control operation that must compare the Actor
  and snapshot proof atomically with `ResumeActor`. A client that can only run
  a preflight read is rejected before actor creation.

The currently vendored Substrate client cannot satisfy this contract. Its
protocol predates immutable `ActorSnapshot` records, while the pinned upstream
API and current upstream `ResumeActor` request still carry only the Actor
reference and `boot` flag, with no expected UID/version or snapshot
precondition. The in-tree client therefore does not advertise atomic data
resume support. Suspend-capable pools stay closed before actor creation until
the provider protocol and client add that operation.

The gate may only open when all of the following hold, each with live
adversarial coverage:

1. **Credential invalidation.** No long-lived shared credential remains in
   supervisor memory; every runtime credential binds to the Session UID and
   generation, exact Actor lifetime, snapshot generation, boot ID, controller
   epoch, operation, and expiry; the pre-suspend credential generation is
   revoked before any restored process can reach controller, provider, model,
   MCP, SCM, or publication endpoints; a restored supervisor starts sealed,
   discards snapshotted credentials, and completes a fresh authenticated
   bootstrap before admission opens.
2. **Quiescence and settlement.** Admission stops and the active prompt
   settles or cancels before checkpointing; no child process, tool call,
   clean-room publication, workspace mutation, or outbound request remains
   active; an accepted or possibly accepted prompt is never replayed; failure
   to prove quiescence rejects the suspension or quarantines the workspace.
3. **Restore and clone fencing.** Exactly one resume of one exact snapshot
   generation into one exact Actor and RuntimeSession generation; concurrent
   resume, duplicate restore, stale snapshots, tag retargeting, Actor
   recreation, template drift, and controller-epoch mismatch are rejected; a
   restored process adopts a fresh boot ID and RuntimeInstanceID before
   serving; snapshot and tag identifiers stay internal.
4. **Policy and rollout.** An explicit operator class and template policy plus
   a controller feature gate are all required; observed Full snapshots under
   data-only classes stay rejected; disabling the gate closes admission and
   drains or quarantines full-memory workspaces without converting them; the
   supported Substrate versions and sandbox classes are documented.

## Consequences

- The mode cannot open by accident: flipping the gate function alone changes
  nothing because the admission enum, binding verification, and boundary
  policy proofs each reject non-Data policies independently.
- Existing suspended pools without the immutable proof remain quarantined.
  Deriving consent from the provider's current mutable observation would turn
  an upgrade into an unsafe snapshot backfill.
- The prerequisites double as the acceptance checklist for the future
  implementation, and the negative tests added with this ADR must be extended
  into that implementation's adversarial matrix rather than weakened.
