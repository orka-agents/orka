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

## CRD update ordering {#the-one-thing-that-will-bite-you}

CRDs define Orka's Kubernetes resource types. Helm installs them on a fresh
install but does not update them during `helm upgrade`. Apply target CRDs before
starting the target controller. When removing the in-tree workspace providers,
first drain the old allocations through their original owner, as described below.
Applying the pruned RuntimePool schema before that drain can discard settings
needed by the old owner to clean up.

## External workspace migration {#external-workspace-migration}

The external-provider core binary does not allocate, suspend, resume, or delete
old in-tree Agent Sandbox or Substrate ACP workspaces. It does not adopt their
resources. Keep the original core binary, backend access, credentials, worker
RBAC, and NetworkPolicies available while draining them.

Before applying the pruned RuntimePool CRD or replacing the core binary:

1. Stop new legacy workspace demand and settle active Tasks and RuntimeSessions
   through the original controller.
2. Drain and delete every legacy RuntimePool and retained ExecutionWorkspace,
   including records in `Ready`, `Suspended`, `Failed`, and `Deleted` states.
   Allow the original owner to observe exact Pod/native termination and storage
   cleanup before its finalizers complete.
3. Retire legacy checkpoint references, catalogs, and template journals through
   their original owner. Preserve required data separately before destructive
   cleanup. Do not strip finalizers or relabel an allocation to transfer ownership.
4. Verify no old allocations or retained records remain, then apply the target
   CRDs and start the new core. Install external provider CRDs, registrations,
   immutable profiles, and Deployments as separate components.

Stock core startup lists RuntimePools and legacy-labelled ExecutionWorkspaces
across all namespaces. It rejects every remaining legacy workspace state and
names the blockers before starting dispatch. A pruned pool missing its generic
external binding also blocks startup. The gate does not mutate provider status,
seed runtime credentials, recreate compute, or perform native cleanup. Return to
the original owner to finish the drain when it rejects startup.

Tasks now select `execution.workspace.classRef` only. Provider-specific Task
selectors, `RuntimeProviderConfig`, `RuntimeWorkspaceProfile`, and the old
Sandbox/Substrate ACP flags are removed. Install the selected provider's own
config/profile kinds instead. Native MCP Tools remain separate and use
`--substrate-mcp-tools-enabled`; their control credentials are not external ACP
provider credentials.

The [local upgrade proof](https://github.com/orka-agents/orka-workspace/blob/main/scripts/external-workspace-upgrade-e2e.sh)
uses actual API objects and a released stock core image. It covers old-shaped
pools, all retained workspace states, schema pruning, unchanged legacy identities,
and fresh startup after exact synthetic cleanup. This scoped migration proof does
not establish general release-to-release or database restore support.

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
executionworkspaces,executionworkspacecheckpoints \
  -o json > orka-workspace-crs.json
```

Export provider-owned configuration and profile resources from each installed
provider group as well, such as `SandboxProviderConfig`, `SandboxWorkspaceProfile`,
`SubstrateProviderConfig`, and `SubstrateWorkspaceProfile`. They are installed by
the separate providers. Before migrating an old installation, also back up its
legacy `RuntimeProviderConfig` and `RuntimeWorkspaceProfile` records for the
original owner's drain.

Leave out any kind your cluster does not have; `kubectl` fails the whole command on an
unknown resource rather than skipping it.

:::danger[Do not copy the SQLite file from a running controller]
Copying `orka.db` while the controller is writing can lose records.
Snapshot the whole data volume, or stop the controller before copying it.
:::

### 2. Drain legacy workspaces, then apply the target CRDs

If the target removes in-tree ACP workspace providers, complete the
[external workspace migration drain](#external-workspace-migration) first.
Then, from a checkout matching the version you are upgrading to:

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

The snapshot key also seals linked accounts (connectors). When the controller
starts, it checks that its key opens every retained execution snapshot and
every linked account's sealed custody, and it refuses to start if one does
not: restore the previous key. Orka has no tool yet that re-wraps snapshots
or custody under a new key, so treat the key as fixed for the life of the
release. Disconnecting every linked account does not make a key change safe:
retained execution snapshots still need the old key.

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
