---
name: agent-sandbox-deploy
description: Stand up the upstream kubernetes-sigs agent-sandbox workspace provider on a local kind cluster, validate the currently supported install/config and plain-agent paths, and treat workspace-backed Orka agent Tasks as expected-failure/future API checks until harness support lands. Use when the user asks to install, enable, deploy, configure, validate, demo, or troubleshoot agent-sandbox execution workspaces for Orka (Task.spec.execution.workspace with provider agent-sandbox).
---

# Agent Sandbox Deploy

Stand up the experimental [`kubernetes-sigs/agent-sandbox`](https://github.com/kubernetes-sigs/agent-sandbox)
workspace provider against an Orka controller on a local kind cluster. Today,
validate install/config and the plain harness-backed agent path; treat a
`type: agent` Task with `spec.execution.workspace` as an expected-failure check
for the documented harness gate until workspace-backed agent Tasks are wired.

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

Use a resolved `kindctl` binary for the snippets below. Prefer `KINDCTL_BIN` or
`PATH`; fall back to the repo-local skill checkout when it exists:

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
   > default kindctl cluster has **no** such image registry (kindctl's own
   > "registry" is a JSON cluster-metadata store, not a Docker registry). Either:
   > - commit a repo `.kind/cluster.yaml` + `.kind/setup.sh` that stands up a
   >   `localhost:5001` registry and wires the containerd mirror **before** running
   >   `"$kindctl" create`; if the cluster already exists, delete/recreate it after
   >   adding those files, **or**
   > - run with `AGENTIC=0` for a base-layer-only install (no router/model run).
   > `AGENTIC=1` always builds and `docker push`es the runtime and router images;
   > `ORKA_SANDBOX_RUNTIME_IMAGE` only changes the runtime image tag/reference and
   > there is no router-image override in the installer. Confirm the registry path
   > with the user before assuming `AGENTIC=1` works on a bare kindctl cluster.

   ```bash
   "$kindctl" create
   "$kindctl" kubectl get nodes
   ```

2. **Deploy the Orka controller** into the cluster with `$orka-kind-deploy`
   (build + load controller and worker images, install CRDs, roll out
   `orka-controller-manager`). Run the deploy under the kindctl-scoped kubeconfig
   so its `kubectl` discovery sees the repo-scoped cluster:

   ```bash
   orka_kind_deploy="${ORKA_KIND_DEPLOY_BIN:-.agents/skills/orka-kind-deploy/scripts/deploy_orka_kind.sh}"
   test -x "$orka_kind_deploy"
   eval "$("$kindctl" env)"
   "$orka_kind_deploy"
   ```

   The harness wrapper image must be present too — sandbox runs re-exec the
   agent CLI inside the sandbox.

3. **Install agent-sandbox** by driving the canonical script against the kindctl
   kubeconfig. Export `KUBECONFIG` from kindctl so the script's `kubectl` calls
   hit the right cluster, and pass the kindctl cluster name through the env vars
   the script reads:

   ```bash
   eval "$("$kindctl" env)"   # exports scoped KUBECONFIG
   kube="$("$kindctl" path)"   # ~/.kube/kind/<name>.kubeconfig
   ORKA_DEMO_CLUSTER="$(basename "$kube" .kubeconfig)" \
   AGENTIC=0 \
     bash hack/demos/cluster/install-agent-sandbox.sh
   ```

   The script selects its context by checking `kind get clusters` for
   `ORKA_DEMO_CLUSTER`; when that name is not a plain `kind get clusters` entry
   it falls back to the current context, which the exported `KUBECONFIG` makes
   the kindctl cluster. Verify the selected context in the script's logs before
   continuing. Use `AGENTIC=1` only once the registry precondition above is met.

4. **Optional: add the agentic/model layer (vekil) — pause for the human.**
   Skip this step for base-layer-only or model-free validation. For a
   model-backed smoke, first satisfy the registry/image precondition above, then
   rerun the installer with `AGENTIC=1` so it deploys the sandbox runtime/router,
   vekil, model/git Secrets, and the API client ServiceAccount:

   ```bash
   eval "$("$kindctl" env)"
   kube="$("$kindctl" path)"
   agent_sandbox_version="${ORKA_AGENT_SANDBOX_VERSION:-v0.4.6}"
   go mod download "sigs.k8s.io/agent-sandbox@${agent_sandbox_version}"
   test -d "$(go env GOMODCACHE)/sigs.k8s.io/agent-sandbox@${agent_sandbox_version}/clients/python/agentic-sandbox-client/sandbox-router"
   ORKA_DEMO_CLUSTER="$(basename "$kube" .kubeconfig)" \
   AGENTIC=1 \
     bash hack/demos/cluster/install-agent-sandbox.sh
   ```

   The agentic layer calls the vekil deploy script with `--skip-wait`, which
   starts a GitHub device-code login. **Surface the login URL and code to the
   user and wait for their confirmation; never complete the login on their
   behalf.** This mirrors the `$vekil-reverse-proxy-deploy` guardrail. First
   disarm the liveness race below; then read and surface the device-code prompt.

   > **Login race (verified live 2026-06): disarm vekil's liveness probe before
   > surfacing the code.** vekil binds its port only after the Copilot login
   > completes, so its `livenessProbe` on `/healthz` fails and restarts the pod
   > every ~60s — and **each restart mints a NEW device code**, so a human login
   > against the old code can never land. Remove the probe and collapse to one
   > pod before handing the user a code:
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
   > test -z "$("$kindctl" kubectl -n vekil-system get pod -l app.kubernetes.io/name=vekil,app.kubernetes.io/instance=vekil -o name 2>/dev/null)"
   > "$kindctl" kubectl -n vekil-system scale deploy/vekil --replicas=1
   > for _ in $(seq 1 60); do
   >   [ "$("$kindctl" kubectl -n vekil-system get pod -l app.kubernetes.io/name=vekil,app.kubernetes.io/instance=vekil --no-headers 2>/dev/null | wc -l | tr -d ' ')" = "1" ] && break
   >   sleep 2
   > done
   > test "$("$kindctl" kubectl -n vekil-system get pod -l app.kubernetes.io/name=vekil,app.kubernetes.io/instance=vekil --no-headers 2>/dev/null | wc -l | tr -d ' ')" = "1"
   > ```
   >
   > Then read the code from the single fresh pod:
   >
   > ```bash
   > "$kindctl" kubectl -n vekil-system logs deploy/vekil | grep 'login/device'
   > ```
   >
   > GitHub device codes expire in
   > ~15 min; surface promptly, and if it expires, bounce the pod
   > (`"$kindctl" kubectl -n vekil-system delete pod -l app.kubernetes.io/name=vekil,app.kubernetes.io/instance=vekil`) for a fresh code
   > rather than waiting.

   Then wait for readiness before any model-backed Task:

   ```bash
   "$kindctl" kubectl -n vekil-system exec deploy/vekil -- \
     wget -qO- http://127.0.0.1:1337/readyz
   ```

   If you only need model-free confidence, run the CI parity script below. It
   validates installation/configuration only while workspace-backed agent Tasks
   remain gated; it is not a claim/readiness/exec/cleanup smoke.

## Validate

> **Known gate (verified live 2026-06): agent Tasks with an execution workspace
> are rejected by the current service-backed harness runtime.** Any Task that
> sets `spec.execution.workspace` fails immediately with
> `status.executionWorkspace.reason=WorkspaceValidationFailed` and message
> `execution workspace is not supported by harness runtime yet` — an
> unconditional gate in `internal/controller/harness_wrapper.go`
> (`runHarnessWrapperTask`), not a misconfiguration. The agent CLI runtimes now
> run through the long-lived `agent-harness-wrapper` service, and the
> Task→sandbox-workspace path for agents is not wired through it yet. A **plain**
> agent Task (no `execution.workspace`) runs fine through the harness + model
> proxy, so use that to confirm the model path. The model-free e2e currently
> confirms installation/configuration only; it deliberately skips the
> execution-workspace Task smoke while the harness gate is present. Treat only
> the execution-workspace YAML in the optional expected-failure check as the
> intended future API once the harness wires workspaces.

Do **not** use an execution-workspace agent Task as the success criterion yet.
Validate the two currently wired paths separately:

- **Model path through the harness** (requires the optional `AGENTIC=1` step and
  vekil ready): run a plain agent Task with no `execution.workspace` and wait
  for it to succeed.

```bash
"$kindctl" kubectl -n demo-magic apply -f - <<'YAML'
apiVersion: core.orka.ai/v1alpha1
kind: Agent
metadata:
  name: sandbox-codex-agent
  namespace: demo-magic
spec:
  runtime:
    type: codex
    defaultMaxTurns: 1
    defaultAllowBash: true
  model:
    name: gpt-5.5
  secretRef:
    name: sandbox-model-key
---
apiVersion: core.orka.ai/v1alpha1
kind: Task
metadata:
  name: orka-live-model-smoke
  namespace: demo-magic
spec:
  type: agent
  agentRef:
    name: sandbox-codex-agent
  agentRuntime:
    maxTurns: 1
  timeout: 10m0s
  prompt: "Reply exactly: ORKA_LIVE_MODEL_OK"
YAML

"$kindctl" kubectl -n demo-magic \
  wait --for=jsonpath='{.status.phase}'=Succeeded task/orka-live-model-smoke --timeout=10m
```

- **Installation/configuration parity**: run the model-free CI parity script
  below when you want a self-contained cluster bring-up with fake model
  credentials. It verifies the install/config path, but it does **not** exercise
  claim → ready → exec → cleanup while the harness gate is present.

If you need to demonstrate the intended API shape before harness workspace
support lands, run it only as an **expected-failure** check and wait for the gate
instead of `Succeeded`:

```bash
"$kindctl" kubectl apply -f - <<'YAML'
apiVersion: core.orka.ai/v1alpha1
kind: Agent
metadata:
  name: sandbox-codex-agent
  namespace: demo-magic
spec:
  runtime:
    type: codex
    defaultMaxTurns: 1
    defaultAllowBash: true
  model:
    name: gpt-5.5
  secretRef:
    name: sandbox-model-key
---
apiVersion: core.orka.ai/v1alpha1
kind: Task
metadata:
  name: orka-live-sandbox-smoke
  namespace: demo-magic
spec:
  type: agent
  agentRef:
    name: sandbox-codex-agent
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

"$kindctl" kubectl -n demo-magic \
  wait --for=jsonpath='{.status.executionWorkspace.reason}'=WorkspaceValidationFailed \
  task/orka-live-sandbox-smoke --timeout=2m
```

Once the harness wires agent Tasks to execution workspaces, the expected-failure
check can become the live success smoke. At that point, a successful sandbox
wrapper log should include the claimed workspace name, e.g. `completed in
sandbox workspace sandbox-claim-...`. Orka Task status does **not** expose
sandbox claim/exec/cleanup state — read worker logs and upstream agent-sandbox
resources for lifecycle detail.

### Model-free CI parity

`scripts/live-agent-sandbox-e2e.sh` (run by the `Live Agent Sandbox E2E`
workflow) stands up a clean kind cluster with fake model credentials and **no
model access**. Because the current harness-wrapper runtime is service-backed,
the script logs `Skipping agent-sandbox Task smoke...` and does not create a
SandboxClaim or exercise the router exec data path. Use it for installation and
controller-flag CI parity, not as proof of workspace execution:

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
