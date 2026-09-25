---
slug: /upgrading
description: "Back up Orka, update its Kubernetes resource definitions, and check the result."
---

# Upgrading

Orka currently supports new installations only. Upgrades between versions are
not yet supported. Follow [Install Orka](installation.md) for setup.

## What release checks cover {#release-checks}

The release checks install the chart after creating CRDs and internal Secrets
separately. They restart the controller using the same data and encryption key,
confirm that tasks still work, and check that Helm blocks a change to
`controller.mode`. Results are in the release's `acceptance.json`.

These checks do not test Helm's automatic CRD and Secret creation.
Tests for upgrades between versions and restoring a lost installation from
backups are tracked in
[#499](https://github.com/orka-agents/orka/issues/499) and
[#505](https://github.com/orka-agents/orka/issues/505).

## Supported database layout

Orka creates its SQLite database on first startup. Restarts reuse that database
and keep its records. Keep the data volume and encryption key together.

If startup reports `unsupported SQLite schema`, preserve the database and stop.
Orka does not convert, reset, or replace an incompatible database.

## Update CRDs first {#the-one-thing-that-will-bite-you}

CRDs define Orka's Kubernetes resource types. Helm installs them on a fresh
install, but does not update them during `helm upgrade`. Apply the target
chart's CRDs before updating Orka, or Kubernetes may drop new fields.

## Upgrade steps

Use these steps only when the target release publishes a tested upgrade
procedure for your installed version.

Use a host with Bash, Helm, kubectl, and jq installed. Choose the target chart and
Kubernetes context before taking backups:

```bash
export TARGET_CHART='<path-or-reference-to-target-chart>'
export TARGET_CONTEXT='<kubeconfig-context>'
```

Use that context for the backup, CRD update, upgrade, and verification commands below.

### 1. Back up first

Back up both Kubernetes state and the controller's volume before upgrading:

- The controller's data volume, which holds its SQLite database.
- Kubernetes resources and Secrets, including configuration and the records
  Orka uses to track running tasks and published changes.

The JSON exports below provide additional records for inspection and configuration
reference. They complement your cluster's backup system.

```bash
kubectl --context "$TARGET_CONTEXT" -n orka-system get \
  agents,providers,tools,skills,tasks,repositorymonitors,repositoryscans,\
outboundaccesspolicies,gateways,gatewaybindings,agentruntimes,substrateactorpools \
  -o json > orka-crs.json

# Cluster-scoped, so no -n:
kubectl --context "$TARGET_CONTEXT" get gatewayclasses -o json > orka-gatewayclasses.json
```

For coding-agent tasks, also export the records Orka uses to track work:

```bash
kubectl --context "$TARGET_CONTEXT" -n orka-system get \
  runtimepools,controllerepochs,promptattempts,runtimesessioncontrols,\
publications,externaleffects \
  -o json > orka-acp-control-state.json

# BranchClaims are cluster-scoped:
kubectl --context "$TARGET_CONTEXT" get branchclaims -o json > orka-branchclaims.json
```

:::warning[JSON exports are not a full backup]
These files cannot safely restore running tasks or prevent repeated operations.
Do not reapply controller-owned records with `kubectl apply` to rebuild a lost
installation. This procedure keeps the existing resources and volumes in place.
:::

If you have the workspace provider API enabled, back up its resources too:

```bash
kubectl --context "$TARGET_CONTEXT" -n orka-system get \
  executionworkspaceclasses,executionworkspaceproviders,executionworkspacepools,\
executionworkspaces,executionworkspacecheckpoints,runtimeproviderconfigs,runtimeworkspaceprofiles \
  -o json > orka-workspace-crs.json
```

Include the `RuntimeProviderConfig` and `RuntimeWorkspaceProfile` resources.
Workspace classes need them to run.

Leave out any kind your cluster does not have; `kubectl` fails the whole command on an
unknown resource rather than skipping it.

:::danger[Do not copy the SQLite file from a running controller]
Copying `orka.db` while the controller is writing can lose records.
Snapshot the whole data volume, or stop the controller before copying it.
:::

### 2. Apply the target CRDs

From a checkout matching the version you are upgrading to:

```bash
scripts/apply-helm-crds.sh "$TARGET_CHART" "$TARGET_CONTEXT"
```

The [script](https://github.com/orka-agents/orka/blob/main/scripts/apply-helm-crds.sh)
applies the exact CRD definitions from the chart and waits until Kubernetes
accepts them. It removes fields omitted by the target chart and stops if
another writer changes a CRD during the update.

If a separate platform team or GitOps system owns CRDs in your cluster, do this step
through that system instead, wait for every Orka CRD to become `Established`, then
continue. Do not run two CRD apply workflows against one cluster.

### 3. Upgrade

```bash
helm upgrade orka "$TARGET_CHART" --kube-context "$TARGET_CONTEXT" \
  --namespace orka-system --wait --timeout 10m
```

On Azure Kubernetes Service, the admission controller adds namespace selectors to webhooks. If Helm 4 reports
an apply conflict with `admissionsenforcer` on those selectors, add `--server-side=false`
to the upgrade command. Client-side updates preserve those added selectors.

Keep the timeout longer than the controller's termination grace period plus time for
the replacement Pod to become Ready. The harness-v2 default grace period is six minutes;
increase the timeout if you configure a longer drain or need more rollout time.

### 4. Verify

```bash
# A Helm release named `orka` creates a Deployment named `orka-controller`.
kubectl --context "$TARGET_CONTEXT" -n orka-system rollout status deploy/orka-controller
kubectl --context "$TARGET_CONTEXT" get crd -o name | grep '\.orka\.ai$' | wc -l
```

The CRD count should match the target chart.
Also check the pools that run coding agents:

```bash
kubectl --context "$TARGET_CONTEXT" -n orka-system get runtimepools
```

Submit one small Task and confirm it reaches `Succeeded`.

## Values you cannot change on upgrade

Choose these settings during installation. Helm blocks later changes because
existing tasks and data depend on them:

| Value | Why it is fixed |
| --- | --- |
| `controller.mode` | Determines how this installation runs coding agents. |
| `controller.watchNamespace` | Existing Tasks live there. |
| `controller.agentExecutionSnapshot.existingSecret` and `.key` | Saved agent configuration needs the original encryption key. A generated key cannot be swapped for your own later, or the reverse. |
| `controller.acpRuntime.namespace` | Running pools live there. |
| The release fullname | Every owned resource is named from it. |

## `--skip-crds`

Use `--skip-crds` only when a platform team or GitOps system already manages
Orka's CRDs for the cluster. Otherwise, let Helm create them.

If you uninstalled a previous release, update its retained CRDs first, then install the
replacement with `--skip-crds`.

## Uninstall

:::danger[Uninstall can delete stored data]
Helm deletes the chart's persistent volume claims, including `orka-store` and
`orka-workspace-publisher`. If their volumes use the `Delete` reclaim policy,
Kubernetes also deletes the stored data. Back up the data and encryption key,
and verify your recovery plan before uninstalling.
:::

```bash
helm uninstall orka --kube-context "$TARGET_CONTEXT" --namespace orka-system
```

The CRDs and their custom resources stay in the cluster, and so does the generated
snapshot key Secret, `orka-agent-execution-snapshot`, so a restored data volume stays
readable. Keeping them does not preserve the data stored in volumes.

:::danger[Deleting a CRD deletes its data]
Deleting a CRD also deletes every resource of that type across the cluster.
Keep CRDs until their data is no longer needed or has been backed up.
:::
