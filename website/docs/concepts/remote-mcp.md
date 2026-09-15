---
description: "Let native AI Tasks call explicitly selected tools on an independently operated MCP server through a trusted outbound gateway."
---

# Remote MCP tools

A team can keep operating its existing MCP server while an Orka native AI Agent calls selected functions on it. Orka does not create a workspace, actor, or adapter Service for this backend.

The initial backend supports **native `type: ai` Tasks**, **Streamable HTTP protocol `2025-06-18`**, and **`OutboundAccessPolicy.gateway`**. It does not enable remote Tools in ACP runtimes or the OpenAI/Anthropic compatibility APIs. Plain HTTP and managed MCP Tools retain their existing behavior.

## One Tool, one remote function

Create a Tool through the Kubernetes API using an identity authorized to manage Tools. Its name is the model-visible alias; `toolName` is the exact name sent in MCP `tools/call`.

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Tool
metadata:
  name: service-health
  namespace: operations
spec:
  description: Look up a service's health
  # Copy and review this selected function's actual tools/list inputSchema.
  parameters:
    type: object
    properties:
      service: {type: string}
    required: [service]
    additionalProperties: false
  mcp:
    remote:
      url: https://example.com/mcp
      toolName: service_health
  http:
    authSecretRef:
      name: operations-mcp
      key: token
    outboundAccessPolicyRef:
      name: operations-mcp-egress
    timeout: 30s
---
apiVersion: core.orka.ai/v1alpha1
kind: OutboundAccessPolicy
metadata:
  name: operations-mcp-egress
  namespace: operations
spec:
  gateway:
    serviceRef:
      name: operations-gateway
      port: 8080
    scheme: http
```

This is an illustrative descriptor, not a deployable MCP service. Replace the endpoint and reviewed schema with those of your service and configure its fixed route on your gateway. Provision credential values separately in the named Secret; never put them in Tool headers, URLs, arguments, Agent prompts, or Task environment variables.

A Tool selects exactly one remote/workspace/actor MCP backend. Remote MCP forbids `mcp.path`, `http.url`, non-POST method overrides, body authentication, endpoint interpolation, and transport/protocol/credential header overrides. Admission and consumers reject common credential-bearing header names such as `X-Api-Key`, `api_key`, `X-Auth-Token`, and `X-Access-Token`, case-insensitively. Ordinary metadata headers such as `X-Tenant` remain allowed. Consumer URL validation also rejects signed-query credentials such as `sig`, `signature`, provider signature keys, and access IDs, including case/underscore variants; benign routing queries remain allowed. These conservative name checks do not detect secrets hidden in arbitrary metadata or descriptions; operators must still keep all credential values in named Secrets. A reviewed object input schema with resolvable local references is required. Parameter-schema checks run at controller and execution boundaries; the existing schemaless Kubernetes field is preserved for legacy Tools.

Multiple Tools may share a credential and outbound policy. A separate connection inventory or discovery controller is not required.

## Agent selection is a ceiling for remote Tools

Enable only reviewed aliases in the referenced native Agent:

```yaml
spec:
  tools:
    - name: service-health
      enabled: true
```

Remote execution requires a referenced native Agent **in the Task's namespace** that enables that alias. Omit `spec.agentRef.namespace` or explicitly set it to the Task namespace. The initial remote backend rejects foreign Agent references before reading the Agent, even with `--enforce-namespace-isolation=false` or separately granted read access; cross-namespace Agent support is deferred. This restriction concerns the Agent resource, not the remote MCP server or gateway, which may remain elsewhere under the existing outbound trust rules. Plain HTTP, managed MCP, and built-in tools retain their existing cross-namespace Agent behavior.

A Task cannot add a remote Tool outside that Agent's enabled list, and an alias cannot replace a built-in tool. Checks occur before model exposure and again before invocation. The historical additive/default behavior of Agent and Task selections remains unchanged for other backends.

If both initial Tool lookups fail, the worker cannot determine the alias's backend. To preserve legacy/built-in best-effort startup, that unresolved alias is omitted from model definitions and the invocation allowlist; it cannot execute as a remote Tool. Once a definition is known to be remote, any failed reread or binding is fatal. Startup success alone is not proof that every requested alias was available.

The worker verifies its controller-issued Task UID and freezes the Task/Agent/Tool identity, advertised definition, and credential/policy dependency versions. After resolving credentials and the gateway transport, it revalidates those bindings **before the first MCP request**, during both startup discovery and invocation. The prepared endpoint, headers, and TLS material are not resolved again after that fence. Deletion, recreation, selection changes, or changed bindings fail closed rather than redirecting an already advertised call. Even a conservative dependency-version change can require starting a new Task.

## Authentication and outbound governance

There is no new inbound tool-invocation Service. Calls execute inside Orka's trusted native Go worker. Model definitions contain only the reviewed name, description, and schema; the exact same-namespace Secret name/key is resolved by the worker, without legacy key-only mounted-Secret fallback. The default separately deployed code-execution Jobs do not receive these credentials. This is the existing trusted-worker boundary, not isolation against compromise of the worker process itself.

A remote Task needs its **Task-scoped transaction token**, supplied through Orka's owner-referenced transaction Secret mechanism, and transaction metadata authorizing credential use (`orka:secrets:credentials:read` by default). Any namespace or exact Secret constraint is enforced before credential reads. A plain `kubectl apply` of a Task containing only `agentRef` does not manufacture that authority. Use your deployment's authorized transaction issuance/delegation flow; never put token values in Task spec, status, annotations, or logs. Reserved provenance and Secret-reference annotations remain protected by admission.

Initialize, initialized notification, discovery, call, and session cleanup use the same admitted gateway transport. There is no direct fallback, ambient proxy, cookie jar, followed redirect, or automatic replay of a call whose outcome is uncertain. Existing configured transaction exchange remains under the Task's bound authority.

The gateway must authenticate and authorize the transaction, bind the preserved original authority to its configured upstream, and strip `Txn-Token` before forwarding downstream. The resource Bearer credential is separate from transaction authority. See [outbound access policies](outbound-access.md) for exact cross-namespace Service trust and TLS configuration.

Original-URL public-host SSRF checks still apply. A private MCP service needs an explicit trusted gateway route behind an admitted public authority; it does not get the managed-actor routing exemption. The SCM/publisher egress proxy is not this gateway, and `OutboundAccessPolicy` does not by itself impose a Kubernetes NetworkPolicy on every worker connection.

## Schema provenance and drift

1. An operator obtains the selected function's `tools/list` descriptor and reviews its input schema into `Tool.spec.parameters`. Keep the server release and reviewed snapshot with deployment configuration.
2. Before model exposure, Orka initializes a session and lists tools under the actual Task's credential and gateway authority. It verifies the selected name and compares its input schema to the reviewed snapshot.
3. Before each call, the Tool executor validates the actual arguments against the reviewed schema **before resolving call credentials, exchanging tokens, or sending MCP requests**. Native Task/Tool dependency revalidation may already have read credential metadata or Secret data. Argument validation does not coerce values or apply defaults. A new bounded session then repeats discovery and verification. Invalid arguments, missing/duplicate names, unsupported schemas or observed schema drift prevent `tools/call`.
4. Update a Tool only after reviewing the new schema and authorization implications; start a new Task to use the changed definition. Discovery never grants more tools or hot-swaps a running Task's definitions.

Schema compilation and argument validation retain exact JSON numbers, including numeric bounds, `multipleOf`, `enum`, and `const`. The existing bounded schema-representation and local-reference checks are retained, including cardinality-field limits and rejection of references outside recognized schema locations; rounded values from these compatibility checks are never used to validate arguments. Schemas without `$schema` use Draft 2020-12; local references are supported, but external schema resources are never fetched from the network or filesystem. Validation errors report a local failure category, not argument values.

Object member order and mathematically equivalent JSON numeric spellings do not cause drift. Server instructions, descriptions and annotations are not imported into Agent/model instructions; the operator's description remains authoritative. Schema comparison detects observed interface changes, not semantic changes behind the same schema, nor an atomic guarantee that the server cannot change between discovery and execution.

Remote Tool reconciliation reports configuration acceptance, not an authenticated health probe: `Accepted=True`, `Available=Unknown`, and `status.available=false` do not prove execution success. The Task's actual discovery and call provide that evidence.

## Results, precision and bounds

Successful calls return a JSON envelope retaining text `content` and optional object `structuredContent`. Numeric payloads remain raw/exact rather than passing through binary64, including nested integers beyond 2^53 and high-precision decimals. The actual OpenAI Responses/Chat Completions and Anthropic Messages schema serialization also preserves complete reviewed schemas and numeric constraints.

Only unannotated text and structured JSON results are supported. Nontext content, result/content metadata or audience annotations, unsupported interactions and protocol errors fail closed. Server error text is not echoed into errors; local diagnostics identify safe failure categories. Resource credentials and bound/exchanged Task transaction tokens are redacted from strings. The model itself is not an exact-arithmetic engine: exact transport does not guarantee an exact model answer.

Each reviewed schema, selected discovered input schema, and argument object has a separate **65,536-digit aggregate numeric-expansion budget** before exact validation or schema-number canonicalization. For every JSON number, Orka conservatively counts coefficient digits (including fractional digits) plus the absolute decimal exponent. Excessive/overflowing exponents or an exceeded aggregate budget fail closed; values are never rounded or clamped to fit. Numeric-looking strings and successful result payloads are not changed by this validation guard.

Operations default to 30 seconds, with a positive configured timeout capped at two minutes. Responses/results are limited to 1 MiB; discovery to 2 MiB aggregate, eight pages and 1,024 names. Cancellation is preserved. Sessions are per operation, and cleanup is best-effort with a separate bounded timeout; cleanup failure never replays a tool call.

For an SSE response to a POST, Orka accepts one matching JSON-RPC response as terminal and closes that response body immediately. Any interaction or notification received before it fails closed. It does not drain or validate later events: MCP servers should close after their response, but are not required to, so waiting for EOF could turn a completed call into a timeout. Events after the terminal response are not processed and cannot trigger another operation.

Standalone SSE, stream resumption, shared long-lived sessions, sampling, elicitation, resource/prompt APIs, automatic schema refresh and direct OAuth remote qualification are outside this initial backend.
