# ADR 0007: Model Substrate oversubscription as controller-owned actor pools

## Status

Deferred and superseded in part by the ACP RuntimePool model.

The current first-release pool is a controller-owned logical ACP `RuntimePool` with one active Kubernetes Pod, bounded resident RuntimeSessions, bounded concurrent prompts, drain, and scale-to-zero behavior. Tasks do not choose individual workers or Pods.

A future Substrate implementation may map that logical pool to a bounded WorkerPool and Actor density, but each Actor must host one fenced `orka.harness.v2` RuntimeSession. Orka remains authoritative for queueing, admission, prompt leases, outcome classification, validation, publication reservations, and cleanup. Actor suspension or juggling must not occur while prompt, validation, publication, finalization, or Session lease work is active.

Provider-native density and placement may be summarized safely in pool status. Raw snapshot URIs, daemon URLs, tokens, and arbitrary restore controls must not appear in Task status.
