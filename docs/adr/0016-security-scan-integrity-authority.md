# ADR 0016: Additive security-scan integrity authority

- **Status:** Accepted
- **Date:** 2026-08-01

## Context

Repository security scans previously treated mutable Task results/artifacts and the latest `security_findings` row as the durable source of truth. That allowed stale Task incarnations, ambiguous validation JSON, moving Git refs, silent mapper omissions, overwritten finding history, and non-deterministic exports to weaken auditability.

The integrity rollout must remain compatible with existing public finding IDs and Task result/artifact APIs. It must not claim protection against a compromised controller or database, and it must not silently upgrade legacy data to verified state.

## Decision

### Authority and identities

- A scan has a controller-generated 256-bit random `runUID` (`run_<64 hex>`). The existing `scan_*` ID remains the public compatibility alias and Kubernetes label value.
- The pre-resolution request key is SHA-256 over RepositoryScan UID/generation, requested branch/ref, mode, baseline, scope, policy, isolation/completion policy, and deep-scan configuration.
- Target resolution adds a separate immutable resolved-target key. It never replaces the request key.
- This release remains before the immutable-authority cutover. Expanded schema is additive, but N-1 binary write compatibility has not yet been exercised against the expanded schema. Until that automated compatibility suite exists, treat the release as rollback-incompatible: stop writers and take a verified SQLite backup before upgrade, and restore that backup before starting an older binary. No database authority epoch is claimed yet.

### Output binding

- `workerOutputBindingMode` is configured with `ORKA_SECURITY_WORKER_OUTPUT_BINDING_MODE=off|audit|enforce` and defaults to `audit`. Pinned-target enforcement requires `enforce`; the controller rejects an unsafe pinned-target/audit combination rather than silently weakening target verification.
- `enforce` remains fail-closed in this release. The current Task/attempt/Job/Pod/turn binding is useful audit evidence, but it is not the controller-issued unpredictable attempt lease required for enforcement. Until that lease is issued to the trusted outer worker or wrapper, rotated and revoked with the attempt, and revalidated at commit, startup rejects `enforce`; pinned-target mode is therefore also unavailable for production rollout.
- Repository-security result and artifact writes are revalidated after the bounded body is read. Kubernetes workers bind to Task UID, current attempt, Job UID, and Pod UID. Harness uploads additionally bind to wrapper Pod UID plus runtime-session, turn, and correlation headers.
- SQLite staging rows record content digest, size, writer binding, and monotonically increasing generation. Controller ingestion rejects a different Task UID/attempt in enforce mode. Legacy rows remain explicitly unverified.
- The current binding digest is a controller-derived writer binding, not a cryptographic bearer lease. It cannot authorize an enforce-mode deployment. A future authority cutover may replace it with a controller-issued unpredictable one-time capability without changing stored provenance fields.

### Target receipts

- The trusted general-worker mapper resolves the exact full Git object ID before model analysis when pinned-target mode is enabled.
- The receipt includes object format, clean tracked-worktree state, tree OID, full-tree digest, snapshot digest, requested ref hints, and a bounded tree index.
- Regular blobs include size and line-count metadata when no larger than 10 MiB. Symlinks and submodules are never regular evidence. Git LFS entries remain explicitly LFS and cannot satisfy verified file evidence.
- A tree index is bounded at 10,000 entries. A missing entry in a truncated index is rejected for verified evidence rather than refetched from a mutable ref.
- Incremental scans require the base to be an ancestor of the head; divergence requests a full scan rather than presenting normal delta coverage.
- Mapper receipts contain a trusted observed HEAD and clean-tree snapshot. Downstream review, validation, and patch receipts currently bind the expected target/receipt but do not yet carry a trusted workspace-preparation attestation for observed HEAD, clean state, and snapshot digest. They therefore cannot qualify a production run as target-verified; this is an additional reason pinned-target rollout remains unavailable.

### Immutable history

- Stage receipts, finding observations, occurrences, assessments, decisions, target receipts, and sealed bundles are insert-only SQLite records guarded by UPDATE/DELETE triggers.
- Existing `fnd_*` IDs remain public projection keys.
- New semantic identities are full-width SHA-256 values. Producer `ruleId`/anchor/instance fields are bounded proposals. Until a rule policy independently upgrades them, identity quality is `producer-proposed`; it cannot drive automatic resolution, suppression, or feedback reuse.
- Existing findings are `legacy`, `legacy-v2`, and `legacy-unrebuildable`; no historical events are fabricated.
- Validation and attack-path assessments are distinct immutable rows even when transported in one validation artifact.
- Human decisions use an append-only sequence with caller idempotency and optimistic expected-version checks. Legacy dismiss/reopen endpoints translate to this reducer when the integrity store is available.

### Bundles

- Canonical `manifest`, `findings`, and `coverage` JSON are built only from immutable store rows and immutable mapper/target receipt bytes.
- Canonicalization defines UTF-8/LF normalization, stable ordering, absent-versus-empty handling, and domain-separated SHA-256 document/content/run-receipt digests.
- SQLite is the selected initial immutable blob/backend boundary. Normal store APIs cannot update or delete sealed rows.
- The artifact is an API-enforced integrity manifest, not a digital signature and not proof against controller/database compromise.
- Non-`off` bundle rollout modes remain fail-closed in this release. Persisted frozen input sets, immutable finding-evidence receipts, and supplemental bundle revisions are required before `shadow` or `enforce` can start; the canonical builder and store remain available for deterministic compatibility testing only.
- Pre-binding bundle rows with defaulted/empty RepositoryScan UID or generation remain explicitly legacy-unverified and are not served as canonical bundles; they are never rehashed or upgraded by assertion.
- Manual validation is rejected for a sealed source run until supplemental bundle revisions are implemented, so post-seal assessments cannot diverge from the immutable bundle.

### Hardened analysis

- `analysisIsolationPolicy` is `legacy`, `prefer-hardened`, or `require-hardened`.
- The verified baseline uses Orka's existing read-only-agent and runtime-auth-only mechanisms for built-in Codex and Claude runtimes.
- This baseline attests filesystem and credential isolation. It does **not** claim per-turn network confinement in a shared wrapper Pod.
- The existing read-only/runtime-auth annotations are compatibility primitives, not a run-bound capability snapshot. Until Agent UID/generation, implementation identity, wrapper version, credential profile, and capability digest are pinned and revalidated immediately before execution, startup rejects `hardenedAnalysisEnabled`; `prefer-hardened` and `require-hardened` therefore remain unavailable for production rollout.

### Rollout gates

The controller reads these independent environment gates:

- `ORKA_SECURITY_WORKER_OUTPUT_BINDING_MODE` (`audit` by default)
- `ORKA_SECURITY_PINNED_SCAN_TARGETS_ENABLED`
- `ORKA_SECURITY_QUALITY_STATE_WRITES_ENABLED`
- `ORKA_SECURITY_FINDING_OBSERVATION_WRITES_ENABLED`
- `ORKA_SECURITY_BUNDLE_SEALING_MODE=off|shadow|enforce`
- `ORKA_SECURITY_HARDENED_ANALYSIS_ENABLED`
- `ORKA_SECURITY_STRICT_COMPLETION_ENABLED`
- `ORKA_SECURITY_DEEP_SCAN_ENABLED`

Bundle sealing requires pinned targets, quality writes, immutable observations, a persisted frozen input set, and immutable evidence receipts. The last two dependencies are not available yet, so non-`off` bundle modes fail startup. Strict completion likewise remains unavailable until authorization receipts and bundle freezing exist. Deep scan requires immutable observations and is rejected by resource policy unless all prerequisites are enabled.

## Trusted computing base

The scan-integrity TCB is:

1. Orka controller and REST API;
2. Orka-owned mapper/general-worker image;
3. agent harness wrapper and runtime-auth proxy;
4. Kubernetes API identity/ownership data used for Task-attempt binding;
5. SQLite store and its configured persistent volume;
6. canonical bundle implementation;
7. any configured external artifact storage after it gains an immutable content-addressed contract.

Model children, mutable Git refs, model-authored status fields, and caller-supplied actor/provenance fields are outside the TCB.

## Consequences

- Schema growth is significant, but old public APIs remain available.
- Gate-off and audit behavior preserve existing workloads; output enforcement, pinned targets, and hardened analysis remain fail-closed until their unpredictable lease and pinned capability-snapshot prerequisites exist.
- Quality may be degraded while discovery phase is successful. Consumers must use the quality projection or sealed bundle for assurance-sensitive automation.
- Strict admission parity, fresh external-identity scheduling delegation, coverage-baseline identities and retry-backlog receipts, frozen bundle input sets, immutable finding-evidence receipts, supplemental bundle revisions, network-isolated hardened pools, retention tombstones, and deep-scan dispatch budgets remain later rollout gates. `complete-coverage` and `assurance-qualified` incremental baselines force a full scan until their durable identities exist; validated completion is rejected. These states must not be represented as already verified.
