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

The `../controller-webhook` component makes the controller serve the
fail-closed `orka-admission` webhooks, scoped to `orka-system`, as the Helm
chart does. The controller's certificate rotator fills the empty
`orka-webhook-tls` Secret, renews it before expiry, and injects its CA into the
webhook configuration, so no certificate needs to be provisioned. The deploy
script applies the webhook configuration with the other prerequisites, waits
for the controller to become ready (which requires the CA injection), and then
removes the standalone `orka-admission` runtime left by earlier releases.
Webhook-protected writes are rejected while no controller Pod is serving, for
example between the old and new Pod of a `Recreate` rollout.

For same-cluster v1/v2 operation, deploy v1 as a separate release with a
different release namespace, watched namespace, endpoint, RBAC, storage, and
data plane. See `docs/harness-v1-v2-coexistence-plan.md`.
