# 57. Keyless Azure OpenAI Providers authenticate in the controller

Date: 2026-11-20

## Status

Accepted. Covers `Provider.spec.auth.entra` for `azure-openai` Providers in the
controller and for `type: ai` Tasks through a controller relay. Other provider
types, other Task types, and read-only AI Tasks stay key-based.

## Context

Many Azure deployments prefer or require Microsoft Entra ID over API keys, and
key-based auth is often disabled on Azure AI resources. Until now an
`azure-openai` Provider could only present a key from `spec.secretRef`, and
the key reached every Pod that used the Provider: chat in the controller, the
compatibility APIs, and AI worker Pods as an env var.

Entra workload identity removes the key, but an Entra access token is itself a
bearer credential for every deployment the identity may call, for up to an
hour. Handing one to a worker Pod would leave the same exposure a key has:
anything the Task runs could copy it and use it elsewhere until it expires.

## Decision

- An operator configures named workload identities on the controller
  (`--provider-identities`). Each has a tenant, a client ID, a cloud, and the
  exact `https` origins its tokens may reach. The controller presents a
  projected ServiceAccount token, audienced to the identities' exchange
  audience, as their federated credential. The chart protects that
  ServiceAccount with the admission policy agent identities already use.
- A Provider names an identity in `spec.auth.entra.identity` instead of
  `spec.secretRef`; CEL requires exactly one. The REST API cannot set or change
  `spec.auth`, so choosing an identity is a Kubernetes RBAC decision.
- Only the controller ever holds an Entra token. It caches tokens for at most
  five minutes, sends them only to the Provider's origin when that origin is in
  the identity's allowed endpoints, never follows a redirect, and reports
  issuance in the `CredentialReady` condition with the issuer's error code only.
- AI worker Pods get no key and no token. The Job records which Provider each
  keyless slot was built for (namespace, name, UID) in a controller-reserved
  env var. The worker's Azure SDK keeps the Provider's real base URL, and a
  transport rewrites each request to the controller's internal relay,
  authenticated with the worker's ServiceAccount token.
- The relay forwards a request only when every check holds: it is a `POST` to
  one of the two Azure OpenAI paths the AI worker uses (deployment chat
  completions, Responses), with only an `api-version` query; the caller is the
  current worker Pod of a running `type: ai` Task, and the Job is the Task's
  current Job; the slot is one the Job recorded; and the Provider still has the
  recorded UID and is a Ready keyless `azure-openai` Provider. The relay adds
  the token and filters headers both ways like the provider proxy.
- Read-only AI Tasks, whose Pods have no ServiceAccount token, and other Task
  types fail with a clear message when they would use a keyless Provider.

## Consequences

- A compromised worker can make model calls only through the relay, only to its
  own Provider's two paths, and only while its Task runs. It cannot take a
  credential elsewhere, and replacing or re-keying the Provider refuses its
  next request; a request already relayed finishes within its bound.
- Every relayed model call passes through the controller, which bounds them (64
  in flight, 64 MiB per response, 30 minutes per request). A controller restart
  ends in-flight relayed calls, which the AI worker retries like other
  transient provider errors.
- Adding a provider type, path, or Task type to keyless auth means extending the
  relay's allowlist and this record, not handing out tokens.
