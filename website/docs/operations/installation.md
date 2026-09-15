---
slug: /installation
description: "Install Orka on Kubernetes and run a test task."
---

# Install Orka

Install the latest Orka release with Helm, then run a small test task.
These commands use your current Kubernetes context, name the installation `orka`,
and use the namespace `orka-system`.
For development, [build from source](../getting-started.md#option-b-current-main-from-source).

## Before you start

- Bash, Helm, kubectl, and OpenSSL.
- A Kubernetes cluster with no existing Orka installation or Orka CRDs,
  and permission to install cluster-wide resources.
- NetworkPolicy enforcement, a default StorageClass, and HTTPS access to
  `ghcr.io` from the cluster nodes and controller.
- [Vekil](provider-proxy.md) running with access to your model provider.
  Orka expects it at `http://vekil.vekil-system.svc:1337`.

## 1. Prepare the namespace

Create Orka's namespace and encryption key. The namespace label selects the
default agent execution mode, `harness-v2`.

```bash
kubectl create namespace orka-system
kubectl label namespace orka-system orka.ai/controller-mode=harness-v2

openssl rand 32 | kubectl -n orka-system create secret generic orka-agent-snapshot-key \
  --from-file=key=/dev/stdin
```

Back up this Secret with your data. Orka needs the same key to read saved agent
configuration after a restore.

Before continuing, complete the [webhook certificate setup](../reference/configuration.md#webhook-certificate).
The chart currently requires this Secret for Kubernetes to validate Orka resources.

## 2. Install with Helm

Install the latest chart from Orka's Helm repository. It includes the matching
image tags; the settings below connect it to the Secrets you created.

```bash
helm repo add orka https://orka-agents.github.io/orka/charts
helm repo update orka
helm install orka orka/orka --namespace orka-system \
  --set-string controller.agentExecutionSnapshot.existingSecret=orka-agent-snapshot-key \
  --set-string controller.agentExecutionSnapshot.key=key \
  --set-string webhooks.tls.existingSecret=orka-webhook-tls \
  --set-string webhooks.caBundle="$(kubectl -n orka-system get secret orka-webhook-tls -o jsonpath='{.data.ca\.crt}')" \
  --wait --timeout 10m
```

To choose different image tags or pin digests, see
[Image overrides](../reference/configuration.md#image-overrides).

## 3. Check the installation

Check that the deployments are ready and the data volumes show `Bound`,
then run a container task. This test does not call a model.

```bash
kubectl -n orka-system get deployments,pvc

kubectl -n orka-system create -f - <<'YAML'
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
Next, [run a coding agent](../getting-started.md#running-a-coding-agent)
or [connect to the API](../getting-started.md#give-yourself-an-api-client).
