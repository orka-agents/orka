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
[Provider resources](../reference/resources.md#provider) and their API-key
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

Choose a gateway that serves the APIs your agents need and preserves their
retry semantics. Codex uses the
OpenAI Responses API, Claude Code uses the Anthropic Messages API, and OpenCode
uses Chat Completions. [Vekil](https://github.com/sozercan/vekil) is the gateway
Orka's own tests use, and the example below installs it.
[agentgateway](https://agentgateway.dev/) is another option.

Whatever you choose, configure its providers and models, and verify a model
request succeeds through it before connecting Orka.

### When Codex is rate limited

**Retries are off by default.** On an operator-qualified route, the session
proxy can resend a direct, streaming Codex inference POST to the exact `/v1/responses`
endpoint if the auth proxy confirms **HTTP 429** from that route before
anything is relayed to Codex. Unqualified requests still report the failure;
they are not replayed or subject to the retry timeout. The original send
counts toward a maximum of three. Once a qualified 429 is observed, backoff,
retry response headers, and reading the final HTTP error body share a
30-second budget from the first send. If a retry returns 503 and its body
stalls, for example, the proxy keeps the 503
and the bounded detail received so far; it does not send again. It allows up
to one additional second to write the known error to Codex, even if the
initial 429 headers arrived after the window. This does not extend upstream
reads. Neither an initial 503 nor a successful stream gets this retry timeout.

The proxy uses the longest valid `Retry-After` (seconds or HTTP date),
`retry-after-ms` or `x-ms-retry-after-ms` hint. Fractional milliseconds
round up to the next whole millisecond. Without a valid hint
it waits 250 ms, then 500 ms. A wait that cannot fit the budget, request
deadline or active prompt lease returns the last 429 without retrying early.
On exhaustion, the final status and sanitized provider detail still reach
prompt failure accounting.

Retries share the original inference turn. Closing prompt admission stops
backoff and cancels any retry still waiting for response headers. A response
already being relayed can drain. This does not undo a POST the gateway already
received or erase the 429 already observed. If that rejection
settles the prompt before Codex acknowledges cancellation, the result is
`Failed`, not `Cancelled`. A lost connection, a 503, or an error inside a
200 stream is **not** a rejection that permits replay. Other routes (including
compaction, query-bearing requests and tools) and other runtimes do not use
this policy. No Task, ACP prompt or tool is retried.

The session proxy is the only retry owner in this Orka path. The Codex
profile disables native HTTP retries, stream reconnects and unbounded
connection retries; the ACP adapter submits the turn to Codex, and the Orka
auth proxy forwards the nonempty inference POST without a rewindable body.
Neither Orka hop lets an `Idempotency-Key` or `X-Idempotency-Key` trigger a
Go transport retry of this POST on a reused connection. The limit counts sends
**to the configured gateway**, not calls inside it. Orka cannot verify whether an
external gateway or provider retries, fails over, or reports a 429 after doing
work. For an end-to-end single retry owner, use a single-send gateway route
that returns 429 only for rejected requests and verify at its upstream.

#### Qualifying one route

Qualify the entire chain, not a model name or the shared auth-proxy Service.
Use a dedicated gateway DNS name whose first label ends in `-` and at least
six lowercase hex digits identifying an immutable revision. Freeze its image,
single-send route configuration, public model and physical deployment. An
accepted provider response must never be translated into a replayable 429.
Use a **new name** if any of these change; neither DNS nor a digest in a
Service name verifies how an operator configured the backend.

For example, a qualified gateway origin `http://gateway-a1b2c3.example.svc:1337`
and public model `example-model` give frontend Service name
`codex-429-24450096f2a7fa4adfb33fa6bca21e25`. Its suffix is the first 32
lowercase hex digits of SHA-256 over these exact UTF-8 bytes:
`orka-codex-429-v1\n<gateway origin>\n<model>\n` (`\n` denotes a newline).

| Operator-owned startup setting | Value for this example |
|---|---|
| Auth proxy `--upstream-base-url` (env default `ORKA_PROVIDER_AUTH_PROXY_UPSTREAM_BASE_URL`) | `http://gateway-a1b2c3.example.svc:1337` |
| Auth proxy `ORKA_PROVIDER_AUTH_PROXY_CODEX_429_RETRY_BASE_URL` | `http://gateway-a1b2c3.example.svc:1337` |
| Auth proxy `ORKA_PROVIDER_AUTH_PROXY_CODEX_429_RETRY_MODEL` | `example-model` |
| Runtime `ORKA_ACP_PROVIDER_PROXY_BASE_URL` (existing controller setting `--acp-provider-proxy-base-url`) | `http://codex-429-24450096f2a7fa4adfb33fa6bca21e25.example.svc:8080` |
| Runtime `ORKA_ACP_CODEX_429_RETRY_BASE_URL` | `http://codex-429-24450096f2a7fa4adfb33fa6bca21e25.example.svc:8080/v1` |
| Runtime `ORKA_ACP_CODEX_429_RETRY_MODEL` (must match frozen `ORKA_ACP_MODEL`) | `example-model` |

Set each qualification pair independently in reviewed, immutable startup
configuration (for example the digest-pinned runtime image), **not** from
the current request or by copying whatever destination is active. The auth
proxy pin has no path; the supervisor pin includes the effective `/v1`.
A partial or malformed opt-in fails startup. An unset pair or a valid pin
that does not match the active route cannot authorize a retry. Neither
component guesses qualifications from a provider response or client header.

Both the original request (before normalization) and the forwarded body must
contain one literal lowercase `model` matching the pin and one literal
lowercase `stream` set to boolean `true`. Escaped selector names do not
qualify; escaping inside text or tool data does not change qualification.
Non-streaming, ambiguous, provider-state or compaction
operations and routing overrides are not qualified. Ordinary messages, tool
definitions and prior tool results do not authorize or prevent a retry.

For each send the supervisor issues a fresh challenge. Only the auth proxy
may confirm it, after checking its own egress pin, the bounded request body,
and the upstream status. The auth proxy sends confirmed 429 headers promptly,
then reads up to 4 KiB of rejection detail for up to 250 ms. It bounds writes
to the supervisor as well, allowing one extra second. The supervisor
waits for the body to finish before resending, so the auth proxy has closed
the gateway response and released its slot. If the retry deadline arrives
first, the supervisor keeps the known 429 and the detail received so far.
Other responses keep streaming normally.
A conditional resend uses a distinct HTTP
authentication scheme with the existing proxy credential: an older,
Bearer-only proxy rejects it instead of forwarding it unchecked. Neither
the child nor the provider receives these internal headers. Use both upgraded components and
keep the legacy Service separate for rollback. Remove the opt-ins to disable
replay; native Codex retries remain disabled either way. No new controller
field is required.

#### Vekil and Azure/Foundry

**Vekil v0.14.14 does not meet that Codex retry contract.** Its Responses
handler can turn an upstream HTTP 200 into an HTTP 429 if the first SSE event
is `response.failed` with `rate_limit_exceeded`. This can happen without a
`Retry-After` hint, or [even when the failed response reports token usage](https://github.com/sozercan/vekil/blob/554f8e261f477a5b45eb08442b82f9cee485c38a/proxy/responses_failure_translate_test.go#L857-L885).
Orka sees the gateway's 429, not the original 200. Neither the error text nor
the presence of a delay distinguishes it from a rejected POST. Do not use
this route for automatic Codex retries until the gateway preserves accepted
streams and is verified to make a single physical send per request.

Once Vekil has sent HTTP 200, a later rate-limit event stays in the stream.
In its [access log](https://github.com/sozercan/vekil/blob/554f8e261f477a5b45eb08442b82f9cee485c38a/server/server.go#L340-L385),
`status=200, stats_status=429` means the stream failed **after** headers were
committed. It neither authorizes replay nor proves what Azure returned on
the wire before that event.

The `providers[].models` example below omits `schema_version`, selecting
version 1: a persistent upstream HTTP 429 can cause three physical sends
for one request to Vekil. A version 2 **explicit** `model_routes` entry with
`routing.mode: primary_only`, `max_target_attempts: 1` and
`max_upstream_sends: 1` caps each gateway operation at one send. It does
**not** disable conversion of an early failed stream into HTTP 429.

Vekil's [Azure cooldown](https://github.com/sozercan/vekil/blob/554f8e261f477a5b45eb08442b82f9cee485c38a/docs/troubleshooting.md#L66-L120)
applies to explicit `azure-openai` routes. It can delay *subsequent* requests
after a throttle with valid reset information, or a successful response with
an exhausted balance and reset. Aliases share the cooldown by resource origin
and deployment within one Vekil process. It does not reserve TPM or RPM before
every send or coordinate other replicas and callers. Neither completed token
usage nor the model's context window tells a session how much deployment
capacity remains. Preventing a burst requires bounded, cancellable admission
before each physical model send, against the same operator-allocated deployment
budget across callers and replicas. This is not a feature of Vekil v0.14.14.

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
