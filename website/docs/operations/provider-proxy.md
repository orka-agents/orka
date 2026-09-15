---
slug: /provider-proxy
description: "Connect built-in coding agents to your model gateway."
---

# Provider proxy

Install Orka first, then configure your own providers and models. Orka does not
install a model gateway or require Vekil.

AI-worker tasks, `type: ai`, use your [Provider resources](../reference/configuration.md#provider)
and their referenced Secrets directly. Built-in coding agents, `type: agent`, use
Orka's authenticated provider proxy to reach a gateway you manage.

## Choose your model gateway

Use a gateway configured for the APIs your coding agents need.
[Vekil](https://github.com/sozercan/vekil) and
[agentgateway](https://agentgateway.dev/) are examples. Configure its providers,
credentials, and models yourself, and verify a model request succeeds before
connecting Orka.

Your gateway holds the real provider credentials. Coding agents receive only a
session token and permission to use their configured model:

```text
coding agent → session proxy → Orka auth proxy → your gateway → model provider
```

Orka's auth proxy authenticates runtime requests and forwards them to your gateway.
It does not forward the runtime token or inject provider credentials. Configure
your gateway to accept traffic from that proxy and supply its own provider credentials.

## Connect the gateway

The Helm chart leaves `providerProxy.enabled=false` until you configure it.
Save your connection settings in a values file, for example `model-access.yaml`:

```yaml
providerProxy:
  enabled: true
  upstreamBaseURL: http://model-gateway.models.svc:8080
  egress:
    - to:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: models
          podSelector:
            matchLabels:
              app.kubernetes.io/name: model-gateway
      ports:
        - protocol: TCP
          port: 8080
```

Replace the endpoint, namespace, Pod labels, and port with your gateway's settings.
The egress port is the gateway Pod's listening port, which can differ from its
Service port. DNS access is already included. The chart does not create resources
in your gateway's namespace or change its ingress policy.

Allow gateway ingress from Orka's release namespace and Pods labeled
`orka.ai/network-role: provider-auth-proxy`.

Use the Kubernetes context, release name, and namespace from your installation.
For a released chart, read its installed chart version:

```bash
ORKA_CONTEXT='<your-kubeconfig-context>'
helm get metadata orka --kube-context "$ORKA_CONTEXT" --namespace orka-system
```

Use that chart version below. `--reuse-values` keeps your existing Secret and image
settings:

```bash
helm upgrade orka orka/orka --version '<installed-chart-version>' \
  --kube-context "$ORKA_CONTEXT" --namespace orka-system \
  --reuse-values --values model-access.yaml --wait
```

For a source installation, keep `ORKA_CONTEXT` from the source instructions and
run from the same checkout, using its generated chart:

```bash
helm upgrade orka ./manifest_staging/charts/orka \
  --kube-context "$ORKA_CONTEXT" --namespace orka-system \
  --reuse-values --values model-access.yaml --wait
```

Check that the proxy is ready before [running a coding agent](../getting-started.md#running-a-coding-agent):

```bash
kubectl --context "$ORKA_CONTEXT" -n orka-system get deploy \
  -l app.kubernetes.io/component=provider-auth-proxy
```

Built-in coding agents cannot start without this authenticated connection. Creating
an AI-worker Provider resource does not configure the coding-agent gateway.

## Rotating the proxy token

The proxy reloads its mounted Secret every five seconds. To rotate without dropping
requests, keep the old token as `previous-token` and set `previous-token-valid-until`
to an absolute RFC3339 deadline. The overlap defaults to ten minutes and cannot
exceed 24 hours.

- When updating the proxy first, publish the new current token and old previous
  token, wait for reload, then roll the controller and runtime pools.
- When updating the controller first, pre-stage the new token as previous while
  current stays old. Verify it through the proxy, then swap the tokens and roll.

Remove the old token once every workload reports the new generation. If a token
file becomes unreadable or invalid, readiness and authenticated forwarding stop
until the files are valid again.

## Troubleshooting

| Symptom | Check |
| --- | --- |
| Chart rejects `upstreamBaseURL` | Use an HTTP(S) URL without credentials, a query, or a fragment. |
| Chart rejects `egress` | Supply NetworkPolicy rules allowing the proxy to reach your gateway. |
| Model calls time out | Check gateway readiness, egress rules, and gateway ingress policy. |
| Authentication errors | Verify the gateway's provider credentials and configured models. |
| Model calls return 404 | Check the gateway's API routes and the upstream URL path. |
| Requests fail after token rotation | Check the overlap token and its expiry. |

See [Troubleshooting](troubleshooting.md) for other installation and runtime errors.
