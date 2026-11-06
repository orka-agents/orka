---
slug: /guides/runtime-skills
description: "Give built-in coding agents Agent Skills from digest-pinned images, copied into each runtime's native skills folder."
---

# Runtime skills

An [Agent Skill](https://agentskills.io) is a folder with a `SKILL.md` file (YAML
frontmatter plus instructions) and optional supporting files. Codex, Claude Code,
Copilot CLI, and OpenCode each discover skills in their own skills folder and load a
skill's instructions only when the task calls for it.

Codex, Claude, and OpenCode Agents list skills in `spec.runtime.skills`. Each skill comes from a
digest-pinned container image. Orka binds the skill folder read-only into the runtime
Pod with the same delivery as [toolboxes](./toolboxes.md), checks it, and copies it into
every session's native skills folder, owned by that session's own identity.

Native Agents (`type: ai` Tasks) keep using the [Skill](../reference/resources.md#skill)
resource and `spec.skills`; see [Agents, skills, and tools](./agents-skills-and-tools.md#add-skills).

## The rules

- **Skills are instructions, never code that runs on its own.** Nothing from a skill
  image runs: the copier is Orka's own binary, and the supervisor only reads files. A
  skill may ship scripts, but the agent can run them only through tools its policy
  already allows, as the session's unprivileged identity.
- **A skill can't widen the session's policy.** Frontmatter may contain only `name`,
  `description`, `license`, `compatibility`, and `metadata`. Keys such as
  `allowed-tools`, `hooks`, `model`, `context`, and `agent` are rejected, and so is the
  shell preprocessing marker `` !` `` that Claude Code would run when the skill loads.
- **`SKILL.md` is untrusted prompt input.** It must be UTF-8 text with frontmatter whose
  `name` equals the declared skill name, and it fails the same credential-shape check
  Orka applies to other prompt inputs if it appears to contain a secret.
- **Only folders and regular files.** Symlinks, devices, FIFOs, and sockets fail the
  skill. Limits: 256 entries, 4 MiB in total, 1 MiB per file, 8 folders deep, and 64 KiB
  for `SKILL.md`.
- **One skill per folder.** A nested `SKILL.md` (any letter case) is rejected, because
  Codex and OpenCode would load it as an extra skill that Orka never checked. So is
  `agents/openai.yaml`, which Codex reads for tool dependencies it may install.
- **Images are referenced by digest** and must come from a registry the operator
  allowed. Nothing is fetched over the network inside the runtime Pod.
- **Each session gets its own copy.** The read-only source is never writable by a
  session; the copy belongs to the session's unique UID and GID and is deleted with the
  session.
- **The feature is off by default.** A skill that can't be set up fails the Task with a
  stable reason; an agent never starts without the skills it was promised.

## Where each runtime finds skills

| Runtime | Native skills folder in the session | What else Orka sets |
| --- | --- | --- |
| Claude | `$CLAUDE_CONFIG_DIR/skills/<name>/` (`~/.claude/skills`) | The SDK `skills` option lists exactly the Orka skills, so Claude Code hides every other skill and its `Skill` tool rejects them. `disableSkillShellExecution` is on. With a restricted tool policy, Orka adds `Skill` to the allowed tools; it only loads skill instructions. |
| Codex | `$HOME/.agents/skills/<name>/` | Nothing: Codex always loads user skills. |
| OpenCode | `$OPENCODE_CONFIG_DIR/skills/<name>/` | The `skill` permission allows exactly the Orka skill names and denies everything else, including OpenCode's built-in skills. `external_directory` stays denied except for each Orka skill folder, so the `read` tool (when allowed) can open a skill's supporting files. |
| Copilot | — | Not supported. Copilot CLI in ACP mode keeps loading repository skills (`.github/skills`, `.agents/skills`, `.claude/skills`) whatever its settings say, so enabling its `skill` tool would also hand the model untrusted repository skills. Agents are rejected at admission. |

Codex discovers repository skills (`.agents/skills` in the workspace) on its own, with or
without Orka skills; Orka skills don't change that.

External runtimes (`runtime.runtimeRef`) and execution workspaces
(`Task.spec.execution.workspace`) can't receive skills yet; Agents and Tasks that combine
them are rejected at admission and planning.

## Operator setup

Skills reuse the toolbox delivery, so enable both and allow the registries that serve
skill images:

```yaml
controller:
  acpRuntime:
    toolboxes:
      enabled: true
      allowedRegistries:
        - registry.example.com/skills
      imagePullSecrets: []
      nodeSelector: {}
      mountMethod: copy               # copy | imageVolume
    skills:
      enabled: true
```

The controller flag is `--acp-skills-enabled` (`ORKA_ACP_SKILLS_ENABLED`). It requires
`--acp-toolboxes-enabled`, and skills use the toolbox allowlist, pull secrets, node
selector, and mount method. See [Toolboxes](./toolboxes.md#operator-setup) for those
settings and the trade-offs between `copy` and `imageVolume`.

## Agent YAML

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Agent
metadata:
  name: reviewer
spec:
  model:
    name: claude-sonnet-4-5
  runtime:
    type: claude
    skills:
      - name: code-review
        image: registry.example.com/skills/code-review@sha256:<digest>
      - name: release-notes
        image: registry.example.com/skills/bundle@sha256:<digest>
        path: /skills/bundle/release-notes   # optional; defaults to /skills/<name>
```

Validation, enforced by the CRD schema, Agent admission, Task planning, and again in the
runtime Pod:

- `name` uses lowercase letters, digits, and single hyphens (at most 64 characters) and
  must equal the `name` in the skill's `SKILL.md`. Names are unique per Agent.
- `image` is a fully qualified reference pinned by digest
  (`<registry>/<repository>@sha256:<64 hex>`), from an allowed registry.
- `path` is a clean folder under `/skills` inside the image, with no `.` or `..`
  components. It defaults to `/skills/<name>`. `SKILL.md` must sit directly in it.
- At most 8 skills per Agent.

Skills are part of the immutable runtime profile: the profile digest covers each skill's
name, image, and effective path, in order. Changing any of them creates a new
RuntimePool, and Tasks that are already running keep the skills frozen in their execution
snapshot.

## Build a skill image

```dockerfile
FROM scratch
COPY code-review/ /skills/code-review/
```

```bash
docker buildx build --platform linux/amd64,linux/arm64 -t registry.example.com/skills/code-review:1 --push .
crane digest registry.example.com/skills/code-review:1   # pin this digest in the Agent
```

A `FROM scratch` image works because nothing from the image runs. With the `copy` mount
method, folders must be readable and traversable by everyone (`o+rx`) and files readable
by everyone (`o+r`), because the copier runs as a fixed non-root identity. One image may
carry several skills under different `/skills/...` folders; declare each one with its own
`path`.

A minimal `SKILL.md`:

```markdown
---
name: code-review
description: Review a change for correctness, tests, and risky patterns. Use when asked to review a diff or pull request.
---

# Code review

1. Run `make test` and report failures first.
2. ...
```

## Troubleshooting

Skill failures surface through the same RuntimePool condition as toolbox failures:
`RolloutReady=False` with reason `ToolboxUnavailable`, and Tasks waiting on the pool fail
with the same reason. The message names the skill and carries one stable code.

| Code | Meaning | Fix |
| --- | --- | --- |
| `TOOLBOX_IMAGE_PULL`, `TOOLBOX_SOURCE_OPEN`, `TOOLBOX_MOUNT_FAILED`, … on a `skill-copy-<n>` container or `skill-<n>` volume | The skill image could not be pulled, or `path` does not exist as a real folder in it. | Check the digest, registry access, `imagePullSecrets`, and the image layout. |
| `SKILL_MISSING_MOUNT` | The supervisor did not find the skill folder in the runtime Pod. | Check the node runtime and the RuntimePool events. |
| `SKILL_MISSING_SKILL_MD` | `SKILL.md` is not directly in the skill folder. | Fix `path` or the image layout. |
| `SKILL_INVALID_FRONTMATTER` | The frontmatter is missing, malformed, has a `name` that does not match, has no `description`, or uses a key that is not allowed. | Keep only `name`, `description`, `license`, `compatibility`, and `metadata`. |
| `SKILL_UNSAFE_CONTENT` | `SKILL.md` is not UTF-8 text, contains `` !` ``, or appears to contain a secret; or the skill has a nested `SKILL.md` or `agents/openai.yaml`. | Remove the construct, credential, or file. |
| `SKILL_UNSUPPORTED_FILE_TYPE` | The skill contains a symlink, device, FIFO, or socket. | Ship only folders and regular files. |
| `SKILL_INVALID_FILE_NAME` | A file name contains a control character or is too long. | Rename the file. |
| `SKILL_TOO_DEEP`, `SKILL_TOO_LARGE`, `SKILL_FILE_TOO_LARGE`, `SKILL_TOO_MANY_ENTRIES` (or the `TOOLBOX_` equivalents from the copier) | A limit was exceeded. | Trim the skill. |

`kubectl describe runtimepool <name>` shows the condition;
`kubectl get task <name> -o jsonpath='{.status.execution.message}'` shows the Task-side
reason.
