# kindctl

[![CI](https://github.com/sozercan/kindctl/actions/workflows/ci.yml/badge.svg)](https://github.com/sozercan/kindctl/actions/workflows/ci.yml)

`kindctl` is a small Bash wrapper and agent skill for running **repo/worktree-scoped kind clusters** without ever touching your global Kubernetes config.

It lets you create and operate many local `kind` clusters across many repos and git worktrees while keeping each cluster isolated in its own kubeconfig:

```text
~/.kube/kind/<derived-cluster-name>.kubeconfig
```

The global kubeconfig stays out of the loop:

```text
~/.kube/config  # never read or written by kindctl
```

## Motivation: agents across many repos and worktrees

AI coding agents often work differently from humans:

- they jump between repos quickly;
- they run commands from fresh shell invocations;
- they may work in several git worktrees of the same repo;
- they can forget implicit shell state like `export KUBECONFIG=...`;
- they should never accidentally point `kubectl` at the wrong local cluster.

Raw `kind` is not ideal for that workflow. A normal command like this:

```sh
kind create cluster --name my-service
```

writes a context into `~/.kube/config` and switches the active Kubernetes context. With multiple agents, repos, and worktrees, that turns your global kubeconfig into shared mutable state. One terminal or agent can silently change which cluster another command targets.

`kindctl` avoids that by making the cluster identity deterministic from the current workspace path:

```text
repo/worktree path -> hash -> cluster name -> scoped kubeconfig
```

For example:

```text
/Users/me/projects/api              -> api-a13f02
/Users/me/projects/api-feature-wt   -> api-feature-wt-89be11
/Users/me/projects/web              -> web-7710aa
```

Each worktree gets its own cluster automatically. Re-running `kindctl` from the same worktree reuses the same derived cluster and kubeconfig. Running it from another worktree derives a different cluster.

That gives agents a simple rule:

> When working with local kind clusters, use `kindctl kubectl` or `kindctl exec`, never bare `kubectl`.

No global context switching. No stale `kind-*` contexts in `~/.kube/config`. No accidental cross-repo cluster targeting.

## How it works

From any repo or worktree, `kindctl` derives:

```text
workspace root: git root, or nearest .kind/ marker, or cwd
hash:           first 6 chars of sha256(workspace root)
cluster name:   <sanitized basename>-<hash>[-tag]
context:        kind-<cluster name>
kubeconfig:     ~/.kube/kind/<cluster name>.kubeconfig
```

Example:

```text
root:       /Users/me/projects/my-api
cluster:    my-api-2440c3
context:    kind-my-api-2440c3
kubeconfig: ~/.kube/kind/my-api-2440c3.kubeconfig
```

`kindctl create` calls `kind` with an explicit scoped kubeconfig:

```sh
kind create cluster \
  --name my-api-2440c3 \
  --kubeconfig ~/.kube/kind/my-api-2440c3.kubeconfig
```

All later operations derive the same name again from the current directory.

## Install

This skill is vendored into this repository at `.agents/skills/kindctl/` and is
surfaced to each harness (`.claude`, `.codex`, `.copilot`) through a
`skills/kindctl` symlink, so no separate install step is required here.

Call the wrapper by its repo-relative path:

```sh
.agents/skills/kindctl/bin/kindctl --help
```

The upstream skill (with its installer, Makefile, and tests) lives at
<https://github.com/sozercan/kindctl>; this is a trimmed, functional copy.

## Quick start

Create a cluster for the current repo/worktree:

```sh
kindctl create
```

Use it safely:

```sh
kindctl kubectl get nodes
kindctl kubectl get pods -A
```

Run other tools with scoped `KUBECONFIG`:

```sh
kindctl exec -- helm list -A
kindctl exec -- k9s
```

Delete it:

```sh
kindctl delete
```

## Multiple worktrees

Two git worktrees of the same repo automatically get different clusters because their absolute paths differ.

```sh
cd ~/projects/my-api
kindctl create
kindctl path

cd ~/projects/my-api-feature-worktree
kindctl create
kindctl path
```

The two `path` outputs will point at different files under `~/.kube/kind/`.

Running from subdirectories inside the same git worktree reuses the same cluster because `kindctl` normalizes to the git root.

## Multiple clusters per repo

Use `--tag` when one repo needs more than one cluster:

```sh
kindctl create --tag e2e
kindctl kubectl --tag e2e get nodes
kindctl delete --tag e2e
```

Tags are sanitized and capped so kind cluster names remain within kind/hostname limits.

## Repo-specific configuration

A repo can optionally commit:

```text
.kind/
  cluster.yaml
  setup.sh
```

### `.kind/cluster.yaml`

Native kind config. Use it for Kubernetes version, node count, port mappings, mounts, and networking.

```yaml
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
  - role: worker
```

`kindctl` passes this file to kind but still injects the derived cluster name with `--name`.

### `.kind/setup.sh`

Optional executable post-create hook. It runs with scoped environment:

```sh
KUBECONFIG=<scoped kubeconfig>
KINDCTL_CLUSTER=<cluster name>
KINDCTL_CONTEXT=kind-<cluster name>
KINDCTL_ROOT=<workspace root>
```

Example:

```sh
#!/usr/bin/env bash
set -euo pipefail

kubectl apply -f config/crd/bases
kubectl apply -k config/default
```

Inside the hook, bare `kubectl` is safe because `KUBECONFIG` is scoped for that process.

## Commands

```sh
kindctl create [--config FILE] [--tag TAG] [--k8s-version vX.Y.Z]
kindctl delete [--tag TAG]
kindctl exec [--tag TAG] -- <command> [args...]
kindctl kubectl [--tag TAG] <args...>
kindctl path [--tag TAG]
kindctl ctx [--tag TAG]
kindctl env [--tag TAG]
kindctl list [--workspace|--all]
kindctl load [--tag TAG] <image>
kindctl hibernate [--tag TAG]
kindctl resume [--tag TAG]
kindctl doctor
kindctl prune (--workspace|--dead) [--yes]
kindctl nuke [--yes]
```

## Safety properties

- No arbitrary `--name` override in v1.
- No code path reads or writes `~/.kube/config`.
- Each kubeconfig is stored under `~/.kube/kind/`.
- Store directory is `0700`; registry and kubeconfigs are `0600`.
- Registry writes use a lock and atomic replace.
- Bulk destructive operations are registry-scoped.
- Unmanaged kind clusters are reported but not deleted.
- `hibernate`/`resume` select containers by kind's Docker label, not by name glob.

## GitHub Actions security and dependency updates

Workflow actions are pinned to full commit SHAs, and `make lint` includes a workflow pinning check so unpinned `uses:` references fail CI. Dependabot is configured for GitHub Actions updates, and a Dependabot automerge workflow merges successful non-draft Dependabot PRs after the main CI workflow passes.

## Testing

Fast mocked tests:

```sh
make lint
make test
```

Real kind/docker integration tests:

```sh
make test-integration
```

The integration test creates real clusters for multiple worktrees, verifies unique cluster names, verifies reuse within each worktree, and deletes the clusters afterward.

## Current invariant

> No `kindctl` code path — and no tool the skill tells an agent to run — ever reads or writes `~/.kube/config`. Every cluster lives in its own `~/.kube/kind/<name>.kubeconfig`, addressed by a deterministic worktree-path hash plus optional sanitized tag.
