# Web search with a custom Tool

Gives a native `type: ai` Task the ability to search the web, by wiring the
[Tavily](https://tavily.com) search API up as an Orka `Tool`.

A `Tool` is how you hand an agent a capability Orka does not ship with. You describe an
HTTP endpoint and its JSON parameters; Orka turns that into a function the model can call,
and holds the API key so the model never sees it.

## Apply it

```bash
# Your Anthropic key, if you do not already have this Secret
kubectl -n orka-system create secret generic anthropic-secret \
  --from-literal=api-key='<your-anthropic-key>'

# Your Tavily key — edit secret.yaml first, or skip it and create the Secret directly
kubectl -n orka-system create secret generic tavily-secret \
  --from-literal=api-key='<your-tavily-key>'

kubectl apply -n orka-system -f examples/tavily/tool.yaml -f examples/tavily/task.yaml
kubectl logs -n orka-system -l orka.ai/task=tavily-test -f
```

`kubectl -n orka-system apply -k examples/tavily` applies all three files including `secret.yaml`, whose
key is the literal string `YOUR_TAVILY_API_KEY_HERE`. Replace it before using kustomize,
or use the two `kubectl create secret` commands above and skip `secret.yaml`.

Get a Tavily key at [app.tavily.com](https://app.tavily.com) — the free tier is enough to
run this example.

## How the pieces connect

`tool.yaml` declares the tool:

- `spec.description` and `spec.parameters` are what the model sees. The description tells
  it *when* to call the tool; the JSON Schema tells it what arguments to send. Orka
  forwards whatever the model produces as the request body, so the schema is guidance for
  the model, not a check on it — the endpoint itself rejects anything malformed.
- `spec.http` is what the model never sees: the URL, the timeout, and the credential.
  `authSecretRef` reads the key from `tavily-secret`, and `authInject: body` with
  `authBodyKey: api_key` puts it in the JSON request body, which is what Tavily expects.
  (The default, `authInject: header`, sends `Authorization: Bearer <token>` instead.)

`task.yaml` opts in by name — `spec.ai.tools: [tavily-search]`. Only listed tools are
available to that Task; a `Tool` existing in the namespace does not make it callable.

Full field reference: [Tool CRD schema](../../website/docs/reference/api-reference.md#tool-crd-schema).

## Adapting it

Any JSON-over-HTTP API works the same way. Change the URL, rewrite `spec.parameters` to
match its request body, and point `authSecretRef` at your Secret. If the API wants a
header instead of a body field, drop `authInject` and `authBodyKey`.
