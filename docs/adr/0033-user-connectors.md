# ADR 0033: Per-user connectors for third-party services

Date: 2026-09-25

## Status

Accepted and implemented behind `--connectors-enabled` (default off): the
`ConnectorProvider` and `Connection` resources and their reconcilers, the
consent flow and sealed custody, injection through `connection`-mode
`OutboundAccessPolicy`, controller-only execution, the GitHub built-ins, and
the dashboard, CLI, and `list_connections` surfaces. The chat decision below
was narrowed when the surfaces landed. Builds on the outbound-access split in
[ADR 0011](0011-vendor-neutral-transaction-and-outbound-access.md) and reuses the
`OutboundAccessPolicy` resolver, the ACP MCP broker, approvals, and the
external-effect ledger. Proactive (event-driven) connectors and a remote MCP
server backend with tool discovery are noted as follow-ups, not decided here.

## Context

Consumer assistants (Meta AI, OpenClaw, ScoutOS) let a person link accounts such
as Gmail, Google Docs, or Spotify once, after which any agent acting for that
person can use them: read by default, act after approval, disconnect instantly.
Orka has nothing equivalent. Every credential today is namespace- or
Task-scoped, static, and operator-managed:

- A `Tool` reaches a service with a static `authSecretRef` or an
  `OutboundAccessPolicy` that performs service-to-service token exchange. Nothing
  is scoped to the person who asked for the work.
- Identity exists only as `Task.spec.requestedBy`, stamped for OIDC and
  context-token callers. ServiceAccount callers carry no human identity.
- The controller-hosted MCP broker already executes custom Tools on behalf of ACP
  runtimes, so runtime Pods never hold credentials. Native `type: ai` workers run
  the Tool executor in-Pod and can read Secrets directly.
- Approvals gate consequential tool calls through the broker, the UI, the CLI,
  and the API. `brokeredToolClass` already separates read from write tools.
- There is no OAuth authorization-code flow, no refresh-token handling, no
  settings page in the UI, and no user-scoped storage anywhere.

The design has to add per-user consent and token custody without opening a new
path by which agent processes, worker Pods, or namespace users can read a
person's third-party tokens.

## Decision

### Two resources: a catalog and a linked account

`ConnectorProvider` is the operator-owned catalog entry for one service. It
holds the OAuth client configuration (authorize and token endpoints, PKCE,
scopes grouped by capability, revocation endpoint, client secret reference) and
declares how tools reach the service: a curated set of HTTP tool definitions
carried in the provider, or, later, a remote MCP server URL. Provider URLs pass
the same public-address and SSRF validation as `internal/outboundaccess`.

`Connection` is one person's linked account with one provider. Its spec carries
an immutable `subject`, a `providerRef`, and a `mode` of `readOnly` or
`readWrite`. Its status carries state, the granted scopes, expiry, and
last-refresh time, never token material. Deleting the Connection is the disconnect.

### Only verified human identities own Connections

A Connection may be created only by a caller whose subject Orka verified through
OIDC or a context token, the same identities that receive `spec.requestedBy`
today. ServiceAccount callers fail closed. The subject is taken from the
authenticated request, never from the body, and the API lists and mutates only
Connections whose subject matches the caller.

### Token custody: sealed rows in the controller store, one wrapped key per Connection

Token material for a Connection is stored as a sealed row in the controller's
persistent SQLite store, never as a Kubernetes Secret. It is reachable only from
the controller process or by mounting its data volume, which is a smaller
surface than any namespace: real clusters have several principals with
cluster-wide Secret read (cluster-admins, backup tooling, external-secrets,
cert-manager), and a dedicated namespace would need new hand-written RBAC whose
mistakes silently widen exposure. This was chosen over the earlier
"controller-only namespace" draft after weighing 1000 people with 100 linked
accounts each.

Each Connection gets its own random data key. Tokens are sealed with that key
using AES-256-GCM and additional data binding the Connection UID, subject, and
provider; the data key is itself sealed with the controller's existing
agent-execution snapshot key (`--agent-execution-snapshot-key-file` /
`--agent-execution-snapshot-secret`) using the same cipher the snapshot store
uses. Deleting a Connection deletes its wrapped key, so the ciphertext in the
live store becomes unrecoverable. That crypto-shredding does not reach copies
Orka did not make: a volume snapshot, filesystem backup, or database copy taken
while the row existed still holds the ciphertext together with its wrapped data
key, and both open with the snapshot key of that time. Operators who need
deletion to reach backups must bound backup retention, or change the snapshot
key and discard copies sealed under the old one (today that means every
person disconnects first; see Consequences). Raw tokens never appear in
Task specs, status, events, logs, or anywhere in the store outside the sealed
column; the controller records its own audit events for connector use.

### Consent flow lives in the API server

Creating a Connection returns an authorize URL. The `state` parameter is signed,
short-lived, and bound to the caller's subject and the Connection UID. A
callback route exchanges the code with PKCE, stores the encrypted material, and
marks the Connection ready. The callback base URL is an operator flag; the
provider's redirect URI must match it exactly. Refresh happens lazily at call
time, single-flight per Connection, with write-back of rotated refresh tokens.

### Credential injection reuses OutboundAccessPolicy

`OutboundAccessPolicy` gains a third mutually exclusive mode, `connection`,
naming a `ConnectorProvider`. At call time the resolver looks up the Connection
for the executing Task's `requestedBy` subject and emits the bearer header. No
Connection, an expired or revoked Connection, or a Task without a verified
`requestedBy` fails closed. Tasks created by chat or by delegation inherit
`requestedBy` and therefore the same Connections. Gateway-originated Tasks carry
a gateway-issued sender subject and fail closed until an explicit link between a
gateway sender and a Connection exists.

The execution snapshot freezes the Connection UID and generation, not the
sealed row's version, because refresh rotates the row continuously.

### Connector tools execute only in the controller

Tools whose policy is in `connection` mode execute in the controller-hosted
broker for ACP runtimes and through an internal controller endpoint for native
`type: ai` workers. The in-Pod executor refuses them. Worker Pods therefore
never receive a person's tokens, matching the ACP credential boundary.

### Chat reaches connectors through Tasks, with one read-only exception

The in-process chat loop does not run connector-backed Tools. A custom Tool
behind a `connection`-mode policy reaches a person's account only from a Task,
stamped with the caller's `requestedBy`, which goes through the broker,
approval gate, and external-effect ledger. The dashboard chat offers only
`list_connections`. This costs latency on simple reads and is accepted so that
writes have exactly one execution path enforcing the rules above.

One exception was accepted when the user surfaces landed. In coordinator mode
the compatibility proxies run the GitHub read built-ins (for example
`check_pull_request_ci`) directly as the signed-in person when that person
holds a Ready link to a provider that declares the tool. There is no dispatch
to freeze, so the Connection and its provider are read live on every call, and
a link that exists but cannot be used fails the call rather than falling back.
Under enforced context-token authorization, a token without the connector-read
scope uses no linked account there. These surfaces have no approval gate, so a
linked write built-in such as `create_pull_request` is refused on them once the
person has a link; linked writes run only from a Task, where they wait for
approval. The cost is a second, read-only path whose checks mirror the Task
path's rather than reusing its frozen snapshot.

### Read by default, approve to act, disconnect instantly

Connector tools carry a `read` or `write` class (`spec.tools[].class`). A
`readOnly` Connection removes write tools from the effective tool list. Write
tools default into the Agent's `approvalRequiredTools`, so the existing broker gate and approval UI ask before
an email is sent or a document is changed. Every write call is recorded as
an `ExternalEffect` with a Connection digest for audit; read calls leave no
effect record. Deleting a Connection
deletes its sealed row and wrapped key, best-effort revokes the upstream token,
and causes in-flight calls to fail closed.

### First provider: GitHub

The first `ConnectorProvider` is GitHub via OAuth. The GitHub tools already exist
and currently resolve tokens from per-Task workspace Secrets; the connector path
lets them resolve the requester's Connection instead. This proves the consent
flow, custody, injection, and approval path with no new tool definitions. Gmail,
Google Docs, and Spotify follow as curated HTTP providers; a generic hosted MCP
backend with tool discovery is a later ADR.

### User surfaces

A Settings > Connectors page in the UI lists a person's Connections, starts the
consent flow, toggles `mode`, and disconnects. The CLI gains
`orka connect <provider>` reusing the browser handoff from `orka login`. Chat and
agents gain a `list_connections` tool so an agent can ask the person to connect a
service it needs.

## Consequences

- Connectors require an OIDC issuer or a context-token profile; local kind demos
  need an OIDC stub before any account can be linked.
- The snapshot key becomes load-bearing for connector custody as well as for
  execution snapshots. There is no re-wrap path yet: a controller given a key
  that cannot open every retained custody row refuses to start, so changing the
  key today means every person disconnects first (so each Connection finalizer
  can still revoke, where the provider supports it, while the old key opens
  the tokens) and reconnects afterwards.
  A re-wrap tool is a follow-up.
- Connector custody requires the persistent controller store; an ephemeral
  store loses every link on restart and people must reconnect.
- The controller becomes the only process holding third-party user tokens and
  must never log request headers for connector calls; the existing redaction
  helpers apply.
- Native `type: ai` Tasks gain a controller round trip per connector call.
- Chat reads through custom connector Tools go through a Task and are slower
  than an in-process call would be; the proxies' GitHub read built-ins are the
  exception described above.
- Existing per-Task GitHub token Secrets remain supported; the connector path is
  additive and does not change publication credentials, which stay outside the
  ACP process tree per the workspace credential rules.
- Proactive updates (a calendar change waking an agent) need provider webhooks
  landing as Gateway events and are out of scope here.
