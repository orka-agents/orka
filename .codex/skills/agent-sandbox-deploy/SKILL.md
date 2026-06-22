---
name: agent-sandbox-deploy
description: Stand up the upstream kubernetes-sigs agent-sandbox workspace provider on a local kind cluster and run an Orka agent Task through a sandbox-backed workspace. Use when the user asks to install, enable, deploy, configure, validate, demo, or troubleshoot agent-sandbox execution workspaces for Orka (Task.spec.execution.workspace with provider agent-sandbox).
---

# Agent Sandbox Deploy

Stand up the experimental [`kubernetes-sigs/agent-sandbox`](https://github.com/kubernetes-sigs/agent-sandbox)
workspace provider against an Orka controller on a local kind cluster, then
validate it by running a `type: agent` Task whose `spec.execution.workspace`
routes to an agent-sandbox workspace.

This skill is for **local/kind evaluation and validation**, not production.
Orka does not install or manage upstream agent-sandbox CRDs, the router,
templates, or warm pools in production — install and operate those separately.
See `website/docs/concepts/agent-sandbox.md` for the full design and the
production controller flags.

## What this skill orchestrates (do not retype)

The repeatable standup already lives in
`hack/demos/cluster/install-agent-sandbox.sh`. **Drive that script in place; do
not copy it into the skill.** It pins the agent-sandbox version
(`ORKA_AGENT_SANDBOX_VERSION`, default `v0.4.6`) and owns the gotchas (kind
registry addressing, the SDK sandbox-router build from the Go module cache, the
controller flag patch). Re-pin by overriding the env var, not by editing a copy.

What the script does:

1. **Base layer (always):** installs agent-sandbox CRDs + controllers, applies
   the `orka-live-template` SandboxTemplate, and patches the existing Orka
   controller Deployment with `--agent-sandbox-enabled=true`,
   `--agent-sandbox-router-url`, `--agent-sandbox-default-template`,
   `--agent-sandbox-cleanup-policy`, and
   `--execution-workspace-default-provider=agent-sandbox`.
2. **Agentic layer (`AGENTIC=1`, default):** builds + pushes the
   `sandbox-runtime` image (real `codex` + `git` + `gh`) and the upstream
   `sandbox-router` image to the local registry, deploys the router, deploys the
   vekil model proxy (device-code login), and creates the model + git Secrets and
   the Orka API client ServiceAccount.

## Ordering matters — the script assumes Orka is already deployed

`install-agent-sandbox.sh` **does not create the cluster and does not deploy
Orka.** It patches an existing `orka-controller-manager` Deployment in
`orka-system`. If the controller is not already running, the flag-patch step is
skipped and the feature never turns on. So the correct sequence is:

1. **Cluster** — via `$kindctl` (see below) or an existing kind cluster.
2. **Orka controller** — via `$orka-kind-deploy`.
3. **agent-sandbox** — this skill's script.
4. **Model proxy** — vekil, with a **human-in-the-loop device-code login**.

## Standard workflow (kindctl-scoped)

Use `$kindctl` so the kubeconfig stays scoped to this repo/worktree and never
touches `~/.kube/config`. Every command below runs against the kindctl
kubeconfig.

1. **Create the repo-scoped cluster.**

   ```bash
   .agents/skills/kindctl/bin/kindctl create
   .agents/skills/kindctl/bin/kindctl kubectl get nodes
   ```

   > **Registry precondition for `AGENTIC=1`.** The agentic layer
   > `docker push`es to `localhost:${KIND_REGISTRY_PORT}` (default `5001`) and
   > expects the kind node to pull from it as a containerd mirror. A default
   > kindctl cluster has **no** such image registry (kindctl's own "registry" is
   > a JSON cluster-metadata store, not a Docker registry). Either:
   > - commit a repo `.kind/cluster.yaml` + `.kind/setup.sh` that stands up a
   >   `localhost:5001` registry and wires the containerd mirror before running
   >   the agentic layer, **or**
   > - run with `AGENTIC=0` for a base-layer-only install (no model run), **or**
   > - point the script at pre-loaded images via `ORKA_SANDBOX_RUNTIME_IMAGE` /
   >   `ORKA_SANDBOX_ROUTER_IMAGE` that you `kindctl load` yourself.
   > Confirm the registry path with the user before assuming `AGENTIC=1` works on
   > a bare kindctl cluster.

2. **Deploy the Orka controller** into the cluster with `$orka-kind-deploy`
   (build + load controller and worker images, install CRDs, roll out
   `orka-controller-manager`). The harness wrapper image must be present too —
   sandbox runs re-exec the agent CLI inside the sandbox.

3. **Install agent-sandbox** by driving the canonical script against the kindctl
   kubeconfig. Export `KUBECONFIG` from kindctl so the script's `kubectl` calls
   hit the right cluster, and pass the kindctl cluster name through the env vars
   the script reads:

   ```bash
   eval "$(.agents/skills/kindctl/bin/kindctl env)"   # exports scoped KUBECONFIG
   kube="$(.agents/skills/kindctl/bin/kindctl path)"   # ~/.kube/kind/<name>.kubeconfig
   ORKA_DEMO_CLUSTER="$(basename "$kube" .kubeconfig)" \
   AGENTIC=0 \
     bash hack/demos/cluster/install-agent-sandbox.sh
   ```

   The script selects its context by checking `kind get clusters` for
   `ORKA_DEMO_CLUSTER`; when that name is not a plain `kind get clusters` entry
   it falls back to the current context, which the exported `KUBECONFIG` makes
   the kindctl cluster. Verify the selected context in the script's logs before
   continuing. Use `AGENTIC=1` only once the registry precondition above is met.

4. **Stand up the model proxy (vekil) — pause for the human.** The agentic layer
   calls the vekil deploy script with `--skip-wait`, which starts a GitHub
   device-code login. **Surface the login URL and code to the user and wait for
   their confirmation; never complete the login on their behalf.** This mirrors
   the `$vekil-reverse-proxy-deploy` guardrail.

   ```bash
   .agents/skills/kindctl/bin/kindctl kubectl -n vekil-system logs deploy/vekil | grep 'login/device'
   ```

   > **Login race (verified live 2026-06): disarm vekil's liveness probe before
   > surfacing the code.** vekil binds its port only after the Copilot login
   > completes, so its `livenessProbe` on `/healthz` fails and restarts the pod
   > every ~60s — and **each restart mints a NEW device code**, so a human login
   > against the old code can never land. Remove the probe and collapse to one
   > pod before handing the user a code:
   >
   > ```bash
   > .agents/skills/kindctl/bin/kindctl kubectl -n vekil-system patch deploy vekil \
   >   --type=json -p '[{"op":"remove","path":"/spec/template/spec/containers/0/livenessProbe"}]'
   > ```
   >
   > GitHub device codes expire in ~15 min; surface promptly, and if it expires,
   > bounce the pod (`kindctl kubectl -n vekil-system delete pod -l app=vekil`)
   > for a fresh code rather than waiting.

   Then wait for readiness before any model-backed Task:

   ```bash
   .agents/skills/kindctl/bin/kindctl kubectl -n vekil-system exec deploy/vekil -- \
     wget -qO- http://127.0.0.1:1337/readyz
   ```

   If you only need to validate Orka's sandbox plumbing (claim → ready →
   exec → cleanup) without a real model, prefer the model-free e2e in step 6
   instead of standing up vekil.

## Validate

> **Known gate (verified live 2026-06): agent Tasks with an execution workspace
> are rejected by the current service-backed harness runtime.** The smoke Task
> below fails immediately with
> `status.executionWorkspace.reason=WorkspaceValidationFailed` and message
> `execution workspace is not supported by harness runtime yet` — an
> unconditional gate in `internal/controller/harness_wrapper.go`
> (`runHarnessWrapperTask`), not a misconfiguration. The agent CLI runtimes now
> run through the long-lived `agent-harness-wrapper` service, and the
> Task→sandbox-workspace path for agents is not wired through it yet. A **plain**
> agent Task (no `execution.workspace`) runs fine through the harness + model
> proxy, so use that to confirm the model path; use the model-free e2e
> (`scripts/live-agent-sandbox-e2e.sh`) to confirm sandbox plumbing. Treat the
> Task YAML below as the intended API once the harness wires workspaces.

Run the live smoke Task from the concept doc against the kindctl cluster
(namespace, agent, and template match `install-agent-sandbox.sh`'s defaults):

```bash
.agents/skills/kindctl/bin/kindctl kubectl apply -f - <<'YAML'
apiVersion: core.orka.ai/v1alpha1
kind: Task
metadata:
  name: orka-live-sandbox-smoke
  namespace: orka-system
spec:
  type: agent
  agentRef:
    name: claude-agent
  agentRuntime:
    maxTurns: 1
  timeout: 10m0s
  execution:
    workspace:
      enabled: true
      templateRef:
        name: orka-live-template
      reusePolicy: none
      cleanupPolicy: delete
  prompt: "Reply exactly: ORKA_LIVE_SANDBOX_OK"
YAML

.agents/skills/kindctl/bin/kindctl kubectl -n orka-system \
  wait --for=jsonpath='{.status.phase}'=Succeeded task/orka-live-sandbox-smoke --timeout=10m
```

A successful sandbox wrapper log includes the claimed workspace name, e.g.
`completed in sandbox workspace sandbox-claim-...`. Orka Task status does **not**
expose sandbox claim/exec/cleanup state — read worker logs and upstream
agent-sandbox resources for lifecycle detail.

### Model-free CI parity

`scripts/live-agent-sandbox-e2e.sh` (run by the `Live Agent Sandbox E2E`
workflow) stands up the whole path with a deterministic fake `claude` CLI and
**no model access**. Use it to validate Orka's sandbox plumbing independently of
provider availability:

```bash
bash scripts/live-agent-sandbox-e2e.sh
```

That script owns its own cluster lifecycle; do not run it against a kindctl
cluster you want to keep.

## Guardrails

- **Local/kind eval only.** Do not present this as a production install. Orka
  does not own upstream agent-sandbox lifecycle in production.
- **Reference, don't fork.** Drive `hack/demos/cluster/install-agent-sandbox.sh`
  and override pins via env (`ORKA_AGENT_SANDBOX_VERSION`, `AGENTIC`,
  `KIND_REGISTRY_PORT`, `ORKA_SANDBOX_RUNTIME_IMAGE`). Copying the script into
  the skill invites version-pin drift.
- **Human-in-the-loop vekil login.** Surface the device-code URL + code and wait
  for confirmation. Never complete the GitHub login yourself.
- **No secrets in logs or status.** Provider credentials are forwarded as command
  env into the sandbox; never print them. Do not paste tokens into prompts.
- **kindctl invariant.** Never run bare `kubectl`/`kind` against a kindctl
  cluster, and never read/write/switch `~/.kube/config`. Use
  `kindctl kubectl` / `kindctl exec`, or export `KUBECONFIG` via `kindctl env`
  for child scripts.

## Troubleshooting

- **Task rejected before Job creation:** confirm the controller actually has
  `--agent-sandbox-enabled=true` (the flag patch is skipped if Orka was not
  deployed first); check `templateRef.name` / default template, `reusePolicy`,
  `cleanupPolicy`, and `sessionRef.name` for `reusePolicy: session`.
- **ImagePullBackOff on sandbox/router pods:** the `localhost:5001` registry is
  missing or not wired as a containerd mirror — see the registry precondition.
- **Inner agent CLI connection refused:** exec into a retained sandbox and verify
  DNS/TCP reachability to the model/proxy base URL from inside the sandbox pod.
- **Controller rollout fails after the flag patch:** preserve the controller's
  `/data` and `/tmp` volume mounts, probes, resources, and security context.
- Full troubleshooting matrix: `website/docs/concepts/agent-sandbox.md`.
