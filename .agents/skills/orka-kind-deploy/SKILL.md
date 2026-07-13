---
name: orka-kind-deploy
description: Rebuild and redeploy the Orka controller and all worker images into a local kind cluster from an Orka repository checkout. Use when the user asks to rebuild Orka, redeploy all components, refresh the local cluster after code changes, or roll out the full local stack for verification.
---

# Orka Kind Deploy

Use the bundled script for the standard local deploy path instead of retyping
Makefile, image-load, and rollout commands.

## Workflow

1. Use `$kindctl` to create or select the repo/worktree-scoped cluster. The
   deploy script requires an existing cluster and never creates one.
2. Run the script from the Orka checkout:

   ```sh
   .agents/skills/orka-kind-deploy/scripts/deploy_orka_kind.sh
   ```

   With no `--kubeconfig` option, the script asks the repo-vendored `kindctl`
   for this worktree's isolated kubeconfig. Ambient `KUBECONFIG` is ignored.
   For an
   explicitly selected existing kind cluster, pass one isolated kubeconfig:

   ```sh
   .agents/skills/orka-kind-deploy/scripts/deploy_orka_kind.sh \
     --repo /path/to/orka \
     --kubeconfig /path/to/scoped.kubeconfig \
     --cluster exact-kind-name
   ```

3. Let the script:
   - verify the selected context maps to `kind-<cluster>`;
   - export a fresh kubeconfig from the exact local kind cluster and compare
     the API server plus CA identity with the selected scoped context;
   - run the full build, load, render, apply, restart, and rollout sequence
     inside the hardened E2E helper's `--reuse-only --cleanup` critical section,
     so the shared cluster lock remains held and temporary state is removed on
     success or failure;
   - build and load the controller, AI worker, general worker, and harness
     wrapper images while holding a daemon-wide Orka image lock, with kind
     identity revalidation immediately before every name-based load;
   - generate CRDs/RBAC into a temporary copy of `config/`, set controller and
     harness images there, and render the configured AI/general worker image
     arguments without changing repository files;
   - apply through the freshly exported kubeconfig only;
   - restart and wait for both `deployment/orka-controller-manager` and
     `deployment/orka-agent-harness-wrapper` in `orka-system`;
   - summarize pods, services, and deployments.

Image values can be set with `IMG`, `AI_WORKER_IMG`, `GENERAL_WORKER_IMG`, and
`HARNESS_WRAPPER_IMG`, or with the matching command-line options shown by
`--help`.

## Guardrails

- Never use or merge `~/.kube/config`. The script rejects the file by path or
  inode identity, ignores ambient `KUBECONFIG`, rejects CA-file references back
  to the global config, and requires a single-context/single-cluster scoped file
  or `kindctl path`.
- A context/cluster name match is insufficient: a server/CA identity mismatch
  aborts before builds, loads, or applies.
- The cluster must already exist by exact name. The deploy path will not create
  or delete it.
- The E2E helper uses a stable login-home lifecycle lease, while deploy state
  lives in a secured out-of-worktree runtime directory. Cleanup is mandatory;
  if an owned lock survives—or the outer deploy is interrupted—the runtime
  ownership metadata is preserved and reported for recovery.
- Image build/load is serialized across worktrees through
  `~/.cache/orka/kind-deploy-image-locks`. A stale image lock is reported and
  must be inspected before removal.
- If rollout fails, inspect through kindctl, for example:

  ```sh
  .agents/skills/kindctl/bin/kindctl kubectl -n orka-system get pods
  .agents/skills/kindctl/bin/kindctl kubectl -n orka-system describe deployment/orka-controller-manager
  .agents/skills/kindctl/bin/kindctl kubectl -n orka-system describe deployment/orka-agent-harness-wrapper
  ```

## Tests

Run the deterministic mocked regression suite without a real cluster:

```sh
.agents/skills/orka-kind-deploy/scripts/deploy_orka_kind_test.sh
```
