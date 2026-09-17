# Runtime feedback for native ACP Tasks

The optional `runtime_feedback` MCP tool lets a native Codex or OpenCode agent
read Gatekeeper Runtime (GKR) network evidence after an operation fails. The
controller registers the exact worker container before submitting the prompt.
The tool reads that existing registration; it cannot start capture, change
policy, approve a destination, or retry an operation.

This feature requires GKR's `runtime.gatekeeper.sh/v1alpha1` runtime-feedback
service. GKR interprets enforcement evidence. Orka establishes which current
Task attempt may read it.

## Enable the controller client

For a single-node deployment, configure a fixed origin and the three mTLS flags:

```text
--acp-runtime-feedback-url=https://gkr-feedback.example.internal:9444
--acp-runtime-feedback-ca-file=/var/run/secrets/gkr-feedback/ca.crt
--acp-runtime-feedback-cert-file=/var/run/secrets/gkr-feedback/tls.crt
--acp-runtime-feedback-key-file=/var/run/secrets/gkr-feedback/tls.key
```

For a multi-node deployment, replace `--acp-runtime-feedback-url` with:

```text
--acp-runtime-feedback-node-urls-file=/var/run/secrets/gkr-feedback/node-urls.json
```

The operator-owned file is a JSON object mapping exact Kubernetes node names
to node-specific HTTPS origins:

```json
{
  "node-a": "https://gkr-feedback-node-a.example.internal:9444",
  "node-b": "https://gkr-feedback-node-b.example.internal:9444"
}
```

Each origin must reach only the GKR agent on its named node and match that
server's certificate. All mappings use the same configured CA and controller
client certificate/key. The fixed URL and file flags are mutually exclusive;
exactly one is required when feedback is enabled. The file is limited to
64 KiB and 256 entries. Empty, duplicate, malformed, or invalid node/origin
entries fail controller startup. Every origin uses the same HTTPS restrictions
as the fixed URL: no user information, path beyond `/`, query, or fragment.

The map is loaded once at controller startup. Registration, report, and
completion select only the exact node from the live Pod binding. An unmapped
node produces unavailable diagnostics before any transport; there is no
fallback origin or load balancing. A Task scheduled onto a newly autoscaled,
unmapped node can still run under its existing authority, with unavailable
diagnostics. Add the node's dedicated endpoint and restart the controller to
load a changed mapping. Merely updating the mounted file does not reroute a
live capture.

Mount the certificate material from an operator-owned Secret into the
controller only. The GKR service must allow the controller certificate's exact
URI SAN, for example `spiffe://orka.ai/controller`. Its server certificate must
match the configured HTTPS hostname and CA. Environment proxies and redirects
are disabled. Requests have a 15-second deadline and responses are capped at
128 KiB and 128 events. No service address or credential is supplied to an ACP
child.

The endpoint must select the GKR agent on the worker's actual node. A Service
that load-balances multiple node agents is unsuitable. GKR validates the node,
Pod UID, full CRI container ID, container name, and restart count against its
live Kubernetes/CRI view. A wrong-node or unavailable registration produces
unavailable diagnostics; it never borrows another node's evidence. Orka does
not deploy GKR or alter NetworkPolicies as part of tool registration. GKR
permits one active capture per node; concurrent opted-in Tasks on the same node
can receive unavailable diagnostics until that capture ends.

## Configure a Helm installation

The chart keeps feedback disabled by default. For `controller.mode: harness-v2`,
create an operator-owned Secret in the Helm release namespace containing the
GKR CA (`ca.crt`), controller client certificate (`tls.crt`) and private key
(`tls.key`). Keep those contents out of Helm values. Reference the existing
Secret for a single-node deployment:

```yaml
controller:
  runtimeFeedback:
    enabled: true
    url: https://gkr-feedback.example.internal:9444
    existingSecret: gkr-feedback-client
```

For multiple nodes, add the exact node-name-to-origin JSON map described above
as another item in the same Secret, and select it instead of `url`:

```yaml
controller:
  runtimeFeedback:
    enabled: true
    nodeURLsKey: node-urls.json
    existingSecret: gkr-feedback-client
```

Leave `url` empty in node-map mode. Exactly one route and all three mTLS item
selectors are required when enabled. Use `caKey`, `certKey` and `privateKeyKey`
if the Secret uses different item names. Helm passes the existing flags and
projects only the selected items into the controller at fixed paths under
`/var/run/secrets/gkr-feedback`; it does not create the Secret, install GKR,
change network policy, or opt any Task into the tool.

The mount is read-only without `subPath`, with requested mode `0400`. The
chart's existing non-root UID 65532 and Pod `fsGroup: 65532` are unchanged;
Kubernetes assigns the volume's group and adds group-read permission so that
the controller can read the files. Preserve equivalent group access if
customizing the Pod security context. Restart the controller after changing
routing or TLS material: both are loaded at startup, so projected Secret
updates alone do not reload them. Existing startup validation still rejects
invalid origins, node maps and TLS material.

## Opt a Task into feedback

Add `runtime_feedback` to the existing explicit native Agent
`runtime.defaultAllowedTools` or Task `agentRuntime.allowedTools` list. Keep the
runtime's existing native tool policy intact. This is additive configuration
on an Agent with `runtime.contractVersion: orka.harness.v2`; it is not a custom
HTTP Tool resource.

For example, an existing Codex Agent that uses the complete native tool surface
can retain it while adding feedback:

```yaml
spec:
  runtime:
    type: codex
    contractVersion: orka.harness.v2
    defaultAllowBash: true
    defaultAllowedTools:
      - Read
      - Write
      - Edit
      - Bash
      - Glob
      - Grep
      - WebSearch
      - WebFetch
      - runtime_feedback
```

This excerpt does not replace the Agent's required model/provider settings.
Existing workspace, command, and approval enforcement still applies. In
particular, a read-intent workspace remains read-only. OpenCode's read-intent
policy disables Bash, so its native command path requires its existing
write-workspace authorization and publication configuration. Do not change
those permissions solely to make a diagnosis work. Codex's existing native
read command path can exercise a network request without publication
credentials, subject to its native approval and command checks.

Opted-in Tasks receive a controller-derived, Task-specific RuntimePool with
capacity for one resident RuntimeSession and one prompt. A fresh Task UID
selects a different pool even when its Agent profile is identical. The frozen
execution snapshot verifies this identity across retries and runtime image
rotation. `sessionRef` and `execution.workspace` are unsupported for this
initial integration, which uses native Deployment-backed pools. Claude,
Copilot, external AgentRuntimes, and harness v1 are unsupported. Without the
configured client the name is unavailable; it cannot fall back to a custom
Tool resource.

## Agent flow and result boundaries

A suitable instruction is:

> Make network calls sequentially. If a native network operation fails, call
> `runtime_feedback` with `{}`. Describe only what its evidence supports. Use an
> alternative destination or method only when the task already authorizes it.
> Complete the Task and state any remaining uncertainty.

The tool accepts exactly an empty JSON object. Agent-supplied Task IDs,
container selectors, run IDs, timestamps, and URLs are rejected. The existing
session-local MCP route, controller bearer, operation capability, active
PromptAttempt, and runtime fences govern access. No additional agent auth
protocol or AuthZEN decision is introduced.

Before registration and reading, Orka verifies the live dedicated pool, exact
Pod UID, a single running `runtime` application container, and the authenticated
supervisor's sole RuntimeSession. Tool reads also require that supervisor's
active prompt to match the current Task UID, attempt, and session generation.
Orka repeats these checks before releasing a report and uses the broker's
existing epoch/prompt interlock. Cancellation, pool replacement, container
restart, and session changes cannot release a report under the old binding.

The result identifies the Task attempt, runtime fence, and container. GKR's
capture status, source instance, window, completeness, losses, and individual
network events retain their original meaning. `Collecting` reports must be
recent. `Finalized` and `Expired` reports describe their explicitly bounded
historical capture; they do not establish current coverage. Stale or missing
identity, mismatched responses, oversized responses, and unavailable services
never yield borrowed evidence. A failed registration does not itself prevent
the authorized Task from running, but no capture is claimed.

Evidence is **container-scoped**. Supervisor traffic and other descendants can
appear alongside the agent's command. Sequential tool calls aid interpretation
but do not prove per-tool causality. A monitor warning is not a kernel block.
No events does not mean success, permission, or absence of enforcement. Loss
counters can be node-wide and do not become Task-specific counts.

Orka completes the exact registration when native dispatch exits. GKR's
10-minute capture deadline bounds controller-loss and transport-failure cases;
its own bounded retention governs later reads. A report never authorizes
capture extension, policy relaxation, launch, or retry.

Validate the integration with an authorized native network request from a
fresh Task. Check that `runtime_feedback` returns evidence for the exact
worker container and that recovery uses only already-authorized destinations
or methods. Controller-brokered `web_fetch`/`web_search` execute outside the
worker container and cannot validate worker-container enforcement.

## Verification

The focused tests cover Task-specific pool materialization and snapshot
validation, existing broker HTTP credentials/capabilities and epoch interlock,
read-only tool arguments, exact Pod/container/runtime/session fences,
cancellation during diagnosis, GKR mTLS, response bounds, and capture windows.
Two mTLS backends verify exact node routing for registration, report, and
completion; an instrumented transport verifies that unmapped nodes issue no
requests. Configuration tests cover ambiguous modes, malformed/duplicate
mappings, invalid origins, file and entry bounds, and startup-frozen routing.
They use real broker/status HTTP and Kubernetes/store fixtures. Live
integration validation requires an actual native runtime request, matching
GKR evidence, and recovery within the Task's existing authority.
