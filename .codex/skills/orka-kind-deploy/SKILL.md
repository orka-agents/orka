---
name: orka-kind-deploy
description: Rebuild and redeploy the Orka controller and all worker images into a local kind cluster from an Orka repository checkout. Use when the user asks to rebuild Orka, redeploy all components, refresh the local cluster after code changes, or roll out the full local stack for verification.
---

# Orka Kind Deploy

Use the bundled script for the standard local deploy path instead of retyping the Makefile sequence.

## Workflow

1. Ensure the working directory is an Orka repository checkout with `Makefile`, `config/manager/kustomization.yaml`, and the `workers/` tree.
2. Prefer the active `kubectl` context. If it targets a kind cluster, derive the cluster name from it. If not, pass `--cluster` explicitly; when `--context` is omitted, the script uses `kind-<cluster>`.
3. Run the script:
   - Default: `scripts/deploy_orka_kind.sh`
   - Explicit repo or cluster: `scripts/deploy_orka_kind.sh --repo /path/to/repo --cluster codex`
4. Let the script:
   - build `controller:kind` plus all worker images
   - load them into the target kind cluster with `make test-e2e-setup-only`
   - install CRDs and deploy the controller with `make install deploy IMG=controller:kind`
   - wait for `deployment/orka-controller-manager` in `orka-system`
   - restore `config/manager/kustomization.yaml` after the deploy so the worktree stays clean
5. Summarize the resulting `pods`, `services`, and `deployments` in `orka-system`.

## Guardrails

- Use this skill for local kind-based Orka deployments. For remote or shared clusters, inspect the intended registry and tag flow before deploying.
- If the active context is not a kind context and no explicit cluster is provided, stop and explain the mismatch instead of guessing.
- If rollout fails, inspect `kubectl -n orka-system describe deployment/orka-controller-manager`, `kubectl -n orka-system get pods`, and `kubectl -n orka-system logs deployment/orka-controller-manager`.
