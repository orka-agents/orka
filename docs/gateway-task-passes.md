# Gateway-held Task passes

Gateway pass tools support root and delegated child Tasks on ACP runtimes. Children inherit only the intersection of their gateway tools and their UID-pinned parent's frozen tools, with no later deadline or different sender/account generation. Native AI workers, continuations and unbound Tasks cannot use `http.gatewayTaskPass` tools. Unsupported bound-tool configurations fail permanently; pending durable UID linkage remains retryable. Ordinary tools are unchanged. Child issuance also requires the gateway contract extension below; a root-only gateway refuses children.

## Admission and deployment

Send `eventType: delegatedText` using `orka.gateway.v1`, with nonempty metadata `gatewayGrantRef`. The GatewayClass must explicitly allow that metadata key. Ordinary `text` events cannot carry it. The reference identifies this request and its immutable sender/account generation; it is not a credential. References that metadata sanitization would rewrite are rejected rather than silently changed. Keep the existing synthetic gateway `requestedBy` identity.

The controller resolves root authority from the retained event linked to the server-assigned Task UID, not mutable Task markers or a shared Session. Pending UID linkage defers binding. Its existing immutable execution snapshot freezes the reference, namespace/Gateway UIDs, gateway origin, named gateway tools and an absolute deadline: the earlier of event expiry and Task timeout measured from creation. A child freezes its parent's namespace/name/UID and snapshot digest; parent live Agent policy never replaces that snapshot. No pass is persisted.

Child grants require existing Task provenance protection (`--task-provenance-admission-enabled` or `--task-provenance-admission-external`); otherwise child binding fails permanently, and already-bound children cannot acquire passes. Remove Tasks whose ancestry predates authenticated admission before enabling it, as those flags require. Root grants do not require this setting. An orchestrator with no frozen gateway tools cannot authorize children to use them.

A parent can be Running or successfully parked `Waiting` for this exact child UID by `park_until` with `kind: tasks`. `wait_for_tasks` waits within the active tool call; it does not durably park the turn. Parked authority requires the same successful execution attempt/prompt, an unexpired wait and chain deadline, and no cancellation or deletion. Failure, abandoned/expired waits and continuations carry no such authority. Binding after a parent parks further narrows the child's deadline; later wait changes are live validity checks, not rewrites of an immutable gateway binding. Final reply projection does not consume parked delegation, but neither children nor parked parents gain reply authority. Cross-namespace delegation retains the root gateway namespace UID; each executing Task's own namespace incarnation is separately verified. Parked waits reference same-namespace children; cross-namespace children require a Running parent.

Configure `--gateway-task-pass-credentials-file` with a controller-only mounted JSON object mapping Gateway UID to an operator-pinned origin and dedicated credential: `{"gateway-uid":{"endpoint":"https://gateway.example","token":"dedicated-controller-credential"}}`. Issuance refuses if the live and frozen origin differs from this operator-owned entry; editing a Gateway while retaining its UID cannot redirect the credential. The file is loaded at controller startup; restart the controller to reload it. The gateway authenticates that credential as the particular target Orka; it is **not** the delivery bearer or a Task/Tool Secret. The operator must store it somewhere worker service accounts cannot read, for example a controller-only non-API volume or secret provider. Merely mounting a Kubernetes Secret only into the controller does not hide its API contents: do not back this file with a Secret in any worker-accessible Task namespace. This feature changes no cluster RBAC. Missing credentials refuse; there is no static/provider fallback.

A marked Tool uses `http.gatewayTaskPass: true` and a fixed HTTPS POST URL exactly `<authenticated-gateway-origin>/v1/task-tools/<toolName>`. It cannot configure headers, authSecretRef, outbound policy, body auth, MCP, forwarding, query parameters or URL interpolation. Generic health probes are skipped; readiness is checked under Task authority per call. The existing Gateway endpoint policy governs issuance and execution transport; redirects are refused.

Expose a marked Tool by setting `brokeredToolClass` and listing it in the Agent's `runtime.defaultAllowedTools`. Codex cannot enforce a narrower native tool list, so outside read-intent workspaces a Codex Agent must also allow every Codex native tool (`Read`, `Write`, `Edit`, `Bash`, `Glob`, `Grep`, `WebSearch`, `WebFetch`) with `defaultAllowBash: true`; otherwise the Task fails before execution.

## Gateway contract

For each call the controller makes HTTPS `POST /v1/task-passes` at the frozen origin, authenticated only by the dedicated target credential. The JSON body contains:

```json
{"protocolVersion":"orka.gateway.v1","grantRef":"opaque-request-generation-ref","namespaceUID":"namespace-uid","gatewayUID":"gateway-uid","taskUID":"task-uid","tools":["lookup"],"deadline":"2026-01-01T12:00:00Z"}
```

The gateway must durably bind the reference's first target/namespace/Gateway/Task UID, tool set and deadline, refuse conflicting bindings and sender/account-generation changes, and allow reacquisition of only that same binding after restart. Never relink a running Task to a newly linked account.

For children the body additionally contains `"parentTaskUID":"parent-task-uid"`. Before child issuance, the controller establishes/reacquires the exact frozen ancestor bindings root-to-leaf using this same endpoint, even if the parent has never made a tool call. Unused ancestor passes are discarded; no ancestor tool executes. Root requests omit this field and retain their existing format.

The gateway follow-up must:

- Accept the optional parent UID and durably retain separate bindings by grant reference and Task UID, rather than replacing the root binding.
- Require an already-bound parent under the same authenticated target, root namespace UID, Gateway UID and immutable sender/account generation; enforce child tool/deadline subsets and reject cycles/conflicting parent links.
- Accept only identical reacquisition at every level after retries/restart. Never compare a cross-namespace child's execution namespace to the root `namespaceUID` in the request.
- Validate tool calls and write reservations/idempotency against the issuing child's binding, not the root's binding. Revalidate account generation on every issuance and call.

This Orka change does not implement gateway storage, provider access or that enforcement.

Return HTTP 200 with `{"pass":"opaque-bearer","expiresAt":"2026-01-01T11:59:00Z"}`. The response is limited to 16 KiB; the pass is 1–4096 bytes using only ASCII letters, digits and `-._~+=`. Slashes are excluded because JSON may escape them as `\/`. This alphabet keeps standard encoded responses covered by existing bearer redaction. Expiry must be in the future, no later than the frozen deadline, and at most five minutes from controller receipt. Issuance has a 30-second bound; no issuance retry is added. Unsupported responses, redirects and gateway error bodies yield a fixed refusal without exposing response content or tokens.

The existing HTTP executor sends reviewed JSON arguments to the fixed tool endpoint, injecting only the ephemeral pass as `Authorization: Bearer`. The existing operation identity remains `Idempotency-Key`; transaction tokens and static credentials are not forwarded. The gateway checks target, Task, tool, account generation, expiry and deadline on **every** call and selects provider credentials itself. Arguments must never choose identity, credential, destination or headers. Existing broker policy, approval, effect-ledger/replay and liveness checks still apply; the controller also rechecks the executing Running Task, all UID/snapshot-pinned ancestors, attenuation and durable root origin before/after each issuance and before send. Ancestor cancellation or an invalid/expired wait prevents new calls without waiting for child cancellation to be projected; ordinary completion ends authority. Calls already started may finish. Discarding a pass locally is not remote revocation.

## Rollback

Older ingress rejects `delegatedText`. After rollback, an already persisted unbound event may still start an old Task. Marked tools have no static credential, so a gateway pass-only endpoint must refuse those unauthenticated calls. There is no global version floor or fallback to another credential.
