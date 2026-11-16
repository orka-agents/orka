# GitHub Connector Example

This example lets people link their own GitHub account and lets an ACP agent
act as them through Orka's built-in GitHub tools. See the
[Connectors guide](../../website/docs/guides/connectors.md) for the full flow.

1. Register a GitHub OAuth App whose callback URL is
   `<connector-callback-base-url>/api/v1/connections/callback`.
2. Run the controller with `--connectors-enabled`,
   `--connector-callback-base-url`, Task provenance admission, and a sign-in
   that yields an issuer and subject (OIDC or a context-token profile).
3. Create the client secret and apply the `ConnectorProvider` and `Agent`.
4. Each person links GitHub once through the API (`POST /api/v1/connections`)
   and completes consent in the browser.
5. Tasks that person creates through the API get the GitHub tools under their
   account.

## Secrets

Create the OAuth client secret outside git, in the controller's watched
namespace:

```bash
kubectl -n orka-system create secret generic github-connector-oauth \
  --from-literal=clientSecret='<github-oauth-app-client-secret>'
```

## Apply

```bash
kubectl apply -n orka-system -k examples/github-connector
kubectl -n orka-system get connectorprovider github
```

`Accepted=True` and `ResolvedRefs=True` mean the provider is ready to link.

## Link and use

Signed in as a person (not a ServiceAccount):

```bash
curl -sS -X POST "$ORKA_API_URL/api/v1/connections" \
  -H "Authorization: Bearer $ORKA_TOKEN" -H 'Content-Type: application/json' \
  -d '{"provider":"github","mode":"readWrite"}'
```

Open the returned `authorizeURL` in a browser and finish consent. Then create
a Task through the API, so it carries the person's verified identity:

```bash
orka task create "List the open pull requests in example/project and summarize them" \
  --namespace orka-system \
  --type agent \
  --agent github-as-me \
  --workspace-intent read \
  --git-repo https://github.com/example/project
```

Add `--read-credential <secret>` for a private repository clone. Every command
here uses the controller's watched namespace, `orka-system`.

The agent's `list_pull_requests` call runs in the controller with the person's
token. `create_pull_request` and the other write tools appear only for a
`readWrite` Connection, pause for approval first, and therefore need a runtime
that can ask: register the same allowlist and `approvalRequiredTools` on an
external AgentKit or Foundry `AgentRuntime` (see the guide) to use them; the
built-in `codex` runtime in this example gets the read tools.

`orka connection delete <name>` (or `DELETE /api/v1/connections/<name>`)
disconnects and deletes the tokens Orka holds. GitHub has no standard
revocation endpoint, so revoke the authorization itself in GitHub under
Settings → Applications.
