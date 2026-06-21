# ADR 0008: Start RuntimeSession persistence as an internal store

Date: 2026-06-11

## Status

Accepted for the first frontier implementation wave.

## Context

The remaining frontier introduces backend-neutral runtime sessions that can be claimed, reused, released, retained, suspended, and deleted. The lifecycle must support non-Substrate providers first while keeping Agent Substrate optional. Runtime sessions require strict namespace ownership and cleanup semantics, but the first provider still needs to prove the turn protocol and conformance suite before operators need a public CRD surface.

## Decision

Start with an internal RuntimeSession state model and persistence boundary, then add a CRD only after the non-Substrate provider and cleanup loop have stable status requirements.

The frozen state machine lives in `internal/harness` so the controller/provider implementation can share validation. Public API/CLI visibility can read from the internal store initially and later migrate to a CRD-backed implementation without changing the turn protocol.

## Consequences

- Provider integration can ship behind feature gates without expanding the Kubernetes API surface prematurely.
- Namespace ownership, cleanup policies, and transition validation are testable before persistence is introduced.
- Operator visibility is initially API/CLI-driven rather than `kubectl get runtimesessions`.
- A future CRD migration must preserve IDs, owner metadata, cleanup policy, active task, provider, phase, idle timeout, and max lifetime.

## Revisit

Revisit once the non-Substrate provider passes conformance and the cleanup controller needs watch/reconcile semantics that are awkward for an internal store.
