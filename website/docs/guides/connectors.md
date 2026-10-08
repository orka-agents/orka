---
slug: /connectors
description: "Letting people link their own GitHub account so agents act as them, with approval on every write."
---

# Connectors: act as the person, not the namespace

A connector lets a person link one of their own accounts to Orka once, and
then lets the agents they start act through that account. The first provider
is GitHub. This guide sets it up end to end: the OAuth App, the
`ConnectorProvider`, the link, and a Task whose GitHub tools run as the person
who created it.

The controller is the only process that ever holds the linked token. Runtime
Pods, worker Pods, Task specs, status, events, and logs never carry it.

## How it fits together

- **`ConnectorProvider`** is the operator-owned catalog entry: the OAuth
  client, the scopes for `readOnly` and `readWrite` links, and the tools the
  provider offers. It lives in the controller's watched namespace.
- **`Connection`** is one person's link to one provider. The API server
  creates it from the caller's verified sign-in identity; nobody can create
  one for somebody else, and a Connection is visible only to its owner.
- **Tools** come in two shapes. `Builtin` tools are Orka's own GitHub tools,
  which run under the link. `HTTP` tools are curated request definitions the
  controller executes with the person's bearer token; they attach to Tools
  through a connection-mode `OutboundAccessPolicy` (see
  [Outbound access](../concepts/outbound-access.md#linked-account-credentials-connection-mode)).

When a Task is dispatched, the controller looks up the requester's Ready
Connection for each provider its tools need and freezes that exact link (its
UID, generation, and consent grant) into the Task's execution snapshot. A
link made, re-linked, or removed after dispatch never changes what a running
Task can do.

## Prerequisites

- A sign-in that yields an issuer and subject: OIDC (`--oidc-issuer`,
  `--oidc-audience`) or a context-token profile. ServiceAccount bearer tokens
  cannot link accounts.
- Task provenance admission (`--task-provenance-admission-enabled` or the
  external webhook). Connector use trusts `spec.requestedBy` only when the API
  server provably stamped it.
- `--connectors-enabled` and `--connector-callback-base-url`, the public
  https origin the OAuth provider redirects back to. See
  [Configuration](../reference/configuration.md).
- The persistent controller store. Sealed credentials live there.

## 1. Register a GitHub OAuth App

In GitHub, create an OAuth App (Settings > Developer settings > OAuth Apps)
with the authorization callback URL set to exactly:

```text
<connector-callback-base-url>/api/v1/connections/callback
```

Note the client ID. Store the client secret in the controller's watched
namespace, outside git:

```bash
kubectl -n orka-system create secret generic github-connector-oauth \
  --from-literal=clientSecret='<github-oauth-app-client-secret>'
```

## 2. Declare the provider

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: ConnectorProvider
metadata:
  name: github
  namespace: orka-system
spec:
  displayName: GitHub
  oauth:
    authorizeURL: https://github.com/login/oauth/authorize
    tokenURL: https://github.com/login/oauth/access_token
    clientID: Iv1.replace-with-your-client-id
    clientSecretRef:
      name: github-connector-oauth
      key: clientSecret
    scopes:
      read:
        - read:user
      write:
        - repo
  tools:
    - name: list_pull_requests
      class: read
      source: Builtin
    - name: get_issue
      class: read
      source: Builtin
    - name: create_pull_request
      class: write
      source: Builtin
    - name: comment_on_issue
      class: write
      source: Builtin
```

`kubectl get connectorprovider github` shows `Accepted` and `ResolvedRefs`.
The controller rejects a provider whose built-in declarations do not match
the catalog below, so a typo or a wrong class never becomes a live provider.
The declared tools are part of what a person consents to: adding a built-in
(or an HTTP tool) later asks every linked person for consent again before
the new tool can use their account.

GitHub OAuth Apps have no read-only scope for private repositories: reading
them needs `repo`, which also grants write. Orka hides write tools from a
`readOnly` Connection regardless of what the token could do, but decide
deliberately whether `repo` belongs in `read`.

### Built-in GitHub tools

Only the tools below can be declared with `source: Builtin`, and each has a
fixed class. A read tool only reads through the link; a write tool mutates
the forge, is hidden from `readOnly` Connections, and always asks for
approval before it runs.

| Tool | Class | What it does |
| --- | --- | --- |
| `list_pull_requests` | read | List pull requests in the Task's repository. |
| `list_issues` | read | List issues. |
| `get_issue` | read | Read one issue. |
| `review_pull_request` | read | Fetch a pull request's diff and metadata. |
| `check_pull_request_ci` | read | Report CI status for a pull request. |
| `comment_on_issue` | write | Comment on an issue or pull request. |
| `create_pull_request` | write | Open a pull request. |
| `post_review_comment` | write | Post a review comment. |

`check_pr_review_marker` is not in the catalog: its marker secrets and
trusted author come from the worker Task's environment, which the
controller does not hold. Declaring any other built-in, or declaring one of
these with the other class, is rejected: a tool that ignored the credential would silently run
under something else, and a write tool declared as read would skip the
approval and `readOnly` rules its class carries. The built-ins call
`https://api.github.com`, so only a provider whose OAuth endpoints are on
`github.com` may declare them; a GitHub Enterprise Server or any other
issuer is rejected, because its token would be sent to the wrong server.
Curated `HTTP` tools remain available for such providers.

Under a linked account the repository scope comes from `spec.workspace` of
the current Task. A child Task it controls (a coordinator opening the pull
request for its coder's work) may be named too, but only for repositories
the current Task itself holds. Any other `task_name` is refused, a
transaction's repository context never widens the scope, and `repo_url` must
fall inside it. A `review_pull_request` or `get_issue` result that would
exceed the broker's result limit is cut (file patches or the oldest comments
first, then the diff or body) and marked `truncated` instead of failing; a
GitHub page larger than 1 MiB is refused with a request for a smaller
`per_page`. Each built-in carries a fixed time bound (two minutes, eleven
for `check_pull_request_ci`, whose `wait_timeout` is clamped to ten minutes
and whose `poll_interval` is never shorter than five seconds), and the
controller refreshes a token that would expire within that bound before
the call starts.

The full example, with an Agent that uses these tools, is in
[`examples/github-connector/`](https://github.com/orka-agents/orka/tree/main/examples/github-connector).

## 3. Link an account

Each person links once, signed in as themselves (an OIDC or context-token
identity; a ServiceAccount token has no accounts to link). Three ways:

**Dashboard.** Open **Settings › Connectors** (`/settings/connectors`). Every
provider shows its read and write tools and whether you are linked. *Connect
(read only)* or *Connect with writes* sends you to the provider's consent
page; the callback brings you back and the page finishes the link. From the
same page you can allow or limit writes, reconnect a link that lost its
consent, and disconnect.

**CLI.**

```bash
orka connect github --mode readWrite       # opens the consent page, waits until Ready
orka connection list                       # your linked accounts
orka connection get github-<digest>
orka connection delete github-<digest>     # disconnect and revoke
orka connection providers                  # what an operator has made available
```

`orka connect` needs a personal token: your OIDC token with `--token` (or
the token `orka config` stores), or a context token with `--txn-token` or
`--txn-token-file` (it travels in the `Txn-Token` header; `--token` sends a
bearer, which carries a context token only when the server opts in). It
explains a `403`, whether from a ServiceAccount token or from a context
token that lacks the connector scope. `--no-open` prints the consent URL instead of opening
a browser, `--no-wait` returns as soon as consent has started. The provider
sends the browser back to the dashboard, which finishes the link when it is
signed in as you; when it is not, copy the value after `#completion=` from
the address bar and run `orka connection complete <name> --namespace
<namespace> --completion <value>` (the namespace the consent was started
in; `orka connect` prints the exact command) so the CLI finishes it with
your token instead.

**API.**

```bash
curl -sS -X POST "$ORKA_API_URL/api/v1/connections" \
  -H "Authorization: Bearer $ORKA_TOKEN" -H 'Content-Type: application/json' \
  -d '{"provider":"github","mode":"readWrite"}'
```

The response carries the Connection and an `authorizeURL`. Open it in a
browser, approve the GitHub consent screen, and the callback commits the
tokens under the controller key. `GET /api/v1/connections` then shows the
Connection as `Ready` with its granted scopes and no token material.

`mode` is `readOnly` (only read scopes; write tools hidden) or `readWrite`.
Widening the mode later (`PUT /api/v1/connections/<name>`) asks for consent
again with the extra scopes; narrowing takes effect immediately. Disconnect
with `DELETE`: the controller deletes the sealed material, first revoking
the committed tokens, against the client they were issued to, at the
provider's `revocationURL` when it names one. GitHub offers no standard
revocation endpoint, so the GitHub example sets none: a GitHub disconnect
deletes the tokens Orka holds, and the person revokes the authorization
itself in GitHub under Settings → Applications.

The [API reference](../reference/api-reference.md#connector-endpoints) lists
every route and the scopes a context token needs for them.

## 4. Use it from a Task

Create the Task through the API, so it carries the person's verified
identity. `kubectl apply` of a Task has no requester and gets no link.

```bash
orka task create "List the open pull requests and summarize them" \
  --namespace orka-system \
  --type agent \
  --agent github-as-me \
  --workspace-intent read \
  --git-repo https://github.com/example/project
```

With the Agent from the example (an ACP runtime whose allowed tools include
the GitHub built-ins), the controller:

1. Finds the requester's Ready GitHub Connection and freezes it.
2. Offers `list_pull_requests` and the other read tools to the agent. Write
   tools are offered only for a `readWrite` link, only on a runtime that
   can pause for brokered approval (an external AgentKit or Foundry
   registration, see below), and always in the approval-required set.
3. Executes each call in its MCP broker with the person's token, refreshing
   it first if it is about to expire.
4. Pauses a write call for approval and, once approved, executes exactly the
   stored call.

The Task's workspace still scopes which repository the tools may touch; the
link changes whose credential is used, not where.

### Ask what is linked: `list_connections`

Agents and chat have a read-only `list_connections` tool. It returns the
signed-in person's (or the Task's verified requester's) linked accounts with
their mode and readiness, the providers they could still link, and the
settings path, never any token. A link whose provider was removed is still
listed, marked `providerMissing` and never ready, until the person
disconnects it. An agent that needs an account the person
has not linked should say so and point them at **Settings › Connectors**
rather than try another credential. The tool is available to chat, to the
compatibility proxies' coordinator mode, and to ACP runtimes through the
broker; a Task without a verified requester gets an explicit "no identity"
result. The tool exists only while `--connectors-enabled` is set: without
it chat, the proxies, and the broker neither offer nor run it. Under
enforced context-token authorization it follows the same boundary as the
connector routes: a delegated token without the connector-read scope
(`orka:connectors:read` by default) is not offered the tool and is refused
if it calls it anyway, and a Task created by such a token is refused the
same way through the broker.

### Chat and the compatibility proxies

The GitHub read tools the compatibility proxies offer in coordinator mode
(for example `check_pull_request_ci`) run as the signed-in person when they
hold a Ready link to a provider that declares the tool (the dashboard chat
offers no GitHub tools, only `list_connections`):
the Connection is read live at call time, since there is no dispatch to
freeze it. These surfaces execute tools directly, with no approval gate,
so a linked write tool such as `create_pull_request` is refused there once
the person has a link; linked writes run only from a Task, where the write
waits for approval. A linked call may name a Task in `task_name` only
when this conversation's tools created that Task for the same person
(the API stamps it with their identity), so the person's token is scoped
by a repository they chose themselves; any other `task_name` is refused,
because there is no current Task whose repository scope could bound it.
Without a link those tools keep the Task-Secret path they always had; a
link that exists but cannot be used (pending, expired, revoked, or being
deleted) fails the call rather than falling back. Under enforced
context-token authorization, a delegated token without the connector-read
scope (`orka:connectors:read` by default) uses no linked account on these
surfaces: its tools keep the Task-Secret path, as for a person with no
link. Audit mode uses the link and records the missing scope.

### What runs where

| Path | GitHub credential |
| --- | --- |
| Built-in ACP runtimes (`codex`, `claude`, `copilot`, `opencode`) | The requester's Connection only. The read tools are offered when the provider declares them and the requester holds a Ready link; otherwise they are not offered, and a call without a frozen link is refused. These runtimes cannot ask for brokered approval, so the write tools are never offered on them. |
| External v2 `AgentRuntime` registrations (AgentKit, Foundry) | The requester's Connection only, and only when the link leaves the registered policy untouched: every declared GitHub built-in in the registered allowlist needs a Ready link (`readWrite` for write tools), and every write tool must already be in the registered `approvalRequiredTools`. Otherwise the Task is refused with a permanent reason rather than the profile narrowed per person. This is the path where linked write tools with approval run. |
| Native `type: ai` worker Pods | The Task's own credential Secrets (`readCredentialRef`, `forgeCredentialRef`), as before. Worker Pods never receive linked tokens. |
| Custom `HTTP` tools behind a connection-mode `OutboundAccessPolicy` | The requester's Connection only, in the controller, for both ACP and native Tasks. |

Existing per-Task GitHub Secrets keep working everywhere they did. Linking is
additive.

## What fails closed

- No verified requester, no Ready link at dispatch, a link re-consented after
  dispatch, or a provider whose OAuth client changed since consent: the call
  fails. Nothing falls back to Task Secrets, environment variables, or
  another person's Connection.
- A `readOnly` link never runs a write tool, even if the token could.
- Two accepted providers declaring the same built-in tool is a configuration
  error; the Task is refused rather than one provider chosen.
- A coordination child inherits its parent's requester, so its workspace may
  only name repositories its parent chain holds; a child delegated to another
  repository is refused rather than given the person's account there.
- Curated `HTTP` connector tools are not available on external v2
  `AgentRuntime` registrations, and harness v1 Tasks receive no connector
  tools at all.

## Troubleshooting

- **Provider shows `Accepted=False`**: the condition message names the
  problem, usually a built-in declared with the wrong class or a missing
  client secret.
- **`Ready=False` with `ConsentRequired`**: the provider or the mode now
  needs scopes the last consent did not grant. `POST
  /api/v1/connections/<name>/authorize` starts consent again.
- **A link in state `Ready` is reported as not ready** (the dashboard shows
  *Reconnect needed*): the provider's client, endpoints, tools, or required
  scopes changed since the person consented, and credential resolution
  refuses the old grant before the Connection's conditions catch up.
  Reconnect the link. *Duplicate links* means the person holds more than one
  link to the provider; every one is unusable until the extras are
  disconnected.
- **The agent does not see the GitHub tools**: check that the Task was
  created through the API by a signed-in person, that the Connection is
  `Ready`, and that the Agent's allowed tools include them. Write tools also
  need `mode: readWrite`.
- **`requires the requester's linked account`** in a tool result: the Task
  was dispatched without a Ready link. Link first, then create a new Task.
