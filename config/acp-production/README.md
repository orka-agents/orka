# ACP production overlay

This is the canonical direct-Kustomize deployment surface. It includes the
cross-namespace Vekil ingress policy and renders controller, provider proxy,
SCM proxy, and Workspace/Publisher images by immutable digest.

The checked-in all-zero digests are intentional fail-closed placeholders. Use
`make deploy` with digest-pinned `IMG`, `WORKSPACE_PUBLISHER_IMG`,
`ACP_CODEX_RUNTIME_IMG`, `ACP_CLAUDE_RUNTIME_IMG`,
`ACP_COPILOT_RUNTIME_IMG`, and `ACP_OPENCODE_RUNTIME_IMG`, or replace all four
runtime entries in `runtime-images.env` before applying. Never deploy a rendered
all-zero placeholder.

The production overlay intentionally excludes CRDs. Before the first workload
deployment, run `scripts/upgrade-orka-crds.sh` with verified backup markers and
resolve every reported v1/legacy blocker. `make deploy` verifies that cutover
state before applying only workload resources.
