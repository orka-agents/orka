# 40. Let command-line clients reach services through forwarding Tools, starting with the Azure CLI

Date: 2026-11-05

## Status

Proposed. Read requests from the Azure CLI are implemented. Builds on ADR
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
- Writes and actions (`PUT`, `PATCH`, `DELETE`, `POST`, including listing keys)
  are refused. Supporting them needs approval, idempotency, and effect records
  per request, and is future work.
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
