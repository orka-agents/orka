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

For takeover liveness under continuous writes:

- **The holder pauses at least one successor retry interval, plus jitter,
  after removing a token before writing the next one (rejected).** This makes
  takeover likely but not certain. A successor's read and compare-and-swap can
  still straddle the pause, and the pause adds latency to the holder's writes
  every max hold.
- **A successor records that it is waiting, and the holder writes no new token
  while it does (chosen).** The request lives on a Lease only successors
  write, so recording it cannot be starved by the holder. The bound then
  follows from the write bound and the max hold (below).
- **The waiting request as an annotation on the epoch Lease (rejected).** It
  would compete with the holder's own Lease writes for the same
  `resourceVersion`, which is the race it is meant to settle.

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
- **Every fenced write is bounded by its token.** The interlock hands each
  fenced write the context it must use, and that context ends 60s (half the
  token's two-minute life) after the token covering the write was written,
  whatever context the caller passed. This holds on every acquisition path:
  `WithControllerEpochMutation` (which also keeps its own 20s bound) and all
  26 store methods that take the interlock directly, among them
  `FinalizeSessionTurn`, `ResumeSessionTurnFinalization`, the outbox, Session
  control, prompt attempt, publication, branch claim and external effect
  writes. Releasing the interlock cancels the context.
- **A waiting successor stops new tokens.** A successor records a takeover
  request: the epoch it waits to replace, its holder ID and the time. It does
  this before every attempt to advance the epoch, and in the epoch manager
  before its first epoch read and before each read that recovers from a
  conflict. It learns the epoch from a single read of the epoch Lease, which
  cannot conflict.
  - Recording it before the attempt matters. A successor that recorded it only
    after seeing a live token could keep reading a gap, lose its update to the
    holder's next token, and never ask.
  - Recording it before those reads matters too. An epoch read or change
    re-reads the Lease to check the `ControllerEpoch` mirror, and fails when
    its `resourceVersion` changed in between. A busy holder's token rollovers
    alone can change it every time. Those checks stay.
  - An epoch change publishes its request only while the Lease still carries
    the epoch it replaces. A stale change that has already lost cannot hold
    off the epoch that won. A change that committed with an ambiguous result
    cannot hold off its own new epoch either. The manager's conflict recovery
    publishes only against the epoch its failed attempt replaces, so a
    manager that lost to another holder leaves no request against the winner.
    Before its first read, the manager publishes against whatever epoch is
    current. It holds none yet, so that epoch is the one it replaces, even
    when a restart kept its holder ID.
    A manager read whose epoch Lease kept changing between the reads that
    check its status mirror, as a busy holder's token writes make it, is
    retried within the acquisition window, republishing the request each
    time, instead of ending startup after the store's few quick retries.
    Any other read error, including a stale or mismatched mirror, still ends
    acquisition at once. The request goes on a Lease of its own
  (`controller-epoch-takeover-*`), which only successors write, so it never
  competes with the holder's writes to the epoch Lease. The
  `acp-workspace-lease-protection` admission policy reserves that prefix for
  the controller's identity, as it does the admission ledgers: a forged
  request kept fresh would otherwise stop the holder's new tokens and stall
  every fenced write, and a removed one would let the holder starve its
  successor. An attempt rewrites it
  once the successor's own last write of it is 10s old on its monotonic clock,
  which gives the request a new `resourceVersion`. Once the epoch
  change commits, it deletes its own request against the epoch it replaced.
  A newer request against the epoch it created is left in place, even when it
  shares the holder ID, as a restart's does. No publisher overwrites a
  request against a newer epoch than the one it read, so a publisher delayed
  past another change cannot downgrade a newer successor's request.
- Before writing a token, a store reads that request. A request version is in
  force for 30s from when this store first saw it, on this store's own
  monotonic clock. No store compares the time a requester wrote into the
  request with its own clock, so neither side's skew can make a request last
  forever or not at all. While a request that waits to replace its current
  epoch is in force, the store writes no token, whoever made the request (a
  restart can keep its holder ID). It fails the write with
  `ErrControllerEpochMutationContention` instead. Reusing a held token does
  not read the request.
- A write that fails to acquire removes any held token it did not reuse or
  write over.
- A token whose removal fails is not forgotten. It stays parked, and the idle
  timer retries the removal with backoff (100ms doubling to 5s) until the
  token would have expired anyway. Such a token is never reused. A failed
  removal may still commit on the server, and its precondition is the
  `resourceVersion` it was sent against. Reuse is the only path that leaves
  the Lease at that version. A confirmed removal or a fresh token write
  changes it, so the stale removal can no longer commit.
  `ReleaseHeldControllerEpochInterlock` tries five times with backoff. If the
  removal still fails, it returns the error and leaves the token parked for
  the timer. Each removal attempt runs under its caller's context, capped at
  5s, so an explicit release never outlasts its caller's deadline. The idle
  timer's retries, which have no caller, are capped at 5s each.
- The held token is removed by any of these:
  - the idle timer, when the linger ends without a write. The timer only acts
    while it holds the slot, and if the slot is busy it checks again shortly;
  - the next write, once the token is older than the max hold or the fence
    differs;
  - the store itself, before it changes the epoch
    (`CompareAndSwapControllerEpoch`);
  - the epoch manager, when it stops on leadership loss or shutdown, and the
    process again after the manager stops (`ReleaseHeldControllerEpochInterlock`
    both times). So a clean restart or leadership handoff leaves no token
    behind.
- A write records the epoch and the Lease `resourceVersion` it ran under, as
  before. Consecutive writes under one held token record the same
  `resourceVersion`. That version is still a Lease state that carried this
  store's live token for the write's epoch.
- The hold is opt-in: `--controller-epoch-interlock-hold`
  (`ORKA_CONTROLLER_EPOCH_INTERLOCK_HOLD`), default off. Without it, the
  controller passes `WithControllerEpochInterlockHold(0, 0)`, so every write
  releases its token at once, as before this ADR. Takeover requests are
  published and honoured either way.
  - Why off by default: a controller from before this ADR publishes no
    takeover request. If such a controller is the successor (a rollback) and
    the holder runs with the hold on and keeps writing faster than the
    linger, the epoch Lease is free only for a few reads at each max-hold
    rollover. Against a real API server, that rollback pairing starved the
    successor for 20s in 4 of 5 runs with writes 200ms apart, and for a
    3-minute acquisition window with back-to-back writes. With the hold off
    on both sides it took over at once. A holder keeps writing only after it
    has lost leader election; a normal rollback stops the holder first, and
    then the successor takes over in about 0.1s either way.
  - Turn the hold on once no replica can roll back past this ADR. Takeover
    requests from this release onward keep a successor live under the hold.
  - Measured saving with the hold on: about 3 API calls per reused write,
    about 0.17s per ACP session plan on kind. That is below the estimate
    above, because a live Lease round trip cost about 25ms, not 110ms.

## Why the guarantee is unchanged

The claim is that **no write commits under a stale epoch, and every write's
recorded Lease version carried a live token for the write's epoch.**

A reused write reads the Lease. It sees the same UID, the same
`resourceVersion` and the same token that this store's own compare-and-swap
produced. Every Lease write changes its `resourceVersion`. So nothing has
written the Lease since that token write: no epoch change and no other token.
The checks a token write runs then pass on the same state. The token was
written less than the max hold ago, so it is live. The write's context ends 60s
after the token was written, so the token is still live, with 60s to spare,
when the write can no longer act. Until it expires, no other writer can remove
it, because:

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
  the old fence fail closed. A restart that keeps the same identity
  (`ORKA_CONTROLLER_HOLDER_ID`) is a new store with no held token. It cannot
  adopt its predecessor's token, so its writes under the old fence fail after
  the bounded acquisition wait. Its own epoch change waits for that token like
  any successor. The hold makes it more likely that a crash lands while a
  token is on the Lease (during a burst or its 500ms linger), but it does not
  lengthen the wait. A clean shutdown or a lost leadership removes the token.
- **Leader takeover.** A successor cannot change the epoch while a token is
  live. Gaps between tokens alone do not bound its wait: under continuous
  writes, the holder can write a new token right after removing the old one,
  and a successor's retries can keep missing the gaps. Without the takeover
  request, the test with slow successor reads starves for its full 10s. The
  request gives a bound:
  - Let R be when the request commits. After R, the holder writes no token,
    except one whose request read came before R.
  - Any token written at w ≤ R is reused only until w + max hold. Every write
    under it ends by w + 60s, its context bound. The token is removed at the
    holder's next acquisition or by its idle timer, both of which wait for the
    write in progress. So a live holder that can reach the API server clears
    the epoch Lease by R + 60s + linger. With the default 5s max hold and
    short writes, that is about R + 5.5s.
  - A holder that crashed leaves its token to expire, by w + 2 minutes.
  - The successor's next retry, at most 1s later, then commits the epoch
    change.

  Its three-minute acquisition window covers both cases. Once the epoch has
  changed, the old process's next write reads a changed Lease. It does not
  reuse its token, fails the fence check, and fails closed. A request from a
  successor that stopped trying lapses 30s after the holder first saw its last
  version, whatever time the successor's clock wrote into it. Until then, the old holder's
  writes fail with contention, which is what an old holder should get.
- **Epoch change mid-batch.** While the batch's token is live, an epoch change
  cannot commit. It can commit only after the token is gone (removed or
  expired). No write can still be acting at expiry, because of its 60s
  bound, so an expired token can stand only if the process stalled for longer
  than the token's life while it held the token. In that case the change rewrites the Lease, so the next write of
  the batch sees a different `resourceVersion` and epoch and fails closed. It
  never reuses the token, and it leaves no token on the new epoch.
- **Stalled API server.** Every fenced write's context ends 60s after its
  token was written, so a stalled write is abandoned while its token is live.
  The age bound and the write bound are measured on the monotonic clock from
  before the token write was sent. A token whose write had an ambiguous
  outcome is not held: the error returns before anything is parked. As today,
  it expires on its own. A removal that fails is retried until it succeeds or
  the token expires. A request already on the wire can still commit late on
  the server, which is the existing limit of the interlock stated on
  `WithControllerEpochMutation`.
- **Concurrent writers.** In one process, the slot still runs one fenced write
  at a time, and the slot owner is the only one that can take or park the
  token. Across processes, a held token is a live token, so other stores wait
  for it or fail with `ErrControllerEpochMutationContention`, exactly as they
  do while a write runs today. Only the leader writes under its epoch, so in
  practice nobody else contends.
- **Clock skew.** Other processes judge a token's expiry by their own clocks,
  as today. Every fenced write now has a uniform 60s margin for skew. Before
  this change, writes through `WithControllerEpochMutation` had about 100s,
  and direct callers had no bound at all. Takeover requests do not depend on
  wall clocks. The holder times each request version on its own monotonic
  clock from when it first saw that version. The successor renews the request
  on its own monotonic clock, every 10s while it tries. A successor 40s ahead
  or behind is neither starved nor able to stall the holder past the 30s.

## Measurement

`TestControllerEpochInterlockWriteCost` runs three consecutive fenced writes
against a fake API server that delays each GET 30ms and each UPDATE 110ms:

| | write path per write | 3 writes | round trips per write |
| --- | --- | --- | --- |
| Before (base branch) | 297, 298, 292ms | 887ms | 4.0 (2 GET + 2 UPDATE) |
| After (hold) | 208, 63, 65ms | 336ms | 2.7 (2 GET each, plus a takeover-request GET and an UPDATE for the burst's token) |

The token removal happens after the burst, off the write path. With the hold
turned off, a write now costs five round trips (322ms), because it also reads
the takeover request.

`TestInterlockHoldWaitingSuccessorIsNotStarvedByContinuousWrites` runs a
holder writing continuously against a successor whose reads are slowed. The
successor changes the epoch 291ms after it starts waiting. Without the
takeover request, it is still waiting when the test gives up at 10s.

## Consequences

- A burst of N fenced writes costs two GETs per write, plus one takeover-request
  GET and one token write for the burst, instead of four round trips per
  write. On the measurement cluster the Session plan's three interlocks drop
  from about 0.9s to about 0.34s. Every other fenced write in the turn saves
  about 230ms.
- A fenced write that runs longer than 60s after its token was written now
  fails with a context deadline, instead of running past the point where its
  token's protection ends.
- An epoch change by a successor waits at most about 61.5s after its takeover
  request when the holder is live, and about 5.5s for short writes at the
  default max hold. It waits two minutes after a crash, as before. A crash is
  now more likely to leave a token that the successor must wait out.
- A successor's takeover request stops the old holder's new tokens for up to
  30s after the holder first sees its last version. That bound holds
  whatever time the successor's clock wrote into the request.
- Records written back to back under one held token share the Lease
  `resourceVersion` in their fence evidence. Nothing compares that field
  across records, and it remains a version under which the record's epoch held
  a live token.
- Out-of-band changes to the Lease or the `ControllerEpoch` object are still
  caught by the next write, because the reads stay.
- The hold lives in one store instance. A second store in the same process
  gets no benefit and contends like another process. The controller builds
  one.
