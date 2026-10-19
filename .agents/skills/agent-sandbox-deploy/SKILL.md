---
name: agent-sandbox-deploy
description: Stand up the upstream kubernetes-sigs agent-sandbox backend and the separately deployed orka-workspace Sandbox provider on a local kind cluster, then validate class-backed ACP workspace Tasks and the external proof harnesses. Use when the user asks to install, enable, deploy, configure, validate, demo, or troubleshoot agent-sandbox execution workspaces for Orka (Task.spec.execution.workspace.classRef backed by the Sandbox provider).
---

# Agent Sandbox Deploy

Stand up the experimental [`kubernetes-sigs/agent-sandbox`](https://github.com/kubernetes-sigs/agent-sandbox)
backend and the external `orka-workspace-sandbox` provider against an Orka
controller on a local kind cluster. Orka core has no Agent Sandbox flag or
in-tree provider. The provider, its CRDs, and its proof harnesses live in the
[`orka-workspace`](https://github.com/orka-agents/orka-workspace) repository at
the revision pinned in this repository's `go.mod`.

This skill is for **local/kind evaluation and validation**, not production.
Orka does not install or manage upstream agent-sandbox CRDs, controllers,
templates, or warm pools. See `website/docs/concepts/agent-sandbox.md` for the
design and `website/docs/reference/configuration.md#workspace-providers` for the
controller flags.

## What this skill orchestrates (do not retype)

- **Backend:** `hack/demos/cluster/install-agent-sandbox.sh` installs the
  pinned agent-sandbox release (`ORKA_AGENT_SANDBOX_VERSION`, default `v1.0.3`)
  and the `orka-live-template` SandboxTemplate. With `AGENTIC=1` (default) it
  also builds the archived demo runtime and router images and prepares vekil,
  the model Secret, the Git Secret, and the API client ServiceAccount. It does
  not change Orka controller flags and does not install the provider or a class.
- **Provider:** the orka-workspace shared bundle (`config/`) and Sandbox bundle
  (`providers/sandbox/config`), installed as described in its
  `docs/external-providers.md` and `providers/sandbox/README.md`.
- **Proofs:** orka-workspace `scripts/external-sandbox-e2e.sh` (installed
  backend persistence) and `scripts/external-workspace-e2e.sh core` (real Core
  RuntimeSession through an external provider). Drive these in place.

Existing v0.5 installations must complete the upstream storage migration before
applying v1. The installer does not run or preflight that migration. After a
v1 install it removes only the four obsolete namespaced conversion-webhook
resources documented by upstream.

## Ordering

1. **Cluster** — via `$kindctl` (see below) or an existing kind cluster.
2. **Orka controller** — via `$orka-kind-deploy`, with the generic workspace
   path enabled: `--enable-workspace-provider-api`,
   `--acp-workspace-dispatch-enabled`, `--workspace-class-use-admission-enabled=true`,
   and Task provenance admission (`--task-provenance-admission-enabled=true` or
   `--task-provenance-admission-external=true`). Without dispatch, class-backed
   workspace Tasks fail closed.
3. **agent-sandbox backend** — this skill's installer.
4. **Shared workspace bundle and Sandbox provider** — from orka-workspace.
5. **Profile and class** — `SandboxProviderConfig`, `SandboxWorkspaceProfile`,
   and an `ExecutionWorkspaceClass` requiring `acp.runtime.v2`.
6. **Model proxy** (model-backed smoke only) — reuse or deploy vekil; a human
   completes device-code login when required.

## Standard workflow (kindctl-scoped)

Use `$kindctl` so the kubeconfig stays scoped to this repo/worktree and never
touches `~/.kube/config`. Resolve the binary first:

```bash
kindctl="${KINDCTL_BIN:-$(command -v kindctl || true)}"
if [ -z "$kindctl" ] && [ -x .agents/skills/kindctl/bin/kindctl ]; then
  kindctl=.agents/skills/kindctl/bin/kindctl
fi
test -x "$kindctl"
```

1. **Create the repo-scoped cluster.**

   > **Registry precondition for `AGENTIC=1`: do this before cluster create.** The
   > agentic layer `docker push`es to `localhost:${KIND_REGISTRY_PORT}` (default
   > `5001`) and expects the kind node to pull from it as a containerd mirror. A
   > default kindctl cluster has **no** such image registry. Either commit a repo
   > `.kind/cluster.yaml` + `.kind/setup.sh` that wires a `localhost:5001`
   > registry before `"$kindctl" create`, or run the installer with `AGENTIC=0`.

   ```bash
   "$kindctl" create
   "$kindctl" kubectl get nodes
   ```

2. **Deploy the Orka controller** with `$orka-kind-deploy`, passing the kindctl
   context explicitly and enabling the workspace flags listed under Ordering:

   ```bash
   orka_kind_deploy="${ORKA_KIND_DEPLOY_BIN:-.agents/skills/orka-kind-deploy/scripts/deploy_orka_kind.sh}"
   test -x "$orka_kind_deploy"
   "$kindctl" exec -- "$orka_kind_deploy" \
     --context "$("$kindctl" kubectl config current-context)"
   ```

   Confirm the controller arguments include the workspace provider API, ACP
   dispatch, class-use admission, and provenance admission before continuing.

3. **Install the agent-sandbox backend** against the kindctl kubeconfig:

   ```bash
   eval "$("$kindctl" env)"   # exports scoped KUBECONFIG
   kube="$("$kindctl" path)"
   ORKA_DEMO_CLUSTER="$(basename "$kube" .kubeconfig)" \
   AGENTIC=0 \
     bash hack/demos/cluster/install-agent-sandbox.sh
   ```

   The script selects `kind-${ORKA_DEMO_CLUSTER}` when `kind get clusters` lists
   it and otherwise uses the current context, which the exported `KUBECONFIG`
   makes the kindctl cluster. Verify the selected context in its logs.

4. **Install the shared bundle and the Sandbox provider** from an orka-workspace
   checkout at the revision this repository pins:

   ```bash
   workspace_rev="$(go list -m -f '{{.Version}}' github.com/orka-agents/orka-workspace)"
   workspace_src=/path/to/orka-workspace   # checked out at ${workspace_rev}
   eval "$("$kindctl" env)"
   (cd "$workspace_src" && kubectl apply --server-side -k config)
   ```

   Bind the core-admission and operator roles from `config/rbac/` as the
   orka-workspace installation guide describes. Build
   `providers/sandbox/Dockerfile`, publish it to a registry the kind node can
   pull from, replace the example Deployment image with the digest, then apply
   `providers/sandbox/config` with `kubectl apply --server-side -k`. Wait for
   the provider registration to report its supported contracts.

5. **Create the profile and class.** Follow `providers/sandbox/README.md` and
   `website/docs/concepts/agent-sandbox.md`: a `SandboxProviderConfig`, a
   same-namespace `SandboxWorkspaceProfile`, and an `ExecutionWorkspaceClass`
   that references the registration and profile and requires `acp.runtime.v2`.
   Allow `Suspend` only when the profile enables it.

6. **Optional: model proxy (vekil) — pause for the human.** Skip this for
   model-free validation. For a model-backed smoke, satisfy the registry
   precondition, then rerun the installer with `AGENTIC=1`:

   ```bash
   eval "$("$kindctl" env)"
   kube="$("$kindctl" path)"
   ORKA_DEMO_CLUSTER="$(basename "$kube" .kubeconfig)" \
   AGENTIC=1 \
     bash hack/demos/cluster/install-agent-sandbox.sh
   ```

   When no vekil deployment exists and the deploy helper is available, the
   installer calls it with `--skip-wait`, which can start a GitHub device-code
   login. Verify `deployment/vekil` exists before continuing. If login is
   required, **surface the login URL and code to the user and wait for their
   confirmation; never complete the login on their behalf.** This mirrors the
   `$vekil-reverse-proxy-deploy` guardrail.

   > **Login race (verified live 2026-06): disarm vekil's liveness probe before
   > surfacing the code.** vekil binds its port only after login, so its
   > `livenessProbe` restarts the pod about every 60s and each restart mints a
   > new device code. Remove the probe and collapse to one pod first:
   >
   > ```bash
   > "$kindctl" kubectl -n vekil-system get deploy vekil >/dev/null
   > if "$kindctl" kubectl -n vekil-system get deploy vekil \
   >   -o jsonpath='{.spec.template.spec.containers[0].livenessProbe.httpGet.path}' | grep -q .; then
   >   "$kindctl" kubectl -n vekil-system patch deploy vekil \
   >     --type=json -p '[{"op":"remove","path":"/spec/template/spec/containers/0/livenessProbe"}]'
   > fi
   > "$kindctl" kubectl -n vekil-system scale deploy/vekil --replicas=0
   > for _ in $(seq 1 60); do
   >   [ -z "$("$kindctl" kubectl -n vekil-system get pod -l app.kubernetes.io/name=vekil,app.kubernetes.io/instance=vekil -o name 2>/dev/null)" ] && break
   >   sleep 2
   > done
   > "$kindctl" kubectl -n vekil-system scale deploy/vekil --replicas=1
   > "$kindctl" kubectl -n vekil-system logs deploy/vekil | grep 'login/device'
   > ```
   >
   > Device codes expire in about 15 minutes; bounce the pod for a fresh code
   > rather than waiting.

   Then wait for readiness before any model-backed Task:

   ```bash
   "$kindctl" kubectl -n vekil-system exec deploy/vekil -- \
     wget -qO- http://127.0.0.1:1337/readyz
   ```

## Validate

Read `references/validate.md` before treating anything as proven. A Task
selects only a class; provider, template, pool, and native selectors never
belong in a Task:

```yaml
spec:
  type: agent
  execution:
    workspace:
      classRef:
        name: sandbox-coding
      reusePolicy: none
```

With dispatch enabled and the class admitted, the Task binds a dedicated
single-session `acp-ws-*` RuntimePool whose Sandbox hosts the supervisor. Task
status stays provider-neutral; read the ExecutionWorkspace, RuntimePool, and
upstream agent-sandbox resources for lifecycle detail.

## Guardrails

- **Local/kind eval only.** Orka does not own upstream agent-sandbox lifecycle
  in production.
- **Reference, don't fork.** Drive the installer and the orka-workspace bundles
  and proofs in place; override pins via env (`ORKA_AGENT_SANDBOX_VERSION`,
  `AGENTIC`, `KIND_REGISTRY_PORT`, `ORKA_SANDBOX_RUNTIME_IMAGE`).
- **Pinned provider revision.** Install orka-workspace from the revision in
  `go.mod`; a different revision can change the shared schema.
- **Human-in-the-loop vekil login.** Surface the device-code URL + code and wait
  for confirmation. Never complete the GitHub login yourself.
- **No secrets in logs or status.** Never print provider credentials or paste
  tokens into prompts.
- **kindctl invariant.** Never run bare `kubectl`/`kind` against a kindctl
  cluster, and never read/write/switch `~/.kube/config`. Use
  `kindctl kubectl` / `kindctl exec`, or export `KUBECONFIG` via `kindctl env`
  for child scripts.

## Troubleshooting

Read `references/troubleshooting.md` when a step fails.
