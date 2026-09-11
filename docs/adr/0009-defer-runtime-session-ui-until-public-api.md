# ADR 0009: Defer runtime-session UI until a public API exists

Date: 2026-06-13

## Status

Accepted; updated for the ACP v2 cutover.

## Context

The evented-execution UI follow-up (task/session timelines, trace, approvals,
fork) is shipping in the web UI. Its final phase asks whether the UI should also
surface RuntimeSession lifecycle (claim, reuse, release, retain, suspend,
delete).

The ACP hard cutover now stores RuntimeSession control, ownership, lifecycle,
and mutation-lease state in Kubernetes-authoritative
`RuntimeSessionControl` records and coordination Leases. The public HTTP surface
still exposes Task execution/delivery projections and read-only RuntimePool
endpoints rather than direct RuntimeSession mutation. Conversation session
endpoints (`/api/v1/sessions/...`) remain a separate canonical-transcript
surface backed by SQLite payload storage.

The UI follow-up plan is explicit that the UI must not invent backend behavior or
call endpoints that do not exist.

## Decision

Do not add any production UI that lists, gets, or deletes runtime sessions until a
public runtime-session API exists. No runtime-session API client, hook, route, or
component ships in this follow-up. This ADR is the documented follow-up that the
plan requires.

When a public read API does land, it must project the Kubernetes-authoritative
control record rather than create a second SQLite authority. Add a
feature-gated runtime-session view that surfaces, per session:

- runtime session id
- namespace
- RuntimePool and exact runtime instance identity
- provider/model profile
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
- The field list above is fixed up front and must remain a safe projection of
  `RuntimeSessionControl`, RuntimePool identity, and non-secret transcript data.
- When the API ships, this is additive UI work behind a capability check rather
  than a redesign.

## Revisit

Revisit when a public RuntimeSession read API is exposed under `/api/v1`. At
that point, implement the feature-gated view described above and update the UI
guide (`website/docs/guides/ui.md`).
