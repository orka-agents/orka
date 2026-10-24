# ADR 0035: Run per-user agent hosts as Orka-owned Pods, relay first

Date: 2026-10-17

## Status

Proposed. Adds `AgentHostTemplate` and `AgentHost`, which run one long-lived, single-user
[Agent Host Protocol](https://microsoft.github.io/agent-host-protocol/specification/overview.html)
(AHP) daemon per user on Orka-owned Pods, behind `--enable-agent-hosts` (default off).
Execution-workspace backends (Agent Sandbox, Substrate) for suspension with less cold
start and denser packing are left for a later record that reuses the host workload
contract below. Serving AHP from Orka itself is a separate record (0034); this record
does not depend on it.

## Context

An agent host is a long-lived daemon that serves AHP to desktop and editor clients. The
host owns sessions, worktrees, tools and approvals; the client is a UI. `copilotd` is one
such host: MIT licensed, published as a public image (`ghcr.io/github/copilotd`), serving
AHP to clients and driving the Copilot CLI through `copilot-sdk`. Running it in a cluster
gives a user remote compute and sessions that outlive their laptop.

Relevant `copilotd` facts, from its public `--help` and live tests on kind (copilotd 0.9.6):

- It serves exactly one GitHub user per process; the first sign-in wins.
- Its session catalog, projects, settings and agent state live on disk. A replacement Pod
  on the same volume resumed a session; the follow-up turn took 6.6 s on a warm cache.
- `--standalone` serves AHP on a loopback listener gated by a connection token passed on
  a private stream (`--connection-token-stream`). Clients reach it by port-forward.
- `--relay` registers the host as an environment on the user's GitHub account and
  accepts client connections through GitHub's relay, outbound only. The client lists it
  with no URL or token to copy. With a stable `--compute-id`, a replacement Pod
  re-registered the same environment 11 s after deletion, the client reconnected and the
  session resumed. The initial registration credential must come from a flag or the
  environment; `--mission-control-token-file` only rotates it.
- `--idle-status-file` writes JSON with `idle`, `idleSince`, `activeSessions`,
  `busySessions`, `backgroundWork` and `connectedClients` for a co-located controller.
- Sealed `authenticate` is required on relay lanes and optional on the direct listener.
- The agent runtime holds the user's GitHub token in its environment, and every process
  in the Pod runs as one uid, so code the agent runs can read it.

Orka today:

- ACP agents run by default as sessions in controller-owned RuntimePool Pods. Execution
  workspaces are an opt-in upgrade to separately deployed providers (Agent Sandbox,
  Substrate) for isolation, suspension and checkpoints. Providers moved out of tree in
  #733 and implement a contract built around Orka's ACP supervisor: core writes the
  workload, and losing the runtime Pod fails the workspace instead of restarting it.
- [ADR 0033](0033-user-connectors.md) keeps linked user tokens in sealed controller
  custody and never in worker Pods.
- Nothing runs a long-lived, per-user daemon that a person's own client connects to.

An earlier spike implemented hosts as Service-mode ExecutionWorkspaces with a new
`kubernetes-pod` provider. It worked live, but it added a third provider for what is
Orka's default runtime, and its label-only ownership, Pod-Ready-only readiness and
model-routing sidecar did not hold up in review.

## Decision

### Two resources: an operator template and a user host

- `AgentHostTemplate` (`core.orka.ai/v1alpha1`, namespaced, spec immutable) is the
  operator's definition of a host: the workload (below), its access mode, its storage, its
  resource requests and limits, an optional `runtimeClassName`, extra egress, and an
  optional idle-suspend policy.
- `AgentHost` (`core.orka.ai/v1alpha1`, namespaced) is one user's host:
  `templateRef` (immutable), `suspended`, `wakeRequestedAt`, and an optional
  `githubTokenSecretRef` naming a Secret in the AgentHost's namespace.
- Creating an AgentHost requires `use` on the template, checked against the live
  admission caller, as classes are for Tasks and Tools. RBAC on `agenthosts` decides who
  may create one at all, and who controls it afterwards: anyone who may update a host can
  point it at a Secret they can read and restart it on its volume. AgentHosts live in the
  watch namespace, which the controller reconciles, so everyone's hosts share it and each
  person needs a Role scoped to their own host.
- Naming a GitHub token Secret, on creation or by changing the reference, requires `get`
  on that Secret for the same caller. The controller reads Secrets with its own
  credentials, so without this check anyone allowed to create a host could mount someone
  else's token into it.

### The default runtime is Orka-owned Pods, not a provider

Hosts mirror RuntimePools: the AgentHost controller owns the host's objects directly in a
host namespace (`--agent-host-namespace`, default `orka-agent-hosts`) that the manager
cache includes. Startup rejects a host namespace that is the watch, ACP runtime or
controller namespace: host Pods run under the restricted Pod Security profile, while ACP
supervisors need privileges the runtime namespace allows. There is no provider, ExecutionWorkspace or
class in this path. A later record can let a template select an execution-workspace class
instead; it receives the same host workload.

Per host, in the host namespace: a connection-token Secret, an optional GitHub-token
Secret, a PersistentVolumeClaim, one Pod and one NetworkPolicy. There is no Service and no
TLS endpoint in this release: clients arrive through the relay or a port-forward.

### The host workload contract

The template's workload is ordinary containers (images by digest, command, arguments,
plain environment, resources, mounts of scratch volumes and the state volume) with one
container named `host`. Orka adds, and promises nothing else:

- `ORKA_HOST_ID`: the AgentHost UID, stable for the host's life (for `--compute-id`).
- `ORKA_HOST_NAME`: the AgentHost name, for display.
- `ORKA_CONNECTION_TOKEN_FILE`: a per-host random token staged in a memory-backed
  volume mounted only into `host`. The launcher reads it once and deletes it.
- `ORKA_GITHUB_TOKEN_FILE`: only when the template asks for it and the AgentHost names a
  token Secret; a read-only mount in `host` only.
- `ORKA_HOST_AGENT`: a read-only copy of the Orka host agent. Run as
  `orka-host-agent init -- <command>`, it is the container's PID 1: it reaps every orphan
  the container adopts, which agent daemons do not, forwards signals and exits with the
  command's status.
- The state volume at `/workspace`. Everything that must survive a restart lives there.
  Executable caches stay on per-Pod scratch.
- A loopback AHP listener on the template's `listenPort`, gated by the connection token.

The workload may declare `idleStatusFile`, a path inside the Pod where it writes JSON
with at least `idle` (boolean) and `idleSince` (RFC 3339 or null), rewritten at least every
five minutes. Freshness is judged by `updatedAt` (RFC 3339) when the file has one and by the
file's modification time otherwise. A status is stale ten minutes after that time, twice the
rewrite interval, or when that time or `idleSince` is more than ten minutes ahead.
copilotd's `--idle-status-file` format satisfies it.

### Readiness and status come from an Orka sidecar

Every host Pod also runs `orka-host-agent`, an Orka binary shipped in the controller
image and pinned by digest (`--agent-host-agent-image`):

- Its startup probe completes an AHP `initialize` (offering every protocol version the
  AHP Go client supports) and `ping` against the loopback listener with the connection
  token. It never sends `authenticate`, so it cannot claim a single-user host. Pod Ready
  therefore means an AHP client can talk to the host.
- After startup its readiness and liveness checks open no AHP session, because an
  attached client would reset the host's idle clock. A host that declares an idle status
  file is alive while it keeps that file fresh, so its listener is not touched at all;
  copilotd logs every connection that never completes a handshake. Other hosts get a TCP
  check of the listener. A restarted sidecar completes the startup handshake again from
  these checks, because the host's startup probe never runs again.
- It serves the parsed idle status (no other fields) on a port that only the Orka
  controller may reach.
- It reads the connection token from its own Secret mount, which `host` does not share.
  The same binary, run as the first init container, stages the token and a copy of itself
  for `host`, so host images need no tools of their own.

### Restarts and suspension are driven by core

- The Pod uses `restartPolicy: Never`; kubelet restarts are not relied on. The Pod is
  retired as a whole when it terminates, when the `host` container exits even though the
  sidecar still runs, or when the host fails its liveness check (its listener stays
  down). The controller confirms the old Pod is gone, then creates a new one on the same
  volume with a freshly staged connection token, backing off exponentially (10 s
  doubling to 5 min) until a replacement becomes ready.
- `spec.suspended: true` deletes the Pod and keeps the volume and Secrets; `false` creates
  it again. A suspended or replaced host never replays an in-progress turn.
- An operator changes a template by recreating it under the same name. Every host on it
  then gets a new Pod from the new template on its existing volume: the Pod records the
  template UID it was rendered from, and a Pod from an earlier UID is replaced. The
  volume keeps the storage settings it was created with.
- Idle suspension is opt-in per template (`idleSuspend.after`, from 1m to 720h, checked by
  the API server so a malformed duration can never be stored). The controller reads the
  sidecar's idle status and suspends a host idle for that long, recording
  `status.idleSuspendedAt`. A `spec.wakeRequestedAt` newer than that wakes it; anyone
  who may update the AgentHost can set it. A CLI command can follow once the Orka API
  serves AgentHosts.
- Over the relay, a client cannot wake a suspended host: the relay delivers connection
  requests over the host's own connection. Hosts therefore stay on unless an operator
  enables idle suspension.

### Access modes

- `relay` (the copilotd sample): the template passes `ORKA_HOST_ID` as `--compute-id`
  and the GitHub token as the initial registration credential. The token file is also
  passed for rotation. The host needs only egress.
- `standalone`: the client port-forwards to the loopback listener and presents the
  connection token, which the user reads from the host's connection Secret. Access is
  `pods/portforward` on that Pod plus `get` on that Secret.

### Credentials

- The connection token is minted by the controller per host and never leaves the
  cluster except to a client the user authorizes by RBAC.
- The GitHub token is the user's own, supplied in a Secret in the AgentHost's namespace
  and copied into the host namespace. A new value in that Secret reaches the running host
  through the copy; another Secret or key replaces the Pod, because copilotd fixes its
  single-user identity when it starts. It is not taken from Connections: ADR 0033 keeps
  linked tokens out of every worker Pod, while an agent host is the user's own
  environment whose agent must hold that token by design.
- Sealed authentication and connection binding stay required by the copilotd sample.

### Ownership and cleanup

- The finalizer and the host namespace are persisted before any object is created.
- Before creating an object, the controller records a receipt (kind, namespace, name and
  a random creation nonce) in AgentHost status; the object carries the AgentHost UID label
  and the nonce annotation, and its UID is recorded as soon as the create returns. Once a
  UID is recorded, only that exact object is accepted. An object found while its receipt
  has no UID, which happens only when the controller stopped between creating it and
  recording it, is adopted only if it carries the receipt's nonce and the host's UID
  label. This relies on the controller being the only writer in the host namespace. Any
  other object under a host's name stops reconciliation with `ObjectCollision` and is
  never adopted or deleted.
- The Standalone connection Secret in the AgentHost's namespace is held to the same
  receipts, but never recovered by nonce: users may create Secrets there and can read the
  receipt, so only the recorded UID counts, and an unrecorded create fails closed with
  `ObjectCollision`. A controller owner reference alone is not trusted either.
- A host that cannot run correctly (a missing template or token, a collision) has its
  Pod stopped and keeps its volume. When the token's source Secret is gone, its copy in
  the host namespace is deleted too. Suspension is honored even then.
- Deletion runs through the finalizer with uncached reads, using the recorded UIDs as
  preconditions: first the Pod, so the workload never runs without its NetworkPolicy or
  credentials, then the NetworkPolicy, the volume and the Secrets. It completes only
  when every recorded object is gone.
- If the controller's host namespace changes, existing hosts report
  `HostNamespaceChanged` and are left alone until deleted; cleanup then needs the
  controller's permissions to remain in the old namespace.

### Governance

- Images by digest; non-root; no service-account token; dropped capabilities; read-only
  root filesystem where the workload allows; Pod Security `restricted` in the host
  namespace; optional `runtimeClassName` (for example gVisor or Kata).
- The NetworkPolicy denies all ingress except the controller (its namespace and Pod
  labels together) to the sidecar's status port, and allows egress to DNS (the cluster DNS
  Pods, and port 53 on link-local addresses for node-local DNS caches), TCP 443 on public
  addresses (private, shared, link-local and loopback ranges excluded) and the template's
  extra rules. It confines hosts only on clusters whose network plugin
  enforces NetworkPolicy.
- Release gate: the copilotd sample must boot, restart and resume under enforced
  restricted Pod Security with its volume, scratch and caches writable.
- The template pins the host's own policy flags (for copilotd: allowed models,
  approval-mode ceiling, repositories, commands and session limits).
- Nobody else may create Pods or exec in the host namespace; either would expose the volume
  and tokens.

## Consequences

- This is a trusted-code, single-user alpha. The agent holds the user's GitHub token and
  egress to 443 is open, so hostile repository content can exfiltrate it. Neither "public
  repositories only" nor the NetworkPolicy is a sandbox boundary.
- The GitHub token rests in a user-managed Secret and in the host's environment. Relay
  mode also depends on a GitHub service and on the user's organization allowing it;
  `standalone` remains for clusters that cannot use it.
- Orka core holds Pod, PVC, Secret and NetworkPolicy permissions in the host namespace, as
  it does in the ACP runtime namespace. A core-owned default runtime is not a provider;
  #733 moved providers out of tree, and this does not move one back.
- Hosts cost a Pod each while running. Idle suspension saves compute but cold-starts on
  wake (about 10 s plus image pull); denser packing and faster resume wait for the
  execution-workspace path.
- Orka does not see turns on this path. Governance is who may create a host, what it runs,
  what it may reach and its pinned policy. Serving AHP from Orka (ADR 0034) is how
  Orka joins the conversation.
- Not in this release: routing model traffic through Orka, an in-cluster TLS endpoint,
  multi-user hosts, Connection-sourced tokens and execution-workspace backends.

### Alternatives considered

- **A `kubernetes-pod` execution-workspace provider** (the spike). Rejected: a third
  provider for the default runtime, plus provider deployment and parameter kinds, with no
  capability the core-owned path lacks.
- **Agent Sandbox or Substrate now.** Their contracts fit Orka's ACP supervisor, not an
  operator workload that must be restarted. They become additional backends behind the
  same host workload contract.
- **Tokens from Connections.** Conflicts with ADR 0033's custody rule.
- **Port-forward only.** Works, but every user needs cluster credentials and a URL with a
  token. The relay needs nothing on the client.
- **Readiness from the workload's own probes.** Would make readiness host-specific and
  could not check AHP without claiming the host.
