---
name: kindctl
description: Use repo/worktree-scoped kind clusters without touching the global kubeconfig. Trigger when users mention kind cluster, local k8s, local Kubernetes, spin up a cluster, Kubernetes-in-Docker, multi-repo kind, or multi-worktree cluster work.
---

# kindctl

Use this skill whenever work involves a local `kind` cluster, local k8s/Kubernetes, Kubernetes-in-Docker, or repo/worktree-specific test clusters.

## Mental model

- `kind` clusters are global to the Docker daemon.
- `~/.kube/config` is a shared mutable global and must not be used for kindctl-managed clusters.
- `kindctl` derives a cluster name from the current repo/worktree path and stores its kubeconfig in `~/.kube/kind/<name>.kubeconfig`.
- The current directory is enough: random repos and git worktrees do not need to know anything special.

## Golden rule for agents

Never run bare `kubectl` or `helm` against a kindctl-managed cluster.

Prefer:

```sh
.agents/skills/kindctl/bin/kindctl kubectl get nodes
.agents/skills/kindctl/bin/kindctl exec -- helm list
```

Do not run:

```sh
kubectl get pods
kubectl config use-context kind-something
kind create cluster
```


## Installation

This skill is vendored into the repo at `.agents/skills/kindctl/` and surfaced to
each harness (`.claude`, `.codex`, `.copilot`) via a `skills/kindctl` symlink, so no
install step is required. Invoke the wrapper by its repo-relative path:

```sh
.agents/skills/kindctl/bin/kindctl <command>
```

The skill itself should invoke `bin/kindctl` by absolute path or by resolving the skill directory. Do not rely on shell aliases.

## Commands

```sh
kindctl create [--config F] [--tag T] [--k8s-version vX.Y.Z]
kindctl delete [--tag T]
kindctl kubectl [--tag T] <args...>
kindctl exec [--tag T] -- <cmd...>
kindctl path [--tag T]
kindctl ctx [--tag T]
kindctl env [--tag T]
kindctl load [--tag T] <image>
kindctl hibernate [--tag T]
kindctl resume [--tag T]
kindctl list [--workspace|--all]
kindctl doctor
kindctl prune --workspace [--yes]
kindctl prune --dead [--yes]
kindctl nuke [--yes]
```

If `kindctl` is not on `PATH`, use the repo-relative path:

```sh
.agents/skills/kindctl/bin/kindctl <command>
```

## Recipes

### Create and use a repo cluster

```sh
kindctl create
kindctl kubectl get nodes
kindctl kubectl get pods -A
```

### Use a second cluster for the same repo

```sh
kindctl create --tag e2e
kindctl kubectl --tag e2e get nodes
```

Use the same `--tag` on every command that targets that cluster.

### Load a local image

```sh
docker build -t myapp:dev .
kindctl load myapp:dev
kindctl kubectl set image deployment/myapp myapp=myapp:dev
```

### Run helm, k9s, or other tools safely

```sh
kindctl exec -- helm list -A
kindctl exec -- k9s
```

### Ingress

For ingress, commit `.kind/cluster.yaml` in the target repo with explicit `extraPortMappings`. Example:

```yaml
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
    extraPortMappings:
      - containerPort: 80
        hostPort: 8080
        protocol: TCP
```

Only one running cluster can bind a given host port. Use `kindctl hibernate` on other clusters or choose distinct host ports.

### Hibernate/resume after a work session or reboot

```sh
kindctl hibernate
kindctl resume
kindctl doctor
```

### Recover from failed setup

If `.kind/setup.sh` fails, the cluster is preserved and registry status is `failed`.

```sh
kindctl doctor
kindctl exec -- kubectl get pods -A
kindctl delete       # if you want to recreate cleanly
kindctl create
```

### Delete safely

```sh
kindctl delete
```

Bulk cleanup is registry-scoped and asks for confirmation unless `--yes` is passed:

```sh
kindctl prune --workspace
kindctl prune --dead
kindctl nuke
```

## Repo-specific `.kind/`

Optional repo files:

```text
.kind/cluster.yaml  # native kind config: nodes, Kubernetes version, ports, mounts
.kind/setup.sh      # post-create setup: CRDs, ingress, namespaces, local images
```

Inside `.kind/setup.sh`, bare `kubectl` is okay because kindctl sets `KUBECONFIG` for that one hook process.

Outside that hook, use `kindctl kubectl` or `kindctl exec`.

## Invariant

No skill instruction should read, write, merge, or switch `~/.kube/config`. Every operation must use `kindctl` or a scoped kubeconfig path produced by `kindctl`.
