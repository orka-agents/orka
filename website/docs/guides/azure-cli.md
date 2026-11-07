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
   az (from a toolbox) signs in with a placeholder that works only in this session
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
- **Not writes or actions.** `az group create`, `az vm start`, and
  `az storage account keys list` send `PUT`, `POST`, or `DELETE` and are
  refused with `OrkaReadOnly`.
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
- `az account show` reports a tenant ID of all zeros. Subscription IDs and
  everything Resource Manager returns are real. With no subscription, `az`
  signs in to a tenant-level account and commands that need a subscription
  say so.

## 1. Build the toolbox

[`examples/azure-cli`](https://github.com/orka-agents/orka/tree/main/examples/azure-cli)
has the Dockerfile. It installs a self-contained Python and the Azure CLI
under `/opt/azure-cli`, plus a small `az` wrapper that signs in on the first
command. The image carries no credential.

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
identity an Azure role, for example **Reader** on one resource group.

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
  (`agentIdentity`, `connection`, or `direct`), not a gateway.
- When the policy draws on a ConnectorProvider, the provider declares a tool
  of the same name with the same `url` and `forward`.

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

- The agent needs `Bash` to run `az`, and the Task must allow `azurearm`.
- Allow at most one Azure forwarding Tool per Agent. A session that would
  have two fails to start, so which identity `az` uses is never ambiguous.

Run a Task such as "List my resource groups and their locations." The agent
runs `az group list` as usual.

## Troubleshooting

| What az prints | Why |
| --- | --- |
| `(OrkaReadOnly)` | The command writes or acts on a resource. Only reads are forwarded. |
| `(OrkaResourceNotForwarded)` | The command needs a service other than Resource Manager. |
| `(OrkaNotAuthorized)` | No prompt is running, or the Task does not allow the forwarding Tool. |
| `(OrkaConnectionRequired)` | The person who asked has no usable linked account: none, expired, revoked, or changed since the Task started. The message names the `orka connect` command; run it, then run the Task again. |
| `(OrkaToolRefused)` or `(OrkaBrokerError)` | The controller refused or could not complete the call: for example, the identity is disabled or the policy changed. For an agent identity, the `agent-identity-audit` log says why. |
| `(AuthorizationFailed)` | Azure refused the identity. Give it a role on the scope you are reading. |
| `Please run 'az login'` | The Agent's toolbox is not this wrapper, or the Task does not allow the forwarding Tool, so the session has no Azure endpoint. |
