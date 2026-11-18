---
name: orka-kind-deploy
description: Rebuild and load all Orka images into a named local kind cluster, publish them through its local registry, and redeploy with digest-pinned references and an explicit context from an Orka repository checkout. Use when the user asks to rebuild Orka, redeploy all components, refresh the local cluster after code changes, or roll out the full local stack for verification.
---

# Orka Kind Deploy

Use the bundled script for the standard local deploy path instead of retyping the Makefile sequence.

## Workflow

1. Ensure the working directory is an Orka repository checkout with `Makefile`, `config/manager/kustomization.yaml`, and the `workers/` tree.
2. Pass `--cluster` or `--context` explicitly; when `--context` is omitted, the script uses `kind-<cluster>`. It never relies on current-context. For kindctl-managed clusters, run through `kindctl exec` or set `KUBECONFIG` to the path returned by `kindctl path`.
3. Run the script (paths below are relative to this skill directory):
   - Named cluster: `scripts/deploy_orka_kind.sh --cluster codex`
   - Explicit repo or cluster: `scripts/deploy_orka_kind.sh --repo /path/to/repo --cluster codex`
4. Let the script:
   - validate the existing admission TLS Secret type, keypair, and certificate chain before starting the registry or building images
   - build `controller:kind` plus all worker images
   - load them into the target kind cluster with `make test-e2e-setup-only`
   - use `scripts/lib/kind-local-registry.sh` from the repository to start the cluster-local registry and push the controller, publisher, AI/general workers, and four ACP runtimes, resolving their immutable digests just like CI
   - install CRDs, bootstrap seven-day test-only admission TLS if absent, ensure the `vekil-system` namespace exists for the bundled ingress policy (without installing Vekil), and run `make deploy` with all six gated image variables digest-pinned (plus the AI/general worker references)
   - wait for the controller, publisher, provider-auth proxy, SCM egress proxy, and admission Deployments in `orka-system`; current main does not deploy the legacy harness wrapper
   - restore `config/manager/kustomization.yaml` after the deploy so the worktree stays clean
5. Summarize the resulting `pods`, `services`, and `deployments` in `orka-system`.

## Guardrails

- Use this skill for local kind-based Orka deployments. For remote or shared clusters, inspect the intended registry and tag flow before deploying.
- If the explicit context does not target the named kind cluster, stop and explain the mismatch instead of guessing.
- Expired or invalid admission TLS stops the script before image builds. Use a fresh kindctl cluster for a disposable environment. For retained clusters, coordinate renewal under `config/orka-admission-webhooks/README.md` in the repository. Automatic renewal is outside this skill's scope because deleting the shared webhooks requires first disabling admission trust on every affected controller and completing their rollouts.
- If rollout fails, inspect `kubectl --context <context> -n orka-system describe deployment/orka-controller-manager`, `kubectl --context <context> -n orka-system get pods`, and `kubectl --context <context> -n orka-system logs deployment/orka-controller-manager`, using the same scoped `KUBECONFIG`.
