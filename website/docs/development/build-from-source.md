---
slug: /build-from-source
description: "Build Orka's images from a source checkout and install the chart they produce."
---

# Build from source

## What you need

This path builds and pushes images from your checkout and installs the chart
generated from the same source. Use it to develop Orka; to try Orka, follow
[Install Orka](../operations/installation.md) instead.

In addition to the [installation prerequisites](../operations/installation.md#before-you-start):

- Go, Bun, and Docker — see [Development](development.md#prerequisites) for versions
- A container registry you can push to and your cluster can pull from

## Build and push

```bash
export ORKA_CONTEXT='<your-kubeconfig-context>'
git clone https://github.com/orka-agents/orka.git
cd orka
```

Choose a registry prefix you can push to and your cluster can pull from. Replace
`ghcr.io/your-org/orka` below, authenticate Docker to that registry, and run the build,
push, and install commands in the same shell. Use a fresh tag when rebuilding with local
source changes.

```bash
export ORKA_IMAGE_PREFIX=ghcr.io/your-org/orka
export ORKA_IMAGE_TAG="dev-$(git rev-parse --short=12 HEAD)"
export IMG="${ORKA_IMAGE_PREFIX}:${ORKA_IMAGE_TAG}"
export AI_WORKER_IMG="${ORKA_IMAGE_PREFIX}/ai-worker:${ORKA_IMAGE_TAG}"
export GENERAL_WORKER_IMG="${ORKA_IMAGE_PREFIX}/general-worker:${ORKA_IMAGE_TAG}"
export HARNESS_WRAPPER_IMG="${ORKA_IMAGE_PREFIX}/agent-harness-wrapper:${ORKA_IMAGE_TAG}"
export ACP_CODEX_RUNTIME_IMG="${ORKA_IMAGE_PREFIX}/acp-codex-runtime:${ORKA_IMAGE_TAG}"
export ACP_CLAUDE_RUNTIME_IMG="${ORKA_IMAGE_PREFIX}/acp-claude-runtime:${ORKA_IMAGE_TAG}"
export ACP_COPILOT_RUNTIME_IMG="${ORKA_IMAGE_PREFIX}/acp-copilot-runtime:${ORKA_IMAGE_TAG}"
export ACP_OPENCODE_RUNTIME_IMG="${ORKA_IMAGE_PREFIX}/acp-opencode-runtime:${ORKA_IMAGE_TAG}"
export WORKSPACE_PUBLISHER_IMG="${ORKA_IMAGE_PREFIX}/workspace-publisher:${ORKA_IMAGE_TAG}"

make docker-build-all
make docker-push-all
```

The Helm command below uses these repositories and the same tag for both native workers.
The controller, publisher, and ACP runtimes require registry digests. After pushing, list
their references and replace the corresponding digest placeholders in the Helm command:

```bash
docker image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' \
  "$IMG" "$WORKSPACE_PUBLISHER_IMG" \
  "$ACP_CODEX_RUNTIME_IMG" "$ACP_CLAUDE_RUNTIME_IMG" \
  "$ACP_COPILOT_RUNTIME_IMG" "$ACP_OPENCODE_RUNTIME_IMG"
```

## Install the development chart

Install the development chart from `manifest_staging/charts/orka`. It matches
the source checkout. The root `charts/orka` directory holds files prepared for
release and may not match your code:

```bash
helm install orka ./manifest_staging/charts/orka \
  --kube-context "${ORKA_CONTEXT}" \
  --namespace orka-system --create-namespace \
  --set controller.image.repository="${ORKA_IMAGE_PREFIX}" \
  --set controller.image.digest="sha256:<controller-digest>" \
  --set workers.ai.image.repository="${ORKA_IMAGE_PREFIX}/ai-worker" \
  --set-string workers.ai.image.tag="${ORKA_IMAGE_TAG}" \
  --set workers.general.image.repository="${ORKA_IMAGE_PREFIX}/general-worker" \
  --set-string workers.general.image.tag="${ORKA_IMAGE_TAG}" \
  --set publisher.image.repository="${ORKA_IMAGE_PREFIX}/workspace-publisher" \
  --set publisher.image.digest="sha256:<publisher-digest>" \
  --set controller.acpRuntime.codexImage="${ORKA_IMAGE_PREFIX}/acp-codex-runtime@sha256:<codex-digest>" \
  --set controller.acpRuntime.claudeImage="${ORKA_IMAGE_PREFIX}/acp-claude-runtime@sha256:<claude-digest>" \
  --set controller.acpRuntime.copilotImage="${ORKA_IMAGE_PREFIX}/acp-copilot-runtime@sha256:<copilot-digest>" \
  --set controller.acpRuntime.opencodeImage="${ORKA_IMAGE_PREFIX}/acp-opencode-runtime@sha256:<opencode-digest>" \
  --wait --timeout 10m
```

The controller issues its own webhook certificate. To bring your own, see
[Webhook certificate](../reference/configuration.md#webhook-certificate).

To disable an unused runtime, set its image to an empty string, for example
`--set-string controller.acpRuntime.codexImage=`. Otherwise, the chart uses its
release image tag for any runtime you do not override.

If Helm refuses to render, that is deliberate — the chart checks its inputs up front
rather than installing something broken. [Troubleshooting](../operations/troubleshooting.md)
lists the guards and what each one wants.

:::info[Kustomize instead of Helm]
Install the shared CRDs from `config/crd` through your cluster's designated CRD owner
before deploying workloads. The `config/acp-production` workload overlay excludes CRDs;
it adds the network policy that stops model traffic from bypassing the provider proxy.
`make deploy` checks the shared CRD prerequisite and creates the artifact, publisher,
and proxy Secrets before applying that overlay. Its controller, publisher, and runtime
image variables must use the pushed `repository@sha256:...` references.
:::

## Upgrades

Orka currently supports new installations only. Read
[Upgrading](../operations/upgrading.md) for support details and CRD requirements.

## Next

Continue with [Connect to the API](../getting-started.md#connect-to-the-api)
in Getting started. Those commands use your current kubectl context, so select
`ORKA_CONTEXT` first with `kubectl config use-context` if it is not already current.
