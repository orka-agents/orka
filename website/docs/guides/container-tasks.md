---
slug: /container-tasks
description: "Running an ordinary container as a Task, and the filesystem rules it has to live with."
---

# Container tasks

A `type: container` Task runs an ordinary container command — no model involved. It is how
you get a build, a test run, or a lint pass done, either on its own or because a coding
agent asked for one.

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Task
metadata:
  name: unit-tests
  namespace: orka-system
spec:
  type: container
  image: golang:1.26
  command: ["sh", "-c"]
  args: ["export GOCACHE=/tmp/gocache GOMODCACHE=/tmp/gomodcache && go test fmt"]
  timeout: 20m
```

This example tests Go's standard `fmt` package, whose source is included in the image.
It needs no repository checkout. To run `go test ./...` on your own module, provide the
source through `spec.workspace` or a custom image and run from the module directory.
The `./...` examples below assume that source is present.

The example also redirects Go's caches and uses a non-login shell. Both are required by
the filesystem and environment rules below.

## The filesystem is read-only

Task Pods run with `readOnlyRootFilesystem: true`. Which paths are writable depends on
whether the Task has a workspace:

| Path | What it is | When you get it |
| --- | --- | --- |
| `/tmp` | Scratch space | Always |
| `/workspace` | The working directory, including any cloned repository | Only with `spec.workspace` |
| `/home/worker` | The worker's home directory | Only with `spec.workspace` |

Everything else — including the places most language toolchains put their caches — will
reject writes.

:::caution[A container Task without `spec.workspace` gets `/tmp` and nothing else]
`/workspace` and `/home/worker` are backed by `emptyDir` volumes that the controller only
attaches when the Task needs a workspace — that is, agent Tasks, and container Tasks that
set `spec.workspace`. Write to `/workspace` from a plain container Task and the path is
simply not there. Either set `spec.workspace`, or keep everything under `/tmp`.
:::

This is a deliberate hardening choice, not an oversight. It means a command cannot modify
its own image at runtime. See [Security](../concepts/security.md#execution-workloads).

### Redirect your toolchain's cache

Every toolchain that caches outside those three paths needs an environment variable:

| Toolchain | Default location | Set instead |
| --- | --- | --- |
| Go modules | `/go/pkg/mod` | `GOMODCACHE=/tmp/gomodcache` |
| Go build | `/root/.cache/go-build` | `GOCACHE=/tmp/gocache` |
| npm | `~/.npm` | `npm_config_cache=/tmp/npm-cache` |
| pip | `~/.cache/pip` | `PIP_CACHE_DIR=/tmp/pip-cache` |

:::danger[An inline prefix only applies to the first command]
This looks right and is not:

```bash
GOCACHE=/tmp/gocache GOMODCACHE=/tmp/gomodcache go test ./... && go build ./...
```

Shell variable prefixes apply to a single command. `go test` gets the redirected caches;
`go build` reverts to `/go/pkg` and crashes with
`could not create module cache: mkdir /go/pkg: read-only file system`.

Use `export` so the whole chain inherits it:

```bash
export GOCACHE=/tmp/gocache GOMODCACHE=/tmp/gomodcache && go test ./... && go build ./...
```
:::

You can also set them in `spec.env` instead of in the command, which is cleaner when the
command is long:

```yaml
spec:
  type: container
  image: golang:1.26
  env:
    - name: GOCACHE
      value: /tmp/gocache
    - name: GOMODCACHE
      value: /tmp/gomodcache
  command: ["sh", "-c"]
  args: ["go test ./... && go build ./..."]
```

## Use `sh -c`, not `bash -lc`

Always:

```yaml
command: ["sh", "-c"]
```

Never:

```yaml
command: ["bash", "-lc"]    # breaks official language images
```

The `-l` flag makes bash a *login* shell, which rebuilds `PATH` from `/etc/profile`. The
official `golang`, `node`, and `python` images put their tool on `PATH` through a
Dockerfile `ENV` instruction, and a login shell throws that away. The result is:

```
bash: line 1: go: command not found
```

A non-login `sh -c` keeps the environment the image set up. If the image has its own
entrypoint that you want, use that instead.

## Error messages and what they mean

| Message | Cause | Fix |
| --- | --- | --- |
| `go: command not found` in a `golang:*` image | `bash -lc` reset `PATH` | Use `["sh","-c"]` |
| `could not create module cache` | Go module cache outside `/tmp` | `export GOMODCACHE=/tmp/gomodcache` |
| `mkdir /go/pkg: read-only file system` | Same | Same |
| `mkdir /root/.cache: read-only file system` | Go build cache outside `/tmp` | `export GOCACHE=/tmp/gocache` |
| Second command in a chain fails, first succeeded | Inline env prefix, not `export` | Use `export ... && ...` |
| `EACCES` / `EROFS` from npm or pip | Cache outside `/tmp` | Set `npm_config_cache` or `PIP_CACHE_DIR` |
| `spec.env[N] (NAME) uses valueFrom.secretKeyRef, which container Tasks receive only when the operator enables direct Secret mounts` | An `env` entry reads a Secret or ConfigMap | For a ConfigMap (`configMapKeyRef`), remove the entry; for a Secret (`secretKeyRef`), drop the entry or ask an operator to grant your identity `use` on the Secret under [the Secret use policy](#secrets) |

## Other fields

Container Tasks use the same Task fields as everything else:

- `spec.timeout` — how long before the Task is killed
- `spec.retryPolicy` — retries with backoff
- `spec.resources` — CPU and memory requests and limits
- `spec.secretRef` — a Secret. For a container Task it is **not delivered by default**;
  see [Secrets](#secrets)
- `spec.execution` — a `RuntimeClass` such as `gvisor` for stronger isolation
- `spec.schedule` — a cron expression, for recurring work

See [Configuration](../reference/configuration.md) for the full list.

## Secrets

Container Tasks are treated as untrusted compute, so by default they receive no Secrets:
the controller skips `spec.secretRef`, and an `env` entry that uses
`valueFrom.secretKeyRef`, `valueFrom.configMapKeyRef`, or `valueFrom.fileKeyRef` makes the
Task end `Failed` before its Pod is created, with a message that names the entry.
`valueFrom.fieldRef` and `valueFrom.resourceFieldRef` read the Pod's own metadata and are
always allowed. With a workspace, `env` reaches your command but not the init container that
clones the repository with the read credential; only the `HTTP_PROXY`, `HTTPS_PROXY`, and
`NO_PROXY` variables pass through to it.

An operator can let specific container Tasks receive specific Secrets with the Secret use
policy (`--container-task-secret-use-authorization-enabled`, Helm
`controller.containerTaskSecretUse.enabled`). A container Task that names a Secret, through
`spec.secretRef` or an `env` entry's `secretKeyRef`, is then admitted only if the identity that
writes it holds the `use` verb on that Secret:

```yaml
- apiGroups: [""]
  resources: ["secrets"]
  verbs: ["use"]
  resourceNames: ["repository-bot-key"]
```

Grant it to the identity that applies your job manifests, for example a GitOps controller
or an operator's `kubectl` user. Orka grants `use` on Secrets to none of its own identities,
so Tasks created by agents, tools, chat, or the Orka API never receive Secrets this way, and
the controller also refuses a Task that carries their provenance. The runs of a scheduled
Task receive the Secrets the scheduled Task names. The Secret must be in the Task's
namespace, and `configMapKeyRef` and `fileKeyRef` stay refused. Changing the spec of such a Task,
for example its image or command, needs `use` again. Wait until the 30-second settling period
after installing or updating the policy ends before creating Tasks; a Task created before the
policy took effect or during that settling window fails instead and must be recreated.

`spec.secretRef` arrives as **files under `/secrets/task`**, one file per key; read the key you
need from `/secrets/task/<key>`. An `env` entry's `secretKeyRef` becomes that environment
variable. Neither value appears in the Task's status or events. See
`config/container-task-secret-use-policy/README.md` for the details.

:::caution[`ORKA_AGENT_DIRECT_SECRET_MOUNTS` opens the boundary for every container Task]
The legacy `ORKA_AGENT_DIRECT_SECRET_MOUNTS=true` opt-in delivers `spec.secretRef` and `env`
Secret references to every container Task in the namespace, including ones agents create, and
lets them name any Secret there. Prefer the Secret use policy; the two cannot be combined.
Environment-variable injection of a whole Secret applies to an Agent's `secretRef`, not a
Task's, and the Secret use policy never delivers an Agent's Secret.
:::

## When an agent creates these

Coordinator agents create container Tasks through the `create_container_task` tool to
compile and test their own work. They are given the same rules above in their system
prompt, so they usually get it right — but if you see an agent looping on
`go: command not found`, this page is what it failed to apply.

See [Multi-agent coordination](multi-agent-coordination.md).
