---
title: Agent Host Protocol clients
description: "Install, expose and connect desktop Agent Host Protocol clients so they run sessions on Orka's built-in runtimes."
---

Orka serves the [Agent Host Protocol](https://microsoft.github.io/agent-host-protocol/specification/overview.html)
(AHP) at `/ahp`. A desktop AHP client connects to Orka as a remote agent host. It sees
the models of Orka's built-in runtimes, picks a repository, and chats. Each message runs as an
agent Task in an Orka Session, governed like any other Task: Orka's policy, credentials,
provider proxy and audit trail apply. The design is
[ADR 0034](https://github.com/orka-agents/orka/blob/main/docs/adr/0034-agent-host-protocol-api.md).

Setting it up takes three steps: an operator turns the endpoint on and exposes it over TLS,
and each person mints a connection token with the CLI and pastes the URL it prints into
their client.

## Install with Helm

The endpoint is off by default. It needs the `harness-v2` execution mode, which is the
chart's default, and a way for people to sign in: AHP connection tokens are minted by
people signed in with OIDC, never by ServiceAccounts or delegated context tokens.

```bash
helm upgrade --install orka orka/orka --namespace orka-system --create-namespace \
  --set controller.ahp.enabled=true \
  --set controller.oidc.issuer=https://login.example.com \
  --set controller.oidc.audience=orka \
  --set controller.oidc.allowedSubjects='*@example.com' \
  --set controller.oidc.namespace=orka-system \
  --wait --timeout 10m
```

Set `controller.oidc.namespace` to the namespace people's sessions run in, normally the
release namespace: Orka binds OIDC callers to it, and each AHP user's Tasks, Sessions and
Agents live there. The chart refuses `controller.ahp.enabled` without an OIDC issuer, or
outside `harness-v2`.

Optional values:

| Value | Default | Purpose |
| --- | --- | --- |
| `controller.ahp.tokenMaxTTL` | `2160h` | Longest lifetime a connection token may be minted with. Tokens default to `720h`. |
| `controller.ahp.turnTimeout` | `2h` | Timeout of each turn's Task. |
| `controller.ahp.gitCredentialSecret` | `""` | Secret in the user's namespace whose `token` key clones and publishes their github.com repositories. See [Repositories and credentials](#repositories-and-credentials). |
| `controller.ahp.workspaceClass` | `""` | ExecutionWorkspaceClass for session-scoped workspaces. Needs an installed workspace provider. |
| `controller.ahp.workspaceImage` | `""` | Digest-pinned image with `sh` and `git` that turns on [workspaces and terminals](#workspaces-and-terminals). |
| `controller.ahp.workspaceNamespace` | `orka-ahp-workspaces` | Namespace the chart creates for workspace Pods when `workspaceImage` is set. |

Without Helm, start the controller with the equivalent flags:

```text
--enable-ahp-api
--oidc-issuer=<issuer> --oidc-audience=<audience> --oidc-allowed-subjects=<patterns> --oidc-namespace=<namespace>
--ahp-git-credential-secret=<secret name>   # optional
--ahp-workspace-class=<class name>          # optional
```

See [Controller flags](../reference/configuration.md#controller-flags) for all of them.

## Expose /ahp over TLS

`/ahp` is served on the API port with the rest of the API, so the chart's controller
Service (`orka` for a release named `orka`, port `8080`, named `api`) already carries it,
WebSocket upgrades included. The chart creates no Ingress: put the TLS-terminating
Ingress, Gateway or load balancer you already use for the API in front of that Service.
Clients then connect to `wss://<API host>/ahp`.

Whatever sits in front must:

- **Terminate TLS.** The connection token travels in the URL (`?tkn=`), because desktop
  AHP clients connect by URL alone and send no `Authorization` header. Only `wss://`
  keeps it off the network in the clear; `orka ahp connect` refuses to print a `ws://`
  URL for any host but the local machine unless told `--insecure`. The hop from the proxy
  to the Service is plain HTTP inside the cluster.
- **Pass WebSocket upgrades.** Most ingress controllers and every Gateway API
  implementation do this for `GET` requests with `Upgrade: websocket`.
- **Allow long-lived, idle connections.** A client stays connected while a person works,
  often idle between messages, and turns can stream for up to `controller.ahp.turnTimeout`.
  Raise the proxy's read, send and idle timeouts well past a minute; an hour is a
  reasonable floor. A client that is cut off reconnects and replays what it missed, but
  short timeouts cause needless churn.
- **Not log query strings.** Orka logs only the path of `/ahp` requests, never its query.
  Proxies commonly log the full request line, which would record tokens. Turn access
  logging off for `/ahp`, or log without the query.
- **Not rewrite `Origin`.** Orka refuses browser origins (`http` and `https`) at `/ahp`
  so a web page cannot ride a pasted token; desktop clients send none or a non-web one.

For ingress-nginx, a dedicated Ingress for `/ahp` keeps these settings off the rest of the
API:

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: orka-ahp
  namespace: orka-system
  annotations:
    nginx.ingress.kubernetes.io/proxy-read-timeout: "3600"
    nginx.ingress.kubernetes.io/proxy-send-timeout: "3600"
    # The request line carries the connection token.
    nginx.ingress.kubernetes.io/enable-access-log: "false"
spec:
  ingressClassName: nginx
  tls:
    - hosts: [orka.example.com]
      secretName: orka-example-com-tls
  rules:
    - host: orka.example.com
      http:
        paths:
          - path: /ahp
            pathType: Exact
            backend:
              service:
                name: orka
                port:
                  name: api
```

With the Gateway API, route `/ahp` to the same Service from an HTTPS listener with an
`HTTPRoute`, and set the implementation's request and idle timeouts. Check the exposure
from outside the cluster: without a token the door answers `401`, which proves the request
reached Orka.

```bash
curl -s -o /dev/null -w '%{http_code}\n' https://orka.example.com/ahp   # 401
```

## Connect a desktop client

Each person does this once per device, with the [CLI](../reference/cli.md#desktop-ahp-clients)
signed in as themselves:

```bash
orka config set-server https://orka.example.com
orka config set-namespace orka-system
orka config set-token --file - < id-token.txt   # an OIDC ID token for you, from your identity provider

orka ahp connect --name work-laptop
```

`orka ahp connect` mints a token named after the device (the host name unless `--name`
says otherwise) and prints the URL to paste:

```text
wss://orka.example.com/ahp?tkn=orka_ahp_...
```

In the desktop client, add a remote agent host with that WebSocket URL. Pick the host, add
a repository as a project, choose a model, and send a message.

The URL is shown once and carries the token: anyone who has it acts as you on Orka until it
expires or you revoke it. Treat it like a password. Only the URL goes to standard output, so
`orka ahp connect | pbcopy` copies it without echoing it to the terminal. `-o json` prints it
with the token's ID and expiry instead.

The URL is built from the server the CLI talks to. When the CLI reaches Orka another way, for
example through a port-forward, pass the public address with `--url https://orka.example.com`.
To try AHP on a laptop without exposing anything, port-forward the API and use the local URL
it prints, which works only on that machine and only while the port-forward runs:

```bash
kubectl -n orka-system port-forward svc/orka 8080:8080 &
orka ahp connect --server http://localhost:8080   # ws://localhost:8080/ahp?tkn=...
```

## Manage tokens

AHP connection tokens are made for URLs that stay pasted in an app:

- **Lifetime.** A token lasts `720h` (30 days) unless `--ttl` asks for another lifetime,
  up to `controller.ahp.tokenMaxTTL` (`2160h`, 90 days, by default). When a token expires the client is
  disconnected; run `orka ahp connect` again and paste the new URL.
- **One per device.** Mint a token for each device, named after it, so a lost or retired
  device can be cut off alone.
- **Owned by one person.** A token acts as the person who minted it, in the namespace it was
  minted in, with no more authority than they had. Only its owner can list or revoke it.
- **Bound to sign-in.** At connect, and every 30 seconds while connected, Orka checks that the
  token's owner could still sign in: an OIDC-minted token needs the configured issuer and a
  subject `controller.oidc.allowedSubjects` still admits, and a token minted from a context
  token needs its issuer still configured. Removing someone from the allowlist cuts off
  their tokens, even ones minted before. Disabling a person only in the identity provider
  does not; revoke their tokens too.
- **Stored as hashes.** Orka keeps only a SHA-256 hash of each token and never logs one. A
  lost URL cannot be shown again: mint a new one.

List and revoke your tokens:

```bash
orka ahp tokens list           # active tokens: ID, name, namespace, created, expires
orka ahp tokens list --all     # also expired and revoked ones
orka ahp tokens revoke ahpt_0123456789abcdef
```

Revoking a token disconnects every client using it within seconds, and its URL stops
working. To rotate a device's token, connect again, paste the new URL, then revoke the old
token. The same operations are available over the API; see the
[API reference](../reference/api-reference.md#agent-host-protocol).

## What a person sees

- **Agents and models.** Orka offers one agent, shown as Orka, with the id `copilot`
  because some clients send only to an agent with that id. Each Agent in the person's
  namespace that uses a built-in runtime and names a model becomes one of its models,
  labelled with that runtime, and turns on that model run there: for example an Agent with
  `runtime.type: claude` and `model.name: claude-sonnet-5` is offered as
  `claude-sonnet-5 (Claude Code)`. Models the person's token could not run are not
  offered. A model several runtimes serve is offered once, on the first of Copilot CLI,
  Claude Code, Codex and OpenCode that serves it.
- **Projects.** The person adds a repository by its https URL. Each turn's Task clones
  it in its own workspace; with workspaces on, the person's workspace has a checkout too.
  Orka looks up a repository's default branch on github.com only, so a project on another
  host needs the session's `baseBranch` set by the client. Removing a project takes it off
  the list and leaves its checkout; a project that sessions still use cannot be removed
  until they are deleted.
- **Sessions.** A session runs in an Orka Session, which is bound to one runtime profile.
  When the person picks another model, the session moves to a new Orka Session that
  starts with the conversation so far as context. A branch a turn already published
  belongs to the old Orka Session, so the new one continues on `<branch>-<n>`, based on
  it, and the chat says so.
- **Turns.** Text, reasoning and tool calls stream while the Task runs, with a running
  tool's output as it arrives, the agent's plan as one tool call that updates in place
  (`extensions/getPlan` returns it too), and token usage as the runtime reports it.
  Cancelling the turn cancels its Task. A failed turn's error carries the runtime's
  reason.
- **Messages during a turn.** Orka's runtimes take no input mid-turn. A message sent
  while a turn runs is queued and becomes a later turn, in the client's order; a
  steering message is queued first and shown as queued.
- **Changes.** Files the agent edits appear in the session's changes as it reports them.
  Once a turn's changes land, the changes become exactly what the session's branch
  changed since it forked from the base branch, including files changed with shell
  commands. Large and binary files show a note instead of their contents.
- **Approvals.** With `manual` approvals, Orka holds every permission request its policy
  would allow and asks the person; policy still refuses what it refuses. With
  `allow-all`, policy decides alone. `assisted` behaves like `manual`. A client's
  approve-all toggle (`extensions/setSessionApproveAll`) selects `allow-all` or `manual`.
  A changed mode applies from the session's next turn.
- **New-session form.** `resolveSessionConfig` returns the approval mode and branches a
  new session would take, with the project's default branch filled in.

## Repositories and credentials

The agent never holds Git credentials. With `controller.ahp.gitCredentialSecret`
(`--ahp-git-credential-secret`), Orka looks up that Secret in each person's namespace and
uses its `token` key to clone github.com repositories and publish the agent's changes.
Orka never sends it to another host: a project on another host is cloned anonymously and
its turns cannot publish. Each turn
that changes files publishes them to the session's branch, `orka/session-<uid>` unless the
client named one, and the next turn starts from it. Without the Secret, sessions clone
public repositories read-only, and a turn that changes files fails with
`ReadOnlyWorkspaceModified`.

With `controller.ahp.workspaceClass` (`--ahp-workspace-class`), turns use session-scoped
execution workspaces, so a session's files stay warm between turns. This needs an
installed workspace provider.

## Workspaces and terminals

Desktop AHP clients run their own git commands on the host, through terminals, before they
create a session or send a message. Give them a place to do that with
`controller.ahp.workspaceImage=<image with sh and git>@sha256:<digest>`. The chart then
creates `controller.ahp.workspaceNamespace` (`orka-ahp-workspaces`) with the restricted Pod
Security profile, the controller's Role in it, and a NetworkPolicy that admits no inbound
traffic and allows only DNS and TCP 443. Without Helm, apply the same objects and start the
controller with `--ahp-workspace-image`:

```bash
kubectl apply -k config/ahp-workspaces
```

Each AHP user then gets one workspace Pod in that namespace (`--ahp-workspace-namespace`):

- each project is cloned at its path, and each session has a git worktree on its branch;
- terminals are shells in that Pod;
- clients read, list and watch files under `/workspace` through `resourceRead`,
  `resourceList` and `createResourceWatch`; a watch polls the workspace and lasts as long
  as its connection keeps it, and reads never start a workspace;
- agents never run there: each turn publishes to the session's branch, and the session's
  worktree fast-forwards to it;
- the Pod reaches only DNS and HTTPS.

### Private repositories

A workspace reads public repositories anonymously, on any host. Private repositories work
on github.com only. To let each person's workspace read their private repositories, link
GitHub through [Connectors](../guides/connectors.md) and start the
controller with `--ahp-workspace-connector-provider=<ConnectorProvider name>`
(`--connectors-enabled` is required). The named provider, in each person's namespace, must
issue github.com tokens. Orka then gives the person's workspace their own token from their
link, while the link is Ready, its provider is accepted and unchanged since consent:

- it arrives on standard input and sits in a file on the Pod's private `/tmp` that only
  the person's user can read;
- git hands it to `https://github.com` only, through a credential helper;
- it is replaced about every 20 minutes, before it would expire, while the person uses the
  workspace or types in one of its terminals, and removed when the person's link no longer
  serves it.

The token is the person's own, so their shell in the workspace can read it, as it could
on their own machine. Agents never run in the workspace.

### Lifetime

The Pod's files are ephemeral: Orka rebuilds the checkouts when it starts again. It
removes the Pod with the person's last session unless a terminal is open, and, with
`--ahp-workspace-idle-timeout` (default `4h`, `0` turns it off), after that long without
use. An idle Pod is removed only while no shell or turn of its person runs and nothing in
it exists only there: no uncommitted or untracked files, no commits that any local ref
or worktree reaches and no remote-tracking branch has (stashes and tags included), no
files git was told not to check, no submodules and no files outside the checkouts. When
git warns or a check fails, the Pod stays. Shell history, files git ignores inside a
checkout and commits only the reflog keeps are not kept.

For the same reason the chart does not keep the namespace: unsetting `workspaceImage` or
uninstalling the release deletes it with its Pods. A namespace created earlier with
`config/ahp-workspaces` is not Helm's; delete it before turning workspaces on with Helm.

## Not offered yet

- Tools the client offers to agents.
- Questions from the agent to the person: the built-in runtimes run without them.
- Changesets per turn, of uncommitted files only, or of the branch alone; the session's
  changeset covers the whole branch.
- Agent hosts as a session backend.
