# Orka Helm chart

## Install

Follow [Install Orka](https://orka-agents.github.io/orka/docs/installation) for a
complete Helm installation. Published charts are available from
[GitHub Releases](https://github.com/orka-agents/orka/releases) and the Helm
repository at `https://orka-agents.github.io/orka/charts`.

For development, use `manifest_staging/charts/orka`. It selects `0.0.0-dev`, the
latest validated `main` image set published by the Development Images workflow.
Tagged controller, publisher, and native worker images pull on each Pod start;
republishing a tag does not restart existing Pods. Coding-runtime tags resolve
to digests at controller startup.

For a feature branch or older checkout, override every image with a matching
commit-specific tag or digest. The workflow publishes `sha-<full-commit>` tags
and a `development-images-<full-commit>` artifact containing `images.json` and
`values.json`. Pass the latter with `--values` to pin all nine images together.
See [Build from source](https://orka-agents.github.io/orka/docs/build-from-source).

## Values

[values.yaml](values.yaml) contains the chart defaults. Pass your settings to
Helm with `--values <file>` or `--set-string`.

A published release chart needs no image overrides. The chart generates its own encryption key
and webhook certificate. The settings you are most likely to change later:

| Setting | Notes |
| --- | --- |
| `controller.image`, `publisher.image`, `workers.*.image` | Published charts default to the release tag; development charts use `0.0.0-dev`. Set `tag` or `digest`; a digest takes precedence. |
| `controller.acpRuntime.*Image` | Published charts default to release tags; development charts use `0.0.0-dev`. Set a full tagged or digest reference, or an empty string to disable a provider. |
| `controller.agentExecutionSnapshot.existingSecret` | Empty by default; the chart generates the snapshot encryption Secret once and keeps it on uninstall. Set only to bring your own key. Immutable after install. |
| `webhooks.tls.existingSecret`, `.caBundle`, `.caInjectionAnnotations` | Empty by default; the controller issues and renews a self-signed webhook certificate. Set to bring your own certificate or use cert-manager. |
| `providerProxy.enabled`, `.upstreamBaseURL` | Off by default. Enable and name your model gateway's in-cluster Service to connect built-in coding agents; the chart derives the egress rule from that Service. Set `.egress` yourself for anything else. See [Provider proxy](https://orka-agents.github.io/orka/docs/provider-proxy). |
| `controller.watchNamespace` | The one namespace Orka watches. Defaults to the Helm namespace and cannot change after installation. |

Use a distinct Helm release name and namespace for each installation in a cluster.
Each installation also needs its own `controller.acpRuntime.namespace`.
`service.port` sets the controller Service port; `controller.apiPort` sets its
container listener and Service target port.

See the [Helm values reference](https://orka-agents.github.io/orka/docs/configuration#helm-chart)
for all settings and Secret rotation, and [Security](https://orka-agents.github.io/orka/docs/security#scm-proxy-networkpolicy-limits)
for NetworkPolicy limits.

The chart does not install or choose a model gateway. AI-worker tasks use
Provider resources you create after installation. Built-in coding agents need
the provider proxy connected to a gateway you manage.

Runtime tags are resolved to digests at controller startup. This requires HTTPS
access to a registry that allows anonymous pulls. Use digest references for
private registries or installations without registry access from the controller.

## CRDs and Helm lifecycle

- The chart packages core and shared workspace CRDs under `crds/`. Provider
  configuration CRDs and compute controllers are installed separately.
- Helm creates these CRDs during installation, but does not update them during
  `helm upgrade` or delete them during `helm uninstall`.
- Drain every legacy ACP allocation under the old release before applying the
  RuntimePool schema or upgrading the controller. Retained legacy objects block
  startup even after their native settings are pruned.
- Use `--skip-crds` only when another workflow manages compatible Orka CRDs.
- Uninstall removes chart-managed PVCs except the controller store PVC, which is
  annotated `helm.sh/resource-policy: keep`. Depending on the volume reclaim
  policy, removing a PVC can delete stored data. Back up before uninstalling.

See [Upgrading](https://orka-agents.github.io/orka/docs/upgrading) for upgrade
support, CRD updates, and uninstall instructions.

## Remote memory control plane

`controller.memoryBackend.enabled` is disabled by default. Enabling it requires a
stable `controller.memoryBackend.clusterId`. On an upgrade, apply the exact
target `MemoryBackend` CRD first; Helm does not upgrade files under `crds/`. The
controller keeps the feature disabled until the CRD schema marker is observed
unless `crdsReadyOverride=true` is used after independent verification.

`controller.memoryBackend.activationEnabled` is the separate second-release
cutover gate. The foundation chart and controller artifacts reject activation
even when this value or the matching runtime environment variable is forced. The foundation
controller advertises feature epoch 1; only a later source-gated activation
artifact may advertise epoch 2 and accept the activation value. The SQLite
authority requires `controller.replicas: 1`, and the controller Deployment uses
`Recreate`, so the foundation replica stops before the activation artifact
starts. Activation also requires durable evidence that a lower feature epoch was
previously observed and that every live heartbeat supports the activation epoch.
Creating `MemoryBackend/default` in `Staged` validates without changing SQLite
authority. Activation, decommission, force-orphan, and restore-legacy remain
explicit audited API/CLI actions. Dispatcher concurrency, sustained rate, and
burst controls are configured under `controller.memoryBackend.dispatcher*`; the
defaults bound both global and per-namespace work.

The chart always installs fail-closed admission policies that reserve Orka task
Job/Pod provenance for the owning controllers and require
`memorybackends/finalizers` update authorization before the backend protection
finalizer can be removed. Do not disable these policies or grant their protected
status/finalizer permissions to untrusted namespace writers.

Helm uses release-scoped pre-install/pre-upgrade RBAC and provenance policy hooks
before rolling the controller Deployment, then installs the retained
steady-state policies. This closes the Helm kind-ordering gap while allowing the
old controller and Kubernetes Job controller to create legacy-format work during
the upgrade. Helm removes the release-scoped preflight RBAC after each successful
hook run. If a later hook aborts the release, pre-delete hooks replace any
leftover grants with inert, subject-free/rule-free tombstones during uninstall;
Helm versions may then remove those tombstones after the cleanup hook completes.
Intentionally retained steady-state controls remain in place. The raw installer
likewise places the task provenance policies and bindings after their RBAC
grants but before any Deployment.

`store.persistence.existingClaim` may select an operator-managed controller
store PVC; a chart-created store PVC is retained on uninstall. The
MemoryBackend finalizer policy and binding are also retained on uninstall so
surviving `MemoryBackend` objects cannot have their lifecycle barrier stripped.
Delete retained resources manually only after every backend is safely
decommissioned or force-orphaned and a matched recovery set is verified.

Example foundation values:

```yaml
controller:
  memoryBackend:
    enabled: true
    activationEnabled: false
    clusterId: production-cluster-a
    crdsReadyOverride: true # only after separately applying/verifying target CRDs
```

Orka ships the provider-neutral `orka.memory.v1` protocol and conformance
harness, but provider adapters, their Helm charts, image lifecycles, and live
provider release gates are maintained and deployed independently.
The Orka chart never creates an adapter or a `MemoryBackend`. Install the chosen
adapter separately, expose it through a public HTTPS endpoint, bind an
operator-managed client-auth Secret, and stage/activate the backend explicitly.

## Chart development

Edit chart inputs under `cmd/build/helmify/static`, then run this from the
repository root:

```bash
make manifests
```

This regenerates `manifest_staging/charts/orka`. Do not edit generated chart
copies directly. Release preparation promotes the staged chart to `charts/orka`.
See the [generator README](https://github.com/orka-agents/orka/blob/main/cmd/build/helmify/README.md)
for the generation process.
