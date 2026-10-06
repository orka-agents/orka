# `orka.gateway.v1` adapter protocol and security contract

`orka.gateway.v1` is the provider-neutral boundary between Orka and out-of-tree external adapters. Provider SDKs, raw provider payloads, and provider credentials stay at the adapter edge.

## Compatibility and evolution

`protocolVersion` is an exact contract discriminator, not a minimum version. The V1 controller accepts only `orka.gateway.v1` on capability, event, and delivery envelopes. It rejects unknown JSON fields, trailing JSON values, missing required fields, and responses above the documented bounds. Consequently, adding an unrecognized field is not a forward-compatible extension of V1.

`adapterVersion` is bounded, sanitized operator metadata. It is not parsed as SemVer and does not negotiate behavior. Capability flags describe behavior of the currently connected adapter and gate Gateway/Binding readiness; they do not authorize a different protocol version.

Changes to required fields, field meaning, delivery statuses, authentication, idempotency semantics, or capability interpretation require a new protocol version. A future version must define an explicit controller/adapter overlap period and downgrade behavior before mixed-version rolling upgrades are supported. Until then, independently rolled controller and adapter builds are compatible only when both continue to emit exactly `orka.gateway.v1` and preserve the V1 semantics in this document.

The bounded interim-delivery extension ([ADR 0032](../adr/0032-gateway-interim-delivery.md)) adds optional `interimDelivery` and delivery kind `message` without changing existing fields, `final`/`error` semantics, or response statuses. **Roll controllers first, then enable adapter advertisement.** Old adapters work unchanged with the new controller. Older strict controllers reject the new capability field: there is no reverse-skew guarantee. Before rollback, settle/abandon outstanding messages with the new controller and disable advertisement (`--interim-delivery=false` on the reference adapter); do not leave pending message rows for an old dispatcher.

The `gateway.orka.ai/v1alpha1` Kubernetes resources and the controller SQLite schema are separate compatibility surfaces from the adapter protocol. Apply CRDs before a controller upgrade, back up the WAL-consistent database plus related Kubernetes objects, and do not run an older controller against a forward-migrated database unless the release explicitly documents that rollback as safe. Operational backup, migration, version-skew, and rollback procedures are documented in `website/docs/operations/gateways.md`.

`GatewayRoute`, standalone `GatewayPolicy`, `GatewayContext`, and approval-created bindings are intentionally outside this V1 adapter contract and remain deferred resource seams.

## Adapter endpoints

Adapters expose these HTTP API routes relative to their configured HTTPS base URL:

- `GET /v1/health` → `{"status":"ok"}`
- `GET /v1/capabilities` → the contract version, sanitized adapter name/version, and provider-neutral capabilities
- `POST /v1/deliveries` → one synchronous delivery receipt for `final`, `error`, or capability-gated `message`

All three endpoints use the Gateway's outbound bearer Secret. Delivery requests are idempotent by `deliveryId`/`idempotencyId`; replaying one ID must return the original provider message correlation without a second provider-side send.

A capability response uses:

```json
{
  "protocolVersion": "orka.gateway.v1",
  "adapterName": "example-adapter",
  "adapterVersion": "v1.2.3",
  "capabilities": {
    "inboundText": true,
    "outboundText": true,
    "threads": true,
    "senderIdentity": true,
    "explicitSessions": false,
    "idempotentDelivery": true,
    "interimDelivery": true
  }
}
```

`interimDelivery` is optional: omit it (or return false to a supporting controller) for final/error-only operation. The controller sends `kind: "message"` only when a current, ready adapter advertises true. Messages use the same delivery envelope and controller-derived reply routing as terminal deliveries, with a 16 KiB text bound instead of 64 KiB. They do not complete the Task/event, write canonical transcript messages, or release the Session lock.

Delivery responses are exactly one of:

```json
{"status":"delivered","providerMessageId":"safe-stable-id"}
{"status":"retryableError","message":"temporary failure"}
{"status":"nonRetryableError","message":"unsupported target"}
```


Messages for one event are sent in admission order before its terminal delivery. A live predecessor blocks later sends; a permanent, exhausted, or expired predecessor is abandoned so terminal delivery can proceed. A new message must not appear after terminal. Replaying an already-delivered message ID after terminal is still idempotent and must return the original receipt without a new send. The controller prevents manually retrying a message once any later delivery has started, including uncertain or subsequently expired attempts. Terminal retry behavior is unchanged.

### Optional reference-adapter fixture profile

The bundled reference adapter and conformance CLI can opt into a non-normative fixture profile with `--reference-fixtures`. In that mode only, delivery metadata `fixture=retryable` and `fixture=permanent` requests deterministic error-classification responses. Third-party adapters are not required to implement these fixture keys.

### Reference adapter and conformance tooling

For local protocol development, run the reference adapter over plain HTTP:

```bash
ORKA_GATEWAY_BEARER_TOKEN='local-test-token' \
  go run ./cmd/orka-gateway-reference-adapter --listen :8090
```

Plain HTTP is only for direct local development. Configured Gateway `endpoint` and `serviceRef` targets require HTTPS. To serve TLS directly, provide the certificate and key together; supplying only one is rejected:

```bash
ORKA_GATEWAY_BEARER_TOKEN='outbound-bearer-token' \
  go run ./cmd/orka-gateway-reference-adapter \
  --listen :8443 \
  --tls-cert-file /path/to/tls.crt \
  --tls-key-file /path/to/tls.key
```

For a `serviceRef`, the server certificate must be valid for `<service>.<namespace>.svc`, and the signing CA must be trusted by the Orka controller. The adapter can also be built as a non-root container image:

```bash
docker build \
  -f cmd/orka-gateway-reference-adapter/Dockerfile \
  -t orka-gateway-reference-adapter:dev .
```

Run conformance from a network location that can reach the adapter. When a private CA is not already trusted by the host, point `SSL_CERT_FILE` at its certificate:

```bash
SSL_CERT_FILE=/path/to/ca.crt \
ORKA_GATEWAY_BEARER_TOKEN='outbound-bearer-token' \
  go run ./cmd/orka-gateway-conformance \
  --endpoint https://gateway-adapter.example.com:8443 \
  --reference-fixtures
```

`--reference-fixtures` is appropriate only for the bundled reference adapter; omit it for third-party adapters. The reference adapter advertises interim support by default. Use `--interim-delivery=false` to omit the capability for legacy controllers/fixtures. It keeps at most 1,024 distinct delivery identities (including failed fixture attempts) in memory, rejects new IDs at capacity, and never evicts existing receipts to make room. It is a deterministic fixture, not a durable production adapter.

Full conformance is mutating. When interim support is advertised it sends two distinct messages, a duplicate of the first, then the existing final and its duplicate, then replays the first message after final. It also sends one oversized-message rejection probe. Without the capability, it sends **no message probes**, including negative probes; the existing final-only checks remain. Readiness `Probe` stays non-mutating.

`--delivery-fixture /path/to/private.json` supplies authorized routing only (see [operations](../../website/docs/operations/gateways.md#upgrade-compatibility-and-version-skew)). It cannot supply text or credentials and retains all result masking. Capable adapters can produce **three real visible sends** per invocation, all with the fixed safe text `[Orka conformance check] No action required.` Use an explicitly approved destination and an originating event that has not yet received terminal delivery. Do not fabricate or alter provider identities to avoid this constraint. Reusing an already-closed event may be rejected even though the private-fixture run generates fresh delivery IDs.

## Inbound Orka endpoint

Adapters call:

```text
POST /api/v1/gateways/{namespace}/{name}/events
Authorization: Bearer <Gateway inbound token>
Content-Type: application/json
```

Example normalized text event:

```json
{
  "protocolVersion": "orka.gateway.v1",
  "externalEventId": "stable-provider-event-id",
  "eventType": "text",
  "accountId": "stable-account-id",
  "contextId": "stable-conversation-id",
  "threadId": "optional-thread-id",
  "sender": {"id":"stable-sender-id","displayName":"Safe display name"},
  "text": "Message text",
  "replyTarget": "normalized-reply-target",
  "occurredAt": "2026-07-16T07:00:00Z",
  "metadata": {"tenantTier":"internal"}
}
```

The GatewayClass must allow every metadata key. Orka never accepts or stores a raw provider request.

Durable admission returns HTTP `202` with `accepted`, `duplicate`, `rejected`, or `deadLettered` plus the stable Orka event ID. Authentication and malformed envelopes return normal `4xx` errors and are not durably admitted.

## Gateway-only agent tool

The production native AI worker and ACP broker expose `reply_in_conversation` only for an authenticated gateway-origin Task whose tool and transaction policies permit it. Invocation is content-only:

```json
{"content":"A bounded intermediate update"}
```

The schema requires a string `content` with `minLength: 1`, `maxLength: 16384`, and `additionalProperties: false`. Execute independently rejects unknown/duplicate fields, invalid Unicode, empty or sanitized-empty text, and content above **16384 UTF-8 bytes**. The schema's character bound is not a substitute for the byte check. No destination, request ID, quota override, or approval argument is accepted. The configurable controller lifetime cap defaults to 10; it is not a model parameter.

Eligibility requires exact durable event/Task UID ownership, not a prompt, tool name, environment flag, or provenance label alone. Ordinary AI/ACP Tasks, delegated children, container Tasks, and compatibility-proxy callers do not receive the tool. Explicit denials, closed tool lists, and transaction scopes still apply. Pending durable linkage defers configuration rather than freezing an execution without the tool. Current adapter readiness/capability is checked at admission, independently of origin.

The host supplies a stable logical operation ID. The tool derives a domain-separated, bounded request ID from namespace, Task UID, and that operation ID. Native hosts scope it to the execution/model turn/call; ACP uses its sealed broker operation ID, never the raw JSON-RPC ID. The same logical call can replay without another quota charge; distinct calls with identical text remain distinct. Regenerating a call after restart is not exactly-once recovery.

Execute reads the authoritative accepted-count/limit/replay snapshot before enqueueing. This read is neither a reservation nor an admission grant: the controller independently authorizes and atomically enforces quota on enqueue. Existing requests can replay at exhaustion. Success is the normal tool envelope containing only a receipt:

```json
{"success":true,"data":{"deliveryID":"gdm-example","status":"Pending","created":true}}
```

It confirms durable acceptance, **not provider delivery**, and does not stop the normal tool loop, complete the Task, append final history, or release the Session. Known admission rejections produce controlled model-visible errors. Unknown backend/transport failures remain ambiguous; do not regenerate a call to work around them. ACP also retains its consequential external-effect replay ledger and `OutcomeUnknown` fence. Approval behavior is unchanged.

### ACP broker authorization

ACP freezes reply policy/descriptors before session creation. Built-in providers retain their normal native defaults when reply is added. External runtime profiles must explicitly include it and exactly match the registered, conformed policy. An external profile without it remains final-only, and an existing frozen session is not silently upgraded.

The production ACP registry injects a request-bound sender only after broker authorization against that frozen policy. Budget and enqueue each use the existing signed operation capability, active prompt/session guard, exact Task UID checks, and authorized task-data transaction. Live preparation is outside the SQLite writer; durable reads/admission run inside the writer under the mutation fences. ACP never calls the native worker routes or impersonates a Job. Its authority is the broker/prompt context plus durable origin, not Pod authentication.

## Internal native worker reply endpoints

These are execution-host APIs, not model or operator/admin send APIs. All require the current worker Pod-bound ServiceAccount token and exact native Task/Job/Pod identity. ACP uses the broker path above.

### Origin and budget reads

```text
GET /internal/v1/tasks/{namespace}/{taskName}/gateway-messages/origin
Authorization: Bearer <current worker Pod-bound ServiceAccount token>
```

Successful origin bootstrap returns HTTP 200 with only `{"taskUID":"<exact Task UID>"}`. The production client requires that UID to match its controller-supplied Task identity. This read proves active durable origin without requiring current readiness/interim capability; it is not permission to enqueue. Job publication can lag Pod startup, so the client retries only 503, at most five attempts within ten seconds. Origin-route TokenReview backend failure is reported as 503, never authenticated identity; an invalid token remains 401. Transient bootstrap unavailability omits the optional tool while ordinary model work continues. Explicit identity denial and malformed success fail closed. There is no late tool upgrade during that execution.

```text
GET /internal/v1/tasks/{namespace}/{taskName}/gateway-messages/budget?requestID=<host-derived-id>
Authorization: Bearer <current worker Pod-bound ServiceAccount token>
```

A successful budget read returns HTTP 200, for example `{"accepted":1,"limit":10,"requestExists":false}`. It exposes no content or routing, uses the same durable identity/replay population as enqueue, and retains native authorization plus inside-writer Job revocation checks. Current admission gates apply to new IDs; an authorized retained receipt remains recognizable at exhaustion or after capability/readiness withdrawal. This snapshot can become stale immediately: only enqueue performs final atomic admission.

The reusable native client reads the projected token file on every request, disables environment proxies and redirects, and bounds controller responses. It does not accept a raw environment token override or automatically retry message POSTs.

### Message admission

```text
POST /internal/v1/tasks/{namespace}/{taskName}/gateway-messages
Authorization: Bearer <current worker Pod-bound ServiceAccount token>
Content-Type: application/json
```

```json
{"content":"A bounded intermediate update","requestID":"stable-internal-call-id"}
```

Only the authentic current native worker Pod/Job for the exact gateway-created Task UID is authorized. No target/routing arguments are accepted. The Task must be Running and the admitted event still eligible. The endpoint sanitizes text after checking the raw UTF-8/size bound. The stable request ID is at most 256 bytes; reuse it for a retry, not for different content. Here, “different content” means different **sanitized delivery text**, not different raw request bytes. Distinct raw values that sanitize to the same text are a replay with no additional enqueue or quota charge. Only sanitized text is retained; no raw-content digest or additional persisted identity is stored.

A newly enqueued message returns **HTTP 202**; a replay with the same sanitized content returns **HTTP 200** with the same ID, current durable status, and `created: false`:

```json
{"deliveryID":"gdm-example","status":"Pending","created":true}
```

This acknowledges admission, not provider delivery. An authenticated replay can recover an existing receipt even if the same Gateway is now unready, has a stale readiness observation, or no longer advertises the supported contract/capability. It does not requeue or modify the delivery. Exact live Task/namespace/Gateway identity and generation fences still apply; the Task must remain Running, and worker Job revocation still denies receipt recovery. Changed sanitized content still conflicts. A new request ID must pass the current admission gate: missing/false capability on a ready current Gateway returns **HTTP 409** with no enqueue:

```json
{"error":{"code":"interim_delivery_unsupported","message":"gateway adapter does not advertise interimDelivery capability"}}
```

Errors produced by the message handler use the same `error.code`/`error.message` shape with string codes: `invalid_request` (400), `unauthorized` (401), `forbidden` (403), `not_found` (404), `conflict` (409, including changed sanitized content or lifecycle conflict), `too_large` (413), `limit_reached` (429), `unavailable` (503), or `internal_error` (500). Authentication can fail before the message handler runs: the shared auth middleware uses the common error envelope with a numeric HTTP status in `error.code` (for example, `{"error":{"code":401,"message":"missing authorization header"}}`), not the handler's string `unauthorized` code. A terminal worker loses authorization; do not rely on replay to authorize a completed Task. For new admissions, transient unready/stale observations return `unavailable` (503), not a current adapter's unsupported-capability error.

## Verification boundaries

The Gateway live E2E workflow retains three paths: external-v2 final-only with no Job, native production-tool acceptance/replay before final, and a no-capability native rejection followed by final. The deterministic native fixture executes the actual `ReplyInConversationTool` and `workerclient` with a real controller-created Pod/Job token; it is not a model-driven production worker. The suite requires the interim reference-adapter receipt while the Task is still Running before releasing its final fence.

Native worker tests separately exercise actual tool visibility, dispatch, and normal-loop continuation. ACP tool proof is an integration path with real SQLite, a signed broker capability, an active prompt/session guard, and no native Job; Kubernetes and credential-resolution fixtures are not a live ACP provider. Local fixture tests and tagged E2E compilation do not establish live delivery. Only an executed cluster suite establishes that result, and the current Gateway workflow does not claim a live ACP tool invocation.

## Bounds

- request body: 256 KiB;
- ingress and terminal (`final`/`error`) text: 64 KiB;
- interim (`message`) text: 16 KiB UTF-8;
- accepted interim messages per Task: 10 by default, `--gateway-interim-messages-per-task` (positive values; failed/expired messages still count, retries do not);
- event and identity fields: 256 bytes;
- metadata: 32 keys, 256 bytes per key/value;
- adapter response: 64 KiB;
- pending events per Session: 100;
- retained operational event records per Gateway: 1,000 by default;
- rejected-event audit records per Gateway: a separate 250-record budget by default;
- event/delivery expiry: 24 hours;
- delivery call timeout: 15 seconds;
- delivery attempts: 10;
- terminal retention: 30 days by default.

## Authentication and endpoint policy

Inbound and outbound directions use different same-namespace Secrets.

Inbound Secret metadata:

```yaml
metadata:
  labels:
    gateway.orka.ai/inbound-auth: "true"
    # Optional selector-safe form of the Gateway name.
    gateway.orka.ai/gateway-name: <Gateway name or selector-safe hash>
  annotations:
    gateway.orka.ai/gateway-name: <exact Gateway name>
```

Outbound Secret metadata:

```yaml
metadata:
  labels:
    gateway.orka.ai/outbound-auth: "true"
    gateway.orka.ai/gateway-name: <Gateway name or selector-safe hash>
  annotations:
    gateway.orka.ai/gateway-name: <exact Gateway name>
    gateway.orka.ai/adapter-endpoint: <exact resolved endpoint>
```

The bearer value is read from the configured key and compared in constant time. Secret values and authorization headers must never be logged or copied into Tasks, status, events, or delivery records.

Direct endpoints and `serviceRef` endpoints require HTTPS and reject credentials, query strings, and fragments. Direct endpoints may resolve only to public unicast addresses; local, private, link-local, reserved, and Kubernetes Service targets are rejected on every dial, with proxies and redirects disabled to prevent DNS rebinding and controller-side SSRF. A `serviceRef` resolves to the same-namespace Service DNS name; the adapter must present a certificate trusted by the Orka controller and valid for that name. Selector presence is a routing constraint, not workload authentication.

## Routing and identity

Bindings match exact normalized `accountId` and `contextId`, with optional exact thread and sender constraints. Sender policy defaults to `allowlist`; `all` is an explicit trusted-context opt-in. The highest-priority authorized binding wins. Equal-priority overlap fails closed.

Session modes are `ephemeral`, `context`, `thread`, `sender`, `context-sender`, `thread-sender`, and `explicit`. New messages for a busy Session remain FIFO queued. Gateway-created Tasks keep `spec.prompt` empty so external message text is not copied into the Task CR. They consume bounded canonical Session input through `sessionRef.promptIncluded` and `sessionRef.throughMessageId`, while carrying only safe correlation, a Gateway-scoped `requestedBy`, the bound Agent, and bounded Task defaults.

The bound Agent selects execution at Task creation: no `spec.runtime` yields a native `type: ai` Task; a configured runtime yields `type: agent`. Native AI uses the Agent's model/provider configuration and does not accept the runtime-only `taskDefaults.agentRuntimeMaxTurns`. This selection does not change the V1 admission or delivery envelopes.

Admission preserves Agent UID identity, not Agent generation or execution kind. Edits before creation may affect execution selection; recovery of an existing deterministic Task preserves its immutable kind. External-runtime tool policy already frozen in the event cannot be dropped by switching to native AI.

## Failure and recovery

Events and deliveries use expiring claims. A controller crash may replay Task creation or adapter delivery with the same deterministic IDs. Both native AI and runtime-backed Tasks leave canonical terminal transcript append, delivery creation, and Session lock release to gateway terminal projection. A native AI worker requires a non-empty final user turn in its transcript when `sessionRef.promptIncluded` is set; a missing transcript cannot fall back to a direct prompt. Unauthorized or ambiguous events create no Session and no Task. When a validated reply target exists, Orka may enqueue a generic denial delivery that does not expose binding names, internal errors, endpoints, tokens, or provider-native identifiers.
