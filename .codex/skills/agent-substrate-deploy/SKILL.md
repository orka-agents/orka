---
name: agent-substrate-deploy
description: Stand up Agent Substrate (warm, suspendable, gVisor-isolated Actors) on a dedicated local kind cluster wired to Orka, validate the direct Substrate/MCP smoke paths, and treat workspace-backed Orka agent Tasks as expected-failure/future API checks until harness support lands. Use when the user asks to install, enable, deploy, configure, validate, demo, or troubleshoot the Orka substrate execution-workspace provider, WorkerPool, ActorTemplate, or Substrate-backed MCP Tools.
---

# Agent Substrate Deploy

Stand up [Agent Substrate](https://github.com/agent-substrate/substrate) and an
Orka-compatible `WorkerPool` + `ActorTemplate` on a local kind cluster, wire Orka
with `--substrate-*` flags, then validate the direct Substrate actor/router and
MCP tool paths. Today, `type: agent` Tasks whose `spec.execution.workspace` uses
`provider: substrate` are expected-failure/future API checks because the
service-backed harness rejects execution workspaces.

This skill is for **local/kind evaluation and validation**, not production. Orka
does not install or manage Substrate (CRDs, control plane, router, snapshot
store, WorkerPool capacity) in production. See
`website/docs/concepts/substrate.md` for the full design, controller flags, and
ActorTemplate compatibility contract.

## What this skill orchestrates (do not retype)

The repeatable standup already lives in
`hack/demos/cluster/install-substrate.sh`, a thin `KEEP_CLUSTER=1` wrapper over
the CI-proven `scripts/agent-substrate-e2e.sh`. **Drive the installer in place;
do not copy either script into the skill.** They pin the Substrate revision
(`SUBSTRATE_REF`, default `b80031d260959b1fc5c6f61e3099fe2a6d368af1`) and own the
heavy lifting: clone Substrate at the pinned ref, create the kind cluster + local
registry, deploy the `ate-system` control plane, build/push the controller +
codex-worker + workspace-agent images via `docker` + `ko`, create a `WorkerPool`
+ gVisor `ActorTemplate`, initialize the RustFS snapshot bucket, and deploy Orka
wired with `--substrate-*`. Re-pin by overriding `SUBSTRATE_REF`, not by editing
a copy.

The base standup is **secret-free**. The agentic layer (`AGENTIC=1`, default)
additionally builds a codex-capable Actor image, deploys the vekil model proxy
(device-code login), and creates the model + git Secrets.

## Why kindctl cannot host this cluster (the honest exception)

Unlike agent-sandbox, **Substrate must own its own kind cluster** and cannot
bolt onto a `$kindctl`-managed one. Substrate's `hack/create-kind-cluster.sh`
uses a custom local Docker registry plus gVisor/`runsc` node configuration, and
its standup does `kind delete` + recreate. A default kindctl cluster has neither
the registry mirror nor the gVisor runtime, and kindctl only operates on
clusters it created (`kindctl kubectl` rejects unowned clusters).

So for Substrate:

- **Do not** run `kindctl create` and try to install Substrate into it.
- **Do** let `install-substrate.sh` create and own the cluster (named by
  `KIND_CLUSTER`, default `orka-agent-substrate-e2e`; context
  `kind-<KIND_CLUSTER>`). This cluster is not kindctl-owned. After a fresh
  standup, export its kubeconfig into a scoped kubeconfig file before running
  plain `kubectl --context kind-<KIND_CLUSTER>` commands.

If the user explicitly wants kindctl-style isolation, the supported route is to
commit a repo `.kind/cluster.yaml` (gVisor node image + `containerd` registry
mirror) and `.kind/setup.sh` and adapt the Substrate install to target it — that
is a larger task; confirm scope before attempting it.

## Standard workflow

1. **Preflight tools.** The installer needs `kind`, `ko`, `docker`, `go`, `git`,
   `jq`, `kubectl`, `curl` (and `gh` for the git-token convenience). Put the Go
   bin dir on PATH so a `go install`ed `ko` is found:

   ```bash
   PATH="$(go env GOPATH)/bin:$PATH"
   command -v ko >/dev/null || go install github.com/google/ko@v0.18.1
   ```

2. **Stand up Substrate + Orka.** For a fresh cluster, start with the base
   standup (secret-free, no model run). It builds four images and the Substrate
   control plane — several minutes.

   ```bash
   AGENTIC=0 bash hack/demos/cluster/install-substrate.sh
   ```

   Re-running on an existing cluster prompts reuse/recreate/cancel. For
   non-interactive runs set `DEMO_CLUSTER_REUSE=reuse|recreate|cancel` — **note
   that `recreate` destroys the cluster and any completed vekil login.**

3. **Export kubeconfig for follow-up kubectl commands.** The e2e standup uses an
   isolated kubeconfig internally. Before reading vekil logs, patching probes,
   or applying Tasks from your shell, make the retained kind cluster visible in a
   throwaway kubeconfig and set the context variable used below:

   ```bash
   cluster="${KIND_CLUSTER:-orka-agent-substrate-e2e}"
   ctx="kind-${cluster}"
   export KUBECONFIG="$(mktemp -t orka-substrate-kubeconfig.XXXXXX)"
   kind export kubeconfig --name "${cluster}" --kubeconfig "${KUBECONFIG}"
   ```

4. **Add the model proxy (vekil) — pause for the human.** For model-backed
   validation, rerun the installer against the exported/reused cluster with the
   agentic layer enabled (the default). It builds the codex Actor image, deploys
   vekil with `--skip-wait`, and creates the model/git Secrets. vekil starts a
   GitHub device-code login: **surface the login URL and code to the user and
   wait for their confirmation; never complete the login on their behalf** (the
   `$vekil-reverse-proxy-deploy` guardrail). Install/readiness flow:

   ```bash
   cluster="${KIND_CLUSTER:-orka-agent-substrate-e2e}"
   ctx="kind-${cluster}"
   export KUBECONFIG="$(mktemp -t orka-substrate-kubeconfig.XXXXXX)"
   kind export kubeconfig --name "${cluster}" --kubeconfig "${KUBECONFIG}"
   DEMO_CLUSTER_REUSE=reuse bash hack/demos/cluster/install-substrate.sh

   # The base e2e creates codex-substrate-ci without model env and patches the
   # service-backed harness wrapper to use a fake Codex CLI. Patch the Agent with
   # the model Secret from the agentic layer and remove the fake CLI override
   # before using a plain agent Task as model-validation evidence.
   kubectl --context "$ctx" -n default patch agent codex-substrate-ci --type=merge \
     -p "$(jq -cn \
       --arg ref substrate-model-key \
       --arg model gpt-5.5 \
       '{spec:{model:{name:$model},secretRef:{name:$ref}}}')"
   kubectl --context "$ctx" -n orka-system set env deployment/orka-agent-harness-wrapper CODEX_CLI_PATH-
   kubectl --context "$ctx" -n orka-system rollout status deployment/orka-agent-harness-wrapper --timeout=5m
   ```

   > **Login race (verified live 2026-06): disarm vekil's liveness probe before
   > surfacing the code.** vekil does not bind its port until the Copilot login
   > completes, so its `livenessProbe` on `/healthz` fails and restarts the pod
   > every ~60s — and **each restart mints a NEW device code**, so a human login
   > against the old code can never land. The pod's `/readyz` readiness gate stays
   > un-ready until login, which is correct; the liveness probe is the problem.
   > Before handing the user a code, remove it and collapse to one pod:
   >
   > ```bash
   > kubectl --context "$ctx" -n vekil-system get deploy vekil >/dev/null
   > if kubectl --context "$ctx" -n vekil-system get deploy vekil \
   >   -o jsonpath='{.spec.template.spec.containers[0].livenessProbe.httpGet.path}' | grep -q .; then
   >   kubectl --context "$ctx" -n vekil-system patch deploy vekil --type=json \
   >     -p '[{"op":"remove","path":"/spec/template/spec/containers/0/livenessProbe"}]'
   > fi
   > kubectl --context "$ctx" -n vekil-system scale deploy/vekil --replicas=0
   > for _ in $(seq 1 60); do
   >   [ -z "$(kubectl --context "$ctx" -n vekil-system get pod -l app.kubernetes.io/name=vekil,app.kubernetes.io/instance=vekil -o name 2>/dev/null)" ] && break
   >   sleep 2
   > done
   > test -z "$(kubectl --context "$ctx" -n vekil-system get pod -l app.kubernetes.io/name=vekil,app.kubernetes.io/instance=vekil -o name 2>/dev/null)"
   > kubectl --context "$ctx" -n vekil-system scale deploy/vekil --replicas=1
   > for _ in $(seq 1 60); do
   >   [ "$(kubectl --context "$ctx" -n vekil-system get pod -l app.kubernetes.io/name=vekil,app.kubernetes.io/instance=vekil --no-headers 2>/dev/null | wc -l | tr -d ' ')" = "1" ] && break
   >   sleep 2
   > done
   > test "$(kubectl --context "$ctx" -n vekil-system get pod -l app.kubernetes.io/name=vekil,app.kubernetes.io/instance=vekil --no-headers 2>/dev/null | wc -l | tr -d ' ')" = "1"
   > ```
   >
   > Then read the code from the single fresh pod:
   >
   > ```bash
   > kubectl --context "$ctx" -n vekil-system logs deploy/vekil | grep 'login/device'
   > ```
   >
   > GitHub device codes expire in
   > ~15 min, so surface it promptly and, if it expires, bounce the pod
   > (`kubectl --context "$ctx" -n vekil-system delete pod -l app.kubernetes.io/name=vekil,app.kubernetes.io/instance=vekil`) for a
   > fresh code rather than waiting.

   After the human completes login, check readiness before model-backed Tasks:

   ```bash
   kubectl --context "$ctx" -n vekil-system exec deploy/vekil -- wget -qO- http://127.0.0.1:1337/readyz
   ```

   For a model-free validation, stay on `AGENTIC=0` and rely on the built-in
   smoke exercises (next section) instead of standing up vekil.

## Validate

> **Known gate (verified live 2026-06): agent Tasks with an execution workspace
> are rejected by the current service-backed harness runtime.** A
> `provider: substrate` (or `agent-sandbox`) agent Task fails immediately with
> `status.executionWorkspace.reason=WorkspaceValidationFailed` and message
> `execution workspace is not supported by harness runtime yet`. This is an
> unconditional gate in `internal/controller/harness_wrapper.go`
> (`runHarnessWrapperTask`), not a misconfiguration — the agent CLI runtimes now
> run through the long-lived `agent-harness-wrapper` service, and the
> Task→workspace path for agents is not wired through it yet. The bundled e2e
> reflects this: it prints `Skipping agent Task execution-workspace checks:
> harness-wrapper runtime is service-backed`. What IS validated end-to-end today
> is the **direct** Substrate path (actor create/resume/router/daemon exec/
> suspend/delete) plus Substrate-backed MCP tool create/reconcile/cleanup,
> exercised during standup. After clearing the e2e's fake `CODEX_CLI_PATH`
> override as shown above, a **plain** agent Task (no `execution.workspace`)
> runs through the harness + model proxy. Treat the
> Task YAML below as the intended API once the harness wires workspaces; until
> then, validate via the e2e's direct-actor exercises.

The installer leaves a fully wired cluster. During standup it smoke-tests direct
actor create/resume/exec/suspend/delete and Substrate-backed MCP tool lifecycle.
It does **not** currently smoke-test retained workspace reuse for Orka agent
Tasks because those execution-workspace checks are skipped by the harness gate.

If you skipped the kubeconfig export in the workflow above, do it before any
manual `kubectl` commands — the e2e standup uses an isolated kubeconfig and does
**not** leave `kind-<KIND_CLUSTER>` in your default one. Keep using the scoped
`KUBECONFIG` in that shell:

```bash
cluster="${KIND_CLUSTER:-orka-agent-substrate-e2e}"
ctx="kind-${cluster}"
export KUBECONFIG="$(mktemp -t orka-substrate-kubeconfig.XXXXXX)"
kind export kubeconfig --name "${cluster}" --kubeconfig "${KUBECONFIG}"
```

To drive an Orka Task yourself (intended shape; currently gated as noted above):

```bash
cluster="${KIND_CLUSTER:-orka-agent-substrate-e2e}"
ctx="kind-${cluster}"
export KUBECONFIG="$(mktemp -t orka-substrate-kubeconfig.XXXXXX)"
kind export kubeconfig --name "${cluster}" --kubeconfig "${KUBECONFIG}"
kubectl --context "$ctx" -n default apply -f - <<'YAML'
apiVersion: core.orka.ai/v1alpha1
kind: Task
metadata:
  name: substrate-smoke
  namespace: default
spec:
  type: agent
  agentRef:
    name: codex-substrate-ci
  prompt: "Run make test and summarize the result."
  sessionRef:
    name: substrate-demo
    create: true
  execution:
    workspace:
      enabled: true
      provider: substrate
      templateRef:
        name: orka-codex-ci
        namespace: ate-demo
      reusePolicy: session
      cleanupPolicy: retain
YAML

kubectl --context "$ctx" -n default get task substrate-smoke -o yaml
```

Check the provider-neutral workspace lifecycle in
`status.executionWorkspace` (`phase`, `placement`, `density`, `resumeLatency`).
Status is intentionally sanitized — it must not expose actor IDs, snapshot URIs,
worker pod IPs, daemon URLs, or tokens.

### CI parity

`scripts/agent-substrate-e2e.sh` (the `Agent Substrate E2E` workflow) runs the
same path end-to-end and is secret-free. Run it directly when you want a clean,
self-contained validation with its own cluster lifecycle:

```bash
PATH="$(go env GOPATH)/bin:$PATH" SUBSTRATE_E2E_EXTENDED=1 bash scripts/agent-substrate-e2e.sh
```

Set `KEEP_CLUSTER=1` to inspect the cluster after a failure.

## Guardrails

- **Local/kind eval only.** Do not present this as a production install. Orka
  does not own Substrate lifecycle, WorkerPool capacity, or runtime-artifact
  supply chain in production.
- **Substrate owns its cluster.** Do not try to host it on a kindctl cluster;
  use the installer-created `kind-<KIND_CLUSTER>` context. Be explicit with the
  user that this is the one provider where kindctl is not the cluster creator.
- **Reference, don't fork.** Drive `hack/demos/cluster/install-substrate.sh` and
  override pins via env (`SUBSTRATE_REF`, `KIND_CLUSTER`, `AGENTIC`,
  `DEMO_CLUSTER_REUSE`). Copying the script invites drift from the CI-proven
  flow.
- **Human-in-the-loop vekil login.** Surface the device-code URL + code and wait
  for confirmation. Never complete the GitHub login yourself.
- **Destructive recreate.** `DEMO_CLUSTER_REUSE=recreate` (and a fresh
  `scripts/agent-substrate-e2e.sh` run) deletes the cluster. Confirm with the
  user before recreating a cluster that holds a completed vekil login or state.
- **No secrets in status/logs.** Never print API keys, source-control tokens, or
  Substrate bootstrap tokens, and never store them in Task specs/status. The
  bundled local e2e creates bootstrap-token Secrets for controller lookup, but
  the current pinned workspace `ActorTemplate` still carries
  `ORKA_WORKSPACE_BOOTSTRAP_TOKEN` as a literal env value; treat that as a
  local/CI exception to document and audit, not a production pattern. Prefer
  `valueFrom.secretKeyRef` for any production template.

## Troubleshooting

- `... requires substrate to be enabled`: controller missing
  `--substrate-enabled=true`.
- `bootstrap token secret name is required`: set
  `--substrate-bootstrap-token-secret-name` and create that Secret in the Task
  namespace **and** the `ActorTemplate` namespace.
- `ActorTemplate ... missing label/annotation` / `not Orka-compatible`: the
  template needs `orka.ai/execution-workspace: "true"`,
  `orka.ai/workspace-provider: substrate`, the daemon-port/protocol/staging-root
  annotations, and must run `/orka-workspace-agent` from the agent harness
  wrapper image. See the ActorTemplate contract in the concept doc.
- `ActorTemplate ... is not Ready`: inspect Substrate `WorkerPool`, snapshot
  config, image pulls, and `runsc` configuration.
- Task `Failed` with `WorkspaceCleanupFailed` after `resultRef.available=true`:
  command + result succeeded but Substrate failed to checkpoint/delete the actor
  (a known pinned-revision `runsc delete` flake in GitHub-hosted kind). Inspect
  `atelet` / `ateom-gvisor` logs.
- Inspect Substrate directly:
  `kubectl --context "$ctx" -n ate-system get pods`,
  `kubectl --context "$ctx" -n ate-demo get workerpool,actortemplate`,
  `kubectl --context "$ctx" -n ate-system logs deployment/atenet-router`.
- Full troubleshooting matrix: `website/docs/concepts/substrate.md`.
