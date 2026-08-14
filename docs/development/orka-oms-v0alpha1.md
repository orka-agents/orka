# Orka Memory Service profile `orka.oms.v0alpha1`

## Status and scope

This document is the normative wire contract for the initial Orka Memory
Service (OMS) adapter profile. The profile string is exactly:

```text
orka.oms.v0alpha1
```

The key words **MUST**, **MUST NOT**, **REQUIRED**, **SHOULD**, and **MAY** are
to be interpreted as requirements. The profile is closed: implementations MUST
reject unknown versions, unknown or duplicate JSON fields, missing required
fields, trailing JSON values, oversized bodies, invalid UTF-8, unsafe control
characters, invalid identities, and non-canonical mutation envelopes.

The profile supplies the provider-side semantics required by an activated Orka
memory backend:

- exact binding identity echo;
- finite capability validation;
- one exclusive writer claim per authority scope;
- a durable routing fence;
- durable operation-ID idempotency and digest-conflict detection;
- create-if-absent and conditional replace/delete;
- monotonic generation and delete high-watermark fencing;
- exact record and operation lookup;
- stable, bounded, restart-safe keyword pagination;
- explicit behavior for optional semantic and hybrid search.

This profile does not define Orka's public memory API, governance catalog,
admission ledger, or a provider's native API. It is the boundary between Orka and an
OMS adapter.

## Transport and authentication

Production traffic MUST use HTTPS. Every endpoint requires the HTTP `Authorization` header with the `Bearer`
scheme and the runtime value loaded from the configured secret reference. The
wire contract never embeds an example credential value.

The adapter compares the configured value without prefix timing leaks. Missing
or invalid authentication returns `401` and MUST NOT echo the configured value.

Every POST request uses `Content-Type: application/json`. A charset parameter is
allowed by the media-type parser, but the JSON bytes themselves MUST be valid
UTF-8. Redirects are not part of the profile and clients MUST NOT follow them.
Responses use `application/json`, `Cache-Control: no-store`, and are bounded by
the advertised and hard profile response limits.

`GET /v1/health` is an authenticated transport-liveness exception. It does not
read or mutate authority state and therefore carries no binding.
`POST /v1/stores/resolve` carries a pre-authority store-resolution binding that
has cluster, namespace, backend, and tenant identity but intentionally has no
authority/routing epochs or store UUID. Every post-resolution profile request
carries a complete authority `binding`, and every semantic response echoes the
exact request binding. An error produced before a binding can be decoded has
`"binding": null`.

## Hard profile limits

These are maxima, not defaults. A capability response MAY advertise smaller
positive values and MUST never advertise larger values.

| Item | Hard maximum |
|---|---:|
| Request body | 2,097,152 bytes (2 MiB; covers the worst-case JSON encoding of 256 KiB content plus the bounded mutation envelope) |
| Response body | 4,194,304 bytes (the remote-response hard cap) |
| Content | 262,144 UTF-8 bytes (64 KiB Orka default; 256 KiB hard cap) |
| Identity | 256 bytes |
| Operation ID | 128 bytes |
| Tags per record | 64 |
| Tag | 128 UTF-8 bytes |
| Metadata entries | 32 |
| Metadata key | 64 bytes |
| Metadata value | 1,024 UTF-8 bytes |
| Search query | 1,024 UTF-8 bytes |
| Page size | 8 records |
| Persisted records per snapshot | 1,024 |
| Error message | 512 UTF-8 bytes |

Content and queries MAY contain tab, carriage return, and newline. Other Unicode
control characters are forbidden. Identities are ASCII and match:

```text
[A-Za-z0-9][A-Za-z0-9._:-]*
```

Operation IDs match `mop-[A-Za-z0-9][A-Za-z0-9._-]*`. Memory IDs match
`mem-[A-Za-z0-9][A-Za-z0-9._-]*`.

Counters and epochs are positive values no greater than signed 64-bit maximum,
except `expectedGeneration`, which MAY be zero.

## Strict JSON representation

All fields shown in this document are required, including fields whose value is
an empty string, empty array, empty object, `false`, zero, or `null`. Omitting a
field is not equivalent to supplying its empty representation.

In particular:

- `expectedBackendVersion` is `""` when no provider version is supplied;
- create/replace `state` is an object;
- delete `state` is explicit JSON `null`;
- live `tags` and `metadata` are explicit arrays/objects, including `[]`/`{}`;
- tombstone content is `""`, tags are `[]`, and metadata is `{}`;
- first-page `pageToken` is `""`;
- absent exact-get records and operation receipts are explicit `null`;
- an exhausted page has `"exhausted": true` and `"nextPageToken": ""`.

Timestamps use RFC 3339 JSON strings and MUST be non-zero. Adapter timestamps
SHOULD be UTC.

## Binding identity

Store resolution happens before authority epochs and the stable store UUID
exist. Its pre-authority binding is:

```json
{
  "clusterId": "cluster-1",
  "namespaceUid": "namespace-uid-1",
  "backendUid": "backend-uid-1",
  "tenantId": "orka-tenant-sha256:<64 lowercase hex>"
}
```

It deliberately omits `authorityEpoch`, `routingEpoch`, and `storeUuid`.

Every post-resolution authority operation carries:

```json
{
  "clusterId": "cluster-1",
  "namespaceUid": "namespace-uid-1",
  "backendUid": "backend-uid-1",
  "authorityEpoch": 3,
  "routingEpoch": 7,
  "tenantId": "orka-tenant-sha256:<64 lowercase hex>",
  "storeUuid": "store-uuid-1"
}
```

`tenantId` is not caller-selected. It is:

```text
"orka-tenant-sha256:" + lowercase_hex(
  SHA-256(UTF8(clusterId) || 0x00 || UTF8(namespaceUid))
)
```

The canonical upsert key is:

```text
orka:<clusterId>:<namespaceUid>:<authorityEpoch>:<memoryId>
```

The binding digest is `sha256:` followed by lowercase hex SHA-256 over the
canonical JSON encoding of all binding fields, including `routingEpoch`.

The durable authority identity is the same encoding without `routingEpoch`.
The exclusive claim scope consists of `tenantId`, `storeUuid`, and
`authorityEpoch`; `backendUid` is deliberately excluded from the scope so a
second backend UID conflicts instead of creating a parallel writer.

A binding mismatch is non-retryable and no content may be accepted or returned.
An exact replay of an already-completed operation is the only exception to a
later routing fence: it returns the original receipt and performs no write.

## Endpoints

### `GET /v1/health`

Authenticated transport liveness only.

```json
{
  "protocolVersion": "orka.oms.v0alpha1",
  "status": "ok"
}
```

### `POST /v1/stores/resolve`

This strict, authenticated endpoint resolves an immutable operator-selected
store name before an authority epoch is allocated.

Request:

```json
{
  "protocolVersion": "orka.oms.v0alpha1",
  "binding": {
    "clusterId": "cluster-1",
    "namespaceUid": "namespace-uid-1",
    "backendUid": "backend-uid-1",
    "tenantId": "orka-tenant-sha256:<64 lowercase hex>"
  },
  "storeName": "production-memory"
}
```

Response:

```json
{
  "protocolVersion": "orka.oms.v0alpha1",
  "binding": {
    "clusterId": "cluster-1",
    "namespaceUid": "namespace-uid-1",
    "backendUid": "backend-uid-1",
    "tenantId": "orka-tenant-sha256:<64 lowercase hex>"
  },
  "storeName": "production-memory",
  "storeUuid": "d4a8d58c-90c4-4e80-8d4e-a1fd5ca8e310"
}
```

`storeName` matches `[A-Za-z0-9][A-Za-z0-9._-]{0,127}`. `storeUuid` is a
canonical lowercase UUID and MUST remain stable for the same `(tenantId,
storeName)` across adapter restarts, upgrades, and matched restores. The response
MUST echo the exact pre-authority binding and store name. The reference adapter
creates and persists this mapping. A production provider bridge resolves an
operator-precreated provider store and MUST NOT silently switch the name to a
different store UUID.

The returned UUID is inserted into the complete `protocol.Binding` used for
capabilities, ownership claim, fences, mutations, reads, and search.

### `POST /v1/capabilities`

Request:

```json
{
  "protocolVersion": "orka.oms.v0alpha1",
  "binding": { "...": "complete binding" }
}
```

Response:

```json
{
  "protocolVersion": "orka.oms.v0alpha1",
  "binding": { "...": "exact request binding" },
  "adapterName": "adapter-name",
  "adapterVersion": "adapter-version",
  "revision": "stable-capability-revision",
  "expiresAt": "2026-07-28T20:00:00Z",
  "capabilities": {
    "durableIdempotency": true,
    "idempotencyDigestConflicts": true,
    "createIfAbsent": true,
    "conditionalMutation": true,
    "monotonicGenerations": true,
    "deleteHighWatermark": true,
    "durableRoutingFence": true,
    "operationLookup": true,
    "exactGet": true,
    "stablePagination": true,
    "exclusiveOwnership": true,
    "keywordSearch": true,
    "auditVersionVisibility": true,
    "semanticSearch": false,
    "hybridSearch": false
  },
  "limits": {
    "maxRequestBytes": 2097152,
    "maxResponseBytes": 4194304,
    "maxContentBytes": 262144,
    "maxTags": 64,
    "maxTagBytes": 128,
    "maxMetadataEntries": 32,
    "maxMetadataKeyBytes": 64,
    "maxMetadataValueBytes": 1024,
    "maxQueryBytes": 1024,
    "maxPageSize": 8,
    "maxSnapshotRecords": 256,
    "snapshotTtlSeconds": 900
  }
}
```

All capabilities through `auditVersionVisibility` are required for writable
activation. `semanticSearch` and `hybridSearch` are optional. `revision` MUST be
stable for one effective behavior/configuration revision. `expiresAt` MUST be in
the future; Orka fences admission and dispatch after expiry or revision change.

`auditVersionVisibility` in v0alpha1 means current backend version visibility on
exact get plus durable terminal audit lookup by operation ID. An implementation
MAY retain richer internal history, but no history-list endpoint is defined by
this version.

### `POST /v1/ownership/claim`

Request:

```json
{
  "protocolVersion": "orka.oms.v0alpha1",
  "binding": { "...": "complete binding" }
}
```

Response:

```json
{
  "protocolVersion": "orka.oms.v0alpha1",
  "binding": { "...": "exact request binding" },
  "result": "applied|identityConflict",
  "bindingDigest": "sha256:<64 lowercase hex>",
  "claimIdentity": "sha256:<stable authority digest>",
  "maximumRoutingEpoch": 7,
  "claimedAt": "2026-07-28T19:00:00Z"
}
```

The first claim durably installs the authority owner and initializes the maximum
routing epoch from the request. Repeating the exact authority claim is
idempotent and returns the already-persisted `claimedAt`, stable
`claimIdentity` (the authority digest that excludes routing epoch), and current
maximum routing epoch, even if the request's routing epoch is older. A different
authority identity in the same claim scope returns `identityConflict` and does
not replace the owner. A store UUID that was not returned by store resolution
for the tenant is also an identity conflict.

### `POST /v1/routing-fences/advance`

Request carries the complete binding; the requested minimum fence is
`binding.routingEpoch`.

Response:

```json
{
  "protocolVersion": "orka.oms.v0alpha1",
  "binding": { "...": "exact request binding" },
  "result": "applied|preconditionFailed|identityConflict",
  "bindingDigest": "sha256:<64 lowercase hex>",
  "maximumRoutingEpoch": 8,
  "completedAt": "2026-07-28T19:10:00Z"
}
```

Only the exact claimed authority may advance its fence. The maximum is durable
and monotonically non-decreasing. A request below the maximum returns
`preconditionFailed`. Every new mutation and read with a lower routing epoch is
rejected without changing content. Exact replay of a previously completed
operation still returns its original receipt.

### `POST /v1/mutations`

Create, replace, and delete use one envelope:

```json
{
  "protocolVersion": "orka.oms.v0alpha1",
  "operationId": "mop-example-1",
  "binding": {
    "clusterId": "cluster-1",
    "namespaceUid": "namespace-uid-1",
    "backendUid": "backend-uid-1",
    "authorityEpoch": 3,
    "routingEpoch": 7,
    "tenantId": "orka-tenant-sha256:<64 lowercase hex>",
    "storeUuid": "store-uuid-1"
  },
  "memoryId": "mem-example-1",
  "upsertKey": "orka:cluster-1:namespace-uid-1:3:mem-example-1",
  "kind": "create|replace|delete",
  "generation": 4,
  "expectedGeneration": 3,
  "expectedBackendVersion": "ref-v3",
  "contentDigest": "sha256:<64 lowercase hex>",
  "mutationDigest": "sha256:<64 lowercase hex>",
  "state": {
    "content": "exact post-redaction content bytes",
    "tags": ["canonical", "sorted"],
    "metadata": {"key": "trimmed value"}
  }
}
```

For delete, `state` is `null` and `contentDigest` is SHA-256 of zero bytes.

#### Canonical content and collections

- Content MUST be valid UTF-8 and is hashed exactly as received. It is not
  Unicode-normalized and line endings are not rewritten.
- Tags are trimmed with Unicode whitespace rules, lowercased, deduplicated,
  validated, and sorted by UTF-8 byte order. The request MUST already contain
  this canonical representation.
- Metadata keys are trimmed and lowercased. Values are trimmed but otherwise
  retain their exact UTF-8 text. Normalization collisions are errors. The
  request MUST already be canonical.
- Metadata is provider content metadata only. Orka governance, trust,
  provenance, or authorization data returned by a provider is never trusted.

#### Canonical mutation digest

`contentDigest` is:

```text
"sha256:" + lowercase_hex(SHA-256(exact UTF-8 content bytes))
```

The delete digest uses the empty byte sequence.

`mutationDigest` is SHA-256 over this JSON object, in this exact field order,
with no insignificant whitespace and with `mutationDigest` itself excluded:

```json
{
  "protocolVersion": "...",
  "operationId": "...",
  "binding": {"clusterId":"...","namespaceUid":"...","backendUid":"...","authorityEpoch":3,"routingEpoch":7,"tenantId":"...","storeUuid":"..."},
  "memoryId": "...",
  "upsertKey": "...",
  "kind": "...",
  "generation": 4,
  "expectedGeneration": 3,
  "expectedBackendVersion": "...",
  "contentDigest": "sha256:...",
  "state": null
}
```

Canonical JSON rules for this profile are:

1. object fields appear in the order shown above;
2. metadata map keys are sorted lexicographically by UTF-8 bytes;
3. arrays retain their canonical order;
4. integers are base-10 with no leading zero;
5. strings use JSON escaping for quote, backslash, and controls; U+2028 and
   U+2029 use `\u2028`/`\u2029`; other non-ASCII characters are emitted as
   UTF-8 and `<`, `>`, and `&` are not HTML-escaped;
6. empty strings, arrays, objects, and `null` are encoded distinctly as shown.

The Go client helper `protocol.PrepareMutation` implements these rules.

#### Conditional mutation semantics

- `generation` MUST be positive and greater than `expectedGeneration`.
- **Create:** the live record MUST be absent. `expectedGeneration` MUST equal
  the retained high watermark (`0` for a never-seen key). The generation MUST
  be greater than that watermark. `expectedBackendVersion` is `""`.
- **Replace:** a live record MUST exist. `expectedGeneration` MUST equal the
  current generation. If `expectedBackendVersion` is non-empty it MUST also
  match. The new generation MUST be greater than the current generation.
- **Delete:** `expectedGeneration` MUST equal the current live or tombstone high
  watermark (`0` when never seen). An optional backend version MUST match. The
  new generation MUST be greater. Deleting an absent record returns `notFound`
  but still persists the supplied generation as a tombstone high watermark.

A later intentional recreation after deletion uses create with
`expectedGeneration` equal to the tombstone generation and a strictly greater
new generation. A stale create cannot resurrect deleted content.

#### Durable idempotency

The adapter durably stores the first terminal receipt for an accepted
`operationId` and canonical `mutationDigest`:

- exact replay returns the original byte-equivalent receipt and performs no
  provider write;
- reuse of the operation ID with a different mutation digest returns
  `idempotencyConflict` and leaves the original receipt/content unchanged;
- `applied` and `notFound` mean the receipt, generation/tombstone, and content
  decision are recoverable after adapter restart.

#### Mutation receipt

```json
{
  "protocolVersion": "orka.oms.v0alpha1",
  "binding": { "...": "exact mutation binding" },
  "result": "applied|notFound|preconditionFailed|idempotencyConflict|identityConflict|retryableError|nonRetryableError",
  "operationId": "mop-example-1",
  "bindingDigest": "sha256:<64 lowercase hex>",
  "appliedGeneration": 4,
  "backendVersion": "provider-version",
  "backendMemoryId": "provider-memory-id",
  "contentDigest": "sha256:<64 lowercase hex>",
  "mutationDigest": "sha256:<64 lowercase hex>",
  "completedAt": "2026-07-28T19:20:00Z"
}
```

`applied` and `notFound` receipts include non-zero `appliedGeneration` and
non-empty backend version/memory ID. Conflict/error receipts use generation `0`
and empty backend identity. Receipts contain no raw provider body, header,
credential, endpoint, or unrestricted message.

### `POST /v1/records/get`

Request:

```json
{
  "protocolVersion": "orka.oms.v0alpha1",
  "binding": { "...": "complete binding" },
  "upsertKey": "orka:cluster-1:namespace-uid-1:3:mem-example-1"
}
```

Response:

```json
{
  "protocolVersion": "orka.oms.v0alpha1",
  "binding": { "...": "exact request binding" },
  "found": true,
  "record": {
    "memoryId": "mem-example-1",
    "upsertKey": "orka:cluster-1:namespace-uid-1:3:mem-example-1",
    "state": "live|tombstone",
    "generation": 4,
    "backendVersion": "provider-version",
    "backendMemoryId": "provider-memory-id",
    "contentDigest": "sha256:<64 lowercase hex>",
    "content": "content or empty tombstone string",
    "tags": [],
    "metadata": {},
    "updatedAt": "2026-07-28T19:20:00Z"
  }
}
```

No row is `"found": false, "record": null`. Tombstones are found records so
Orka can verify the retained high watermark.

### `POST /v1/operations/get`

Request:

```json
{
  "protocolVersion": "orka.oms.v0alpha1",
  "binding": { "...": "current binding" },
  "operationId": "mop-example-1"
}
```

Response:

```json
{
  "protocolVersion": "orka.oms.v0alpha1",
  "binding": { "...": "exact current request binding" },
  "found": true,
  "receipt": { "...": "original terminal receipt" }
}
```

The receipt retains the routing epoch of the original operation; the response
binding echoes the current request. The authority identity must match. Missing
operations return `found=false` and `receipt=null`.

### `POST /v1/search`

Request:

```json
{
  "protocolVersion": "orka.oms.v0alpha1",
  "binding": { "...": "complete current binding" },
  "mode": "keyword|semantic|hybrid|auto",
  "query": "search text",
  "pageSize": 4,
  "pageToken": ""
}
```

Response:

```json
{
  "protocolVersion": "orka.oms.v0alpha1",
  "binding": { "...": "exact request binding" },
  "requestedMode": "auto",
  "actualMode": "keyword",
  "records": [],
  "nextPageToken": "",
  "exhausted": true,
  "snapshotExpiresAt": "2026-07-28T19:35:00Z"
}
```

Keyword search is a case-insensitive substring match of the complete query over
content, tags, metadata keys, and metadata values. Empty query lists all live
records. Each live record may include a non-negative finite `score`; keyword/list-style
results use `0`, while semantic/hybrid providers preserve their provider-local score.
Results are ordered by the provider's stable snapshot order (the reference keyword
adapter uses canonical upsert key ascending). Tombstones are never search results.

The first request creates a durable immutable snapshot containing the bounded
record representations, not merely current record pointers. Therefore updates
or deletes after page one do not alter later pages, and newly created records do
not appear. A page token is bound to the authority, mode, exact query, and page
size. Replaying a token returns the same page. Snapshot state and expiry survive
adapter restart. An invalid or expired token fails closed. The terminal page
MUST set `exhausted=true` and `nextPageToken=""`.

Explicit `semantic` or `hybrid` requests require the matching capability. When
unsupported they return HTTP `422` with code
`MEMORY_SEARCH_MODE_UNSUPPORTED`. An explicit mode is never downgraded. Only
`auto` may downgrade, and the response reports the actual mode.

## Error representation and HTTP status

Codec, authentication, transport-profile, unsupported-mode, and bounded
capacity errors use:

```json
{
  "protocolVersion": "orka.oms.v0alpha1",
  "binding": null,
  "code": "OMS_INVALID_REQUEST",
  "message": "bounded sanitized message",
  "retryable": false,
  "retryAfterSeconds": 0
}
```

When a binding was safely decoded, `binding` echoes it. A non-retryable error
has `retryAfterSeconds=0`. Retryable errors MAY set both the JSON field and the
HTTP `Retry-After` header.

| Outcome | HTTP status |
|---|---:|
| Successful capabilities/claim/fence/get/lookup/search | 200 |
| Mutation `applied` or delete `notFound` | 200 |
| Mutation/claim/fence identity, idempotency, or precondition conflict | 409 |
| Unsupported explicit semantic/hybrid mode | 422 |
| Closed-codec/profile validation failure | 400 (or 413/415 where applicable) |
| Missing/invalid bearer token | 401 |
| Retryable adapter/storage failure | 503 |

The mutation result variant, not the HTTP reason phrase or raw provider body,
is authoritative.

## Durable reference adapter

`internal/oms/referenceadapter` is the normative deterministic fixture. It is
not a provider bridge. It uses its own SQLite database and persists:

- schema version;
- stable tenant/store-name to store-UUID resolutions;
- exclusive ownership claims and maximum routing fences;
- a monotonic provider-writer epoch and the active process holder identity;
- canonical mutation requests and terminal deduplication receipts;
- live records and compact tombstone high watermarks;
- per-generation internal audit rows;
- immutable pagination snapshots containing bounded record JSON.

The database uses WAL, full synchronous durability, foreign keys, and one local
writer connection. A separate SQLite lock file holds an exclusive transaction
for the process lifetime. A second active process cannot open the same adapter
database; an OS/process crash releases the lock. The initial profile supports
one active adapter process only.

## Conformance harness

The reusable library is `pkg/oms/conformance`.

- `conformance.Check` runs the complete semantic suite in one adapter process.
- `conformance.Prepare` returns a credential-free checkpoint after creating
  durable receipts, records, tombstones, a routing fence, and a partially
  consumed snapshot.
- Restart the exact adapter process/image while preserving its database.
- `conformance.VerifyAfterRestart` proves ownership, receipts, exact replay,
  generations, tombstones, routing rejection, operation lookup, and snapshot
  continuation survived.

The command is:

```bash
# ORKA_OMS_BEARER_TOKEN must already be set in the environment.
  go run ./cmd/orka-oms-conformance \
  --endpoint https://adapter.example \
  --phase prepare \
  --state-file /tmp/oms-checkpoint.json

# Restart the adapter, preserving its durable DB/PVC.

# ORKA_OMS_BEARER_TOKEN must already be set in the environment.
  go run ./cmd/orka-oms-conformance \
  --endpoint https://adapter.example \
  --phase verify \
  --state-file /tmp/oms-checkpoint.json
```

`--phase check` is useful during development but does not independently prove a
process restart. The checkpoint is mode `0600`, contains no bearer token, and is
strictly decoded on verification.

The suite verifies, at minimum:

- auth rejection and credential non-reflection;
- stable pre-authority store resolution across restart;
- unknown/duplicate fields, trailing JSON, unsafe identity, and size rejection;
- required capability revision/expiry/limits;
- exclusive ownership conflict;
- create-if-absent, wrong-CAS rejection, replace, and monotonic generation;
- exact durable replay and operation-ID/digest conflict;
- delete-if-absent high-watermark and stale-resurrection rejection;
- durable routing-fence rejection without content change;
- exact get and operation lookup;
- stable pagination, snapshot exclusion, exhaustion, and restart continuation;
- explicit semantic/hybrid `422` behavior and `auto` downgrade reporting.

## Out-of-tree provider adapters

Provider-specific transport, credential handling, deployment manifests, image
publication, and live-provider release gates are maintained outside this
repository. The KD6 implementation lives in
[`orka-agents/orka-oms-kd6-adapter`](https://github.com/orka-agents/orka-oms-kd6-adapter).

External adapters should import the canonical `pkg/oms/protocol` package and
must pass the `pkg/oms/conformance` prepare/restart/verify sequence against the
exact image digest before release. The provider and durable adapter control
state form one matched recovery set; restoring provider content without the
corresponding receipts, claims, fences, tombstones, and snapshot metadata is
non-conformant.
