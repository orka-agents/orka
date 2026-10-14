---
title: Native session migration
description: Move native Claude Code, Codex, Copilot CLI, and OpenCode conversations between a local agent and Orka Sessions.
---

# Native session migration

`orka session migrate` moves an agent's own conversation between a local
install and an Orka Session and keeps the agent's session ID. `--agent` selects
the agent, and `--state-dir` names the directory the agent keeps its state in:

| `--agent` | Supported release | `--state-dir` | Bundle |
| --- | --- | --- | --- |
| `claude` | Claude Code 2.1.293; export also to later releases | `CLAUDE_CONFIG_DIR` (`~/.claude` by default) | The session's JSONL transcript, plus the tool results it spilled to files |
| `codex` | Codex 0.160.0 `codex/paginated` threads | `CODEX_HOME` | The verified SessionKit manifest and the thread's original rollout bytes |
| `copilot` | Copilot CLI 1.0.96; export also to later releases | `COPILOT_HOME` (`~/.copilot` by default) | The session's `events.jsonl` |
| `opencode` | OpenCode 1.19.0 | `XDG_DATA_HOME` (`~/.local/share` by default) | A copy of `opencode/opencode.db` holding only the session's rows, plus the output its own tool calls spilled to files |

All run on Linux and macOS. A bundle never contains authentication files,
provider configuration, or process memory. A Claude bundle excludes
`.claude.json`, settings, project memory, and every other session. A Codex
bundle excludes Codex's SQLite databases. A Copilot bundle excludes
`workspace.yaml`, lock files, settings, logs, and every other session. An
OpenCode bundle excludes every
other session and project, the database's event log, saved permission grants,
and its account and credential tables.

Orka defaults to 8 MiB of encoded bundle data, configurable up to 64 MiB.
This is a consumer transport limit, separate from SessionKit's native storage
limits.

The controller and runtime must both support native Sessions. Use the pinned
Claude Code 2.1.293 / Claude Agent ACP 0.89.1, Codex 0.160.0 / codex-acp 2.1.1,
Copilot CLI 1.0.96, or OpenCode 1.19.0 runtime image. A checkpoint
restores only into a runtime of the agent that saved it. Private checkpoints use
the controller's existing execution-snapshot encryption key with separate
associated data. Retain that key with the SQLite backup.

Deploy this schema on a new installation. Existing Orka databases are not
upgraded in place; see [Upgrading](../operations/upgrading.md).

## Codex

Legacy rollouts and encrypted reasoning context are not supported.

Use a model that supports non-reasoning turns and set the destination Codex Agent's
`spec.runtime.defaultReasoningEffort: none`. Set `model_reasoning_effort = "none"`
in the source and return homes too. This selects Codex's non-reasoning mode; it
does not bypass capture validation. A rollout with encrypted reasoning is still
rejected, even if the configured effort is `none`.

### Returning to an existing Codex home

Export cannot merge a returned thread into a populated `~/.codex` or replace its
older local copy. The original UUID is preserved, so a return can collide with
that copy. Existing paginated SQLite state can also shadow a newly copied
rollout. SessionKit does not modify those databases or reconcile divergent
local and cluster histories.

Use a separate fresh `CODEX_HOME` on return and resume the UUID explicitly.
Keep `CODEX_SQLITE_HOME` unset or set it to an absolute path without surrounding
whitespace resolving to that exact private `CODEX_HOME`. The export CLI rejects
relative and whitespace-padded overrides, resolves symlinks, and rejects a
different database directory, including a nested directory whose existing
database state is not checked by fresh-home validation, before fetching or
staging the checkpoint. Your original
home remains unchanged and can still be an import source. Safe per-thread
replacement in an existing home is not implemented.

### Encrypted native reasoning

A non-null `encrypted_content` field anywhere in the selected rollout,
including reasoning items or compaction history, rejects the migration. This
is provider-issued opaque native context, not Orka's checkpoint encryption at
rest. Preserving its bytes has not established a supported reload contract with
destination provider settings and credentials. Migration neither decrypts nor
strips it to make the thread acceptable.

Setting reasoning effort to `none` before creating and continuing a thread can
avoid reasoning items on a model that supports that mode. It does not remove
existing encrypted content, and some models do not support `none`. Supported
non-encrypted compaction records remain byte-identical. This is not general
migration support for reasoning-enabled Codex sessions.

## OpenCode

OpenCode keeps every session in one SQLite database. Import copies it with
SQLite and keeps only the named session, its project, and the files its own
tool calls spilled their output to. The source must be a database OpenCode
1.19.0 wrote: any other schema is rejected. Import reads the database without
changing it.

Import rejects a session that holds state Orka does not carry: a todo list,
subagent (child) sessions, per-session permission rules, or OpenCode
workspaces. It also rejects a source database larger than twice the expanded
budget (16 times the bundle limit) and a session whose tool calls spilled more
than 256 outputs.

OpenCode resumes a session in the directory it recorded, not the directory it
starts in. Import takes that directory from the database; a `--cwd` you pass
must match it. Export and Orka's runtime restore move the session to the new
working directory and rewrite the paths the conversation recorded.

Export installs into a state directory without an OpenCode database. Use a
fresh directory: OpenCode also keeps provider credentials there, so
authenticate again before resuming.

## Claude Code

Claude Code keeps each session as one append-only JSONL transcript,
`projects/<folder>/<session-id>.jsonl`, and finds it by ID in any project
folder. Large tool results spill to `<session-id>/tool-results/`, and the
transcript refers to them by absolute path. Import takes the transcript and
those files; subagent transcripts and anything else in the session's folder
stay behind. Export and Orka's runtime restore install the transcript under the
new working directory's project folder and rewrite only the spilled results'
paths. Claude Code resumes in whatever directory it is started in.

Claude Code releases often. Import accepts a transcript that Claude Code
2.1.293 or an earlier release last wrote, because Orka's runtime must be able to
read it; a transcript from a later release is rejected, since it may hold
entries 2.1.293 cannot read. Export accepts a local Claude Code 2.1.293 or later,
since later releases resume what earlier ones wrote. Import also rejects a
transcript with an entry type Claude Code 2.1.293 does not read, entries of
another session, an unfinished last entry, or more than 256 spilled tool
results, and a session that two project folders hold. At runtime, capture falls
back to the canonical transcript in the same cases.

Answering `session/resume`, Claude Code sends one "Hi" request to its model.
Inside Orka the provider proxy refuses it, because no prompt has been granted
provider access yet; Claude Code resumes anyway. Thinking blocks stay in the
transcript, but Claude Code drops earlier turns' thinking when it rebuilds the
request after a resume, so their signatures are never replayed.

## Copilot CLI

Copilot CLI keeps each session as an append-only event log,
`session-state/<session-id>/events.jsonl`, that begins with the `session.start`
event naming the session and the release that started it. Copilot cannot resume
a session, so Orka's runtime restores it with `session/load`, which replays the
history before the next prompt; Orka discards the replay instead of recording
it again. Copilot runs tools in the working directory `session/load` names, so
nothing is rewritten. Large shell output spills to `TMPDIR`, which is not
carried: a restored conversation keeps the spilled output's preview.

Import accepts a session that Copilot CLI 1.0.96 or an earlier release started,
and rejects a later release's session, an event log with another schema, a
second `session.start`, or an unfinished last event. Export accepts a local
Copilot CLI 1.0.96 or later. Copilot keeps its encrypted reasoning in the
event log and does not send it again after a load. Every load appends a
`session.resume` event and a full system message, about 40 KB, so a Session
restored every turn grows by that much per turn.

## Bundle size

The default limit is 8 MiB, or 8,388,608 bytes, for the complete encoded bundle.
Operators can select a limit from 1 byte through the 64 MiB hard ceiling with
`--native-session-max-bytes` or `ORKA_NATIVE_SESSION_MAX_BYTES` on the
controller. The controller propagates that policy to built-in runtime pools,
including workspace-backed pools. Independent runtimes must set the same
environment variable; their advertised capability limit is also enforced.
Changing the built-in runtime policy uses the existing pool replacement and
drain path.

The local CLI has a separate matching limit. Use
`orka session migrate --max-bundle-bytes 16777216 import ...` or the same flag
with `export` for a 16 MiB bundle. `ORKA_NATIVE_SESSION_MAX_BYTES` supplies the
CLI default when the flag is omitted. Raising only the local CLI limit cannot
raise the cluster limit. New clients request the additive native-size capability
with `X-Orka-Native-Session-Limits: 1`; older clients receive the original capability
shape without the new field. An older runtime that does not advertise its limit
retains the previous 512 KiB bound and rejects larger native restores before
request delivery. Capture requests use the same opt-in header; older callers
retain the 512 KiB capture limit and its structured rejection behavior.

This is a byte limit, not a model token limit. A Codex bundle is JSON that
base64-encodes both the rollout and manifest, leaving approximately 6 MiB of
combined raw bytes under the 8 MiB default, slightly less after JSON overhead.
An OpenCode bundle is gzip-compressed, and its uncompressed contents may reach
eight times the limit. The HTTP envelope adds another encoding layer with its
own bounded allowance. Ordinary API, prompt, and ACP message limits are
unchanged.

The cap applies to import and every runtime checkpoint, including growth after
continuation. It counts the full saved conversation, including tool outputs and
historical records retained after compaction. Orka does not truncate the
bundle. Oversized native data is rejected; an established native-continuity
Session does not silently fall back to transcript-only continuation. Lowering
the policy preserves stored checkpoints but can block export, restore, or new
capture until the limit is raised again. Before lowering the cap, pause new
native Tasks, let active turns finish and durably finalize their checkpoints,
and verify that retained checkpoints fit the proposed cap. If capture is already
blocked, preserve the exact runtime Pod and its capture evidence; force-deleting
it can lose uncommitted native history. This is an Orka transport restriction,
not SessionKit's native storage limit. Larger limits increase memory, temporary
storage, and encrypted database usage; choose them for the expected workload.

## Explicit API target and authentication

Native migration requires `--server` or a saved API endpoint. Automatic
Kubernetes Service discovery is not used: a familiar name, labels, and Service
UID do not establish the application's identity. Select the trusted Orka API
endpoint explicitly before sending portable history.

Provide an API bearer credential (`--token`, a saved Orka token, or a static
kubeconfig bearer credential) or a supported transaction token. Kubernetes exec,
auth-provider, and certificate credentials are not automatically resolved into
an Orka API credential. Missing API authentication fails before capture or
persistence of the migration bundle. Configure the endpoint and authentication
before running the commands below.

## Import a local session

Stop every local agent process using the source state directory before capture.
`--source-stopped` records that assertion; it does not terminate a local
client. Use a new Session name:

```bash
orka session migrate import migrated-review --agent codex \
  --session-id 01a10020-1222-76e3-977d-d5165792ae72 --source-stopped
```

For OpenCode, `--session-id` is the `ses_` ID that `opencode session list`
shows:

```bash
orka session migrate import migrated-review --agent opencode \
  --session-id ses_ec7cc3c6cffejC2X3k579awtpZ --source-stopped
```

Every path has a default:

| Flag | Import default | Export default |
| --- | --- | --- |
| `--state-dir` | The agent's own: `$CLAUDE_CONFIG_DIR` or `~/.claude`; `$CODEX_HOME` or `~/.codex`; `$COPILOT_HOME` or `~/.copilot`; an absolute `$XDG_DATA_HOME` or `~/.local/share` | A fresh `~/.orka/sessions/<namespace>/<session>/<agent>` |
| `--cwd` | OpenCode: the directory the session recorded. Claude Code, Codex, and Copilot CLI do not use it | The current directory |
| `--journal-dir` | `~/.orka/migrations/<namespace>/<session>/import` | `~/.orka/migrations/<namespace>/<session>/export` |
| `--agent-bin` | Not used | `claude`, `codex`, `copilot`, or `opencode` on `PATH` |

A default journal is removed once its migration completes, so its private copy
of the conversation does not linger and the next migration of that Session
starts anew. A journal you name with `--journal-dir` is kept. Every directory
the CLI creates has mode `0700`.

Import reserves a fresh non-Gateway Session. It allocates no runtime or Task
lease. An existing Session, including an empty one, cannot be overwritten.

Submit a normal Task for an Agent of the same runtime type with
`spec.sessionRef.name: migrated-review` and `spec.sessionRef.create: true` for
the first continuation. A Task for another runtime type is rejected. Use the
Task's current workspace and tool policy. During admission, Orka installs the
bundle in the child's fresh private state directory before starting the agent.
It uses capability-gated `session/resume` with the original session ID and
current cwd, provider, MCP tools, approvals, and sandbox configuration. The
runtime must advertise resume support and prove restoring the exact bundle
before Orka sends the new user prompt. Canonical history is not injected again.
Missing resume support or a failed resume rejects creation without falling back
to `session/new`. The imported history stays native; the Orka transcript
records new Tasks.

## Export a checkpoint

After a successful Task, Orka proves the provider and its descendants have exited
before capture. The checkpoint and canonical terminal result commit together
before the Session lease is released. The runtime home is deleted only after
that commit. The next Task cold-starts with the saved checkpoint and fresh
credentials.

Export uses the last committed checkpoint. It does not force an active Task to
stop. The checkpoint must still match the exact Session owner and canonical
transcript boundary.

Run export from the directory to resume in:

```bash
cd /absolute/local/workspace
orka session migrate export migrated-review --agent codex
```

`--agent` must match the agent that saved the checkpoint. The local agent
executable must report the supported release. The destination must be a fresh
private state directory with directory mode `0700`. A default destination that
already holds an earlier export is rejected; remove it or pass `--state-dir`.
The journal must be outside the destination and private, also with directory
mode `0700`. Authenticate the destination with current local credentials and
configure its provider before resuming the session. Migration does not copy
source authentication or source policy into authority.

Point the agent's state variable at the destination the command printed, and
resume the session ID from the same directory:

```bash
CLAUDE_CONFIG_DIR=~/.orka/sessions/default/migrated-review/claude claude --resume 30840f35-9928-4d2d-8785-873a00b244bb
CODEX_HOME=~/.orka/sessions/default/migrated-review/codex codex resume 01a10020-1222-76e3-977d-d5165792ae72
COPILOT_HOME=~/.orka/sessions/default/migrated-review/copilot copilot --resume bff1f96a-be09-426e-933b-ec3720e975ac
XDG_DATA_HOME=~/.orka/sessions/default/migrated-review/opencode opencode --session ses_ec7cc3c6cffejC2X3k579awtpZ
```

## Retry and failure handling

Retain the journal and repeat the same command after an uncertain response;
a default journal stays until the migration completes. The CLI reuses its saved
bundle and operation ID. A Codex export also reuses the
same frozen SessionKit install plan and receipt. A failed OpenCode install
removes the files it created. Once an export installs its session, a retry with
the same journal reports it again without reinstalling. Changing the agent,
target, bundle, or cwd with that journal is rejected. The explicitly selected API endpoint must stay
unchanged across retries. Automatic Kubernetes Service discovery and streaming
are not available for native migration; use `--server` or a saved trusted API
endpoint and supply API authentication as described above.

An uncertain runtime install retains its target and frozen journal and closes
pool admission. Exact create retries report that retained outcome without
allocating another home or starting a child. Automatic reconciliation of that
supervisor creation state is not part of this delivery.

A capture failure after proven writer exit can retry from the retained private
home under a new fenced operation. Receipt reconciliation never starts a capture.
If native capture is unsupported and there is no prior native checkpoint, Orka
records one warning execution event with reason `native_capture_unsupported`.
The next turn uses the canonical transcript instead of native continuity.
The warning survives controller retries and restarts without duplicates. If a
prior native checkpoint exists, unsupported capture still blocks settlement;
Orka never silently replaces required native continuity with transcript replay.

Failed, cancelled, or lost provider prompts do not publish a new native
checkpoint. The runtime that ran them is poisoned and retired, so a Task that
appends to a Session with a native checkpoint settles its terminal marker with
the existing checkpoint carried forward: the checkpoint's transcript boundary
advances past the marker, the lease is released, and the next Task resumes the
same native state. That state omits only the prompt that produced no result.
A successful prompt can retain its native state after delivery failure once
workspace finalization and canonical settlement complete. Poisoned workspace
validation failures cannot produce a native checkpoint; for appending Tasks on
a Session with a native checkpoint they retain the exact runtime evidence and
hold the Session lease until reconciliation. Accepted turns with
`sessionRef.append: false` leave the checkpoint unchanged and retire their
runtime before a later Task loads the saved state.
Native restore reloads the agent's whole saved conversation, so
`sessionRef.maxMessages` limits only the canonical transcript Orka sends when
no checkpoint is used. A Task whose `sessionRef.throughMessageId` names any
message other than the Session's latest fails before it binds the Session, with
reason `NativeSessionHistoryBoundaryUnsupported`, because restore cannot stop
at an earlier message.
A capture intent that reaches the runtime only after it expired is reconciled
against the runtime's own record. When the same runtime incarnation never
recorded that capture, it reports `native_capture_not_started` and the
controller begins one fresh capture under a superseding intent instead of
reconciling indefinitely.
A changed canonical boundary makes an older checkpoint unavailable for restore
or export.
If a native-continuity Session cannot produce a supported checkpoint, Orka
retains the runtime evidence and blocks finalization. It does not silently start
a new provider session. An ordinary Session with no native checkpoint may use
canonical continuity after a terminal format or size rejection, but only after
the runtime proves process exit. Before a capture intent or native checkpoint
exists, canonical recovery also remains available after the exact runtime has
been retired or replaced. A surviving native-capable runtime still produces
its first checkpoint.

Normal pool drain preserves resident native evidence until the controller
commits the checkpoint and deletes the runtime. Uncommitted native state uses
the runtime Pod's temporary storage; a forced Pod loss can destroy it. Recovery
with native continuity or a capture intent then blocks on the missing exact
runtime or capture receipt.

Native bundles are private Session data, accessible only through the migration
endpoint. Export requires `get` on `core.orka.ai/sessions`. Import requires
`create` on the Session collection and `get` on the requested name, plus the
corresponding context-token Session scopes when enabled. Session deletion removes
the native bundle and its receipts with the existing cleanup flow.

## Abrupt-crash verification

The subprocess regressions use explicit barrier acknowledgements and SIGKILL,
not graceful shutdown or returned persistence errors. They cover completed
capture before checkpoint commit, committed checkpoint before runtime deletion,
and supervisor death after capture admission but before snapshot publication.
They use scripted ACP responses, without live inference.

The controller test runs the production dispatcher in subprocesses. It keeps
the Kubernetes API and runtime fixture outside the killed process and reopens
the same SQLite database in a replacement process.
It checks the prior checkpoint, canonical boundary, immutable capture receipt,
epoch fences, prompt counts, and exact-generation cleanup. By default it uses
an isolated envtest API server. To exercise a disposable kind API, run from the
VM checkout with a kindctl-scoped kubeconfig:

```bash
ORKA_NATIVE_CRASH_KUBECONFIG="$(.agents/skills/kindctl/bin/kindctl path --tag native-crash)" \
  go test ./internal/controller \
    -run '^TestACPDispatcherNativeSessionProcessCrash$' -count=3 -v
```

The supervisor test is Linux-only and requires root plus the production ACP
exec helper from the pinned runtime image. Its parent kills the subprocess at
an acknowledged in-progress capture and checks the missing success receipt,
unchanged committed bundle, private evidence, and replacement-boot fencing.
It pins the ACP child with a pidfd and proves child exit after supervisor death
before any failure-path test cleanup signals the child:

```bash
./supervisor.test -test.run '^TestSupervisorNativeCaptureProcessCrash$' -test.count=3 -test.v
```

Run these checks only in disposable test environments. They do not make
uncommitted runtime storage durable or enable automatic recovery of a missing
capture receipt after forced runtime loss. That case still fails closed.
