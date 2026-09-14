---
description: "The orka.harness.v2 contract an external agent runtime must implement to work with Orka."
---

# AgentRuntime adapter contract

`orka.harness.v2` is the only supported Orka-facing contract for ACP runtime supervisors. It is session-centric, uses `/v2/...` paths, and fails closed on controller/runtime version skew. The old turn-oriented adapter surface is not supported.

## Scope

Built-in Kubernetes RuntimePools and external `AgentRuntime` registrations share the portable v2 identity, request, event, cancellation, duplicate, and fencing rules. Pool creation, Kubernetes scaling, exact-Pod routing, NetworkPolicy, and rollout are built-in controller behavior rather than portable adapter operations.

The controller validates external registrations and records observed
capabilities. `Agent.spec.runtime.runtimeRef` Task planning admits only a
current-generation ready, strict-governed registration whose frozen profile,
endpoint, authentication authority, and observed instance still match.
Registration drift fails closed before a runtime mutation.

## Probe and control endpoints

Safe unauthenticated probes:

- `GET /v2/health` — liveness only; no session-sensitive data;
- `GET /v2/capabilities` — static protocol/profile identity, limits, provider capabilities, and workspace-governance claims.

Authenticated control operations:

- `GET /v2/status` — exact instance fence, lifecycle, drain/admission state, resident sessions, prompts, permissions, descendants, and bounded pressure metadata;
- `PUT /v2/drain` — atomically stop admission of new RuntimeSessions.

External runtimes advertise whether they implement the drain extension. All status and mutation operations require controller authentication and operation-scoped authorization. Mutations present an exact-fence operation capability; status presents a status capability (audience `orka.harness.v2/status`, expiry-bounded, signed with the same operation-capability secret) because status is the channel through which the controller first learns the runtime-generated fence components. Conformance rejects runtimes that serve status on the controller bearer alone.

## RuntimeSession operations

- `PUT /v2/runtime-sessions/{sessionID}` — create one provider process and initialized ACP session;
- `PUT /v2/runtime-sessions/{sessionID}/prompts/{promptID}` — start one prompt and return a bounded, non-reconnectable NDJSON stream;
- `PUT /v2/runtime-sessions/{sessionID}/prompts/{promptID}/lease` — renew the bounded prompt lease;
- `PUT /v2/runtime-sessions/{sessionID}/prompts/{promptID}/permissions/{requestID}` — resolve one ACP permission request;
- `PUT /v2/runtime-sessions/{sessionID}/prompts/{promptID}/cancel` — cancellation barrier that waits for settlement or forced termination;
- `PUT /v2/runtime-sessions/{sessionID}/workspace-deltas/{deltaID}` — after prompt settlement, freeze mutations and produce one durable validated workspace delta;
- `PUT /v2/runtime-sessions/{sessionID}/publication-finalization` — record what Orka did with that delta, moving the session from `PublicationPrepared` to `Finalizing`;
- `DELETE /v2/runtime-sessions/{sessionID}` — cancel, prove descendant cleanup, remove session state, and return idempotently.

Publication finalization closes the loop on a write Task. The runtime produces the delta but
never pushes it; the Workspace/Publisher does that, and this call is how the runtime is told
the resulting publication identity, generation, version, and terminal receipt digest. It is
duplicate-safe: replaying the same receipt returns the recorded one, and a *different* receipt
for a session already finalizing is a `digest_conflict`, not an overwrite.

Provider conversation restoration uses the optional creation extension below.
Do not add separate provider-session load, prompt replay, stream reconnect,
transparent recovery or workspace checkpoint endpoints.

### Native conversation creation extension

`supportsNativeSessionRestore` advertises support for an explicit
`nativeRestore` field on the existing RuntimeSession creation request. It is
currently used only by Orka's pinned OpenCode 1.18.9 supervisor on a selected
durable agent-sandbox class. Unsupported and external runtimes receive their
existing creation request without this field.

`nativeRestore` authorizes one immutable `snapshotID` and a private `snapshot`.
The snapshot binds the owning Session UID, provider kind/version, native
conversation ID, runtime-profile digest, absolute working directory and
workspace content digest. Its compressed data and SHA-256 digest are covered
by the creation request's existing canonical integrity check and exact-fence
operation capability. The payload is bounded to 512 KiB before JSON base64
encoding. Native restoration requires read intent and is mutually exclusive
with a transcript bootstrap artifact.

The supervisor creates a fresh provider process, imports only selected
conversation rows, and regenerates credentials, provider settings, MCP
connections and tool policy. It applies the current model and mode through
the pinned ACP configuration methods. It prefers an advertised
`sessionCapabilities.resume` object, then advertised `loadSession` support.
It never guesses support or probes an unadvertised method.

Load notifications are confined to a restoration phase with a 60-second
deadline, at most 4,096 notifications, the normal ACP line limit and a 32 MiB
aggregate limit. The client validates each notification's session identity,
discards historical output and accounting, and rejects permission requests.
Restoration cannot invoke the prompt handler or authorize model/tool work.

The creation response reports `nativeRestoration.snapshotID`, `method` and a
bounded reason. A successful method is `session/resume` or `session/load`, and
the descriptor must retain the requested native conversation ID. A safe
fallback reports `reconstructed`; the controller then supplies allowed
canonical history with the new prompt. A failed restoration permits that
fallback only after process and descendant cleanup is proven. Unproven
cleanup poisons the runtime instance. A response cannot claim restoration
when the request did not authorize it.

Authenticated status includes the creation Task UID/attempt, native
conversation ID and restoration result. These fields let the controller
adopt an interrupted create without confusing an empty conversation with a
previously used one. Public health and capability probes expose none of this
Session data.

The read workspace-delta request may set `captureNativeSession`. After
successful prompt settlement and unchanged workspace validation, the response
may include a private `nativeSession` snapshot or a safe
`nativeSessionReason`. Capture occurs inside the same proven freeze and
ownership barrier. The controller stages the copy in its private Session
store; transcript commit and completion of Session finalization are required
before it becomes available for another creation request. These bytes are
never workspace artifacts or public checkpoint data.

Run the real Linux image checks without external provider credentials:

```bash
scripts/opencode-native-restore-e2e.sh
```

This runs both methods against the pinned binary with local model and MCP
fixtures. The load case suppresses only resume's capability advertisement.
The tests verify unique earlier tool results, fresh credentials and policy,
no restoration work, no transcript/accounting duplication, and cleanup.
To exercise normal Task completion, DataOnly suspension and cold Pod
replacement on the selected class, use the isolated sandbox E2E:

```bash
ORKA_AGENT_SANDBOX_ACP_TASK_SMOKE=0 \
ORKA_AGENT_SANDBOX_SUSPEND_RESUME=0 \
ORKA_AGENT_SANDBOX_LIFECYCLE=0 \
ORKA_AGENT_SANDBOX_NATIVE_SESSION=1 \
bash scripts/live-agent-sandbox-e2e.sh
```

The sandbox fixture checks that the follow-up receives an earlier unique
native `read` result without canonical reconstruction or another tool call.
The authenticated observer verifies native conversation identity under a new
runtime instance, Session generation and credential digest. The script uses
its own kubeconfig and deletes the cluster when it created it.

## Request identity and fencing

Every mutation is bound to:

- runtime instance ID and supervisor boot ID;
- controller epoch;
- RuntimePool UID and generation when pool-backed;
- RuntimeSession UID and generation;
- immutable runtime-profile digest and digest schema version;
- Task UID, attempt, prompt ID, operation ID, and request digest where applicable;
- expiry.

External supervisors must also report the current Orka controller epoch from
authenticated status. The supervisor reads `ORKA_ACP_CONTROLLER_EPOCH` once at
startup, so its operator must restart or replace it after each controller epoch
change. Preserve the registered runtime instance ID and rotate the supervisor
boot ID. A stale epoch fails conformance and dispatch admission.

A stale fence or digest conflict is a terminal protocol error for that request. An exact duplicate returns the recorded operation state without repeating the side effect. If prompt acceptance is known but the terminal result is not provable, Orka classifies the attempt as outcome unknown rather than replaying it.

## Event stream

Prompt responses use NDJSON. Every line carries the immutable prompt identity and a monotonic sequence number. Implementations must enforce the advertised request, line, result, buffer, update-rate, lease, permission, and workspace-delta limits.

Independently of those advertised limits, `orka.harness.v2` enforces a fixed, non-negotiable ceiling of 32 MiB of encoded update-event JSON per one-second window. This protocol invariant is not advertised or negotiated and applies in addition to all advertised limits.

Terminal events are mutually exclusive:

- prompt completed;
- prompt failed;
- prompt cancelled;
- prompt outcome unknown.

Assistant updates are diagnostics, not durable authority. Orka owns canonical Task outcome, approvals, external-effect records, workspace validation, publication, and result projection.

## Workspace governance profiles

An external runtime may claim strict `read` or `write` support only if it advertises and passes conformance for all of these guarantees:

- Orka-owned workspace delta production;
- prompt-scoped broker authorization;
- no direct SCM publication capability during prompts;
- Orka-owned clean-room publication;
- exact-instance fencing;
- duplicate-safe mutations;
- cancellation settlement.

A `trusted-non-governed` registration is an explicit operator escape hatch. It must not claim strict workspace guarantees and cannot satisfy a Task that requires them.

## Security boundary

Adapters receive safe tool schemas and operation-scoped authority, never downstream Tool credentials or Git publication credentials. Runtime processes must not have direct SCM publication egress. The separate Workspace/Publisher performs source clone, deterministic commit preparation, exact-ref publication, independent verification, and optional PR reconciliation.

RuntimeSession creation carries the canonical MCP tool and approval policy.
The provider child may discover that policy while idle, but every execution
must traverse a credential-protected loopback proxy and the Orka controller
broker. Each call binds the RuntimeSession, Task UID/attempt, prompt ID, lease
generation/expiry, runtime fences, tool descriptor, arguments digest, and
approval evidence. Prompt settlement, cancellation, lease expiry, poisoning,
and deletion revoke the authority and cancel in-flight calls. Consequential
calls reserve a durable `ExternalEffect` identity and may replay only a
committed matching response.

Git/forge operations are outside this broker. The Workspace/Publisher obtains
frozen source-read, target-read, target-write, and forge credentials from the
controller credential broker and obtains artifact bytes through short-lived
artifact capabilities. Neither broker grants session-wide authority.

Runtime children must start from an allowlisted environment with private `HOME`, `TMPDIR`, XDG paths, workspace, process group/session, and unique UID/GID. The supervisor must reap all descendants and poison the runtime instance when cleanup cannot be proven.

## Registration

An external v2 registration pins the endpoint, two controller-side auth references, exact runtime/profile identity, limits, drain support, and governance mode. Start from `config/samples/core_v1alpha1_agentruntime.yaml` and make every declared capability match the runtime's responses exactly.

## Conformance

The reusable conformance implementation lives under `internal/harness/v2/conformance`. It checks protocol identity, endpoint safety, authentication, mutation capabilities, duplicate handling, fencing, cancellation, bounded event streams, workspace-delta behavior, and governance claims. A registration becoming Ready records the exact matching generation, instance/profile, auth Secret versions, workspace intent, and governance surface. `runtimeRef` Task dispatch admits only that observed generation and freezes the matching endpoint, authentication authority, profile, policy, instance, and controller epoch into the execution binding. Any later drift fails closed before another runtime mutation.
