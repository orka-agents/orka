# ADR 0037: Sign people in with GitHub

Date: 2026-10-21

## Status

Proposed. Adds GitHub as a way for people to sign in to Orka, beside OIDC. `orka login`
uses GitHub's device flow with the operator's GitHub App, and Orka issues its own
short-lived session token. The same login links the person's GitHub account as a
Connection, which provisions their agent host under
[ADR 0036](0036-agent-hosts-from-connections.md). Where people cannot reach Orka at all,
an operator invites them instead, and Orka runs the device flow itself. Builds on
[ADR 0033](0033-user-connectors.md). OIDC sign-in is unchanged.

## Context

- The day-0 path we want: an operator runs `helm install`, then each of N people runs one
  CLI command and starts working on GitHub from the Copilot desktop app.
- Today Orka's API accepts OIDC JWTs, ServiceAccount tokens and context tokens. Neither
  the CLI nor the dashboard has a sign-in flow: people paste a token from their identity
  provider.
- GitHub is not an OIDC provider for people; it issues opaque OAuth tokens. A GitHub App
  can enable the device flow, and with expiring user tokens it issues an eight-hour access
  token and a six-month refresh token. Both were confirmed live on 2026-10-21.
- GitHub's check-token API (`POST /applications/{client_id}/token`, Basic auth with the
  app's client ID and secret) confirms that a token was issued to that app and names its
  user. An invalid token gets 404.
- ADR 0036 already needs one GitHub App per installation, for the agent-host provider.

## Decision

### One GitHub App per installation

The operator's GitHub App serves sign-in, the agent-host `ConnectorProvider` and the
hosts' tokens. It has the device flow and expiring user tokens turned on, and its client
ID and secret are the provider's. Keep its permissions narrow: everything it may do, a
signed-in person's host may do.

### `orka login`

1. `GET /api/v1/auth/config` is unauthenticated. It returns the sign-in methods and the
   GitHub App's client ID, and nothing secret.
2. The CLI runs the device flow against GitHub itself. It shows the code and the URL,
   polls GitHub, and never holds the client secret.
3. It sends the GitHub access and refresh tokens to `POST /api/v1/auth/github` once.
   Orka then:
   - checks with the check-token API that the token was issued to its own app, so a
     token another app obtained cannot sign in;
   - redeems the refresh token once, requires the access token GitHub returns to be its
     own app's and the same numeric account's, and keeps only that rotated pair; GitHub
     invalidates the refresh token it redeemed;
   - takes the identity from GitHub: issuer `https://github.com`, subject the numeric
     user ID (logins can change), username the login;
   - requires active membership of a configured org
     (`GET /user/memberships/orgs/{org}`), or of a configured team
     (`GET /orgs/{org}/teams/{team_slug}/memberships/{username}`), with `state: active`.
     Org membership alone never satisfies a team allow-list. The app must be installed on
     those orgs with read access to members. Any error denies sign-in, and each later
     membership check applies the same rule. `{username}` is the login GitHub returns for
     the token at that check, never a stored one.
   - issues an Orka session token: random, shown once, stored only as a hash with its
     owner and a twelve-hour expiry, as AHP tokens are.
4. The CLI keeps the session token in the user's configuration directory, readable only
   by them. `orka logout` revokes it.

The API's authentication accepts session tokens beside OIDC. A session's identity is a
verified person, like an OIDC sign-in: it may link accounts and mint AHP tokens, and it
works in the sign-in namespace, which is the watch namespace. Session tokens are never
logged, and neither are GitHub tokens, device codes or GitHub's answers that carry them:
requests and errors are logged by route and status, never by body. GitHub's user code and
verification URL are shown only to the person signing in, or returned to the operator who
started an invite. The device code that polls for the token stays with whoever polls: the
CLI, or Orka for an invite.

This extends [ADR 0033](0033-user-connectors.md)'s owner rule, which admits only OIDC and
context-token subjects. A GitHub session also owns Connections, as issuer
`https://github.com` and subject the numeric user ID taken at sign-in, never the login.

### Renewal and offboarding

- Before a session expires, the CLI renews it at `POST /api/v1/auth/session/renew`. Orka
  refreshes the person's GitHub token from custody and checks membership again. A person
  no longer in an allowed org or team cannot renew; they sign in again, and are refused.
- The agent host's token refresh under ADR 0036, at most every eight hours, checks
  membership too. If it is gone, the controller handles it as a disconnect: it stops the
  host and keeps its volume.
- Removing someone from the GitHub org therefore ends their Orka access within one
  session lifetime, and ends their host within one token lifetime.

### Signing in also links GitHub

- `POST /api/v1/auth/github` records the GitHub tokens as the person's Connection to the
  agent-host provider, committed to sealed custody as a consent completion is. It is
  bound to the provider's current consent authority digest. ADR 0036 then provisions the
  person's host.
- Before it shows the device code, the CLI prints ADR 0036's disclosure: signing in
  starts your Copilot environment on Orka, and code running there can read this GitHub
  credential.
- `orka login --no-host` links the account for renewal and connector use, and records
  that the person declined a host. ADR 0036 provisions none for such a Connection.
  Signing in again without `--no-host` clears that, and ADR 0036 then provisions the
  host for the same Connection.
- A host takes only a token that expires, so a sign-in starts one only when GitHub
  confirmed its token's expiry through the refresh above. A sign-in without a refresh
  token links the account without a host.
- A link made another way, such as `orka connect`, with a token that never expires still
  gets a host, which fails with `GitHubTokenNeverExpires` and names the fix, rather than
  no host and no reason.

### Reaching Orka

- Only the `orka` CLI needs a path to Orka's API. The Copilot app reaches hosts through
  GitHub's relay, outbound from the cluster, and the device flow has no callback.
- The chart can render an Ingress for the API, given a host and a TLS Secret. With
  `ingressClassName: tailscale`, the Tailscale Kubernetes operator serves the API to the
  tailnet only. It provisions a MagicDNS name such as `orka.<tailnet>.ts.net` and a Let's
  Encrypt certificate, so a team on one tailnet needs no domain and exposes nothing to
  the internet. That address is what people pass to `orka login --server`.
- The CLI refuses a plain-HTTP `--server` other than localhost, because it sends session
  tokens.

### Invites, for people who cannot reach Orka

- `orka host invite <github-login>` asks Orka to start the device flow itself. The caller
  needs `create` on `agenthosts` in the watch namespace, which the API checks with a
  SubjectAccessReview. Orka returns the code, the verification URL and ADR 0036's
  disclosure, which the CLI prints for the operator to pass on before the person
  approves. Orka then polls GitHub outbound as the device flow requires: at the interval
  GitHub returns, waiting through `authorization_pending`, slowing down on `slow_down`,
  and stopping when the person approves, when GitHub answers `expired_token` or
  `access_denied`, or when the code's `expires_in` passes, after 15 minutes.
- On approval Orka makes the sign-in checks: its own app, membership of an allowed org or
  team. It also requires that the GitHub login, as GitHub returns it at approval, is the
  invited one; a login renamed in between is refused, and can be invited again. If
  someone else entered the code, Orka refuses, and deletes the tokens with GitHub's
  app-authenticated token API.
- Orka then records the Connection as signing in does, and ADR 0036 provisions the host.
  It issues no session token: the invited person needs only a browser and the Copilot app.
- An invite is single-use and lasts as long as its code. `orka host invite` waits for the
  outcome and prints it.
- Invited people cannot manage their host themselves. Keep idle suspension off for them,
  or an operator wakes their hosts. If they later sign in with `orka login`, they reach the
  same Connection, and so the same host.

### Configuration

The chart takes:
- the GitHub App's client ID, and a Secret holding its client secret;
- the allowed orgs and teams;
- whether signing in creates hosts;
- optionally, the API Ingress.

From these it renders the `ConnectorProvider` with the agent-host template, the Ingress,
and the controller flags.

### Not in this record

- Signing in to the dashboard with GitHub's web flow. The CLI comes first; the dashboard
  can accept a session token from `orka dashboard`.
- GitHub Enterprise Server: this record covers github.com only.
- Mapping GitHub teams to Orka roles beyond allowing or refusing sign-in.

## Acceptance

The ADR 0036 acceptance run passes with these people:
- two people who sign in with `orka login` over a tailnet address;
- one person invited with `orka host invite`, who has no CLI;
- a person outside the allowed org, who is refused;
- someone other than the invited person entering an invite's code, who is refused.

Removing one person from the org then ends their renewal and stops their host.

## Consequences

- Orka issues and verifies its own session tokens. They are a new credential: hashed at
  rest, short-lived, revocable and never logged.
- Sign-in depends on GitHub's availability and on the app staying installed on the
  allowed orgs.
- One app's permissions serve sign-in, any connector tools its provider declares, and
  hosts.
- Session tokens need a new SQLite table, and so a migration step.
- OIDC deployments are unaffected. Both sign-in methods can be configured at once.

### Alternatives considered

- **Dex, an OIDC broker with a GitHub connector, in front of Orka's OIDC.** It needs no
  sign-in code in Orka, but it is another component to deploy and run. People would still
  need an OIDC CLI login and a separate `orka connect`.
- **Raw GitHub tokens as Orka bearer tokens.** Orka would issue no token of its own, but
  every request would cost a GitHub call, and the CLI would hold a credential with
  repository access.
- **Checking membership with the app's installation token.** That needs the app's private
  key in Orka. The person's own token is enough.
- **A public address only.** It is simplest where one exists. A tailnet address or an
  invite serves teams that have none, or do not want one.
