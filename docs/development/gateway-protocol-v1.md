# `orka.gateway.v1` adapter protocol and security contract

`orka.gateway.v1` is the provider-neutral boundary between Orka and out-of-tree external adapters. Provider SDKs, raw provider payloads, and provider credentials stay at the adapter edge.

## Compatibility and evolution

`protocolVersion` is an exact contract discriminator, not a minimum version. The V1 controller accepts only `orka.gateway.v1` on capability, event, and delivery envelopes. It rejects unknown JSON fields, trailing JSON values, missing required fields, and responses above the documented bounds. Consequently, adding an unrecognized field is not a forward-compatible extension of V1.

`adapterVersion` is bounded, sanitized operator metadata. It is not parsed as SemVer and does not negotiate behavior. Capability flags describe behavior of the currently connected adapter and gate Gateway/Binding readiness; they do not authorize a different protocol version.

Changes to required fields, field meaning, delivery statuses, authentication, idempotency semantics, or capability interpretation require a new protocol version. A future version must define an explicit controller/adapter overlap period and downgrade behavior before mixed-version rolling upgrades are supported. Until then, independently rolled controller and adapter builds are compatible only when both continue to emit exactly `orka.gateway.v1` and preserve the V1 semantics in this document.

The `gateway.orka.ai/v1alpha1` Kubernetes resources and the controller SQLite schema are separate compatibility surfaces from the adapter protocol. Apply CRDs before a controller upgrade, back up the WAL-consistent database plus related Kubernetes objects, and do not run an older controller against a forward-migrated database unless the release explicitly documents that rollback as safe. Operational backup, migration, version-skew, and rollback procedures are documented in `website/docs/operations/gateways.md`.

`GatewayRoute`, standalone `GatewayPolicy`, `GatewayContext`, and approval-created bindings are intentionally outside this V1 adapter contract and remain deferred resource seams.

## Adapter endpoints

Adapters expose these HTTP API routes relative to their configured HTTPS base URL:

- `GET /v1/health` → `{"status":"ok"}`
- `GET /v1/capabilities` → the contract version, sanitized adapter name/version, and provider-neutral capabilities
- `POST /v1/deliveries` → one synchronous terminal result

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
    "idempotentDelivery": true
  }
}
```

Delivery responses are exactly one of:

```json
{"status":"delivered","providerMessageId":"safe-stable-id"}
{"status":"retryableError","message":"temporary failure"}
{"status":"nonRetryableError","message":"unsupported target"}
```


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

`--reference-fixtures` is appropriate only for the bundled reference adapter; omit it for third-party adapters.

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

## Bounds

- request body: 256 KiB;
- text: 64 KiB;
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

## Failure and recovery

Events and deliveries use expiring claims. A controller crash may replay Task creation or adapter delivery with the same deterministic IDs. Unauthorized or ambiguous events create no Session and no Task. When a validated reply target exists, Orka may enqueue a generic denial delivery that does not expose binding names, internal errors, endpoints, tokens, or provider-native identifiers.
