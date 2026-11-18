---
slug: /installation
description: "Install Orka on Kubernetes and run a test task."
---

# Install Orka

Install the latest Orka release with Helm, then run a small test task.
The whole install is one Helm command. It creates the `orka-system` namespace
for Orka itself and a second namespace, `orka-runtimes`, for the Pods that run
coding agents.
These commands use your current Kubernetes context, name the installation `orka`,
and use the namespace `orka-system`.
For development, [build from source](../development/build-from-source.md).

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

## 1. Install with Helm

Install the latest chart from Orka's Helm repository. It already includes the
matching image tags, generates its own encryption key, and issues its own
webhook certificate.

```bash
helm repo add orka https://orka-agents.github.io/orka/charts
helm repo update orka
helm install orka orka/orka --namespace orka-system --create-namespace \
  --wait --timeout 10m
```

The command returns when Orka is ready, which takes a minute or two on a laptop while
the images pull. To bring your own webhook certificate or use cert-manager, see
[Webhook certificate](../reference/configuration.md#webhook-certificate).

## 2. Check the installation

Check that the Deployments are ready and the data volumes show `Bound`:

```bash
kubectl -n orka-system get deployments,pvc
```

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

:::note[Back these up]
The install created two Secrets that the controller filled in on its first start.
`orka-agent-execution-snapshot` is the key that encrypts saved agent execution
records; without it, Orka cannot read those records after a restore, so back it up
with the data volume. `orka-webhook-tls` is the self-signed certificate for Orka's
admission webhooks, which the controller renews on its own. Never run
`helm upgrade --force` on this release, because it would replace both with the
chart's empty versions. To supply your own key or certificate, see
[Snapshot encryption key](../reference/configuration.md#snapshot-encryption-key)
and [Webhook certificate](../reference/configuration.md#webhook-certificate).
:::

## Next steps

1. [Connect to the API](../getting-started.md#connect-to-the-api) with a
   port-forward and a client token.
2. [Add a provider and run your first AI task](../getting-started.md#your-first-task)
   with an API key or a model server your cluster can reach.
3. To run coding agents such as Codex or Claude Code,
   [connect a model gateway](provider-proxy.md). This step is optional and can
   come later.

## Clean up

On a throwaway kind or minikube cluster, deleting the cluster removes everything.

:::danger[Uninstall deletes Orka's data]
`helm uninstall` removes the persistent volume claims that hold the task database
and workspace data. Unless your StorageClass retains released volumes, that data is
gone. A later reinstall starts empty; recovering records means restoring both the
volumes and the snapshot key Secret from a backup. Take that backup first if you
care about what is stored.
:::

On a cluster you keep:

```bash
helm uninstall orka --namespace orka-system
```

Helm leaves the CRDs, the two generated Secrets, and the `orka-system` namespace in
place. To remove those too:

```bash
kubectl delete namespace orka-system orka-runtimes
kubectl get crd -o name | grep '\.orka\.ai$' | xargs kubectl delete
```

Deleting the CRDs deletes every Orka resource in the cluster. See
[Upgrading](upgrading.md#uninstall) for the details.
