---
slug: /keyless-azure-openai
description: "Authenticating azure-openai Providers with a Microsoft Entra workload identity instead of an API key, for chat and AI Tasks."
---

# Keyless Azure OpenAI Providers

Many Azure deployments prefer or require Microsoft Entra ID over API keys, and
key-based auth is often disabled on Azure AI resources. An `azure-openai`
Provider can authenticate with an Entra workload identity instead of a key
from a Secret. No key is stored anywhere, and no token reaches a Pod: the
controller obtains short-lived tokens and adds them to the requests it sends.

Keyless Providers serve chat, the compatibility APIs, and
`type: ai` Tasks. The design is
[ADR 0057](https://github.com/orka-agents/orka/blob/main/docs/adr/0057-keyless-azure-openai-providers.md).

## How it fits together

1. In Azure, a **workload identity** (a user-assigned managed identity or an
   app registration) has a data-plane role on the Azure OpenAI resource, and a
   federated identity credential that trusts the Orka controller's Kubernetes
   ServiceAccount. No client secret is stored.
2. The controller is configured with that identity under a name, and with the
   exact endpoints its tokens may be sent to.
3. A **Provider** names the identity in `spec.auth.entra.identity` instead of
   setting `spec.secretRef`.
4. Chat and the compatibility APIs run in the controller, which adds the token
   to each model request.
5. An AI Task's worker sends its model requests to the controller instead,
   authenticated with the worker Pod's own ServiceAccount token. The controller
   checks that the request comes from that Task's current worker, for the
   Provider its Job was built with, on an Azure OpenAI chat completions or
   Responses path, and then forwards it with the token.

## Before you start

- A cluster whose ServiceAccount token issuer is reachable by Entra. On AKS,
  enable the OIDC issuer and read its URL with
  `az aks show -g <group> -n <cluster> --query oidcIssuerProfile.issuerUrl -o tsv`.
- An Azure OpenAI (or Azure AI Foundry) resource with a model deployment, and
  permission to assign roles on it.
- Orka installed with Helm in the namespace `team-a`. A release reconciles
  only its own namespace, so the Provider and Tasks below live there too.
- No privileged or `hostPath` Pod able to run on the controller's node, since
  one could read the controller's projected token from it: enforce at least
  the `baseline` Pod Security profile on `team-a`
  (`kubectl label ns team-a pod-security.kubernetes.io/enforce=baseline`;
  run it with `--dry-run=server` first, which lists running Pods that break
  the profile, and delete them), and on every other namespace whose Pods can
  be scheduled onto that node, or run the controller on dedicated nodes
  ([Security](../concepts/security.md#agent-identities)).
- No Pod running as the controller ServiceAccount except the controller
  Deployment's own, and no ServiceAccount token Secret for it. Delete any
  before you configure the identity, since they could obtain a token the
  federated credential trusts. The chart's admission policy keeps new ones
  from being created.

## Create the identity

This uses a user-assigned managed identity; an app registration with a
federated identity credential works the same way.

```bash
RG=<resource group>
ISSUER=<cluster OIDC issuer URL>
# The controller ServiceAccount, whose name the chart derives from the release
ORKA_SA=system:serviceaccount:team-a:$(kubectl -n team-a get deploy \
  -l app.kubernetes.io/component=controller -o jsonpath='{.items[0].spec.template.spec.serviceAccountName}')
MODELS=$(az cognitiveservices account show -g "$RG" -n <resource> --query id -o tsv)

az identity create -g "$RG" -n orka-models
CLIENT_ID=$(az identity show -g "$RG" -n orka-models --query clientId -o tsv)
TENANT_ID=$(az identity show -g "$RG" -n orka-models --query tenantId -o tsv)
PRINCIPAL_ID=$(az identity show -g "$RG" -n orka-models --query principalId -o tsv)

az identity federated-credential create -g "$RG" --identity-name orka-models \
  --name orka-controller --issuer "$ISSUER" --subject "$ORKA_SA" \
  --audiences api://AzureADTokenExchange

az role assignment create --assignee-object-id "$PRINCIPAL_ID" \
  --assignee-principal-type ServicePrincipal \
  --role "Cognitive Services OpenAI User" --scope "$MODELS"
```

Role assignments can take a few minutes to apply.

## Configure the controller

Add the identity to the Helm values. `allowedEndpoints` lists the exact
`https` origins the identity's tokens may be sent to; a Provider whose
`baseURL` has another origin is refused.

```yaml
controller:
  providerIdentities:
    tokenAudience: api://AzureADTokenExchange
    identities:
      - name: models
        tenantID: <TENANT_ID>
        clientID: <CLIENT_ID>
        allowedEndpoints:
          - https://<resource>.openai.azure.com
```

The chart mounts a projected ServiceAccount token for the controller with
that audience, and installs the policy that stops other workloads from
obtaining a token the federated credential trusts. In the public cloud the
token scope defaults to `https://ai.azure.com/.default`. Other clouds use
their own token-exchange audience, and `tokenAudience`, every identity's
exchange audience, and the federated credential's `--audiences` must all
match it:

- Azure Government: set `cloud: AzureUSGovernment`, `scope`, and
  `tokenAudience: api://AzureADTokenExchangeUSGov`, and create the federated
  credential with `--audiences api://AzureADTokenExchangeUSGov`.
- Azure China: set `cloud: AzureChina`, `scope`, `exchangeAudience`, and the
  same value in `tokenAudience` and the federated credential.

See [`--provider-identities`](../reference/configuration.md) for every field.

## Create the Provider

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Provider
metadata:
  name: azure-keyless
  namespace: team-a
spec:
  type: azure-openai
  baseURL: https://<resource>.openai.azure.com
  azure:
    deploymentName: <deployment>
  auth:
    entra:
      identity: models
```

`spec.auth` is managed with Kubernetes RBAC: the REST API cannot set or change
it. Check that the identity can obtain a token:

```bash
kubectl -n team-a get provider azure-keyless \
  -o jsonpath='{.status.ready}{"  "}{.status.conditions[?(@.type=="CredentialReady")].reason}{"\n"}'
```

`TokenIssued` means it works. `TokenRefused` carries Entra's error code (for
example a missing federated credential), and `IssuerUnavailable` means Entra
did not answer and the controller is retrying.

## Use it

Select the Provider like any other, in chat, the compatibility APIs, an Agent's
model configuration (as the Provider or a fallback), or an AI Task:

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Task
metadata:
  name: summarize
  namespace: team-a
spec:
  type: ai
  ai:
    prompt: Summarize the release notes.
    model: <deployment>
    providerRef:
      name: azure-keyless
```

Read-only AI Tasks have no ServiceAccount token, so they cannot authenticate to
the controller, and other Task types run code that the controller cannot
restrict to model calls. Both fail with a message that names `spec.auth`; use a
key-based Provider for them.

## Limits

- Only `azure-openai` Providers can be keyless.
- The controller relays only the Azure OpenAI chat completions and Responses
  paths, at most 64 requests at a time across all Tasks, each up to 30
  minutes, with responses up to 64 MiB. A controller restart ends relayed
  requests in flight, and the AI worker retries them.
- Deleting or replacing the Provider, switching it to `secretRef`, or removing
  the identity from the controller refuses the next relayed request once the
  controller sees the change. A request already being relayed runs to
  completion, within its 30-minute bound.
