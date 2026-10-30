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
| `spec.env[N] (NAME) uses valueFrom.secretKeyRef, which container Tasks receive only when the operator enables direct Secret mounts` | An `env` entry reads a Secret or ConfigMap | Drop the entry, or ask an operator to enable `ORKA_AGENT_DIRECT_SECRET_MOUNTS` |

## Other fields

Container Tasks use the same Task fields as everything else:

- `spec.timeout` — how long before the Task is killed
- `spec.retryPolicy` — retries with backoff
- `spec.resources` — CPU and memory requests and limits
- `spec.secretRef` — a Secret. For a container Task it is **not delivered by default**;
  see the caution below
- `spec.execution` — a `RuntimeClass` such as `gvisor` for stronger isolation
- `spec.schedule` — a cron expression, for recurring work

See [Configuration](../reference/configuration.md) for the full list.

:::caution[A container Task does not receive `spec.secretRef` by default]
Container Tasks are treated as untrusted compute, so the controller skips the direct Secret
mount for them. Your Secret is delivered only if an operator has turned on the legacy
`ORKA_AGENT_DIRECT_SECRET_MOUNTS` opt-in — and even then it arrives as **files under
`/secrets/task`** (one file per Secret key), never as environment variables. Read the key
you need from `/secrets/task/<key>`. [Security](../concepts/security.md) explains the
boundary.

Environment-variable injection applies to an Agent's `secretRef`, not a Task's.

`spec.env` follows the same policy. Without the opt-in, a container Task whose `env`
entry uses `valueFrom.secretKeyRef`, `valueFrom.configMapKeyRef`, or `valueFrom.fileKeyRef`
ends `Failed` before its Pod is created, and the message names the entry and the opt-in.
`valueFrom.fieldRef` and `valueFrom.resourceFieldRef` read the Pod's own metadata and are
always allowed. With a workspace, `env` reaches your command but not the init container that
clones the repository with the read credential; only the `HTTP_PROXY`, `HTTPS_PROXY`, and
`NO_PROXY` variables pass through to it.
:::

## Custom image result and artifact contract {#custom-image-result-and-artifact-contract}

A finished Task keeps one result and any number of artifacts: screenshots, traces, HAR
files, reports. How they reach Orka depends on the image.

### Images that use Orka's worker entrypoint

The default worker image, and any custom image whose `ENTRYPOINT` runs Orka's `/worker`
binary (the browser worker image wraps it), deliver for you:

- The command's stdout and stderr become the result. A command that prints nothing posts
  no result, and the Pod log is kept instead; its artifacts are still uploaded. Output that
  is a [structured result](#recommended-result-shape) is kept as it is; with a workspace,
  the worker adds `baseSHA`, `headSHA`, `diff`, and `files` to it, and wraps any other
  output in the `summary` of its own.
- Every regular file in `/tmp/artifacts` is uploaded as an artifact when the command
  ends: at most 10 MB per file and 50 MB per run. Subdirectories and symlinks are skipped.
  Uploads do not depend on the result being posted.
- When the Task has a `timeout`, the worker stops the command shortly before it, uploads
  what is already in `/tmp/artifacts`, and posts the output so far with the outcome
  `unknown_outcome`. The Task ends `Failed` as timed out, like a Task killed by the Job
  deadline, and a `retryPolicy` does not rerun it: the outcome is unknown, so a follow-up
  Task reconciles it. The worker exits with code 124. The margin is a tenth of the
  timeout, from 20 seconds to 2 minutes, and never more than half of a very short timeout.
- The timeout runs from the Task's start, as the Job deadline does, so scheduling, the
  image pull, and the repository clone count against it. When startup alone uses the
  budget, the command is stopped at once and only what is already in `/tmp/artifacts` can
  be delivered. Give a Task on a large image, or on a node that has not pulled it yet,
  a timeout well above its expected run time.

With a custom image, put the command in `args` and leave `command` empty. An explicit
`command` replaces the entrypoint, and then only the log fallback below applies.

### Any other image

Any other image posts its own result and artifacts over HTTP. The Pod has these
environment variables:

| Variable | Value |
| --- | --- |
| `ORKA_RESULT_ENDPOINT` | Where to `POST` the result |
| `ORKA_CONTROLLER_URL` | Base URL for artifact uploads |
| `ORKA_TASK_NAMESPACE`, `ORKA_TASK_NAME`, `ORKA_TASK_UID` | The Task's identity |
| `ORKA_SA_TOKEN_PATH` | The file holding the Pod's callback token |
| `ORKA_TIMEOUT_SECONDS` | The Task's `timeout`, when it has one |
| `ORKA_DEADLINE` | When that timeout ends, in RFC 3339; stop and deliver before it |

Authenticate every request with `Authorization: Bearer <token>`, reading the token from
`ORKA_SA_TOKEN_PATH` each time, because the kubelet rotates it. The token is bound to this
Pod and to the audience `orka.ai/task-callback`. It works only for this Task's result,
artifact, and event endpoints, and only while the Task is active. The Kubernetes API
rejects it.

```sh
# A request can arrive before the controller has recorded the Pod's Job, which answers
# 503 until it has, so retry transient answers. The token is read for each attempt; a
# value cached at startup expires after an hour.
post() { # post <url> <content-type> <curl data option>...
  url=$1; type=$2; shift 2
  for attempt in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15; do
    status=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$url" \
      -H "Authorization: Bearer $(cat "$ORKA_SA_TOKEN_PATH")" -H "Content-Type: $type" "$@") || true
    case "$status" in
      2*) return 0 ;;
      000|502|503|504) sleep 1 ;;
      *) echo "POST $url: HTTP $status" >&2; return 1 ;;
    esac
  done
  echo "POST $url: gave up after HTTP $status" >&2; return 1
}

# Upload an artifact. The last path segment is the artifact's name.
post "$ORKA_CONTROLLER_URL/internal/v1/artifacts/$ORKA_TASK_NAMESPACE/$ORKA_TASK_NAME/home.png" \
  image/png --data-binary @/tmp/home.png

# Post the result.
post "$ORKA_RESULT_ENDPOINT" application/json \
  --data '{"version":1,"outcome":"completed","summary":"Checked the home page","data":{"title":"Example Domain"},"artifacts":[{"filename":"home.png","contentType":"image/png"}]}'
```

| | Result | Artifact |
| --- | --- | --- |
| Size limit | 10 MB | 10 MB per file |
| Name | — | One path segment: at most 255 bytes, no `/`, no control characters, not `.` or `..` |
| Repeated `POST` | The last one wins | The same name replaces the earlier file |
| Success status | `204` | `201` |
| After the Task finishes | Refused with `403` | Refused with `403` |

Without a result `POST`, the controller stores up to 5 MiB of the Pod's log as the result
when the Task finishes. Artifacts have no fallback. Outside Orka's worker entrypoint,
`/tmp/artifacts` is only a naming convention: files left there are discarded with the Pod.
Upload them as they are written rather than at the end, because a Task stopped by its
deadline uploads nothing more. `ORKA_TIMEOUT_SECONDS` lets the image stop itself in time.

Read them back with the Task API or the CLI. Without a namespace, the API looks in the
caller's own namespace and the CLI in `default`, so name the Task's namespace, `orka-system`
in the example above:

| | API | CLI |
| --- | --- | --- |
| Result | `GET /api/v1/tasks/{name}/result?namespace={namespace}` returns `{"result": "<the posted body>"}` | `orka task result <name> -n <namespace>` |
| List artifacts | `GET /api/v1/tasks/{name}/artifacts?namespace={namespace}` | `orka task artifacts <name> -n <namespace>` |
| Download one | `GET /api/v1/tasks/{name}/artifacts/{filename}?namespace={namespace}` | `orka task download <name> <filename> -n <namespace>` |

### Recommended result shape

Orka stores any result body. A JSON object with `"version": 1` is parsed as a structured
result, so a coordinator's `wait_for_tasks` returns its `outcome`, `summary`, `data`, and
`artifacts` instead of raw text. Orka does not enforce this vocabulary for `outcome`, but
coordinators can branch on it:

| `outcome` | Meaning |
| --- | --- |
| `completed` | The run did what it was asked; `summary` and `data` hold the answer |
| `blocked` | The site refused it: an error page, rate limit, or bot wall |
| `captcha` | A CAPTCHA stopped it |
| `login_required` | The site needs a login the run does not have |
| `decision_required` | Continuing needs a choice the Task was not asked to make |
| `unknown_outcome` | The run stopped before it could tell whether its action took effect |

Keep large payloads in artifacts and list them in `artifacts`; keep `summary` short.

:::caution[Retry only runs that can safely repeat]
`retryPolicy` reruns a failed Task from the start. That is fine for a read-only check. A run
that may already have acted, by submitting a form, placing an order, or sending a message,
must not be retried after `unknown_outcome` or a failure. Reconcile it instead: create a
follow-up Task that checks at the source what actually happened.
:::

## Browsers

Playwright and Chromium need a few extra settings to run under these rules. Orka publishes a
browser worker image that handles them; see [Browser tasks](browser-tasks.md).

## When an agent creates these

Coordinator agents create container Tasks through the `create_container_task` tool to
compile and test their own work. They are given the same rules above in their system
prompt, so they usually get it right — but if you see an agent looping on
`go: command not found`, this page is what it failed to apply.

The tool can also set `env` values, `resources`, a `retryPolicy`, and an
`execution.runtimeClassName` from the operator's allowlist. Its
[parameter reference](../reference/built-in-tools.md#create_container_task) lists the limits.

The tool can name Secrets too, through `secretRef` and `env[].valueFrom.secretKeyRef`. Each
Secret is authorized for the caller before the Task exists, but authorization does not
deliver it: the [caution above](#other-fields) still applies. Without the operator's
`ORKA_AGENT_DIRECT_SECRET_MOUNTS` opt-in, a `secretRef` is not mounted and an env
`secretKeyRef` fails the Task before its Pod starts.

See [Multi-agent coordination](multi-agent-coordination.md).
