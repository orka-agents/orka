# ADR 0038: Validate repository monitor repairs before publishing them

Date: 2026-10-22

## Status

Proposed. Not implemented. Builds on the clean-room publication path and the
repository monitor validation image; it changes neither for Tasks outside
repository monitor repairs.

## Context

A repository monitor repair is an agent Task with write workspace intent. When
its turn ends, the runtime supervisor captures the workspace delta as an
`application/vnd.orka.workspace-delta.v1+tar` artifact, and the controller asks
the Workspace Publisher to commit that artifact onto the exact pull request
head and push it, compare-and-swap, to the head branch. Nothing checks that the
change builds or passes tests first.

`spec.validation.image` only checks reviews: the reviewer picks one command, and
Orka runs it in a separate container against the exact head, read-only, with a
deny-all NetworkPolicy. A broken repair is therefore pushed first and caught,
at best, by the next review.

A live run on orka-agents/agentkit#75 showed the cost. The repairer had no Go
toolchain, so its first fix pushed a test that asserted the wrong line. Nothing
noticed until the following review, and the loop spent one of its three repairs
on undoing that. GitHub also held CI for the commits Orka pushed to the fork
until a maintainer approved them, so CI could not catch it either. Every tool we
surveyed that validates its own fixes (Copilot, Codex, Aider, OpenHands) runs
the checks before pushing, and OpenHands has the same split as Orka: the agent
edits in a sandbox and a fresh clone publishes the extracted diff.

Repairers can now mount a language toolbox and are told to run the checks, but
that is the agent reporting on its own work. The gate has to be something the
agent cannot fake.

## Decision

### Run the validation image on the head plus the repair's patch

When a monitor with `spec.validation.image` repairs a pull request, the
controller does not create the Publication when the repair Task's turn ends.
It first creates a repair validation Task:

1. **Image:** the monitor's validation image, pinned by digest as today.
2. **Checkout:** the exact pull request head the repair started from (the
   repair's `expectedRemoteSHA`), through the existing checkout init container
   and read credential.
3. **Patch:** a new Orka-owned init container downloads the repair's delta
   artifact and applies it to the checkout. It uses a single-use artifact
   capability bound to that artifact's digest, the validation Task's UID, and a
   short expiry, with download as the only operation. It applies the delta with
   the publisher's path rules: no paths outside the relative root, no symlinks
   that escape it, and repository control paths rejected when the repair's
   workspace denies them. A mismatched digest fails the Task before any user
   code runs.
4. **Network:** the existing deny-all NetworkPolicy gate, after the patch is in
   place and before the command starts.
5. **Command:** the operator's `spec.validation.repairCommand`, for example
   `make test`. The repairer does not choose it: it is the party being checked.
   Without a repair command, repairs are not gated, as today.

The command's output stays suppressed as it is for review validation. Only the
exit status and the existing bounded, redacted evidence are recorded.

### Publish only what passed

The controller records the validation result against the delta artifact's
digest. It creates the Publication only when a passing result exists for the
exact digest it is about to publish. If the repair session continues and
produces a different delta, that delta is validated again.

### Feed failures back, bounded by the existing retry limit

A failing validation does not push. The controller continues the repair session
with the failure evidence, up to `spec.repair.maxValidationRetries` times (the
field exists today and defaults to 2). When the retries run out, the repair job
fails with `repair_validation_failed`, nothing is pushed, and the pull request's
readiness status says why. A validation Task that cannot run, such as an image
pull failure, fails closed the same way, recorded as `unavailable`.

## Alternatives considered

- **A brokered check tool in the repair session.** The runtime supervisor would
  run the checks in the repair workspace and record the exit status against the
  delta. It is closer to Copilot and Aider, but it changes the harness protocol
  and process isolation, and the checks would run in an environment the agent
  controls.
- **Push to a staging ref, validate from Git, then fast-forward the branch.**
  It reuses Git as the transport, but it writes unvalidated code to the
  contributor's fork or the base repository first, can start CI there, and
  depends on custom ref namespaces GitHub may not accept.
- **Run validation inside the Workspace Publisher.** The publisher is a clean
  room that never executes repository code; giving it arbitrary execution
  removes the property that makes it trustworthy.
- **Trust the repairer's toolbox and instructions.** Kept as a complement,
  since it catches most failures before the gate runs, but it is not a gate.

## Consequences

- Each repair costs one more container run before it is pushed, and a failing
  repair costs up to `maxValidationRetries` more agent turns.
- Operators must set `spec.validation.repairCommand` for the gate to apply, and
  the validation image must contain everything that command needs, offline.
- A new artifact capability role lets a validation Pod download one delta. It
  is scoped to one digest and one Task and never permits upload.
- Repairs whose tests pass but are still wrong are left to the next review, as
  today; the gate catches build and test failures, not bad judgment.
