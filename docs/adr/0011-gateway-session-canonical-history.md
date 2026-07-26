# ADR 0011: Keep external conversation history in Orka Sessions

Date: 2026-07-16

## Status

Accepted for `gateway.orka.ai/v1alpha1`.

## Context

External adapters and agent runtimes can each maintain provider-native transcripts, but those projections are replaceable and may be unavailable after an adapter or runtime change. Gateway ingress also needs durable acknowledgement before agent execution begins, so the user message must have a canonical owner before a Task exists.

## Decision

Orka Sessions own canonical user-visible history for generic gateways.

- Durable admission atomically creates or upserts the derived Session and appends one user message.
- Session messages have a stable message ID, source type, source reference, and bounded safe metadata.
- Gateway-created Tasks use `sessionRef.create: false` and `sessionRef.append: false`; the gateway projection owns user and terminal message writes.
- Successful terminal results append one assistant message. Failed or cancelled Tasks append one sanitized visible error message.
- Runtime-native transcripts remain execution projections and may be discarded or replaced.

## Consequences

A duplicate event cannot append a second prompt, and a retried terminal projection cannot append a second result. Ordered dispatch can use the Session as its serialization key. Gateway backup and restore must include Sessions together with the event and delivery ledgers.
