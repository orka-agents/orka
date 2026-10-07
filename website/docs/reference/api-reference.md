---
slug: /api-reference
description: "Every Orka CRD field and HTTP endpoint, with types and defaults."
---

# API reference

The controller exposes a REST API for programmatic access. Almost every `/api/v1/*`
endpoint requires authentication. Kubernetes ServiceAccount tokens are the default;
operators can also enable OIDC or context-token authentication. The header requirements
for each mode are listed below.

Four routes are deliberately outside that middleware, because they authenticate a different
way:

| Route | How it authenticates instead |
| --- | --- |
| `GET /healthz`, `GET /readyz` | Not authenticated. Kubernetes probes them. |
| `POST /api/v1/gateways/:namespace/:name/events` | The Gateway's own inbound bearer Secret, not a user token. |
| `POST /webhooks/github` | HMAC over the request body (`X-Hub-Signature-256`). |
| `/internal/v2/acp/*` | Pool-scoped bearer credentials checked by the ACP handlers themselves. Not for client use. |

## Authentication

Send Kubernetes ServiceAccount and OIDC credentials with the standard bearer token header:

```http
Authorization: Bearer <token>
```

Authentication modes:

- **Kubernetes ServiceAccount token** — default mode. Tokens are validated with the Kubernetes TokenReview API.
- **OIDC JWT** — enabled when the controller is configured with `--oidc-issuer` and `--oidc-audience` (or `ORKA_OIDC_ISSUER` / `ORKA_OIDC_AUDIENCE`). Tokens are validated against the issuer, audience, expiration, RS256 signature, and `--oidc-allowed-subjects`; authorized OIDC callers are assigned `--oidc-namespace` for namespace isolation. If `--oidc-jwks-url` is omitted, Orka discovers the JWKS URL from the issuer metadata.
- **Context token / `transaction-token` TxToken** — enabled with `--context-token-profile=transaction-token`, `--context-token-issuer`, and `--context-token-audience` (or the matching `ORKA_CONTEXT_TOKEN_*` env vars). The built-in profile validates RS256 TxTokens with `typ: txntoken+jwt`, issuer/audience/time claims, `kid`, and required `iat`, `txn`, `scope`, and `req_wl` claims. By default tokens are read from the raw `Txn-Token` header; `Authorization: Bearer` support is opt-in with `--context-token-headers=Txn-Token,Authorization:Bearer`.

```http
Txn-Token: <txntoken+jwt>
```

When a Task is created through OIDC or context-token authentication, Orka stamps the verified caller identity into immutable `spec.requestedBy` (`subject`, `issuer`, `username`, `email`, `groups`, and `roles` when present). Context-token Task creation also stamps immutable `spec.transaction` plus transaction labels/annotations for audit correlation. Clients cannot provide or override `requestedBy` or `transaction`; requests containing top-level or nested `spec.requestedBy`/`spec.transaction` are rejected with `400`. Right after creating such a Task the API seals `orka.ai/requested-by-stamp`, an HMAC over the server-assigned Task UID and the requester, next to the controller-only `orka.ai/requested-by-source=api` annotation; connector use trusts a requester only when that stamp verifies for the Task's own UID, so a Task planted with the source annotation while admission was disabled is never trusted. A coordination child created by a worker is sealed only through `POST /internal/v1/tasks/:namespace/:taskName/children/:child/requester-stamp`, which authenticates the caller as the parent's current worker and checks that the child is controller-owned by that parent and names the same requester; an owner reference alone never lets a child inherit authority. Because the seal is a second write after the create (or, for a coordination child, a request from the parent's worker), an API-stamped Task or controller-owned child less than two minutes old whose seal has not landed is retried at dispatch rather than dispatched without Connections; an older unsealed Task is treated as unverified. See [Transaction Token integration](../concepts/transaction-tokens.md) for scope/`tctx` authorization, TTS exchange, delegation, and audit behavior.

## Webhooks

GitHub webhooks use HMAC verification instead of bearer-token authentication.

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/webhooks/github` | POST | Accept GitHub `issues` / `pull_request` label triggers and pull request events for exact-head repository monitor runs |

The controller requires `ORKA_GITHUB_WEBHOOK_SECRET` and verifies the `X-Hub-Signature-256` header. The `orka:implement` issue label queues the managed repository workflow; configured pause-label changes queue fresh reconciliation. Pull request events can also queue exact-head `RepositoryMonitor` runs when a matching monitor has `spec.review.exactEventEnabled: true`. See [GitHub Label Triggers](../guides/github-label-triggers.md) for configuration and webhook behavior.

## Tasks

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/v1/tasks` | POST | Create a task |
| `/api/v1/tasks` | GET | List tasks (paginated; `labelSelector` narrows by label with `kubectl -l` syntax, invalid selectors return 400) |
| `/api/v1/tasks/:id` | GET | Get task details |
| `/api/v1/tasks/:id` | DELETE | Cancel/delete task |
| `/api/v1/tasks/:id/logs` | GET | Stream task logs |
| `/api/v1/tasks/:id/result` | GET | Get task result |
| `/api/v1/tasks/:id/artifacts` | GET | List task artifacts |
| `/api/v1/tasks/:id/artifacts/:filename` | GET | Download a task artifact |
| `/api/v1/tasks/:id/plan` | GET | Get task plan |
| `/api/v1/tasks/:id/children` | GET | Get child tasks |

### Agent Task workspace and delivery schema

`POST /api/v1/tasks` accepts the Task CRD shape. Agent repository configuration belongs at top-level `spec.workspace`; `spec.agentRuntime` contains only per-Task runtime overrides.

| Path | Type | Values/default | Notes |
| --- | --- | --- | --- |
| `spec.workspace.intent` | string | `read` for agent Tasks; `read` or `write` | Immutable effective intent for the attempt. |
| `spec.workspace.gitRepo` | string | empty | Credential-free source repository URL. Embedded credentials, query strings, and fragments are rejected. |
| `spec.workspace.sourceRepository` | object | empty | Optional canonical provider/ID source identity. |
| `spec.workspace.branch` / `ref` | string | empty | Source branch or exact ref/commit/tag. When both are empty, the Publisher resolves and freezes the repository's advertised default branch before execution. |
| `spec.workspace.readCredentialRef.name` | string | empty | Secret used only by the clean-room source clone/read operation. |
| `spec.workspace.publicationGitRepo` | string | empty | Credential-free publication repository URL. |
| `spec.workspace.publicationRepository` | object | empty | Optional canonical provider/ID publication identity. |
| `spec.workspace.publicationReadCredentialRef.name` | string | empty | Target-read Secret used only for publication preflight and independent verification. |
| `spec.workspace.publicationCredentialRef.name` | string | empty | Target-write Secret used only for the exact branch compare-and-swap push. |
| `spec.workspace.forgeCredentialRef.name` | string | empty | Forge API Secret used only for pull-request reconciliation; required when `createPR` is true. |
| `spec.workspace.subPath` | string | empty | Repository subdirectory exposed as workspace root. |
| `spec.workspace.pushBranch` | string | generated for write Tasks when omitted | Publication branch; Orka-generated names use full Task or Session identity entropy. |
| `spec.workspace.prBaseBranch` | string | empty | Pull-request base branch. |
| `spec.workspace.prTitle` | string | prompt's first nonblank line | Exact pull-request title, up to 256 characters. Empty uses the default, which trims whitespace and truncates to 256 characters. An empty or whitespace-only prompt uses `Orka publication generation N`. Nonempty whitespace-only titles are rejected. |
| `spec.workspace.prBody` | string | publisher summary and Task namespace/name | Pull-request body, up to 32,768 characters. The publisher appends the publication generation and reconciliation markers to custom and default bodies. Reserved Orka reconciliation comments are rejected. |
| `spec.workspace.createPR` | boolean | `false` | Reconcile a pull request only after branch publication when true; requires `intent: write`. |
| `spec.agentRuntime.maxTurns` | integer | Agent default | Per-Task prompt-loop limit. |
| `spec.agentRuntime.allowedTools` / `disallowedTools` | list | Agent defaults | Per-Task tool policy override. |
| `spec.agentRuntime.allowBash` | boolean | Agent default | Per-Task bash policy override. |
| `spec.timeout` | duration | `30m` for Orka harness v2 agent Tasks | Maximum wall-clock duration measured from Task creation, including queue, runtime admission, and prompt execution time. An explicit positive value overrides the default. |

`prTitle` and `prBody` apply only when `createPR: true`. Supplying presentation text alone does not request a pull request; it can remain configured on branch-only Tasks.

Orka rejects secret-like pull-request titles and bodies at runtime before publication, including prompt-derived titles. Explicit overrides wait for a publisher with pull-request presentation support before prompt admission.

Source read, target read, target write, and forge references are distinct
credential roles. The selected Secret UID/resourceVersion is frozen for the
attempt, and the credential broker releases a value only to the
Workspace/Publisher for the exact active operation. Secret contents are never
copied to Task status or delivered to the ACP process tree.

The durable ACP attempt is exposed in `status.execution`. Workspace validation and publication use `status.delivery`, including publication ID, repository identities, branch, starting/remote/tree/commit SHAs, artifact digest, and optional PR receipt. A Task is not delivered merely because the model reports success; require a terminal verified delivery outcome.

`Task.spec.execution.workspace` runs the agent inside an external sandbox provider instead of a
plain runtime Pod. It is off unless the operator turns it on: `--acp-workspace-dispatch-enabled`
plus the flag for the provider you want (`--agent-sandbox-enabled` or `--substrate-enabled`).
Without them the field is rejected rather than ignored. See
[Agent Sandbox](../concepts/agent-sandbox.md) or [Agent Substrate](../concepts/substrate.md) for
the two supported providers, and [Configuration](configuration.md#workspace-providers)
for the flags and the class-based lifecycle.

### Get Task plan

Retrieve the autonomous plan state for a task.

**Endpoint:** `GET /api/v1/tasks/{id}/plan`

**Response (200):**
```json
{
  "TaskName": "build-feature",
  "Namespace": "default",
  "Iteration": 3,
  "Summary": "Completed auth module, working on CRUD endpoints",
  "ProgressPct": 40,
  "GoalComplete": false,
  "PlanDocument": "# Plan\n- [x] Auth\n- [ ] CRUD\n...",
  "CreatedAt": "2024-01-15T10:00:00Z",
  "UpdatedAt": "2024-01-15T12:30:00Z"
}
```

**Errors:**
- `404` — No plan found for this task
- `501` — Plan store not configured

## Sessions

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/v1/sessions` | GET | List sessions |
| `/api/v1/sessions/:id` | GET | Get session transcript |
| `/api/v1/sessions/:id` | DELETE | Delete session |


## Memory

Memory endpoints manage namespace-scoped durable memories and reviewable memory proposals. See [Memory](../concepts/memory.md) for the full lifecycle, worker behavior, and examples.

### Durable memories

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/v1/memories` | GET | List durable memories |
| `/api/v1/memories` | POST | Create durable memory |
| `/api/v1/memories/:id` | GET | Get durable memory |
| `/api/v1/memories/:id` | PUT | Update durable memory |
| `/api/v1/memories/:id` | DELETE | Soft-delete durable memory |
| `/api/v1/memories/:id/disable` | POST | Disable memory for normal recall |
| `/api/v1/memories/:id/enable` | POST | Re-enable memory for normal recall |

Common list query parameters: `namespace`, `query`/`q`, `sessionName`, `agentName`, `taskName`, `parentTask`, `source`, `tags`, `ids`, `includeDisabled`, `includeDeleted`, and `limit`.

### Memory proposals

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/v1/memory-proposals` | GET | List memory proposals |
| `/api/v1/memory-proposals` | POST | Create a memory proposal |
| `/api/v1/memory-proposals/:id` | GET | Get a memory proposal |
| `/api/v1/memory-proposals/:id/review` | POST | Record a review decision without applying it |
| `/api/v1/memory-proposals/:id/apply` | POST | Apply an accepted `memory` proposal into durable memory |
| `/api/v1/memory-proposals/:id/archive` | POST | Archive a proposal without applying it |

Common list query parameters: `namespace`, `taskName`, `agentName`, `type`, `status`, `query`/`q`, and `limit`. Review and archive return `204 No Content`. Apply accepts optional `appliedBy` and returns the linked durable memory JSON; repeated apply requests return the same memory.

## ACP runtime resources

| Endpoint | Method | Description |
| --- | --- | --- |
| `/api/v1/runtime-pools` | GET | List controller-owned RuntimePools and lifecycle/admission/capacity status. |
| `/api/v1/runtime-pools/:name` | GET | Get one RuntimePool. |
| `/api/v1/agent-runtimes` | GET | List external `orka.harness.v2` registrations. |
| `/api/v1/agent-runtimes` | POST | Create an external v2 registration. |
| `/api/v1/agent-runtimes/:name` | GET | Get an external registration and observed capabilities. |
| `/api/v1/agent-runtimes/:name` | PUT | Replace an external registration. |
| `/api/v1/agent-runtimes/:name` | DELETE | Delete an external registration. |

RuntimePools are controller-owned for built-in Codex, OpenCode, Claude, and Copilot Tasks; the public API is read-only. A current-generation ready, strict-governed external registration can be selected through `Agent.spec.runtime.runtimeRef`. Orka revalidates its frozen endpoint, profile, authentication authority, and observed instance before dispatch and recovery mutations.

## Agents

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/v1/agents` | POST | Create an agent |
| `/api/v1/agents` | GET | List agents |
| `/api/v1/agents/:name` | GET | Get agent details |
| `/api/v1/agents/:name` | PUT | Update an agent |
| `/api/v1/agents/:name` | DELETE | Delete an agent |

## Skills

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/v1/skills` | POST | Create a skill |
| `/api/v1/skills` | GET | List skills |
| `/api/v1/skills/:name` | GET | Get skill details |
| `/api/v1/skills/:name/content` | GET | Get raw `spec.content.inline` markdown |
| `/api/v1/skills/:name` | PUT | Update a skill |
| `/api/v1/skills/:name` | DELETE | Delete a skill |


## Generic Gateways

See [Generic Gateway API](gateway-api.md) for the adapter contract, Kubernetes resources, durable ledger endpoints, filters, and retry workflow.

## Tools

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/v1/tools` | GET | List tools (built-in + CRDs) |
| `/api/v1/tools/:name` | GET | Get tool details |

### Tool CRD schema

`GET /api/v1/tools/:name` returns built-in tool metadata or the full `Tool` CRD. Custom Tool CRDs can call plain HTTP endpoints or MCP servers hosted in durable Substrate actors.

Plain HTTP tools set `spec.http.url` and may inject authentication from a Kubernetes Secret into either the `Authorization: Bearer` header or the JSON request body:

This example uses a placeholder catalog API. Replace its URL and Secret reference with your service's values.

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Tool
metadata:
  name: catalog-search
spec:
  description: "Search a product catalog"
  parameters:
    type: object
    properties:
      query:
        type: string
    required:
      - query
  http:
    url: "https://catalog.example.com/search"
    method: POST
    authSecretRef:
      name: catalog-api-key
      key: api-key
    authInject: body
    authBodyKey: api_key
```

MCP actor-backed tools set `spec.mcp.substrateActor` and may omit `spec.http` entirely. Orka creates or reuses the Substrate actor, waits for the MCP endpoint, stores the resolved endpoint in `status.endpoint`, and workers call the MCP tool through JSON-RPC `tools/call` using the Tool name as the MCP tool name.

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Tool
metadata:
  name: repo-inspector
spec:
  description: "Inspect repository metadata through an MCP server"
  parameters:
    type: object
    properties:
      message:
        type: string
    required:
      - message
  mcp:
    path: /mcp
    substrateActor:
      templateRef:
        name: orka-mcp
        namespace: ate-demo
      poolRef:
        name: mcp-substrate-pool
      boot: true
```

| Path | Type | Values/default | Notes |
|------|------|----------------|-------|
| `spec.description` | string | required | Description shown to the LLM. |
| `spec.parameters` | JSON Schema | empty | Tool argument schema in function-calling format. |
| `spec.http.url` | string | required for plain HTTP tools | Endpoint called by workers. MCP actor-backed tools may omit it because Orka uses `status.endpoint`. |
| `spec.http.method` | string | default `POST`; allowed `GET`, `POST`, `PUT`, `PATCH`, `DELETE` | HTTP method for plain HTTP tools. MCP actor-backed tools use `POST`. |
| `spec.http.headers` | map | empty | Static headers sent with the request. Reserved token propagation headers cannot be overridden when outbound TxToken propagation is enabled. |
| `spec.http.timeout` | duration | default `30s` | Per-call request timeout. |
| `spec.http.authSecretRef` | Secret key selector | empty | Secret value used as the auth token. Cannot coexist with a direct OutboundAccessPolicy. |
| `spec.http.outboundAccessPolicyRef.name` | string | empty | Same-namespace `OutboundAccessPolicy` required to be Accepted with ResolvedRefs. |
| `spec.http.authInject` | string | default `header`; allowed `header`, `body` | `header` sends `Authorization: Bearer <token>`. `body` injects the token into the JSON request body and is invalid for MCP actor-backed tools. |
| `spec.http.authBodyKey` | string | empty | JSON key used when `authInject: body`. |
| `spec.mcp.path` | string | `/mcp` | HTTP path exposed by the MCP server inside the actor. |
| `spec.mcp.substrateActor.templateRef.name` | string | required | Substrate `ActorTemplate` hosting the MCP server. |
| `spec.mcp.substrateActor.templateRef.namespace` | string | Tool namespace or configured default | Namespace containing the actor template. |
| `spec.mcp.substrateActor.poolRef.name` | string | empty | Optional `SubstrateActorPool` for actor placement and reuse. The pool template must match the MCP actor template. |
| `spec.mcp.substrateActor.poolRef.namespace` | string | Tool namespace | Namespace containing the referenced actor pool. |
| `spec.mcp.substrateActor.boot` | boolean | `false` | Boots the actor from scratch on first resume; later reconciles reuse an already booted actor. |
| `status.available` | boolean | false | Whether the controller can reach the resolved endpoint. |
| `status.endpoint` | string | empty | Resolved non-secret endpoint used by workers. For MCP actor-backed tools this is the Substrate router endpoint. |
| `status.actor` | object | empty | Safe actor metadata, including provider, actor ID, route host, resolved template, and pool reference. |

MCP actor-backed tools require Substrate support to be enabled on the controller. If transport auth is needed for an MCP endpoint, set `spec.http.authSecretRef`, keep `authInject` as `header` or omit it, and omit `spec.http.url`.

## OutboundAccessPolicy

`OutboundAccessPolicy` is namespaced and selects exactly one adapter. Direct mode performs RFC 8693/RFC 7523 exchange and injects a validated Bearer resource credential. Gateway mode dials a trusted Kubernetes Service while preserving the original Tool authority, path, query, method, body, and protocol headers. Connection mode injects the requesting person's linked-account credential for a `ConnectorProvider`.

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: OutboundAccessPolicy
metadata:
  name: resource-api
  namespace: default
spec:
  direct:
    grant: TokenExchange
    tokenEndpoint:
      url: https://identity.example.test/oauth/token
    subject:
      source: TransactionToken
    scopes: [api.read]
    requestedTokenType: urn:ietf:params:oauth:token-type:access_token
    expectedIssuedTokenType: urn:ietf:params:oauth:token-type:access_token
```

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: OutboundAccessPolicy
metadata:
  name: github-as-me
  namespace: default
spec:
  connection:
    providerRef:
      name: github
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `spec.connection.providerRef.name` | string | required | Same-namespace `ConnectorProvider`. `ResolvedRefs` is True only while the provider is Accepted. |
| `spec.connection.output` | object | `Authorization: Bearer` | Header and prefix for the injected credential. `Txn-Token` is forbidden. |

Connection mode resolves at call time, in the controller only. A `readOnly` Connection hides the provider's write-class tools from the agent, and connector write tools are always approval-required. For native `type: ai` Tasks the worker Pod never runs such a tool itself: it calls `POST /internal/v1/tasks/:namespace/:taskName/connector-tools/:tool` with its ServiceAccount token, and the controller verifies the caller is the Task's current worker, that the arguments satisfy the Tool's declared JSON Schema, that the tool is connector-backed and enabled for the Task, and that the policy frozen into the Job at dispatch is honored: the Job carries the tool list, the approval-required set, the spec digest of every dispatched connector tool, and the Connection bindings (`ORKA_CONNECTOR_TOOL_DIGESTS`, `ORKA_CONNECTION_BINDINGS`). A tool whose definition changed since dispatch, a write-class tool the frozen policy does not cover, or a Task whose `status.connectionBindings` no longer match the Job's is refused, and a tool the policy covers executes only with an approved approval (`approvalId`) that binds this tool's configuration, the connection-mode policy's injection configuration, and the Connection frozen for its policy (so a Job re-created after the decision against a re-linked account or a changed policy needs a fresh approval), these exact arguments, and is claimed atomically for a single execution. Every failure before a provider request hands the claim back; a claimed call is recorded in the durable external-effect ledger under its claim with the frozen Connection digest, using the Tool's own request timeout as its lease, and a worker that lost the response receives the committed result again (`replayed: true`) without a second request. Results larger than 256 KiB are retained as a digest-and-size receipt and cannot be replayed. The effect record is reserved before the claim is recorded, and a spent claim is reconciled from it: a record still Pending is moved to Failed (which fences out any request that has not started its call) and the claim is handed back; an in-flight record whose lease lapsed settles as outcome unknown (`502`) and the approval stays spent; a live in-flight record is `409 still executing`. It then executes the call against the Connection frozen into `status.connectionBindings` at Job creation, which a recovered Job always reads back from its own environment. The Task's verified `spec.requestedBy` selects the person, the Connection identity frozen into the Task's execution snapshot at dispatch must still match the live Connection (UID, generation, and grant sequence, so a re-link of the same Connection object needs a re-dispatch), the policy object checked against the dispatched configuration must be the one that resolves, the Connection must be Ready for its current generation, and the Tool URL must be HTTPS without `authSecretRef`. A token that expires within 60 seconds is refreshed once per Connection at a time and the rotated material written back to custody. A provider that rejects the refresh marks the Connection `Revoked` and shreds its custody; an expired token with no refresh token marks it `Expired`. Any other condition fails the call with no fallback to Task Secrets, environment credentials, or other people's Connections. Worker Pods have no credential source and refuse connection-mode policies.

Policy status contains only `observedGeneration`, `Accepted`, and `ResolvedRefs`. Secret references are key-specific and same-namespace. Cross-namespace Service refs require exact controller allowlist entries. See [Outbound Access Policies](../concepts/outbound-access.md).

## ConnectorProvider

`ConnectorProvider` is the operator-owned catalog entry for one third-party service people may link through OAuth. It is namespaced, reconciled only when the controller runs with `--connectors-enabled`, and carries public OAuth client settings plus the tools the service offers. Only the client secret lives in a Secret.

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: ConnectorProvider
metadata:
  name: github
  namespace: default
spec:
  displayName: GitHub
  oauth:
    authorizeURL: https://github.com/login/oauth/authorize
    tokenURL: https://github.com/login/oauth/access_token
    clientID: Iv1.example-client-id
    clientSecretRef:
      name: github-connector-oauth
      key: clientSecret
    scopes:
      read: [read:user]
      write: [repo]
  tools:
    - name: list_pull_requests
      class: read
      source: Builtin
    - name: create_pull_request
      class: write
      source: Builtin
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `spec.displayName` | string | object name | Shown to people in the dashboard and CLI. |
| `spec.oauth.authorizeURL` | string | required | Absolute HTTPS authorization endpoint. Private, loopback, link-local, and cluster-internal hosts are rejected, and so are single-label names (which Pod DNS search domains complete to cluster Services) and non-canonical numeric hosts. Hosts must be ASCII: write an internationalized name in its punycode (`xn--`) form. The same host rules apply to every endpoint and HTTP tool URL. |
| `spec.oauth.tokenURL` | string | required | Absolute HTTPS token endpoint used for the code exchange and refresh. |
| `spec.oauth.revocationURL` | string | empty | Optional RFC 7009 endpoint called best-effort on disconnect for the committed credential. Orka never revokes material it did not commit: a token from a consent nobody completed is deleted and left to expire, because it may belong to a different person's grant. |
| `spec.oauth.clientID` | string | required | Public OAuth client identifier, printable ASCII, at most 256 bytes. |
| `spec.oauth.clientSecretRef` | Secret key selector | required | Same-namespace Secret holding the client secret. |
| `spec.oauth.clientAuthentication` | `ClientSecretBasic` \| `ClientSecretPost` | `ClientSecretBasic` | How the client secret is presented to the token endpoint. |
| `spec.oauth.pkce` | bool | `true` | Enable RFC 7636 code verification. |
| `spec.oauth.scopes.read` / `.write` | []string | empty | Scopes requested for `readOnly` Connections, and additionally for `readWrite`. Entries must be RFC 6749 scope tokens (printable ASCII without spaces, quotes, or backslashes) of at most 256 bytes; the read and write sets together must encode to at most 2048 bytes. |
| `spec.oauth.additionalAuthorizeParameters` | map | empty | Static authorize query parameters, each value at most 512 bytes and 2048 bytes encoded in total. Reserved OAuth fields and credential-like names (`token`, `secret`, `api_key`, `assertion`, and similar) are rejected; the spec is public configuration. Endpoint URLs may carry a query of at most 1024 bytes, but not credential-like parameters or reserved OAuth fields (including `code_verifier`). An HTTP tool URL may use any query name that is not credential-like, such as `state=open`. |
| `spec.tools[].name` | string | required | Tool name exposed to agents. Unique within the provider. |
| `spec.tools[].class` | `read` \| `write` | required | Write tools are hidden from `readOnly` Connections and require approval. |
| `spec.tools[].source` | `Builtin` \| `HTTP` | required | `Builtin` names an existing Orka tool. `HTTP` carries a curated definition in `http`. |
| `spec.tools[].description`, `.parameters`, `.http` | | | HTTP tools only. `parameters` must be a JSON Schema whose root declares `type: object` and that resolves in full, including nested property schemas. `http.url` must be HTTPS and is the exact destination, without template placeholders; static headers are limited to 128-byte names, 1024-byte values, and 4096 bytes in total; `Authorization`, `Cookie`, `Host`, `Txn-Token`, and hop-by-hop headers (`Connection`, `Keep-Alive`, `Te`, `Trailer`, `Upgrade`, and similar) are reserved, and credential-like header names (`X-Api-Key`, `X-Auth-Token`, and similar) are rejected: the linked account is the only credential. |

Status contains only `observedGeneration`, `Accepted`, and `ResolvedRefs`. `Builtin` tool names are checked against the controller's built-in tool registry, so a misspelled built-in is rejected.

## Connection

`Connection` is one person's linked account with one `ConnectorProvider`. The API server creates it from the caller's verified OIDC or context-token identity; `spec.subject` and `spec.providerRef` are immutable. Deleting the Connection is the disconnect. Status never carries token material.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `spec.subject.issuer` | string | required | Identity issuer that verified the subject. Immutable. |
| `spec.subject.subject` | string | required | Issuer-scoped stable subject. Immutable. |
| `spec.providerRef.name` | string | required | Same-namespace `ConnectorProvider`. Immutable. |
| `spec.mode` | `readOnly` \| `readWrite` | `readOnly` | `readOnly` hides the provider's write tools and requests only read scopes. |
| `status.state` | string | | `Pending`, `Ready`, `Expired`, `Revoked`, or `Error`. |
| `status.grantedScopes` | []string | | Scopes the provider reported at consent time. |
| `status.linkedAt`, `.expiresAt`, `.lastRefreshTime` | time | | Link, access-token expiry, and refresh timestamps. |
| `status.grantSequence` | integer | | The grant custody assigned to the linked material; it rises with every committed consent and never repeats. Authority frozen or approved under one grant does not survive a re-link. |
| `status.consent.providerUID`, `.authorityDigest` | string | | The `ConnectorProvider` UID and a non-secret digest of everything the last consent authorized a token to reach: the provider's OAuth client identity (client ID, secret reference, authentication method, endpoints, static authorize parameters) and the curated HTTP tool destinations (name, class, URL, method, static headers). Changing any of these asks for consent again; adding or changing built-in declarations does not. |

Conditions are `ProviderResolved` and `ScopesGranted` (set by the controller; the latter compares `status.grantedScopes` with the scopes the current mode and provider require, so widening the mode or a provider requiring more scopes projects `Pending` with reason `ConsentRequired` without erasing the consent, and narrowing restores readiness; it also requires `status.consent` to match the current provider, so a replaced provider or a changed OAuth client asks for consent again instead of refreshing or revoking the held token against a different authority) and `Ready` (set by the consent and refresh paths). A Connection is usable only when `Ready` is True and both controller conditions are True for the current generation. See [ADR 0033](https://github.com/orka-agents/orka/blob/main/docs/adr/0033-user-connectors.md) for the design.

## Connector endpoints

Available when the controller runs with `--connectors-enabled` and `--connector-callback-base-url`. Every route requires a verified OIDC or context-token identity carrying an issuer and subject; ServiceAccount bearer tokens are refused with 403. Under context-token authorization, reads need `orka:connectors:read` and every mutation needs `orka:connectors:manage`, so a delegated token narrowed to other work cannot inspect or revoke a person's accounts. A person sees and changes only Connections whose `spec.subject` matches their identity; a foreign Connection reads as 404. Responses never carry token material.

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/v1/connectors` | GET | List the provider catalog: name, display name, readiness, scopes, and tools with their `read`/`write` class. |
| `/api/v1/connections` | GET | List the caller's Connections. |
| `/api/v1/connections` | POST | Body `{"provider": "github", "mode": "readOnly"}`. Creates or reuses the caller's Connection for that provider and returns `{"connection": ..., "authorizeURL": ...}`. The browser opens `authorizeURL`; unknown body fields, including any subject, are rejected. |
| `/api/v1/connections/:name` | GET | One Connection. |
| `/api/v1/connections/:name` | PUT | Body `{"mode": "readWrite"}` (`mode` is required). Whenever the granted scopes do not cover the requested mode the response carries a new `authorizeURL`, so widening to `readWrite`, and retrying it, asks for the write scopes; narrowing takes effect immediately. |
| `/api/v1/connections/:name` | DELETE | Disconnect. The controller's finalizer revokes the committed tokens (against the client and revocation endpoint sealed with them, so a moved token endpoint does not skip revocation) and then deletes the sealed material. A provider that refuses a revocation is best effort; a missing client Secret or key is not: custody and the finalizer are kept, and the disconnect retries until the Secret is restored, unless the provider itself is being deleted (namespace teardown), in which case the tokens are left to expire and the disconnect finishes. |
| `/api/v1/connections/:name/authorize` | POST | Restart consent for an existing Connection, for example after the provider revoked it. |
| `/api/v1/connections/callback` | GET | OAuth redirect target. Unauthenticated: the signed single-use `state` and the server-side PKCE verifier authenticate it. It exchanges the code, parks the tokens sealed as a pending completion, and redirects to `<callback base>/settings/connectors?status=pending&connection=<name>#completion=<token>`, or `?status=error&reason=<code>`. A provider that grants fewer scopes than the mode requires is refused (`reason=scopes_denied`); the issued token is discarded and left to expire, never revoked, because Orka cannot prove whose grant a token nobody committed belongs to. The requested set is the one sealed when consent started, never the provider's current configuration. A provider that omits the `scope` field granted the requested set; an explicitly empty `scope` is a grant of nothing and is refused. Granted scope lists are split on spaces and, as GitHub reports them, commas; a configured scope name may not contain a comma, so splitting never fabricates a required scope. A provider whose OAuth client changed since consent started is refused before any exchange (`reason=provider_changed`). So is a Connection whose mode changed while the person was at the provider (`reason=mode_changed`). Consents are numbered when they start; a callback from a consent older than one already parked or committed for the Connection is refused (`reason=consent_superseded`), and a newer callback replaces an older uncommitted completion, so callbacks that finish out of order never let an older grant replace a newer one. |
| `/api/v1/connections/:name/complete` | POST | Body `{"completion": "<token from the fragment>"}`. Commits the parked tokens. Only the Connection's owner can call it and only with the one-time token the completing browser received, so a consent link forwarded to someone else can never bind their account to the sender's Connection. Requires the controller's custody finalizer to be present; after a disconnect the UID is tombstoned and completion is refused. A short-lived token without a refresh token that expired while the browser held the completion is refused (`409`) and the completion discarded, never committed as Ready. If a `PUT` changed the mode between the completion fence and the status write, the link is judged again from the committed material against the new mode. A Connection keeps at most 16 superseded grants for revocation at disconnect; a completion past that bound is refused (`409`) and the person disconnects and links again. |

The consent `state` and the completion token are HMACs over random single-use nonces, keyed by a value derived from the controller's snapshot key. The pending consent row, sealed with the controller key, binds the nonce to the Connection UID, owner digest, provider, and a 10 minute expiry. Token material is sealed under a per-Connection data key that is itself wrapped by the controller key. Disconnect deletes both halves and tombstones the Connection, so the live store and later backups hold nothing; a backup taken before the disconnect still contains the sealed material and must be protected like the store itself. See [ADR 0033](https://github.com/orka-agents/orka/blob/main/docs/adr/0033-user-connectors.md). A Connection holds one pending consent at a time: a new authorize replaces the earlier one. A `ConnectorProvider` name is at most 63 characters, since it labels every Connection.

## Security

Repository security endpoints manage `RepositoryScan` configurations and their generated threat models, scan runs, findings, patch proposals, and remediation pull requests. Like other `/api/v1/*` endpoints, they require ServiceAccount bearer token authentication.

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/v1/security/repositories` | POST | Create a repository scan |
| `/api/v1/security/repositories` | GET | List repository scans |
| `/api/v1/security/repositories/:name` | GET | Get repository scan details |
| `/api/v1/security/repositories/:name` | PUT | Update repository scan spec |
| `/api/v1/security/repositories/:name` | DELETE | Delete repository scan |
| `/api/v1/security/repositories/:name/threat-model` | GET | Get latest threat model |
| `/api/v1/security/repositories/:name/threat-model` | PUT | Update threat model |
| `/api/v1/security/repositories/:name/scans` | GET | List scan runs |
| `/api/v1/security/repositories/:name/scans` | POST | Trigger manual scan |
| `/api/v1/security/repositories/:name/scans/:scanID/progress` | GET | Per-stage Task counts for one scan run (see below) |
| `/api/v1/security/repositories/:name/slices` | GET | List deterministic review slices |
| `/api/v1/security/repositories/:name/slices/:sliceID` | GET | Get review slice details |
| `/api/v1/security/repositories/:name/dropped-findings` | GET | List v2 dropped-finding diagnostics |
| `/api/v1/security/repositories/:name/findings` | GET | List findings |
| `/api/v1/security/findings/:id` | GET | Get finding details |
| `/api/v1/security/findings/:id/dismiss` | POST | Dismiss finding |
| `/api/v1/security/findings/:id/reopen` | POST | Reopen finding |
| `/api/v1/security/findings/:id/validate` | POST | Trigger validation |
| `/api/v1/security/findings/:id/patch` | POST | Generate patch proposal |
| `/api/v1/security/findings/:id/patches` | GET | List patch proposals |
| `/api/v1/security/findings/:id/pull-request` | POST | Create remediation PR |

Common query parameters:

- `namespace` — Kubernetes namespace to operate in.
- `limit` — page size for list endpoints that support pagination.
- `continue` — Kubernetes continue token for `GET /api/v1/security/repositories`.
- `cursor` — store cursor for `GET /api/v1/security/repositories/:name/scans`, `GET /api/v1/security/repositories/:name/slices`, `GET /api/v1/security/repositories/:name/dropped-findings`, and `GET /api/v1/security/repositories/:name/findings`.
- `severity`, `validationStatus`, `state`, `sliceID`, `category` — filters for `GET /api/v1/security/repositories/:name/findings`.
- `status` — filter for `GET /api/v1/security/repositories/:name/slices`.
- `scanRunID`, `sliceID`, `layer` — filters for `GET /api/v1/security/repositories/:name/dropped-findings`. `layer` is one of `validation`, `filter`, or `cap`.
- `reason` — exact dropped-finding reason filter; use `reason=contains=<text>` for substring matching.
- `recommended=true` — filters findings to recommended remediation candidates.

`GET /api/v1/security/repositories/:name/scans/:scanID/progress` returns the scan run,
`complete` (whether the run has finished), and `stages`: one entry per pipeline stage in
order (`threat-model`, `mapper`, `review`, `validation`, `patch`) with `label`, `tasks`,
`pending`, `running`, `succeeded`, `failed`, `cancelled`, and `failedTasks` (the names of
failed and cancelled Tasks). The server groups the run's Tasks by their
`orka.ai/security-scan-id` and `orka.ai/security-stage` labels under its own identity, so
the caller needs the same security read permission as listing scan runs and no Task list
permission. Stages the scan has not reached yet are present with zero counts.

### Create repository scan

**Endpoint:** `POST /api/v1/security/repositories`

**Request Body:**
```json
{
  "name": "example-repo",
  "namespace": "default",
  "spec": {
    "provider": "github",
    "repoURL": "https://github.com/example/app",
    "branch": "main",
    "ref": "v1.2.3",
    "schedule": "0 2 * * *",
    "validationMode": "light",
    "validationMaxFindingsPerRun": 8,
    "validationMinSeverity": "medium",
    "validationMinConfidence": "medium",
    "customScanInstructionsRef": {"name": "repo-security-policy", "key": "policy"},
    "falsePositivePolicyRef": {"name": "repo-security-policy", "key": "false-positives"},
    "analysisAgentRef": {"name": "security-reviewer"}
  }
}
```

**Response (201):** The created `RepositoryScan` resource.

Required fields are `name`, `spec.repoURL`, and `spec.analysisAgentRef.name`. The API defaults or infers provider, owner, repository, branch, and validation mode where possible. Set `spec.ref` to pin scan tasks to a specific tag, branch, or commit SHA; when `ref` is set without `branch`, scan workspaces check out that ref directly instead of forcing the default `main` branch.

The request accepts the same `RepositoryScan` spec fields as the CRD, including automatic validation tuning (`validationMaxFindingsPerRun`, `validationMinSeverity`, `validationMinConfidence`) and ConfigMap-backed scanner policy refs (`customScanInstructionsRef`, `falsePositivePolicyRef`). Policy ConfigMaps must be in the same namespace and opt in with `orka.ai/security-policy: "true"` as a label or annotation.

### Security findings workflow

A typical remediation workflow is:

1. List findings with `GET /api/v1/security/repositories/:name/findings?namespace=default&recommended=true`.
2. Inspect evidence with `GET /api/v1/security/findings/:id`.
3. Optionally validate with `POST /api/v1/security/findings/:id/validate`.
4. Generate a patch with `POST /api/v1/security/findings/:id/patch`.
5. Review patch proposals with `GET /api/v1/security/findings/:id/patches`. A proposal is successful only after the governed publication is verified and the agent's patch result envelope matches the diff derived from the published commit; the stored diff and summary artifacts come from that verification, never from agent-written files.
6. Create a remediation pull request with `POST /api/v1/security/findings/:id/pull-request`.

Review slice and dropped-output inspection:

1. List slices with `GET /api/v1/security/repositories/:name/slices?namespace=default`.
2. Inspect one slice with `GET /api/v1/security/repositories/:name/slices/:sliceID?namespace=default`.
3. List rejected v2 model output with `GET /api/v1/security/repositories/:name/dropped-findings?namespace=default&scanRunID=scan_...&layer=filter&reason=contains=rate-limit`.

## Repository monitors

Repository monitor endpoints manage `RepositoryMonitor` configurations and their durable monitor runs, issue/PR inventory, command events, workflow actions, typed action records, implementation jobs, GitHub mutation audit records, review/repair state, readiness state, and audit events.

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/v1/monitors/repositories` | POST | Create a repository monitor |
| `/api/v1/monitors/repositories` | GET | List repository monitors |
| `/api/v1/monitors/repositories/:name` | GET | Get repository monitor details |
| `/api/v1/monitors/repositories/:name` | PUT | Update repository monitor spec |
| `/api/v1/monitors/repositories/:name` | DELETE | Delete repository monitor |
| `/api/v1/monitors/repositories/:name/runs` | POST | Trigger a manual monitor run |
| `/api/v1/monitors/repositories/:name/runs` | GET | List monitor runs |
| `/api/v1/monitors/repositories/:name/items` | GET | List current monitor items |
| `/api/v1/monitors/repositories/:name/commands` | POST | Create an explicit issue/PR workflow command |
| `/api/v1/monitors/commands?name=` | GET | List durable command events. **`name` is required.** |
| `/api/v1/monitors/commands/:id` | GET | Get a command event |
| `/api/v1/monitors/work-actions?name=` | GET | List durable workflow actions and leases. **`name` is required.** |
| `/api/v1/monitors/work-actions/:id` | GET | Get a workflow action |
| `/api/v1/monitors/actions?name=` | GET | List typed action records. **`name` is required.** |
| `/api/v1/monitors/actions/:id` | GET | Get a typed action record |
| `/api/v1/monitors/implementation-jobs?name=` | GET | List issue implementation jobs. **`name` is required.** |
| `/api/v1/monitors/implementation-jobs/:id` | GET | Get an issue implementation job |
| `/api/v1/monitors/mutations?name=` | GET | List controller-owned GitHub mutation audit records. **`name` is required.** |
| `/api/v1/monitors/mutations/:id` | GET | Get a GitHub mutation audit record |
| `/api/v1/monitors/events?name=` | GET | List monitor audit events. **`name` is required.** |

Common query parameters:

- `namespace` - Kubernetes namespace to operate in.
- `limit` - page size for list endpoints.
- `continue` or `cursor` - pagination cursor for store-backed list endpoints.
- `kind`, `number`, `state`, `verdict`, `repairState`, and `automergeState` - filters for `GET /api/v1/monitors/repositories/:name/items`.
- `name`, `runID`, `itemKind`, `itemNumber`, and `eventType` — filters for `GET /api/v1/monitors/events`.

:::warning[Six list endpoints require `?name=`]
`/monitors/events`, `/monitors/commands`, `/monitors/actions`, `/monitors/work-actions`,
`/monitors/implementation-jobs`, and `/monitors/mutations` take the monitor name as a
**query parameter**, not a path segment. Omitting it returns `400` with
`name query parameter is required`. The `/monitors/repositories/:name/...` routes are the
ones that use a path segment.
:::

Context-token authorization scopes are `orka:monitors:read` for list/get endpoints, `orka:monitors:write` for create/update/delete, and `orka:monitors:operate` for manual run creation.

### Create repository monitor

**Endpoint:** `POST /api/v1/monitors/repositories`

**Request Body:**
```json
{
  "name": "example-app",
  "namespace": "default",
  "spec": {
    "provider": "github",
    "repoURL": "https://github.com/example/app",
    "branch": "main",
    "gitSecretRef": {"name": "repo-monitor-github"},
    "schedule": "*/30 * * * *",
    "targets": {
      "pullRequests": {
        "enabled": true,
        "includeDrafts": false,
        "maxPerRun": 10
      }
    },
    "agents": {
      "reviewer": {"name": "repo-reviewer"}
    },
    "review": {
      "event": "COMMENT",
      "staleReviewTTL": "24h",
      "exactEventEnabled": true
    },
    "policy": {
      "protectedLabels": ["security-sensitive"],
      "pauseLabels": ["orka:pause"]
    },
    "validation": {
      "image": "ghcr.io/example/app-validation@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
    }
  }
}
```

**Response (201):** The created `RepositoryMonitor` resource.

Required fields are `name`, `spec.repoURL`, and `spec.agents.reviewer.name` when pull request monitoring is enabled. The API defaults or infers provider, owner, repository, branch, pull request enablement, pull request `maxPerRun`, and `review.event` where possible. `spec.repoURL` must be a credential-free GitHub repository root URL such as `https://github.com/owner/repo`, `https://github.com/owner/repo.git`, or `git@github.com:owner/repo.git`; pull request, issue, branch/tree, blob/file, commit, query-string, fragment, non-GitHub, HTTP, and embedded-credential URLs are rejected.

`spec.validation.image` optionally configures isolated pull request validation. The image must use an immutable `@sha256:` digest. The reviewer chooses one offline shell command after inspecting the repository. Orka runs it in the configured image against the exact read-only PR head, releases it only after a deny-all NetworkPolicy exists, and independently verifies the child Task before accepting a `passed` verdict. The image must contain `/bin/sh`, every required tool such as `golangci-lint`, Terraform, or Azure CLI, and any dependencies the command needs. Commands, args, credentials, and network access are not configured on the monitor.

GitHub pull request and issue targets are supported. Commit targets are rejected.
`review.requireGreenCI` is supported for gating review selection on green CI.
Pull request monitoring requires `spec.agents.reviewer.name`. The reviewer Agent must
use a built-in `claude`, `codex`, or `opencode` runtime and omit `spec.secretRef`;
the runtime proxy supplies provider credentials. External `runtimeRef` reviewers
are rejected. Issue-only monitors can set `targets.pullRequests.enabled: false`
and `targets.issues.enabled: true`. When `gitSecretRef` is set, the Git Secret must
exist in the monitor namespace and contain a non-empty `token`, `password`, or
`GITHUB_TOKEN` key.

### Trigger manual monitor run

**Endpoint:** `POST /api/v1/monitors/repositories/{name}/runs`

**Request Body:**
```json
{
  "targetKind": "pull_request",
  "targetNumber": 123,
  "targetSHA": "abc123"
}
```

The request body can be omitted to run a full inventory pass. `targetKind` may be empty, `pull_request`, or `issue`; `targetNumber` and `targetSHA` narrow the run to one issue, one PR, or an exact PR head. When `targetNumber` is set, the controller fetches that target directly from GitHub and does not retire unrelated monitor items. The API returns `409` when the monitor already has a queued or running run.

### Create monitor command

**Endpoint:** `POST /api/v1/monitors/repositories/{name}/commands`

**Request Body:**
```json
{
  "kind": "issue",
  "number": 123,
  "intent": "plan",
  "targetSHA": ""
}
```

Supported issue intents are `triage`, `research`, `plan`, `implement`, `decompose`, `stop`, and `resume`. Supported pull request intents are `review`, `fix`, `fix_ci`, `update_branch`, `stop`, and `resume`. Head-bound pull request commands (`review`, `fix`, `fix_ci`, `update_branch`) must include `targetSHA`; `stop` and `resume` can omit it. The command creation endpoint always requires `orka:monitors:operate`. Mutating intents (including `implement`, `decompose`, `fix`, `fix_ci`, `update_branch`, `stop`, and `resume`) additionally require `orka:monitors:write`; `review` also requires monitor-write when review publishing is enabled. The endpoint validates that the target kind is enabled, records a durable command event, and queues a targeted monitor run.

### List monitor commands, actions, implementations, and mutations

**Endpoints:**

- `GET /api/v1/monitors/commands?namespace=&name=&kind=&number=&intent=&status=`
- `GET /api/v1/monitors/commands/{id}`
- `GET /api/v1/monitors/work-actions?namespace=&name=&kind=&number=&intent=&desiredAction=&status=&taskName=`
- `GET /api/v1/monitors/work-actions/{id}`
- `GET /api/v1/monitors/actions?namespace=&name=&kind=&number=&actionKind=&taskName=`
- `GET /api/v1/monitors/actions/{id}`
- `GET /api/v1/monitors/implementation-jobs?namespace=&name=&issueNumber=&phase=&taskName=`
- `GET /api/v1/monitors/implementation-jobs/{id}`
- `GET /api/v1/monitors/implementation-jobs/{id}/patch-preview`
- `GET /api/v1/monitors/mutations?namespace=&name=&kind=&number=&operation=&status=`
- `GET /api/v1/monitors/mutations/{id}`

Command events record label/API intake, actor/source authorization, target SHA/snapshot bindings, status, and errors. Work actions are the durable queue/lease view for prerequisites and follow-up work. Action records store typed triage/research/plan/implementation/review/repair/readiness outcomes. Implementation jobs track issue coding attempts, patch artifacts, validation state, branches, and linked PRs. Mutation records audit every controller-owned GitHub write such as label consumption, review submission, branch pushes, PR creation, and readiness statuses.

See [Repository Monitors](../guides/repository-monitors.md) for the full workflow and CRD example.

## Auth

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/v1/auth/validate` | GET | Validate auth token |

## Secrets

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/v1/secrets` | GET | List secret names (metadata only) |

## Chat

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/v1/chat` | POST | Send message (SSE streaming or JSON) |
| `/api/v1/chat/config` | GET | Get chat configuration and available tools |
| `/api/v1/chat/:sessionId` | DELETE | Cancel a chat session |

See [Interactive Chat](../guides/chat.md) for full chat documentation.

## OpenAI-compatible API

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/openai/v1/chat/completions` | POST | Chat completions (streaming & non-streaming) |
| `/openai/v1/responses` | POST | Stateless Responses (streaming & non-streaming; requires `store:false`) |
| `/openai/v1/models` | GET | List available models |

See [OpenAI Compatibility](openai-compat.md) for details.

## Anthropic-compatible API

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/anthropic/v1/messages` | POST | Create a message (streaming & non-streaming) |
| `/anthropic/v1/models` | GET | List available models |

The `/anthropic/v1/messages` endpoint injects built-in tools and runs server-side tool execution by default. Set `X-Orka-Tools: disabled` header to use as a transparent proxy instead. See [Anthropic Compatibility](anthropic-compat.md) for details.

## Internal API (worker communication)

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/internal/v1/results/:namespace/:taskName` | POST | Submit task result |
| `/internal/v1/artifacts/:namespace/:taskName/:filename` | POST | Upload task artifact |
| `/internal/v1/sessions/:namespace/:name/transcript` | GET | Get session transcript |
| `/internal/v1/plans/:namespace/:taskName` | POST | Save plan state |
| `/internal/v1/plans/:namespace/:taskName` | GET | Get plan state |
| `/internal/v1/messages/:namespace` | POST | Send inter-agent message |
| `/internal/v1/messages/:namespace/:taskName` | GET | Get messages for a task |

### Save plan state

Workers call this to persist autonomous plan state.

**Endpoint:** `POST /internal/v1/plans/{namespace}/{taskName}`

**Request Body:**
```json
{
  "summary": "Completed phase 1",
  "progress_pct": 25,
  "goal_complete": false,
  "plan_document": "# Plan\n..."
}
```

**Response:** `204 No Content`

### Get plan state

Workers call this to load the current plan state at startup.

**Endpoint:** `GET /internal/v1/plans/{namespace}/{taskName}`

**Response (200):** Same as public plan endpoint.

**Errors:**
- `404` — No plan found

### Send message

Workers call this to send messages to sibling tasks (same parent coordinator).

**Endpoint:** `POST /internal/v1/messages/{namespace}`

**Request Body:**
```json
{
  "fromTask": "worker-a",
  "toTask": "worker-b",
  "parentTask": "coordinator",
  "content": "Found a bug in the auth module"
}
```

Use `"toTask": "*"` to broadcast to all siblings.

**Response:** `204 No Content`

### Get messages

Workers call this to check for unread messages.

**Endpoint:** `GET /internal/v1/messages/{namespace}/{taskName}?parentTask={parentTask}&markRead={true|false}`

**Query Parameters:**
- `parentTask` (required) — Parent coordinator task name (scopes messages to siblings)
- `markRead` (optional, default: `true`) — Whether to mark returned messages as read

**Response (200):**
```json
[
  {
    "id": 1,
    "namespace": "default",
    "fromTask": "worker-b",
    "toTask": "worker-a",
    "parentTask": "coordinator",
    "content": "Found a bug in the auth module",
    "read": false,
    "createdAt": "2026-01-15T10:30:00Z"
  }
]
```

## Health

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/healthz` | GET | Health check |
| `/readyz` | GET | Readiness check |

## Example usage

```bash
# Create a task
curl -X POST http://localhost:8080/api/v1/tasks \
  -H "Authorization: Bearer $(kubectl create token orka-client)" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "my-task",
    "type": "ai",
    "agentRef": {"name": "assistant"},
    "prompt": "Explain microservices architecture"
  }'

# Get task result
curl http://localhost:8080/api/v1/tasks/my-task/result \
  -H "Authorization: Bearer $(kubectl create token orka-client)"

# List task artifacts
curl http://localhost:8080/api/v1/tasks/my-task/artifacts \
  -H "Authorization: Bearer $(kubectl create token orka-client)"

# Download an artifact
curl -L http://localhost:8080/api/v1/tasks/my-task/artifacts/output.json \
  -H "Authorization: Bearer $(kubectl create token orka-client)" \
  -o output.json

# Chat with SSE streaming
curl -N http://localhost:8080/api/v1/chat \
  -H "Authorization: Bearer $(kubectl create token orka-client)" \
  -H "Content-Type: application/json" \
  -d '{
    "message": "Create an AI task that summarizes Kubernetes best practices",
    "sessionId": "my-session"
  }'
```

## Built-in Tools

These tools are available to AI worker agents:

| Tool | Description | Parameters |
|------|-------------|------------|
| `web_search` | Search the web using a configured search API or DuckDuckGo | `query` (required), `limit` (default 5) |
| `code_exec` | Execute code in a sandboxed environment | `language` (python/javascript/bash), `code`, `timeout` (max 60s) |
| `file_read` | Read files from the workspace | `path`, `offset`, `limit` (max 1MB) |
| `web_fetch` | Fetch and extract URL content | `url` (required), `max_chars` (default 50000), `raw` |
| `file_write` | Write or append files in workspace paths | `path` (required), `content` (required), `mode` (`write`/`append`), `create_dirs` |

### Coordination Tools

These tools are injected into AI worker agents when the Agent has `coordination.enabled: true`. They are not returned by `GET /api/v1/tools`.

The following tools are **auto-injected** when coordination is enabled:

| Tool | Description | Parameters |
|------|-------------|------------|
| `delegate_task` | Delegate a subtask to another agent | `agent`, `prompt` (required); `namespace`, `priority`, `auto_retry`, `max_retries` |
| `wait_for_tasks` | Wait for delegated tasks to complete | `tasks` (required), `timeout` (default 10m) |
| `create_container_task` | Create a child container task | `name`, `image`, `command`/`args`, env/workspace fields |
| `cancel_task` | Cancel a running child task | `task_name` (required); `namespace`, `reason` |
| `send_message` | Send a message to a sibling task | `to_task` (required, or `*` to broadcast), `content` (required) |
| `check_messages` | Check for messages from sibling tasks | `mark_read` (boolean, default true) |
| `recall_memory` | Recall durable namespace-scoped memories | `query`, `tags`, `task_name`, `agent_name`, `source`, `limit`, `include_disabled` |
| `remember` | Submit a durable memory proposal for review | `content` (required); `title`, `description`, `tags`, `agent_name` |
| `propose_memory` | Submit a memory-adjacent governance proposal | `title` (required); `type`, `skill_name`, `description`, `content`, `patch`, `agent_name` |
| `search_transcript` | Search prior session transcripts | `query` (required); `session_name`, `exclude_session_name`, `roles`, `limit`, `max_snippet_length` |
| `create_pull_request` | Create a GitHub pull request | `task_name`, `head_branch`, `base_branch`, `title` (required); `body` |
| `check_pull_request_ci` | Check GitHub CI status without merging | `pr_number` (required); `task_name`, `repo_url`, `wait_timeout`, `poll_interval` |
| `list_pull_requests` | List open pull requests in a repository | `task_name`, `repo_url`; `per_page`, `page` |
| `check_pr_review_marker` | Check for an existing Orka PR review marker on a PR head | `pr_number` (required); `task_name`, `repo_url`, `head_sha` |
| `merge_pull_request` | Merge a GitHub pull request | `task_name`, `pr_number` (required); `merge_method`, `commit_title`, `commit_message` |
| `auto_merge_pull_request` | Poll CI checks and merge a PR when all pass | `task_name`, `pr_number` (required); `merge_method`, `commit_title`, `commit_message`, `timeout` |
| `review_pull_request` | Fetch PR diff for review | `pr_number` (required); `task_name`, `repo_url` |
| `post_review_comment` | Post a review on a PR | `pr_number`, `body`, `event` (required); `task_name`, `repo_url`, `comments` |
| `create_agent` | Create an Agent CRD at runtime | `name`, `provider`, `model` (required); `systemPrompt` (required except OpenCode; omit for OpenCode), `tools`, `coordination` |
| `delete_agent` | Delete an Agent CRD | `name` (required), `namespace` |
| `update_plan` | Update the autonomous execution plan | `summary`, `plan_document` (required); `progress_pct`, `goal_complete` |

The following tools require explicit `spec.tools[]` entries on the Agent CRD:

| Tool | Description | Parameters |
|------|-------------|------------|
| `list_issues` | List open GitHub issues in a repository | `task_name`, `repo_url`; `unassigned_only` (default true), `per_page`, `page` |
| `get_issue` | Fetch full details of a GitHub issue | `issue_number` (required); `task_name`, `repo_url` |
| `comment_on_issue` | Post a comment on a GitHub issue | `issue_number`, `body` (required); `task_name`, `repo_url` |

For GitHub tools that accept `repo_url`, explicit repository URLs are scope-checked when task context is available. The requested repository must match the current task's workspace repository or signed transaction repository context; otherwise the tool fails closed before resolving credentials or calling GitHub.

`create_pr_monitor` is exposed through the chat/management tool set rather than auto-injected into every coordinator worker. Parameters are `name`, `repo_url`, `schedule`, and `agent_ref` (required), plus optional `namespace`, `provider_ref`, `gitSecretRef`, `per_page`, `review_event`, and `prompt`. `repo_url` must be a credential-free GitHub repository root URL such as `https://github.com/owner/repo`, `https://github.com/owner/repo.git`, or `git@github.com:owner/repo.git`; pull request, issue, branch/tree, blob/file, commit, query-string, fragment, non-GitHub, HTTP, and embedded-credential URLs are rejected.

`create_pr_monitor` is the compatibility path for prompt-orchestrated scheduled PR monitors. It creates a scheduled `type: ai` Task, sets `spec.workspace.gitRepo` to `repo_url`, injects only the PR review loop tools, and instructs the task to pass the same `repo_url` to `list_pull_requests`, `check_pr_review_marker`, `check_pull_request_ci`, `review_pull_request`, and `post_review_comment`. The Agent referenced by `agent_ref` must be an AI Agent with coordination enabled and autonomous coordination disabled. The `create_pr_monitor` tool accepts its compatibility `gitSecretRef` parameter (or a supported default Secret name) and maps the selected Secret into the created Task's top-level `spec.workspace.readCredentialRef`. When the parameter is omitted, the supported default Secret names are `git-credentials`, `github-credentials`, `copilot-token`, `github-token`, and `git-token`. The selected Secret must contain a non-empty `token`, `password`, or `GITHUB_TOKEN` key.

`check_pr_review_marker` returns a hidden marker that the monitor should include unchanged in the subsequent review body. The marker includes `repo`, `pr`, `head_sha`, and `sig` fields, and matching is scoped to that repository, pull request, and exact head. Marker signatures are stable across GitHub token rotation. Operators can set `ORKA_PR_REVIEW_MARKER_SECRET` in the worker Task environment for dedicated marker signing and `ORKA_PR_REVIEW_MARKER_PREVIOUS_SECRETS` for comma-separated previous keys during rotation. Legacy markers are accepted only from a trusted review author, configured with `ORKA_PR_REVIEW_MARKER_TRUSTED_AUTHOR` or resolved from the Task's authenticated GitHub user.
