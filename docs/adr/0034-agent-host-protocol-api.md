# ADR 0034: Serve the Agent Host Protocol as an Orka client API

Date: 2026-10-17

## Status

Proposed. Adds an [Agent Host Protocol](https://microsoft.github.io/agent-host-protocol/specification/overview.html)
(AHP) endpoint to the Orka API server, behind `--enable-ahp-api` (default off), with
session backends. The first backend runs each turn as an Orka Task on Orka's built-in
runtimes over ACP. A backend that re-hosts sessions from per-user agent hosts
(ADR 0035, proposed separately) is left for later. Lands in stages; the core
capabilities listed under **Prerequisites** come first.

## Context

AHP defines how clients share a synchronized view of agent sessions served by a
standalone host. The host owns sessions, worktrees, tools and approvals; the client is a
UI. AHP clients include the GitHub Copilot desktop app and VS Code's agent sessions UI.
AHP sits above ACP: a host speaks AHP to its clients and may speak ACP to the agents
behind it.

Orka already serves two client protocols, the OpenAI- and Anthropic-compatible APIs,
which map standard requests onto Orka execution. Gateways (`orka.gateway.v1`) carry
text-only envelopes for messaging platforms and cannot carry tool rows, diffs,
approvals or cancellation. A separate bridge that maps AHP onto a gateway stubs most of
AHP for that reason.

What a host must get right, learned from the reference host in VS Code, the public
AHP-over-ACP hosts and the AHP issue tracker:

- There is no server SDK. Microsoft's Go module
  (`github.com/microsoft/agent-host-protocol/clients/go`, MIT) provides wire types,
  pure reducers and a WebSocket transport. The per-action version table and the table of
  client-dispatchable actions exist only in the TypeScript sources.
- The protocol has one `serverSeq` per host and no user concept. Hosts that keep that
  sequence in memory break reconnect after a restart; an empty replay to an unknown client
  left clients showing stale state. Anyone who learns a `clientId` can hijack its
  reconnect.
- Approval modes are not in the spec; each host defines its own session-config vocabulary,
  and clients differ in which `_meta` keys and URI shapes they expect.
- Transport gating is outside the protocol and should happen at the handshake. Clients
  pass a connection token as `?tkn=`; some can send headers.
- ACP-to-AHP mappings converge on a tool-call state machine, the plan as one synthetic
  tool row, diffs as file edits whose contents the host serves, never defaulting a
  permission to an "always" option, and settling pending permissions before cancelling.

Orka today runs each agent turn as a Task with policy-decided approvals. Task and
Session event streams exist, but they are read from a journal every few seconds,
assistant text is persisted only after redaction at turn end, and ACP diff and terminal
content is dropped. Session-scoped workspace reuse exists, fenced by the Session's UID
and lineage. There is no cancel request short of deleting the Task, no human prompt for
built-in runtimes' permission requests, and no read access to a live workspace.

## Decision

### An AHP endpoint in the API server

`/ahp` accepts AHP over WebSocket (text frames, one JSON-RPC message each) next to the
OpenAI- and Anthropic-compatible groups. It is off by default.

### AHP connection tokens identify users and carry no more authority than they had

- Minting an AHP connection token requires a person's own OIDC sign-in. ServiceAccount
  callers and delegated context tokens cannot mint one: a connection could not yet keep
  to a context token's scopes and transaction constraints everywhere it reaches, from
  workspace shells to session reads, and built-in runtimes do not run Tasks with
  transaction delegation. Delegated connections are left for a later decision.
- Tokens are stored hashed, compared in constant time, revocable and scoped to `/ahp`.
- The token arrives as `?tkn=` or `Authorization: Bearer`. A missing, wrong or revoked
  token is refused with 401 before the WebSocket upgrade. Browser origins are refused.
  URLs and `authenticate` frames are redacted from every log.
- Every AHP command and client action is authorized as the Orka operation it causes
  (for example creating a Task or answering an approval) as the token's owner;
  being allowed onto `/ahp` authorizes nothing else. `requestedBy` is stamped only after
  that check, as provenance.
- Revocation or expiry closes every connection using the token and stops delivery,
  including queued output; passive subscribers are not exempt.
- A token outlives the sign-in that minted it, so its owner is checked against the
  controller's current sign-in policy at connect and on every recheck: a token needs the
  configured OIDC issuer and a subject the allowlist still admits.
- A pasted host URL is long-lived. Tokens default to 30 days, are capped by
  `--ahp-token-max-ttl` (90 days by default), carry a description naming the device,
  and are listed and revoked only by their owner (`orka ahp tokens`).
- `initialize`, `subscribe` and `reconnect` authorize each channel. Replay and snapshots
  include only what the user may still see; anything else is returned in `missing`.

### The host core

- It uses the Go module's types and reducers; Orka ports the two missing tables.
- Each user has their own sequence, replay journal, snapshots and session catalog, stored
  durably in Orka's store. Replay is bounded by count and bytes; a gap beyond it gets
  fresh snapshots.
- A client record binds a `clientId` to the user who first used it and stores the
  negotiated protocol version and dialect. `reconnect` restores them before delivering
  anything; with no record, or a record for another user, it is rejected and the client
  must `initialize` again. It never gets an empty replay.
- Every client action is validated before it is applied; a rejected action is echoed back
  with `rejectionReason`.
- Each connection negotiates its version independently (0.9.0 and 1.x), and the host never
  sends an action newer than the negotiated version, in live delivery, replay or
  snapshots.
- A dialect layer per connection translates client-specific URI shapes, `_meta` keys and
  session-config vocabulary. The Copilot desktop app comes first; it uses the
  `approvalMode` vocabulary (`manual`, `assisted`, `allow-all`) and project commands.

### Session backends

A session router chooses a backend when a session is created. The first backend is ACP;
an AHP backend for agent hosts is a later decision.

### The ACP backend

- Agents: one AHP agent runs Orka's built-in runtimes (copilot, claude, codex, opencode).
  Each Agent in the user's namespace on a built-in runtime with a model is one of its
  models, labelled with that runtime and run there, offered only when the user's token
  would let them run it. A model several runtimes serve is offered once, on the first in
  that order. The agent's id is `copilot`, because some clients send only to an agent with
  that id; one agent instead of one per runtime keeps clients that show every agent from
  listing each model twice. It advertises no protected resource: Orka never asks for the
  user's own credentials.
- An AHP session runs in an Orka Session. An AHP turn is a Task with `sessionRef`, the
  chosen Agent and the project's repository in `spec.workspace`, created and authorized as
  the person. An Orka Session is bound to one runtime profile, so when the person picks
  another model the AHP session moves to a new Orka Session, which starts with the
  conversation so far as context. A branch a turn already published belongs to the old
  Orka Session's publication claim, so the new one continues on a new branch based on it.
  When an operator names an execution workspace class, turns use session-scoped workspaces
  so files stay warm; otherwise each write turn publishes and the next turn starts from it.
- Messages sent while a turn runs wait: the runtimes take no input mid-turn, so each is
  queued and becomes a later turn. A steering message is queued first and shown as
  queued, so clients do not also show it as input to the running turn.
- Projects are repositories the user adds; Orka never clones them on the host, because
  each turn's Task clones its own workspace. The agent never holds Git credentials. The
  operator names one credential Secret that is looked up in each user's namespace and sent
  only to github.com, since users name project hosts freely; without it, and for projects
  on other hosts, sessions clone public repositories read-only. Write turns publish to an Orka-owned
  session branch through the Publisher.
- Mapping: text and reasoning chunks stream as deltas; tool calls follow the AHP state
  machine and are named as soon as they start, with their raw input, and a running call's
  output streams as it arrives; diffs are file edits the agent reports, listed in the
  session's changeset, whose before and after contents the host serves; usage is reported
  as the runtime sends it; the agent's plan is one tool call that updates in place. Once a
  turn's changes land and the session's worktree follows them, the changeset becomes
  exactly what the session's branch changed since it forked from the base branch. A turn
  that ends clears the session's activity.
- Approvals: a session's `approvalMode` is `manual` or `allow-all` (`assisted` is treated
  as `manual`; Orka has no model-reviewed approvals). In `manual`, each turn's Task holds
  every permission request policy would allow for its requester, shown as an AHP
  confirmation; policy answers what it rejects. In `allow-all`, policy decides. Only the
  session owner may answer. Orka never chooses an "always" option on the user's behalf.
  A `manual` turn's built-in runtime is launched to ask before every shell command and
  file change; an `allow-all` turn's runtime runs them unasked. An Orka Session's runtime
  session is bound to that launch mode, so the first turn after the mode changes starts a
  new runtime session from the Session transcript instead of reusing one launched the
  other way.
- Cancellation answers pending permissions as cancelled, cancels the Task, and kills the
  agent process if the cancel does not settle in time, so late updates cannot reach the
  next turn.
- Live text streams to the session owner with exact-value redaction of the credentials
  Orka injected, matched across chunk boundaries before any fan-out. The journal and
  transcript keep only fully redacted text.

### Host reach

The TCP channel is not advertised. Client-provided tools are never offered to agents.
`resourceRead` serves the contents a session's changeset lists and, on the root channel,
files under `/workspace` in the user's own running workspace; `resourceList` lists its
directories, and `createResourceWatch` polls it for changes for as long as the connection
keeps the watch. Nothing on the server is readable through them, and reads never start a
workspace. `completions` answers with an empty list. The host pings open connections.

### Workspaces and terminals

Clients treat the host as a machine with git checkouts: they run their own git commands
in a project's checkout and a session's worktree through AHP terminals, and a client may
refuse to create a session or send a message until those commands work. When an operator
names a workspace image, Orka gives each AHP user one workspace Pod in a dedicated,
restricted namespace:

- the user's projects are cloned at their project paths, anonymously unless the operator
  names a ConnectorProvider whose github.com links serve the user's own reads;
- each session has a git worktree at its working directory, on the session's branch;
- AHP terminals are shells in the Pod, streamed over the terminal channel.

The Pod holds no Orka credential, accepts no inbound traffic and reaches only DNS and
HTTPS. When an operator opts in, it holds the user's own github.com token from their
linked account, refreshed while the link serves it, in a file only the user's shell can
read, which git hands to github.com only; the user's shell can read it as it could on
their own machine. Agents never run there: each turn still runs as a Task in its own
workspace, publishes to the session's branch, and the worktree fast-forwards to what it
published. A project's branch belongs to one session. The workspace is ephemeral and is
rebuilt from the user's projects and sessions when it starts again; it is removed with the
user's last session unless a terminal still runs, and after an idle timeout only while no
shell or turn runs and it holds nothing unpushed. Terminals are usable only by connections
allowed to create Tasks, and disposing one stops everything its shell started.

### Client dialects

Clients extend the protocol through `_meta` keys and host-specific commands. Orka serves
the Copilot dialect for every connection, because not every client identifies itself at
`initialize`: the `copilot.projectManagement` root extension and the
`extensions/listProjects`, `extensions/cloneProject`, `extensions/removeProject`,
`extensions/getPlan` and `extensions/setSessionApproveAll` commands. Unknown extension
commands are answered with method-not-found.

### Prerequisites in Orka core

Each lands separately and is useful without AHP:

1. Extend the existing Task event pipeline to push text and reasoning deltas, tool calls
   with their name, input and file edits (relative to the workspace), and plan updates,
   redacted as above. A runtime sends the new fields only to a controller that asks for
   them; Session streams keep durable events only.
2. A cancel request for a running Task.
3. Human decisions for built-in runtimes' permission requests.
4. Use the existing session-scoped workspace reuse, with its UID, lineage and fencing,
   for interactive sessions; no new Session or workspace identity.
5. Read access to a session's changed files. The first release serves the before and
   after contents the agent reports with each edit, rather than reading a workspace.
6. AHP connection tokens and identity mapping.

## Consequences

- Orka joins the conversation: every turn is a governed Task with Orka-held Git
  credentials, models through the provider proxy, approvals and an audit trail.
- The endpoint is a large, stateful surface. Client dialects drift with client releases
  and must be tested against the real clients, not only the spec.
- The journal stays in the single API server replica's store until Orka's storage work
  allows more; reconnects then need sticky routing.
- Copilot app users get Orka's runtimes, including Copilot's CLI over ACP, not the
  native `copilotd` experience; that remains ADR 0035's agent hosts.
- The gateway bridge for the Copilot app is no longer needed once this serves chat.

### Alternatives considered

- **Extend the gateway bridge.** The gateway protocol cannot carry what AHP needs.
- **Register Orka itself with GitHub's relay.** That relay's protocol is not public.
- **Keep the journal in memory**, as the reference host does. Breaks reconnect on restart
  and cannot be filtered per user.
- **Depend on a third-party AHP server library.** None is official or mature; the AHP
  repository tracks the request for one.
