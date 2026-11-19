# 58. Hold the controller-epoch interlock token across consecutive writes

Date: 2026-11-19

## Status

Accepted. The user approved this direction. It changes how the Kubernetes
control store pays for the controller-epoch mutation interlock and leaves the
fencing guarantee unchanged.

## Context

Every fenced control-store write runs under the controller-epoch mutation
interlock (`internal/store/kube/controller_epoch.go`):

1. take the process-wide mutation slot (a one-slot semaphore);
2. read the authoritative epoch Lease and check its epoch and holder against
   the write's fence, then read the `ControllerEpoch` object and check that it
   mirrors the Lease (same Lease UID, same epoch);
3. write a mutation token and a two-minute expiry onto the Lease with a
   compare-and-swap on its `resourceVersion`;
4. run the write;
5. remove the token with a second compare-and-swap, and free the slot.

The guarantee rests on one rule. `CompareAndSwapControllerEpoch` refuses to
advance the epoch while an unexpired token is on the Lease, and every token
write changes the Lease's `resourceVersion`. So a write that runs while its
token is live cannot overlap an epoch change. The write records the epoch and
the Lease `resourceVersion` it ran under (`setMutationStatus`).

That costs four API round trips per write: two GETs and two Lease UPDATEs. On
the kind/gVisor measurement cluster a GET takes about 30ms and an UPDATE about
110ms, so the interlock alone adds about 290ms to every fenced write. Round 7
live measurement attributed about 3.6s of every warm turn to Core work after
the turn starts. The Session plan alone takes about 1.0s, and it is three
fenced writes in a row: the attempt moves to SessionStarting, the Session
lease, and the turn opens. Reservation, the attempt transitions and the outbox
pay the interlock too, before and after the start. Our warm turn takes 8.0s;
kagent's whole turn takes 4.3s.

The fenced writes come in bursts from one process. Only the elected leader
holds the epoch and writes under it, and it builds one control store.

## Options considered

- **Hand the token from one write to the next (chosen).** The first write of
  a burst writes the token. The next write reuses it when the Lease is exactly
  the state that token write produced. The token is removed once the burst
  ends. Each write keeps every check except the token write, which its
  predecessor already made. Each write records the same kind of evidence as
  before.
- **Rotate the token with one UPDATE per write.** This replaces "remove, then
  write" with one compare-and-swap, so it costs three round trips instead of
  four. A held token costs two (two GETs). Rotation proves nothing that the
  `resourceVersion` match below does not already prove.
- **Hold one interlock across a turn's batch of writes.** This needs a new
  batch API threaded through the Session plan, reservation and outbox call
  sites, and it covers only the paths that adopt the API. The token hand-off
  gives the same saving to every caller, with no API change.
- **Batch the three Session plan writes under one acquisition.** This is a
  special case of the batch API and saves less.
- **Check the epoch against a fresher local view with a bounded-staleness
  proof**, such as a Lease `resourceVersion` observed within the renew window.
  This replaces mutual exclusion through compare-and-swap with a time bound.
  Under it, a takeover could commit between the check and the write unless
  every process trusted the others' clocks to a margin. That is a weaker
  guarantee, so this option is rejected.
- **Also skip the two GETs while a token is held.** A held token cannot be
  removed by anyone else, so the GETs are not needed to rule out an epoch
  change. That would save another ~60ms per write. But it would stop each
  write from noticing out-of-band changes to the Lease or the `ControllerEpoch`
  object, which it notices today. This is left for a separate decision.

## Decision

- A Kubernetes control store holds its interlock token between consecutive
  fenced writes. After a write, the store leaves the token on the Lease for a
  linger of 500ms. The next write that arrives in time reuses it.
- A reused token must be younger than a max hold of 5s, measured from when the
  store sent the token write. After that, the next write removes the token and
  writes a fresh one. The max hold is capped at 30s.
- Every write still takes the process-wide slot. It still reads the Lease and
  the `ControllerEpoch` object and runs every check a token write runs: the
  fence's epoch and holder, the Lease UID binding, and the mirror. A write
  reuses the held token only if all of these hold:
  - the fence is the same name, epoch and holder;
  - the token is the one on the Lease;
  - the Lease has the same UID and `resourceVersion` that the store's own
    token write produced;
  - the token is younger than the max hold.
- If the Lease carries the store's own token but it cannot be reused, because
  it reached the max hold (even while the write was reading) or because the
  Lease changed in any other way, the write removes that token and then
  acquires as without a hold. It never writes over its own token. So every
  token's age counts from a gap, nothing can extend a hold past the max hold,
  and continuous writes always leave a successor a gap between tokens. If
  anything else differs, the write proceeds exactly as without a hold.
- A write that fails to acquire removes any held token it did not reuse or
  write over.
- The held token is removed by any of these:
  - the idle timer, when the linger ends without a write. The timer only acts
    while it holds the slot, and if the slot is busy it checks again shortly;
  - the next write, once the token is older than the max hold or the fence
    differs;
  - the store itself, before it changes the epoch
    (`CompareAndSwapControllerEpoch`);
  - the process, on the way out (`ReleaseHeldControllerEpochInterlock` after
    the manager stops).
- A write records the epoch and the Lease `resourceVersion` it ran under, as
  before. Consecutive writes under one held token record the same
  `resourceVersion`. That version is still a Lease state that carried this
  store's live token for the write's epoch.
- The hold is on by default. `WithControllerEpochInterlockHold(0, 0)` turns it
  off and restores an immediate release after each write.

## Why the guarantee is unchanged

The claim is that **no write commits under a stale epoch, and every write's
recorded Lease version carried a live token for the write's epoch.**

A reused write reads the Lease. It sees the same UID, the same
`resourceVersion` and the same token that this store's own compare-and-swap
produced. Every Lease write changes its `resourceVersion`. So nothing has
written the Lease since that token write: no epoch change and no other token.
The checks a token write runs then pass on the same state. The token was
written under two minutes ago and less than the max hold ago (at most 30s), so
it is live and remains live through the 20s mutation bound with at least 70s
to spare (95s at the default 5s max hold, against 100s for a fresh token
today). Until it expires, no other writer can remove it, because:

- an epoch change refuses a live token;
- a token write refuses a live token that is not its own;
- a token removal only removes its own token.

So no epoch change can commit before the reused write finishes. That is
exactly what holds after a fresh token write. A fresh write's checks run
before its compare-and-swap, so it, too, runs under a state it verified once.

- **Crash.** A process that dies holding a token leaves it to expire, the same
  as a process that dies in the middle of a write today. The successor's epoch
  change retries for three minutes, which covers the two-minute expiry plus
  the max hold. A restarted process holds a new holder ID, so its writes under
  the old fence fail closed. The hold makes it more likely that a crash lands
  while a token is on the Lease (during a burst or its 500ms linger), but it
  does not lengthen the wait. A clean shutdown removes the token.
- **Leader takeover.** A successor cannot change the epoch while a held token
  is live. It waits for the linger, or under continuous writes for the max
  hold, at which point the writer removes its token before writing a fresh
  one. That gives the successor the same window between tokens that
  back-to-back writes give it today. The successor can wait at most the max
  hold, which is far within its three-minute retry. Once it has changed the
  epoch, the old process's next write reads a changed Lease. It does not reuse
  its token, fails the fence check, and fails closed.
- **Epoch change mid-batch.** While the batch's token is live, an epoch change
  cannot commit. It can commit only after the token is gone (removed or
  expired). An expired token can stand only if the writer stalled for more
  than 70s. In that case the change rewrites the Lease, so the next write of
  the batch sees a different `resourceVersion` and epoch and fails closed. It
  never reuses the token, and it leaves no token on the new epoch.
- **Stalled API server.** A write's reads and its token write block exactly as
  before, and the age bound is measured from before the token write was sent.
  A token whose write had an ambiguous outcome is not held: the error returns
  before anything is parked. As today, it expires on its own. A write that
  outlives its bound can still commit late on the server, which is the
  existing limit of the interlock stated on `WithControllerEpochMutation`.
- **Concurrent writers.** In one process, the slot still runs one fenced write
  at a time, and the slot owner is the only one that can take or park the
  token. Across processes, a held token is a live token, so other stores wait
  for it or fail with `ErrControllerEpochMutationContention`, exactly as they
  do while a write runs today. Only the leader writes under its epoch, so in
  practice nobody else contends.
- **Clock skew.** Other processes judge a token's expiry by their own clocks,
  as today. The margin for skew shrinks from about 100s to 95s at the default
  max hold, and to 70s at the 30s cap.

## Measurement

`TestControllerEpochInterlockWriteCost` runs three consecutive fenced writes
against a fake API server that delays each GET 30ms and each UPDATE 110ms:

| | write path per write | 3 writes | round trips per write |
| --- | --- | --- | --- |
| Before (no hold) | 291, 292, 297ms | 879ms | 4.0 (2 GET + 2 UPDATE) |
| After (hold) | 179, 63, 64ms | 306ms | 2.3 (2 GET each, 1 UPDATE for the burst) |

The token removal happens after the burst, off the write path.

## Consequences

- A burst of N fenced writes costs one token write plus two GETs per write,
  instead of four round trips per write. On the measurement cluster the
  Session plan's three interlocks drop from about 0.9s to about 0.3s. Every
  other fenced write in the turn saves about 230ms.
- An epoch change by a successor may wait up to the linger after the last
  write, or up to the max hold under continuous writes. A crash is now more
  likely to leave a token that the successor must wait out (at most two
  minutes, as before).
- Records written back to back under one held token share the Lease
  `resourceVersion` in their fence evidence. Nothing compares that field
  across records, and it remains a version under which the record's epoch held
  a live token.
- Out-of-band changes to the Lease or the `ControllerEpoch` object are still
  caught by the next write, because the reads stay.
- The hold lives in one store instance. A second store in the same process
  gets no benefit and contends like another process. The controller builds
  one.
