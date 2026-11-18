---
name: agent-substrate-deploy
description: Stand up official Agent Substrate on a dedicated gVisor kind cluster and validate Orka direct, MCP, ACP, and data-only checkpoint paths.
---

# Agent Substrate deployment

Use the unmodified official provider pinned in
`hack/agent-substrate/upstream.env`. Read
`website/docs/concepts/substrate.md` for the native API and lifecycle contract.
This workflow is local evaluation, not a production provider installer.

## Run the existing installer

Drive `hack/demos/cluster/install-substrate.sh` in place. It retains the cluster
and calls the same `scripts/agent-substrate-e2e.sh` used by CI. The suite builds
the provider from its official pin and runs direct, MCP, and real ACP runtime
execution against a local Responses fixture. It needs no model or Git account.

1. Check Docker, kind, ko, Go 1.27, git, jq, kubectl, openssl, Python, curl, and
   ripgrep. Install missing local CLI dependencies as needed. Do not restart
   shared Docker infrastructure or delete another cluster to repair preflight.
2. Choose a dedicated cluster and run directory. Substrate's gVisor/node setup
   owns this cluster; it cannot be installed onto a default kindctl cluster.
3. Run the installer, then use only its scoped kubeconfig for follow-up work.

```bash
export PATH="$PWD/bin:$(go env GOPATH)/bin:$PATH"
export KIND_CLUSTER=orka-substrate-eval
export SUBSTRATE_E2E_RUN_DIR="$PWD/bin/substrate-eval"
bash hack/demos/cluster/install-substrate.sh
export KUBECONFIG="$SUBSTRATE_E2E_RUN_DIR/kubeconfig"
```

An existing cluster requires `SUBSTRATE_REUSE_CLUSTER=1` or
`DEMO_CLUSTER_REUSE=reuse`. Reuse installs the selected official version and
reruns tests with fixed resource names. Inspect existing objects first; do not
replace application data or a completed model login. Choose a fresh cluster
when reuse is unsuitable. The installer never automatically recreates one.
The source URL and commit must match the pin; arbitrary ref overrides and
provider patches are rejected.

## Verify

Read `references/validate.md`. Do not claim live conformance from a doctor or
unit test result. Keep operation journals and credentials out of user-facing
logs. Native Actors and templates are ate-api resources, not Kubernetes CRDs.

For a real model provider, use the existing Vekil deployment skill separately.
If that starts device-code authentication, surface the URL and code and wait
for the user to complete it. Never complete the login on their behalf.

## Troubleshoot

Read `references/troubleshooting.md`. Preserve source and last verified data
when a boot or checkpoint is uncertain. Do not patch the provider, manufacture
lifecycle preconditions, restore process memory, or retry an uncertain Task to
make the test pass.
