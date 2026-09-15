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
Helm with `--values <file>` or `--set-string`.

A new installation needs only these values:

| Setting | What to set |
| --- | --- |
| `webhooks.tls.existingSecret` | The TLS Secret for the admission webhooks. |
| `webhooks.caBundle` | The base64 CA certificate that signed it, or configure `webhooks.caInjectionAnnotations` for cert-manager. |

Everything else has a working default. The settings you are most likely to
change later:

| Setting | Notes |
| --- | --- |
| `controller.image`, `publisher.image`, `workers.*.image` | Use the release tag by default. Set `tag` to choose another tag, or `digest` to pin an image. A digest takes precedence over the tag. |
| `controller.acpRuntime.*Image` | Use release tags by default. Override with a full tagged or digest reference; set an empty string to disable a runtime. |
| `controller.agentExecutionSnapshot.existingSecret` | Empty by default; the chart generates the snapshot encryption Secret once and keeps it on uninstall. Set only to bring your own key. Immutable after install. |
| `providerProxy.enabled`, `.upstreamBaseURL`, `.egress` | Off by default. Enable to connect built-in coding agents to your model gateway. See [Provider proxy](https://orka-agents.github.io/orka/docs/provider-proxy). |
| `controller.mode` | `harness-v2` for new installations. It cannot change after installation. |
| `controller.watchNamespace` | Defaults to the Helm namespace, which needs a matching `orka.ai/controller-mode` label. |

Use a distinct Helm release name and namespace for each installation in a cluster.
Each `harness-v2` installation also needs its own `controller.acpRuntime.namespace`.
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
