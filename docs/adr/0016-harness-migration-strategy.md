# ADR 0016: Select full active coexistence for the harness v1/v2 migration

Date: 2026-08-05

## Status

Superseded by ADR 0018 on 2026-08-07. This record is retained as the historical
full-active-coexistence decision; it is no longer an implementation or release
contract.

Previously accepted (engineering). Product and operations countersign are recorded in the
release tracker before the first dual release ships; implementation may proceed
on this record, but canary admission (Phase 7 of the plan) must not open without
the countersign.

## Context

The `acp` line is an intentional v2-only hard cutover: the turn-oriented
harness-wrapper path (`workers/harness`, `cmd/orka-agent-harness-wrapper`,
`internal/controller/harness_wrapper.go`) exists only on `origin/main`
(`21f8ef15`), while the ACP v2 RuntimePool path exists only on `acp`
(`77bdc4db`). The two controllers cannot run concurrently against one Task
population, the v2 CRD prunes v1-only fields, `runtime.type: opencode` exists in
both baselines with incompatible configuration contracts, and the wrapper keeps
active turns in process memory. Installations with in-flight v1 work therefore
have no safe upgrade to v2 today.

`docs/harness-v1-v2-coexistence-plan.md` (Revision 4) evaluates three
strategies and specifies the full-coexistence design. Its Phase 0 gate requires
one strategy to be selected before implementation.

## Decision

Select **full active coexistence** (plan §2.3 option 3) over blue/green
replacement (option 1) and the zero-active-state in-place bridge (option 2).

Decision drivers, recorded per the plan's quantitative gate:

- **Continuity:** in-flight v1 Tasks, wrapper turns, and v1 Sessions must be
  preserved and remain continuable; Session continuity is mandatory for the
  primary installation, which is sufficient under the plan's "one high-value
  installation" rule. Blue/green abandons in-flight runtime state; the
  zero-active bridge requires a full drain window.
- **Canaries:** v2 must be provable with same-cluster canaries under the same
  controller ownership scope before v1 admission closes.
- **Downtime:** near-zero; no maintenance window long enough for a full drain
  is available on the primary installation.
- **External usage:** external v1 `runtimeRef` runtimes and legacy v1 OpenCode
  Agents are assumed present (fail-closed assumption); both are preserved
  during the window under the compatibility policy.
- **Rollback:** consequential external effects (publication, PRs) require the
  checkpointed in-place rollback and coordinated restore procedures of plan
  §14; Helm rollback alone is insufficient in every option.
- **Cost:** full coexistence carries the highest build and retirement cost;
  that cost is accepted and time-bounded below.
- **Operator-supplied at rollout:** affected cluster/customer counts and
  active-turn volume are installation-specific and are captured in the
  pre-upgrade inventory report (plan §15) rather than in this record.

Compatibility window: the owner is the repository owner (sozercan). Target
retirement: v1 admission moves to `drain-only` by default two minor releases
after the first dual release and the v1 data plane is removed one release
later, with a maximum window of six months from the first dual release unless
this ADR is superseded.

Blue/green replacement and the zero-active-state bridge are rejected for this
line, and their reduced plans are not produced. Per plan §2.3 this selection
scopes all later phases to the full-coexistence design.

## Consequences

- The remainder of `docs/harness-v1-v2-coexistence-plan.md` is the implementation
  contract; ADR 0017 records its architecture.
- The v1 execution plane is restored and maintained for the bounded window,
  including build, scanning, and CI lanes for both image families.
- Until the compatibility release and its preflight tooling exist, simultaneous
  v1 and v2 operation still requires separate clusters or non-overlapping
  control planes.
- Retirement is a gated, staged operation (plan §12 Phase 8, §15), not a
  side effect of the window expiring.
