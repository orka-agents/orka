---
slug: /upgrading
description: "Back up Orka, update its Kubernetes resource definitions, and check the result."
---

# Upgrading

Orka supports in-place SQLite upgrades from the exact layouts listed below.
This is a narrow database compatibility guarantee, not support for every older
release, controller mode, or deployment topology. Follow
[Install Orka](installation.md) for new installations.

## What release checks cover {#release-checks}

The release checks install the chart after creating CRDs and internal Secrets
separately. They restart the controller using the same data and encryption key,
confirm that tasks still work, and check that Helm blocks a change to
`controller.mode`. Results are in the release's `acceptance.json`.

These checks do not test Helm's automatic CRD and Secret creation.
Broader coverage for upgrades between versions and restoring a lost installation
from backups is tracked in
[#499](https://github.com/orka-agents/orka/issues/499) and
[#505](https://github.com/orka-agents/orka/issues/505).

## Supported database layout

Orka creates its SQLite database on first startup. Restarts reuse that database
and keep its records. Keep the data volume and encryption key together.

The controller compares the complete schema, including every table and index
definition, before upgrading. These immutable main-branch revisions identify
supported source layouts; they are **unreleased baselines**, not release-version
compatibility promises. Other commits with the exact same layout are equivalent.
Object counts alone do not establish compatibility.

| Source layout | Representative revision | Transition |
| --- | --- | --- |
| 149 objects | [`0e567418`](https://github.com/orka-agents/orka/commit/0e567418ae20b4a11b5684eb42da47c1cdf1c92e) | Add the GitHub mutation index, then empty connector and native-session tables and their indexes. |
| 150 objects | [`1f18614c`](https://github.com/orka-agents/orka/commit/1f18614ca9dd2339a9c8caac8e3320e7daf61c2e), #731 | Add empty connector and native-session tables and their indexes. |
| 162 objects | [`57583975`](https://github.com/orka-agents/orka/commit/57583975d265a1ca23ebc8106d8fe769c84ae4f3), #701 | Add empty native-session tables. |
| 166 objects (current) | [`abd2c0406ab660abd37af7c176b4eef610a19ce1`](https://github.com/orka-agents/orka/commit/abd2c0406ab660abd37af7c176b4eef610a19ce1), #732 | Validate only; no migration or automatic backup. |

### Unsupported intermediate connector layout

The **161-object layout from #700**, introduced at
[`b5265055`](https://github.com/orka-agents/orka/commit/b526505518fa139848cc610f6055b33e919ce594),
is explicitly unsupported. The subsequent #701 changes the existing
`connector_credentials` and `connector_retired_credentials` table definitions as
well as adding a table. This cannot be handled by adding new empty tables alone.
The controller refuses this exact layout even if its connector tables are empty,
names #700 and its revision in the error, and points here. **No rewrite path is
provided.** Do not change the schema or delete custody records to force acceptance.

Unknown, partially modified, and newer layouts are also refused. If startup
reports `unsupported SQLite schema`, preserve the database and stop. Orka does
not reset or replace it. In-memory initialization and the low-level `NewDB`
constructor do not migrate; production startup uses `OpenLockedStore` to hold the
exclusive controller lock throughout backup and upgrade.

### Automatic pre-upgrade backup

For a recognized older layout, startup creates a consistent SQLite backup beside
the database before applying any DDL. With the default `--store-path`, its name is
`/data/orka.db.pre-upgrade-<random>.db`. Startup logs the exact path. The backup
includes committed WAL records, retains the original schema and rows, is created
with owner-only permissions (`0600`), and is verified and synced before migration.
If backup creation or verification fails, startup stops without changing schema.
Only an incomplete backup created by that attempt is removed.

All additive steps run in one `BEGIN IMMEDIATE` transaction under the controller
lock. Each resulting layout is checked exactly, and integrity and foreign-key
checks must pass before commit. A process interrupted before commit leaves the
old schema; retry takes a new backup and repeats the migration. A committed
current layout only validates on reopen. Completed backups remain available even
if a later migration step fails; they are not overwritten or automatically
pruned, so allow disk space for them and manage retention explicitly.

Backups contain the same sensitive records as the live database. Keep them
protected with the original encryption key, include them in your backup policy,
and account for retained credential material when deleting old backups. The
adjacent backup is **not** a replacement for the independent volume, Kubernetes
state, and Secret backups below.

Migration is forward-only. An older image cannot necessarily open the upgraded
store. Rolling back an installation requires a coordinated, offline restoration
of the database, original key, configuration, and Kubernetes state—not just an
image downgrade. This page does not qualify a general restore procedure.

### Controller startup allowance

Database backup, integrity checks, and migration run before the controller starts
serving `/healthz`. Both deployment sources include a startup probe that allows
about ten minutes for the entire initialization (60 failed probes, ten seconds
apart, with a one-second request timeout). Kubernetes holds off the existing
liveness and readiness probes until startup succeeds; their checks are unchanged.

Before upgrading a large database or a slow persistent volume, increase the
budget as needed. Helm exposes `controller.startupProbe.failureThreshold`,
`periodSeconds`, and `timeoutSeconds`; for example,
`--set controller.startupProbe.failureThreshold=120` allows about twenty minutes
at the default ten-second interval. For Kustomize, patch the manager container's
`startupProbe` timing fields in your overlay. The allowance is bounded: exceeding
it can restart initialization and create another retained backup, so size the
budget and free disk space before the upgrade.

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
