# ADR 0009: Defer runtime-session UI until a public API exists

Date: 2026-06-13

## Status

Accepted.

## Context

The evented-execution UI follow-up (task/session timelines, trace, approvals,
fork) is shipping in the web UI. Its final phase asks whether the UI should also
surface RuntimeSession lifecycle (claim, reuse, release, retain, suspend,
delete).

Per ADR 0008, RuntimeSession persistence starts as an internal `internal/harness`
state model with **no public CRD or HTTP API**. A scan of the server routes
(`internal/api/server.go`) confirms there is currently no
`runtimesession`/`runtime-session` endpoint — `RuntimeSession*` types exist only
in `internal/harness` as the turn-protocol contract. The only session-scoped HTTP
surfaces are the conversation-session endpoints (`/api/v1/sessions/...`), which
are unrelated to runtime sessions.

The UI follow-up plan is explicit that the UI must not invent backend behavior or
call endpoints that do not exist.

## Decision

Do not add any production UI that lists, gets, or deletes runtime sessions until a
public runtime-session API exists. No runtime-session API client, hook, route, or
component ships in this follow-up. This ADR is the documented follow-up that the
plan requires.

When a public API does land (reading from the internal store first, then a
CRD-backed implementation per ADR 0008), add a feature-gated runtime-session view
that surfaces, per session:

- runtime session id
- namespace
- provider
- state / phase
- active task (linkable to task detail)
- idle age (and idle timeout)
- max lifetime
- cleanup / retention policy
- owner metadata
- actions: get, and delete when the API supports it

The view must hide gracefully (render nothing, raise no errors) when the backend
reports the capability as unavailable — the same `501 Not Implemented` pattern the
execution-event surfaces already use.

## Consequences

- No UI depends on unimplemented runtime-session backend behavior; nothing calls a
  nonexistent endpoint.
- The field list above is fixed up front, matching the migration-preserving fields
  in ADR 0008, so a future implementation has a clear target.
- When the API ships, this is additive UI work behind a capability check rather
  than a redesign.

## Revisit

Revisit when the non-Substrate provider passes conformance and a public
runtime-session read API (internal-store-backed or CRD-backed) is exposed under
`/api/v1`. At that point, implement the feature-gated view described above and
update the UI guide (`website/docs/guides/ui.md`).
