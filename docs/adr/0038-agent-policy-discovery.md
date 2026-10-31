# ADR 0038: Tell agents which policies apply before they act

Date: 2026-10-30

## Status

Proposed.

**What it defines.** An engine-neutral contract that lets an agent ask three questions:

- which policies apply to it (Describe);
- whether a proposed action would be allowed (Check);
- what an enforcement point did after the fact (Explain).

It also defines one refusal format for every refusal Orka mediates.

**Where it lives.** Orka's own frozen policy is the first provider, and the contract lives
in `pkg/policy` as its own Go module.

**Advice only.** Discovery never changes enforcement, and never selects or relaxes a
container enforcement requirement such as the one #952 proposes.

**Not yet.**

- This record implements nothing. The delivery order is at the end.
- It is not accepted until the contract has been walked through with the maintainers of
  at least one provider outside Orka.

**Inventory.** The surfaces it covers are listed in
[Policy surfaces agents run into](../development/agent-policy-surfaces.md).

## Context

**Agents learn rules by breaking them.** Most refusals reach an agent as opaque errors.

- **One MCP error for two causes.**
  - The ACP MCP proxy returns the same JSON-RPC error, `-32001` "MCP tool call is not
    authorized", for two different cases:
    - a tool that isn't an allowed brokered tool;
    - a prompt whose authority has ended.
  - A model can name any MCP tool, so this path is reachable in ordinary sessions.
- **One broker error for every failure.** The supervisor's broker client collapses every
  non-200 broker response into `-32002` "MCP broker call failed". That covers a stale
  fence, an inactive prompt and a failed tool alike.
- **Mislabelled Kubernetes refusals.** Built-in tools that use the shared classifier
  label Kubernetes errors this way:
  - Every 403 becomes `permission_denied` with "Check RBAC permissions". That includes
    Gatekeeper's admission denials.
  - A ValidatingAdmissionPolicy denial without a `reason` (422) becomes `internal_error`.
  - So does a webhook denial that sets no status code, which the API server returns as
    400.
  - Admission warnings are dropped.

  Some paths return the raw error instead. One example is `delegate_task` failing to
  create the child Task, so a Gatekeeper denial there reaches an ACP agent as `-32002`.
- **Egress.** Blocked egress looks like an outage.
- **Tool hints.** `tools/list` doesn't say which tools are read-only or need approval.

**MCP's rule.** The MCP specification says clients *should* pass tool execution errors
(`isError`) to the model, but only *may* pass protocol errors. Both MCP refusals above
are protocol errors.

**The exception is approvals.** The broker returns approval outcomes as `isError`
results, each with a code and plain guidance. For example, `tool_outcome_unknown` says
"The tool may have run. Do not repeat it automatically."

**Nothing answers the whole question.** No existing system answers "what applies to this
caller for action X on resource Y" across engines. Each covers one slice:

- **Authorization:** SubjectAccessReview.
- **Admission:** server-side dry-run, `gator` and the Kyverno CLI.
- **Runtime enforcement**, such as Gatekeeper Runtime: no preflight API at all.
- **Configuration:** kpt validators and ConfigHub validation.

**General-purpose precedents.**

- **AuthZEN.** The Authorization API 1.0 standardizes single and batch evaluation and
  action search. It treats a deny as an answer, not an error. Its decision is a boolean.
- **Cloud preflight.** Azure Policy's `checkPolicyRestrictions` and the AWS IAM policy
  simulator are the closest preflight precedents.
- **All advisory.** Each of these calls its answers advisory.

**Related work.** Two open PRs touch this boundary, and neither tells an agent the rules
beforehand.

- **#952: a gate before execution.**
  - An operator-trusted provider must confirm enforcement before an agent runs.
  - It talks mTLS gRPC (`orka.enforcement.v1`).
  - Providers are listed in an operator-owned registry.
- **#640: `runtime_feedback`.**
  - It reads Gatekeeper Runtime network evidence after a failure.
  - It registers a capture before the prompt starts.
  - It binds evidence to the exact Pod, container and RuntimeSession, and checks the
    prompt's start time.

**The #512 pattern.** The storage provider work for #512 (open PRs #887 and #917) proposes
a pattern for pluggable providers in Orka, `pkg/storage`:

- a contract module with no Orka imports;
- a shared provider test suite;
- a gRPC wire;
- an in-process default.

## Decision

### 1. Ground rules

1. **Advisory.** Discovery grants nothing and changes no enforcement point. An answer
   describes one moment.
2. **A deny is an answer.** A provider that errors, times out or can't evaluate an action
   contributes `unknown`, never `allow`.
   - From a required provider, that makes the result `unknown`, unless another provider
     denies the action (section 3).
   - From an optional provider, the result is marked incomplete (section 3).
3. **The subject is the authenticated caller.**
   - For an ACP session, Orka fills it in from the session:
     - the Task, attempt and prompt, with the prompt's start time;
     - the RuntimeSession and its generation;
     - the runtime identity Orka observes for that session: node, Pod UID, container
       name and ID, restart count, and the session's process UID.

     Some workspace sessions run on a provider's native worker, not a Pod. For those, the
     runtime identity is the instance identity core verifies for that worker. If a provider
     can't bind evidence to it, Observe and Explain aren't available there, and Describe
     reports the `runtime.*` domains as `not_covered`.
   - For a native AI worker, the subject is the Task, the attempt and the worker Pod.
   - For chat, it is the signed-in caller and the chat turn.
   - For the compatibility API, it is the authenticated caller of the request.
   - An agent can't name another Task or identity, or supply or override any of these
     fields.
   - Discovery tools reject subject fields in their arguments.
4. **Provenance.** Every answer says whether it came from the enforcement point itself
   (`authoritative`), and whether it covers everything that applies (`complete`).
5. **Versioned.** Answers carry the policy digests they were based on, and a
   `validUntil` time.
6. **Policy text is data.**
   - Orka builds summaries from fixed templates.
   - Provider and API server messages appear only as bounded excerpts in an
     `untrustedMessage` field.
7. **Receipts and evidence are historical.** An enforcement receipt or a capture report is
   never proof that a control is active now.
8. **Enforcement is out of reach.** Discovery never selects, narrows or relaxes an
   enforcement requirement.

**Scope of the first version.**

- **Policy and configuration Orka owns:**
  - the tool and approval policies;
  - workspace intent and publication constraints;
  - OutboundAccessPolicies on HTTP Tools;
  - namespace scope;
  - Task and delegation limits;
  - Agent runtime ceilings, once #865 lands;
  - transaction-token scopes.
- **Kubernetes checks:** authorization and admission, run for the identity that actually
  acts.
- **Later, through providers:** configuration owned by external systems, such as a GitOps
  controller that reverts direct edits, ConfigHub or kpt.

### 2. The contract

**Operations.** There are four. A provider implements the ones it supports, and declares
which.

| Operation | Answers | Used by |
| --- | --- | --- |
| Describe | The constraints and configuration capabilities in force for the subject, with coverage for each domain | `policy_describe`, session instructions |
| Check | A decision for each of a bounded batch of proposed actions, without performing them | `policy_check` |
| Explain | Evidence of what an enforcement point did, after the fact | `policy_explain` |
| Observe | Attach to a subject before it acts and detach afterwards, for providers that must start capturing first | Orka, around each prompt |

**Actions.** An action has:

- a `domain`;
- a `verb`;
- a `resource`: kind, namespace, name and bounded attributes;
- for Kubernetes domains, the complete proposed object. Orka derives it from an allowed
  tool call (section 4); an agent never supplies one directly.

**Domains:**

- `orka.tool`;
- `orka.api`;
- `orka.delegation`;
- `orka.workspace`;
- `kubernetes.authorization`;
- `kubernetes.admission`;
- `runtime.network`;
- `runtime.file`;
- `runtime.process`;
- `runtime.enforcement`;
- `config.<system>`.

**Decisions and retry guidance:**

| `decision` | Meaning | Usual `retry` |
| --- | --- | --- |
| `allow` | No `enforce` answer refuses the action, and every required provider answered | `not_needed` |
| `deny` | At least one provider refuses it | `never`, or `with_changes` when a different request may pass |
| `requires_approval` | Allowed only after a person decides | `after_approval` |
| `conditional` | Allowed if the listed conditions hold, such as required fields or allowed values | `with_changes` |
| `unknown` | An applicable provider couldn't answer | `later` with `retryAfterSeconds`, or `never` when nothing covers the domain |

**Example 1.** A Check result from `orka-policy`, for a child Task that asks for a tool
above the Agent's runtime ceiling (illustrative):

```json
{
  "decision": "deny",
  "retry": "with_changes",
  "authoritative": true,
  "complete": true,
  "reasons": [{
    "provider": "orka-policy",
    "enforcedBy": "orka-controller",
    "policy": "agent/reviewer/ceiling",
    "mode": "enforce",
    "code": "ceiling_exceeded",
    "summary": "The reviewer Agent's ceiling doesn't allow the tool Write.",
    "fields": [{"path": "spec.agentRuntime.allowedTools", "allowed": ["Edit", "Glob", "Grep", "Read"]}]
  }],
  "versions": {"orka.agentConfiguration": "sha256:…"},
  "evaluatedAt": "2026-10-30T09:00:00Z",
  "validUntil": "2026-10-30T09:05:00Z"
}
```

**Example 2.** A dry-run denial (illustrative). The API server reports only a status and
a message, so the provider fills in just these fields:

- `enforcedBy`, from the webhook or policy name the API server writes into the message;
- the message itself, as a bounded `untrustedMessage`.

It doesn't parse policy names or field constraints out of third-party text.

```json
{
  "decision": "deny",
  "retry": "with_changes",
  "authoritative": true,
  "complete": true,
  "reasons": [{
    "provider": "kubernetes-dry-run",
    "enforcedBy": "validation.gatekeeper.sh",
    "mode": "enforce",
    "code": "admission_denied",
    "untrustedMessage": "[allowed-repos] container image must come from ghcr.io/example/"
  }],
  "evaluatedAt": "2026-10-30T09:00:00Z",
  "validUntil": "2026-10-30T09:05:00Z"
}
```

**Structured fields and text.** Only providers that return structured constraints fill in
`policy` and `fields`. Orka-owned policy does; so does a provider whose own API returns
them. Text that comes from a provider or the API server always goes in
`untrustedMessage`, never in `summary`. Orka builds `summary` from fixed templates.

**Describe results** list constraints. Each constraint has `id`, `domain`, `effect`,
`summary`, `values`, `source`, `version`, `mode`, `authoritative` and `complete`. Coverage
is reported for each domain:

- `described`;
- `check_only`: Orka can evaluate a request but doesn't list the policies. Admission is
  like this.
- `not_covered`.

**Explain results** carry the evidence window, completeness, loss counters and bounded
events. They are never combined into a decision.

**Modes.** `mode` belongs to a reason, and is one of `enforce`, `warn`, `audit` or
`observe`.

- A provider's decision says what its enforcement point would do with the request.
- A `deny` must carry at least one `enforce` reason. A reason in any other mode is
  information only: it never refuses a request, and it never counts as enforcement.

### 3. Combining answers

**Each provider returns one decision per action.** It returns `unknown`, or fails, when it
can't evaluate the action.

For each action:

1. If the agent submitted an action it can't make, the result is `unknown` with
   `retry: never` and the code `action_not_reachable` (section 4).
2. Otherwise, if any provider returns `deny`, the result is `deny`.
3. Otherwise, if a required provider for any domain the action touches failed or returned
   `unknown`, the result is `unknown` with `retry: later`.
4. Otherwise, if no provider returned a decision other than `unknown`, the result is
   `unknown`:
   - with `retry: never` when no provider is configured for the domain;
   - with `retry: later` otherwise.
5. Otherwise, if any provider returns `requires_approval`, the result is
   `requires_approval`. Conditions from any `conditional` answers are kept alongside it.
6. Otherwise, if any provider returns `conditional`, the result is `conditional`, with
   every condition.
7. Otherwise, the result is `allow`.

**Domains an action touches.** These are the action's own domain plus the domains its
effects reach.

- A Check on a call to a tool that writes Kubernetes objects always touches
  `kubernetes.authorization` and `kubernetes.admission`, whether or not Orka manages to
  derive the request.
- If derivation fails, those domains count as their required providers failing: `unknown`,
  `retry: later`.
- If the tool has no reusable request builder yet, they count the same way, but with
  `retry: never`.
- A write that creates several objects touches those domains once per derived request,
  and every one of them must be answered.

**Required providers.** The in-process default providers in section 4 are required for
the domains they serve.

**Optional providers.** An operator can mark an out-of-process provider optional, so its
outage doesn't block answers.

- An optional provider that fails or returns `unknown` doesn't decide the result.
- It does mark the result `complete: false`.

**What `allow` guarantees.** `allow` always means two things:

- at least one provider evaluated the action and nothing refused it;
- every required provider, for every domain the action touches, answered.

A provider failure never produces `allow` by itself.

### 4. Providers

**In-process providers** implement Go interfaces in `pkg/policy/v1alpha1`.

- `Info` is required. It returns:
  - the provider's name;
  - the contract version;
  - the domains and operations it supports;
  - the domains where its answers are authoritative.
- `Describer`, `Checker`, `Explainer` and `Observer` are each optional.

**Observer lifecycle.**

- Orka attaches before the prompt starts, using the stable part of the subject as the
  attachment key:
  - the Task UID and attempt;
  - the prompt ID;
  - the RuntimeSession UID and generation;
  - the Pod UID and container ID.
- The prompt's start time doesn't exist yet at attach time. Orka supplies it on detach and
  on Explain.
- On detach, it passes a reason:
  - `completed`;
  - `cancelled`;
  - `failed`, which covers an unknown outcome, a replaced session and a recycled pool.
- Attach is bounded and best-effort.
  - A failed or slow attach never blocks the prompt past that bound.
  - Explain then reports the window as incomplete.
- Attach and detach are idempotent on the attachment key.
- After a controller restart or a pool replacement, reconciliation detaches whatever is
  still attached.
- Providers also expire attachments after a bounded lifetime, so a lost detach can't leave
  a capture running.

**Out-of-process providers** implement the same operations as the gRPC service
`orka.policy.v1alpha1.DiscoveryProvider`. It uses mTLS and bounded messages. It is a
gRPC contract of the same kind as `orka.enforcement.v1` (#952) and `orka.storage.v1`
(#917).

The default providers need nothing extra installed:

| Provider | Operations | Authoritative |
| --- | --- | --- |
| `orka-policy` | Describe and Check, covering:<br>- the frozen tool and approval policies, and each tool's effect;<br>- workspace intent;<br>- publication constraints: `createPR`, `expectedRemoteSHA`, `maxChangedFiles`, `allowedPaths`, `denyRepositoryControlPaths`, `rejectBinaryFiles` and `rejectSecretLikeContent`;<br>- namespace scope;<br>- Task and delegation limits;<br>- Agent runtime ceilings, once #865 lands;<br>- transaction-token scope constraints, never the token itself;<br>- the OutboundAccessPolicy each allowed HTTP Tool references;<br>- fixed RuntimePool egress and the fixed writable paths;<br>- once #952 lands, an admitted enforcement requirement: format, capabilities and requirement digest. | Yes, tied to the frozen digests, with four exceptions:<br>- the enforcement requirement, whose receipt is historical (`authoritative: false`);<br>- fixed egress and writable paths, which the cluster's network plugin and container runtime enforce, not Orka (`authoritative: false`);<br>- the connector-read check for `list_connections`. It's authoritative only at that moment, because its scopes come from current controller configuration rather than a frozen digest. Its `versions` entry is a digest of the connector-read scopes and the enforcement mode, which the controller reads from its startup flags;<br>- OutboundAccessPolicies. The resolver reads each one live on every call, and only connector-backed Tools pin a policy's UID and generation at dispatch. So these answers are authoritative only at that moment. Each carries its own version: the policy's UID, its generation, a digest of its contents, and its readiness. |
| `kubernetes-authorization` | Check, with a SubjectAccessReview for the identity that will make the request: the controller or worker ServiceAccount. Never the agent's own identity, because ACP runtime Pods have no Kubernetes RBAC. | Yes, for RBAC at that moment |
| `kubernetes-dry-run` | Check, with a server-side dry run of the proposed object for that same identity. It records the denying webhook or policy, its message and any warnings. It covers ValidatingAdmissionPolicy, MutatingAdmissionPolicy, Gatekeeper, Kyverno, Orka's own policies, and built-in admission plugins such as ResourceQuota. It runs only when the agent asks. | Yes, for admission at that moment |

**Kubernetes Checks are tied to the agent's own tools.** Agents can't submit
`kubernetes.*` actions. A direct one gets `action_not_reachable` (section 3, step 1).

Instead, a Check goes like this:

1. The agent proposes a call to one of its allowed tools: `orka.tool`, verb `call`, with
   the tool name and arguments.
2. Orka checks the call against `orka-policy` first.
3. Orka derives a Kubernetes request only when `orka-policy` answers `allow` or
   `conditional`. For any other answer (`deny`, `requires_approval`, `unknown`, or a
   failure), that answer stands, because the real call wouldn't reach the API server
   either.
4. When it does derive one, Orka uses that tool's own request builder: the same kind, the
   same namespace scope and the same Orka-set fields.
5. Only that derived request goes to SubjectAccessReview and dry run.

The audit records the kind, namespace and name of each derived request.

**What Check discloses.** Only what the agent would learn by making the same call itself,
at that moment.

**Orka API authorization (`orka.api`).**

- **When it applies.** In chat and the compatibility API (Delivery step 7), a tool call
  also touches `orka.api`. Those are the callers whose tools run under the caller's own
  authorization hooks.
- **How it's checked.** `orka-policy` runs the tool's own authorization hooks for the
  subject: the same ones the real call runs, such as the Task-create and Secret-read
  checks. A refusal there is `deny`, because the real call would be refused too. Today that
  refusal is `authorization_failed`, or an unclassified error from `code_exec` (section 5).
- **Until then.** Discovery serves ACP sessions only. Describe reports `orka.api` as
  `not_covered` for every other caller.
- **Why ACP doesn't need it.** ACP brokered calls run under the Task's frozen authority.
  - The broker's tool context sets namespace isolation, which `orka-policy`'s namespace
    scope covers.
  - For `list_connections`, the broker also adds `AuthorizeConnectorRead`, which checks the
    Task's transaction scopes.
    - It refuses only when three things hold: enforcement is on, connector-read scopes are
      configured, and the Task has a transaction.
    - The read scopes come from controller configuration, not a frozen digest. So
      `orka-policy`'s answer for this check reflects the controller's current settings.
    - `orka-policy` evaluates it as part of the transaction-token scopes it covers.
  - The broker sets no other caller authorization hook.
- **Native AI workers don't need it either.** Their tools act with the worker's
  ServiceAccount, which `kubernetes-authorization` covers.

**Outside providers** plug in the same way:

- **Gatekeeper Runtime:**
  - Explain and Observe for `runtime.network`, replacing #640's direct client;
  - Describe from activation receipts, if Gatekeeper Runtime gains an API for that.

  **Evidence isolation.** Its evidence is container-scoped, so a session's own activity
  can't be separated from other sessions in the same container.
  - Explain returns that evidence only when the session is isolated, as #640 requires
    today: a dedicated, Task-specific pool with one session and one prompt.
  - A provider that can verifiably attribute evidence to a single session could lift that
    requirement.
  - Without either, Explain returns no Gatekeeper Runtime evidence. A RuntimeSession ID in
    the capture request doesn't isolate the events by itself.
  - Describe is unaffected. Policy in force on a container applies to every session in it,
    so describing it discloses nothing about other sessions.
- **Container enforcement providers from #952:** Describe and Explain, if they implement
  the discovery service.
- **AuthZEN decision points,** through an adapter.
- **Kyverno,** for payloads that aren't Kubernetes objects.
- **Configuration systems** such as ConfigHub or kpt.

**Registration.** It's operator-owned, and reuses the enforcement provider registry from
#952.

- **What #952's registry holds.** It lists exact Node incarnations (`nodeName`,
  `nodeUID`). For each Node incarnation it lists that Node's mandatory providers, each with:
  - `name`;
  - `endpoint`;
  - `serverName`;
  - the CA, client certificate and key files.
- **Discovery adds two things:**
  - **`services`.** Each provider entry gains a `services` field, which lists
    `enforcement`, `discovery` or both.
    - An entry without `services` is enforcement-only, so existing registries keep their
      meaning.
    - A discovery-only entry never counts as a mandatory enforcement provider.
  - **Cluster-scoped entries.** These are for providers that aren't tied to a Node, such
    as a decision point or a configuration system. They use the same name, endpoint,
    server identity and mTLS fields, outside any Node incarnation.
    - They can only serve `discovery`.
    - Per-Node entries stay the only way to register an enforcement provider.
- **Discovery fields.** Discovery entries also declare their domains, whether they're
  `required`, and their `disclosure` level.
- **Who can register.** Tasks and Agents can't add providers or choose trust.
- **If #952 changes.** If its registry lands in a different shape, discovery follows that
  shape. The in-process providers need no registration at all.

### 5. Refusal format

**Scope.** This covers every refusal Orka mediates that reaches the agent as a tool result
or a JSON-RPC response. A refusal is then classified even when the agent never called
Check.

**Shape.** Refusals keep the existing `ChatToolResult` fields (`success`, `data`, `error`,
`errorType`, `suggestion`) and add four optional ones:

- `code`: the specific reason within the `errorType` class;
- `retry`;
- `policy`, with `provider`, `enforcedBy` and `name`, for policy refusals;
- `untrustedMessage`: a bounded excerpt of API server or provider text.

**Messages.** For a refusal the shared classifier produces, `error` comes from a fixed
template, and any API server or provider text goes only into `untrustedMessage`. Existing
tools keep their current `error` text until they move to the shared classifier.

**Approval results** keep their existing `code` values, and gain `errorType` and `retry`.

**Closed vocabulary.** Existing `errorType` values keep their meaning. These are the new
classes, each with a closed set of codes:

| `errorType` | Codes | Meaning |
| --- | --- | --- |
| `not_allowed` | `tool_not_brokered` | The tool isn't an allowed brokered tool. This includes provider-native tools such as `Bash`: they can be in the policy, but they can't be called over MCP. |
| `policy_denied` | `admission_denied`, `ceiling_exceeded`, `outbound_access_denied`, `provider_denied`, `enforcement_refused` | A policy refused the request. |
| `approval` | `approval_pending`, `approval_declined`, `approval_expired`, `approval_cancelled`, `approval_stale` | The call needs a person, or needed one. |
| `outcome_unknown` | `tool_outcome_unknown` | The tool may have run. Don't repeat it automatically. |
| `no_effect` | `tool_failed_without_effect`, `request_not_sent` | The tool definitely did nothing. |
| `transient` | `broker_unavailable`, `connection_binding_unavailable`, `session_at_capacity`, `rate_limited`, `request_expired`, `token_exchange_unavailable`, `enforcement_uncertain` | Something was unavailable or busy, nothing happened, and trying later may help. |

**Other codes.**

- `internal_error`, an existing class, gains `tool_failed`. It covers a tool that failed
  with an unclassified error. It carries no `retry`, because Orka can't tell whether trying
  again would help.
- `invalid_arguments`, an existing class, gains `arguments_rejected`, with retry
  `with_changes`. It covers every rejection of model-supplied arguments, wherever it
  happens:
  - arguments that aren't a JSON object, that won't canonicalize, or that are too large,
    at the supervisor's digest or validation step;
  - a type that contradicts the tool's schema, in `Registry.Execute`.

  The broker already returns a registry argument rejection as a 200 `isError` result with
  `code: invalid_arguments`.
  - The refusal format puts it in the `invalid_arguments` class and renames the code to
    `arguments_rejected`.
  - The tool hasn't run, so the refusal isn't `outcome_unknown`. The exception is when a
    post-call check fails: prompt authority, approval revalidation, or committing the
    result. A consequential call then settles as unknown. An approval-required call, of
    either kind, settles as unknown and returns `tool_outcome_unknown`. A read-only call
    without approval has no effect record, and ends as a 502 (`internal_error` /
    `tool_failed`).

  **Retry rule.**
  - `retry: later` and `no_effect` need evidence that the whole invocation had no effect.
    Any of these counts:
    - the tool never started;
    - the endpoint or an enforcement point refused the request;
    - the tool is read-only.

    A refusal of one request doesn't count if an earlier request in the same call already
    had an effect. For example, `delegate_task` creates the child Task before a later
    token step can fail, and its cleanup is best-effort.
  - When a call that isn't known to be read-only could have had an effect and its outcome
    isn't confirmed, the refusal is `outcome_unknown` / `tool_outcome_unknown`,
    `retry: never`.
  - When Orka can't tell, it gives no retry guidance (`internal_error` / `tool_failed`).
    "Can't tell" means it knows neither the tool's effect class nor whether the request
    was sent, as with native and chat built-in tools today.
- `authorization_failed`, an existing class, gains `transaction_scope_denied` and
  `resource_authorization_denied`.
- The Check-only code `action_not_reachable` (section 3) is never a refusal.

**Which channel.** A refusal the model can act on comes back as an `isError` result,
because MCP clients should pass those to the model. That includes every case where nothing
happened and trying later may help.

JSON-RPC protocol errors are kept only for two cases: the agent's turn is already over, or
the session can no longer be trusted to continue. They add
`data: {"reason": …, "retry": "never"}`, with one of three reasons:

- `prompt_inactive`;
- `session_replaced`;
- `session_invalid`: the supervisor and broker no longer agree on the session's identity,
  its request format, or what an operation contains.

**Exceptions.** Two refusals stay outside this rule, and both are known gaps:

- the oversized-body `-32600`;
- the proxy-wide HTTP 429.

Each happens before a request can be parsed, so neither can carry a classified result.

| Surface | Today | Decision |
| --- | --- | --- |
| Proxy: not an allowed brokered tool | JSON-RPC `-32001` | `isError`: `not_allowed` / `tool_not_brokered`, `retry: never`. The message says when the name is a provider-native tool, and points to `tools/list` and `policy_describe`. |
| Supervisor: ACP permission request for a provider-native tool outside the policy | A rejected ACP permission outcome | Unchanged. An ACP permission outcome has no field for a reason, and the runtime decides what the model sees. `tools/list` lists only brokered tools, so its hints don't cover provider-native tools. Describe covers them before the call, but only for sessions that opt into `policy_describe`. For other sessions, this is a discovery gap. |
| Proxy: prompt authority inactive before the call | `-32001` | A protocol error with its own code: `prompt_inactive` |
| Proxy: prompt authority revoked during the call (lease expiry, deactivation, cancellation) | `-32002` | The broker may already have started the call, so it depends on the tool's frozen effect:<br>- consequential: `isError` with `outcome_unknown` / `tool_outcome_unknown`, `retry: never`;<br>- read-only: a protocol error, `prompt_inactive`. |
| Proxy: session at capacity | `-32003` | `isError`: `transient` / `session_at_capacity`, `retry: later` |
| Proxy: request body over the HTTP limit (arguments plus 64 KiB) | `-32600` "invalid MCP JSON-RPC request" | Unchanged, and a known gap, like the HTTP 429 below. The proxy can't parse the request, so the error carries no request ID, and the server may close the connection. Delivery step 1 records how each pinned runtime handles it. |
| Proxy: the proxy-wide connection limit | HTTP 429 "MCP proxy is at capacity" | Unchanged, and a known gap. The response comes before any JSON-RPC exchange exists, and a transport-level rejection can close a client's shared MCP stream. Delivery step 1 records how each pinned runtime handles it. |
| Supervisor: request not prepared or not sent (local digest, validation or signing failure) | `-32002` | Nothing was sent. It depends on why:<br>- prompt authority went inactive before sending: a protocol error, `prompt_inactive`;<br>- arguments that aren't a JSON object, won't canonicalize, or are too large, whether the digest step or validation catches them: `isError` with `invalid_arguments` / `arguments_rejected`, `retry: with_changes`;<br>- any other cause: `isError` with `no_effect` / `request_not_sent`, `retry: later`. |
| Supervisor: broker transport failure, invalid broker response, or call deadline | `-32002` | `isError`, depending on the tool's frozen effect. The proxy can't tell whether the broker ran a consequential tool.<br>- Consequential: `outcome_unknown` / `tool_outcome_unknown`, `retry: never`.<br>- Read-only: `transient` / `broker_unavailable`, `retry: later`. |
| Broker 400 or 403: the request's metadata expired in transit (it lives at most 30 seconds) | `-32002` | No tool ran. Depends on the prompt:<br>- prompt still active: `isError` with `transient` / `request_expired`, `retry: later`;<br>- prompt no longer active: a protocol error, `prompt_inactive`.<br><br>Today the expiry surfaces under generic messages: at `ValidateAt` as 400 "invalid MCP broker request", and at capability verification as 403 "MCP broker operation authorization failed". This is a broker change: report expiry as its own reason at both checks. |
| Broker 401: pool authentication failed; broker 400: a malformed request, or one that doesn't match its authenticated pool identity | `-32002` | Protocol error: `session_invalid`. No tool ran. The supervisor checks arguments with the same validation before sending, so a broker 400 means the supervisor and broker disagree, not that the model's arguments were wrong. |
| Broker 410: stale fence or policy | `-32002` | Protocol error: `session_replaced` |
| Broker 403: the call's operation capability can't be verified for a reason other than expiry, or the prompt isn't active | `-32002` | Protocol error: `prompt_inactive`. Either way, the call's authority can't be shown to be current. |
| Broker 503 before any tool runs: the broker is unavailable; approval storage is unavailable; or "MCP approval could not be recorded" (the approval request can't be stored before any approval exists) | `-32002` | `isError`: `transient` / `broker_unavailable`, `retry: later` |
| Broker 503 after an approved call may have run: "MCP approval continuation is unavailable; the original call must not be repeated" | `-32002` | `isError`: `outcome_unknown` / `tool_outcome_unknown`, `retry: never` |
| Broker 409: an approval call doesn't match its stored action, or an approval call's operation conflicts with a previous call (a reused call ID with a different payload) | `-32002` | Protocol error: `session_invalid`. No new tool run starts. |
| Broker 200 `isError` results the broker already shapes: `code: invalid_arguments` (registry argument rejection) and `code: tool_execution_failed` (a custom tool's failure, or a read-only tool's request deadline) | `isError` with those codes | Map them into the shared vocabulary:<br>- `invalid_arguments`: `invalid_arguments` / `arguments_rejected`, `retry: with_changes`;<br>- `tool_execution_failed` on a consequential tool: `outcome_unknown` / `tool_outcome_unknown`, `retry: never`. A failure isn't evidence of no effect;<br>- `tool_execution_failed` on a read-only tool: `internal_error` / `tool_failed`, with no `retry`.<br><br>A replayed receipt carries the same mapping as the original result. |
| Broker 502: tool execution failed, or the tool returned an invalid result | `-32002` | `isError`, depending on the cause:<br>- consequential tool whose outcome is settled as unknown: `outcome_unknown` / `tool_outcome_unknown`, `retry: never`;<br>- tool that reported no effect: `no_effect` / `tool_failed_without_effect`, `retry: with_changes`;<br>- read-only tool: `internal_error` / `tool_failed`, with no `retry`. |
| Broker: connection binding unavailable | `-32002` | `isError`: `transient` / `connection_binding_unavailable`, `retry: later` |
| A discovery tool over its rate limit | Not yet possible | `isError`: `transient` / `rate_limited`, `retry: later` |
| Approval outcomes | `isError` with a code | Codes stay the same; each gains `errorType` and `retry`:<br>- `approval_declined`: `approval`, `never`;<br>- `approval_expired`: `approval`, `after_approval`;<br>- `approval_cancelled`: `approval`, `never`;<br>- `approval_stale`: `approval`, `never`;<br>- `tool_outcome_unknown`: `outcome_unknown`, `never`;<br>- `tool_failed_without_effect`: `no_effect`, `with_changes`. |
| A call waiting for approval (#841) | Not yet possible | `approval` / `approval_pending`, `retry: after_approval` |
| Kubernetes errors in built-in tools | Where tools use the shared classifier, every 403 becomes `permission_denied` and anything unrecognized becomes `internal_error`. Some paths return raw errors instead. | Every Kubernetes error goes through one classifier:<br>- an RBAC 403, recognized from the authorizer's own message ("… cannot `<verb>` resource …"): stays `permission_denied`, `retry: never`;<br>- an admission denial: `policy_denied` / `admission_denied`, with the message in `untrustedMessage`. It's recognized at any status code in one of three ways:<br>&nbsp;&nbsp;- a webhook denial, by the message prefix `admission webhook "<name>" denied the request`. A webhook chooses its own code, and the API server returns 400 when it sets none;<br>&nbsp;&nbsp;- a ValidatingAdmissionPolicy denial, by `ValidatingAdmissionPolicy '<policy>' [with binding '<binding>'] denied request`. It appears after the `is forbidden: ` wrapper and again in `details.causes`. Its code comes from the policy's `reason`, and is 422 when none is set;<br>&nbsp;&nbsp;- a built-in plugin denial, by its own message. The recognized forms are:<br>&nbsp;&nbsp;&nbsp;&nbsp;- ResourceQuota's "exceeded quota";<br>&nbsp;&nbsp;&nbsp;&nbsp;- NamespaceLifecycle's "because it is being terminated".<br><br>LimitRange validates only Pods and PersistentVolumeClaims, and PodSecurity enforces only on Pods (it only warns on a Job). Built-in tools don't create Pods or PVCs directly. When a `code_exec` Job's Pod is rejected, the agent sees a Job that can't start, plus at most a create-time warning, not a refusal. That's a known gap.<br><br>A 403 with no positive admission evidence stays `permission_denied`. That includes Orka's own local authorization refusals, such as the compatibility API's "not authorized". A built-in denial in an unrecognized form also falls back to `permission_denied`, which is a known gap.<br><br>`enforcedBy` comes from the webhook or policy name in that text;<br>- a field-validation 422 that matches none of these: `invalid_arguments`, `retry: with_changes`.<br><br>Retry for `admission_denied` depends on the source:<br>- a webhook or a ValidatingAdmissionPolicy: `with_changes`;<br>- ResourceQuota ("exceeded quota"): `later`, since the quota can free up without any change;<br>- NamespaceLifecycle, for a terminating namespace: `never`.<br><br>Warnings go in `data.warnings`. |
| Task creation limit for the turn | `limit_reached` | Add `retry: never`. The limit doesn't reset within the turn, and the message says it resets on the next one. |
| Child transaction-token subset check or exchange | An unclassified error | - Refused: `authorization_failed` / `transaction_scope_denied`, `retry: never`.<br>- Exchange service unavailable: `transient` / `token_exchange_unavailable`, `retry: later`. |
| Outbound transaction-token exchange for a native worker's HTTP Tool call | An unclassified error ("Error executing tool: …") | Same split as for child Tasks: `transaction_scope_denied` when refused, `token_exchange_unavailable` when the service is down |
| An HTTP Tool's OutboundAccessPolicy refuses the call, in a native worker or the ACP broker | An unclassified error: "resolve outbound access policy: …", shown as "Error executing tool: …" or `-32002` | `policy_denied` / `outbound_access_denied`, `retry: never` |
| `code_exec` resource authorization (chat and the compatibility API) | An unclassified error: "resource authorization: …" | `authorization_failed` / `resource_authorization_denied`, `retry: never` |
| `list_connections` refused by `AuthorizeConnectorRead` (ACP broker) | `errorType: unauthorized_tool` | `authorization_failed` / `transaction_scope_denied`, `retry: never` |
| Native AI worker tool error | Free text "Error executing tool: …" | A `ChatToolResult`, depending on the error:<br>- `NoEffectError`: `no_effect` / `tool_failed_without_effect`;<br>- `ToolRequestAttemptedError`, where the request was sent but the outcome isn't confirmed: `outcome_unknown` / `tool_outcome_unknown`, `retry: never`. Native workers don't use a tool's effect class (`brokeredToolClass` is ACP-only), so this applies to every such error;<br>- anything else: `internal_error` / `tool_failed`. |
| Task failure | `FailedEvent{stopReason, code, message, retryable}` for ACP prompts; a Failed Task for other kinds | A policy-caused failure takes its `code` from the vocabulary above: `admission_denied`, `enforcement_refused` or `enforcement_uncertain`. #952's `EnforcementRefused` and `EnforcementUncertain` outcomes map to the last two. `FailedEvent.retryable` stays `false`, as it must for an accepted prompt; whether to retry the Task is up to the caller. |

**Review gate.** The proxy and broker changes ship only after a security review of the
broker's fences, because they replace deliberately uniform errors.

The new codes are a fixed vocabulary, and they reveal no policy contents. They do reveal
operational state the agent can't see today, and the review must clear each item:

- broker availability, session capacity and request expiry (`broker_unavailable`,
  `session_at_capacity`, `request_expired`);
- connection-binding state (`connection_binding_unavailable`);
- token-exchange availability (`token_exchange_unavailable`);
- local preparation failures (`request_not_sent`, `arguments_rejected`);
- session replacement and session integrity (`session_replaced`, `session_invalid`);
- the end of the agent's own turn (`prompt_inactive`).

### 6. Disclosure and limits

- **Orka-owned constraints:** full values. It's the caller's own policy, and `tools/list`
  already shows the allowlist.
- **Dry-run Check:**
  - It returns the API server's denial message as a bounded `untrustedMessage`. That's
    the same text the real request would return.
  - Describe never lists cluster admission policies. Their coverage is `check_only`.
- **Third-party providers:**
  - The default is `summary`: fixed templates and codes, with no provider text.
  - An operator can set `full` for each provider.
- **Size:** each result has a size limit. A result over the limit is cut at a constraint
  boundary and marked `complete: false`.
- **Rate and audit:** queries are rate-limited per prompt and audited. The audit records
  domains, decisions and counts, never payloads.

### 7. How agents use it

1. **`tools/list` hints.**
   - `annotations.readOnlyHint` comes from the tool's frozen effect.
   - `_meta["orka.ai/approvalRequired"]` marks approval-required tools.
   - Both are hints. Enforcement doesn't change.
2. **Three read-only brokered tools:**
   - `policy_describe {}`;
   - `policy_check {"actions": [...]}`;
   - `policy_explain {"domain": "..."}`, which replaces #640's `runtime_feedback`.
3. **Opt-in at first.** An Agent or Task lists these tools in its allowed tools.
   - Adding a tool to every Task would change the tool-policy digest in every RuntimePool
     profile.
   - That would replace every pool on upgrade.
4. **Session instructions.** Once Describe is stable, a bounded Describe summary goes into
   the session instructions, through the existing system prompt composition. The same
   digest consideration applies.
5. **Classified refusals** on every surface in section 5.
6. **Measurements:**
   - how often a refused call is repeated unchanged;
   - the share of refusals that are classified;
   - the share of answers that are `unknown`;
   - discovery calls per Task.

### 8. Where the code lives

**The module.** The contract is the module `github.com/orka-agents/orka/pkg/policy`, with
the package `v1alpha1` at `pkg/policy/v1alpha1`.

- A test keeps it free of Orka imports.
- It ships with a shared provider test suite.
- The proto goes under `proto/orka_policy/v1alpha1/`, added with the first
  out-of-process provider.

**Host code.** Subject binding, composition, limits, audit and the MCP tools stay under
`internal/`.

**When it moves out.** The contract moves to its own repository when either of these
happens:

- a second consumer adopts it;
- someone maintains a provider outside Orka.

### 9. Wire format

**Orka defines its own small schema, mapped to AuthZEN.**

- **Booleans lose information.** An AuthZEN decision is a boolean. A generic client would
  read `unknown` as a deny and miss `requires_approval`, because both would have to live
  in `context`, which such a client ignores.
- **Subject.** AuthZEN takes the subject from the request body. Here it must never come
  from the agent.
- **No equivalents.** Explain and Observe have no AuthZEN counterpart.

AuthZEN decision points plug in through an adapter:

| This contract | AuthZEN 1.0 |
| --- | --- |
| Subject, filled in by Orka | `subject`, set by the adapter from Orka's subject |
| `domain` and `verb` | `action.name` |
| `resource` | `resource` (`type`, `id`, `properties`) |
| `allow`, `deny` | `decision: true`, `decision: false` |
| `requires_approval`, `conditional` | `context` fields the adapter understands. An unrecognized `false` is `deny`. |
| A decision point that fails to answer | `unknown` |
| Check with several actions | `evaluations`, with `execute_all` semantics |
| Describe for one resource | Action search |
| Explain, Observe | No equivalent |

## Delivery

Implementation issues, in this order:

1. **`tools/list` hints.** First check, for each pinned runtime:
   - how it handles permission requests for tools hinted read-only;
   - how it handles the proxy's HTTP 429 and the oversized-body `-32600`.
2. **The first contract slice:**
   - the contract module;
   - the `orka-policy` provider's Describe;
   - opt-in `policy_describe` for ACP Tasks.
3. **The refusal format in built-in tools.**
   - Classify admission and RBAC errors, add `retry`, and return warnings.
   - Test against real API server errors, at least these four:
     - a ValidatingAdmissionPolicy without a `reason` (422);
     - one with `reason: Forbidden` (403);
     - a webhook that sets no code (400);
     - a ResourceQuota denial (403).
4. **Proxy and broker refusal classification,** after the security review.
5. **A Gatekeeper Runtime provider** with Explain and Observe, plus `policy_explain`. It
   replaces #640's direct integration.
   - Explain returns `runtime.*` evidence only for a session in a dedicated single-session
     pool.
   - #640's limits apply until they're lifted: native Deployment-backed pools only, and no
     `sessionRef`, execution workspaces, Claude, Copilot or external runtimes.
   - Once the workspace limit is lifted, a workspace-backed `acp-ws-*` pool would qualify.
6. **The `kubernetes-authorization` and `kubernetes-dry-run` providers,** plus
   `policy_check`.
7. **The same tools and refusal format** for native AI workers, chat and the compatibility
   API.

## Consequences

**What this adds.**

- A second kind of operator-registered provider. The enforcement registry gains a
  `services` list, and has to keep enforcement-only entries meaning what they mean today.
- Describe answers from the same frozen data the broker enforces, and carries the same
  digests. Any change to how tool policy is frozen has to keep the two in step.
- Some inputs aren't frozen, and are evaluated at the moment of the answer.
  - Connector-read scopes and OutboundAccessPolicies each carry their own version, so a
    changed constraint can't appear under an unchanged version.
  - The Kubernetes checks carry only `evaluatedAt` and `validUntil`. A SubjectAccessReview
    and a dry run return no version, so their answers can't be compared by version.

**What this rules out.**

- Discovery answers can't be used as authorization.
- The subject can't come from the agent.
- Provider text isn't copied by default.
- Dry runs don't run on every write.

**Changed behavior.** Code or tests that rely on today's results will need to change:

- matching the current `-32001` and `-32002` messages;
- expecting `internal_error` for admission denials;
- matching the codes the refusal format renames:
  - the broker's `code: invalid_arguments` becomes `arguments_rejected`;
  - `code: tool_execution_failed` becomes `tool_outcome_unknown` or `tool_failed`;
  - `errorType: unauthorized_tool` from the ACP broker's connector-read refusal for
    `list_connections` becomes `authorization_failed` / `transaction_scope_denied`. Its
    uses in chat and the compatibility API are unchanged until Delivery step 7.

**Limits.**

- **Opt-in tools.** Agents that don't opt in get only the `tools/list` hints and
  classified refusals.
- **In-container denials.** Denials inside the container, such as `EACCES` or a refused
  connection under kernel enforcement, never pass through Orka. Explain is the only way an
  agent learns about them, and only after the fact.
- **Dry-run accuracy.** A dry run is authoritative for admission only at that moment.
  Mutation, external data and quotas can differ on the real request.

**Versioning.** The contract starts at `v1alpha1`. A breaking change needs a new version,
and both versions are served while providers move over.

**External configuration ownership** isn't in the first version. That covers, for example,
a GitOps controller that will revert a direct edit. It arrives later, through
configuration providers.

### Alternatives considered

- **Building on #640 or #952 directly.**
  - #640 answers one question after the fact, for one engine.
  - #952 gates execution and never talks to the agent.
  - Either would tie discovery to one engine's model.
  - A separate contract lets both plug in as providers.
- **AuthZEN 1.0 as the wire format.** Section 9 covers why not.
- **Only marking tools in `tools/list`.**
  - It's cheap, and it ships first anyway.
  - But it can't describe egress, admission, workspace intent or publication limits.
  - It can't evaluate a proposed request either.
- **Discovery tools on for every Task.** That would change every RuntimePool's profile
  digest, so every pool would be replaced on upgrade. It would also add tools to sessions
  that never use them. Opt-in comes first, and measurement decides whether to change the
  default.
- **A dry run before every agent-initiated write.** That doubles the API calls. It would
  also fail a write that would have succeeded whenever the dry run itself is unavailable.
  Check runs when the agent asks, and the refusal format covers writes that go ahead
  without it.
- **Code placement:**
  - **A package inside Orka:** extracting it later would get harder over time.
  - **A separate repository now:** Orka would be its only consumer.
  - **Instead:** the nested module keeps the contract free of Orka while Orka proves it.
