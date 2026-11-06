# 40. Inject Substrate egress credentials at the gateway

Date: 2026-11-06

## Status

Accepted. Extends ADR 0039: native Substrate workspaces stop receiving the
provider-auth-proxy token, and MCP Tool Actors can use upstream credentials, by
having the Agent Substrate v1 egress gateway inject them. The sealed bootstrap of
ADR 0039 still delivers the per-Actor controller and capability credentials.

## Context

After ADR 0039 every native Substrate ACP runtime received the single
installation-wide provider-auth-proxy bearer through its sealed bootstrap. The
runtime holds it in memory, so any process in the guest (the agent, a tool it
runs, a dependency it installs) can read and exfiltrate it, and every runtime
shares it. MCP Tool Actors had no way to call authenticated upstream APIs without
baking a credential into an operator template or image.

Agent Substrate v1 can replace a request header at its egress gateway:
`HttpRuleEffects.replace_headers` names a header, a verbatim prefix and a
`credential_uri`. The bundled Kubernetes Secrets credential provider resolves
`ate-secret://k8s.io/default/<namespace>/<secret>/<key>` for an Actor only when
its Atespace is granted that namespace (optionally narrowed by `secretSelector`)
in the `k8s-credential-provider-namespace-policy` ConfigMap. The header is
replaced only when the Actor's request carries it, and resolution failures deny
the request (403/503, or 500 on Envoy without a provider). The gateway reads
cleartext `http` rules and terminated `https` rules; it re-originates `https`
trusting only public roots, so an in-cluster origin can be injected only over
`http`.

## Decision

- **Shared contract.** `RuntimeEgressRule.replaceHeaders` (orka-workspace) names
  up to four headers per exact-host `HTTP`/`HTTPS` rule, each with an optional
  prefix and a `secretKeyRef` (namespace, name, key). Values are never carried.
  Validation rejects `TLSPassthrough`, wildcard hosts, hop-by-hop, framing,
  routing and cookie headers, case-insensitive duplicates and control characters
  in prefixes. A request carrying replacements must require
  `runtime.egress-credential-injection`; only providers that enforce it advertise
  it. The reference namespace defaults to the workload's; another namespace is
  accepted only from the core-admitted intent and only when the provider lists it.
- **Substrate provider.** The provider compiles replacements into gateway header
  effects. It advertises the feature only with `--egress-credential-provider=k8s.io`
  (the operator's attestation that the gateway was installed with that provider)
  and accepts other namespaces only through `--egress-credential-namespace`.
  Otherwise it refuses such requests before creating native objects; a journaled
  request whose configuration later disappears fails verification and installs
  deny-all rather than resuming.
- **Core ACP path.** For a gateway-enforced native workspace whose provider
  advertises the feature, Core admits a separate `HTTP` rule for the
  provider-auth-proxy origin that replaces `Authorization` with `Bearer ` plus
  the `token` key of Core's private, owner-referenced provider Secret in its
  runtime namespace, and requires the feature. The sealed bootstrap then carries
  only a public placeholder, which the supervisor sends as its bearer; the proxy
  accepts the injected header unchanged and never the placeholder. Injection
  requires an `http` proxy origin whose host the controller API does not share.
  If injection is unavailable, admission fails with a pool condition naming the
  reason, unless the operator sets `--acp-workspace-allow-provider-token-in-sandbox`,
  which restores bootstrap delivery. Pod-backed providers are unchanged.
- **Core MCP path.** `spec.mcp.substrateActor.egress[].credential` names a header
  (default `Authorization`), a prefix and a Secret key in the Tool's namespace.
  The controller compiles it into the Actor's `EgressPolicy` only with
  `--substrate-egress-credential-provider=k8s.io`, requires the Secret key to exist
  and be non-empty, and otherwise fails closed before resume. Creating or updating
  such a Tool requires the caller to be allowed to `get` each referenced Secret
  (ValidatingAdmissionPolicy and webhook); the REST API rejects it. Policy
  comparison includes exact effects, so a changed or removed reference is
  re-applied.

## Consequences

The provider-proxy token, and MCP upstream credentials, are never in the
sandbox's environment, bootstrap, memory, filesystem or volume snapshots, so a
compromised guest cannot read, persist or exfiltrate them, and a snapshot or
journal leak carries no credential. Limits remain:

- Any process in the Actor can send the placeholder and so use the credential
  against the allowlisted destination, as it could use the proxy before.
- A destination that echoes request headers reveals the credential; injection on
  `http` crosses the cluster network in cleartext after the gateway.
- Credential-provider grants are per Atespace: any principal that can write an
  `EgressPolicy` in a granted Atespace can name every Secret the grant admits.
  Operators should dedicate Atespaces to Orka and narrow grants with
  `secretSelector` (Core labels its provider Secrets
  `orka.ai/runtime-pool-provider-credential: "true"`).
- The reference credential provider reads its namespace policy only at startup,
  and its upstream RBAC can read every Secret; operators must restart it after
  edits and should replace its ClusterRoleBinding with per-namespace RoleBindings.
- Core cannot observe the gateway's configuration; the provider flag is an
  operator attestation. A misconfigured gateway fails closed per request.
- Existing gateway-enforced native workspaces need either a provider release that
  advertises the feature or the explicit fallback flag before they are admitted.
