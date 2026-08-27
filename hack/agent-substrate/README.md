# Agent Substrate evaluation patches

Orka pins Agent Substrate at
`b80031d260959b1fc5c6f61e3099fe2a6d368af1` for local/CI evaluation. The
installer clones that immutable revision and applies the reviewed patches in
this directory before building Substrate. These are evaluation-only
compatibility and hardening patches; Orka does not install or manage Agent
Substrate in production. Patch-set changes are not rolled through a live worker
fleet: the retained-cluster marker binds the exact patch blobs and requires a
full cluster recreate, so mixed-version snapshot readers and rollback migration
are intentionally outside this local/CI evaluation contract.

Each patch is fail-closed:

- `scripts/agent-substrate-e2e.sh` verifies the exact upstream Git blob for
  every existing source file the patch changes.
- The patch must apply with `--whitespace=error-all` and reverse-apply cleanly.
- The parsed patch path set must exactly match its declared scope.
- Focused upstream package tests run before the Kind cluster is created.
- A different `SUBSTRATE_REF` is rejected unless the blobs and patches are
  explicitly reviewed and updated together.

## Patch set

### `atelet-root-supervisor-capabilities.patch`

Scopes `CAP_SETUID` and `CAP_SETGID` to the Orka workspace-agent supervisor and
keeps its extracted root filesystem traversable after the supervisor drops task
commands to UID/GID 1000.

### `atenet-router-authorization-redaction.patch`

Makes `atenet-router` request logging allowlist-only. Request metadata retains
only method, sanitized path, host/authority, and request ID; every other header
is discarded before its normal or `RawValue` content is read. Request-target
parsing keeps only the escaped path, including for legal absolute-form proxy
requests. Host/authority values are parsed independently and reject userinfo,
paths, queries, and fragments, so query credentials and absolute-URI userinfo
cannot reach logs or the status recorder. Upstream tests cover
`Authorization`, `Proxy-Authorization`, `Txn-Token`, `Cookie`, `X-API-Key`,
unknown `RawValue` headers, and query credentials. The patch also lowers Envoy's
`ext_proc`, router, and upstream component logging from debug to info in both
the static install manifest and the programmatic runner so the sidecar cannot
log raw request header tuples before the application-level allowlist boundary.
It disables the fixed ten-second xDS route timeout so quiet intervals in ACP
streaming responses do not terminate an otherwise healthy request. An upstream
xDS snapshot test requires an explicit zero timeout.

### `ateom-runsc-delete-recovery.patch`

Hardens the pinned `ateom-gvisor` checkpoint cleanup path:

- prepares a durable recovery directory and points the ordinary checkpoint path
  at it before `runsc checkpoint` writes any bytes;
- pins `flate-best-speed` so the stopped sandbox produces exactly one statefile,
  then validates that file with runsc's own `statefile` reader before cleanup;
- keeps committed recovery in its original one-file format, then materializes a
  separate upload view with a hard link to `checkpoint.img` plus explicitly
  marked `pages.img`/`pages_meta.img` compatibility files. Restore removes only
  those exact markers (including a surviving half of an interrupted cleanup)
  while preserving native multi-file snapshots even when pages are empty;
- reconciles an interrupted commit by checking the pause-container state: a
  valid statefile plus `stopped` is committed in place, while absent/partial
  bytes may be discarded only when the sandbox is still `created` or `running`;
- trusts checkpoint bytes only after atomically writing a deterministic commit
  inventory covering the statefile mode, size, and SHA-256 digest;
- stages the atomic commit temporary beside (not inside) the inventoried
  recovery directory, so a crash cannot turn its orphan into an extra artifact;
- runs every direct `runsc list` under the PID-1 child-reaper read lock and
  parses `runsc state` JSON from stdout separately from stderr diagnostics;
- restores the worker Pod network before fallible `runsc` state/delete cleanup
  and treats an already-restored interface as an idempotent retry state;
- converges across partial container deletion by reusing the durable checkpoint
  instead of checkpointing the stopped sandbox again;
- retries a failed `runsc delete --force` a bounded four times;
- after every failed delete, runs `runsc list --quiet` and accepts only an exact,
  verified absence of the target container;
- fails closed if checkpoint/container/network state cannot be verified;
- includes deterministic fake-`runsc` tests for crash-after-checkpoint recovery,
  two-call cleanup after retry exhaustion, exit-128-after-removal, transient
  retry success, persistent failure, and verification failure.

With `SUBSTRATE_E2E_EXTENDED=1`, the live E2E deletes an assigned worker and
verifies store/Deployment replacement, then installs a temporary fail-once
`runsc` delegator on a live replacement worker. The Actor must suspend through
the reviewed retry path, the original binary must be restored, and a subsequent
direct Actor lifecycle must route, execute, suspend, and delete without leaving
any Actor in `STATUS_SUSPENDING`.

## Validation

Fast repository checks:

```bash
bash scripts/tests/agent-substrate-patches-test.sh
bash -n scripts/agent-substrate-e2e.sh hack/demos/cluster/install-substrate.sh
```

The complete Linux/Kind validation is destructive to a same-named Kind cluster:

```bash
PATH="$(go env GOPATH)/bin:$PATH" \
SUBSTRATE_E2E_EXTENDED=1 \
KEEP_CLUSTER=1 \
bash scripts/agent-substrate-e2e.sh
```

Review and update the source blob constants in `scripts/agent-substrate-e2e.sh`
only after inspecting the replacement upstream source and regenerating the
corresponding patch.
