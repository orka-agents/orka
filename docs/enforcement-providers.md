# Enforcement provider lifecycle

The `internal/enforcementprovider` package coordinates policy enforcement while a workload remains held. Orka does not select the provider implementation; operators pin its identity and trust material.

Operator configuration supplies a `TrustProfile` that pins provider ID, endpoint identity, Ed25519 key ID/key, minimum epoch, and algorithm. Orka verifies the descriptor signature and freshness against that profile before consuming the descriptor's proof key. A self-signed substitute, wrong endpoint, stale or revoked descriptor, unsupported protocol, or unsupported required capability fails closed.

Authorization binds exact policy bytes and digest to an attempt, workload, provider, audience, challenge, and idempotency key. The provider performs prepare, activate, independent readback, and proof operations. The workload stays held after activation. Proof consumption atomically creates an immutable release obligation. `FileStore` persists this state with versioned compare-and-swap semantics; a restart-safe releaser executes only the exact obligation.

`NewUnixProvider` uses a bounded versioned JSON protocol over a Unix socket and authenticates the server UID with peer credentials. The synthetic provider remains available for isolated tests. The external conformance test drives a configured server by supplying socket and trust-file environment variables.
