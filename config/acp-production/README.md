# ACP production overlay

This is the canonical direct-Kustomize deployment surface. It includes the
cross-namespace Vekil ingress policy and renders controller, provider proxy,
SCM proxy, replicated stateless admission runtime, and Workspace/Publisher
images by immutable digest. The fail-closed admission policies remain a
separate post-readiness wave and are intentionally absent from this overlay.

The checked-in all-zero digests are intentional fail-closed placeholders. Use
`make deploy` with digest-pinned `IMG`, `WORKSPACE_PUBLISHER_IMG`,
`ACP_CODEX_RUNTIME_IMG`, `ACP_CLAUDE_RUNTIME_IMG`,
`ACP_COPILOT_RUNTIME_IMG`, and `ACP_OPENCODE_RUNTIME_IMG`, or replace all four
runtime entries in `runtime-images.env` before applying. Never deploy a rendered
all-zero placeholder.

The production overlay intentionally excludes CRDs. Apply the reviewed dual
v1/v2 bridge CRDs as an explicit upgrade wave before the first workload wave;
fresh clusters may use `make install`. Existing installations require the
source-aware coexistence migration procedure and must not use the v2-only
`scripts/upgrade-orka-crds.sh` hard-cutover helper. `make deploy` verifies the
live dual AgentRuntime schema and all coexistence control CRDs before applying
only workload resources.

The supported `make deploy` path creates the snapshot-encryption Secret when it
is absent, retains and validates an existing key without printing it, and
creates the fixed `orka-system/cluster` `AgentExecutionControl` only when it is
absent. Later deploys never reapply the bootstrap modes, so operator-managed
coexistence transitions are preserved.

Before `make deploy`, provision `orka-system/orka-admission-tls` with
`tls.crt`, `tls.key`, and `ca.crt`; the serving certificate must cover
`orka-admission.orka-system.svc`. Deployment fails closed when that material is
missing. The apply script rolls out two admission replicas, waits for two ready
Service endpoints, sends an AdmissionReview smoke request to every protected
handler through the Kubernetes Service proxy, and pins the webhook CA bundle
from `ca.crt`. It activates the fail-closed
`../orka-admission-webhooks` wave before rolling the coexistence controller,
then waits for the controller to report the current control generation.
