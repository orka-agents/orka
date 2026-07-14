# Security model for bring-your-own AgentRuntime demos

Remote execution backends are workload substrates, not governance authorities.

> Remote agents may ask; Orka decides and executes.

## Invariants

- `AgentRuntime.spec.deployment.endpoint` must not contain credentials.
- `AgentRuntime.spec.deployment.transportSecurity` defaults to `tls`. Demo HTTP
  facades explicitly opt into `insecure-cluster-local-http` and must resolve to
  a selector-backed, non-`ExternalName` Service in the same namespace.
- Runtime bearer tokens live in Kubernetes Secrets and must opt in with `orka.ai/agent-runtime-auth=true`.
- Runtime auth Secrets are bound to the expected `AgentRuntime` name and endpoint.
- Remote runtimes receive safe tool schemas only: name, description, brokered class, and JSON parameters.
- Remote runtimes never receive Tool CRD execution URLs, auth Secret refs, headers, bearer tokens, kubeconfigs, or approval bypass credentials.
- Orka validates allowed tools/classes before every brokered call.
- Orka creates canonical approval events for write tools and verifies exact argument/spec digests before execution.
- Brokered write execution records a pre-execution ledger entry; unresolved prior executions fail closed instead of duplicating consequential side effects.

## Demo-only controls

The checked-in generic HTTP fixture uses bearer-token authentication and
explicitly opts into `insecure-cluster-local-http` for kind/local demos. Orka
revalidates the Service at dispatch, rejects redirects, disables proxies for
this mode, and omits bearer auth from the public health/capabilities probes.
Production adapters should use TLS and may add mTLS or signed short-lived turn
credentials.

## Do not commit

- runtime bearer token values;
- Foundry credentials;
- AgentKit credentials;
- downstream tool API keys;
- raw transcripts or auth headers;
- kubeconfigs or service-account tokens.
