---
description: "Advanced setup for running both Orka controller modes on one cluster."
---

# Running both controller modes

For a new installation, follow [Install Orka](installation.md). It uses the
default `harness-v2` mode.

This advanced guide is for clusters that also need a separate `harness-v1`
compatibility installation. The two installations share the Kubernetes API
server and CRDs. Each has its own Tasks, Sessions, controller state, and agent
execution resources.

The examples name the default installation `orka` and the compatibility
installation `orka-compat`. These are Helm installation names. They do not
select an Orka version or controller mode.

## Controller modes

Each controller accepts exactly one required mode:

| Mode | Agent execution path |
| --- | --- |
| `harness-v1` | Legacy turn-oriented harness wrapper |
| `harness-v2` | ACP RuntimePools and RuntimeSessions |

There is no `dual`, `auto`, or `harness-v1-drain` mode. An installation never changes
mode in place.

The controller also requires a non-empty watched namespace labeled with the
same mode:

```bash
export ORKA_CONTEXT='<your-kubeconfig-context>'
kubectl --context "${ORKA_CONTEXT}" create -f - <<'EOF'
apiVersion: v1
kind: Namespace
metadata:
  name: orka-compat-system
  labels:
    orka.ai/controller-mode: harness-v1
---
apiVersion: v1
kind: Namespace
metadata:
  name: orka-system
  labels:
    orka.ai/controller-mode: harness-v2
EOF
```

:::warning[The label must exist before the controller starts]
A missing or mismatched `orka.ai/controller-mode` label fails startup. Set it when you create
the namespace. Do not adopt an unlabeled namespace, and do not relabel one to move it between
modes — the two harnesses own different resources in it.
:::

## Isolation checklist

The two installs share exactly two things: the Kubernetes API server, and one cluster-scoped CRD
bundle. Everything else is duplicated.

```mermaid
flowchart TB
    subgraph shared["Shared CRDs, one owner"]
        CRDs["CRD schema bundle<br/><i>applied once, by a platform or GitOps owner</i>"]
    end

    subgraph v1["Installation orka-compat<br/>namespace orka-compat-system"]
        direction TB
        C1["controller<br/><code>mode=harness-v1</code>"]
        L1["Lease 03b49a10.orka.ai"]
        S1[("v1 SQLite / PVC")]
        D1["wrapper Service + ledger"]
    end

    subgraph v2["Installation orka<br/>namespace orka-system"]
        direction TB
        C2["controller<br/><code>mode=harness-v2</code>"]
        L2["Lease 03b49a10.orka.ai"]
        S2[("v2 SQLite / PVC")]
        R2["ACP runtimes<br/><i>namespace orka-runtimes</i>"]
    end

    CRDs -.->|schema only| v1
    CRDs -.->|schema only| v2

    v1 x--x|"NetworkPolicy + namespaced RBAC<br/>must forbid this"| v2

    style shared fill:#f3f0ff,stroke:#7048e8
    style v1 fill:#fff4e6,stroke:#d9822b
    style v2 fill:#eaf4ff,stroke:#2b7bd9
```

The leader-election ID is hardcoded to `03b49a10.orka.ai` in both installs, but each Lease lives in
its own watched namespace — so the two controllers never contend for the same lock, and never
coordinate over one Task population.

Use different values for every release-owned resource:

| Boundary | Harness v1 | Harness v2 |
| --- | --- | --- |
| Helm installation name | `orka-compat` | `orka` |
| `controller.mode` | `harness-v1` | `harness-v2` |
| Namespace and `controller.watchNamespace` | `orka-compat-system` | `orka-system` |
| Controller API | v1-specific Service/endpoint | v2-specific Service/endpoint |
| Leader election | Lease in `orka-compat-system` | Lease in `orka-system` |
| State | v1-only SQLite/PVC/backups | v2-only SQLite/PVC/backups |
| Data plane | Wrapper Service and ledger | Dedicated ACP runtime namespace |
| Identity | v1 ServiceAccounts and Secrets | v2 ServiceAccounts and Secrets |

Set `controller.acpRuntime.namespace=orka-runtimes` for the default installation.

NetworkPolicies and namespace-scoped RBAC must prevent either controller from
reading or writing the other watched namespace. A namespace-scoped controller
cache is not an authorization boundary by itself.

The supported Helm topology co-locates each controller and its watched objects
in the release namespace. The chart rejects a different
`controller.watchNamespace`, which keeps all namespaced RBAC and release-owned
data-plane resources inside one boundary. Harness v2 still uses a separate
runtime namespace.

The v2 release owns cluster-scoped gateway and workspace-provider reconcilers.
Do not enable those reconcilers in v1. Install common cluster-scoped admission
resources once.

## Manage CRDs once

CRDs are cluster-scoped, so both installations use one schema bundle capable of
storing the supported v1 and v2 shapes. Designate a platform or GitOps owner,
and have that owner apply the CRDs from the selected chart. From a source checkout
matching the chart, the owner can run:

```bash
export ORKA_CHART='<path-to-chart.tgz>'
scripts/apply-helm-crds.sh "${ORKA_CHART}" "${ORKA_CONTEXT}"
```

Use `--skip-crds` for both Helm installations. Each still needs complete settings
for its images, Secrets, proxy, Publisher, storage, and selected mode.
The table above covers the settings that keep the installations separate.
Do not let both installations manage CRDs independently. Helm does not update `crds/` during
`helm upgrade`.

Keep each installation's mode and watched namespace unchanged, including when
recreating a deleted controller. Changing modes requires a separate installation.
Orka currently supports new installations only. See [Upgrading](upgrading.md)
for support limits.

## Route new work explicitly

Producers choose an installation by its API endpoint and watched namespace.
During v2 rollout:

1. leave existing v1 Tasks and Sessions on the v1 endpoint;
2. install v2 in its fresh namespaces and run new v2 canaries;
3. verify cross-namespace API access is forbidden;
4. point selected producers at the v2 endpoint;
5. create new v2 Agents, Tasks, and Sessions.

Unavailable v2 capacity fails closed. It never sends work to the v1 wrapper.

## No protocol migration

Do not:

- patch a v1 Agent, AgentRuntime, or Task into v2;
- reuse a v1 controller PVC, database, wrapper ledger, or Session in v2;
- copy a transcript and claim continuation of the original Session;
- ask one controller to cancel, settle, publish, finalize, or clean up the
  other controller's work;
- change `controller.mode` or `orka.ai/controller-mode` on an existing
  installation.

You may copy non-secret configuration into a newly created object in the other
installation. It has a new namespace, UID, Session lineage, attempt history,
and external-effect history; it is not migrated work.

## Drain and retire v1

Draining v1 is an operational procedure, not a controller mode:

1. stop v1 API ingress and every internal or external v1 producer;
2. revoke permissions that create v1 agent Tasks;
3. record a cutoff and inventory queued, active, finalizing, and cleanup work;
4. let existing work settle through v1, preserving unknown outcomes where
   acceptance cannot be disproved;
5. repeat uncached inventory until v1 execution, Session settlement,
   finalizers, and wrapper-ledger cleanup reach zero;
6. back up retained v1 history and state;
7. remove v1 workloads and revoke their credentials;
8. delete PVCs or historical data only under a separate retention decision.

The wrapper's authenticated Pod-template drain remains required before a
wrapper upgrade. It protects in-memory v1 turns and is unrelated to controller
mode or v2.

## Rollback

Rollback changes only future routing. Stop new submissions to v2 and, if the
v1 installation is still intentionally open, submit replacement work there as
new v1 Tasks. Existing v2 work remains owned by v2 and must settle or be
canceled through v2.

Keep the shared CRD superset while either v1 or v2 objects remain. Restore each
installation only from its own coordinated Kubernetes and persistent-state
backup; never restore one mode's state into the other.

The full architecture and release gates are in the
[isolated coexistence plan](https://github.com/orka-agents/orka/blob/main/docs/harness-v1-v2-coexistence-plan.md).
