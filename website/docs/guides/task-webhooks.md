---
sidebar_label: Task webhooks
description: "Signed, durable completion webhooks for Orka Tasks, and how a receiver verifies them."
---

# Task webhooks

Set `spec.webhookURL` on a Task and the controller POSTs a JSON notice when the
Task reaches a terminal phase (`Succeeded`, `Failed`, or `Cancelled`). Every
notice is signed with an Ed25519 key. Delivery is durable across controller
restarts, retried with bounded backoff, and limited to destinations that are
safe for a controller to call.

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Task
metadata:
  name: nightly-report
spec:
  type: ai
  prompt: "Summarize yesterday's incidents"
  webhookURL: https://hooks.example.com/orka
```

## Where a notice may go

The controller fails closed. A destination must be one of:

- **A public `http` or `https` host.** The name is resolved when the
  controller connects, on every connection, and *every* address it resolves to
  must be globally routable. Loopback, RFC 1918, CGNAT (`100.64.0.0/10`),
  link-local (including `169.254.169.254`), IPv6 unique-local and link-local,
  multicast, broadcast, unspecified (`0.0.0.0`, `::`), IPv4-mapped forms of any
  of these, and the other IANA special-use ranges are refused. A DNS failure is a
  failed attempt, never a pass, and because the check runs at connect time a
  name that is re-pointed after validation (DNS rebinding) cannot reach an
  internal address.
- **A Service in the Task's own namespace**, as `http://<name>.<namespace>.svc[.cluster.local][:port]/...`.
  The Service must exist, have a selector and a ClusterIP, and not be
  `ExternalName`. The controller connects straight to that ClusterIP without
  DNS. Services in other namespaces and `kubernetes.default` are refused.

The controller never follows redirects (a `3xx` is a failed attempt) and never
sends webhooks through an HTTP proxy, even when `HTTP_PROXY` is set, because a
proxy would connect on its behalf and skip these checks. If your controller
needs an egress proxy, put a same-namespace Service in front of it.

A URL with a non-`http(s)` scheme, a literal non-public IP, a blocked host
name, or another namespace's Service is rejected without any network attempt:
the delivery goes straight to `Failed`.

## The request

```http
POST /orka HTTP/1.1
Content-Type: application/json
User-Agent: Orka-Controller/1.0
X-Orka-Task: nightly-report
X-Orka-Namespace: team-a
X-Orka-Event: task.succeeded
X-Orka-Delivery-ID: 6f1c3c2e-5b0a-4f7e-9a51-0d3f1f7f6a10
Idempotency-Key: 6f1c3c2e-5b0a-4f7e-9a51-0d3f1f7f6a10
X-Orka-Delivery-Attempt: 1
X-Orka-Signature: eyJhbGciOiJFZERTQSIsImtpZCI6Ii4uLiIsInR5cCI6IkpXVCJ9...

{"deliveryId":"6f1c3c2e-...","eventType":"task.succeeded","occurredAt":"2026-11-06T10:00:00Z",
 "taskUID":"0b9e0f43-...","taskName":"nightly-report","taskNamespace":"team-a",
 "phase":"Succeeded","message":"...","startTime":"...","completionTime":"...",
 "attempts":1,"resultRef":{"available":true}}
```

`eventType` is `task.succeeded`, `task.failed`, or `task.cancelled`. Every
attempt of one delivery sends a byte-identical body under the same delivery
ID; only `X-Orka-Delivery-Attempt` and the signature change.

Any `2xx` response acknowledges the notice. Respond quickly; one attempt times
out after 30 seconds.

## Verifying a notice

`X-Orka-Signature` is a compact JWS (a JWT) signed with `alg: EdDSA`
(Ed25519). Its header carries the signing key's `kid`, the RFC 7638
thumbprint of the public key. The public keys are published, without
authentication, by the Orka API:

```text
GET https://<orka-api>/.well-known/orka/task-webhook-jwks.json
```

```json
{"keys":[{"kty":"OKP","crv":"Ed25519","x":"...","kid":"...","alg":"EdDSA","use":"sig"}]}
```

Fetch the JWKS out of band or over a trusted channel: it is what your receiver
trusts. Then, for each request:

1. Read `kid` from the JWS header and find that key in your cached JWKS. If it
   is missing, refetch the JWKS once (rate-limit this) and try again; reject if
   it is still missing. Accept only `alg: EdDSA`.
2. Verify the Ed25519 signature over the first two dot-separated segments.
3. Check the claims:
   - `iss` is `orka.ai/task-webhook`;
   - `aud` is exactly the `webhookURL` configured on the Task;
   - `exp` is in the future and `nbf` is not, allowing about a minute of clock
     skew. Tokens live for five minutes and are minted for each attempt;
   - `payload_sha256` equals the lowercase hex SHA-256 of the raw request body
     you received, before any JSON parsing.
4. Use `task.uid`, `task.namespace`, `task.name`, `event`, and `delivery_id`
   from the verified claims rather than from headers or the body.
5. Deduplicate on `delivery_id` (also sent as `X-Orka-Delivery-ID` and
   `Idempotency-Key`). Delivery is at-least-once: a controller that restarts
   mid-attempt sends the same delivery again.

The claim set looks like:

```json
{
  "iss": "orka.ai/task-webhook",
  "sub": "0b9e0f43-...",
  "aud": "https://hooks.example.com/orka",
  "iat": 1793872800, "nbf": 1793872800, "exp": 1793873100,
  "jti": "6f1c3c2e-....1",
  "delivery_id": "6f1c3c2e-...",
  "attempt": 1,
  "event": "task.succeeded",
  "task": {"uid": "0b9e0f43-...", "namespace": "team-a", "name": "nightly-report"},
  "payload_sha256": "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
}
```

Any standard JOSE library that supports EdDSA can do steps 1–3. In Go,
`github.com/lestrrat-go/jwx/v3` verifies with
`jwt.ParseString(token, jwt.WithKeySet(set), jwt.WithIssuer("orka.ai/task-webhook"), jwt.WithAudience(webhookURL))`
followed by the `payload_sha256` comparison. Orka's own reference
implementation is `taskwebhook.Verify` in `internal/taskwebhook`.

## Retries and status

The delivery is recorded in the Task's status, so it survives controller
restarts, upgrades, and leader changes:

```yaml
status:
  webhookDelivered: true
  webhookDelivery:
    deliveryID: 6f1c3c2e-5b0a-4f7e-9a51-0d3f1f7f6a10
    eventType: task.succeeded
    occurredAt: "2026-11-06T10:00:00Z"
    payloadSHA256: 9f86d0...
    state: Delivered          # Pending, Delivered, or Failed
    attempts: 1
    lastStatusCode: 202
```

- A failed attempt (connection error, timeout, refused address, redirect, or a
  non-`2xx` response) is retried after 10s, 20s, 40s, and so on, capped at 15
  minutes, for at most **8 attempts**. After that the delivery is `Failed` and
  is never retried. `lastError` records a short reason with no URL or
  credential in it.
- Each attempt is claimed in Task status before it is sent. A controller that
  dies mid-attempt leaves a two-minute lease; the next controller waits for it,
  then retries under the same delivery ID.
- Only one notice is pending per Task. If the Task's terminal state changes
  before a pending notice is delivered, the controller supersedes it: the old
  delivery ID is never sent again, a new one carries the newer state, its
  attempts start over, and `superseded` counts the replaced notices.
  Supersession never shortens an existing backoff.
- If signing is enabled but no signing key is available yet (for example, the
  Secret cannot be read or holds a malformed key), nothing is sent and no
  attempt is consumed; the delivery stays `Pending` until a key is available.
- If signing is disabled (`--task-webhook-signing-secret=""`), nothing is sent
  and the delivery is marked `Failed` at once. Enabling signing later does not
  retry notices that already failed this way.

## Signing keys and rotation

The controller signs with the Ed25519 key in the `current` item of a Secret in
the `orka-task-webhook-signing` namespace (Helm value
`controller.taskWebhooks.signingNamespace`, flag
`--task-webhook-signing-namespace`). The Secret is
`<fullname>-task-webhook-signing` for the Helm chart, where `<fullname>` is the
chart's full name (`orka` for a release named `orka`; override the Secret name
with `controller.taskWebhooks.signingSecretName`), or
`orka-task-webhook-signing` for the kustomize install. On first use, the
controller creates that Secret and mints `current` if either is missing. Keys
are PEM-encoded PKCS#8, which is what
`openssl genpkey -algorithm ed25519` writes. A malformed key is never
replaced: delivery pauses until it is fixed.

Every replica rereads the Secret at least every 15 seconds, so rotation needs no
restart. To rotate:

```bash
NS=orka-task-webhook-signing
SECRET=orka-task-webhook-signing  # <fullname>-task-webhook-signing for Helm
openssl genpkey -algorithm ed25519 -out new.pem
OLD=$(kubectl -n "$NS" get secret "$SECRET" -o jsonpath='{.data.current}')
kubectl -n "$NS" patch secret "$SECRET" --type merge \
  -p "{\"data\":{\"previous\":\"$OLD\",\"current\":\"$(base64 -w0 < new.pem)\"}}"
shred -u new.pem
```

The new key signs from then on. The JWKS publishes both `current` and
`previous`, so a token signed by a replica that has not yet reloaded still
verifies, and receivers that see the new `kid` refetch the JWKS. After
TokenTTL plus the reload interval (in practice, wait at least ten minutes),
drop `previous`:

```bash
kubectl -n "$NS" patch secret "$SECRET" --type json -p '[{"op":"remove","path":"/data/previous"}]'
```

If a key is compromised, rotate and remove `previous` at once. Receivers that
keep the old JWKS cached keep trusting the old key until they refetch.

Setting `--task-webhook-signing-secret=""` disables signing. Since notices are
never sent unsigned, that also stops webhook delivery: notices for Tasks that
finish while signing is disabled are marked `Failed` and are not retried after
signing is enabled again.

### Who can read the signing key

Anyone who can read or change the signing Secret can forge notices, so it
lives alone in its own namespace and only the controller is bound there:
`get` and `update` on that one Secret, and `create` on Secrets there. Nothing
that runs agent code can reach it:

- Worker roles, including the default AI worker profile that can read and
  write Secrets, are bound in the release namespace only.
- A Task's or Agent's `secretRef` mounts a Secret from the Task's namespace,
  and Tasks live in the release namespace.

The chart and the controller both refuse a signing namespace that is the
release (watch), controller, client, ACP runtime or agent-host namespace. Keep
the signing namespace free of other workloads and of role bindings beyond the
controller's and your cluster administrators'.
