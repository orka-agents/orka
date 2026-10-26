---
slug: /configuration
description: "Helm values, controller flags, and environment variables for running Orka."
---

# Configuration

This page covers how to run Orka: Helm values, controller flags, environment variables,
authentication settings, and metrics. The custom resources you create — Tasks, Agents,
Providers, Tools, and the rest — are in the [Resource reference](resources.md).

## Helm chart

Key configuration values for the Helm chart. Published charts use release image
tags; development charts use the rolling `0.0.0-dev` tag. See
[Image overrides](#image-overrides) for exact source image selection.

| Parameter | Default | Description |
|-----------|---------|-------------|
| `controller.replicas` | `1` | Controller replicas |
| `controller.image.repository` | `ghcr.io/orka-agents/orka` | Controller image |
| `controller.image.tag` | release version | Controller image tag. |
| `controller.image.digest` | `""` | Optional SHA256 digest; takes precedence over `tag`. |
| `controller.mode` | `harness-v2` | Fixed agent execution protocol identity. Only `harness-v2` is supported. |
| `controller.watchNamespace` | Helm release namespace | Must match the release namespace, labeled `orka.ai/controller-mode` with the matching mode. |
| `controller.agentExecutionSnapshot.existingSecret` | `""` | Your own snapshot encryption Secret. Empty renders an empty `<release>-agent-execution-snapshot` that the controller fills on first start. Immutable after install. See [Snapshot encryption key](#snapshot-encryption-key). |
| `controller.agentExecutionSnapshot.key` | `key` | Item inside that Secret holding 32 raw bytes or their base64 encoding. Immutable after install. |
| `controller.enforceNamespaceIsolation` | `true` | Restrict namespace-bound API callers and default Helm RBAC to their namespace |
| `service.port` | `8080` | Controller Service port used by controller and Publisher in-cluster URLs. |
| `controller.apiPort` | `8080` | Controller container listener and Service target port. |
| `controller.metricsPort` | `8081` | Metrics endpoint port |
| `controller.healthPort` | `8082` | Health probe port |
| `controller.logLevel` | `info` | Log level (debug/info/warn/error) |
| `controller.dashboard.enabled` | `false` | Serve the [web dashboard](../guides/ui.md) on the API port. |
| `controller.connectors.enabled` / `controller.connectors.callbackBaseUrl` | `false` / `""` | Enable per-person [connectors](../guides/connectors.md), and the public https origin the OAuth provider redirects back to. |
| `controller.agentHosts.enabled` / `.namespace` / `.agentImage` | `false` / `orka-agent-hosts` / `""` | Run [agent hosts](../operations/agent-hosts.md) in a chart-created namespace with the restricted Pod Security profile. An empty `agentImage` uses the controller image, which then must set `controller.image.digest`. |
| `github.webhook.secretName` / `github.webhook.secretKey` | `""` / `secret` | Secret holding the shared secret for signed GitHub webhooks at `/webhooks/github`, used by [label triggers and pull request events](../guides/issue-to-pr-automation.md#start-from-a-github-label). |
| `controller.acpRuntime.namespace` | `orka-runtimes` | Namespace for controller-owned RuntimePool workloads. |
| `controller.acpRuntime.providerProxyNamespace` | `""` | Compatibility guard for the chart-managed provider proxy. Leave empty or set exactly to the Helm release namespace; any other nonempty value is rejected when the proxy is enabled. |
| `controller.acpRuntime.codexImage` | release image tag | Codex ACP image tag or digest; an empty string disables it. |
| `controller.acpRuntime.claudeImage` | release image tag | Claude ACP image tag or digest; an empty string disables it. |
| `controller.acpRuntime.copilotImage` | release image tag | GitHub Copilot ACP image tag or digest; an empty string disables it. |
| `controller.acpRuntime.opencodeImage` | release image tag | OpenCode ACP image tag or digest; an empty string disables it. |
| `controller.acpRuntime.upgradeDrain.*` | enabled | Two-phase planned-upgrade admission closure and RuntimePool drain settings. |
| `controller.acpRuntime.toolboxes.enabled` | `false` | Let built-in Agents declare `runtime.toolboxes`, read-only tool images bound into runtime Pods. See [Toolboxes](../guides/toolboxes.md). |
| `controller.acpRuntime.toolboxes.allowedRegistries` | `[]` | Registry hosts or host/path prefixes toolbox images may come from. Empty allows nothing. |
| `controller.acpRuntime.toolboxes.imagePullSecrets` | `[]` | Secret names in the runtime namespace added to runtime Pod `imagePullSecrets` when toolboxes are bound. Never mounted into containers. |
| `controller.acpRuntime.toolboxes.nodeSelector` | `{}` | Node selector merged into runtime Pods that bind toolboxes. |
| `controller.acpRuntime.toolboxes.mountMethod` | `copy` | `copy` (init-container copier, every Kubernetes version) or `imageVolume` (Kubernetes 1.36+ with containerd 2.2+ or CRI-O 1.33+). |
| `providerProxy.enabled` | `false` | Enable the authenticated proxy when connecting built-in coding agents to your model gateway. Installation and AI-worker tasks do not require it. |
| `providerProxy.upstreamBaseURL` | `""` | Your gateway's HTTP(S) endpoint. Required when the proxy is enabled. Credentials, queries, and fragments are forbidden in the URL. |
| `providerProxy.egress` | `[]` | NetworkPolicy egress rules allowing the proxy to reach your gateway. Leave empty when `upstreamBaseURL` names an in-cluster Service; the chart then derives the rule from that Service at install time. Required for external gateways, named target ports, or offline renders. DNS access is always added. |
| `providerProxy.auth.existingSecret` | `""` | Existing current/optional-overlap proxy bearer Secret. RuntimePool copies are controller-managed. |
| `providerProxy.tokenReloadInterval` | `5s` | Atomic projected-Secret reload interval. Invalid generations fail readiness and forwarding closed. |
| `publisher.enabled` | `true` | Deploy the separate clean-room Workspace/Publisher service. |
| `publisher.image.repository` / `publisher.image.tag` | workspace publisher image / release version | Publisher image. Set `publisher.image.digest` to override the tag. |
| `publisher.allowedSCMHosts` | `github.com` | Exact lower-case SCM hosts accepted by both Publisher validation and the SCM egress proxy. |
| `publisher.auth.existingSecret` | `""` | Existing controller-auth/capability Secret for publisher operations. |
| `publisher.auth.rolloutNonce` | `""` | Non-secret revision marker that restarts controller and Publisher during coordinated publisher-auth Secret rotation. |
| `scmEgressProxy.enabled` | `true` | Require all Publisher Git and forge HTTPS traffic to traverse the dedicated authenticated proxy. The chart rejects `publisher.enabled=true` when this is false. |
| `scmEgressProxy.auth.existingSecret` | `""` | Existing Secret shared only by Publisher and proxy. Its token must contain 32-256 RFC 3986 unreserved characters. |
| `scmEgressProxy.auth.rolloutNonce` | `""` | Non-secret revision marker that restarts Publisher and SCM proxy during coordinated proxy-auth Secret rotation. |
| `scmEgressProxy.maxTunnelBytes` | `1073741824` | Maximum bytes allowed in each CONNECT tunnel direction. |
| `scmEgressProxy.maxConcurrent` | `8` | Maximum concurrent forward requests and CONNECT tunnels. |
| `webhooks.tls.existingSecret` | `""` | Your own TLS Secret for the admission webhooks. Empty lets the controller issue and renew a self-signed certificate in `<release>-webhook-tls`. See [Webhook certificate](#webhook-certificate). |
| `webhooks.tls.certKey` / `webhooks.tls.privateKeyKey` | `tls.crt` / `tls.key` | Certificate and private-key keys inside the webhook TLS Secret. |
| `webhooks.caBundle` | `""` | Base64-encoded PEM CA bundle for an operator-supplied certificate. Leave empty with controller-issued TLS, which injects its own CA, or when `webhooks.caInjectionAnnotations` configures an injector. |
| `webhooks.caInjectionAnnotations` | `{}` | CA-injection annotations (for example cert-manager) placed on the chart ValidatingWebhookConfiguration. With `webhooks.tls.existingSecret` set, rendering fails unless this or `webhooks.caBundle` is set. |
| `webhooks.timeoutSeconds` | `10` | Admission webhook timeout. |
| `controller.executionWorkspace.dispatchEnabled` | `false` | Enable class-backed ACP dispatch through registered external providers. |
| `controller.executionWorkspace.workerNamespaces` | `[]` | Grant core read-only Pod observation in native worker namespaces; provider mutation RBAC is separate. |
| `controller.substrate.mcpToolsEnabled` | `false` | Enable in-tree native Substrate MCP Tool actors and actor pools. |
| `workers.ai.image.repository` | `ghcr.io/orka-agents/orka/ai-worker` | AI worker image |
| `workers.general.image.repository` | `ghcr.io/orka-agents/orka/general-worker` | General worker image |
| `service.type` | `ClusterIP` | Service type |
| `client.create` | `true` | Create client ServiceAccount for API access |
| `client.name` | `orka-client` | Client ServiceAccount name |
| `client.namespace` | Helm release namespace | Client ServiceAccount namespace; must match `controller.watchNamespace`. |

### Image overrides

Released charts include matching version tags for the controller, workers,
Publisher, and coding-agent runtimes. For controller, worker, and Publisher
images, set `image.tag` to choose another version or `image.digest` to pin a
SHA256 digest. A digest takes precedence over the tag.

The development chart uses version and image tag `0.0.0-dev`, selecting the latest
validated `main` build. For an exact checkout, use its `sha-<full-commit>` tags or
the digest-pinned `values.json` from the Development Images workflow artifact.
The workflow validates all nine images before updating rolling aliases; use
pinned values to avoid mixing images during those individual tag updates.

Tagged controller, Publisher, and native worker images use `Always` pull policy.
Digest-pinned native workers and RuntimePools remain cacheable with `IfNotPresent`.
New image publication does not restart existing Pods. When testing source upgrades,
`--reuse-values` retains prior references, so replace them and clear unused runtimes.

Runtime fields such as `controller.acpRuntime.codexImage` accept a full image
reference with a tag or digest. The controller resolves tags to digests once at
startup, so running sessions use fixed images. If any resolution fails, the
controller logs the error, disables every coding-agent runtime for that process,
and keeps running: AI and container Tasks work, agent Tasks fail closed as
unavailable. Fix registry access or pin digests, then restart the controller.
An explicit digest skips this lookup.

Tag resolution requires controller HTTPS access to a registry that allows
anonymous pulls. Use digest references for private registries or installations
without registry access from the controller. Cluster nodes still need access to
pull the configured images. Set an individual runtime image to an empty string
to disable that provider.

### Snapshot encryption key

The controller encrypts agent execution snapshots at rest with an AES-256 key
mounted from a Secret. By default the chart renders an empty Secret named
`<release>-agent-execution-snapshot` and the controller mints 32 random bytes
into it when it creates a new SQLite dataset, then reuses them on every later
start. The chart never renders key material, so Helm release history and
dry-run output contain no secrets, and Helm leaves the controller-written data
alone on upgrade. The Secret is kept on `helm uninstall` so a restored data
volume can still be decrypted. Back it up with the data volume.

The controller decides whether it may mint a key from the dataset history recorded
inside the SQLite database, not from whether the database file exists. It mints a
key only for a new dataset that has never completed key bootstrap and holds no
records. A dataset that completed bootstrap, or was adopted from an older layout
during an upgrade, needs its existing key even if every record has since been
deleted. If the Secret is empty for such a dataset, the key was lost, and the
controller refuses to start rather than mint a new key that could not read the
existing records. Restore the Secret from backup, or delete the data volume to
start over. See
[Dataset identity and key bootstrap](../operations/upgrading.md#dataset-identity-and-key-bootstrap).

To bring your own key, create the Secret before installing and set
`controller.agentExecutionSnapshot.existingSecret`. The item must hold exactly
32 raw bytes or their base64 encoding:

```bash
kubectl -n orka-system create secret generic orka-agent-snapshot-key \
  --from-literal=key="$(openssl rand -base64 32)"
```

```bash
helm install orka orka/orka --namespace orka-system \
  --set-string controller.agentExecutionSnapshot.existingSecret=orka-agent-snapshot-key \
  --set-string controller.agentExecutionSnapshot.key=key \
  ...
```

The Secret name, item key, and key material are immutable for the life of the
release. Helm rejects an upgrade that changes the name or item, including a
switch between the generated Secret and your own. It cannot detect changed
bytes under the same name, so never rotate the material in place, and never
run `helm upgrade --force`, which replaces the Secret with the chart's empty
version.

### Webhook certificate

Orka validates its resources through fail-closed Kubernetes admission webhooks
served by the controller. Those webhooks need a serving certificate that the API
server trusts. The chart supports two ways to provide it.

**Controller-managed, the default.** With `webhooks.tls.existingSecret` empty,
the chart renders an empty Secret named `<release>-webhook-tls` and the
controller fills it using
[cert-controller](https://github.com/open-policy-agent/cert-controller), the
library Gatekeeper uses for the same job. It mints a ten-year self-signed CA and
a one-year serving certificate, renews the serving certificate before expiry,
and writes the CA into the `caBundle` of the release's
ValidatingWebhookConfiguration. The Secret is the source of truth and is kept on
`helm uninstall`, so a reinstall under the same release name reuses the CA. The chart never renders certificate material. The
Secret is mounted into the controller Pod, and the Pod reports ready only after
the kubelet has projected the certificate and the CA is injected, which can take
about a minute on a fresh install. Never run `helm upgrade --force`, which
replaces the Secret with the chart's empty version. This grants the
controller `list` and `watch` on all ValidatingWebhookConfigurations, which
cannot be name-scoped, plus `update` on its own. Because that update is
whole-object, a ValidatingAdmissionPolicy installed with the release lets the
controller's ServiceAccount change nothing on that configuration except each
webhook's `caBundle`, so a compromised controller cannot repoint or widen its
own admission webhooks.

**Operator-supplied.** Set `webhooks.tls.existingSecret` to a Secret holding
`tls.crt` and `tls.key` valid for `<release>-webhook.<namespace>.svc`, and
supply the CA either as `webhooks.caBundle` or through
`webhooks.caInjectionAnnotations`. The controller then only reads the mounted
files and never touches the webhook configuration.

```yaml
webhooks:
  tls:
    existingSecret: orka-webhook-tls
  caInjectionAnnotations:
    cert-manager.io/inject-ca-from-secret: orka-system/orka-webhook-tls
```

For a one-off self-signed certificate without cert-manager:

```bash
(
  umask 077
  openssl req -x509 -newkey rsa:2048 -nodes -sha256 -days 365 \
    -keyout tls.key -out tls.crt \
    -subj '/CN=orka-webhook.orka-system.svc' \
    -addext 'subjectAltName=DNS:orka-webhook.orka-system.svc,DNS:orka-webhook.orka-system.svc.cluster.local' &&
  cp tls.crt ca.crt
)
kubectl -n orka-system create secret generic orka-webhook-tls \
  --type=kubernetes.io/tls \
  --from-file=tls.crt=tls.crt --from-file=tls.key=tls.key --from-file=ca.crt=ca.crt
rm -f tls.key tls.crt ca.crt
```

```bash
helm install orka orka/orka --namespace orka-system \
  --set-string webhooks.tls.existingSecret=orka-webhook-tls \
  --set-string webhooks.caBundle="$(kubectl -n orka-system get secret orka-webhook-tls -o jsonpath='{.data.ca\.crt}')" \
  ...
```

Switching between the two modes is an ordinary `helm upgrade`. A certificate for
any other name is rejected by the API server at admission time, not at install
time.

### Helm authentication Secret rotation

The Publisher, SCM proxy, and controller clients load these credentials at process startup. Update the Secret and bump its corresponding nonce in the same Helm upgrade: use `publisher.auth.rolloutNonce` for the publisher-auth Secret, and `scmEgressProxy.auth.rolloutNonce` for the SCM proxy-auth Secret. The publisher nonce is applied only to controller and Publisher Pod templates; the SCM nonce is applied only to Publisher and SCM proxy Pod templates. Nonces are safe revision strings, not Secret values. Coordinated rotation can briefly fail closed while Pods roll but prevents stale or split credential generations from persisting.

### Canonical Kustomize overlay

Direct Kustomize deployments must use:

```bash
kubectl apply -k config/acp-production
```

`config/acp-production` composes the CRD-free `config/acp-workload` base with the cross-namespace Vekil
ingress NetworkPolicy. Applying `config/default` alone omits the boundary that
prevents runtime Pods from bypassing the authenticated provider proxy. Configure
the required system Secrets and digest-pinned controller, Publisher, Codex,
Claude, Copilot, and OpenCode images before applying the overlay. `make deploy` validates those image
references and applies the equivalent resource set.

The overlay contains no CRDs. Install the shared CRDs from `config/crd` — all 29 production
CRDs, without the two development-only `fake.workspace.orka.ai` CRDs — through your cluster's
designated CRD owner before deploying workloads. The Helm chart carries the same 29.
`make deploy` checks that the required shared CRDs are already established.

### ACP storage split

The `--store-backend=sqlite` flag does not make SQLite authoritative for ACP
control transitions. `ControllerEpoch`, `PromptAttempt`,
`RuntimeSessionControl`, `BranchClaim`, `Publication`, and `ExternalEffect`
status plus coordination Leases are the control authority. SQLite stores
transcript/SessionTurn payloads, deferred outbox projections, and artifacts
behind those Kubernetes fences.


For the Kustomize deployment, create the proxy-auth Secret before applying
`config/acp-production` (the Secret is intentionally not stored in Git):

```bash
token="$(openssl rand -hex 32)"
kubectl -n orka-system create secret generic scm-egress-proxy-auth \
  --from-literal=token="$token"
unset token
```

The token is an ingress credential for the proxy only; it is not a Git or forge credential. `config/publisher` sets `HTTPS_PROXY`/`NO_PROXY`, and the Publisher copies only those validated proxy variables into its otherwise empty Git subprocess environments. The Publisher NetworkPolicy has no public `0.0.0.0/0` rule. Only the SCM egress proxy may reach public port 443.

`cmd/orka-workspace-publisher` also requires `ORKA_PUBLISHER_ARTIFACT_AUTHORIZATION_BROKER_URL`, `ORKA_PUBLISHER_CREDENTIAL_BROKER_URL`, and `ORKA_PUBLISHER_SCM_EGRESS_PROXY_REQUIRED=true` in normal startup.

:::danger[`ORKA_PUBLISHER_ALLOW_DEVELOPMENT_FALLBACKS` is for isolated local tests only]
It turns on the legacy local artifact signing key, a filesystem credential root, and
proxy-less mode — three of the boundaries that keep publication credentials away from the
agent. Never set it in a cluster manifest.
:::

When `ORKA_PUBLISHER_PUBLISH_TIMEOUT` is raised above the default on the Publisher, set the same value on the controller Deployment as well. The controller bounds each publisher-backed external-effect call and sizes the effect's ledger lease from this timeout plus a settlement margin; without the controller-side value, publisher operations are clamped to the default four-minute call bound. Brokered custom-Tool calls always keep the fixed four-minute clamp that their Tool descriptors were admitted under.

### Helm CRD lifecycle

CRD behavior is not controlled through chart values. A fresh install creates all CRDs in the chart unless `--skip-crds` is used. Because CRDs are cluster-scoped, designate one lifecycle owner and use `--skip-crds` for other Orka releases. Helm does not update CRDs during `helm upgrade`; apply the CRDs from the exact target chart before upgrading the controller. Helm retains CRDs and Orka custom resources on uninstall. See the [Helm CRD lifecycle guide](https://github.com/orka-agents/orka/blob/main/charts/orka/README.md).

Each installation owns a separate watched namespace, runtime namespace, endpoint,
RBAC, Lease, database, and credentials. See
[installation ownership](../operations/harness-modes.md).

Context-token flags can also be configured through Helm under
`controller.contextToken`. For example:

```yaml
controller:
  contextToken:
    profile: transaction-token
    issuer: https://issuer.example.com
    audience: orka
    headers: Txn-Token
    authzMode: enforce
    scopes:
      taskCreate: orka:tasks:create
      providerUse: orka:providers:use
      toolUse: orka:tools:use
      secretCredentialRead: orka:secrets:credentials:read
      monitorRead: orka:monitors:read
      monitorWrite: orka:monitors:write
      monitorOperate: orka:monitors:operate
      connectorRead: orka:connectors:read
      connectorManage: orka:connectors:manage
      gatewayRead: orka:gateways:read
      gatewayOperate: orka:gateways:operate
    tts:
      endpoint: https://tts.example.com/oauth/token
      audience: orka-workers
      timeout: 5s
      tokenSource: serviceAccount
      childScope: orka:tasks:create
      outboundScope: orka:tools:use
      childTokenTTL: 5m
      toolTokenTTL: 2m
  outboundAccess:
    trustedGatewayServices:
      - gateway-system/agentgateway:8080
    trustedTokenEndpointServices:
      - identity-system/token-service:8443
```

The Helm keys mirror the controller flags: for example,
`controller.contextToken.jwksUrl` renders `--context-token-jwks-url`,
`controller.contextToken.scopes.secretRead` renders
`--context-token-secret-read-scopes`,
`controller.contextToken.scopes.secretCredentialRead` renders
`--context-token-secret-credential-read-scopes`,
`controller.contextToken.scopes.monitorRead` renders
`--context-token-monitor-read-scopes`,
`controller.contextToken.scopes.gatewayRead` renders
`--context-token-gateway-read-scopes`, and
`controller.contextToken.tts.toolTokenTTL` renders
`--context-token-tool-token-ttl`.

See [charts/orka/values.yaml](https://github.com/orka-agents/orka/blob/main/charts/orka/values.yaml) for the full list.

## Controller flags

| Flag | Default | Description |
|------|---------|-------------|
| `--api-port` | `8080` | REST API server port |
| `--dashboard-enabled` | `false` | Serve the embedded web dashboard on the API port. Without it, paths outside the APIs answer with a JSON 404, `orka dashboard` stops with an error, and the connector consent callback lands on a plain page that shows the `orka connection complete` command. Helm: `controller.dashboard.enabled`. Env: `ORKA_DASHBOARD_ENABLED` |
| `--gateway-enabled` | `true` | Enable generic gateway reconciliation and ingress |
| `--connectors-enabled` | `false` | Enable per-user connector reconciliation (`ConnectorProvider` and `Connection`). Requires Task provenance admission (`--task-provenance-admission-enabled` or `--task-provenance-admission-external`); the controller refuses to start otherwise, because connector use trusts `spec.requestedBy` only when the API server provably stamped it. Env: `ORKA_CONNECTORS_ENABLED` |
| `--connectors-allow-private-endpoints` | empty | **Local fixtures only.** Accept private, loopback, and cluster-local connector provider endpoints and, for Tools behind a connection-mode outbound access policy, private tool endpoints (HTTPS is still required), and let linked-account requests reach them over the same hardened transport (no proxy, verified TLS). A person's token would be sent to such an address, so the flag takes only the literal `i-understand-tokens-may-leave-the-cluster`, and the controller refuses to start with it unless `--connector-callback-base-url` is a plain-http `localhost` origin, which no production deployment has. The Live Connectors E2E uses it for its in-cluster fake provider. Env: `ORKA_CONNECTORS_ALLOW_PRIVATE_ENDPOINTS` |
| `--connector-callback-base-url` | `ORKA_CONNECTOR_CALLBACK_BASE_URL` env or `""` | Required with `--connectors-enabled`. Absolute https origin (scheme and host only, no path) the OAuth provider redirects back to; the provider must register exactly this origin plus `/api/v1/connections/callback`. Plain http is accepted only for `localhost`. Helm: `controller.connectors.callbackBaseUrl`. Sealed linked-account credentials live in the controller store, which the chart already requires to be persistent in every mode. |
| `--gateway-pending-per-session` | `100` | Maximum pending gateway events per Session |
| `--gateway-interim-messages-per-task` | `10` | Lifetime cap for distinct accepted interim messages per Task; failed/expired messages count, retries do not. Helm: `controller.gateway.interimMessagesPerTask` |
| `--gateway-max-records-per-gateway` | `1000` | Maximum retained accepted/dead-letter event records per Gateway before ingress is throttled |
| `--gateway-max-rejected-records-per-gateway` | `250` | Separate audit budget for rejected events so unauthorized traffic cannot consume operational capacity |
| `--gateway-event-expiry` | `24h` | Queue and delivery retry expiry |
| `--gateway-terminal-retention` | `720h` | Terminal event and delivery retention |
| `--gateway-delivery-timeout` | `15s` | One synchronous adapter delivery timeout |
| `--gateway-delivery-max-attempts` | `10` | Delivery attempts before dead-lettering |
| `--gateway-claim-lease` | `1m` | Event and delivery claim lease |
| `--gateway-poll-interval` | `500ms` | Dispatcher and delivery poll interval |
| `--gateway-batch-size` | `25` | Maximum gateway records processed per iteration |
| `--controller-mode` / `ORKA_CONTROLLER_MODE` | `harness-v2` | Fixed `harness-v2` protocol identity. Other values are rejected. |
| `--watch-namespace` | required | One non-empty watched namespace. Its `orka.ai/controller-mode` label must match; the controller adds it when absent. |
| `--claim-namespace-mode` | `true` | Label an unlabeled watched namespace with the controller's mode on first start. Set `false` to require an operator-applied label. |
| `--enforce-namespace-isolation` | `false` | Restrict users to their ServiceAccount's namespace |
| `--max-tasks-per-namespace` | `0` | Max active tasks per namespace (0 = unlimited) |
| `--acp-workspace-dispatch-enabled` | `ORKA_ACP_WORKSPACE_DISPATCH_ENABLED` env or `false` | Admit workspace-provider-backed ACP RuntimeSession dispatch; when false, workspace-backed agent Tasks fail closed |
| `--substrate-mcp-tools-enabled` | `ORKA_SUBSTRATE_MCP_TOOLS_ENABLED` env or `false` | Enable native MCP Tool actors and actor pools; does not allocate ACP workspaces. |
| `--acp-toolboxes-enabled` | `ORKA_ACP_TOOLBOXES_ENABLED` env or `false` | Let built-in Agents declare `runtime.toolboxes`. Off by default; see [Toolboxes](../guides/toolboxes.md) |
| `--acp-toolbox-allowed-registries` | `ORKA_ACP_TOOLBOX_ALLOWED_REGISTRIES` env or `""` | Comma-separated registry hosts or host/path prefixes toolbox images may come from. Empty allows nothing |
| `--acp-toolbox-image-pull-secrets` | `ORKA_ACP_TOOLBOX_IMAGE_PULL_SECRETS` env or `""` | Comma-separated Secret names in the runtime namespace added to runtime Pod `imagePullSecrets` when toolboxes are bound |
| `--acp-toolbox-node-selector` | `ORKA_ACP_TOOLBOX_NODE_SELECTOR` env or `""` | Comma-separated `key=value` node selector merged into runtime Pods that bind toolboxes |
| `--acp-toolbox-mount-method` | `ORKA_ACP_TOOLBOX_MOUNT_METHOD` env or `copy` | `copy` or `imageVolume`. With `imageVolume` on an API server older than 1.36, toolbox Agents are rejected with `ToolboxUnavailable`; there is no silent fallback |
| `--controller-url` | `""` | Base URL workers use to reach the controller API (e.g., `http://orka-api.orka-system.svc:8080`). Required for worker result callbacks and session transcript fetching |
| `--oidc-issuer` | `ORKA_OIDC_ISSUER` env or `""` | OIDC issuer URL for external API bearer token validation. Requires `--oidc-audience` when set |
| `--oidc-audience` | `ORKA_OIDC_AUDIENCE` env or `""` | Expected OIDC audience for external API bearer tokens. Requires `--oidc-issuer` when set |
| `--oidc-jwks-url` | `ORKA_OIDC_JWKS_URL` env or `""` | Optional JWKS URL. When empty, Orka discovers it from the issuer metadata |
| `--oidc-allowed-subjects` | `ORKA_OIDC_ALLOWED_SUBJECTS` env or `""` | Required comma-separated OIDC subject allowlist patterns when OIDC is enabled |
| `--oidc-namespace` | `ORKA_OIDC_NAMESPACE` env or `default` | Namespace assigned to authorized OIDC callers for namespace isolation |
| `--context-token-profile` | `ORKA_CONTEXT_TOKEN_PROFILE` env or `""` | Context-token profile for external API requests. Currently supports `transaction-token` |
| `--context-token-issuer` | `ORKA_CONTEXT_TOKEN_ISSUER` env or `""` | Context-token issuer URL. Requires `--context-token-profile` and `--context-token-audience` when set |
| `--context-token-audience` | `ORKA_CONTEXT_TOKEN_AUDIENCE` env or `""` | Expected context-token audience. Requires `--context-token-profile` and `--context-token-issuer` when set |
| `--context-token-jwks-url` | `ORKA_CONTEXT_TOKEN_JWKS_URL` env or `""` | Optional context-token JWKS URL. For `transaction-token`, defaults to `<issuer>/.well-known/jwks.json` |
| `--context-token-headers` | `ORKA_CONTEXT_TOKEN_HEADERS` env or `""` | Comma-separated context-token header locations. Use `Header` for raw tokens or `Header:Scheme` for scheme-prefixed tokens. The `transaction-token` default is `Txn-Token` |
| `--context-token-authz-mode` | `ORKA_CONTEXT_TOKEN_AUTHZ_MODE` env or `""` | Context-token authorization mode: `off`, `audit`, or `enforce`. Empty defaults to `off` |
| `--context-token-task-create-scopes` | `ORKA_CONTEXT_TOKEN_TASK_CREATE_SCOPES` env or `""` | Comma-separated scopes authorizing Task creation. Defaults to `orka:tasks:create` |
| `--context-token-task-read-scopes` | `ORKA_CONTEXT_TOKEN_TASK_READ_SCOPES` env or `""` | Comma-separated scopes authorizing Task reads and related data. Defaults to `orka:tasks:get` |
| `--context-token-task-list-scopes` | `ORKA_CONTEXT_TOKEN_TASK_LIST_SCOPES` env or `""` | Comma-separated scopes authorizing Task listing. Defaults to `orka:tasks:list` |
| `--context-token-task-delete-scopes` | `ORKA_CONTEXT_TOKEN_TASK_DELETE_SCOPES` env or `""` | Comma-separated scopes authorizing Task deletion. Defaults to `orka:tasks:delete` |
| `--context-token-tool-read-scopes` | `ORKA_CONTEXT_TOKEN_TOOL_READ_SCOPES` env or `""` | Comma-separated scopes authorizing Tool reads. Defaults to `orka:tools:read` |
| `--context-token-tool-use-scopes` | `ORKA_CONTEXT_TOKEN_TOOL_USE_SCOPES` env or `""` | Comma-separated scopes authorizing Orka-managed chat/OpenAI/Anthropic tool execution. Defaults to `orka:tools:use` |
| `--context-token-provider-use-scopes` | `ORKA_CONTEXT_TOKEN_PROVIDER_USE_SCOPES` env or `""` | Comma-separated scopes authorizing chat/OpenAI/Anthropic model-provider use and model listing. Defaults to `orka:providers:use` |
| `--context-token-secret-read-scopes` | `ORKA_CONTEXT_TOKEN_SECRET_READ_SCOPES` env or `""` | Comma-separated scopes authorizing Secret metadata reads. Defaults to `orka:secrets:read` |
| `--context-token-secret-credential-read-scopes` | `ORKA_CONTEXT_TOKEN_SECRET_CREDENTIAL_READ_SCOPES` env or `""` | Comma-separated scopes authorizing Secret data or ServiceAccount tokens as outbound credentials. Defaults to `orka:secrets:credentials:read` |
| `--context-token-agent-read-scopes` | `ORKA_CONTEXT_TOKEN_AGENT_READ_SCOPES` env or `""` | Comma-separated scopes authorizing Agent reads. Defaults to `orka:agents:read` |
| `--context-token-agent-write-scopes` | `ORKA_CONTEXT_TOKEN_AGENT_WRITE_SCOPES` env or `""` | Comma-separated scopes authorizing Agent writes. Defaults to `orka:agents:write` |
| `--context-token-memory-read-scopes` | `ORKA_CONTEXT_TOKEN_MEMORY_READ_SCOPES` env or `""` | Comma-separated scopes authorizing memory reads. Defaults to `orka:memory:read` |
| `--context-token-memory-write-scopes` | `ORKA_CONTEXT_TOKEN_MEMORY_WRITE_SCOPES` env or `""` | Comma-separated scopes authorizing memory writes. Defaults to `orka:memory:write` |
| `--context-token-session-read-scopes` | `ORKA_CONTEXT_TOKEN_SESSION_READ_SCOPES` env or `""` | Comma-separated scopes authorizing session reads. Defaults to `orka:sessions:read` |
| `--context-token-session-write-scopes` | `ORKA_CONTEXT_TOKEN_SESSION_WRITE_SCOPES` env or `""` | Comma-separated scopes authorizing session writes/deletes. Defaults to `orka:sessions:write` |
| `--context-token-security-read-scopes` | `ORKA_CONTEXT_TOKEN_SECURITY_READ_SCOPES` env or `""` | Comma-separated scopes authorizing security scan reads. Defaults to `orka:security:read` |
| `--context-token-security-write-scopes` | `ORKA_CONTEXT_TOKEN_SECURITY_WRITE_SCOPES` env or `""` | Comma-separated scopes authorizing security scan creates, updates, deletes, and other mutations. Defaults to `orka:security:write` |
| `--context-token-monitor-read-scopes` | `ORKA_CONTEXT_TOKEN_MONITOR_READ_SCOPES` env or `""` | Comma-separated scopes authorizing repository monitor reads. Defaults to `orka:monitors:read` |
| `--context-token-monitor-write-scopes` | `ORKA_CONTEXT_TOKEN_MONITOR_WRITE_SCOPES` env or `""` | Comma-separated scopes authorizing repository monitor create, update, and delete operations. Defaults to `orka:monitors:write` |
| `--context-token-monitor-operate-scopes` | `ORKA_CONTEXT_TOKEN_MONITOR_OPERATE_SCOPES` env or `""` | Comma-separated scopes authorizing repository monitor manual runs. Defaults to `orka:monitors:operate` |
| `--context-token-connector-read-scopes` | `ORKA_CONTEXT_TOKEN_CONNECTOR_READ_SCOPES` env or `""` | Comma-separated scopes authorizing a person to read their own connector Connections. Defaults to `orka:connectors:read` |
| `--context-token-connector-manage-scopes` | `ORKA_CONTEXT_TOKEN_CONNECTOR_MANAGE_SCOPES` env or `""` | Comma-separated scopes authorizing a person to link, update, and disconnect their own connector Connections. Defaults to `orka:connectors:manage` |
| `--context-token-skill-read-scopes` | `ORKA_CONTEXT_TOKEN_SKILL_READ_SCOPES` env or `""` | Comma-separated scopes authorizing Skill reads. Defaults to `orka:skills:read` |
| `--context-token-skill-write-scopes` | `ORKA_CONTEXT_TOKEN_SKILL_WRITE_SCOPES` env or `""` | Comma-separated scopes authorizing Skill writes. Defaults to `orka:skills:write` |
| `--context-token-gateway-read-scopes` | `ORKA_CONTEXT_TOKEN_GATEWAY_READ_SCOPES` env or `""` | Comma-separated scopes authorizing gateway resource and ledger reads. Defaults to `orka:gateways:read` |
| `--context-token-gateway-operate-scopes` | `ORKA_CONTEXT_TOKEN_GATEWAY_OPERATE_SCOPES` env or `""` | Comma-separated scopes authorizing dead-lettered delivery retries. Defaults to `orka:gateways:operate` |
| `--context-token-tts-endpoint` | `ORKA_CONTEXT_TOKEN_TTS_ENDPOINT` env or `""` | Exact transaction-token TTS OAuth endpoint for optional exchange/replacement |
| `--context-token-tts-audience` | `ORKA_CONTEXT_TOKEN_TTS_AUDIENCE` env or `""` | Audience requested from transaction-token TTS exchanges |
| `--context-token-tts-timeout` | `ORKA_CONTEXT_TOKEN_TTS_TIMEOUT` env or `""` | Timeout for transaction-token TTS exchanges. Defaults to `5s` when TTS is enabled |
| `--context-token-tts-token-source` | `ORKA_CONTEXT_TOKEN_TTS_TOKEN_SOURCE` env or `""` | Subject token source for TTS exchanges: `serviceAccount`, `incoming`, or `none`. Defaults to `serviceAccount` when TTS is enabled |
| `--context-token-subject-token-type` | `ORKA_CONTEXT_TOKEN_SUBJECT_TOKEN_TYPE` env or `""` | Subject token type for worker-side TTS exchanges. Workers default to TxToken subject tokens when empty |
| `--context-token-child-scope` | `ORKA_CONTEXT_TOKEN_CHILD_SCOPE` env or `""` | Scope workers request for child delegated TxTokens when TTS is configured |
| `--context-token-outbound-scope` | `ORKA_CONTEXT_TOKEN_OUTBOUND_SCOPE` env or `""` | Scope workers request for outbound HTTP Tool TxTokens when TTS is configured |
| `--context-token-child-token-ttl` | `ORKA_CONTEXT_TOKEN_CHILD_TOKEN_TTL` env or `""` | Requested TTL for child delegation TxTokens. Defaults to `5m` when TTS is enabled |
| `--context-token-tool-token-ttl` | `ORKA_CONTEXT_TOKEN_TOOL_TOKEN_TTL` env or `""` | Requested TTL for outbound tool TxTokens. Defaults to `2m` when TTS is enabled |
| `--outbound-access-trusted-gateway-services` | `ORKA_OUTBOUND_ACCESS_TRUSTED_GATEWAY_SERVICES` env or `""` | Comma-separated exact `namespace/name:port` cross-namespace gateway Service refs; wildcards are rejected |
| `--outbound-access-trusted-token-endpoint-services` | `ORKA_OUTBOUND_ACCESS_TRUSTED_TOKEN_ENDPOINT_SERVICES` env or `""` | Comma-separated exact `namespace/name:port` cross-namespace token endpoint Service refs; wildcards are rejected |
| `--task-provenance-admission-enabled` | `ORKA_TASK_PROVENANCE_ADMISSION_ENABLED` env or `false` | Enable validating admission that rejects untrusted direct Kubernetes Task writes to Orka-managed provenance fields (`spec.requestedBy`, `spec.transaction`, and transaction metadata labels/annotations) |
| `--task-provenance-admission-external` | `ORKA_TASK_PROVENANCE_ADMISSION_EXTERNAL` env or `false` | Declare that a separately deployed fail-closed Task provenance webhook protects coordination ancestry and workspace settlement metadata without hosting a manager webhook |
| `--task-provenance-admission-trusted-users` | `ORKA_TASK_PROVENANCE_ADMISSION_TRUSTED_USERS` env or controller ServiceAccount usernames | Comma-separated Kubernetes usernames trusted to set Orka-managed Task provenance fields |
| `--task-provenance-admission-trusted-service-accounts` | `ORKA_TASK_PROVENANCE_ADMISSION_TRUSTED_SERVICE_ACCOUNTS` env or configured AI/vendor worker ServiceAccounts | Comma-separated ServiceAccount names trusted in the target Task namespace to set Orka-managed Task provenance fields for child Task creation. Explicit values override the worker ServiceAccount defaults. |
| `--ai-worker-image` | `ghcr.io/orka-agents/orka/ai-worker:latest` | Native AI worker container image |
| `--acp-runtime-namespace` / `ORKA_ACP_RUNTIME_NAMESPACE` | `orka-runtimes` | Namespace for managed runtime Deployments, Services, Secrets, and policies. |
| `--acp-provider-proxy-namespace` / `ORKA_ACP_PROVIDER_PROXY_NAMESPACE` | `""` | Approved provider-proxy namespace selector. The Helm chart uses its release namespace when the proxy is enabled. |
| `--acp-provider-proxy-base-url` / `ORKA_ACP_PROVIDER_PROXY_BASE_URL` | unset | Authenticated provider-proxy URL injected into built-in RuntimePools. |
| `--acp-provider-proxy-pod-labels` / `ORKA_ACP_PROVIDER_PROXY_POD_LABELS` | `orka.ai/network-role=provider-auth-proxy` | Exact Pod labels selected by RuntimePool egress policy. |
| `--acp-provider-proxy-token-file` / `ORKA_ACP_PROVIDER_PROXY_TOKEN_FILE` | unset | Controller-mounted bearer file copied into generation-scoped immutable RuntimePool Secrets. |
| `--acp-codex-runtime-image` / `ORKA_ACP_CODEX_RUNTIME_IMAGE` | unset | Codex runtime image with an explicit tag or SHA256 digest. Tags are resolved at startup. |
| `--acp-claude-runtime-image` / `ORKA_ACP_CLAUDE_RUNTIME_IMAGE` | unset | Claude runtime image with an explicit tag or SHA256 digest. Tags are resolved at startup. |
| `--acp-copilot-runtime-image` / `ORKA_ACP_COPILOT_RUNTIME_IMAGE` | unset | GitHub Copilot runtime image with an explicit tag or SHA256 digest. Tags are resolved at startup. |
| `--acp-opencode-runtime-image` / `ORKA_ACP_OPENCODE_RUNTIME_IMAGE` | unset | OpenCode runtime image with an explicit tag or SHA256 digest. Tags are resolved at startup. |
| `--general-worker-image` | `ghcr.io/orka-agents/orka/general-worker:latest` | General worker container image |
| `--store-backend` | `sqlite` | Payload/read-model backend. ACP control authority remains Kubernetes CRDs and Leases. |
| `--store-path` | `/data/orka.db` | Path to the SQLite transcript/outbox/artifact database file. |
| `--ai-worker-service-account-name` | `orka-ai-worker` | ServiceAccount name for AI worker Jobs and dynamically ensured worker RBAC |
| `--vendor-worker-service-account-name` | `orka-vendor-worker` | ServiceAccount name for vendor/agent worker Jobs and dynamically ensured worker RBAC |
| `--container-worker-service-account-name` | `orka-container-worker` | ServiceAccount name for container worker Jobs and dynamically ensured worker RBAC |
| `--chat-enabled` | `true` | Enable the chat endpoint |
| `--chat-provider` | `""` | Default Provider CRD name for chat |
| `--chat-model` | `""` | Default model for chat |
| `--chat-max-iterations` | `50` | Max tool execution loops per chat request |
| `--chat-max-duration` | `30m` | Max wall-clock time per chat request |
| `--chat-tool-timeout` | `60s` | Max time for single tool execution |
| `--chat-max-concurrent` | `10` | Max concurrent chat sessions |
| `--chat-max-tasks-per-turn` | `5` | Max tasks created per chat turn |
| `--native-session-max-bytes` | `8388608` | Maximum encoded native session bundle bytes. Environment default: `ORKA_NATIVE_SESSION_MAX_BYTES`; hard ceiling: `67108864`. Propagated to built-in runtimes. Local migration CLI uses `--max-bundle-bytes`. |
| `--chat-max-session-size` | `512000` | Soft limit for session size before truncation (bytes) |
| `--leader-elect` | `false` | Enable leader election. Static controller installations require `true`; the Lease is stored in the watched namespace. |
| `--metrics-bind-address` | `0` | Metrics endpoint address |
| `--health-probe-bind-address` | `:8081` | Health probe address |
| `--metrics-secure` | `true` | Serve metrics via HTTPS |
| `--enable-http2` | `false` | Enable HTTP/2 for metrics and webhook servers |
| `--enable-telemetry` / `--enable-tracing` | `false` | Enable OpenTelemetry traces and metrics (requires worker-reachable OTLP endpoint for worker telemetry) |

### Agent hosts {#agent-hosts}

Long-lived, single-user agent hosts on Orka-owned Pods. Off by default. See
[Agent hosts](../operations/agent-hosts.md).

| Flag | Environment variable | Default | Purpose |
| --- | --- | --- | --- |
| `--enable-agent-hosts` | `ORKA_ENABLE_AGENT_HOSTS` | `false` | Serves `AgentHostTemplate` and `AgentHost`. Helm: `controller.agentHosts.enabled`. |
| `--agent-host-namespace` | `ORKA_AGENT_HOST_NAMESPACE` | `orka-agent-hosts` | Namespace for host Pods, volumes, NetworkPolicies and Secrets. Must differ from `--watch-namespace`. Helm: `controller.agentHosts.namespace`. |
| `--agent-host-agent-image` | `ORKA_AGENT_HOST_AGENT_IMAGE` | — | Digest-pinned image with the `orka-host-agent` sidecar, normally the controller image. Required with `--enable-agent-hosts`. Helm: `controller.agentHosts.agentImage`. |
| `--agent-host-controller-pod-labels` | `ORKA_AGENT_HOST_CONTROLLER_POD_LABELS` | `orka.ai/network-role=controller` | Labels of the controller Pods that may read host status through each host's NetworkPolicy. The default matches the shipped controller Pods. |

### Workspace providers (`workspace.orka.ai/v1alpha1`) {#workspace-providers}

A separately deployed **workspace provider** realizes Orka's frozen public runtime
request. Core owns runtime credentials, admission, and Task settlement; the
provider owns infrastructure and reports exact startup and termination evidence.
The external repository supplies [Agent Sandbox](../concepts/agent-sandbox.md),
[Substrate](../concepts/substrate.md), and a development fake provider.

#### Enablement

| Core flag | Environment variable | Purpose |
| --- | --- | --- |
| `--enable-workspace-provider-api` | `ORKA_ENABLE_WORKSPACE_PROVIDER_API` | Enables shared provider, class, pool, workspace, and checkpoint reconciliation. |
| `--task-provenance-admission-enabled=true` or `--task-provenance-admission-external=true` | — | Protects reserved Task provenance and settlement metadata. |
| `--workspace-class-use-admission-enabled=true` | — | Enables live class-use authorization through the required webhook. |
| `--acp-workspace-dispatch-enabled` | `ORKA_ACP_WORKSPACE_DISPATCH_ENABLED` | Allows class-backed ACP workspace dispatch. |

Core does not accept Sandbox, Substrate ACP, or in-process fake provider flags.
Install each provider as a separate Deployment with its own ServiceAccount,
leader-election Lease, CRDs, registration, and immutable configuration. Use the
[external provider installation instructions](https://github.com/orka-agents/orka-workspace/blob/main/README.md).
Native MCP Tools retain the separate `--substrate-mcp-tools-enabled` flag.

The source Helm chart enables provenance and class-use admission for `harness-v2`.
`controller.executionWorkspace.dispatchEnabled` controls ACP dispatch;
`controller.executionWorkspace.workerNamespaces` grants only Pod read access for
independent native worker attestation. Configure provider mutation permissions in
its own installation. With dedicated admission, verify fail-closed webhooks before
enabling `--task-provenance-admission-external`.

:::warning[Drain legacy owners before changing the schema]
Before applying the target RuntimePool CRD or starting the new core binary, drain
old in-tree allocations and retained records through the original owner. This
includes legacy workspaces in `Ready`, `Suspended`, `Failed`, and `Deleted` states,
plus native journals and checkpoint references. The startup gate checks all
namespaces, refuses leftovers, and never adopts or removes their finalizers.
Helm does not update CRDs during upgrades. Apply the new schemas only after the
drain, then install the new binary and external providers. See
[external workspace migration](../operations/upgrading.md#external-workspace-migration).
:::

#### Who owns what

| Object | Scope | Owner | Holds |
| --- | --- | --- | --- |
| `ExecutionWorkspaceProvider` | cluster | operator | Installed controller identity, ServiceAccount, contracts, and config reference. |
| Provider config | cluster | operator | `SandboxProviderConfig`, `SubstrateProviderConfig`, or the installed provider's own config kind. |
| Provider profile | namespaced | operator | `SandboxWorkspaceProfile` or `SubstrateWorkspaceProfile`; immutable parameters in the class namespace. |
| `ExecutionWorkspaceClass` | namespaced | operator | Provider/profile binding, required features, reuse policy, and lifecycle bounds. |
| `Task.spec.execution.workspace.classRef` | — | user | Authorized class selection, with optional permitted reuse/detach choices. |

Sandbox config/profile kinds use `sandbox.workspace.orka.ai/v1alpha1`; Substrate
uses `substrate.workspace.orka.ai/v1alpha1`. Development fake kinds use
`fake.workspace.orka.ai/v1alpha1`. Core resolves these through their advertised
API references. It does not own or register their backend settings.

`kubectl get executionworkspaceclass` shows lifecycle actions, idle timeout, and
maximum lifetime; `-o wide` adds the detach timeout. An ACP-only class explicitly
requires `acp.runtime.v2`, rather than claiming unsupported generic exec/TLS features.

#### Who is allowed to use a class

Selecting a class is an authorization decision, checked with a live Kubernetes
`SubjectAccessReview` for verb `use` on that `ExecutionWorkspaceClass`. A denied *or
unavailable* SAR denies the request.

Shipped `ValidatingAdmissionPolicy` resources enforce this at all times, even with the
workspace gates off. With the provider API enabled, a TLS-backed webhook is required as
well (`--workspace-class-use-admission-enabled`). How that webhook gets installed depends
on how you installed Orka:

| Install | Webhooks | What you do |
| --- | --- | --- |
| Helm, `harness-v2` | `task-workspace-class.harness-v2.orka.ai`, `tool-workspace-class.harness-v2.orka.ai` | Nothing — the chart renders them against the release's own webhook Service and sets the flag. Requires `webhooks.tls.existingSecret` plus either `webhooks.caBundle` or `webhooks.caInjectionAnnotations`. |
| Kustomize | `taskworkspaceclassuse.core.orka.ai`, `toolworkspaceclassuse.core.orka.ai` | Apply `config/orka-admission` first, then `config/orka-admission-webhooks` once its README's readiness and TLS prerequisites are met. |

:::danger[Do not apply the Kustomize admission packages to a Helm release]
You get two independent sets of validating webhooks for the same resources.
:::

#### Binding a Task to a class

```yaml
spec:
  execution:
    workspace:
      classRef:
        name: my-workspace-class
      reusePolicy: session   # optional
      workspaceSlot: default # optional
      onDetach: Delete       # optional
```

On admission the controller freezes the class identity, a hash of the profile, the
lifecycle settings, and the effective detach action into the Task's immutable execution
snapshot. Later edits to the class do not reach in-flight Tasks.

`workspaceSlot` composes with `reusePolicy: none`. Session reuse supports only the default
slot and fails closed for any other value, until RuntimeSession controls become
slot-scoped.

#### What happens when the agent detaches

| `onDetach` | agent-sandbox | Substrate |
| --- | --- | --- |
| `Delete` | Works | Works |
| `Suspend` | Requires `SandboxWorkspaceProfile.spec.suspend` | Requires `SubstrateWorkspaceProfile.spec.suspend` with `mode: DataOnly` |

`Delete` is always executable.

Agent-sandbox suspension is cold, never a memory snapshot. The profile's
`SandboxWorkspaceProfile.spec.suspend.volume` freezes a durable workspace PVC shape
(`capacity`, optional `storageClassName` and `accessModes`); the provider's claim requests it, which
forces a cold start instead of adopting a warm sandbox. Suspending patches that exact
Sandbox to `operatingMode: Suspended` so its Pod terminates while the PVC survives.
Resume rotates the bootstrap material, refreshes the Sandbox blueprint, and returns the
Sandbox to `Running` against the preserved volume.

Substrate DataOnly suspension uses verified native Tags, exact worker Pod
termination, and fresh Actors for cold continuation. Its native lifecycle calls
have no UID/version preconditions; Orka journals its own operation intents and
requires fresh authenticated admission. Full-memory restore remains disabled.
See [Substrate workspaces](../concepts/substrate.md) for the trusted provider
boundary, checkpoint export, restore authorization, and explicit recovery.

#### Expiry

A suspend-capable class **must** set `idleTimeout` or `maxLifetime`. Without one, class
readiness and Task binding fail closed.

| Setting | Where | Effect |
| --- | --- | --- |
| `idleTimeout` | class lifecycle | Idle suspended workspaces expire; idle `Ready` workspaces take the class default action. |
| `maxLifetime` | class lifecycle | Hard cleanup bound, always. |
| `spec.retention.maxSuspendedWorkspaces` | provider-owned workspace profile | Caps concurrently suspended workspaces per class and namespace. Rejected at admission, retried at settlement with the frozen Suspend action preserved. This is a cap, not an expiry. |

A queued continuation may take a still-`Ready` workspace directly. Deletion policies that
retain data past workspace deletion are rejected.

ADRs 0026–0030 carry the full contract.

### External API OIDC authentication

ServiceAccount bearer token authentication is always available. To allow external callers such as GitHub Actions to authenticate directly with OIDC JWTs, configure issuer, audience, an explicit subject allowlist, and the namespace assigned to OIDC callers:

```bash
--oidc-issuer=https://token.actions.githubusercontent.com
--oidc-audience=orka-ci
--oidc-allowed-subjects=repo:my-org/my-repo:ref:refs/heads/main
--oidc-namespace=ci
```

The same settings can be supplied with environment variables:

```bash
ORKA_OIDC_ISSUER=https://token.actions.githubusercontent.com
ORKA_OIDC_AUDIENCE=orka-ci
ORKA_OIDC_ALLOWED_SUBJECTS=repo:my-org/my-repo:ref:refs/heads/main
ORKA_OIDC_NAMESPACE=ci
# Optional; when omitted, Orka discovers the JWKS URL from the issuer metadata.
ORKA_OIDC_JWKS_URL=https://token.actions.githubusercontent.com/.well-known/jwks
```

OIDC validation requires RS256-signed JWTs with matching `iss` and `aud`, valid time claims, a non-empty `sub`, and a `sub` value that matches `--oidc-allowed-subjects`. Wildcards `*` and `?` are supported in allowlist patterns; use the narrowest GitHub Actions subject for the trusted repository, branch, environment, or workflow. Authorized OIDC callers are bound to `--oidc-namespace` (or `default` when omitted) so namespace isolation can reject requests for other namespaces. When an OIDC-authenticated caller creates a Task, Orka records the verified identity in `spec.requestedBy`. Clients cannot set `requestedBy` themselves.

### External API context-token authentication

Orka can also authenticate external API requests with generic transaction/context tokens. The built-in `transaction-token` profile validates RS256-signed JWTs with JOSE header `typ: txntoken+jwt`, matching `iss` and `aud`, valid time claims, a non-empty `sub`, and the required transaction-token claims `iat`, `txn`, `scope`, and `req_wl`.

For the strict profile contract, see [Transaction Tokens](../concepts/transaction-tokens.md). To move from the removed Kontxt profile, see [Migrate from the Kontxt profile](../concepts/transaction-tokens.md#migrate-from-the-kontxt-profile).

Enable the profile by configuring the profile, issuer, and audience:

```bash
--context-token-profile=transaction-token
--context-token-issuer=https://issuer.example.com
--context-token-audience=orka-api
```

The same settings can be supplied with environment variables:

```bash
ORKA_CONTEXT_TOKEN_PROFILE=transaction-token
ORKA_CONTEXT_TOKEN_ISSUER=https://issuer.example.com
ORKA_CONTEXT_TOKEN_AUDIENCE=orka-api
# Optional for transaction-token; when omitted, Orka uses <issuer>/.well-known/jwks.json.
ORKA_CONTEXT_TOKEN_JWKS_URL=https://issuer.example.com/.well-known/jwks.json
```

By default, the `transaction-token` profile reads raw transaction tokens from the `Txn-Token` header:

```bash
curl -H "Txn-Token: $TXN_TOKEN" https://orka.example.com/api/v1/tasks
```

To customize token locations, set `--context-token-headers` or `ORKA_CONTEXT_TOKEN_HEADERS` to a comma-separated list. Use `Header` for raw token headers and `Header:Scheme` for scheme-prefixed headers. For example, keep the default `Txn-Token` header and explicitly opt in to `Authorization: Bearer` context-token support:

```bash
--context-token-headers=Txn-Token,Authorization:Bearer
```

`Authorization: Bearer` remains the default location for Kubernetes ServiceAccount and OIDC JWT authentication. Context-token bearer authentication is only attempted when `Authorization:Bearer` is explicitly configured and the bearer JWT has `typ: txntoken+jwt`; other bearer tokens continue through the standard OIDC or Kubernetes TokenReview flow. When an external context-token caller creates a Task, Orka records the verified subject and issuer in immutable `spec.requestedBy` and records safe transaction metadata in immutable `spec.transaction`, transaction labels, and transaction annotations. Clients cannot set `requestedBy` or `transaction` themselves.

Optional authorization is controlled by `--context-token-authz-mode` / `ORKA_CONTEXT_TOKEN_AUTHZ_MODE`. In `audit` mode, Orka logs safe authorization failures and allows the request. In `enforce` mode, Orka rejects context-token callers that lack the configured operation scope or violate signed `tctx` constraints. Task creation can be constrained by `tctx.namespace`, `tctx.taskType`, `tctx.agent`, `tctx.allowedAgents`, workspace `tctx.repo`/`tctx.branch`/`tctx.ref`, and `tctx.allowedTools`. Chat, OpenAI-compatible, and Anthropic-compatible model calls require the provider-use scope (default `orka:providers:use`) and honor `tctx.namespace`, `tctx.provider`, `tctx.allowedProviders`, `tctx.model`, and `tctx.allowedModels`. When Orka-managed server-side tools are exposed to those endpoints, they also require the tool-use scope (default `orka:tools:use`) and honor `tctx.allowedTools`. Security scan read/list/get endpoints require the security-read scope (default `orka:security:read`), and security scan create/update/delete and mutation endpoints require the security-write scope (default `orka:security:write`). Repository monitor read endpoints require the monitor-read scope (default `orka:monitors:read`), monitor create/update/delete endpoints require the monitor-write scope (default `orka:monitors:write`), and manual monitor runs require the monitor-operate scope (default `orka:monitors:operate`). Repository monitor access can also be constrained by `tctx.namespace`, `tctx.repo`, `tctx.branch`, `tctx.agent`, and `tctx.allowedAgents`. The raw TxToken is never logged or persisted in Task specs/status.

### Transaction-token TTS exchange and propagation

Configure `--context-token-tts-endpoint` / `ORKA_CONTEXT_TOKEN_TTS_ENDPOINT` when workers should exchange a mounted subject token for child or outbound replacement TxTokens. Delegation tools require `ORKA_CONTEXT_TOKEN_SUBJECT_TOKEN_FILE` and `ORKA_CONTEXT_TOKEN_CHILD_SCOPE`; HTTP Tool calls can use `ORKA_CONTEXT_TOKEN_OUTBOUND_SCOPE` or fall back to the current transaction scope. Child scopes are fail-closed: Orka rejects a requested child scope that is not already present in the parent transaction scopes before it creates the child Task.

Successful delegation exchanges store the raw child TxToken only in an owner-referenced Kubernetes Secret and annotate the child Task with the Secret name. The controller mounts that Secret into the child worker and sets `ORKA_TRANSACTION_TOKEN_FILE` / `ORKA_CONTEXT_TOKEN_SUBJECT_TOKEN_FILE` so deeper delegation and downstream Tool calls can continue the same transaction with configured child/outbound scopes.

### Task provenance admission hardening

The REST API rejects client-supplied `requestedBy` and `transaction` fields and stamps verified provenance itself. To also protect direct Kubernetes `Task` CRD writes, enable the optional validating admission webhook:

```bash
--task-provenance-admission-enabled=true
```

The webhook denies untrusted `CREATE` or `UPDATE` requests that set or modify Orka-managed provenance fields: `spec.requestedBy`, `spec.transaction`, `orka.ai/transaction-*` labels/annotations, `orka.ai/context-token-profile`, and the child token Secret annotation. By default, trusted writers are the Orka controller ServiceAccount usernames in the controller namespace and the configured AI and vendor worker ServiceAccount names in the target Task namespace; override them with `--task-provenance-admission-trusted-users` and `--task-provenance-admission-trusted-service-accounts`.

Worker-created coordination children must name the caller's active Task as parent, verified through the request's Pod identity and the live Pod, Job, and Task ownership chain. A worker-created Task may omit `sessionRef` or inherit its parent's reference unchanged, including any history cutoff. Workers cannot introduce arbitrary sessions through a new child or an ownerless Task. Trusted controller writers can establish separately authorized session references.

The webhook also makes coordination parent names and the parent Task controller-owner identity immutable after creation, including for trusted workers and controllers. Kubernetes cleanup controllers may remove ownership when orphaning a dependent; that Task then loses access through the removed parent. Internal cross-task transcript searches and coordination messages require protection through manager-hosted admission (`--task-provenance-admission-enabled=true`) or a separately deployed webhook (`--task-provenance-admission-external=true`). With both flags disabled, internal transcript search is limited to the caller's own session and coordination message requests are denied.

Enabling either flag asserts that all retained Task ancestry was created under these checks. Admission does not validate old edges retroactively, and an unchanged update does not attest them. Before first enabling ancestry trust, or upgrading from a validator that did not authenticate coordination parents, follow the Task cleanup requirement below.

Internal transcript search applies `sessionRef.maxMessages` and `sessionRef.throughMessageId` before matching messages or limiting results. When multiple Tasks reference the same session, search uses the intersection of their permitted history windows. An unbounded reference cannot widen a bounded one, and a missing cutoff message yields no matches for that session.

How admission is deployed depends on the installation method. Helm releases install and enable Task-provenance admission automatically: the chart renders `task-provenance.<mode>.orka.ai` with `failurePolicy: Fail` against the release-local controller webhook Service and runs the controller with `--task-provenance-admission-enabled=true`, trusting the release controller identity. For Kustomize installations, admission deployment is opt-in and served by the dedicated admission runtime, not the controller manager: install `config/orka-admission` (Deployment, Service, NetworkPolicy, and RBAC for the admission runtime), then apply `config/orka-admission-webhooks` — which includes `taskprovenance.core.orka.ai` with `failurePolicy: Fail` — only after the readiness, TLS Secret, and CA-injection prerequisites in `config/orka-admission-webhooks/README.md` are met and the trusted identities embedded in `validating_webhook.yaml` match the admission-runtime arguments.

The first `config/acp-production` workload wave leaves ancestry trust disabled. Enable `--task-provenance-admission-external=true` in the controller's post-admission configuration only after the separate webhook is installed and rejects unauthorized Task provenance changes. The flag enables ancestry trust for both internal worker API and brokered MCP coordination policy. Keep it disabled if the webhook wave is omitted, and disable it and complete the controller rollout before removing the webhook.

### Upgrading internal worker authorization

If existing Tasks were created without authenticated ancestry admission, prevent new Task creation in each affected watched namespace throughout the cutover. Drain all Tasks under the old controller, including ACP and remote-runtime agent Tasks. Preserve required outputs outside Task cleanup, then delete every pre-existing Task in those namespaces and wait for its finalizers to finish. Retained terminal Tasks must also be removed because their session references participate in coordination searches.

Install the updated fail-closed admission policy before enabling either provenance flag and allowing Task creation again. An unchanged update or retained labels and owner references do not attest legacy ancestry. Helm enables the flag automatically, so complete this cleanup before upgrading that release. Routine restarts with continuously enforced ancestry admission do not require this cleanup.

Before upgrading from a controller that does not record `Task.status.jobUID`, pause Task producers and drain all Job-backed Tasks while the old controller is still running. Wait until their attempts are terminal and their results and artifacts are stored. Apply the CRDs from the exact target chart, upgrade the controller and admission components, then resume Task producers. A rolling upgrade with active legacy Job-backed Tasks is not supported.

The new authorization checks require the controller-recorded Job UID. They reject workers whose Tasks have only `status.jobName`, including otherwise valid Pods from the previous controller. Orka does not backfill UIDs by looking up Job names: namespace Job creators can replace those Jobs. Do not patch missing UIDs from a name lookup; finish the attempt before upgrading or explicitly submit a new Task after the upgrade.


## Prometheus metrics

Orka registers the following Prometheus metrics on the controller-runtime registry. The metrics endpoint is disabled by default (`--metrics-bind-address=0`); enable it by setting an explicit bind address, for example:

```bash
--metrics-bind-address=:8443   # HTTPS (default when --metrics-secure=true)
--metrics-bind-address=:8080   # HTTP, with --metrics-secure=false
```

Scrape configuration (for example a Prometheus Operator ServiceMonitor) is not shipped with the chart; point your monitoring stack at the metrics port directly.

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `orka_api_requests_total` | Counter | `endpoint`, `method`, `status` | Total API requests (status bucketed as `2xx`/`4xx`/`5xx`) |
| `orka_api_request_duration_seconds` | Histogram | `endpoint`, `method` | API request latency in seconds |
| `orka_skills_loaded_total` | Counter | `skill`, `namespace` | Skills loaded by namespace and name |
| `orka_store_db_size_bytes` | Gauge | — | Size of the SQLite database file in bytes |
| `orka_context_token_auth_total` | Counter | `profile`, `result` | Context-token authentication attempts |
| `orka_context_token_authorization_total` | Counter | `action`, `result`, `reason` | Context-token authorization decisions (allow/deny/audit) |
| `orka_context_token_tts_exchange_total` | Counter | `result`, `reason` | transaction-token TTS token-exchange attempts |
| `orka_context_token_tts_exchange_duration_seconds` | Histogram | `result`, `reason` | transaction-token TTS token-exchange latency in seconds |
| `orka_token_exchange_total` | Counter | `adapter`, `grant_class`, `result`, `reason` | Direct and transaction-token OAuth exchange attempts |
| `orka_token_exchange_duration_seconds` | Histogram | `adapter`, `grant_class`, `result`, `reason` | OAuth exchange latency in seconds |
| `orka_acp_runtime_pool_desired_replicas` | Gauge | `namespace`, `runtime_pool` | Desired RuntimePool replicas |
| `orka_acp_runtime_pool_ready_replicas` | Gauge | `namespace`, `runtime_pool` | Authoritatively selected Ready RuntimePool replicas |
| `orka_acp_runtime_pool_sessions_active` | Gauge | `namespace`, `runtime_pool` | Authenticated resident RuntimeSession count |
| `orka_acp_runtime_pool_prompts_in_flight` | Gauge | `namespace`, `runtime_pool` | Authenticated active prompt count |
| `orka_acp_runtime_pool_queued_tasks` | Gauge | `namespace`, `runtime_pool` | Durable unsatisfied Task demand assigned to the pool |
| `orka_acp_runtime_pool_admission_state` | Gauge | `namespace`, `runtime_pool`, `state` | One-hot authoritative admission state (`unknown`, `closed`, `accepting`, `draining`, or `ambiguous`) |
| `orka_acp_runtime_pool_scale_to_zero_total` | Counter | `namespace`, `runtime_pool` | Completed RuntimePool scale-to-zero transitions |
| `orka_approval_decision_duration_seconds` | Histogram | `outcome`, `path` | Seconds from approval request to committed decision; see [Observability](../guides/observability.md#approval-decision-duration) |

All context-token metric labels use low-cardinality values only. See [Transaction tokens](../concepts/transaction-tokens.md) for the authentication they measure.

## OpenTelemetry

Telemetry is off by default. Enable it with `--enable-telemetry` and point the controller at
an OTLP collector. [Observability](../guides/observability.md) explains what Orka exports,
how settings reach workers and runtime supervisors, collector routing, and privacy.

| Flag / Environment Variable | Default | Description |
|------------------------------|---------|-------------|
| `--enable-telemetry` / `--enable-tracing` | `false` | Enable OpenTelemetry traces and metrics |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | SDK default `localhost:4317` | OTLP collector endpoint for traces and metrics |
| `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` | unset | Trace-specific OTLP endpoint |
| `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` | unset | Metrics-specific OTLP endpoint |
| `OTEL_EXPORTER_OTLP_PROTOCOL` | SDK default gRPC | Set to `http/protobuf` for OTLP/HTTP collectors |
| `OTEL_EXPORTER_OTLP_TRACES_PROTOCOL` / `OTEL_EXPORTER_OTLP_METRICS_PROTOCOL` | unset | Signal-specific exporter protocol overrides |
| `OTEL_EXPORTER_OTLP_INSECURE` and signal-specific insecure vars | SDK default | Disable TLS for in-cluster/dev collectors that require it |
| `OTEL_TRACES_SAMPLER` / `OTEL_TRACES_SAMPLER_ARG` | SDK default | Standard OpenTelemetry sampler configuration |
