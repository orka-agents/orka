---
name: agent-substrate-deploy
description: Stand up Agent Substrate (warm, suspendable, gVisor-isolated Actors) on a dedicated local kind cluster wired to Orka, then run an Orka agent Task through a Substrate-backed execution workspace. Use when the user asks to install, enable, deploy, configure, validate, demo, or troubleshoot the Orka substrate execution-workspace provider, SubstrateActorPool, ActorTemplate, or Substrate-backed MCP Tools.
---

# Agent Substrate Deploy

Stand up [Agent Substrate](https://github.com/agent-substrate/substrate) and an
Orka-compatible `WorkerPool` + `ActorTemplate` on a local kind cluster, wire Orka
with `--substrate-*` flags, then validate by running a `type: agent` Task whose
`spec.execution.workspace` uses `provider: substrate`.

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
  `kind-<KIND_CLUSTER>`). This cluster is registered in the standard kubeconfig,
  not in kindctl's scoped store, so use plain
  `kubectl --context kind-<KIND_CLUSTER>` for it.

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

2. **Stand up Substrate + Orka.** First run builds four images and the Substrate
   control plane — several minutes.

   ```bash
   # Base standup only (secret-free, no model run):
   AGENTIC=0 bash hack/demos/cluster/install-substrate.sh

   # Full agentic standup (codex Actor image + vekil + secrets):
   bash hack/demos/cluster/install-substrate.sh
   ```

   Re-running on an existing cluster prompts reuse/recreate/cancel. For
   non-interactive runs set `DEMO_CLUSTER_REUSE=reuse|recreate|cancel` — **note
   that `recreate` destroys the cluster and any completed vekil login.**

3. **Stand up the model proxy (vekil) — pause for the human.** When `AGENTIC=1`,
   the installer deploys vekil with `--skip-wait`, starting a GitHub device-code
   login. **Surface the login URL and code to the user and wait for their
   confirmation; never complete the login on their behalf** (the
   `$vekil-reverse-proxy-deploy` guardrail). Find the prompt and check
   readiness:

   ```bash
   ctx="kind-${KIND_CLUSTER:-orka-agent-substrate-e2e}"
   kubectl --context "$ctx" -n vekil-system logs deploy/vekil | grep 'login/device'
   kubectl --context "$ctx" -n vekil-system exec deploy/vekil -- wget -qO- http://127.0.0.1:1337/readyz
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
   > kubectl --context "$ctx" -n vekil-system patch deploy vekil --type=json \
   >   -p '[{"op":"remove","path":"/spec/template/spec/containers/0/livenessProbe"}]'
   > kubectl --context "$ctx" -n vekil-system delete rs -l app=vekil \
   >   --field-selector 'status.replicas!=0' 2>/dev/null || true
   > ```
   >
   > Then read the code from the single fresh pod. GitHub device codes expire in
   > ~15 min, so surface it promptly and, if it expires, bounce the pod
   > (`kubectl delete pod -l app=vekil`) for a fresh code rather than waiting.

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
> suspend/delete + retained-workspace reuse), exercised by
> `SUBSTRATE_E2E_EXTENDED=1` during standup. A **plain** agent Task (no
> `execution.workspace`) runs fine through the harness + model proxy. Treat the
> Task YAML below as the intended API once the harness wires workspaces; until
> then, validate via the e2e's direct-actor exercises.

The installer leaves a fully wired cluster; `SUBSTRATE_E2E_EXTENDED=1` (default)
already smoke-tests direct actor create/resume/exec/suspend/delete and the
retained-workspace warm-reuse path during standup.

To talk to the cluster, first make sure its context is in your kubeconfig — the
installer runs the e2e with an isolated kubeconfig and does **not** leave
`kind-<KIND_CLUSTER>` in your default one:

```bash
kind export kubeconfig --name "${KIND_CLUSTER:-orka-agent-substrate-e2e}"
```

To drive an Orka Task yourself (intended shape; currently gated as noted above):

```bash
ctx="kind-${KIND_CLUSTER:-orka-agent-substrate-e2e}"
kubectl --context "$ctx" -n default apply -f - <<'YAML'
apiVersion: core.orka.ai/v1alpha1
kind: Task
metadata:
  name: substrate-smoke
  namespace: default
spec:
  type: agent
  agentRef:
    name: codex-agent
  prompt: "Run make test and summarize the result."
  sessionRef:
    name: substrate-demo
    create: true
  execution:
    workspace:
      enabled: true
      provider: substrate
      templateRef:
        name: orka-codex
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
- **No secrets in spec/status/logs.** Never put API keys, source-control tokens,
  or the Substrate bootstrap token directly in an `ActorTemplate`; use Secrets.
  The bootstrap token is control-plane credential material — keep it in a Secret
  and never print it.

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
