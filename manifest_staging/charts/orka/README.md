# Orka Helm chart

## Install

Follow [Install Orka](https://orka-agents.github.io/orka/docs/installation) for a
complete Helm installation. Published charts are available from
[GitHub Releases](https://github.com/orka-agents/orka/releases) and the Helm
repository at `https://orka-agents.github.io/orka/charts`.

For development, use `manifest_staging/charts/orka` from a source checkout.
See [Build from source](https://orka-agents.github.io/orka/docs/getting-started#option-b-current-main-from-source).

## Values

[values.yaml](values.yaml) contains the chart defaults. Pass your settings to
Helm with `--values <file>`.

| Setting | Chart requirement |
| --- | --- |
| `controller.mode` | Use `harness-v2` for new installations. It cannot change after installation. |
| `controller.watchNamespace` | Defaults to the Helm namespace. That namespace needs a matching `orka.ai/controller-mode` label. |
| `controller.image`, `publisher.image`, `workers.*.image` | Use the release tag by default. Set `tag` to choose another tag, or `digest` to pin an image. A digest takes precedence over the tag. |
| `controller.acpRuntime.*Image` | Use release tags by default. Override with a full tagged or digest reference; set an empty string to disable a runtime. |
| `controller.agentExecutionSnapshot.existingSecret`, `.key` | Reference an existing encryption-key Secret. |
| `webhooks.tls.existingSecret` | Reference an existing webhook TLS Secret. Set `webhooks.caBundle` or configure `webhooks.caInjectionAnnotations` for CA injection. |
| `providerProxy.enabled` | Defaults to `true`, as required for `harness-v2`. |
| `providerProxy.upstreamBaseURL` | Use `http://vekil.vekil-system.svc:1337`. The chart's network policies require this endpoint. |

Use a distinct Helm release name and namespace for each installation in a cluster.
Each `harness-v2` installation also needs its own `controller.acpRuntime.namespace`.
`service.port` sets the controller Service port; `controller.apiPort` sets its
container listener and Service target port.

See the [Helm values reference](https://orka-agents.github.io/orka/docs/configuration#helm-chart)
for further settings and Secret rotation, and [Security](https://orka-agents.github.io/orka/docs/security#scm-proxy-networkpolicy-limits)
for NetworkPolicy limits.

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
