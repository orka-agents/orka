---
description: "Giving Tools a reusable, namespaced way to reach an external API without handing agents the credential."
---

# Outbound access policies

A Tool that calls an API usually needs a credential. Put that credential in the Tool, or in
front of the agent, and anything that steers the agent can misuse it. An
`OutboundAccessPolicy` keeps the credential on Orka's side: the Tool names a policy, and Orka
obtains and attaches the right credential at call time.

A policy uses exactly one mode:

| Mode | What Orka does | Use it when |
| --- | --- | --- |
| [`connection`](#linked-account-credentials-connection-mode) | Injects the requesting person's own linked-account token | Agents should act as the person who asked |
| [`agentIdentity`](#agent-identity-credentials-agentidentity-mode) | Injects a token issued for the Agent's own external identity | The API should see which Agent called, independent of who asked |
| [`direct`](#direct-credential-exchange) | Exchanges a token at an OAuth endpoint (RFC 8693 or RFC 7523) and injects the resulting Bearer token | The API accepts OAuth access tokens and you run a token service |
| [`gateway`](#trusted-gateway-routing) | Sends the call through a trusted gateway Service that holds the upstream credentials | You run an API gateway such as [agentgateway](https://github.com/orka-agents/orka-integration-agentgateway) |

A policy is reusable and namespaced; the Tool and its policy must be in the same namespace.
[Remote MCP Tools](remote-mcp.md) currently require the gateway mode and explicit native Task
authority. Field details are in the [Resource reference](../reference/resources.md#outboundaccesspolicy).

:::tip[Video demo]
Watch [Allow stock checks but block purchasing](https://www.youtube.com/watch?v=1vDI6PxhmfY).
:::

## Linked-account credentials (connection mode)

When a person has linked an account through a `ConnectorProvider`, a policy in `connection` mode lets a Tool act as that person:

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: OutboundAccessPolicy
metadata:
  name: github-as-me
spec:
  connection:
    providerRef:
      name: github
```

The credential belongs to the Task's verified requester, never to the namespace or the Agent. The controller looks up that person's Connection at call time, refreshes the token if it is about to expire, and injects it. Nothing falls back: a missing, revoked, or expired Connection, a Task without a verified requester, or a Connection that changed since the Task was dispatched all fail the call. Tools behind such a policy execute only in the controller, so runtime Pods and worker Pods never see the token: ACP runtimes reach them through the MCP broker, and native `type: ai` workers call an internal controller endpoint that re-checks the caller, the tool, and the frozen Connection. A `readOnly` Connection hides the provider's write tools from the agent, and write tools always ask for approval through the existing approval prompt. See [ADR 0033](https://github.com/orka-agents/orka/blob/main/docs/adr/0033-user-connectors.md).

Orka's built-in GitHub tools use the same links without a policy: a `ConnectorProvider` declares them with `source: Builtin`, and the controller's MCP broker runs them under the requester's frozen Connection for ACP runtimes (write tools only where brokered approval exists). Native worker Pods keep the Task's own credential Secrets for those tools. The [Connectors guide](../guides/connectors.md) covers the setup.

## Agent identity credentials (agentIdentity mode)

When an Agent is bound to an external identity (`Agent.spec.identity`), a policy in
`agentIdentity` mode lets a Tool act as that Agent. The first issuer is a Microsoft
Entra agent identity:

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: OutboundAccessPolicy
metadata:
  name: graph-as-agent
spec:
  agentIdentity:
    mode: Self
    entra:
      resource: https://graph.microsoft.com
```

The policy never names an identity: the identity always comes from the executing Task's
binding, which froze it, with the issuer instance, when the Task was bound. Only the
controller issues these tokens. It authenticates to Entra as the installation's agent
identity blueprint with its own projected ServiceAccount token, requests an exchange token
for the agent identity, and then an app-only token for `<resource>/.default`. Tokens are
injected per Tool call, cached for at most five minutes per policy UID and generation, and
never reach the runtime, the Task, the execution snapshot, events, or logs. Neither the
token requests nor the Tool call follow redirects. ACP runtimes reach these Tools through the
MCP broker; native `type: ai` workers fail closed. The Tool URL must be HTTPS without
template placeholders, and requests must go to the Tool's declared origin. Permissions are
the agent identity's app roles in Entra. See [ADR 0039](https://github.com/orka-agents/orka/blob/main/docs/adr/0039-agent-identities.md).

### Acting for the requester (Delegated)

With `mode: Delegated`, the Agent acts for the Task's verified requester, and both sides
are enforced: Entra issues an on-behalf-of token only for permissions granted to the agent
identity, and the resource applies the requester's own rights.

```yaml
spec:
  agentIdentity:
    mode: Delegated
    delegated:
      providerRef:
        name: entra-blueprint-signin
      maxDelegationAge: 1h
    entra:
      resource: https://graph.microsoft.com
```

Delegated mode is validated against Microsoft Graph. On-behalf-of tokens for other
resources, such as Azure Storage, are not yet validated against a live tenant.

- **The requester's grant** is their own Connection to the named `ConnectorProvider`, an
  Entra front-end client that requests the blueprint's scope. The controller refuses a
  linked token whose audience is not the blueprint, so the requester must sign in through
  Entra, not through Orka's GitHub dashboard sign-in.
- **What it may do** is exactly the provider's curated tool declarations, enforced as in
  connection mode. Write tools always need approval. On a Task bound with a Delegated
  policy, only the requester can approve any call; anyone else allowed to decide
  approvals may still decline.
- **For how long:** calls stop `maxDelegationAge` (default one hour, capped by
  `--agent-identity-max-delegation-age`) after the chain's root Task was created, whether
  the requester created it directly or with a Session message. Time spent in the chat
  before that does not count. Child Tasks, retries, and woken agents do not extend it, and a child whose link to
  its parent no immutable UID proves, such as one Kubernetes orphaned, fails closed. A
  fork of an identity-bound Task does not carry the identity; create a new Task instead.
  Calls also stop at once when the Task is cancelled, deleted, or finished, or when its
  Session, or that of any Task it was delegated from, is deleted: the controller checks
  both live before every call. A chat deleted and recreated under the same name is a new
  Session, so it does not revive its earlier Tasks.
- **Where:** only Tasks on built-in RuntimePools, whose Pods reach the network only
  through Orka. External `runtimeRef` runtimes, workspace-backed sessions, and native
  `type: ai` workers fail closed.

## Direct credential exchange

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: OutboundAccessPolicy
metadata:
  name: resource-api
  namespace: orka-system
spec:
  direct:
    grant: TokenExchange
    tokenEndpoint:
      url: https://identity.example.test/oauth/token
    subject:
      source: TransactionToken
    audiences: [resource-api]
    scopes: [api.read]
    requestedTokenType: urn:ietf:params:oauth:token-type:access_token
    expectedIssuedTokenType: urn:ietf:params:oauth:token-type:access_token
    output:
      header: Authorization
      prefix: "Bearer "
```

Direct mode supports RFC 8693 and RFC 7523, optional actor tokens, arbitrary token-type URNs, audiences, scopes, resources, static non-reserved parameters, ServiceAccount/Secret subjects, client-secret basic/post, and `private_key_jwt`.

Resource responses must be non-empty Bearer tokens with the expected `issued_token_type` for RFC 8693 (RFC 7523 may omit it). `Txn-Token` cannot be the output header, direct mode cannot coexist with `authSecretRef`, and Secret references cannot cross namespaces. Transaction-token scopes cannot expand the parent scope. Context-token Task creation fails closed when a referenced policy is unresolved and requires `orka:secrets:credentials:read` when direct mode reads Secret credentials or mints a ServiceAccount token.

A `ServiceAccount` source may not name the controller's own ServiceAccount: such a policy is not accepted, and the resolver refuses it before requesting a token. For other `ServiceAccount` sources, the controller creates a policy-owned namespaced `Role` whose `serviceaccounts/token` permission is restricted with `resourceNames` to the exact resolved ServiceAccount references. The AI worker is bound only to that Role. Invalid or unresolved policies revoke the binding before execution can mint a token.

The in-process AI worker is a trusted Orka worker; arbitrary task code runs in separate Jobs. These policy grants are shared by trusted AI tasks in the policy namespace. If your threat model includes compromise of the AI worker process itself, use a controller-side credential broker or per-task worker identities rather than enabling ServiceAccount token sources.

## Trusted gateway routing

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: OutboundAccessPolicy
metadata:
  name: agentgateway
  namespace: orka-system
spec:
  gateway:
    serviceRef:
      name: agentgateway
      namespace: gateway-system
      port: 8080
    scheme: http
```

```yaml
spec:
  http:
    url: https://api.example.test/v1/resource?version=1
    outboundAccessPolicyRef:
      name: agentgateway
```

Orka dials the gateway Service but preserves the original target authority, path, query, method, body, explicit `Authorization`, `Txn-Token`, MCP protocol headers, idempotency key, timeout, and cancellation. Orka revalidates public Tool authorities at execution time as defense in depth.

:::danger[Two things the gateway must do that Orka cannot do for you]
- **Strip the transaction token.** Orka forwards `Txn-Token` to the gateway; the final
  downstream must never see it. Configure stripping and resource-token exchange in the
  gateway integration.
- **Bind each authority to a configured upstream.** The allowlisted gateway is the
  enforcement boundary. If it behaves as an unrestricted DNS forward proxy, the allowlist
  means nothing.
:::

Same-namespace Services are automatic. Cross-namespace Services require exact `namespace/name:port` entries:

```yaml
controller:
  outboundAccess:
    trustedGatewayServices:
      - gateway-system/agentgateway:8080
    trustedTokenEndpointServices:
      - identity-system/token-service:8443
```

Wildcards are not supported. Gateway and token-endpoint allowlists are separate.

## Status and safety

Policy status contains only `observedGeneration`, `Accepted`, and `ResolvedRefs`. Reconciliation validates structure, Secret keys, Service ports, ServiceAccounts, TLS CA refs, and trust entries. Tool execution revalidates critical references immediately before use.
