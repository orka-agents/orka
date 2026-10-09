---
slug: /guides/toolboxes
description: "Give built-in coding agents extra command-line tools with read-only toolbox images."
---

# Toolboxes

Orka's built-in coding agents (Codex, Claude, Copilot, and OpenCode) can only
run the programs already inside Orka's runtime image. A **toolbox** is an
ordinary container image that holds extra tools under one folder, such as
`/opt/yq-jq/bin`. An Agent lists its toolboxes, and Orka makes each toolbox
folder appear read-only at the same path inside the runtime Pod and puts its
`pathEntries` on the agent's `PATH`.

## The rules

- **Nothing from a toolbox runs as root.** The supervisor only checks, with
  `lstat`, that the declared folders exist. Agent processes stay non-root with
  no capabilities and `NoNewPrivs` set, so setuid bits and file capabilities
  grant nothing.
- **Toolboxes add tools, nothing else.** No credentials, no extra network
  access, and no extra Kubernetes permissions. A tool that needs those, such
  as `gh` or `kubectl`, is installed but cannot reach anything.
- **Images are referenced by digest** and must come from a registry the
  operator allowed. Tags are rejected.
- **The feature is off by default.**
- **A toolbox that cannot be set up fails the Task** with a clear
  `ToolboxUnavailable` reason. An agent never starts without the tools it was
  promised, and nothing retries forever.

## Operator setup

Enable toolboxes in the chart and name the registries that may serve them:

```yaml
controller:
  acpRuntime:
    toolboxes:
      enabled: true
      allowedRegistries:
        - registry.example.com/tools   # host plus optional path prefix
        - ghcr.io/my-org
      imagePullSecrets: []            # Secret names in the runtime namespace
      nodeSelector: {}                # merged into runtime Pods that bind toolboxes
      mountMethod: copy               # copy | imageVolume
```

The same settings exist as controller flags: `--acp-toolboxes-enabled`,
`--acp-toolbox-allowed-registries`, `--acp-toolbox-image-pull-secrets`,
`--acp-toolbox-node-selector`, and `--acp-toolbox-mount-method`.

An allowlist entry matches the exact registry host (and port) and whole
repository path segments only. `registry.example.com.evil.io` never matches
`registry.example.com`, and `registry.example.com/tools-evil` never matches
`registry.example.com/tools`. Validation makes no network calls.

### Mount methods

| | `copy` (default) | `imageVolume` |
| --- | --- | --- |
| Works on | Every Kubernetes version Orka supports | Kubernetes 1.36+ with containerd 2.2+ or CRI-O 1.33+ on the selected nodes |
| How | An init container copies the toolbox folder into a scratch volume and sanitizes it on the way: modes become `0555`/`0444`, setuid/setgid/sticky bits and file capabilities are dropped, symlinks are copied as text and never followed, and FIFOs, sockets, and device files fail the copy | Kubernetes mounts the toolbox image directly as a read-only `image` volume with a `subPath`; the files arrive as they are, and the Pod's existing protections (`allowPrivilegeEscalation: false`, dropped capabilities, device rules) keep them harmless |
| Start time measured with a 146 MB toolbox | 14 s when the image is not on the node yet, 7 s when it is | 9 s / 1 s |
| Architecture check | Done by the copier | Done by the supervisor through an unprivileged child before it reports ready |

With `imageVolume`, the controller reads the API server version at startup.
On a server older than 1.36 it logs an error and rejects Agents with
toolboxes with `ToolboxUnavailable: image volumes need Kubernetes 1.36 or
newer`. It never silently falls back to copying, so you always know which
method is running. Orka cannot read the container runtime version from the
API, so use `nodeSelector` to pin toolbox Pods to qualified nodes.

Runtime pools scale to zero after a period of idleness, so the start-time
difference lands on real users often; prefer `imageVolume` when your cluster
supports it.

## Agent YAML

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Agent
metadata:
  name: release-helper
spec:
  model:
    name: gpt-5.4
  runtime:
    type: codex
    toolboxes:
      - image: registry.example.com/tools/yq-jq@sha256:<digest>
        mountPath: /opt/yq-jq          # the folder the tools live in, inside the toolbox image
        pathEntries: [bin]             # added to PATH as /opt/yq-jq/bin
```

Validation rules, enforced by the CRD schema, Agent admission, Task planning,
and again inside the runtime Pod:

- `image` is a fully qualified reference pinned by digest
  (`<registry>/<repository>@sha256:<64 hex>`), from an allowed registry.
- `mountPath` is exactly `/home/linuxbrew/.linuxbrew` or `/opt/<name>`. It
  must be clean (no `..`, no `//`, no trailing `/`) and `<name>` must not be
  one the runtime images already use: `codex`, `codex-acp`, `claude`,
  `claude-agent-acp`, `copilot`, `opencode`, `ripgrep`, `yarn-v1.22.22`.
- Two toolboxes may not share, contain, or sit inside each other's
  `mountPath`.
- `pathEntries` are clean relative paths inside `mountPath` with no `..`.
- No path may contain `:`, NUL, or control characters.
- At most 4 toolboxes, 8 `pathEntries` per toolbox, and 256 bytes per path.

Order matters: toolboxes and their `pathEntries` are appended to `PATH` in the
order you declare them, **after** the system folders
(`/usr/local/bin:/usr/bin:/bin`), so a toolbox never replaces a system command.
Changing any toolbox field creates a new RuntimePool; Tasks that are already
running keep the toolboxes frozen in their execution snapshot.

Agents with toolboxes get a short Orka-generated note at the start of the
first prompt of each runtime session that lists the toolbox folders. It lists
folders from the Agent spec, never file names from inside the image, so a
toolbox author cannot put text in front of the model.

## Runtime base

Every built-in runtime image runs on Debian 13 ("trixie") with glibc 2.41.
Toolbox binaries must run with that glibc.

| Runtime | Final base | Debian | glibc |
| --- | --- | --- | --- |
| Codex | `node:22.22.0-trixie-slim` | 13 (trixie) | 2.41 |
| Claude | `node:22.22.0-trixie-slim` | 13 (trixie) | 2.41 |
| Copilot | `node:22.22.0-trixie-slim` | 13 (trixie) | 2.41 |
| OpenCode | `debian:trixie-slim` | 13 (trixie) | 2.41 |

Static binaries work everywhere. Dynamically linked tools must find their
libraries inside the toolbox (for example through an rpath) or in the runtime
image itself, which is deliberately small.

## Recipe: a multi-architecture Dockerfile

The toolbox copier and the supervisor check that the ELF files in each
`pathEntries` folder match the node's CPU architecture, because Kubernetes
pulls an `arm64`-only image onto an `amd64` node without complaint. Build for
both architectures and verify each download with its own checksum, since the
`amd64` and `arm64` release files differ:

```dockerfile
FROM --platform=$BUILDPLATFORM debian:bookworm-slim AS fetch
ARG TARGETARCH
RUN apt-get update && apt-get install -y --no-install-recommends curl ca-certificates
RUN set -eu; case "$TARGETARCH" in \
      amd64) sha=<sha256 of yq_linux_amd64> ;; \
      arm64) sha=<sha256 of yq_linux_arm64> ;; \
      *) exit 1 ;; esac; \
    mkdir -p /out/opt/yq-jq/bin; \
    curl -fsSLo /out/opt/yq-jq/bin/yq "https://github.com/mikefarah/yq/releases/download/v4.54.1/yq_linux_${TARGETARCH}"; \
    echo "$sha  /out/opt/yq-jq/bin/yq" | sha256sum -c -; \
    chmod 0755 /out/opt/yq-jq/bin/yq

FROM scratch
COPY --from=fetch /out/ /
LABEL ai.orka.toolbox='{"version":1,"mountPath":"/opt/yq-jq","pathEntries":["bin"]}'
```

```bash
docker buildx build --platform linux/amd64,linux/arm64 -t registry.example.com/tools/yq-jq:1 --push .
crane digest registry.example.com/tools/yq-jq:1   # pin this digest in the Agent
```

A `FROM scratch` image works because the copier never runs anything from the
toolbox image: Orka hands its own static copier binary into the toolbox
container through a scratch volume.

## Recipe: dalec-homebrew

[dalec-homebrew](https://github.com/sozercan/dalec-homebrew) builds Homebrew
formulae into a container image. Put every formula in **one** image under
`/home/linuxbrew/.linuxbrew` and declare `pathEntries: [bin]`:

```yaml
toolboxes:
  - image: registry.example.com/tools/brew-curl-jq@sha256:<digest>
    mountPath: /home/linuxbrew/.linuxbrew
    pathEntries: [bin]
```

Homebrew prefixes need glibc 2.38 or newer, which all four runtimes provide.
Known blockers: `git` cannot be built yet
([sozercan/dalec-homebrew#32](https://github.com/sozercan/dalec-homebrew/issues/32))
and Homebrew's own glibc cannot be bundled
([sozercan/dalec-homebrew#33](https://github.com/sozercan/dalec-homebrew/issues/33)).

## The `ai.orka.toolbox` image label

The label is optional and informational. Version 1 has this shape:

```json
{"version": 1, "mountPath": "/opt/yq-jq", "pathEntries": ["bin"]}
```

The Agent spec always wins over the label. Orka never reads image labels
when it validates or mounts a toolbox; a future `orka toolbox` command will
use the label to suggest Agent settings and pin digests.

## Troubleshooting

A toolbox failure sets the RuntimePool's `RolloutReady` condition to
`ToolboxUnavailable` and fails every Task waiting on that pool with the same
reason. The message carries one stable code:

| Reason | Meaning | Fix |
| --- | --- | --- |
| `TOOLBOX_IMAGE_PULL` | The toolbox image could not be pulled. | Check the digest, registry access, and `imagePullSecrets`. |
| `TOOLBOX_SOURCE_OPEN` | `mountPath` does not exist in the image, or a symlink sits somewhere in its path. | Build the image so the folder exists as a real directory at exactly `mountPath`. |
| `TOOLBOX_MISSING_PATH_ENTRY` | A `pathEntries` folder is missing or is not a real directory. | Fix the entry or the image layout. |
| `TOOLBOX_ARCH_MISMATCH` | An ELF file in a `pathEntries` folder is built for another CPU architecture. | Publish a multi-architecture image or pin toolbox Pods to matching nodes with `nodeSelector`. |
| `TOOLBOX_UNSUPPORTED_FILE_TYPE` | The toolbox contains a FIFO, socket, or device file. | Remove it; only folders, regular files, and symlinks are copied. |
| `TOOLBOX_TOO_DEEP`, `TOOLBOX_TOO_LARGE`, `TOOLBOX_FILE_TOO_LARGE`, `TOOLBOX_TOO_MANY_ENTRIES` | A copy limit was exceeded (48 folders deep, 2 GiB total, 512 MiB per file, 200,000 entries). | Trim the image; keep only the tools' folder. |
| `TOOLBOX_SOURCE_CHANGED` | A file changed while it was being copied. | Rebuild the image; a toolbox must be immutable. |
| `TOOLBOX_MOUNT_FAILED` | With `imageVolume`, the `subPath` does not exist in the image or the node runtime cannot mount image volumes. | Check the image layout and the containerd or CRI-O version on the selected nodes. |
| `TOOLBOX_MISSING_MOUNT` | The supervisor did not find the toolbox folder in the runtime Pod. | Check the node runtime and the RuntimePool events. |
| `TOOLBOX_COPY_FAILED` | Another I/O error during the copy. | See the init container logs (`toolbox-copy-<n>`) on the runtime Pod. |

`kubectl describe runtimepool <name>` shows the condition;
`kubectl get task <name> -o jsonpath='{.status.execution.message}'` shows the
Task-side reason.

## Limits

Not supported yet, and rejected with a clear message at admission and again
at planning:

- external runtimes (`runtime.runtimeRef`);
- harness v1 installations (`controller.mode: harness-v1`);
- Tasks with `spec.execution.workspace` (execution workspaces, including Agent
  Sandbox and Substrate).

Tools that need credentials or network access, such as `gh` or `kubectl`,
can be installed but cannot reach anything: runtime Pods have no internet
access and no Kubernetes permissions. Credentials and outbound access go
through [connectors](./connectors.md) and
[outbound access](../concepts/outbound-access.md); see the design issues
linked from the toolboxes epic for approvals and Kubernetes API access.
