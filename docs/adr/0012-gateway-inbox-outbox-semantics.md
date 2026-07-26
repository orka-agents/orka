# ADR 0012: Use durable at-least-once inbox and outbox semantics for gateways

Date: 2026-07-16

## Status

Accepted for `orka.gateway.v1`.

## Context

Adapters need a fast durable acknowledgement, while Task creation and provider delivery may happen after process or network failures. Exactly-once network delivery cannot be guaranteed across a controller crash after a provider send but before the local status update.

## Decision

Gateway ingress and delivery use SQLite-backed inbox/outbox records with at-least-once processing and stable idempotency IDs.

- Ingress deduplicates on `(namespace, Gateway UID, external event ID)`.
- Event and delivery claims use expiring leases so another replica can recover abandoned work.
- Task names and delivery IDs are deterministic.
- Per-Session dispatch is FIFO and reserves the Session before Task creation.
- Adapter delivery is synchronous, retryable only with the same delivery ID, capped at ten attempts, and bounded by the event's 24-hour expiry.
- Adapters must advertise and implement idempotent delivery.
- Queue overflow, expiry, and permanent failures are dead-lettered; manual delivery retry creates an audited retry count without changing the idempotency ID.

## Consequences

Orka may replay a request after an ambiguous network outcome, but a conforming adapter produces one provider-side send. Internal records, not per-event Kubernetes resources, carry high-volume state. Operators can inspect and retry dead letters through REST, CLI, and UI surfaces.
