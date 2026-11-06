---
description: "Debug a running agent Task with orka task exec and orka task cp: enablement, permissions, the session taint, and audit."
---

# Operator workspace access

`orka task exec` and `orka task cp` let an operator look inside the execution
workspace of a running ACP agent Task. You can run a command, open an interactive
shell, or copy a single file out of or into the workspace. They are debugging
tools for administrators, not an agent feature:

- They are **off by default**, and an administrator must enable them at three
  independent levels.
- Every call needs a dedicated Kubernetes permission and is **audited**.
- Anything that could change the workspace **taints** the Task's session.
  After that, the Task is never published, reused, checkpointed for forks, or
  continued.

Commands run as the session's own unprivileged child identity, the same one
the agent runs as. They never run as the supervisor or as root, and they
never reach the supervisor's credentials. Traffic always goes through
orka-api. The CLI never connects to a runtime Pod directly.

## What is supported

| Target | Supported |
| --- | --- |
| Class-backed ACP Task (`spec.execution.workspace.classRef`) on an Agent Sandbox or other Pod-backed provider, while its RuntimeSession is live | Yes |
| The same workspace after the Task finished, while it is still attached to the Task or the Task held its last attachment, and its runtime is still running | Yes |
| Substrate native-process workspaces | Not yet (`501`) |
| Suspended or deleted workspaces, built-in pools without an execution workspace, container Tasks, external `AgentRuntime` registrations | No |

The supervisor refuses access whenever the workspace is frozen for validation
or is being published. That covers validation, publication preparation,
publishing, verification, finalization, cancellation, and deletion.

## Enable it

### 1. Turn on the API

Start the controller with:

```bash
--operator-workspace-access-enabled
```

Without the flag, every operator route answers `403 operator workspace access
is disabled`.

### 2. Opt in a class

Annotate the `ExecutionWorkspaceClass` the Tasks use:

```bash
kubectl annotate executionworkspaceclass sandbox-debug -n team-a \
  acp.workspace.orka.ai/operator-access=exec
```

| Value | Allows |
| --- | --- |
| `read` | `orka task cp <task>:<path> <local>` (copy out only) |
| `exec` | Copy out, copy in, and `orka task exec` |
| absent or anything else | Nothing |

A ValidatingAdmissionPolicy only lets callers holding the dedicated
`operator-access` verb on the class set, change, or remove this annotation.
`cluster-admin` has that verb through its wildcard. No built-in Orka role
grants it. Class editors and Task authors, who only need `use` on a class,
cannot enable operator access. To grant the verb to a platform team:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: orka-workspace-operator-access-admin
rules:
- apiGroups: ["workspace.orka.ai"]
  resources: ["executionworkspaceclasses"]
  verbs: ["get", "list", "patch", "update", "operator-access"]
```

orka-api reads the annotation live on every call. Removing it revokes access
immediately, including for running Tasks.

### 3. Grant the operator permission

Each operation is authorized with a SubjectAccessReview on a dedicated
`tasks` subresource. Task `create`/`get` alone is never enough, and no
built-in aggregate role includes these:

| Operation | Resource | Verb |
| --- | --- | --- |
| `orka task exec` | `tasks/exec` | `create` |
| `orka task cp <task>:<path> <local>` | `tasks/files` | `get` |
| `orka task cp <local> <task>:<path>` | `tasks/files` | `update` |

Each operation also needs `get` on the Task.

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: orka-task-debugger
  namespace: team-a
rules:
- apiGroups: ["core.orka.ai"]
  resources: ["tasks"]
  verbs: ["get"]
- apiGroups: ["core.orka.ai"]
  resources: ["tasks/exec"]
  verbs: ["create"]
- apiGroups: ["core.orka.ai"]
  resources: ["tasks/files"]
  verbs: ["get", "update"]
```

Context-token (transaction-token) callers need an explicit scope instead:
`orka:tasks:exec`, `orka:tasks:files:read`, or `orka:tasks:files:write`. The
token's transaction context must also be bound to the Task's namespace. Task
scopes never imply these scopes, and they are enforced in every context-token
authorization mode. Identities with no Kubernetes identity to review, such as
direct OIDC sign-in, are refused.

## Use it

```bash
# One-off command
orka task exec my-task -- git status

# Interactive shell (Ctrl-C, resizing, and job control behave like a terminal)
orka task exec my-task -it -- bash

# Pipe stdin
git diff | orka task exec my-task -i -- git apply --check -

# Copy a file out (does not taint)
orka task cp my-task:src/main.go ./main.go
orka task cp my-task:/logs/build.log - | less

# Copy a file in (taints)
orka task cp ./fix.patch my-task:fix.patch
```

Workspace paths are relative to the workspace root, which is the agent's
working directory. A leading `/` names that root, so `my-task:/src/a.go` and
`my-task:src/a.go` are the same file. Paths with `..`, NUL bytes, or control
characters are rejected. Symbolic links are never followed, in any path
component. Copies transfer exactly one regular file. Copying in replaces the
destination atomically, and only after the complete upload arrives. The parent
directory must already exist.

`-t` allocates a terminal. Because `-t` means `--tty` on `orka task exec`, use
`--token` there for a bearer token. Without a terminal, the first Ctrl-C is
forwarded to the remote command as `SIGINT` and a second abandons the stream.
Closing the stream kills the remote command and every process it started. Each
operation carries a unique supplementary group that its processes cannot drop.
So even descendants that start a new session or process group, or outlive the
command, cannot survive the timeout, a disconnect, or the session's validation
freeze. When a prompt settles and the workspace is frozen
for validation, or the session is deleted, running operator commands are
killed.

| Limit | Default | Maximum |
| --- | --- | --- |
| `--timeout` (exec) | 15 minutes | 1 hour |
| Exec output (stdout + stderr) | 64 MiB | 512 MiB |
| `--max-bytes` (copy out) | 64 MiB | 256 MiB |
| Copy-in file size | — | 256 MiB |
| Stdin buffered ahead of the command | 8 MiB | — |
| Concurrent operations per runtime | 4 | — |

`orka task exec` exits with the remote command's status. Orka-side outcomes
use these exit codes:

| Exit code | Meaning |
| --- | --- |
| `124` | Timeout |
| `125` | Output or stdin buffer limit |
| `126` | Refused or failed to start |
| `130` | Interrupted locally |

## The session taint

`orka task exec` and `orka task cp` into a workspace can change anything the
session's identity can write. Before either one starts, orka-api records the
`acp.workspace.orka.ai/operator-tainted` annotation on the Task and on its
ExecutionWorkspace. If that write fails, the operation is refused. Only the
Orka controller identity can write either annotation, so a Task author cannot
remove it.

A tainted session:

- **Is never published.** The publication is settled as cancelled before
  publish, and the credential broker refuses forge write credentials.
- **Is never reused.** No other Task attaches the workspace, reuses it warm,
  or cold-resumes it. A continuation queued behind the tainting Task waits for
  a fresh workspace, and otherwise fails with `execution workspace was tainted
  by operator access`.
- **Is never checkpointed.** Settlement and idle retention delete the
  workspace instead of suspending it, even when the class freezes `Suspend`.
  `ExecutionWorkspaceCheckpoint` exports from it fail routing with
  `SourceOperatorTainted`, so forks cannot consume it.
- **Is never continued.** Its runtime is not captured as the Session's native
  continuity. A first turn falls back to the canonical transcript, and a
  Session that already depends on native continuity fails closed.

The taint is never cleared. Copying out does not taint. The taint is recorded
before the request reaches the runtime. An exec or copy in that the runtime
then refuses, for example because the prompt settled at that moment, still
leaves the session tainted.

## Audit

Each call produces records for when it started and finished, or when it was
denied, under one audit ID. Records go to the orka-api log under
`audit: operator workspace access` and are emitted as Kubernetes Events on the
Task (`OperatorAccessStarted`, `OperatorAccessFinished`, `OperatorAccessDenied`):

```bash
kubectl get events -n team-a --field-selector involvedObject.name=my-task
```

Each record includes:

- the actor and authentication type
- the namespace, Task, and Task UID
- the RuntimeSession UID and generation, and the workspace
- for exec: the command basename, argument count, and a SHA-256 digest of
  argv. Raw arguments are never recorded.
- the copy path and byte count
- the exit code or outcome, and the start and finish times
- whether the session was tainted

Free-text fields (the command basename, copy path, Task name, and outcome) are
redacted with Orka's shared credential rules, and a value that still looks like
a credential is recorded as `[REDACTED]`.

## Troubleshooting

| Error | Cause |
| --- | --- |
| `operator workspace access is disabled` | The controller lacks `--operator-workspace-access-enabled`. |
| `ExecutionWorkspaceClass ... does not allow operator ...` | The class lacks the annotation, or is `read` for exec or a copy in. |
| `403 not authorized` | Missing `tasks/exec` or `tasks/files` permission. |
| `requires a Kubernetes identity` | The caller signed in with OIDC. Use a ServiceAccount or kubeconfig token. |
| `context token lacks the explicit ... scope` | Add the operator scope to the transaction token. |
| `task workspace is being validated for publication` / `runtime session is validating` | The prompt settled and the workspace is frozen. Wait for publication to finish. |
| `execution workspace is attached to another Task` | A continuation took the workspace. Target the current Task. |
| `task runtime instance is no longer active` / `... was replaced` | The runtime Pod or workspace was replaced. The old session is gone. |
| `not supported for native-process (Substrate) execution workspaces` | Not available yet for Substrate. |
| `runtime supervisor does not support operator access` | The runtime image predates this feature. Upgrade it. |

See [ADR 0044](https://github.com/orka-agents/orka/blob/main/docs/adr/0044-operator-workspace-access.md)
for the design rationale.
