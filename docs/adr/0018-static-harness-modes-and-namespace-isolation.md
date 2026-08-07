# ADR 0018: Use static harness modes and namespace-isolated control planes

Date: 2026-08-07

## Status

Accepted. Supersedes ADR 0016 and ADR 0017. The normative rollout and
verification contract is `docs/harness-v1-v2-coexistence-plan.md` Revision 5.

## Context

ADR 0016 selected full active coexistence, and ADR 0017 placed harness v1 and
harness v2 behind one controller ownership scope and one Task population. That
design safely preserved ambiguous in-flight state, but required three new CRDs,
dynamic backend modes, immutable route bindings and snapshots, legacy
classification, quarantine and adjudication, binding reservations, and a
cluster-global ownership fence.

The actual product requirement is narrower: keep a legacy v1 installation
available while a new v2 installation is proven on the same Kubernetes
cluster. Existing work does not need to change protocols, and a Session does
not need to continue across protocols. Kubernetes namespaces can provide the
ownership boundary if controller caches, RBAC, storage, Leases, endpoints, and
data planes are isolated with them.

CRDs remain cluster-scoped. Separate releases therefore cannot independently
evolve incompatible CRD schemas, even when every custom resource is
namespaced.

## Decision

Run harness v1 and harness v2 as two independent Orka installations. Every
controller requires exactly one static startup mode:

- `harness-v1` enables the legacy wrapper path and does not enable ACP agent
  execution;
- `harness-v2` enables ACP RuntimePool/RuntimeSession execution and does not
  enable the legacy wrapper path.

`dual`, `auto`, and `harness-v1-drain` are not modes. Mode is not a runtime
setting and cannot be changed in place.

Every controller requires a non-empty `--watch-namespace`. That namespace must
carry `orka.ai/controller-mode` with the exact controller mode. A missing or
mismatched claim fails startup. Leader election remains mandatory, but its
Lease is scoped to the watched namespace rather than a cluster-global harness
ownership namespace.

Same-cluster coexistence requires different:

- release names and controller namespaces;
- watched workload namespaces;
- API Services and producer endpoints;
- ServiceAccounts and namespace-enforcing RBAC;
- leader-election Leases;
- SQLite stores, PVCs, backups, and Secrets;
- wrapper, worker, publisher, proxy, and runtime data-plane resources.

The v2 installation is the sole owner of cluster-scoped gateway and
workspace-provider reconcilers. Cluster-scoped CRDs and common admission
resources have one designated platform owner and are not independently owned by
both Helm releases.

One shared CRD bundle preserves the supported v1 and v2 object shapes. Contract
selectors may remain where needed to discriminate that schema, but a selector
must match controller mode and cannot route a Task to another protocol.
`runtime.type: opencode` is not a protocol selector.

There is no supported migration between protocols:

- no Task, Agent, or AgentRuntime changes its execution protocol;
- no Session or transcript continues under the other protocol;
- no controller cancels, settles, finalizes, publishes, or cleans up work from
  the other installation;
- no controller database, PVC, ledger, or runtime store is reused under the
  other mode.

Creating an object in the other installation creates new work with a new
namespace, UID, Session lineage, attempt history, and external-effect history.

The architecture does not include `AgentExecutionControl`,
`AgentExecutionPolicy`, or `AgentExecutionAdjudication`. It also does not
include shared-population binding, classification, quarantine, adjudication,
or dynamic backend-mode machinery.

V1 draining is operational: stop its ingress and producers, revoke create
permission, inventory and settle existing v1 work, then remove the v1 release.
The controller does not acquire a third drain mode.

## Consequences

- V1 and v2 can be canaried on one cluster without sharing execution ownership
  or state.
- The three coexistence CRDs and most shared-population coordination machinery
  are unnecessary.
- Protocol selection is obvious from the installation endpoint, watched
  namespace, and static mode.
- Mode changes require a new namespace and installation. This deliberately
  trades in-place migration for a smaller safety surface.
- Operators temporarily manage two endpoints, stores, data planes, and backup
  sets.
- Existing v1 Tasks and Sessions remain on v1 until they finish or are
  canceled. They cannot be resumed on v2.
- Rollback redirects future submissions; it does not convert accepted v2 work
  into v1 work.
- A designated platform owner must coordinate every CRD upgrade and preserve
  both shapes for the full compatibility window.
- Namespace-scoped caches without namespace-enforcing RBAC are insufficient;
  isolation tests must prove cross-plane reads and writes are forbidden.
- Historical v1 objects may outlive the v1 data plane and keep the shared
  schema broad until retention and archival requirements are complete.
