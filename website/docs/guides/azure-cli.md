---
slug: /guides/azure-cli
description: "Let built-in coding agents run az against Azure Resource Manager without holding an Azure credential."
---

# The Azure CLI

An agent that uses `az` on your laptop relies on your signed-in account. In an
Orka runtime there is no account to rely on, and Orka never puts one there.
Instead, the runtime presents itself to `az` as an Azure cloud:

```text
agent runs:  az group list
   az (from a toolbox) is already signed in: Orka wrote its profile, and its token is a placeholder that works only in this session
   az sends the request to the runtime's loopback endpoint, not to Azure
      → the controller checks the Task may use the forwarding Tool
      → adds the real token: the Agent's own Entra identity, or the requester's linked account
      → sends GET https://management.azure.com/... and returns Azure's answer
```

The agent's commands stay the same. Azure role-based access control on the
identity you choose decides what they can see.

## What works

- **Read requests to Azure Resource Manager**: `az group list`, `az vm show`,
  `az aks list`, `az resource list`, `az account list`, and any other command
  that only reads ARM. Paging works.
- **Writes and actions, when the Tool allows them.** `az group create`,
  `az vm start`, `az aks create`, and `az aks get-credentials` send `PUT`,
  `PATCH`, `DELETE`, or `POST`. A Tool without `forward.writes` refuses them
  with `OrkaReadOnly`; one with it waits for a person to approve each write
  unless the Tool waives approval ([Writes](#writes)).
- **Not other Azure services.** Commands that call Microsoft Graph (`az ad`),
  Key Vault data, Storage data, or `az ssh` fail with
  `OrkaResourceNotForwarded` or a connection error.
- **Only while the agent is working.** A request outside a running prompt, or
  from a Task that does not allow the Tool, is refused with
  `OrkaNotAuthorized`.
- **`az rest` takes a relative URL**: `az rest --url "/subscriptions?api-version=2022-12-01"`.
  `az` prefixes it with the runtime's endpoint. An absolute
  `https://management.azure.com/...` URL bypasses Orka, and the runtime's
  network policy blocks it, so the command hangs until it times out.
- **No `az login`.** When the agent's first turn starts, Orka lists the
  identity's subscriptions through the forwarding Tool and writes them as
  `az`'s profile, with the first enabled one as the default. That read is
  Orka's, not the agent's: it runs once per session, it is recorded like any
  other call to the Tool, and it is skipped when reads of the Tool need
  approval. Until it finishes, `az account show` names the account
  "Orka is still signing az in; run the command again", and a command that
  runs is answered with `OrkaSignedIn`; run it again. After `az account clear`, `az login --identity`
  signs `az` in again.
- `az account show` reports a tenant ID of all zeros. Subscription IDs and
  everything Resource Manager returns are real. An identity with no role on a
  subscription gets a tenant-level account, and commands that need a
  subscription fail with `OrkaNoSubscription`.

## 1. Build the toolbox

[`examples/azure-cli`](https://github.com/orka-agents/orka/tree/main/examples/azure-cli)
has the Dockerfile. It installs a self-contained Python and the stock Azure
CLI under `/opt/azure-cli`. The image carries no credential, and Orka needs
nothing else from it: any toolbox that puts `az` on `PATH` works the same
way.

```bash
docker buildx build --platform linux/amd64,linux/arm64 \
  -t registry.example.com/tools/azure-cli:2.92.0 --push examples/azure-cli
crane digest registry.example.com/tools/azure-cli:2.92.0
```

Enable toolboxes and allow your registry as described in
[Toolboxes](toolboxes.md). The image is about 150 MB, so prefer
`mountMethod: imageVolume` where your cluster supports it.

## 2. Choose whose access az uses

### As the Agent

The Agent reads Azure as its own Entra agent identity. Set the identity up
first ([Agent identities](agent-identities.md), steps 1 to 5), then give that
identity an Azure role, for example **Reader** on one resource group:

```bash
az role assignment create --role Reader \
  --assignee-object-id "<agent identity appId>" --assignee-principal-type ServicePrincipal \
  --scope "$(az group show -n <resource group> --query id -o tsv)"
```

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: OutboundAccessPolicy
metadata:
  namespace: team-a
  name: azure-as-agent
spec:
  agentIdentity:
    mode: Self
    entra:
      resource: https://management.azure.com
```

### As the person who asked

Each person links their own Azure account. Register an Entra app with
the delegated permission **Azure Service Management → user_impersonation**,
then declare it as a ConnectorProvider whose only tool is the forwarding one:

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: ConnectorProvider
metadata:
  namespace: team-a
  name: azure
spec:
  displayName: Azure
  oauth:
    authorizeURL: https://login.microsoftonline.com/<tenant ID>/oauth2/v2.0/authorize
    tokenURL: https://login.microsoftonline.com/<tenant ID>/oauth2/v2.0/token
    clientID: <app registration appId>
    clientSecretRef: {name: azure-connector, key: client-secret}
    clientAuthentication: ClientSecretPost
    scopes:
      read: ["https://management.azure.com/user_impersonation"]
  tools:
    - name: azurearm
      class: read
      source: HTTP
      description: Azure Resource Manager reads for the Azure CLI.
      http:
        url: https://management.azure.com
        forward:
          protocol: AzureResourceManager
---
apiVersion: core.orka.ai/v1alpha1
kind: OutboundAccessPolicy
metadata:
  namespace: team-a
  name: azure-as-person
spec:
  connection:
    providerRef: {name: azure}
```

```bash
orka connect azure -n team-a
```

Request only the `user_impersonation` scope. Entra leaves `offline_access` out
of the scopes it reports as granted, so asking for it makes linking fail
([#1030](https://github.com/orka-agents/orka/issues/1030)). Without it there
is no refresh token: a link lasts until its access token expires, about an
hour, and then the person runs `orka connect azure` again. Until they do, `az`
fails with `OrkaConnectionRequired` and a message that names the command.

The Agent can also act for the requester through its own identity
(`agentIdentity.mode: Delegated`); add the same `azurearm` tool to the sign-in
provider described in [Agent identities](agent-identities.md#act-for-the-requester-delegated).

## 3. Declare the forwarding Tool

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Tool
metadata:
  namespace: team-a
  name: azurearm
spec:
  description: Azure Resource Manager reads for the Azure CLI.
  brokeredToolClass: read
  http:
    url: https://management.azure.com
    forward:
      protocol: AzureResourceManager
    outboundAccessPolicyRef:
      name: azure-as-agent        # or azure-as-person
```

A forwarding Tool is not offered to the model. Its rules are checked at
admission (except `parameters`, which only the controller can read) and again
before each Task:

- `url` is an HTTPS origin with no path.
- Its name is not one of a coding agent's built-in tools (`bash`, `edit`,
  `glob`, `grep`, `read`, `webfetch`, `websearch`, `write`) or one the Orka
  MCP broker serves, such as `remember`, `web_fetch`, or `create_pull_request`.
- `brokeredToolClass` is `read`.
- It sets no `parameters`, `headers`, `authSecretRef`, or MCP backend.
- Its credential comes from an `outboundAccessPolicyRef` that injects one
  (`agentIdentity`, `connection`, or `direct`), not a gateway, and sends it
  as `Authorization: Bearer`, the default. A policy with another `output`
  header or prefix is refused, because Azure would refuse every request.
- When the policy draws on a ConnectorProvider, the provider declares a tool
  of the same name with the same `url` and `forward`.

### Writes

Add `writes` to let `az` change resources:

```yaml
  http:
    url: https://management.azure.com
    forward:
      protocol: AzureResourceManager
      writes:
        approval: Required          # the default; None sends writes without approval
```

- Each write is a separate tool, `azurearm:write`, that Orka derives from the
  Tool. Allowing `azurearm` (and an Agent ceiling that names it) allows both;
  an allowlist that names `azurearm:write` is refused. A Task keeps the reads
  and refuses writes with `disallowedTools: [azurearm:write]`. Reads stay
  unapproved.
- With `approval: Required`, a write waits in the Task's **Approvals** tab (or
  for `orka task approve`) showing its method, path, and body. When it
  creates or updates a resource group, AKS cluster or agent pool, or virtual
  machine, sizing values such as `location`, `sku`, `kubernetesVersion`, and
  each agent pool's `count` and `vmSize` are shown when they look like
  sizing values (up to 64 letters, digits, spaces, `.`, `_`, or `-`); every
  other value is masked because any of them can be a secret. Meanwhile `az`
  waits for the answer. A decision is final: `az` gets Azure's response, or
  `OrkaApprovalNotGranted` if it was declined or expired after 10 minutes.
  When the Agent acts for the requester (`agentIdentity.mode: Delegated`), only
  the requester can approve.
- While a write waits, the Task stays `Running`. `orka task get <task>` shows
  `Waiting on: approval`, `orka task approvals <task>` shows the request, and
  `orka task approve <task> <id>` or `orka task decline <task> <id>` answers
  it. The **Approvals** tab needs the dashboard, which is off by default: set
  `controller.dashboard.enabled: true` and open it with `orka dashboard`.
- A request ends with the agent's turn. If the turn ends first, the request is
  cancelled and nothing is sent to Azure, even though it showed time left; ask
  for the write again, and tell the agent in the prompt to wait for approval.
  Stopping `az` alone does not withdraw it: while the turn goes on, the request
  stays pending, and approving it still sends the write. To make sure a write is
  never sent, decline it, or cancel the Task with `orka task cancel <task>`.
- `az` resends a request that has had no answer for 5 minutes. The resent
  request joins the pending approval, so a write is never sent twice. Orka
  keeps no write's answer, which can carry a secret, so a resent request
  whose write already ran gets `OrkaResultNotKept`: check the resource, or
  run the command again.
- Write bodies must be a JSON object or array. Every write runs once: if Orka cannot tell
  whether it reached Azure, `az` gets `OrkaOutcomeUnknown` and the write is
  not repeated.
- `POST` actions can return secrets: `az aks get-credentials` returns a
  kubeconfig and `az storage account keys list` returns keys, and the agent
  can read them. Keep `approval: Required` on a Tool whose identity can reach
  such actions, or grant that identity a role that cannot.
- For a linked account, the provider's tool needs `writes` too (its
  `approval` is not used), and each person links with
  `orka connect azure -n team-a --mode readWrite`. Adding `writes` to a
  provider asks everyone to consent again. With a `readOnly` link, or none,
  the agent gets reads only.

## 4. Give the Agent the toolbox and the Tool

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Agent
metadata:
  namespace: team-a
  name: azure-reader
spec:
  model:
    name: <model>
  identity:                         # only for the "as the Agent" choice
    entra:
      agentIdentityID: "<agent identity appId>"
  runtime:
    type: codex
    contractVersion: orka.harness.v2
    toolboxes:
      - image: registry.example.com/tools/azure-cli@sha256:<digest>
        mountPath: /opt/azure-cli
        pathEntries: [bin]
    defaultAllowedTools: [Read, Write, Edit, Bash, Glob, Grep, WebSearch, WebFetch, azurearm]
    ceiling:
      allowedTools: [Read, Write, Edit, Bash, Glob, Grep, WebSearch, WebFetch, azurearm]
```

- An agent identity belongs to one Agent. If the Agent from the
  [Agent identities](agent-identities.md) guide already holds it, add the
  `toolboxes` entry and `azurearm` to that Agent instead of creating this
  one, or create another agent identity for this Agent. A second Agent that
  names a held identity reports `Ready=False`.
- The agent needs `Bash` to run `az`, and the Task must allow `azurearm`.
- Allow at most one Azure forwarding Tool per Agent. A session that would
  have two fails to start, so which identity `az` uses is never ambiguous.

Run a Task such as "List my resource groups and their locations." The agent
runs `az group list` as usual:

```bash
orka task create "List my resource groups and their locations." \
  --agent azure-reader --name az-groups -n team-a
orka task wait az-groups -n team-a
orka task get az-groups -n team-a
```

## Troubleshooting

| What az prints | Why |
| --- | --- |
| `(OrkaReadOnly)` | The command writes or acts on a resource, and this Task has no write tool: the Tool has no `forward.writes`, the Task disallows it, or the requester's link was not `readWrite` when the Task started. |
| `(OrkaApprovalNotGranted)` | A reviewer declined the write, or nobody approved it within 10 minutes. Nothing was sent to Azure. |
| `(OrkaResultNotKept)` | `az` resent a write that already ran, for example after a slow approval. Orka keeps no write's answer, which can carry secrets; check the resource, or run the command again. |
| `(OrkaResendRefused)` | `az` resent a write during a later turn than the one that first sent it, for example a command left running in the background. Orka did not send it again; check the resource, or run the command again. A session that has sent 16384 writes also refuses new ones; start a new session. |
| `(OrkaThrottled)` | Azure throttled the write and did not process it, and this session has already had 256 throttled writes, so Orka cannot resend it safely. Run the command again later. |
| `(OrkaOutcomeUnknown)` | The write may have reached Azure. Check the resource before running the command again. |
| `(OrkaResourceNotForwarded)` | The command needs a service other than Resource Manager. |
| `(OrkaNotAuthorized)` | No prompt is running, or the Task does not allow the forwarding Tool. |
| `(OrkaConnectionRequired)` | The person who asked has no usable linked account: none, expired, revoked, or changed since the Task started. The message names the `orka connect` command; run it, then run the Task again. |
| `(OrkaToolRefused)` or `(OrkaBrokerError)` | The controller refused or could not complete the call: for example, the identity is disabled, the policy changed, or a write used a `readOnly` link. For an agent identity, the `agent-identity-audit` log says why. |
| `(AuthorizationFailed)` | Azure refused the identity. Give it a role on the scope you are reading. |
| `(OrkaSignedIn)` | The command started before Orka finished signing `az` in, or under a sign-in that is out of date. Orka has signed in now; run the command again. |
| `(OrkaNoSubscription)` | The identity has no role on any subscription, so `az` has no subscription to use. Give it a role on one, then run the command again. |
| `Please run 'az login'` | `az` forgot its sign-in, for example after `az account clear`: run `az login --identity`. If that fails with one of the codes above, that row says why. If it cannot reach a managed identity endpoint at all, the Task does not allow the forwarding Tool, so the session has no Azure endpoint. |
