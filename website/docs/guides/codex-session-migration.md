---
title: Codex session migration
description: Move supported native Codex conversation state between local homes and Orka Sessions.
---

# Codex session migration

`orka session migrate` preserves Codex's original thread UUID and rollout bytes.
It supports the `codex/paginated@0.160.0` profile on Linux and macOS. Legacy
rollouts and encrypted reasoning context are outside this delivery.

Orka transports at most 512 KiB of encoded bundle data. This is a consumer
transport limit, separate from SessionKit's native storage limits. A bundle
contains the verified manifest and rollout. Authentication files, native SQLite
databases, provider configuration, and process memory are excluded.

The controller and runtime must both support native Sessions. Use the pinned
Codex 0.160.0 / codex-acp 2.1.1 runtime image. Private checkpoints use the
controller's existing execution-snapshot encryption key with separate associated
data. Retain that key with the SQLite backup.

Deploy this schema on a new installation. Existing Orka databases are not
upgraded in place; see [Upgrading](../operations/upgrading.md).

## Import a local thread

Stop every Codex process using the source home before capture. `--source-stopped`
records that assertion; it does not terminate a local client. Use a new Session
name and a journal directory outside the source home:

```bash
orka session migrate import migrated-review \
  --codex-home /absolute/source/codex-home \
  --thread 01a10020-1222-76e3-977d-d5165792ae72 \
  --journal-dir /absolute/private/import-journal \
  --source-stopped
```

Import reserves a fresh non-Gateway Session. It allocates no runtime or Task
lease. An existing Session, including an empty one, cannot be overwritten.

Submit a normal Codex Task with `spec.sessionRef.name: migrated-review` and
`spec.sessionRef.create: true` for the first continuation. Use the Task's current
workspace and tool policy. During admission, Orka installs the bundle in
the child's fresh private `CODEX_HOME` before starting codex-acp. It uses
`session/load` with the original UUID and current cwd, provider, MCP tools,
approvals, and sandbox configuration. The runtime must prove loading the exact
bundle before Orka sends the new user prompt. Canonical history is not injected
again. The imported history stays native; the Orka transcript records new Tasks.

## Export a checkpoint

After a successful Task, Orka proves the provider and its descendants have exited
before capture. The checkpoint and canonical terminal result commit together
before the Session lease is released. The runtime home is deleted only after
that commit. The next Task cold-starts with the saved checkpoint and fresh
credentials.

Export uses the last committed checkpoint. It does not force an active Task to
stop. The checkpoint must still match the exact Session owner and canonical
transcript boundary.

```bash
orka session migrate export migrated-review \
  --codex-home /absolute/fresh/codex-home \
  --cwd /absolute/local/workspace \
  --journal-dir /absolute/private/export-journal \
  --codex-bin /absolute/codex-0.160.0
```

The destination must be a fresh isolated Codex home with directory mode `0700`.
The journal must be outside it and private, also with directory mode `0700`.
Authenticate the destination with
current local credentials and configure its provider before resuming the UUID.
Migration does not copy source authentication or source policy into authority.

## Retry and failure handling

Retain the journal and repeat the same command after an uncertain response. The
CLI reuses its saved bundle and operation ID. Export also reuses the same frozen
SessionKit install plan and receipt. Changing the target, bundle, or cwd with
that journal is rejected. Automatic port-forward retries use the Kubernetes
cluster and Service identity rather than the local port. Recreating the Service
requires a new journal; an explicit server URL must stay unchanged.

An uncertain runtime install retains its target and frozen journal and closes
pool admission. Exact create retries report that retained outcome without
allocating another home or starting a child. Automatic reconciliation of that
supervisor creation state is not part of this delivery.

Failed, cancelled, or lost provider prompts do not publish a new native
checkpoint. The runtime that ran them is poisoned and retired, so a Task that
appends to a Session with a native checkpoint settles its terminal marker with
the existing checkpoint carried forward: the checkpoint's transcript boundary
advances past the marker, the lease is released, and the next Task loads the
same native state. That state omits only the prompt that produced no result.
A successful prompt can retain its native state after delivery failure once
workspace finalization and canonical settlement complete. Poisoned workspace
validation failures cannot produce a native checkpoint; for appending Tasks on
a Session with a native checkpoint they retain the exact runtime evidence and
hold the Session lease until reconciliation. Accepted turns with
`sessionRef.append: false` leave the checkpoint unchanged and retire their
runtime before a later Task loads the saved state.
A capture intent that reaches the runtime only after it expired is reconciled
against the runtime's own record. When the same runtime incarnation never
recorded that capture, it reports `native_capture_not_started` and the
controller begins one fresh capture under a superseding intent instead of
reconciling indefinitely.
A changed canonical boundary makes an older checkpoint unavailable for restore
or export.
If a native-continuity Session cannot produce a supported checkpoint, Orka
retains the runtime evidence and blocks finalization. It does not silently start
a new provider thread. An ordinary Session with no native checkpoint may use
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
