---
sidebar_position: 12
title: Shared OpenAI and Anthropic endpoints
---

The optional compatibility router lets teams use the same OpenAI or Anthropic
base URL. The caller's validated Kubernetes ServiceAccount token selects the
namespace. The router forwards the request to the Orka installation serving that
namespace, where the API checks the caller's permissions and executes tools.

The router serves exactly these routes:

| API | Messages | Model discovery |
| --- | --- | --- |
| OpenAI | `POST /openai/v1/chat/completions` | `GET /openai/v1/models` |
| Anthropic | `POST /anthropic/v1/messages` | `GET /anthropic/v1/models` |

Ordinary JSON and SSE responses retain the installation's format. Both message
routes take conversation history in `messages`. The router has no conversation
store and does not call model Providers itself.

## Prepare the installations

Prepare a separate Orka installation for each enabled namespace, following
[harness modes and namespace isolation](./harness-modes.md). Each controller
must keep its own `--watch-namespace`, ServiceAccount, RBAC, database, storage,
execution settings and leader-election Lease. Keep
`--enforce-namespace-isolation=true` on every receiving installation.

For example, `team-a` and `team-b` each need a working controller and API Service.
Create their Providers, Provider credential Secrets and any referenced Agents
inside the corresponding namespace. The same resource names can be used in
both namespaces. An explicit `model: "provider-name/model-name"` selects that
namespace's Provider; it never selects a Provider from another namespace.

A token does not provision a namespace, install Orka or grant permissions.
Enable a route only after its installation is ready to execute Tasks. In a
cluster with multiple installations, keep one platform owner for shared CRDs,
admission resources and enabled cluster-scoped reconcilers.

## Install the router

The Orka controller image also contains `/compat-router`. Deploy it separately
using `config/compat-router`, selecting an image built with this binary. The
router ServiceAccount only needs `create` on `tokenreviews.authentication.k8s.io`.
It receives no tenant-resource permissions.

Edit the ConfigMap's `routes.yaml` to map each enabled namespace to its Orka API
origin. These are installation URLs, without `/openai`, `/anthropic`, query
parameters, URL credentials or fragments:

```yaml
namespaces:
  team-a: http://orka-api.team-a.svc:8080
  team-b: http://orka-api.team-b.svc:8080
```

Use the actual Service names from your installations. An HTTPS origin uses
normal certificate verification. In-cluster HTTP requires a trusted transport
between the router and each installation. Environment proxy settings do not
change the configured destinations.

From the repository root, copy the example to an operator-owned directory,
update its routes, and select the image before applying it:

```bash
cp -R config/compat-router /path/to/router-config
cd /path/to/router-config
export ORKA_IMAGE='ghcr.io/orka-agents/orka@sha256:<digest-containing-compat-router>'
kustomize edit set image "controller=$ORKA_IMAGE"
kubectl apply -k .
kubectl -n orka-router-system rollout status deployment/orka-compat-router
```

Expose the router Service through your HTTPS ingress at the shared hostname.
Configure ingress timeouts to permit the installations' chat durations and
disable response buffering for SSE. The router preserves client cancellation
and imposes no additional whole-response timeout. Connection and TLS handshake
attempts time out after five seconds. The installation retains its chat and
tool deadlines.

The router bounds request uploads with `--read-timeout` (default `30s`) but
leaves response writes unrestricted, so this limit does not cut off long chats.
On shutdown, `--shutdown-timeout` allows active requests to finish (default
`30m`, matching the installation's default chat limit). The example Deployment
sets `terminationGracePeriodSeconds: 1810`. If an installation permits longer
chats, increase the router's shutdown timeout and keep the Pod grace period at
least ten seconds longer. Keep the Deployment progress deadline above that
window too (the example allows `2100` seconds). New connections stop being
accepted during shutdown.

Browser clients can make unauthenticated CORS preflight requests for the four
compatibility routes. Actual API requests still require authentication. Set
`ORKA_CORS_ALLOWED_ORIGINS` on the router to a comma-separated list of exact UI
origins, such as `https://chat.example.com`; the default is `*`, as on the
installation API. Known Anthropic/Stainless browser SDK metadata headers are allowed during
preflight but are not forwarded to installations. Cookie-based browser
credentials are not enabled. The router
controls this policy instead of forwarding an installation's CORS headers.

Do not configure ingress or CDN caching for these API responses. The router
sets `Cache-Control: private, no-store`, including when authentication uses
`x-api-key`, and overrides less restrictive installation cache headers.

Allow traffic from the router to each installation's API port. If the existing
controller NetworkPolicy restricts ingress, apply this additional policy in
each enabled namespace:

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: allow-compat-router
spec:
  podSelector:
    matchLabels:
      orka.ai/network-role: controller
  policyTypes: [Ingress]
  ingress:
    - from:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: orka-router-system
          podSelector:
            matchLabels:
              app.kubernetes.io/component: compat-router
      ports:
        - protocol: TCP
          port: 8080
```

If router egress is restricted, allow DNS, the Kubernetes API for TokenReview,
and the configured installation API destinations. Router health probes report
the listener's health; they do not imply that every installation is available.

Routes are read at startup. After changing the ConfigMap, restart the router
Deployment and wait for its rollout. Removing a route denies subsequent
requests after the rollout; it does not cancel work already accepted by an
installation.

## Grant caller permissions

Create a ServiceAccount and a RoleBinding in each team's namespace. This minimal
role permits chat and model discovery:

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: chat-client
  namespace: team-a
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: chat-client
  namespace: team-a
rules:
  - apiGroups: [core.orka.ai]
    resources: [chats]
    verbs: [create]
  - apiGroups: [core.orka.ai]
    resources: [providers]
    verbs: [list]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: chat-client
  namespace: team-a
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: chat-client
subjects:
  - kind: ServiceAccount
    name: chat-client
    namespace: team-a
```

Repeat in `team-b`. To create Tasks and retrieve their results through tools,
also grant `create` and `get` on `tasks.core.orka.ai` in the same namespace.
Grant `list` only to callers that use `list_tasks`. Grant other tool permissions
only for the operations the caller needs; see
[API authorization](../reference/api-authorization.md). Chat permission alone
never permits Task creation. Kubernetes grants to other namespaces do not widen
this endpoint's namespace restriction.

An operator with permission to issue ServiceAccount tokens can obtain a
short-lived token without printing or saving it:

```bash
export ORKA_TOKEN="$(kubectl -n team-a create token chat-client --duration=1h)"
```

The API server controls the issued lifetime. Renew the token before it expires
by issuing another token and replacing the client's credential. Avoid shell
tracing, verbose HTTP output and saved request captures containing credentials.
The router reuses Orka's TokenReview validation and its 60-second authentication
cache. The receiving installation also authenticates the caller and evaluates
RBAC for the request and each tool operation.

## Configure clients

| Client | Base URL | Credential header |
| --- | --- | --- |
| OpenAI-compatible | `https://orka.example.com/openai/v1` | `Authorization: Bearer $ORKA_TOKEN` |
| Anthropic-compatible | `https://orka.example.com/anthropic` | `x-api-key: $ORKA_TOKEN` |

The Anthropic base URL is for clients that append `/v1/messages`. The complete
URL is `https://orka.example.com/anthropic/v1/messages`. Anthropic also accepts
`Authorization: Bearer`. When both headers are present, Bearer takes precedence,
as on the installation API. An invalid Bearer credential does not fall back to
`x-api-key`.

Keep the normal request body and omit `namespace`. Changing only the token
selects the other team's installation. A matching `?namespace=team-a` is
accepted; a conflicting value is rejected before forwarding.

```bash
curl --fail-with-body https://orka.example.com/openai/v1/chat/completions \
  -H "Authorization: Bearer $ORKA_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"model":"shared/model","messages":[{"role":"user","content":"Hello"}]}'

curl --fail-with-body https://orka.example.com/anthropic/v1/messages \
  -H "x-api-key: $ORKA_TOKEN" \
  -H 'anthropic-version: 2023-06-01' \
  -H 'Content-Type: application/json' \
  -d '{"model":"shared/model","max_tokens":1024,"messages":[{"role":"user","content":"Hello"}]}'
```

Add `"stream":true` and `curl --no-buffer` for streaming. The
`X-Orka-Tools: disabled` header retains its existing behavior. Model discovery
uses the same credential and routing rules as messages.

The router forwards the selected token as Bearer and removes secondary
credentials, cookies and impersonation headers. It preserves the compatibility
protocol headers, `X-Orka-Tools`, request IDs and trace context. It does not log
credentials or persist request bodies.

## Errors and compatibility

| Status | Meaning |
| --- | --- |
| `401` | Missing, invalid or expired ServiceAccount credential, or transaction-token authentication sent to the shared listener. |
| `403` | No usable ServiceAccount namespace, namespace mismatch, disabled namespace, or insufficient caller permissions. Tool permission denials appear as tool results inside the normal message response. |
| `400` | Invalid query or request, or a missing Provider or Provider credential in the selected namespace. |
| `413` | Request exceeds the installation API's 15 MiB request limit. |
| `503` | The configured installation cannot be reached, TLS verification fails, or it tries to redirect the request. |

Router errors use the requested API's JSON error envelope. Installation JSON
errors and SSE events pass through unchanged. A broken connection after response
streaming has begun closes the stream; it cannot become a new JSON error response.
No error redirects work to another namespace or retries against another
installation.

Existing single-namespace APIs need no configuration changes. OIDC and
transaction-token clients continue using their installation endpoint under
their existing namespace and scope rules. The optional shared listener is for
Kubernetes ServiceAccounts; it does not introduce another login or token format.

## Verification

Run `go test ./internal/api -run TestCompatRouter -count=1` for authentication,
routing, protocol, RBAC and concurrent isolation tests. The deployed test in
`scripts/compat-router-e2e.sh` uses two isolated installations, a deterministic
model fixture and real worker Tasks, without external model credentials.
