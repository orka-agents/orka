# 39. Give Agents an external identity, starting with Microsoft Entra

Date: 2026-10-22, revised 2026-11-04

## Status

Proposed. Stages 1–3 are implemented; stage 1 is opt-in. Identity use records and stage 4 are not. Builds on ADR 0048 (Agent runtime ceilings) and
ADR 0033 (per-user connectors). Amends two ADR 0011 non-goals: "a central
credential broker", for agent identity tokens only, and "long-lived Entra OBO
lifecycle management", for stage 4 only. Agent user accounts and native `ai`
Agents stay out of scope.

## Context

ADR 0048 lets an Agent cap what a Task may make it do inside its runtime:
tools, Bash, and turns. It says nothing about what the Agent reaches outside
the cluster. Today an outbound Tool call authenticates as whatever its
`OutboundAccessPolicy` resolves: a Secret, a ServiceAccount token exchange,
the Task's transaction token, or the requesting person's Connection. None of
these names the Agent. A downstream service or an identity administrator
cannot tell which Agent acted, cannot grant one Agent a permission without
granting it to every Agent that shares the credential, and cannot switch off
one Agent.

An external identity provider can supply that principal. Orka's model has to
stay issuer-neutral, because operators run different providers, but it needs a
concrete first issuer to be honest about. Microsoft Entra Agent ID is that
first issuer
([overview](https://learn.microsoft.com/en-us/entra/agent-id/agent-identities),
[autonomous flow](https://learn.microsoft.com/en-us/entra/agent-id/autonomous-agent-authentication-authorization-flow),
[on-behalf-of flow](https://learn.microsoft.com/en-us/entra/agent-id/agent-on-behalf-of-oauth-flow)):

- An *agent identity* is a service principal with no credentials of its own.
  Tokens issued for it carry it as the subject or the client, and Entra
  sign-in logs record it. Its object ID and app ID are the same value.
- An *agent identity blueprint* holds the credential (Microsoft recommends a
  federated identity credential or a certificate, not a secret) and acquires
  tokens for every agent identity created from it. Administrators can apply
  Conditional Access to, disable, or revoke grants for all identities of one
  blueprint at once. "No managed identity per agent" is therefore true, but
  the blueprint still needs one credential.
- Every token takes two requests. The blueprint authenticates with
  `grant_type=client_credentials`, `scope=api://AzureADTokenExchange/.default`,
  and `fmi_path=<agent identity client ID>`, and receives an exchange token.
  For an app-only token, the exchange token is the `client_assertion` of a
  second `client_credentials` request for the target's `/.default` scope. For
  an on-behalf-of token, it is the client assertion of a `jwt-bearer` request
  whose `assertion` is a user token whose audience is the blueprint's client
  ID. A user token for any other audience is rejected.
- App-only permissions are app role assignments or admin consent on the agent
  identity. Delegated permissions are preauthorized through inheritable
  permissions on the blueprint, after admin consent, because agent identities
  cannot receive interactive consent.
- Agent identities are single-tenant.

Four facts about Orka today make a naive binding unsafe:

1. **Nobody authorizes the use of an Agent.** Creating a Task authorizes the
   Task, not the Agent it names. Workspace classes have a `use` check in the
   API (`internal/api/workspace_class_authorization.go`) and in admission
   (`internal/admission/workspace_class_use.go`); Agents have none. If an
   Agent carried an identity, anyone who can create a Task in its namespace
   would act as that identity.
2. **An installation-wide issuer credential is a master key.** With Entra, the caller chooses
   `fmi_path`, so whoever can present the blueprint's credential can mint
   tokens for any agent identity under it.
3. **Agent editors could claim someone else's identity.** An identity
   reference is just a string. Without a check, an Agent in one namespace
   could name the identity provisioned for another team's Agent.
4. **The direct adapter cannot express these flows, and should not.**
   `DirectOutboundAccess` supports only `TokenExchange` and `JWTBearer` grants
   and `None`, client-secret, or `PrivateKeyJWT` client authentication
   (`api/v1alpha1/outboundaccesspolicy_types.go`). `fmi_path` is not a
   reserved `additionalParameters` key, so stretching that adapter would hand
   the choice of identity to whoever writes the policy.

Orka also defines `gen_ai.agent.id` (`internal/tracing/genai/constants.go`)
but does not emit it, so there is no stable Agent attribution to join with an
issuer's logs.

## Decision

Land in four stages. Stages 1 and 2 are issuer-neutral. Stages 3 and 4 define
issuer-neutral contracts and ship Entra as their only implementation. Stage 3
requires stage 1; stage 4 requires stages 1 and 3 and ADR 0033 Connections.

### Stage 1: authorize the use of an Agent

- Add the SubjectAccessReview verb `use` on `agents.core.orka.ai`, with the
  Agent's name as the resource name and the Agent's own namespace, mirroring
  workspace classes. Every object that makes Orka run work as an Agent is
  covered: a Task's `agentRef`, a RepositoryScan's analysis and patch Agents,
  a RepositoryMonitor's role Agents, and a GatewayBinding's Agent. Only Agents
  an object newly names are checked, so unrelated edits keep working.
- The API checks its caller before it creates or edits such an object.
  ValidatingAdmissionPolicies check direct Kubernetes writes with the CEL
  authorizer, as the workspace-class policies do. A policy runs inside the
  API server, so unlike a webhook it cannot be unavailable while still
  admitting writes.
- Orka's own creators (the controller, API, gateways, monitors, and workers)
  hold `use` broadly, as the controller already holds `use` on workspace
  classes, and must check their own callers before naming an identity-bound
  Agent. Delegation between Agents stays bounded by the parent Agent's
  `coordination.allowedAgents`.
- The check is mandatory for every Agent that declares an identity: such an
  Agent is not Ready, holds no claim, and binds no Task unless the controller
  runs with `--agent-use-authorization-enabled`. The flag asserts that the
  `config/agent-use-policy` policies are installed; the Helm value
  `controller.agentUseAuthorization.enabled` sets the flag and installs the
  policies together, and the controller does not detect the policies itself.
  For other Agents the check is opt-in, because turning it on breaks every
  caller that lacks a `use` grant.
- The check runs when an object is created or edited, but the controller
  resolves a Task's Agent only when it binds the Task. A Task created while
  the check was off, or before its Agent declared an identity, was never
  checked for that identity. So a claim (stage 2) records when it was
  granted (`status.identityClaim.grantedAt`); claims are granted only while
  the check is enforced and the issuer is configured, and are released when
  the controller runs without either; and a Task binds an identity only if it
  was created after the claim was granted. A claim can outlive a run without
  the check when its Agent never reconciled during that run, so the
  controller also records each unbroken period of enforcement in a Lease,
  `orka-agent-use-enforcement`. A run without the check deletes it before
  serving requests and again once it leads. An enforcing run decides its
  period each time it wins leadership, never from what it read earlier, and
  identity decisions wait until the period is recorded. A period continues
  only across enforcing leaders that follow each other directly: the marker
  records the leader election Lease's UID and transition count under the
  last enforcing leader, and the next one keeps the marker only when that
  count is its own or the one just before it. Leader election advances the
  count whenever the holder changes, before the new leader runs anything,
  so any leader in between, including one without the check whose deletion
  had not yet run while it served requests, makes the next enforcing leader
  start a new period, after that leader stopped. Every Task writer in the
  controller runs only while it leads. Without leader election, every start
  begins a new period. A Task or source also binds only if it
  was created after the current period began. The grant time comes from the API server's clock, like
  creation times, never the controller's: a granted claim first carries no
  grant time and binds nothing, and the controller then records the latest
  write time the API server stamped on the Agent's `managedFields`, which is
  no earlier than the write that granted it. Both times are recorded to the
  second, so a Task created in the grant's own second, which may predate the
  grant, is refused too. A claim that stops releasing, because the Agent
  names the identity again or the check is back on, is granted again the
  same way. An older queued
  Task fails closed and must be created again, as does a Task created
  together with a new identity-bound Agent before or as its claim was
  granted, so such Tasks are created once the Agent is Ready. A scheduled
  Task never binds itself, so its runs bind only if the scheduled Task was
  also created after the grant; an older one must be re-created. The same
  holds for every durable object Orka creates Tasks from with its own broad
  `use` grant: a Task controlled by a RepositoryScan or RepositoryMonitor
  binds only if that object, matched by owner UID, was also created after
  the grant, and fails closed with "re-create" otherwise. Task provenance
  admission keeps that owner and the scan's or monitor's labels from being
  removed or changed, so the controller refuses to start an identity issuer
  without that admission. A Task that still carries those labels without its
  owner (as after an orphaning delete) is refused rather than judged as a
  root Task. GatewayBinding
  Tasks record their binding only by name, so they cannot prove this and are
  refused for identity-bound Agents. Before every runtime admission the
  dispatcher checks again that the controller still enforces the check, that
  the issuer instance the Task froze is still configured, and that the Agent
  still holds a non-releasing claim the Task satisfies, and otherwise fails
  the Task with `IdentityUnavailable`. The same check runs again just before
  every prompt submission, the first and any resent after a RuntimePool
  turned it away for capacity, since the RuntimeSession start and the wait
  can outlast the claim.
- A delegated child Task may name an identity-bound Agent only when it is the
  parent's own Agent. Delegation targets have no ceiling yet (ADR 0048), so a
  child must not borrow an identity its root requester was never checked
  against. This blocks the common pattern where a coordinator hands work to a
  different, identity-bound worker Agent. Lifting it needs a delegation
  ceiling (which Agents a parent may hand work to), which is outside this ADR.
  Workers hold Agent `use` broadly, so Task provenance admission also refuses
  a worker's Task that has no parent Task and names an identity-bound Agent;
  otherwise it would bind as a root Task that no `use` check covered. The API
  refuses the same request from a worker's ServiceAccount, because it writes
  Tasks as the controller, which admission trusts.

### Stage 2: bind an identity and attribute actions to it

- Add `Agent.spec.identity`, which sets exactly one issuer-typed member. The
  first is `entra.agentIdentityID`, the agent identity's client ID. Issuer
  instance settings, such as the Entra tenant and blueprint, are controller
  configuration, one instance per issuer type per Orka installation. A second
  issuer adds a sibling member, not a new field shape.
- Setting or changing the identity requires the SubjectAccessReview verb
  `bind` on the virtual resource `agentidentities.core.orka.ai`, with the
  issuer type as the subresource (for example `agentidentities/entra`) and the
  identity ID as the resource name, in the Agent's namespace. Operators grant
  it with a Role listing exact `resourceNames`. This is the pattern Kubernetes
  uses for binding Roles, and it settles who may claim which identity without
  a new CRD.
- One identity maps to one Agent UID. The controller records the holder in
  the Agent's `status.identityClaim`, granting a claim only when no other
  Agent holds it; a second Agent naming the identity reports `Ready=False`.
  An Agent that stops naming an identity keeps the claim until every Task
  bound to it has finished, so two Agents never act as one identity at once,
  but from that moment its bound Tasks' identity-backed calls are refused, as
  cloud IAM revocation applies to running workloads at their next call.
- The Task binding freezes the identity next to the Agent UID and generation
  that `AgentExecutionAgentRef` already pins, together with the issuer
  instance (for Entra, the tenant and blueprint). Editing the Agent or the
  controller configuration never moves a bound Task to another identity; a
  frozen issuer instance that no longer matches the controller's fails the
  call closed.
- An Agent with an identity must set `ceiling.allowedTools` (ADR 0048). Every
  tool a Task could use to carry identity-backed data out of Orka, such as a
  brokered fetch, an HTTP Tool, or a gateway reply, is then one the Agent
  owner listed explicitly. The ceiling does not cover the coordination tools
  Orka injects into delegated children (`send_message` and `check_messages`,
  ADR 0048), so an identity-bound child can pass data to its siblings,
  including Tasks whose Agent has no identity. A sibling is another child of
  the same parent, and an identity-bound child's parent runs the same Agent
  (stage 1), so those messages reach only Agents that Agent's
  `coordination.allowedAgents` lists, plus Agents the parent Task created
  itself with `create_agent`, which coordination admits without listing.
  The list, which its owner also sets, bounds the rest; nothing but the
  parent's own choices bounds the Agents it creates, and none of this is
  the ceiling.
- Emit `gen_ai.agent.id` as the Agent UID, not its reusable name, on Task and
  Tool-call spans, and write an identity use record for every identity-backed
  call (see below). Issuer logs and Orka records then share a join key.
- Until stage 3 ships, the identity is a declaration by an authorized Agent
  editor, not a verified fact, and status and UI must say so.

### Stage 3: the Agent acts as itself

- Add a fourth `OutboundAccessPolicy` adapter, `agentIdentity`, exclusive with
  `direct`, `gateway`, and `connection`. It selects a mode (this stage adds
  `self`) and holds the target in one issuer-typed block, such as
  `entra.resource` for Entra. It never names an identity: the identity always
  comes from the Task's frozen Agent binding, not from the policy, the Tool,
  or the Task spec.
- Only the controller implements the credential source, as with Connections,
  whose source "exists only in the controller; worker Pods have no
  implementation" (`internal/outboundaccess/resolver.go`). ACP Tasks reach it
  through the controller's MCP broker (`internal/controller/acp_mcp_broker.go`).
  Native `ai` workers and every other process fail closed.
- The issuer contract is small: given the frozen identity, a mode, the
  issuer-typed target, and the outbound request, authorize that request or
  fail closed. Each issuer type declares three properties, fixed in code
  rather than configuration:
  - **Output**: a bearer token in a header, or a signature over the request.
    Only bearer issuers use the policy's `header`.
  - **Credential model**: *installation-wide*, where one controller credential
    can act as every identity under it, or *per-identity*, where each identity
    has its own client credential. A per-identity credential is held in the
    controller's sealed store, as Connection tokens are (ADR 0033). It is
    attached to the identity binding, never stored on the Agent, and creating
    or replacing it requires the same `bind` permission.
  - **Delegated support**, used by stage 4.
- For installation-wide issuers, the controller authenticates with its own
  projected ServiceAccount token as a federated credential; a SPIFFE SVID from
  an operator-run SPIRE server is an alternative source for the same
  credential. The issuer's trust names exactly the controller's identity:
  issuer, subject, and audience. No other workload may run as that
  ServiceAccount, and nothing that can create Pods or Jobs, such as the native
  `ai` worker (`config/rbac/worker_role.yaml`), may do so in the controller's
  namespace. This authenticates the controller as a workload; it attests
  nothing about agent code or model behavior.
- Entra implementation: installation-wide, bearer output. The blueprint's
  federated identity credential trusts the controller ServiceAccount, and the
  controller performs the two app-only requests above.
- Tokens are injected per Tool call. They never enter the ACP process tree,
  the Task spec or status, the execution snapshot, events, logs, or metrics.
  The cache is keyed by a digest of the identity, mode, and target. In
  delegated mode (stage 4) the key also holds a digest of the requester's own
  user token from their Connection, so two requesters never share an entry,
  and a refreshed or relinked Connection starts a new one. An entry lives at
  most five minutes. Because nothing else holds the token, disabling the
  identity at the issuer stops new calls within that bound.
- Before every token request, including one the cache answers, the
  controller checks the live Agent: it must still exist with the bound UID
  and hold a claim on the frozen identity that is not being released.
  Removing `spec.identity` or deleting the Agent therefore stops new calls at
  once, without changing the identity the Task's binding froze. The claim
  itself is kept, marked releasing, until every Task bound to it has
  finished, so the identity cannot be given to another Agent while those
  Tasks run.
- A token goes only to the destination the Tool fixes. The audience names
  the resource a token is for, not the host that receives it, so the broker
  refuses an `agentIdentity` policy on a Tool whose URL has template
  placeholders, which a call's arguments would fill, and on MCP-backed Tools,
  as the `connection` adapter already does. Whoever may edit a Tool that the
  Agent's ceiling lists still decides where that Agent's token goes, so that
  permission is as sensitive as the identity.
- Reserve `fmi_path`, `user_id`, `username`, and
  `user_federated_identity_credential` in `DirectOutboundAccess`
  `additionalParameters`, so the direct adapter can never select an identity.
- An identity is verified only after the controller has acquired a token for
  it. With Entra, the controller requests the blueprint's exchange token for
  the identity, which proves the identity is a child of the configured
  blueprint, which stage 2 cannot, and then the identity's own token, because
  Entra still issues the exchange token for a disabled identity. The controller keeps the issuer's
  answer, verified or refused, for one Agent generation and asks again when
  the Agent is edited and at least hourly, so the Agent's own status writes
  never call the issuer again early. A failure, for example an identity
  disabled or deleted at the issuer, sets the Agent `Ready=False` and fails
  new calls; deleting the Agent fails identity-backed calls from Tasks still
  bound to it.

### Stage 4: the Agent acts for its requester

- Add the mode `delegated` to the `agentIdentity` adapter. Its guarantee is
  that both sides are enforced: the downstream service enforces the
  requester's own permissions, and the issuer or resource enforces what was
  granted to that Agent. Each issuer type declares where it can deliver that:
  everywhere, only for named resource types, or nowhere. The controller
  refuses `delegated` for any other target and never falls back to a token
  that names only the requester; a policy author who accepts that uses the
  `connection` adapter instead, and the Agent then appears only in Orka's
  records.
- The requester's proof is their own Connection (ADR 0033) for an issuer-typed
  `ConnectorProvider`, linked through that provider's consent flow. The Task's
  `spec.requestedBy` selects it, exactly as the `connection` adapter does.
  ServiceAccount callers, Tasks without a verified requester, and delegated
  child Tasks whose root requester differs fail closed.
- Only some Tasks have a requester who can delegate. API-created Tasks carry
  the caller's verified `requestedBy`, and chat and delegated child Tasks
  inherit it. Gateway Tasks carry a gateway-issued sender, which matches no
  Connection until an explicit sender link exists (ADR 0033).
  RepositoryMonitor and other controller-created Tasks carry none. The last
  two therefore fail closed with no extra rule.
- Delegation expires. Each delegated policy sets `maxDelegationAge`, default
  one hour, with a controller-configured maximum. It is measured from the
  root requester's last authenticated action in the Task chain: creating the
  root Task, or sending the Session message that started the current turn. A
  child Task, retry, or parked agent woken later does not reset it; only the
  requester acting again does. Cancelling or deleting any Task in the chain
  stops delegated calls from every Task below it; a parent that finished
  normally does not. After it elapses, delegated calls fail closed
  and the Task can ask the requester to continue. A policy that asks for more
  than the controller's maximum gets the maximum, and its `Accepted` condition
  says so.
- Delegated mode runs only where Orka controls the runtime's network. Built-in
  RuntimePool Pods deny all egress except cluster DNS, the model provider
  proxy, and the controller, so identity-backed data cannot reach an
  arbitrary host directly; tools leave through brokered channels that the
  Agent ceiling bounds. DNS is not such a channel: cluster DNS forwards names
  it does not serve, so a runtime that can run commands or resolve arbitrary
  names can encode data in lookups of a domain an attacker controls. Orka
  does not restrict DNS names, and this residual risk remains. Where it
  matters, deny Bash and native fetch tools in the ceiling, or serve the
  runtime namespace a DNS that refuses external names. External `runtimeRef`
  runtimes and workspace-backed sessions are refused until their egress
  governance is verified. This depends on the cluster enforcing
  NetworkPolicy.
- Write-class calls in delegated mode always require approval, and only the
  requester can approve them: the approver must match the Task's
  `requestedBy` issuer and subject. Other authorized reviewers may deny but
  not approve, because the write acts as that person. Today any caller allowed
  to update the Task can decide an approval, so this is a new rule for this
  mode only.
- The Connection must have been issued for the bound identity. Each issuer
  type defines the check, such as the user token's audience. Where an issuer
  addresses user tokens to each agent client, a requester links once per
  identity; where it addresses them to one installation-wide client, once per
  issuer. A mismatch fails closed.
- Orka's Session or Task is the application session the token is bound to.
  Before every exchange and every Tool call, the controller checks that the
  Task is running, its Session is open, the live Agent still holds this
  identity as stage 3 requires, and the Connection is Ready. Ending the Task
  or Session, deleting the Connection, or removing the identity from the
  Agent or deleting the Agent stops new calls. A token already issued is
  still valid at the issuer until it expires; because it never leaves the
  controller, the residual exposure is the cache bound above. Orka
  does not claim issuer-side per-session revocation, and does not rely on
  Continuous Access Evaluation.
- Delegated policies must also declare the operations they allow: target
  hosts, HTTP methods, and path prefixes. The broker checks each request
  against them before it injects a token and fails closed otherwise. Issuer
  scopes are often too coarse to do this (see the Entra notes), and a model
  can be steered, for example by a malicious document, into requests that sit
  within the requester's rights but outside what they asked for. Neither the
  model nor the Tool arguments can widen the list.
- Entra implementation: delegated everywhere Entra issues delegated tokens.
  The `ConnectorProvider` is a front-end Entra client that requests the
  blueprint's custom scope, so the Connection holds a user token whose audience
  is the blueprint, which is also the identity check; requesters link once per
  installation. The blueprint's redirect URI only records consent, so it
  cannot be that client. The controller performs the two on-behalf-of requests
  above. Orka's GitHub dashboard sign-in cannot supply this token; the
  requester must sign in through Entra.

### Identity use records

`ExternalEffect` records are idempotency records for writes. They keep only
digests, and Task cleanup removes them, so they cannot serve as attribution.
The GitHub mutation records in the controller store are the precedent instead.

- The controller appends one immutable record per identity-backed call, reads
  and writes alike: Agent UID and generation, issuer type, issuer instance and
  identity ID, mode, Task UID, policy UID and generation, the `allow` entry
  that matched, outcome, request digest, and time. Records never contain
  tokens, request bodies, or full URLs.
- In delegated mode the requester is stored as a keyed (HMAC) pseudonym under
  a controller-held key, so records can be joined per person without exposing
  who the person is to every reader.
- Records outlive Task deletion. Retention is operator-configured, defaulting
  to 90 days. Reading them requires its own permission, separate from Task
  read.

### Implementation notes

- **Availability.** The broker and token cache run in the controller leader.
  Failover loses only cached tokens, which are fetched again. Per-identity
  credentials and use records live in the controller store, so they share its
  single-writer availability until the storage HA work lands.
- **Testing.** A fake issuer tests the contract and fail-closed paths; Keycloak
  in kind covers a real token endpoint; an optional live-tenant lane, like the
  existing GitHub OIDC lane, covers Entra. Stage 4 against Azure Storage needs
  that live lane before it is documented as supported.
- **Operations.** Metrics count issued, cached, refused, and failed calls per
  issuer and reason, without identity IDs as labels. Issuer throttling and
  outages fail calls closed, so alerts watch the failure rate.
- **Rollout.** Each stage ships behind its own flag. The `use` check stays
  opt-in for Agents without an identity, and existing `connection` policies
  are unchanged.

### Entra notes for operators

These are properties of Azure, not of Orka, but they decide whether a
deployment delivers the guarantees above.

- **Azure Storage has one delegated scope, `user_impersonation`**
  ([Storage authorization](https://learn.microsoft.com/en-us/rest/api/storageservices/authorize-with-azure-active-directory)).
  Entra can decide whether an Agent may reach Storage for a user at all, not
  which containers or operations. Narrowing belongs to the stage 4 operation
  list and to Storage's own RBAC.
- **Azure RBAC grants are additive**
  ([ABAC security](https://learn.microsoft.com/en-us/azure/storage/blobs/storage-auth-abac-security)).
  Granting a reader role with a condition does not cap a broader role the user
  already holds. Shared Key, SAS, and ADLS ACLs bypass role conditions
  entirely; disable Shared Key where delegated agent access must be bounded.
- **Storage ABAC conditions see Entra attributes, not Orka state**
  ([ABAC attributes](https://learn.microsoft.com/en-us/azure/storage/blobs/storage-auth-abac-attributes)).
  Principal attributes are Entra custom security attributes. Orka Agents,
  Sessions, and arbitrary token claims are not condition inputs, so Orka
  enforces the Agent and Session side before the call.
- **Tenants.** Agent identities exist in one tenant, and a resource accepts
  tokens from its own tenant. Each tenant needs its own blueprint
  and agent identities.
- **`offline_access` and linking.** Entra issues a refresh token only when
  the front-end client requests `offline_access`, but it leaves that scope out
  of the granted scopes it reports, so Orka refuses such a link with
  `scopes_denied` ([#1030](https://github.com/orka-agents/orka/issues/1030)).
  Until that is fixed, request only the blueprint's custom scope; the link then
  lasts until its access token expires, and the requester links again.

### Configuring Entra Agent ID

An illustrative walk-through. Flag names and fields that this ADR does not
fix above are placeholders until implementation.

1. **In Entra**, an administrator creates one agent identity blueprint per Orka
   installation. Its federated identity credential trusts the controller
   ServiceAccount: the cluster's OIDC issuer, subject
   `system:serviceaccount:<controller namespace>:<controller ServiceAccount>`,
   and audience `api://AzureADTokenExchange`. The administrator creates one
   agent identity per Agent under the blueprint and grants it app roles for
   stage 3. For stage 4, the administrator also configures inheritable
   delegated permissions on the blueprint with admin consent, a custom scope on
   the blueprint, and a separate front-end client app registration that
   requests that scope.
2. **The controller** is configured once with the tenant and blueprint, for
   example `--agent-identity-entra-tenant-id` and
   `--agent-identity-entra-blueprint-client-id`. It authenticates with its own
   projected ServiceAccount token; no secret is stored.
3. **An operator** lets a namespace claim one identity and lets callers use the
   Agent:

   ```yaml
   apiVersion: rbac.authorization.k8s.io/v1
   kind: Role
   metadata:
     namespace: team-a
     name: inventory-agent-identity
   rules:
   - apiGroups: [core.orka.ai]
     resources: [agentidentities/entra]
     verbs: [bind]
     resourceNames: ["<agent identity client ID>"]
   - apiGroups: [core.orka.ai]
     resources: [agents]
     verbs: [use]
     resourceNames: [inventory-agent]
   ```

4. **The Agent owner** binds the identity:

   ```yaml
   apiVersion: core.orka.ai/v1alpha1
   kind: Agent
   metadata:
     namespace: team-a
     name: inventory-agent
   spec:
     identity:
       entra:
         agentIdentityID: "<agent identity client ID>"
     runtime:
       ceiling:
         allowedTools: [read_report, list_reports]   # required with an identity
   ```

5. **A policy author** chooses how outbound calls use it. The policy names a
   target and a mode, never an identity:

   ```yaml
   # Stage 3: the Agent acts as itself.
   agentIdentity:
     mode: self
     header: Authorization
     entra:
       resource: https://graph.microsoft.com
   ---
   # Stage 4: the Agent acts for its requester, who has linked an Entra
   # Connection through a ConnectorProvider for the front-end client app.
   agentIdentity:
     mode: delegated
     maxDelegationAge: 1h
     header: Authorization
     entra:
       resource: https://storage.azure.com
     allow:
     - host: inventory.blob.core.windows.net
       methods: [GET]
       pathPrefixes: [/reports/]
   ```

A second issuer reuses every step except the first two: it adds a sibling of
`entra` under `Agent.spec.identity` and in the policy, a matching
`agentidentities/<issuer>` subresource, and its own controller configuration.

### Other issuers

Not committed. These mappings show that the contract above does not assume
Entra; each needs its own decision and integration proof before it ships.

| Issuer | Agent principal | Credential model | Output | Delegated |
| --- | --- | --- | --- | --- |
| Microsoft Entra Agent ID | Agent identity | Installation-wide (blueprint) | Bearer | Everywhere |
| Google Cloud | Per-Agent service account, impersonated after Workload Identity Federation | Installation-wide | Bearer | Nowhere; no documented on-behalf-of exchange for Google APIs |
| AWS | Per-Agent IAM role assumed with `SourceIdentity` | Installation-wide (EKS Pod Identity or IRSA) | Request signature (SigV4) | Only services that honor identity-enhanced sessions |
| Okta Cross App Access (ID-JAG) | Per-Agent OAuth client | Per-identity | Bearer | Applications that accept ID-JAG |
| Keycloak | Per-Agent client | Per-identity | Bearer | To be verified; RFC 8693 delegation support is unconfirmed |

- **Google Cloud.** Google's Agent Identity is tied to Google-hosted agent
  runtimes, and external agents must federate
  ([Agent Identity](https://docs.cloud.google.com/iam/docs/agent-identity-overview)).
  Orka federates the controller and impersonates one service account per
  Agent; Cloud Audit Logs record the impersonation.
- **AWS.** AgentCore workload tokens are limited to AgentCore's own services
  ([AgentCore Runtime auth](https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/runtime-oauth.html)),
  so Orka assumes one IAM role per Agent. Delegated access uses
  identity-enhanced role sessions
  ([IAM Identity Center](https://docs.aws.amazon.com/singlesignon/latest/userguide/trustedidentitypropagation-identity-enhanced-iam-role-sessions.html)),
  which only services such as S3 Access Grants evaluate. Its temporary
  credentials sign requests, so it needs the request-signature output.
- **Okta Cross App Access.** ID-JAG exchanges the requester's identity
  assertion at the identity provider, then presents the result to the target
  application's authorization server
  ([Okta](https://support.okta.com/help/s/article/understanding-cross-app-access-with-okta-for-ai-agents)).
  It needs two token endpoints and per-Agent client credentials, and may need
  one Connection per identity.
- **Keycloak.** A self-hosted issuer and the practical test issuer for kind
  E2E, without a live cloud tenant.
- **SPIFFE.** Not an issuer for downstream APIs. A SPIRE server can supply the
  controller's own federated credential for installation-wide issuers.

### Relationship to ADR 0048

The ceiling and the identity bound different things and stay separate checks.
The ceiling bounds what a Task may make the Agent do inside its runtime; issuer
permissions bound what the identity may do outside it. Neither can be derived
from the other: one lists tool names, the other app roles or scopes. An Agent
owner who wants to limit which identity-backed resources a Task may reach can
later add `ceiling.agentIdentityResources`, checked at binding like the rest of
the ceiling; the stage 4 operation list is then checked against it.

## Rejected alternatives

- **A per-Agent ServiceAccount federated to its identity.** Entra agent
  identities cannot hold credentials; only the blueprint can. A per-Agent
  ServiceAccount is also mintable by anyone who can create a Pod naming it.
- **The issuer credential in the runtime Pod or a sidecar**, as in the
  Microsoft Entra SDK sidecar pattern. The runtime runs agent-controlled code,
  and the blueprint credential impersonates every identity under it.
- **Handing issued tokens to the runtime**, even short-lived delegated ones. A
  prompt-injected agent can exfiltrate a bearer token, and the broker could no
  longer check Session state or the operation list per call.
- **Extending the direct adapter** with `client_credentials`, a ServiceAccount
  client assertion, and `fmi_path` as a parameter. That turns impersonation
  into a policy author's choice.
- **A provider-neutral `AgentIdentityIssuer` CRD now.** One issuer does not
  justify a new resource; controller configuration per issuer type is enough
  until a second issuer has a concrete consumer.
- **Deriving the ceiling from issuer permissions**, or the reverse. They do
  not share a vocabulary.

## Consequences

- An Agent becomes a principal outside Orka. Identity administrators can
  grant, scope, disable, and audit each Agent, and downstream services see
  which Agent called, alone or for whom.
- The controller becomes a credential broker for agent identity tokens, which
  amends ADR 0011; on acceptance, 0011's status points here. A compromised
  controller already exposes Connection tokens; it would now also expose every
  agent identity under an installation-wide issuer credential, and, in stage
  4, delegated access for every linked requester. Per-identity issuers limit a
  stolen credential to one identity, but the controller still holds them all.
  Stronger separation needs separate issuer credentials, which means separate
  Orka installations.
- ADR 0011 keeps provider specifics out of core. The adapter, the Agent field,
  the `bind` check, and the issuer properties are issuer-neutral; the Entra
  blueprint flows are the one provider-specific implementation in core, behind
  that contract.
- The issuer must reach the cluster's ServiceAccount issuer. AKS publishes it;
  kind and other private clusters must publish their discovery document and
  JWKS.
- An issuer outage fails identity-backed Tool calls closed, with no fallback
  to another credential.
- Requiring `use` for all Agents is a breaking change and stays opt-in.
- Proven in a live tenant on 2026-11-05: Self-mode tokens and Entra agent
  on-behalf-of tokens against Microsoft Graph, including refusals for a wrong
  audience, an expired delegation, and a disabled identity. Not yet proven:
  on-behalf-of tokens against Azure Storage, and identity provisioning
  latency. Stage 4 needs the Storage proof before Orka documents it as
  supported there.
- One issuer instance per installation means one Entra tenant per Orka
  installation. Serving several customer tenants needs several installations
  until per-namespace issuer references are added; the frozen issuer instance
  keeps that later change from affecting bound Tasks.
- Not covered: inbound tokens addressed to an agent identity, multi-tenant
  blueprints, native `ai` Agents, delegated mode on external or
  workspace-backed runtimes, and delegation to any Agent other than the
  parent's own.
