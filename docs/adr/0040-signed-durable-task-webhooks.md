# 40. Sign Task webhooks and deliver them from durable Task status

Date: 2026-11-06

## Status

Accepted. Replaces the best-effort, unsigned `spec.webhookURL` sender. It leaves
per-receiver secrets, non-terminal events, and webhook egress through a proxy open.

## Context

`Task.spec.webhookURL` was delivered by a sender that resolved the host once,
ignored DNS errors, checked only loopback, RFC 1918, and link-local addresses,
followed redirects, honored proxy environment variables, sent unsigned bodies,
and retried every 30 seconds forever. Any Task author could therefore make the
controller, which has cluster-wide credentials and network reach, call internal
addresses: through a name that fails to resolve during validation, through a
name rebound between validation and dial, through a redirect, or through ranges
the check missed (`0.0.0.0`, CGNAT, IPv6 unique-local, IPv4-mapped IPv6).
Receivers also had no way to tell an Orka notice from a forgery.

## Decision

**Destination policy is enforced at dial time.** The webhook HTTP client uses a
dialer that resolves the host on every connection, rejects the whole answer if
any address is not public (the same `tokenexchange.IsPublicAddress` policy that
guards outbound credentials), dials only checked addresses, and re-checks the
exact socket address in the dialer's `Control` hook. Resolution failure rejects.
The client has no proxy, never follows redirects, and never reuses connections.
Literal non-public IPs, blocked names, and cross-namespace Services fail before
any network I/O. The only non-public destination is a same-namespace Service
with a selector and a ClusterIP, which is dialed by its ClusterIPs with no DNS.

**Every attempt is signed.** The body is covered by a compact JWS with
`alg: EdDSA`, a `kid` equal to the RFC 7638 thumbprint, and claims binding the
issuer, the exact webhook URL as audience, the Task UID/namespace/name, the
event type, the delivery ID and attempt, and the SHA-256 of the body, valid for
five minutes. The key lives in one Secret in the controller namespace. `current`
signs. `previous` is published but never signs. The controller creates the
Secret and mints `current` when either is missing, but never overwrites an
invalid key. With no key, nothing is sent. Public keys are served without
authentication at `/.well-known/orka/task-webhook-jwks.json` on the API.

**Delivery state is in `Task.status.webhookDelivery`.** One record per Task
holds the delivery ID, event, occurrence time, body digest, state, attempt
count, lease or backoff deadline, and a sanitized last error. An attempt is
claimed by writing the incremented count and a lease to status, with the
Task's resourceVersion, before the request is sent. That makes the status
write the cross-replica lease: of two controllers, only one claim succeeds, and
a controller that dies mid-attempt leaves a lease that the next one honors. The
body is rebuilt from current state on every attempt. If its digest no longer
matches, the pending notice is superseded by a new delivery ID with fresh
attempts and the old one is never sent. Attempts are bounded (8, exponential
backoff from 10 seconds, capped at 15 minutes), and then the record is `Failed`.

Task status was chosen over the other existing mechanisms:

- **The storage outbox.** It is the ACP projection contract, versioned in
  `pkg/storage/v1` with conformance suites and epoch fences for two backends.
  Adding a webhook kind would widen a public storage contract for state that
  belongs to one Task.
- **A ConfigMap ledger.** It would add a second object to keep consistent with
  the Task, plus its own garbage collection.

Task status is already where `webhookDelivered` lived. It is deleted with the
Task, needs no new RBAC or backend, and its optimistic concurrency provides the
lease.

## Consequences

- Delivery is at-least-once. Receivers must deduplicate on the delivery ID.
  A crash between send and record repeats the attempt under the same ID.
- A host name that resolves to a private address no longer works, even if it
  did before. In-cluster receivers must be same-namespace Services, and
  egress proxies must be fronted by one.
- Deleting a Task drops its pending notice, as before.
- Rotation is an operator edit of one Secret. Replicas reload within 15
  seconds. Receivers must refetch the JWKS on an unknown `kid`, and `previous`
  must stay published for at least the token TTL plus that reload window.
- Each attempt costs two Task status writes, and attempts are bounded.
- The payload gains `deliveryId`, `eventType`, `occurredAt`, and `taskUID`.
  Existing fields are unchanged.
