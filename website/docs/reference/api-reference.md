---
slug: /api-reference
description: "Every Orka HTTP endpoint: tasks, sessions, memory, agents, tools, connectors, security scans, monitors, chat, and the compatibility APIs."
---

# API reference

The controller exposes a REST API for programmatic access. The request bodies for resource
endpoints use the custom resource shapes in the [Resource reference](resources.md), and the
permissions each route needs are in [API authorization](api-authorization.md).

Almost every `/api/v1/*` endpoint requires authentication. Kubernetes ServiceAccount tokens are the default;
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

The controller requires `ORKA_GITHUB_WEBHOOK_SECRET` and verifies the `X-Hub-Signature-256` header. The `orka:implement` issue label queues the managed repository workflow; configured pause-label changes queue fresh reconciliation. Pull request events can also queue exact-head `RepositoryMonitor` runs when a matching monitor has `spec.review.exactEventEnabled: true`. See [Issue-to-PR automation](../guides/issue-to-pr-automation.md#start-from-a-github-label) for configuration and webhook behavior.

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

`POST /api/v1/tasks` takes `name`, `namespace`, and a `spec` with the full set of
[Task](resources.md#task) fields. As a shorthand, common fields — `type`, `agentRef`,
`prompt`, `image`, `command`, `args`, `env`, `timeout`, `priority`, `retryPolicy`,
`workspace`, `agentRuntime`, `sessionRef`, `schedule`, and a few more — can also go at the
top level, as in the [example](#example-usage) below. Fields outside that set, such as
`resources` or the schedule history limits, take effect only under `spec`.

Task and Session event history, live streams, traces, forks, and approvals have their own
endpoints, described in [Execution events](execution-events.md).

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

Common list query parameters: `namespace`, `taskName`, `agentName`, `type`, `status`, `query`/`q`, and `limit`. Review returns the stored review record (`id`, `namespace`, `status`, `reviewer`, `reviewNote`, `reviewedAt`), and archive returns `204 No Content`. Apply returns the linked durable memory JSON; repeated apply requests return the same memory. The server records the request's signed-in identity as `reviewer` and `appliedBy`; a body with a non-empty `reviewer` (review) or `appliedBy` (apply) returns `400 Bad Request`, and a request without an identity returns `401 Unauthorized`.

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
| `/api/v1/agents/:name/system-prompt` | GET | Get the system prompt an agent reads from its `systemPrompt.configMapRef` |
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


## Gateways

See [Gateway API](gateway-api.md) for the adapter contract, Kubernetes resources, durable ledger endpoints, filters, and retry workflow.

## Tools

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/v1/tools` | GET | List tools (built-in + CRDs) |
| `/api/v1/tools/:name` | GET | Get tool details |

`GET /api/v1/tools/:name` returns built-in tool metadata or the full [Tool](resources.md#tool)
resource. Coordination tools are not listed; see [Built-in tools](built-in-tools.md).

## Connector endpoints

These routes manage [Connections](resources.md#connection) to a
[ConnectorProvider](resources.md#connectorprovider). They are available when the controller runs with `--connectors-enabled` and `--connector-callback-base-url`. Every route except the OAuth callback requires a verified OIDC or context-token identity carrying an issuer and subject; ServiceAccount bearer tokens are refused with 403. The callback is the provider's redirect target and carries no user token: the signed single-use consent `state` authenticates it against the pending consent instead, and the server-side PKCE verifier binds the code exchange to that consent. Under context-token authorization, reads need `orka:connectors:read` and every mutation needs `orka:connectors:manage`, so a delegated token narrowed to other work cannot inspect or revoke a person's accounts. A person sees and changes only Connections whose `spec.subject` matches their identity; a foreign Connection reads as 404. Responses never carry token material.

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/v1/connectors` | GET | List the provider catalog: name, display name, readiness, scopes, and tools with their `read`/`write` class. |
| `/api/v1/connections` | GET | List the caller's Connections. |
| `/api/v1/connections` | POST | Body `{"provider": "github", "mode": "readOnly"}`. Creates or reuses the caller's Connection for that provider and returns `{"connection": ..., "authorizeURL": ...}`. The browser opens `authorizeURL`; unknown body fields, including any subject, are rejected. |
| `/api/v1/connections/:name` | GET | One Connection. |
| `/api/v1/connections/:name` | PUT | Body `{"mode": "readWrite"}` (`mode` is required). Whenever the granted scopes do not cover the requested mode the response carries a new `authorizeURL`, so widening to `readWrite`, and retrying it, asks for the write scopes; narrowing takes effect immediately. |
| `/api/v1/connections/:name` | DELETE | Disconnect. When the provider names a `revocationURL`, the controller's finalizer revokes the committed tokens (against the client and revocation endpoint sealed with them, so a moved token endpoint does not skip revocation); it then deletes the sealed material. GitHub has no revocation endpoint, so a GitHub disconnect only deletes the tokens Orka holds. A provider that refuses a revocation is best effort; a missing client Secret or key is not: custody and the finalizer are kept, and the disconnect retries until the Secret is restored, unless the provider itself is being deleted (namespace teardown), in which case the tokens are left to expire and the disconnect finishes. |
| `/api/v1/connections/:name/authorize` | POST | Restart consent for an existing Connection, for example after the provider revoked it. |
| `/api/v1/connections/callback` | GET | OAuth redirect target. Unauthenticated: the signed single-use `state` authenticates it against the pending consent, and the server-side PKCE verifier binds the code exchange to that consent. It exchanges the code, parks the tokens sealed as a pending completion, and redirects to `<callback base>/settings/connectors?status=pending&connection=<name>&namespace=<namespace>#completion=<token>` (the namespace the consent was sealed in, which the completion must target), or `?status=error&reason=<code>`. A provider that grants fewer scopes than the mode requires is refused (`reason=scopes_denied`); the issued token is discarded and left to expire, never revoked, because Orka cannot prove whose grant a token nobody committed belongs to. The requested set is the one sealed when consent started, never the provider's current configuration. A provider that omits the `scope` field granted the requested set; an explicitly empty `scope` is a grant of nothing and is refused. Granted scope lists are split on spaces and, as GitHub reports them, commas; a configured scope name may not contain a comma, so splitting never fabricates a required scope. A provider whose OAuth client changed since consent started is refused before any exchange (`reason=provider_changed`). So is a Connection whose mode changed while the person was at the provider (`reason=mode_changed`). Consents are numbered when they start; a callback from a consent older than one already parked or committed for the Connection is refused (`reason=consent_superseded`), and a newer callback replaces an older uncommitted completion, so callbacks that finish out of order never let an older grant replace a newer one. |
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
  "namespace": "orka-system",
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

1. List findings with `GET /api/v1/security/repositories/:name/findings?namespace=orka-system&recommended=true`.
2. Inspect evidence with `GET /api/v1/security/findings/:id`.
3. Optionally validate with `POST /api/v1/security/findings/:id/validate`.
4. Generate a patch with `POST /api/v1/security/findings/:id/patch`.
5. Review patch proposals with `GET /api/v1/security/findings/:id/patches`. A proposal is successful only after the governed publication is verified and the agent's patch result envelope matches the diff derived from the published commit; the stored diff and summary artifacts come from that verification, never from agent-written files.
6. Create a remediation pull request with `POST /api/v1/security/findings/:id/pull-request`.

Review slice and dropped-output inspection:

1. List slices with `GET /api/v1/security/repositories/:name/slices?namespace=orka-system`.
2. Inspect one slice with `GET /api/v1/security/repositories/:name/slices/:sliceID?namespace=orka-system`.
3. List rejected v2 model output with `GET /api/v1/security/repositories/:name/dropped-findings?namespace=orka-system&scanRunID=scan_...&layer=filter&reason=contains=rate-limit`.

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
  "namespace": "orka-system",
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

## Internal API

Workers call `/internal/v1` routes to submit results and artifacts, read transcripts, save
plans, and exchange messages. They are not for clients; see
[Controller internals](../development/internals.md#internal-worker-api).

## Health

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/healthz` | GET | Health check |
| `/readyz` | GET | Readiness check |

## Example usage

```bash
# Create a task
curl -X POST http://localhost:8080/api/v1/tasks \
  -H "Authorization: Bearer $(kubectl -n orka-system create token orka-client)" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "my-task",
    "type": "ai",
    "agentRef": {"name": "assistant"},
    "prompt": "Explain microservices architecture"
  }'

# Get task result
curl http://localhost:8080/api/v1/tasks/my-task/result \
  -H "Authorization: Bearer $(kubectl -n orka-system create token orka-client)"

# List task artifacts
curl http://localhost:8080/api/v1/tasks/my-task/artifacts \
  -H "Authorization: Bearer $(kubectl -n orka-system create token orka-client)"

# Download an artifact
curl -L http://localhost:8080/api/v1/tasks/my-task/artifacts/output.json \
  -H "Authorization: Bearer $(kubectl -n orka-system create token orka-client)" \
  -o output.json

# Chat with SSE streaming
curl -N http://localhost:8080/api/v1/chat \
  -H "Authorization: Bearer $(kubectl -n orka-system create token orka-client)" \
  -H "Content-Type: application/json" \
  -d '{
    "message": "Create an AI task that summarizes Kubernetes best practices",
    "sessionId": "my-session"
  }'
```

## Built-in tools

The tools Orka gives agents, and their parameters, are documented in
[Built-in tools](built-in-tools.md).
