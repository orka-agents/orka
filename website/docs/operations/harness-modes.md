---
description: "Namespace and resource ownership for a harness v2 installation."
---

# Installation ownership

Orka runs every agent Task through `orka.harness.v2`. Built-in coding agents use
controller-owned RuntimePools and RuntimeSessions. Externally registered
AgentRuntimes use the same authenticated protocol and execution fences.
`controller.mode` is fixed to `harness-v2`; unsupported protocol settings fail
validation before work starts.

## Namespace identity

Each controller watches exactly one non-empty namespace. Create that namespace
with its installation label before starting the controller:

```bash
kubectl create -f - <<'EOF'
apiVersion: v1
kind: Namespace
metadata:
  name: orka-system
  labels:
    orka.ai/controller-mode: harness-v2
EOF
```

A missing or incompatible label fails startup. Existing unlabeled namespaces
cannot be adopted in place. Do not relabel a namespace to take resources from
another installation.

The Helm release namespace must equal `controller.watchNamespace`.
`controller.acpRuntime.namespace` must name a different namespace dedicated to
that installation's runtime Pods. The controller's leader-election Lease lives
in its watched namespace, and leader election is required.

The [installation guide](../getting-started.md#install) includes the image
digests, provider proxy, webhook certificate, snapshot key, and storage needed
for a runnable deployment. The guarded Kustomize deployment uses
`config/acp-production` and verifies namespace identity before writing resources.

## Resource ownership

Run one controller release per watched namespace. Its API Service, worker
ServiceAccounts, RBAC, SQLite volume, Secrets, and runtime resources belong to
that installation. Namespace-scoped caching does not replace RBAC or
NetworkPolicy.

For multiple installations on one cluster, use distinct release fullnames,
watched namespaces, runtime namespaces, endpoints, databases, and credentials.
Keep their Tasks and Session lineages separate. Configure RBAC and NetworkPolicy
to prevent either controller from reading or changing another installation's
work.

CRDs are cluster-scoped. Designate one platform or GitOps owner to apply the
matching schema bundle, and use `--skip-crds` for releases whose CRDs that owner
already manages. Shared cluster admission resources also need one owner;
individual releases must not race to update them.

## Upgrade identity

An in-place upgrade preserves the namespace claim, controller watch namespace,
chart fullname, runtime namespace, and snapshot encryption Secret name and key.
The chart checks those values against the live controller. A deleted controller
can be recreated under its retained namespace claim and persistent state.

Use the [upgrade procedure](upgrading.md) to apply the target CRDs and drain
runtime work before the replacement controller starts. Keep existing v2 Session
records, execution snapshots, and recovery information. Uncertain execution
outcomes must remain uncertain until recovery can prove their result; do not
resubmit them automatically.

Changing an installation's ownership boundary requires a new release and new
resources. Orka does not convert old Tasks or Sessions or backfill execution
history into another installation.
