# ACP harness-v2 production overlay

This is the canonical direct-Kustomize deployment surface for one static
`harness-v2` Orka installation. It includes the cross-namespace Vekil ingress
policy and renders controller, provider proxy, SCM proxy, and
Workspace/Publisher images by immutable digest. It never deploys the harness
v1 wrapper and cannot adopt or continue work from a v1 installation.

The checked-in all-zero digests are intentional fail-closed placeholders. Use
`make deploy` with digest-pinned `IMG`, `WORKSPACE_PUBLISHER_IMG`,
`ACP_CODEX_RUNTIME_IMG`, `ACP_CLAUDE_RUNTIME_IMG`,
`ACP_COPILOT_RUNTIME_IMG`, and `ACP_OPENCODE_RUNTIME_IMG`, or replace all four
runtime entries in `runtime-images.env` before applying. Never deploy a rendered
all-zero placeholder.

The production overlay intentionally excludes CRDs. Apply the reviewed shared
v1/v2-compatible CRD bundle through one designated cluster-level owner before
the workload wave; fresh clusters may use `make install`. Every other Orka
release on the cluster must leave CRD ownership with that owner. `make deploy`
verifies the required live schema before applying only workload resources.

The controller requires a non-empty watched namespace labeled
`orka.ai/controller-mode: harness-v2`. Its leader-election Lease, SQLite store,
ServiceAccount, API Service, Secrets, and runtime namespace belong only to this
installation. Do not point it at a namespace watched by a `harness-v1`
controller, reuse a v1 PVC, or change the namespace label in place.

This overlay is also not an adoption path for a pre-static controller that
implicitly enabled ACP. `scripts/apply-acp-production.sh` inspects the live
namespace and any existing controller before its first write. An existing
namespace must already claim static `harness-v2`; any live controller must also
declare that mode and the `orka-system` watch namespace. A missing controller
is recoverable only under that retained namespace claim. Settle or retire older
installations and deploy this overlay as a fresh installation and namespace.

This overlay deploys the replicated `orka-admission` runtime but not the
cluster-scoped `ValidatingWebhookConfiguration`. Provision
`orka-system/orka-admission-tls` first, wait for both admission endpoints, and
smoke-test every retained handler before the designated cluster admission
owner applies `config/orka-admission-webhooks`. A shared admission owner must
configure every isolated controller ServiceAccount as an exact trusted
username; individual releases must not race to own the webhook configuration.

For same-cluster v1/v2 operation, deploy v1 as a separate release with a
different release namespace, watched namespace, endpoint, RBAC, storage, and
data plane. See `docs/harness-v1-v2-coexistence-plan.md`.
