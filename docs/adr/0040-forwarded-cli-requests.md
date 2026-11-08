# 40. Let command-line clients reach services through forwarding Tools, starting with the Azure CLI

Date: 2026-11-05

## Status

Proposed. Read requests from the Azure CLI are implemented, and writes are
being added (see "Writes" below). Builds on ADR
0039 (agent identities) and ADR 0033 (per-user connectors); extends ADR 0034's
ceilings to the new Tool kind without changing them.

## Context

An agent that works locally runs CLIs such as `az` with the developer's
signed-in account. In an Orka runtime the same command has nothing to sign in
with: toolboxes add binaries but no credentials, and credentials reach only
typed Tools that the controller's broker executes. To run on Orka, the agent
and its instructions have to be rewritten around those Tools.

Handing the CLI a credential is ruled out by ADR 0039 ("handing issued tokens
to the runtime") and ADR 0033 (worker Pods never receive a person's tokens).
Running the CLI in a separate credentialed sandbox moves the token out of the
controller and makes revocation depend on stopping that sandbox. Writing one
Tool per command does not scale for the Azure CLI's several thousand commands.

Most of `az` is a client for one HTTPS API, Azure Resource Manager, whose own
role-based access control already decides what a caller may do. The CLI
supports custom clouds and the App Service managed-identity protocol, so it can
be pointed at a local endpoint and signed in without a real credential.

## Decision

- A Tool or a ConnectorProvider's curated tool may set
  `http.forward.protocol: AzureResourceManager`. Such a forwarding Tool is not
  a function the model calls: its URL is an HTTPS origin only, it declares no
  parameters, headers, auth Secret, or MCP backend, it must be class `read`,
  and it takes its credential from an `OutboundAccessPolicy`. Admission checks
  this, and the controller checks it again before exposing the Tool.
- When a RuntimeSession's frozen tool policy allows one Azure forwarding Tool,
  the supervisor starts a loopback HTTPS endpoint for that session. It writes
  `~/.azure` so the active cloud's Resource Manager endpoint is that endpoint,
  sets `IDENTITY_ENDPOINT` and `IDENTITY_HEADER` so `az login --identity`
  works, and adds its per-boot CA to `REQUESTS_CA_BUNDLE`. Two Azure
  forwarding Tools in one session are refused, so which identity `az` acts as
  is never left to chance.
- The managed-identity endpoint returns a JWT-shaped placeholder whose
  signature is a per-session secret. It authorizes nothing outside that
  endpoint, and only Resource Manager audiences are issued.
- Each Resource Manager request becomes one broker call to the forwarding Tool,
  under the session's current prompt authority, with the arguments `{method,
  path, query}`. The supervisor and the controller each accept only `GET` and
  `HEAD`, an absolute path with no dot segments or encoded separators, and a
  well-formed query; a request never carries a body or client headers.
- The controller builds the URL on the Tool's own origin, adds the credential
  the policy resolves (the Agent's identity, a Delegated token, or the
  requester's Connection), never follows redirects, and returns the upstream
  status, a fixed list of response headers, and the body. The supervisor
  rewrites the origin in links such as `nextLink` to itself so paging stays on
  the broker.
- Forwarding Tools are hidden from the model's tool list and refused on the MCP
  endpoint. The executor runs them only when the ACP broker enables it, so a
  native `ai` worker or the internal connector endpoint cannot.
- The toolbox image (`examples/azure-cli`) holds the Azure CLI and a wrapper
  that signs in on the first command. It carries no credential.

## Writes

An Azure forwarding Tool can opt in to writes with `http.forward.writes`:

```yaml
http:
  url: https://management.azure.com
  forward:
    protocol: AzureResourceManager
    writes:
      approval: Required   # the default; None sends writes without approval
```

- A Tool with writes becomes two tools in a prompt's tool policy. Its own name
  carries `GET` and `HEAD` as before, read-only and never approved. A derived
  tool, `<name>:write`, carries `PUT`, `PATCH`, `DELETE`, and `POST`. The
  colon cannot appear in a Kubernetes name, so the derived name never meets a
  real Tool. Everything Orka keys by tool name, such as effects, approvals,
  and prompt authority, then works per method class without a per-call
  exception, and a long-running operation's polling stays a plain read.
- The derived tool is consequential. Every write runs under the external-effect
  ledger, so a lost result settles `OutcomeUnknown` and is never retried
  blindly. Because a forwarded `POST` or `PATCH` need not be idempotent, a
  write's effect record is never reclaimed: a redelivery that finds it in
  flight, even after its lease expires, answers with an unknown outcome
  instead of sending it again. Its Tool's `timeout` must fit the ledger's
  call duration. With `approval: Required` the write tool is approval-required: the
  broker records the request (method, path, query, and body) and waits for a
  person, while the gateway holds the CLI's request open. Built-in runtimes
  otherwise cannot ask for approval
  ([#770](https://github.com/orka-agents/orka/issues/770)), but here the
  waiting client is the gateway, not the agent's MCP client, so forwarded
  writes are the one approval-required tool built-in runtimes accept. A
  policy that allows a write tool without approval while its Tool requires
  approval, such as a registered external runtime's, is refused when it is
  built and again before each call, so a Tool switched to `Required` stops
  unapproved writes at once.
- A write request carries its body, a JSON object or array, up to the
  broker's argument limit, and `Content-Type: application/json`. The body is
  JSON in the tool arguments, not a string, so the structural redaction of
  approval previews and events reaches credential-named fields in it. Client
  headers stay dropped. A Task that disallows the Tool, or only
  `<name>:write`, gets no write tool.
- Retries join. The Azure SDK times out a read after 300 seconds and retries,
  keeping the same `x-ms-client-request-id`. The gateway derives a write's
  call ID from that header, the method, the path, the query, and the body's
  digest, so a retry joins the same pending approval or replays the recorded
  result instead of writing twice. A write without the header gets a fresh
  call ID and is never joined. Azure's own answers, including `429` and
  `5xx`, pass through unchanged, so the client retries them as it would
  against Azure directly; with the header, such a retry replays the recorded
  answer instead of resending, so a throttled write fails after the SDK's
  retries and the agent runs the command again.
- `POST` actions are forwarded too, so `az aks get-credentials` and actions
  such as start and stop work. An action that returns a secret, such as
  `listKeys` or `listClusterUserCredential`, hands it to the runtime, as the
  same command would on a developer's machine. Reviewers approving a
  `POST` see its path; Tools that must not return secrets need
  `approval: Required` or no writes.
- Writes follow each credential mode's own write rules: the Agent's identity
  (Self), a Delegated token for the requester, or a `readWrite` Connection.
  A linked account writes only when the provider's forwarding tool also sets
  `forward.writes` (its `approval` is unused), which changes the provider's
  authority digest, so people consent again. A write then matches as class
  `write` and needs a `readWrite` link. Without one (a `readOnly` link or none
  when the Task is planned) the write tool is not offered, as write-class
  connector Tools are hidden; the reads stay.
  Delegated writes follow the Tool's approval setting; when one needs
  approval, only the requester can approve it, as ADR 0039 requires.
- A decided write gets a final answer, never a status the Azure SDK retries:
  a declined, expired, cancelled, or stale approval is `403
  OrkaApprovalNotGranted`, a write refused before it was sent is `403
  OrkaToolRefused`, and a write that may have been sent (an unknown outcome,
  or a failure after delivery such as an oversized response) is `409
  OrkaOutcomeUnknown`. The approval names the method and path.

## Rejected alternatives

- **A credential in the runtime**, even short-lived: a prompt-injected agent
  can copy it, and the broker could no longer check each request.
- **A separate credentialed sandbox running the real CLI**: the token leaves
  the controller, revocation needs the sandbox stopped, and it is new
  infrastructure. It remains the fallback for clients that cannot be pointed
  at a local endpoint, or that do not speak HTTPS.
- **One Tool per command**: does not scale to the Azure CLI.
- **A transparent egress proxy that swaps a placeholder for the token**: it
  needs TLS interception in the runtime, and the credential would depend on
  the destination rather than on the authenticated session.
- **A generic "any URL on this host" Tool for the model**: the model would see
  a raw HTTP tool; the forwarding Tool serves only the CLI.

## Consequences

- An agent's `az` commands work unchanged in Orka for Resource Manager reads.
  Azure RBAC on the chosen identity is the real limit on what they return.
- Every forwarded request is a broker call: it needs a running prompt, is
  recorded as an identity use when the policy is identity-backed, and stops
  when the Task is cancelled, the Session is stopped, or the identity is
  disabled.
- Writes and actions (`PUT`, `PATCH`, `DELETE`, `POST`) are refused unless the
  Tool sets `http.forward.writes`; see "Writes".
- A write waiting for approval holds one of the session's two broker call
  slots and the CLI's request for up to the approval window, ten minutes.
- Only Resource Manager is forwarded. Commands that call other services, such
  as Microsoft Graph (`az ad`), Key Vault data, or Storage data, fail with a
  clear error. Each would need its own forwarding protocol and token audience.
- The placeholder token names a zero tenant, and `az account show` reports
  that zero tenant ID. Subscription IDs and everything Resource Manager returns
  are real.
- A command that names an absolute Resource Manager URL, such as `az rest
  --url https://management.azure.com/...`, bypasses the gateway and is
  blocked by the runtime's network policy; relative URLs go through it.
- Forwarded responses are bounded by the broker's 1 MiB result limit, which a
  single Resource Manager page fits.
- The toolbox is about 150 MB and pins a Python and an Azure CLI version.
