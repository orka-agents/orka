---
slug: /installation
description: "Install Orka on Kubernetes and run a test task."
---

# Install Orka

Install the latest Orka release with Helm, then run a small test task.
You create one namespace and run one Helm command. The chart creates a second
namespace, `orka-runtimes`, for the Pods that run coding agents.
These commands use your current Kubernetes context, name the installation `orka`,
and use the namespace `orka-system`.
For development, [build from source](../getting-started.md#option-b-current-main-from-source).

## Before you start

You need these tools on your machine:

- `kubectl` connected to your cluster. For a laptop,
  [kind](https://kind.sigs.k8s.io/) or [minikube](https://minikube.sigs.k8s.io/) is fine.
- [Helm](https://helm.sh/docs/intro/install/) 3 or newer.

Your cluster needs:

- Permission to create namespaces, CRDs, and cluster-wide RBAC.
- A default StorageClass. Orka stores its database on a small persistent volume.
- HTTPS access to `ghcr.io` from the nodes and from the controller Pod. The controller
  looks up image digests at startup.
- No existing Orka installation or Orka CRDs. If a previous install left CRDs behind,
  see [Upgrading](upgrading.md#--skip-crds).

NetworkPolicy enforcement is recommended for production, because Orka uses
NetworkPolicies to keep model credentials away from agent Pods. It is not
required for a test install. kind and minikube work without it.

You do not need a model API key to install Orka. You add providers and models
after the install.

## 1. Prepare the namespace

Create Orka's namespace. The label selects the default agent execution mode,
`harness-v2`, and the controller refuses to start without it.

```bash
kubectl create namespace orka-system
kubectl label namespace orka-system orka.ai/controller-mode=harness-v2
```

## 2. Install with Helm

Install the latest chart from Orka's Helm repository. It already includes the
matching image tags, generates its own encryption key, and issues its own
webhook certificate.

```bash
helm repo add orka https://orka-agents.github.io/orka/charts
helm repo update orka
helm install orka orka/orka --namespace orka-system --wait --timeout 10m
```

To bring your own certificate or use cert-manager, see
[Webhook certificate](../reference/configuration.md#webhook-certificate).

If Helm refuses to render, the message names the value it wants.
[Troubleshooting](troubleshooting.md#helm-refuses-to-render) lists the common ones.
To choose different image tags or pin digests, see
[Image overrides](../reference/configuration.md#image-overrides).

## 3. Check the installation

Check that the Deployments are ready and the data volumes show `Bound`:

```bash
kubectl -n orka-system get deployments,pvc
```

The install also created two Secrets that the controller filled in on its first
start. `orka-agent-execution-snapshot` holds the key that encrypts saved agent
execution records. Back it up together with the data volume. Without it, Orka
cannot read those records after a restore. To supply your own key instead, see
[Snapshot encryption key](../reference/configuration.md#snapshot-encryption-key).
`orka-webhook-tls` holds the self-signed CA and serving certificate for Orka's
admission webhooks. The controller renews it on its own. Never run
`helm upgrade --force` on this release, because it replaces both Secrets with
the chart's empty versions.

Then run a container task. This test does not call a model.

```bash
kubectl -n orka-system apply -f - <<'YAML'
apiVersion: core.orka.ai/v1alpha1
kind: Task
metadata:
  name: release-install-check
spec:
  type: container
  command: ["/bin/sh", "-c", "printf ORKA_INSTALL_OK"]
  timeout: 3m
  retryPolicy:
    maxRetries: 0
YAML

kubectl -n orka-system wait \
  --for=jsonpath='{.status.phase}'=Succeeded task/release-install-check --timeout=3m
```

When the command reports `condition met`, Orka has completed its first task.

## Next steps

1. [Connect to the API](../getting-started.md#give-yourself-an-api-client) with a
   port-forward and a client token.
2. [Add a provider and run your first AI task](../getting-started.md#your-first-task)
   with an Anthropic, OpenAI, or Azure OpenAI API key.
3. To run coding agents such as Codex or Claude Code,
   [connect a model gateway](provider-proxy.md). This step is optional and can
   come later.
