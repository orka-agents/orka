# Orka Helm chart

## Install

Follow [Install Orka](https://orka-agents.github.io/orka/docs/installation) for a
complete Helm installation. Published charts are available from
[GitHub Releases](https://github.com/orka-agents/orka/releases) and the Helm
repository at `https://orka-agents.github.io/orka/charts`.

For development, use `manifest_staging/charts/orka` with images built from the
same source checkout. Development charts use version `0.0.0-dev` and require
explicit controller, publisher, and worker image tags or digests. Runtime
providers remain disabled until their image references are configured. Do not
pair a development chart with older release images.
See [Build from source](https://orka-agents.github.io/orka/docs/build-from-source).

## Values

[values.yaml](values.yaml) contains the chart defaults. Pass your settings to
Helm with `--values <file>` or `--set-string`.

A published release chart needs no image overrides. The chart generates its own encryption key
and webhook certificate. The settings you are most likely to change later:

| Setting | Notes |
| --- | --- |
| `controller.image`, `publisher.image`, `workers.*.image` | Published charts default to the release tag; development charts require an explicit image. Set `tag` or `digest`; a digest takes precedence. |
| `controller.acpRuntime.*Image` | Published charts default to release tags; development charts leave providers disabled. Set a full tagged or digest reference to enable a runtime. |
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

- The chart packages production CRDs under `crds/`. Development-only fake
  workspace CRDs are excluded.
- Helm creates these CRDs during installation, but does not update them during
  `helm upgrade` or delete them during `helm uninstall`.
- Use `--skip-crds` only when another workflow manages compatible Orka CRDs.
- Uninstall removes chart-managed PVCs. Depending on the volume reclaim policy,
  this can delete stored data. Back up before uninstalling.

See [Upgrading](https://orka-agents.github.io/orka/docs/upgrading) for upgrade
support, CRD updates, and uninstall instructions.

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
