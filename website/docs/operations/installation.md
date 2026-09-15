---
slug: /installation
description: "Install Orka on Kubernetes and run a test task."
---

# Install Orka

Install the latest Orka release with Helm, then run a small test task.
The whole install is one namespace, one certificate Secret, and one Helm command.
These commands use your current Kubernetes context, name the installation `orka`,
and use the namespace `orka-system`.
For development, [build from source](../getting-started.md#option-b-current-main-from-source).

## Before you start

You need these tools on your machine:

- `kubectl` connected to your cluster. For a laptop,
  [kind](https://kind.sigs.k8s.io/) or [minikube](https://minikube.sigs.k8s.io/) is fine.
- [Helm](https://helm.sh/docs/intro/install/) 3 or newer.
- OpenSSL, for a test certificate.

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

Create the webhook certificate. Orka validates its resources through Kubernetes
admission webhooks, and the chart needs a TLS Secret for them. It never generates
one itself. The certificate must be valid for `orka-webhook.orka-system.svc`.

For a test cluster, create a self-signed certificate valid for 30 days:

```bash
(
  umask 077
  openssl req -x509 -newkey rsa:2048 -nodes -sha256 -days 30 \
    -keyout tls.key -out tls.crt \
    -subj '/CN=orka-webhook.orka-system.svc' \
    -addext 'subjectAltName=DNS:orka-webhook.orka-system.svc,DNS:orka-webhook.orka-system.svc.cluster.local' &&
  cp tls.crt ca.crt
)

kubectl -n orka-system create secret generic orka-webhook-tls \
  --type=kubernetes.io/tls \
  --from-file=tls.crt=tls.crt --from-file=tls.key=tls.key --from-file=ca.crt=ca.crt

rm -f tls.key tls.crt ca.crt
```

The certificate is self-signed, so it is its own CA and `ca.crt` is a copy of it.
For a real cluster, use your own CA or
[cert-manager](https://cert-manager.io/) instead. See
[Webhook certificate](../reference/configuration.md#webhook-certificate).

## 2. Install with Helm

Install the latest chart from Orka's Helm repository. It already includes the
matching image tags. The settings below point it at the certificate Secret you created.

```bash
helm repo add orka https://orka-agents.github.io/orka/charts
helm repo update orka
helm install orka orka/orka --namespace orka-system \
  --set-string webhooks.tls.existingSecret=orka-webhook-tls \
  --set-string webhooks.caBundle="$(kubectl -n orka-system get secret orka-webhook-tls -o jsonpath='{.data.ca\.crt}')" \
  --wait --timeout 10m
```

If Helm refuses to render, the message names the value it wants.
[Troubleshooting](troubleshooting.md#helm-refuses-to-render) lists the common ones.
To choose different image tags or pin digests, see
[Image overrides](../reference/configuration.md#image-overrides).

## 3. Check the installation

Check that the Deployments are ready and the data volumes show `Bound`:

```bash
kubectl -n orka-system get deployments,pvc
```

The install also created a Secret named `orka-agent-execution-snapshot`. It holds
the key that encrypts saved agent execution records. Back it up together with the
data volume. Without it, Orka cannot read those records after a restore. To
supply your own key instead, see
[Snapshot encryption key](../reference/configuration.md#snapshot-encryption-key).

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
