---
slug: /agent-identities
description: "Giving an Agent its own Microsoft Entra agent identity, so its calls are made and audited as that Agent, or on behalf of the person who started the Task."
---

# Agent identities with Microsoft Entra

An Agent can have its own identity in an external identity provider. Tools
that call outside services then authenticate as that Agent, not as the
namespace or a shared service account. The first issuer is a
[Microsoft Entra agent identity](https://learn.microsoft.com/entra/agent-id/agent-identities).

There are two ways to use it:

- **Self:** the Agent calls a resource as itself, with the app roles granted
  to its agent identity. For example, an inventory agent that may read one
  storage container.
- **Delegated:** the Agent calls a resource on behalf of the person who
  started the Task, with on-behalf-of tokens. Entra limits the call to what
  the agent identity was granted, and the resource limits it to what that
  person may do.

This guide sets up both end to end. The design is
[ADR 0039](https://github.com/orka-agents/orka/blob/main/docs/adr/0039-agent-identities.md).

:::note Delegated mode is read-only today

Delegated mode runs only on built-in runtimes (codex, claude, copilot,
opencode), because those are the only ones whose network Orka controls.
Built-in runtimes cannot ask a person for approval yet
([#770](https://github.com/orka-agents/orka/issues/770)), and Orka hides
write-class connector tools from runtimes that cannot ask. Until that lands,
delegated Tools can only read.

:::

## How it fits together

1. In Entra, an **agent identity blueprint** stands for this Orka
   installation. Its federated identity credential trusts the controller's
   Kubernetes ServiceAccount, so no client secret is stored.
2. Each Agent that needs an identity gets an **agent identity** under the
   blueprint, with the app roles it needs.
3. In Kubernetes, an **Agent** names its agent identity. Claiming an identity
   needs the `bind` permission on that exact identity, and running work as an
   identity-bound Agent needs `use` on the Agent.
4. An **OutboundAccessPolicy** in `agentIdentity` mode says which resource a
   **Tool** calls and how. The policy never names an identity: it always
   comes from the Task's Agent.
5. When a Task calls the Tool, the controller asks Entra for a token and
   injects it into that one request. Tokens never reach the agent runtime,
   the Task, or logs, and every call writes an audit record.

## Before you start

- A cluster whose ServiceAccount token issuer is reachable by Entra. On AKS,
  enable the OIDC issuer and read its URL with
  `az aks show -g <group> -n <cluster> --query oidcIssuerProfile.issuerUrl -o tsv`.
- An `az` session for the tenant, as someone who holds these Entra roles, or an
  administrator who holds them to run the steps that need them:
  - **Agent ID Administrator** to create the blueprint and agent identities
    (steps 1 and 2);
  - **Privileged Role Administrator** to grant Microsoft Graph app roles
    (step 2), the least-privileged built-in role for that
    [assignment](https://learn.microsoft.com/en-us/graph/api/serviceprincipal-post-approleassignments#permissions);
  - for Delegated mode, **Cloud Application Administrator** or **Application
    Administrator** to grant the tenant-wide delegated consent in steps 7 and 8
    ([admin consent roles](https://learn.microsoft.com/en-us/entra/identity/enterprise-apps/grant-admin-consent#prerequisites)).
- Orka installed with Helm in the namespace `team-a`, with built-in RuntimePools.
  A release reconciles only its own namespace, so everything below lives there.
- The `baseline` Pod Security profile enforced on that namespace
  (`kubectl label ns team-a pod-security.kubernetes.io/enforce=baseline`), so no
  privileged or `hostPath` Pod can read the controller's blueprint credential
  from its node. Admission checks only new Pods, so first run the label with
  `--dry-run=server`; it lists running Pods that break the profile. Delete
  them before you label the namespace.
- For Delegated mode, also:
  - Orka's API reachable over HTTPS by the people who will link accounts;
  - Entra sign-in configured for Orka (step 8);
  - a cluster that enforces NetworkPolicy.

The steps call Microsoft Graph with `curl`. Keep these values in your shell:

```bash
TENANT=<tenant ID>
ISSUER=<cluster OIDC issuer URL>
ORKA_SA=system:serviceaccount:team-a:orka   # the controller ServiceAccount
SPONSOR=$(az ad signed-in-user show --query id -o tsv)   # the person responsible for the identities
TOKEN=$(az account get-access-token --tenant "$TENANT" \
  --scope https://graph.microsoft.com/.default --query accessToken -o tsv)
graph() { curl -s -X "$1" -H "Authorization: Bearer $TOKEN" \
  -H "OData-Version: 4.0" -H "Content-Type: application/json" \
  "https://graph.microsoft.com/$2" ${3:+-d "$3"}; }
```

## 1. Create the blueprint

Create the blueprint and its service principal, then let it trust the
controller's ServiceAccount. `SPONSOR` is the object ID of the person
responsible for it: you, as set above, or another user's object ID.

```bash
graph POST v1.0/applications '{
  "@odata.type": "Microsoft.Graph.AgentIdentityBlueprint",
  "displayName": "orka-<cluster>",
  "sponsors@odata.bind": ["https://graph.microsoft.com/v1.0/users/'"$SPONSOR"'"],
  "owners@odata.bind": ["https://graph.microsoft.com/v1.0/users/'"$SPONSOR"'"]
}'
BLUEPRINT=<appId from the response>
BLUEPRINT_OBJ=<id from the response>

graph POST v1.0/serviceprincipals/microsoft.graph.agentIdentityBlueprintPrincipal \
  '{"appId": "'"$BLUEPRINT"'"}'

graph POST "v1.0/applications/$BLUEPRINT_OBJ/federatedIdentityCredentials" '{
  "name": "orka-controller",
  "issuer": "'"$ISSUER"'",
  "subject": "'"$ORKA_SA"'",
  "audiences": ["api://AzureADTokenExchange"]
}'
```

The subject must match the controller's ServiceAccount exactly, including its
namespace.

## 2. Create an agent identity and grant it app roles

```bash
graph POST beta/serviceprincipals/Microsoft.Graph.AgentIdentity '{
  "displayName": "inventory-agent",
  "agentIdentityBlueprintId": "'"$BLUEPRINT"'",
  "sponsors@odata.bind": ["https://graph.microsoft.com/v1.0/users/'"$SPONSOR"'"]
}'
IDENTITY=<id from the response>
```

An agent identity's object ID and its client ID (`appId`) are the same value,
so `$IDENTITY` serves both as the service principal in Graph URLs and as the
`agentIdentityID` and RBAC resource name below.

Grant it the app roles its Tools need, as for any service principal. For
example, Microsoft Graph's `User.ReadBasic.All`:

```bash
read GRAPH_SP ROLE < <(graph GET "v1.0/servicePrincipals?\$filter=appId%20eq%20'00000003-0000-0000-c000-000000000000'&\$select=id,appRoles" |
  python3 -c "import json,sys;s=json.load(sys.stdin)['value'][0];print(s['id'],[a['id'] for a in s['appRoles'] if a['value']=='User.ReadBasic.All'][0])")
graph POST "v1.0/servicePrincipals/$IDENTITY/appRoleAssignments" \
  '{"principalId": "'"$IDENTITY"'", "resourceId": "'"$GRAPH_SP"'", "appRoleId": "'"$ROLE"'"}'
```

For Azure resources, assign Azure RBAC roles to the agent identity instead.
Only grant what the Agent's Tools need: these permissions are the upper bound
of anything the Agent can do as itself.

## 3. Configure Orka

Set the tenant and blueprint, and turn on the `use` check, which
identity-bound Agents require:

```yaml
controller:
  agentUseAuthorization:
    enabled: true
  agentIdentity:
    entra:
      tenantID: <tenant ID>
      blueprintClientID: <blueprint appId>
      # authority: https://login.microsoftonline.us   # sovereign clouds only
```

With `tenantID` set, the chart mounts a projected ServiceAccount token
audienced to `api://AzureADTokenExchange` into the controller, and the
controller uses it as the blueprint's credential.

:::caution `use` applies to every Agent

With `agentUseAuthorization.enabled`, creating a Task, RepositoryScan,
RepositoryMonitor, or GatewayBinding needs `use` on the Agents it names, for
all Agents, not only identity-bound ones. Grant it before you turn it on.

:::

## 4. Let a namespace claim the identity and use the Agent

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  namespace: team-a
  name: inventory-agent
rules:
- apiGroups: [core.orka.ai]
  resources: [agentidentities/entra]
  verbs: [bind]
  resourceNames: ["<agent identity appId>"]   # who may claim this identity
- apiGroups: [core.orka.ai]
  resources: [agents]
  verbs: [use]
  resourceNames: [inventory-agent]            # who may run work as the Agent
```

Bind it to whoever edits the Agent (`bind`) and whoever starts its Tasks
(`use`). A cluster administrator already holds both. Everyone else needs a
binding, including the ServiceAccount whose token `orka` uses:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  namespace: team-a
  name: inventory-agent
subjects:
- kind: ServiceAccount            # a ServiceAccount that starts Tasks
  namespace: team-a
  name: orka-client
- kind: User                      # a person signed in through Entra (step 9)
  apiGroup: rbac.authorization.k8s.io
  name: "https://login.microsoftonline.com/<tenant ID>/v2.0#<subject>"
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: inventory-agent
```

A person signed in through Entra is checked as the `User` `<issuer>#<sub>`
from their sign-in token, not their email address: `orka auth whoami -o json`
shows their `issuer` and `subject`.

## 5. Bind the identity to an Agent

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Agent
metadata:
  namespace: team-a
  name: inventory-agent
spec:
  model:
    name: <model>
  identity:
    entra:
      agentIdentityID: "<agent identity appId>"
  runtime:
    type: codex
    contractVersion: orka.harness.v2
    defaultAllowedTools: [Read, Write, Edit, Bash, Glob, Grep, WebSearch, WebFetch, graph-users]
    ceiling:
      allowedTools: [Read, Write, Edit, Bash, Glob, Grep, WebSearch, WebFetch, graph-users]
```

- An Agent with an identity must set `runtime.ceiling.allowedTools`, so every
  Tool that can carry identity-backed data is one the Agent owner listed.
- List the runtime's built-in tools as well as your own, in both lists. The
  codex runtime enforces two built-in tool sets: all eight shown above, or
  exactly `Read`, `Glob`, and `Grep` for Tasks whose workspace intent is
  `read` (the default for a Task that names no workspace). An Agent that only
  reads should use the smaller set, which leaves out shell, file writes, and
  web access; brokered tools and Tools such as `graph-users` are listed on top
  of either. OpenCode's are `Read`, `Write`,
  `Edit`, `apply_patch`, `Bash`, `Glob`, `Grep`, and it also needs
  `model.contextWindow` and `model.maxTokens`.
- One identity belongs to one Agent. The first Agent to claim it holds it in
  `status.identityClaim`; another Agent naming it reports `Ready=False`.

Check it:

```bash
kubectl -n team-a get agent inventory-agent \
  -o jsonpath='{.status.identityClaim}{"\n"}{range .status.conditions[*]}{.type}={.status} {.message}{"\n"}{end}'
```

`IdentityVerified=True` means the controller obtained a token for the
identity from Entra. The controller keeps that answer for an hour, or until
the Agent is edited, so an identity disabled in Entra turns it `False` and the
Agent `Ready=False` within the hour, not at once. Token requests are not
held to that hour: new tokens are refused as soon as Entra refuses them (see
[What stops identity-backed calls](#what-stops-identity-backed-calls)). A
change to the Agent's spec checks again at once; metadata such as an
annotation does not.

## 6. Call a resource as the Agent (Self)

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: OutboundAccessPolicy
metadata:
  namespace: team-a
  name: graph-as-agent
spec:
  agentIdentity:
    mode: Self
    entra:
      resource: https://graph.microsoft.com
---
apiVersion: core.orka.ai/v1alpha1
kind: Tool
metadata:
  namespace: team-a
  name: graph-users
spec:
  description: Return the display name of one user in the directory.
  brokeredToolClass: read
  parameters: {type: object, properties: {}, additionalProperties: false}
  http:
    url: "https://graph.microsoft.com/v1.0/users?$top=1&$select=displayName"
    method: GET
    outboundAccessPolicyRef:
      name: graph-as-agent
```

Run a Task on the Agent that calls `graph-users`. With the API reachable and
`orka` configured as in [Connect to the API](../getting-started.md#connect-to-the-api):

```bash
orka task create "Call graph-users and tell me the display name it returns." \
  --agent inventory-agent --name graph-self -n team-a
orka task wait graph-self -n team-a
orka task get graph-self -n team-a
```

The controller requests a token for `<resource>/.default` as the agent
identity and injects it as `Authorization: Bearer`. The Tool URL must be
HTTPS, without template placeholders.

The token goes wherever the Tool's URL points. Whoever may edit a Tool that
the Agent's ceiling lists therefore decides where that Agent's token is sent,
so treat edit access to those Tools as you treat the identity itself.

Each call writes one audit record to the controller log under the
`agent-identity-audit` logger:

```bash
kubectl -n team-a logs deploy/orka-controller | grep agent-identity-audit
```

A record names:
- the policy, the Agent and its UID, the identity, and the Task UID;
- the Tool and its method, and a digest of the call's arguments;
- for Delegated calls, a keyed pseudonym of the requester;
- the outcome (`issued` or `refused`) and, for a refusal, its reason: `policy`,
  `binding`, `delegation`, `issuer`, or `canceled` (see Troubleshooting).

It never contains a token. `issued` means a token was handed out, not that the
resource accepted the request.

## Act for the requester (Delegated)

Delegated mode needs more on both sides. The person who starts the Task must
sign in to Orka through Entra, so Orka knows who they are, and must link an
Entra account once, so Orka holds a user token it can exchange on their
behalf.

### 7. Give the blueprint a scope and delegated permissions

The person's linked token must be addressed to the blueprint, so give the
blueprint an API with one scope:

```bash
SCOPE_ID=$(python3 -c "import uuid;print(uuid.uuid4())")
graph PATCH "v1.0/applications/$BLUEPRINT_OBJ" '{
  "identifierUris": ["api://'"$BLUEPRINT"'"],
  "api": {"oauth2PermissionScopes": [{
    "id": "'"$SCOPE_ID"'", "value": "access_agent", "type": "User", "isEnabled": true,
    "adminConsentDisplayName": "Access agent",
    "adminConsentDescription": "Allow the application to access the agent on behalf of the signed-in user."
  }]}
}'
```

Then decide which delegated permissions agent identities under the blueprint
may use, and consent to them. For Microsoft Graph's `User.Read`:

```bash
BLUEPRINT_SP=<object ID of the blueprint principal from step 1>
graph POST v1.0/oauth2PermissionGrants '{"clientId": "'"$BLUEPRINT_SP"'",
  "consentType": "AllPrincipals", "resourceId": "'"$GRAPH_SP"'", "scope": "User.Read"}'
graph POST "v1.0/applications/microsoft.graph.agentIdentityBlueprint/$BLUEPRINT_OBJ/inheritablePermissions" '{
  "resourceAppId": "00000003-0000-0000-c000-000000000000",
  "inheritableScopes": {"@odata.type": "#microsoft.graph.allAllowedScopes", "kind": "allAllowed"},
  "inheritableRoles": {"@odata.type": "#microsoft.graph.noRoles", "kind": "none"}
}'
```

These permissions are the most any delegated call can do. Each call can only
do less: the person's own rights on the resource still apply.

### 8. Create the front-end app

People sign in to this app, both to link their account and to sign in to
Orka. Its redirect URI is Orka's connector callback:

```bash
graph POST v1.0/applications '{
  "displayName": "orka-signin",
  "signInAudience": "AzureADMyOrg",
  "isFallbackPublicClient": true,
  "web": {"redirectUris": ["https://orka.example.com/api/v1/connections/callback"]},
  "api": {"requestedAccessTokenVersion": 2, "oauth2PermissionScopes": [{
    "id": "'"$(python3 -c "import uuid;print(uuid.uuid4())")"'", "value": "orka.access", "type": "User",
    "isEnabled": true, "adminConsentDisplayName": "Use Orka", "adminConsentDescription": "Sign in to the Orka API."}]},
  "requiredResourceAccess": [{"resourceAppId": "'"$BLUEPRINT"'",
    "resourceAccess": [{"id": "'"$SCOPE_ID"'", "type": "Scope"}]}]
}'
FRONTEND=<appId>; FRONTEND_OBJ=<id>
graph PATCH "v1.0/applications/$FRONTEND_OBJ" '{"identifierUris": ["api://'"$FRONTEND"'"]}'
graph POST v1.0/servicePrincipals '{"appId": "'"$FRONTEND"'"}'
FRONTEND_SP=<id from the response>
graph POST v1.0/oauth2PermissionGrants '{"clientId": "'"$FRONTEND_SP"'",
  "consentType": "AllPrincipals", "resourceId": "'"$BLUEPRINT_SP"'", "scope": "access_agent"}'
graph POST v1.0/oauth2PermissionGrants '{"clientId": "'"$FRONTEND_SP"'",
  "consentType": "AllPrincipals", "resourceId": "'"$FRONTEND_SP"'", "scope": "orka.access"}'
```

Create a client secret for the connector and store it in Kubernetes without
printing it:

```bash
graph POST "v1.0/applications/$FRONTEND_OBJ/addPassword" '{"passwordCredential": {"displayName": "orka-connector"}}' |
  python3 -c "import json,sys;sys.stdout.write(json.load(sys.stdin)['secretText'])" |
  kubectl -n team-a create secret generic orka-signin --from-file=client-secret=/dev/stdin
```

### 9. Turn on connectors and Entra sign-in

```yaml
controller:
  connectors:
    enabled: true
    callbackBaseUrl: https://orka.example.com
  oidc:
    issuer: https://login.microsoftonline.com/<tenant ID>/v2.0
    audience: <front-end appId>
    allowedSubjects: "<subject>,<subject>"   # Entra subjects allowed to use Orka
    namespace: team-a
```

People sign in with a token for `api://<front-end appId>/orka.access`. The
Task's requester is then their verified Entra identity.

Entra gives each person a different subject for each app, so a person's
subject for this app is not known until they sign in to it. Have them sign in
(step 10 shows how) and run `orka -t "$ORKA_TOKEN" auth whoami`. Until they
are on the list, Orka answers `403` and names the subject to add:

```text
your sign-in is valid, but its subject "<subject>" is not allowed to use this Orka installation
```

Add it to `allowedSubjects` and upgrade the release. A pattern with `*`
matches any run of characters, so `"*"` admits every account the front-end
app accepts; list subjects one by one unless that is what you want.

:::caution Signed-in users are equal within their namespace

Today, every subject on `allowedSubjects` can use every API route in the
OIDC namespace. Kubernetes RBAC is not checked for them, except `use` and
`bind`. Treat the allow-list as the access list.
[#1037](https://github.com/orka-agents/orka/issues/1037) tracks authorizing
signed-in users with RBAC.

:::

### 10. Describe the linked account and its tools

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: ConnectorProvider
metadata:
  namespace: team-a
  name: entra-signin
spec:
  displayName: Microsoft Entra
  oauth:
    authorizeURL: https://login.microsoftonline.com/<tenant ID>/oauth2/v2.0/authorize
    tokenURL: https://login.microsoftonline.com/<tenant ID>/oauth2/v2.0/token
    clientID: <front-end appId>
    clientSecretRef: {name: orka-signin, key: client-secret}
    clientAuthentication: ClientSecretPost
    scopes:
      read: ["api://<blueprint appId>/access_agent"]
  tools:
  - name: graphme
    class: read
    source: HTTP
    description: Return the signed-in person's own profile.
    parameters: {type: object, properties: {}, additionalProperties: false}
    http:
      url: "https://graph.microsoft.com/v1.0/me?$select=displayName,userPrincipalName"
      method: GET
```

- Request only the blueprint's scope. Entra leaves `offline_access` out of
  the scopes it reports as granted, so asking for it makes linking fail with
  `scopes_denied` ([#1030](https://github.com/orka-agents/orka/issues/1030)).
  Without it there is no refresh token, so a link lasts until its access
  token expires, about an hour, and then the person links again.
- The provider's tools are the only operations a delegated token is released
  for. Each must have a `Tool` of the same name. Provider tool names reject
  hyphens and Kubernetes `Tool` names reject underscores, so a name both
  accept starts with a lowercase letter and uses only lowercase letters and
  digits, such as `graphme`
  ([#1031](https://github.com/orka-agents/orka/issues/1031)).

Each person signs in to Orka with a token for the front-end app, here with
the device-code flow that `isFallbackPublicClient` enables, and then links
once:

```bash
DC=$(curl -s https://login.microsoftonline.com/$TENANT/oauth2/v2.0/devicecode \
  -d client_id=$FRONTEND -d scope="api://$FRONTEND/orka.access")
echo "$DC" | python3 -c "import json,sys;print(json.load(sys.stdin)['message'])"
# After signing in in the browser:
ORKA_TOKEN=$(curl -s https://login.microsoftonline.com/$TENANT/oauth2/v2.0/token \
  -d grant_type=urn:ietf:params:oauth:grant-type:device_code -d client_id=$FRONTEND \
  -d device_code="$(echo "$DC" | python3 -c "import json,sys;print(json.load(sys.stdin)['device_code'])")" |
  python3 -c "import json,sys;print(json.load(sys.stdin)['access_token'])")
orka -t "$ORKA_TOKEN" connect entra-signin -n team-a --no-wait
```

The consent page ends on an address that carries `#completion=<value>`. With
no dashboard signed in as that person to finish the link, they copy the value
and complete it with the same token, using the Connection name `orka connect`
printed:

```bash
orka -t "$ORKA_TOKEN" connection complete entra-signin-<digest> -n team-a --completion '<value>'
```

The completion value expires a few minutes after consent, so they complete it
right away. They pass the same token with `-t` to create Tasks.

They should sign in with the same Entra account they use for Orka. Orka
binds the link to their Orka sign-in, but does not check that the Entra
account they link is the same one.

### 11. Call a resource for the requester

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: OutboundAccessPolicy
metadata:
  namespace: team-a
  name: graph-for-requester
spec:
  agentIdentity:
    mode: Delegated
    delegated:
      providerRef: {name: entra-signin}
      maxDelegationAge: 1h
    entra:
      resource: https://graph.microsoft.com
---
apiVersion: core.orka.ai/v1alpha1
kind: Tool
metadata:
  namespace: team-a
  name: graphme
spec:
  description: Return the requester's own profile, read through Microsoft Graph on their behalf.
  brokeredToolClass: read
  parameters: {type: object, properties: {}, additionalProperties: false}
  http:
    url: "https://graph.microsoft.com/v1.0/me?$select=displayName,userPrincipalName"
    method: GET
    outboundAccessPolicyRef:
      name: graph-for-requester
```

Add `graphme` to the Agent's tools and ceiling. Then have a signed-in person
create the Task through the API or `orka`, so it carries their identity.
Tasks created with `kubectl`, by a ServiceAccount, or by a gateway have no
verified requester, and delegated calls from them are refused.

For each call, the controller:
1. checks that the person's linked token is addressed to the blueprint;
2. exchanges it on behalf of the person for a token for the resource;
3. injects that token.

## What stops identity-backed calls

| Action | Effect |
|---|---|
| Delete the Agent, or remove `spec.identity` | New calls from its Tasks are refused at once. The claim stays until bound Tasks end, so no other Agent can take the identity meanwhile |
| Disable or delete the agent identity in Entra | New tokens are refused. Cached tokens last at most five minutes; Graph already rejects a disabled identity's tokens |
| Cancel the Task (`orka task cancel`, or the dashboard) or delete it | Its delegated calls are refused at once |
| Stop a Session's turn (`orka session cancel`) | The turn's Task is cancelled, so its delegated calls are refused. The Session takes the next message |
| A Task above it in the delegation chain is cancelled or deleted | Delegated calls from every Task below it are refused. A parent that finished normally does not stop its children |
| `maxDelegationAge` passes | Delegated calls are refused. It runs from the root requester's last action: creating the root Task, or sending the Session message that started the turn. Child Tasks do not reset it; a new Session turn does |
| The person disconnects or their link expires | Delegated calls are refused |

## Troubleshooting

| Symptom | Cause |
|---|---|
| Agent `Ready=False`: `identity issuer "entra" is not configured` | `agentIdentity.entra.tenantID` and `blueprintClientID` are not both set |
| Agent `Ready=False`: requires `--agent-use-authorization-enabled` | Set `agentUseAuthorization.enabled` |
| Agent `Ready=False`: must set `runtime.ceiling.allowedTools` | Add a ceiling (step 5) |
| Admission denies the Agent: `caller is not authorized to bind the Agent's identity` | The editor lacks `bind` on `agentidentities/entra` for that identity ID |
| `IdentityVerified=False` with `AADSTS7000112` | The agent identity is disabled in Entra |
| `IdentityVerified=False` with `AADSTS700016` or `AADSTS70021` | The blueprint ID is wrong, or its federated credential does not match the cluster issuer and controller ServiceAccount |
| Task creation fails: `not authorized to use agent …` or `caller is not authorized to use the Task's Agent` | The caller lacks `use` on the Agent |
| Task fails: `codex ACP runtime cannot exactly enforce provider-native tool restrictions` | A codex Agent must list either all eight built-in tools or, for a Task with workspace intent `read`, exactly `Read`, `Glob`, and `Grep`, in both `defaultAllowedTools` and the ceiling |
| `orka` answers `403`: `your sign-in is valid, but its subject "…" is not allowed` | Add that subject to `oidc.allowedSubjects` (step 9) |
| Linking fails with `scopes_denied` | Remove `offline_access` from the provider's scopes |
| Linking fails with `consent_expired` | The consent link expires within minutes; run `orka connect` again and finish promptly |
| Audit reason `policy` | The policy changed since the Tool checked it, its target is not HTTPS, or it sets `authSecretRef` |
| Audit reason `binding` | The Task's identity no longer holds: the Agent was deleted or its `spec.identity` removed or changed, the claim is being released, or the Task was created before the identity was granted (create it again) |
| Audit reason `delegation` | A Delegated precondition failed: the Task has no verified requester, the person's link expired, `maxDelegationAge` passed, or the Task, a Task above it, or its Session was cancelled or deleted |
| Audit reason `issuer` | The token request failed: Entra refused it, or, for a Delegated call, the person's linked token is not addressed to the blueprint (the provider must request `api://<blueprint appId>/access_agent`). `IdentityVerified` checks only the identity's own app-only token, so it can stay `True` while delegated exchanges fail |
| The agent sees only `MCP broker call failed` | The audit record gives the reason category; it records no error text, so arguments and tokens cannot leak into it. Showing a safe reason to the agent is tracked in [#742](https://github.com/orka-agents/orka/issues/742) |

## Other issuers

The Agent, policy, and audit design are issuer-neutral. A second issuer adds a
sibling of `entra` under `Agent.spec.identity` and in the policy, its own
`agentidentities/<issuer>` permission, and its own controller settings. See
ADR 0039 for how Google, AWS, Okta, Keycloak, and SPIFFE map onto it.
