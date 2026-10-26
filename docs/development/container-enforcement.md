# Container enforcement

The optional enforcement boundary accepts one complete requirement on a Task.
It is not Agent configuration, provider routing, or permission to modify a
policy. Orka forwards the original policy bytes to operator-trusted providers.
Every configured mandatory provider must confirm enforcement, and together
they must cover every requested capability before workload release.

The managed native AI worker and managed ACP RuntimePools have enforcement
admission paths. External AgentRuntime endpoints, separately allocated execution
workspaces, arbitrary container Tasks, and read-only validation workers refuse
an enforcement request. Separate
`code_exec` Jobs, init containers, and remote tools are not covered by the
worker container's evidence. This boundary does not claim those targets.

## Task input

Both Kubernetes manifests and HTTP Task creation accept `spec.enforcement`;
flat HTTP requests accept `enforcement`. Its presence and complete contents
are immutable for the Task lifetime. There is no Agent inheritance or merge.

The following is a schema illustration, not a runnable provider policy:

```yaml
spec:
  type: ai
  enforcement:
    format: "<provider policy format>"
    policy: "<base64-encoded complete policy bytes>"
    requiredCapabilities:
      - "<provider capability identifier>"
    mode: enforce
```

Policy bytes are opaque, base64-encoded in JSON/YAML, and bounded to 512 KiB
after decoding. The format and capability identifiers belong to the provider
contract. Unsupported policies, profiles, or capabilities refuse; there is no
advisory or unprotected fallback.

The [shared native example](../../config/samples/enforcement/README.md) contains
one complete requirement file for standalone Orka and Kaimahi, plus standalone
Agent/Task resources. Its Task template fails closed until the requirement is
materialized; applying an incomplete example never means unprotected fallback.

Before creating a protected Task, clients may probe an absent Task through the
authenticated result API. This controller returns
`Orka-Task-Enforcement: container-v1` on that response. This is a protocol
declaration by the running binary, not a version/CRD/image-label inference,
provider readiness, or successful enforcement proof. Older controllers do not
declare it. Clients must also verify strict server admission preserves the
complete requirement; the ordinary controller/provider gates still decide
whether any workload can start.

`status.enforcementAdmission` records the admitted container binding ID, complete
requirement digest and observed Task generation together with
`EnforcementReady=True`. This is historical admission, not a live provider
check. The receipt survives physical retirement; `status.enforcementBindings`
remains the separate active cleanup responsibility and is removed only after
retirement. Neither the receipt nor a condition substitutes for fresh provider
proof on reuse. Clients should display only safe typed identifiers and current
conditions, not policy bytes or arbitrary provider status prose.

### Dashboard

For AI or Agent runtime Tasks, open **Advanced Options** in the ordinary Task
creation form and paste the complete requirement JSON into **Enforcement
requirement (JSON)**. Leave it blank for the unchanged ordinary path. The form
checks the envelope and size bounds, preserves the decoded opaque policy bytes
and capability order, and submits the same requirement to Task admission. It
does not infer a policy, merge Agent settings, configure providers or enable
enrollment. Switching to an unsupported container Task does not silently drop a
requirement: clear it explicitly or select a supported Task type.

Status details show current-generation enforcement conditions and abbreviated
validated historical admission identifiers. They do not claim a live provider
check or authority to detach controls. Provider prose is not displayed as an
enforcement observation, and the ordinary resource/YAML view always hides opaque
policy bytes.

### Actionable outcomes

Blocked admission also records optional `status.enforcementOutcome`: an
`observedGeneration`, one of `EnforcementPending`,
`EnforcementAdmissionPending`, `EnforcementRefused`, or `EnforcementUncertain`,
and bounded explanatory `needs` identifiers. These are guidance, never
authority to run, retry, change policy, or detach controls. Successful admission
clears this blocked-outcome field atomically with the admission observation.
Outcome publication is fenced to the original Task generation, physical
admission observation and execution attempt, including across status retries.

Known needs identify complete policy, supported format/runtime/target,
configured backend, complete mandatory capability coverage, prepared or stopped
containers, and provider/operator/admission recovery. Unknown provider text is
not copied into Task or execution messages. Missing mandatory capability
coverage is an explicit refusal with `mandatory-capability-coverage`, never a
partial authorization.

The dashboard and Kaimahi render fixed guidance only from a current-generation
outcome consistent with the blocked condition. Malformed or stale optional
detail leaves the Task readable and displays an unavailable-detail notice.
Legacy Task/execution failure messages associated with unconfirmed enforcement
are replaced by fixed guidance in dashboard status, runtime, timeline, graph
and YAML surfaces; unrelated scheduling conditions and ordinary or successfully
admitted workload messages remain unchanged. Historical receipts do not confer
live authority, and Kaimahi keeps answer-only stdout separate from diagnostics.

## Operator trust

### Shared optional installation

The ordinary chart generator stays dependency-free. A developer candidate can
vendor the actual standalone Guardd chart from a trusted local archive:

```sh
go run ./cmd/build/helmify \
  --output-dir /absolute/private/orka-chart \
  --enforcement-chart /absolute/private/guardd-0.1.0.tgz \
  --enforcement-chart-sha256 <independently-verified-lowercase-sha256> \
  < /absolute/private/generated-crds.yaml
```

No download or registry publication occurs. The generator bounds and verifies
the complete archive, checks a standalone Guardd application chart, and preserves
its exact bytes as a local dependency aliased `enforcement`. Do not manually
edit generated chart outputs or copy Guardd templates into Orka.

That candidate exposes `enforcement.enabled=true` and the standalone chart's
values, including its digest-pinned `enforcement.image`. An ordinary unbundled
chart refuses enabling the missing addon. Bundled controllers always retain a
whole optional, group-readable registry/client-identity projection, even with
enrollment disabled; only the trusted controller receives it. Their normal
`podSecurityContext.fsGroup` is required. Fresh disabled installations need no
provider graph or files to start; protected requests still fail closed until
valid provider authority arrives.

The management namespace is privileged. Do not grant workload service accounts
access to its Secrets, Jobs or installer identity. In particular, Orka's trusted
native AI worker role can read Secrets in its Task namespace: run Tasks in an
isolated, precreated `controller.watchNamespace`, not this management namespace.
Enabled or retained installations reject colocation. The chart places worker
identities, client authority and tenant admission there while keeping service
endpoints and controller control state in the release namespace. The ACP runtime
namespace must differ from both; split controllers reject RuntimePool overrides
that would move runtime-control credentials into a Task or management namespace.
Retain these boundaries after disabling enrollment. The ordinary default still
uses one release/Task namespace when no enforcement installation exists. An
existing release's watch scope remains immutable: drain and use a separate
installation rather than silently moving retained work.

For example, precreate `orka-tasks` and use
`--set-string controller.watchNamespace=orka-tasks` alongside the existing
distinct `controller.acpRuntime.namespace`. Authorized OIDC callers default to
the watched namespace unless `controller.oidc.namespace` is explicitly set.
Do not replace an installed bundled chart with an unbundled candidate; compatible
upgrades retain its projections even after enrollment is disabled.

Native container enforcement is not a
replacement for Kubernetes administrative isolation.

Use the shared chart's documented retained disable/uninstall/reinstall lifecycle.
Disabling new enrollment does not remove routes, credentials or cleanup
obligations. Roll back compatible versions through `helm upgrade`, not historical
manifest replay with raw `helm rollback`.

Start the controller with `--enforcement-provider-registry=<file>`, or set
`ORKA_ENFORCEMENT_PROVIDER_REGISTRY`. The file contains exact Node incarnations
and their mandatory providers:

```json
{
  "nodes": [{
    "nodeName": "worker-a",
    "nodeUID": "<actual-node-uid>",
    "providers": [{
      "name": "filesystem",
      "endpoint": "worker-a.provider.internal:7443",
      "serverName": "<node-incarnation-specific-certificate-DNS-name>",
      "caFile": "/var/run/orka/enforcement/ca.pem",
      "certificateFile": "/var/run/orka/enforcement/controller.pem",
      "keyFile": "/var/run/orka/enforcement/controller-key.pem"
    }]
  }]
}
```

This is controller-owned configuration, never Task or Agent input. The client
requires TLS 1.3, the configured server identity, and a controller certificate.
Node-local routes and server identities must distinguish Node incarnations;
reused addresses with a shared identity are not a safe retirement route.
Providers run outside the governed workload. Their credentials and privileged
host/runtime access must not be exposed to that workload.

The registry is loaded at controller startup and refreshed on provider lookup.
The shared optional installation instead sets
`--enforcement-provider-registry-projected` with an absolute registry path.
That explicit mode defers initialization so an absent Kubernetes projection
does not prevent ordinary controller startup. Missing, malformed or incompletely
projected registry/credential files still refuse every protected lookup; no
workload is released until valid authority arrives. Normal manual configuration
retains strict startup validation. Keep whole optional projections mounted after
disabling enrollment so existing bindings retain cleanup authority.

Live updates may only add Node incarnations; existing routes, credential paths
and loaded trust cannot change. Existing clients stay open for current calls
and cleanup. Invalid updates report an error without publishing a partial map.
A binding records its exact
Node UID and mandatory-provider/trust digest. Provider ordering does not change
the digest; changing an endpoint, server identity, or loaded CA does. Retain
old Node routes and trust until their physical bindings retire. Missing or
changed configuration preserves cleanup obligations and reports an error
rather than silently transferring authority.

Check the full serialized registry against its 1 MiB reader limit before
publishing an update. At capacity retain the last valid file and refuse new
enrollment, rather than pruning old routes. Removing retired history or
changing existing configuration requires an explicit operator-controlled
controller restart after the relevant cleanup obligations are resolved.

The controller reloads its client leaf certificate/key on each new TLS
handshake, without changing loaded CA trust or the provider-set digest. Use
whole projected Secret volumes, not `subPath` mounts. An unreadable or
mismatched replacement refuses that connection and surfaces ordinary RPC
uncertainty; it never releases a workload or retires existing controls.
Existing authenticated connections need no workload restart for renewal.

## Native admission and recovery

An enforced AI Job uses `/worker-enforced`. An older image without that
entrypoint fails instead of silently ignoring enforcement. The worker waits
before reading workload inputs or initializing models and tools. Its
authenticated Kubernetes watch requires a controller-written annotation bound
to its exact Pod UID, running container ID, and requirement digest.

The controller persists physical cleanup intent before invoking providers.
An admitted decision is the Task-status authorization point; the subsequent
exact-Pod optimistic patch publishes release. Existing Task timeout and
cancellation ordering remains authoritative. An RPC deadline reports
uncertainty and never expires protection or permits work.

The compact status binding distinguishes logical `admitted` from observed
`published`. Publication proves release authority was published, not that the
worker consumed it or completed successfully. A durable publication receipt
allows idempotent admission-event recovery after Pod removal. Lost publication
responses without any subsequent observation remain unknown; they are not
success.

A replacement physical worker cannot release until the previously selected
target has stopped and all its providers have confirmed retirement. Cleanup
requests ordinary Pod termination, but deletion or absence alone is not
physical stop proof. Providers must independently verify stop. Pending,
uncertain, or refused retirement retains the binding across retries, terminal
handling, and deletion. Event-store failures do not block termination, and
published receipts remain until their admission event is repaired.
An exact published container's termination does not reset historical admission
while the Job controller catches up. This neither releases a replacement
container nor removes the original container's cleanup obligation.

This native attempt fence does not introduce a one-session-per-container
restriction for ACP or change its existing capacity.

## ACP preparation protocol

The supervisor supports an additive prepare/launch boundary. The managed
dispatcher validates the required profile and target before session creation,
then verifies every mandatory provider before adapter launch or compatible reuse.
The protocol alone is not an enforcement claim.

A profile with `enforcementRequirementDigest` must use a prepare-only session
create. The digest separates incompatible pools without changing an ordinary
profile's canonical identity. Preparation materializes the workspace, baseline,
provider projection and private files, but starts no adapter or provider session.
The returned `prepared` session cannot accept prompts.

Task planning carries that digest through the RuntimePool profile, rendered
supervisor environment and deployed-template reconstruction. Ordinary pools omit
the additional environment variable and retain their existing identity. This
transport alone does not prove that a provider is active.

Physical cleanup intent belongs to the pool's single `enforcementBinding`,
not to consuming Tasks or sessions. A live incarnation retains that binding
through normal drain. The reconciler uses a fresh Kubernetes reader to avoid
retiring from an older cache observation; lookup failures retain intent.
Inactive bindings block replacement until every provider confirms retirement.
Pod absence alone is not physical-stop proof. Finalization retains its existing
barrier until provider retirement and an optimistic, exact-association status
update complete.

Clients opt into `supportsSessionPreparation` with
`X-Orka-Session-Preparation: 1`. Old strict clients do not receive the added
capability. Old runtimes cannot acknowledge a prepared session and must not be
used as an immediate-launch fallback.

After current enforcement verification, the controller can issue the
authenticated, capability-bound `PUT /v2/runtime-sessions/{sessionID}/launch`.
It binds the original preparation operation, Task attempt, session generation,
profile and supervisor incarnation. Exact replays return historical
acknowledgments without starting another adapter; they are not current
enforcement proof. A failed or uncertain launch never authorizes automatic
replacement within that session.

Prepared sessions are never adopted as prompt-admissible. If recovery cannot
replay their original preparation exactly, it retires them through normal
fenced deletion before recreating another generation. An uncertain applied launch
can recover the same Idle session, but must obtain fresh provider verification
before workload admission; it does not issue another launch. A pending admission
retains its durable cleanup obligation until verification succeeds. Permanent
refusal or consumer cancellation cleans that session, not the shared physical
container binding. Dependency/RPC deadlines mean uncertainty, not Task expiry.
Prompt-capacity rejection requeues through verification rather than bypassing it.

Durable checkpoints remain committed during preparation. Launch invalidates
the resumed marker immediately before adapter start and recommits only after
initialization succeeds. Native restore is not reported loaded before the
actual provider resume. Prepared deletion, drain and shutdown remove private
files and install journals without inventing a child-stop proof. In-flight
launches cannot use that no-child cleanup path.

Prepared sessions consume normal resident capacity. The default ten resident
sessions and four concurrent prompts remain unchanged; an allocated session
can launch after the UID reserve closes admission of new sessions.
