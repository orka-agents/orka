# 44. Audited, taint-on-write operator access to ACP execution workspaces

Date: 2026-11-06

## Status

Accepted. Adds the operator debugging surface (`orka task exec`, `orka task cp`)
on the class-backed ACP workspace binding from ADR 0026 and the retention rules
from ADR 0029. Substrate native-process workspaces are not supported yet.

## Context

Operators debugging an agent Task need a shell in, and files from, the
workspace the agent is using. Before this decision nothing offered that: the
workspace agent's `/v1/exec` and `/v1/files` endpoints are controller-internal,
the only public file route serves published artifacts, and the ACP supervisor
speaks only the controller's harness v2 protocol. Giving operators raw Pod
access (`kubectl exec`) would reach the supervisor's root identity, every
session in the Pod, and whatever credentials the supervisor holds, and it would
leave no Orka audit trail.

A debugging shell is also a write channel into state Orka later trusts. A
command can change the repository a publisher pushes, the agent's private home
that native continuity captures, or the data a suspension checkpoint preserves
for continuations and forks.

## Decision

Operator access is a separate, off-by-default capability with three
independent opt-ins, a dedicated permission, and a durable taint.

### Enablement and authorization

- The controller serves the routes only with
  `--operator-workspace-access-enabled`.
- An administrator opts a class in with the
  `acp.workspace.orka.ai/operator-access` annotation on the
  `ExecutionWorkspaceClass`: `read` allows copies out, `exec` also allows
  exec and copies in. Absent or unknown values disable access. A
  ValidatingAdmissionPolicy requires the dedicated `operator-access` verb on
  the class to set, change, or remove the annotation. No built-in role grants
  that verb, so class editors and Task authors who can only `use` a class
  cannot enable it. The annotation is read live, so removing it revokes access
  for running Tasks. The live class UID must match the workspace's class
  binding.
- Each call is authorized against dedicated virtual subresources of `tasks` in
  `core.orka.ai` with SubjectAccessReview: `create` on `tasks/exec`, `get` on
  `tasks/files` for copies out, and `update` on `tasks/files` for copies in.
  Each also requires `get` on the Task. No aggregate role grants them.
  Context-token callers instead need the explicit scope `orka:tasks:exec`,
  `orka:tasks:files:read`, or `orka:tasks:files:write`, plus a transaction
  context bound to the Task namespace. This holds in every context-token
  authorization mode. Callers with no Kubernetes identity to review, such as
  direct OIDC, are refused.

### Target resolution

The API resolves the exact session from the Task and fails closed at every
link:

1. The Task is class-backed and not being deleted.
2. It records a live RuntimeSession (pool, instance, session UID and
   generation), and its execution is not `Settling`.
3. Its linked ExecutionWorkspace incarnation (the UID on the Task) exists,
   with desired state `Ready`.
4. The workspace is attached to this Task, or this Task held its most recent
   attachment epoch.
5. The workspace names the Task's RuntimePool.
6. The RuntimePool UID and workspace link match.
7. The active instance matches the Task's instance, at the current controller
   epoch.
8. The runtime Pod's UID and IP match the active instance.
9. The supervisor advertises operator access for the pinned profile digest.

Refused requests produce an audit record and never reach the supervisor's data
plane.

### Data path and execution identity

All traffic flows CLI → orka-api → supervisor. The CLI never reaches a Pod
directly.

- Exec uses a WebSocket. The command is the first stream message, so argv
  never appears in URLs, headers, or access logs.
- orka-api signs a harness v2 operation capability over the canonical request
  digest and the session fence. It sends that capability with the pool's
  controller bearer.
- The supervisor authenticates on headers alone before it upgrades the
  connection or reads a body. It refuses every session state except `idle` and
  `prompt_running`. Refused states include validation (when the workspace is
  frozen), publication preparation, publishing, verification, finalization,
  cancellation, poisoning, and deletion.
- Commands, and the file helper for copies, run through the ACP exec helper.
  They use the session's own non-root child UID and GID, resource limits, and
  parent-death fence. Their only supplementary group is the operation's tag
  (below). They start in the session
  workspace with only the session's minimal environment. Operators never get
  the supervisor's identity or credentials.
- Copies move exactly one regular file. The workspace root is opened without
  following symbolic links and must match the directory identity the
  supervisor recorded before the session child started. Every path beneath
  it is resolved in one `openat2` call with `RESOLVE_BENEATH`, and no
  symbolic links, magic links, or mount crossings, so a directory the session
  moves out mid-copy fails the resolution. Without `openat2` copies are
  refused. Uploads re-verify their parent directory before committing.
  Traversal segments are rejected before any request is made. Uploads commit
  only after exactly the declared size arrives.
- Each operation runs with one supplementary tag group, reserved from a
  dedicated range and unique among live processes. Changing supplementary
  groups needs `CAP_SETGID`, and descendants inherit them, so the tag cannot
  be dropped. On timeout, disconnect, output limit, or revocation, the
  supervisor kills every process of the session UID that carries the tag, and
  repeats until no live process carries it.
  - Terminated processes waiting to be reaped cannot run again, so they do
    not count.
  - A zombie leader whose threads still run does count.
  - Each scan re-lists the process table until no PID appears that was not
    already inspected, so processes forked mid-scan are caught.
  - Containment requires two consecutive settled empty scans, the same
    standard as the RuntimeSession UID cleanup proof. This reaches descendants that changed process
  group, session, or parent, or that killed the command first. The agent
  shares the UID but has no tag, so it is never touched. The tag grants no
  file access.
- If termination cannot be proven, the supervisor fails closed. It poisons the
  session and the pool. It also keeps the operation's registration, tag, and
  concurrency slot, so no barrier, tag reuse, or new operation proceeds past
  it.
- Admission registers each operator operation on its session under the
  supervisor lock, and re-checks the state after the process starts.
  Registrations are removed only after the owning handler's proven cleanup.
  Every transition out of `idle` or `prompt_running` revokes and kills the
  registered operations. Before the validation freeze and before session
  deletion, a synchronous barrier does three things:
  - closes admission,
  - kills bound operations,
  - waits until in-flight launches have been refused and killed.

  If the barrier cannot finish, the session and pool fail closed. Deletion is
  refused and the session's resources are kept.
- The WebSocket reader never blocks on child stdin. Up to 8 MiB is queued
  ahead of the command, end-of-input stays in order, and overflow terminates
  the command. Disconnects and control frames are always observed.
- Exec is bounded by a timeout (default 15 minutes, maximum 1 hour), an output
  cap (default 64 MiB, maximum 512 MiB), and a per-supervisor concurrency cap.
  Interactive terminals get a PTY. Without a terminal, the CLI forwards Ctrl-C
  as `SIGINT`, and disconnecting kills the command's process group.

### Taint

Exec and copies in are write-capable. Before either starts, orka-api writes the
`acp.workspace.orka.ai/operator-tainted` marker to two places: the exact
ExecutionWorkspace incarnation, as a UID- and resourceVersion-preconditioned
patch, and the Task. If either write fails, the operation is refused. Copies
out never taint.

Both prefixes are controller-protected: by the core workspace admission policy
and by the Task provenance webhook. A tainted session is enforced at these
points:

- **Publication.** The fresh and recovery pre-publishing decision treats taint
  like cancellation and settles the publication as cancelled before publish.
  The credential broker also refuses publish-write credentials for a tainted
  Task.
- **Reuse and continuation.** `ensureACPClassWorkspace` never attaches,
  warm-reuses, or cold-resumes a tainted incarnation for another Task. A
  continuation queues while the tainting holder settles, and fails closed
  otherwise.
- **Checkpoints.** Settlement and idle retention never suspend a tainted
  workspace. They apply Delete regardless of the frozen Suspend action, and the
  suspension helper refuses as a backstop. Checkpoint routing refuses a tainted
  source with `SourceOperatorTainted`, so forks never consume its data.
- **Native continuity.** A tainted runtime is never captured as the Session's
  native continuity source. A first turn falls back to the canonical
  transcript. A Session that already depends on native continuity fails
  closed.

Taint is never cleared. Deleting the workspace removes the marker along with
the data it covered.

### Audit

Every call produces structured audit records: started, finished, or denied.
Each record carries the actor and authentication type, the Task and
RuntimeSession identity, and the workspace. Exec records add the command
basename, argument count, and argv digest, never raw arguments. Records also
carry the path, byte count, exit code or outcome, and timestamps. Records are
logged by orka-api and emitted as Kubernetes Events on the Task. No file
content, token, or credential enters a record.

## Consequences

- Debugging a running agent no longer needs `kubectl exec` into a supervisor
  Pod. When it is used, it is attributable and bounded.
- Writing to a workspace is a one-way decision. A tainted Task's work is never
  published, and its Session cannot continue from that workspace. Operators who
  only need to read should use `orka task cp` out.
- Without a cgroup or PID namespace, containment is a `/proc` scan, not an
  atomic freeze. Two independent backstops cover integrity and cleanup. The
  validation freeze stops every process of the session UID before the delta is
  captured, and session teardown kills every process of that UID.
- Operator processes share the session's child UID. Session teardown and
  adapter restarts also reap them, as a backstop to tag containment and
  revocation. Up to 4000 operations can be live per runtime at once, the size
  of the tag range.
- Substrate native-process workspaces are not supported until the provider
  route can carry authenticated streaming. Retained workspaces whose compute is
  suspended are unreachable. Only directly bound Pod-backed runtimes are
  supported.
