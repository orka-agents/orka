---
sidebar_label: Task webhooks
description: "Signed, durable completion and approval webhooks for Orka Tasks, and how a receiver verifies them."
---

# Task webhooks

Set `spec.webhookURL` on a Task and the controller POSTs a JSON notice when the
Task reaches a terminal phase (`Succeeded`, `Failed`, or `Cancelled`). A
[parked agent turn](#parked-agent-turns) reports when its work ends instead. Every
notice is signed with an Ed25519 key. Delivery is durable across controller
restarts, retried with bounded backoff, and limited to destinations that are
safe for a controller to call. A Task can also opt into
[approval notices](#approval-events).

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

`eventType` is one of:

| Event | Sent | Body |
| --- | --- | --- |
| `task.succeeded`, `task.failed`, `task.cancelled` | Always, once the Task's outcome is known | Terminal body above |
| `task.approval_requested` | When selected in `spec.webhookEvents` | [Approval body](#approval-events) |
| `task.approval_resolved` | When selected in `spec.webhookEvents` | [Approval body](#approval-events) |
| `task.approval_overflow` | When any approval event is selected and the queue overflows | [Approval body](#approval-events) |

Every attempt of one delivery sends a byte-identical body under the same
delivery ID; only `X-Orka-Delivery-Attempt` and the signature change.

For a parked agent turn, `eventType` reports the outcome of the work, which can differ
from the Task's own `phase`; see [Parked agent turns](#parked-agent-turns).

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
- Attempts are sent by a pool of delivery workers, separate from Task
  reconciliation, so a slow or unresponsive receiver never delays scheduling
  or monitoring of any Task. Each Task's notices still go one attempt at a
  time. The pool sends at most `--task-webhook-delivery-workers` attempts at
  once (default 8); beyond that, other notices queue until a worker is free.
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

## Parked agent turns

An agent Task whose turn [parked](parking-agent-turns.md) ends with phase `Succeeded`
for its own turn while the work continues in a continuation Task. Its notice is
therefore not sent when it reaches that phase. It follows `status.wait` instead:

| `status.wait.state` | Notice from the parked Task |
| --- | --- |
| `Requested`, `Waiting`, `Resuming` | None yet. Nothing is recorded in `status.webhookDelivery`, and the Task's cleanup and history pruning are not held up. |
| `Resumed` | None. The continuation copies `spec.webhookURL` and sends the notice when it finishes. If it parks too, the rule repeats, so the newest Task of the chain reports the outcome. |
| `Cancelled` | One `task.cancelled` notice. |
| `Expired` | One `task.failed` notice, as for a Task that runs past its deadline. |
| `Abandoned` | One notice for the Task's phase if the turn itself failed or was cancelled. If the turn succeeded but its continuation could not be created, one `task.failed` notice. |

So a receiver sees exactly one notice per chain, and `task.succeeded` only when the
work finished. In a notice decided by the wait, `phase` is still the parked Task's
own phase (`Succeeded`) and `taskName` is the parked Task; read its `status.wait` for
the reason. A wait left over from an earlier attempt that a later, unparked attempt
replaced does not change the event. Notices from continuations name the continuation
Task, whose UID differs from the Task you created.

As with any Task, deleting the parked Task or its continuation drops its pending
notice. A notice that an earlier controller version left pending for a parked Task is
never sent as `task.succeeded`: it is superseded when the wait ends without resuming,
and marked `Failed` without being sent when the wait resumed.

## Approval events

List approval events in `spec.webhookEvents` to be told when the Task asks a
person to approve something and how that ended. Terminal notices are always
sent and cannot be listed or turned off; leaving `webhookEvents` empty or
absent keeps terminal notices only. A Task with `webhookEvents` must set
`webhookURL`. Continuations of a parked turn copy both fields.

```yaml
spec:
  webhookURL: https://hooks.example.com/orka
  webhookEvents:
    - task.approval_requested
    - task.approval_resolved
```

The approvals come from the approval gate, `request_approval`, brokered ACP tool
calls, and [parked sign-offs](parking-agent-turns.md). A runtime's own
permission prompt, which Orka answers from policy, is not an approval and sends
nothing.

Approval notices use the same signing, headers, destination rules and leases
as terminal notices. The JWS `event` claim is the approval event. The body is
different and versioned:

```json
{"schemaVersion":"orka.ai/task-approval-webhook/v1",
 "deliveryId":"2b7c...","eventType":"task.approval_resolved","occurredAt":"2026-11-16T09:01:30Z",
 "taskUID":"0b9e0f43-...","taskName":"release","taskNamespace":"team-a","streamSeq":42,
 "approvalId":"acp-tool-approval-v2:sha256:...","targetTool":"github.create_pr","severity":"warning",
 "expiresAt":"2026-11-16T09:10:00Z","resolution":"approved","parked":false}
```

| Field | Meaning |
| --- | --- |
| `schemaVersion` | `orka.ai/task-approval-webhook/v1`. Reject versions you do not know. |
| `deliveryId` | Deterministic per approval and transition, so a restarted controller sends the same ID. |
| `occurredAt` | When the transition was recorded: the request, or the decision, expiry or cancellation. |
| `streamSeq` | Position of that transition in the Task's approval history; order notices of one Task by it. |
| `approvalId` | The approval to look up in `GET /api/v1/tasks/{task}/approvals`. Absent on `task.approval_overflow`. |
| `targetTool`, `severity` | The gated tool and severity label, when the approval has them. |
| `expiresAt` | Present only when the approval really expires then: brokered ACP reviews and parked sign-offs. |
| `resolution` | On `task.approval_resolved`: `approved`, `declined`, `expired`, or `cancelled`. |
| `parked` | `true` for a parked turn's sign-off. |

A notice never carries the argument preview, the action or risk text, the
reviewer's reason or identity, or any digest. Fetch details from the approvals
API, which checks the caller's access.

How approvals reach a receiver:

- **Requested** is sent once per approval. **Resolved** is sent once, for the
  first decision. A brokered review that passes its `expiresAt` resolves as
  `expired` once the controller records it, a few seconds after the deadline.
  An approval gate request that is still pending when its Task ends gets no
  resolved notice, because nothing resolves it. A parked sign-off stays open
  after its turn `Succeeded`, so its resolution arrives while the parked Task
  is finished.
- Notices of one Task are sent in `streamSeq` order, each after the previous
  one settles. A request whose notice failed does not hold back its resolution.
- An approval notice gets **5 attempts**, retried after 10s, 20s, 40s and 60s,
  so it settles within about three minutes. Review windows are short, and a
  late alert is of little use.
- While no signing key is available, an approval notice waits without using
  an attempt, as a terminal notice does, but only until its `signingDeadline`,
  five minutes after it was queued. A notice still waiting then is marked
  `Failed` unsent, with `lastError` "webhook signing key unavailable past the
  approval notice window", and is never sent or recreated. Later notices then
  go ahead once a key is back. Terminal notices keep waiting until a key is
  available.
- Selecting approval events on a Task whose spec can still change, or
  selecting them again after removing them all, does not replay earlier
  approval history. Notices start with the first transition recorded after
  the controller sees the selection. Removing an event retires its pending
  notices unsent.
- Deleting a Task drops its pending approval notices. Scheduled-run history
  pruning waits for them: a run, or any Task in its continuation chain, with
  a pending approval notice is kept past `successfulRunsHistoryLimit` or
  `failedRunsHistoryLimit` until the notice is delivered or marked `Failed`,
  and is then pruned.
- A Task with the same name as an earlier one never receives the earlier Task's
  approvals.

Receivers should:

1. Verify the signature exactly as for terminal notices.
2. Deduplicate on `deliveryId`. Delivery is at-least-once, not exactly-once.
3. Order by `streamSeq`, and tolerate gaps and a resolution without its request.
4. Treat a notice as an alert, never as authority: decide through the approvals
   API, which revalidates the approval.

Status records the queue and its cursor:

```yaml
status:
  approvalWebhooks:
    projectedSeq: 42
    tracking: true
    notices:
      - deliveryID: 2b7c...
        eventType: task.approval_resolved
        transition: Resolved
        approvalID: acp-tool-approval-v2:sha256:...
        resolution: approved
        streamSeq: 42
        state: Delivered      # Pending, Delivered, or Failed
        attempts: 1
        signingDeadline: "2026-11-16T09:06:30Z"
```

The queue holds at most 16 notices. Delivered and failed notices are pruned,
oldest first, to make room; a pending notice is never dropped. If 15 notices
are pending at once, the controller records one `task.approval_overflow` notice
whose `streamSeq` is the first transition it did not notify, sets
`overflowed: true`, and notifies no later transition for that Task. `omitted`
counts the transitions it skipped. Read the approvals API after an overflow
notice. A signing outage alone overflows the queue only if 15 notices are
queued within five minutes of each other. Each notice is marked `Failed` at
its signing deadline, before new transitions are queued, and frees its slot.

## Signing keys and rotation

The controller signs with the Ed25519 key in the `current` item of a Secret in
a namespace of its own: `<release namespace>-task-webhook-signing` for the Helm
chart (override with `controller.taskWebhooks.signingNamespace`), or
`orka-task-webhook-signing` for the kustomize install (flag
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
NS=orka-task-webhook-signing      # <release namespace>-task-webhook-signing for Helm
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
`get` and `update` on that one Secret, and `create` on Secrets there. This
closes two paths:

- A Task's or Agent's `secretRef` mounts a Secret from the Task's namespace.
  Tasks live in the release namespace, so a `secretRef` cannot name the key.
- Worker roles are bound in the release namespace only, so no worker can read
  or replace the key with its own permissions.

The protection also depends on no worker being able to act as the controller.
The default AI worker RBAC profile lets the AI worker create Secrets and Jobs in
the release namespace (for `code_exec` and child Task credentials). A
ServiceAccount-token Secret or a Job could name the controller's
ServiceAccount, and a Job's Pod could carry the controller's labels, which
would put it behind the API Service that publishes the JWKS. The
`<fullname>-ai-worker-identity-protection` ValidatingAdmissionPolicy, which
both installs ship, blocks all three. The AI worker may only write Opaque
Secrets, may only create or delete `code_exec`'s own ServiceAccounts (so it
cannot replace a platform account and inherit its RoleBindings), and may only
create `code_exec` Jobs: each runs as its own per-run
ServiceAccount with no token mounted, carries only `code_exec` labels, uses
only emptyDir and Secret volumes, gets no host namespaces, privilege or
capabilities, and runs under a `RuntimeDefault` or `Localhost` seccomp
profile. Keep that policy installed (Helm `workers.ai.identityProtection`
defaults to `true`; turning it off reopens the path to the controller's
identity under the default worker profile), and:

- Do not grant the AI worker other ways to run Pods or request tokens. An
  OutboundAccessPolicy `serviceAccountRef` grants the AI worker TokenRequest
  for that account, so the controller refuses Orka's own ServiceAccounts
  there: any labeled `orka.ai/platform-identity: "true"`, plus the controller
  and worker accounts it is configured with, labeled or not. The AI worker's
  own account is allowed, since a token for itself gives it nothing new.
- Do not keep ServiceAccount-token Secrets in the release namespace. The
  default AI worker profile can read every Secret there, so any such Secret
  hands the worker that account's token.

The controller refuses to start with a signing namespace that is the watch,
controller Pod, ACP runtime or agent-host namespace. The chart also refuses the
client namespace when rendering; the controller cannot see that one, so
kustomize and other installs must keep it apart themselves. Keep the signing
namespace free of other workloads and of role bindings beyond the
controller's and your cluster administrators'.
