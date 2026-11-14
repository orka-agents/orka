# Gateway-held root Task passes

Gateway pass tools support root Tasks on ACP runtimes. Child attenuation is **not implemented**: children, native AI workers and unbound Tasks cannot use `http.gatewayTaskPass` tools. Unsupported bound-tool configurations fail permanently; pending durable UID linkage remains retryable. Ordinary tools are unchanged.

## Admission and deployment

Send `eventType: delegatedText` using `orka.gateway.v1`, with nonempty metadata `gatewayGrantRef`. The GatewayClass must explicitly allow that metadata key. Ordinary `text` events cannot carry it. The reference identifies this request and its immutable sender/account generation; it is not a credential. References that metadata sanitization would rewrite are rejected rather than silently changed. Keep the existing synthetic gateway `requestedBy` identity.

The controller resolves authority from the retained event linked to the server-assigned Task UID, not mutable Task markers or a shared Session. Pending UID linkage defers binding. Its existing immutable execution snapshot freezes the reference, namespace/Gateway UIDs, gateway origin, named gateway tools and an absolute deadline: the earlier of event expiry and Task timeout measured from creation. No pass is persisted.

Configure `--gateway-task-pass-credentials-file` with a controller-only mounted JSON object mapping Gateway UID to an operator-pinned origin and dedicated credential: `{"gateway-uid":{"endpoint":"https://gateway.example","token":"dedicated-controller-credential"}}`. Issuance refuses if the live and frozen origin differs from this operator-owned entry; editing a Gateway while retaining its UID cannot redirect the credential. The file is loaded at controller startup; restart the controller to reload it. The gateway authenticates that credential as the particular target Orka; it is **not** the delivery bearer or a Task/Tool Secret. The operator must store it somewhere worker service accounts cannot read, for example a controller-only non-API volume or secret provider. Merely mounting a Kubernetes Secret only into the controller does not hide its API contents: do not back this file with a Secret in any worker-accessible Task namespace. This feature changes no cluster RBAC. Missing credentials refuse; there is no static/provider fallback.

A marked Tool uses `http.gatewayTaskPass: true` and a fixed HTTPS POST URL exactly `<authenticated-gateway-origin>/v1/task-tools/<toolName>`. It cannot configure headers, authSecretRef, outbound policy, body auth, MCP, forwarding, query parameters or URL interpolation. Generic health probes are skipped; readiness is checked under Task authority per call. The existing Gateway endpoint policy governs issuance and execution transport; redirects are refused.

## Gateway contract

For each call the controller makes HTTPS `POST /v1/task-passes` at the frozen origin, authenticated only by the dedicated target credential. The JSON body contains:

```json
{"protocolVersion":"orka.gateway.v1","grantRef":"opaque-request-generation-ref","namespaceUID":"namespace-uid","gatewayUID":"gateway-uid","taskUID":"task-uid","tools":["lookup"],"deadline":"2026-01-01T12:00:00Z"}
```

The gateway must durably bind the reference's first target/namespace/Gateway/Task UID, tool set and deadline, refuse conflicting bindings and sender/account-generation changes, and allow reacquisition of only that same binding after restart. Never relink a running Task to a newly linked account.

Return HTTP 200 with `{"pass":"opaque-bearer","expiresAt":"2026-01-01T11:59:00Z"}`. The response is limited to 16 KiB; the pass is 1–4096 bytes using only ASCII letters, digits and `-._~+=`. Slashes are excluded because JSON may escape them as `\/`. This alphabet keeps standard encoded responses covered by existing bearer redaction. Expiry must be in the future, no later than the frozen deadline, and at most five minutes from controller receipt. Issuance has a 30-second bound; no issuance retry is added. Unsupported responses, redirects and gateway error bodies yield a fixed refusal without exposing response content or tokens.

The existing HTTP executor sends reviewed JSON arguments to the fixed tool endpoint, injecting only the ephemeral pass as `Authorization: Bearer`. The existing operation identity remains `Idempotency-Key`; transaction tokens and static credentials are not forwarded. The gateway checks target, Task, tool, account generation, expiry and deadline on **every** call and selects provider credentials itself. Arguments must never choose identity, credential, destination or headers. Existing broker policy, approval, effect-ledger/replay and liveness checks still apply; the controller also rechecks durable origin and Running Task state before issuance and send. Cancellation/completion prevents new calls; calls already started may finish. Discarding a pass locally is not remote revocation.

## Rollback

Older ingress rejects `delegatedText`. After rollback, an already persisted unbound event may still start an old Task. Marked tools have no static credential, so a gateway pass-only endpoint must refuse those unauthenticated calls. There is no global version floor or fallback to another credential.
