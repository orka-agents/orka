---
slug: /provider-proxy
description: "Connect built-in coding agents to your model gateway."
---

# Provider proxy

This step is optional. You only need it to run built-in coding agents, which
are `type: agent` Tasks running Codex, Claude Code, GitHub Copilot CLI, or
OpenCode. Install Orka first, then come back here.

:::tip[Fastest path]
Have an OpenAI API key? Jump to [With provider API keys](#with-provider-api-keys),
then [Connect the gateway](#connect-the-gateway). Have an OpenAI Codex or
GitHub Copilot subscription instead? Use
[With a Codex subscription](#with-an-openai-codex-subscription) or
[With a GitHub Copilot subscription](#with-a-github-copilot-subscription).
Either way it is one gateway install, one values file, and one `helm upgrade`.
:::

AI-worker tasks, `type: ai`, do not use a gateway. They read your
[Provider resources](../reference/configuration.md#provider) and their API-key
Secrets directly, and they work as soon as Orka is installed.

Coding agents are different. They are third-party CLIs, so Orka never hands
them a provider API key. Instead, they talk to a model gateway that you run and
that holds the real credentials:

```text
coding agent → session proxy → Orka auth proxy → your gateway → model provider
```

Orka's auth proxy authenticates each runtime request and forwards it to your
gateway. Coding agents receive only a session token and permission to use
their configured model. The gateway supplies its own provider credentials.

:::tip[Video demo]
Watch [Compare hosted and routed agents on the same workload](https://www.youtube.com/watch?v=L6W3IPuLmeQ).
:::

## Choose a model gateway

Any gateway that serves the APIs your agents need will work. Codex uses the
OpenAI Responses API, Claude Code uses the Anthropic Messages API, and OpenCode
uses Chat Completions. [Vekil](https://github.com/sozercan/vekil) is the gateway
Orka's own tests use, and the example below installs it.
[agentgateway](https://agentgateway.dev/) is another option.

Whatever you choose, configure its providers and models, and verify a model
request succeeds through it before connecting Orka.

## Example: Vekil on the same cluster

[Vekil](https://github.com/sozercan/vekil) is a small reverse proxy that fronts
GitHub Copilot, OpenAI, Azure OpenAI, Anthropic, and other providers behind one
endpoint. It ships as a container image, `ghcr.io/sozercan/vekil`, listening on
port 1337. Run it as a Deployment and Service named `vekil` in a `vekil-system`
namespace, following its [getting started](https://github.com/sozercan/vekil/blob/main/docs/getting-started.md)
and [configuration](https://github.com/sozercan/vekil/blob/main/docs/configuration.md)
docs, and give it credentials in one of the ways below. Each is a Vekil configuration;
Vekil's [provider routing](https://github.com/sozercan/vekil/blob/main/docs/provider-routing.md)
and [provider API keys](https://github.com/sozercan/vekil/blob/main/docs/provider-api-keys.md)
pages have the details and more providers.

### With provider API keys

Vekil reads provider keys from environment variables that you back with
Kubernetes Secrets, referenced from a providers file with `api_key_env`. Never
put a key in the file itself. This example exposes one OpenAI model; keep the
model IDs you plan to use in Orka.

```yaml title="providers.yaml"
providers:
  - id: openai
    type: openai-compatible
    default: true
    base_url: https://api.openai.com/v1
    api_key_env: OPENAI_API_KEY
    models:
      - public_id: gpt-6-astra
        deployment: gpt-6-astra
        endpoints:
          - /responses
          - /chat/completions
```

For Azure OpenAI, use `type: azure-openai` with your resource's base URL and
deployment names:

```yaml
providers:
  - id: azure-openai
    type: azure-openai
    default: true
    base_url: https://<resource>.cognitiveservices.azure.com/openai/v1
    api_key_env: AZURE_OPENAI_API_KEY
    models:
      - public_id: gpt-6-astra
        deployment: <your-deployment-name>
        endpoints:
          - /responses
```

Mount the file as Vekil's `--providers-config` and set `OPENAI_API_KEY` (or the
name you chose) from a Secret in the Vekil Deployment.

### With an OpenAI Codex subscription

Vekil can use the ChatGPT login that the Codex CLI stores after `codex login`.
Copy that file into a Secret and mount it into the Vekil Deployment at the
Codex home path Vekil documents:

```bash
codex login
kubectl -n vekil-system create secret generic codex-auth \
  --from-file=auth.json="$HOME/.codex/auth.json"
```

```yaml title="providers.yaml"
providers:
  - id: openai-codex
    type: openai-codex
    default: true
```

When the stored login expires, run `codex login` again and recreate the Secret.

### With a GitHub Copilot subscription

Without a providers file, Vekil uses GitHub Copilot as its only upstream. Set
`COPILOT_GITHUB_TOKEN` in the Vekil Deployment from a Secret holding a GitHub
token for a user with Copilot access. Without a token, Vekil starts a
device-code login and prints the code and URL in its Pod logs:

```bash
kubectl -n vekil-system logs deploy/vekil
```

### Verify Vekil

Do not continue until readiness passes and the model you plan to use appears in
the model list. A gateway that is up but cannot reach its provider makes every
agent Task fail with an authentication error that looks like an Orka problem.

```bash
kubectl -n vekil-system port-forward svc/vekil 1337:1337 &
curl http://127.0.0.1:1337/readyz
curl http://127.0.0.1:1337/v1/models
kill %1
```

### Allow Orka to reach Vekil

If your cluster enforces NetworkPolicy and the `vekil-system` namespace
restricts ingress, allow traffic from Orka's auth proxy:

```bash
kubectl -n vekil-system apply -f - <<'YAML'
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: allow-orka-provider-proxy
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/name: vekil
  policyTypes: [Ingress]
  ingress:
    - from:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: orka-system
          podSelector:
            matchLabels:
              orka.ai/network-role: provider-auth-proxy
      ports: [{protocol: TCP, port: 1337}]
YAML
```

Then continue to [Connect the gateway](#connect-the-gateway).

## Connect the gateway

The Helm chart leaves `providerProxy.enabled=false` until you configure it. Save
your connection settings in a values file, for example `model-access.yaml`:

```yaml title="model-access.yaml"
providerProxy:
  enabled: true
  upstreamBaseURL: http://vekil.vekil-system.svc:1337
```

That is all the chart needs when the gateway is a Service in your cluster. At
install time it reads the Service named in the URL and writes the NetworkPolicy
rule that lets Orka's proxy reach that Service's Pods on their listening port,
and nothing else. DNS access is included.

For a gateway outside the cluster, a Service whose target port is a name rather
than a number, or an offline render such as `helm template`, spell the rule out
instead. The egress port is the gateway Pod's listening port, which can differ
from its Service port:

```yaml
providerProxy:
  enabled: true
  upstreamBaseURL: http://vekil.vekil-system.svc:1337
  egress:
    - to:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: vekil-system
          podSelector:
            matchLabels:
              app.kubernetes.io/name: vekil
      ports:
        - protocol: TCP
          port: 1337
```

The chart does not create resources in the gateway's namespace or change its
ingress policy, so allow ingress from Orka's namespace and Pods labeled
`orka.ai/network-role: provider-auth-proxy` yourself, as shown for Vekil above.

Apply the values to your release. The installed chart version keeps the upgrade
on the same release, and `--reuse-values` keeps your existing Secret and image
settings:

```bash
CHART_VERSION="$(helm get metadata orka -n orka-system -o yaml | awk '/^version:/ {print $2}')"
helm upgrade orka orka/orka --version "$CHART_VERSION" --namespace orka-system \
  --reuse-values --values model-access.yaml --wait
```

For a source installation, run from the same checkout and use its generated chart:

```bash
helm upgrade orka ./manifest_staging/charts/orka --namespace orka-system \
  --reuse-values --values model-access.yaml --wait
```

Check that the proxy is ready:

```bash
kubectl -n orka-system get deploy -l app.kubernetes.io/component=provider-auth-proxy
```

Now [run a coding agent](../getting-started.md#running-a-coding-agent). The
Agent's `model.name` must be a model ID your gateway lists.

Built-in coding agents cannot start without this connection. Creating a
Provider resource for AI-worker tasks does not configure the coding-agent gateway.

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
| Chart says the upstream Service was not found | Install the gateway before enabling the proxy, or set `providerProxy.egress` explicitly; offline renders cannot look Services up. |
| Chart asks for `providerProxy.egress` | The upstream is not an in-cluster `<service>.<namespace>.svc` address, or its Service uses a named target port. Write the egress rule by hand. |
| Agent Task fails with an authentication error | Check the gateway's readiness endpoint and provider credentials first. It fails independently of Orka. |
| Model calls time out | Check gateway readiness, egress rules, and gateway ingress policy. |
| Model calls return 404 | Check that the model ID is listed by the gateway and that the upstream URL has no extra path. |
| Requests fail after token rotation | Check the overlap token and its expiry. |

See [Troubleshooting](troubleshooting.md) for other installation and runtime errors.
