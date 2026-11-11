---
slug: /guides/orka-guide
description: "Ask a read-only, in-cluster guide questions about Orka, answered from the docs of the installed version."
---

# In-cluster guide

The Orka guide is an optional Agent that answers questions about Orka from the
docs of the version you installed. You ask it with `orka ask`. It reads a docs
snapshot and nothing else. It has no shell, workspace, credentials or cluster
access, so when something needs changing, it gives you the commands to run.

## How it works

- **The `orka-guide` Agent.** When the guide is enabled, the controller creates
  this Agent in its watch namespace and reverts any edits to it. The Agent
  carries an owner reference to the controller Deployment, so uninstalling Orka
  deletes it. When you disable the guide, the controller deletes the Agent. It
  never changes or deletes an `orka-guide` Agent it didn't create.
- **The docs snapshot.** A data-only image mounted read-only at `/opt/orka-docs`
  as a [toolbox](toolboxes.md). It holds:
  - `VERSION`: the release tag (or `dev`), commit and commit date.
  - The `orka-guide` skill, under `guide/`.
  - The docs, ADRs, API types, chart values and skills of that commit, under
    `src/`.

  The Agent's system prompt only points to the skill, so the instructions and
  the sources always come from the same version.
- **Read-only tools.** The guide has Read, Glob and Grep, and Bash is off. A
  question Task has no workspace, so its runtime session is read-only too.

Build the snapshot from the commit the controller runs, and the guide describes
the version that is installed.

## Enable it

The guide needs:

- the provider proxy (`providerProxy.enabled=true`), because built-in runtimes
  reach models only through it;
- the image of its runtime, `controller.acpRuntime.claudeImage` by default;
- toolboxes enabled, with an `allowedRegistries` entry that covers the snapshot
  image.

Release builds don't publish the snapshot image yet. Build and push it from the
tag you install:

```bash
git checkout <release-tag>
make docker-build-guide-docs docker-push-guide-docs \
  GUIDE_DOCS_IMG=<registry>/orka-guide-docs:<release-tag>
```

Then set the chart values, pinning the image by digest:

```yaml
providerProxy:
  enabled: true
controller:
  acpRuntime:
    toolboxes:
      enabled: true
      allowedRegistries:
        - <registry>
guide:
  enabled: true
  runtime: claude          # claude, codex or copilot
  model: <model-id>        # a model your provider proxy serves
  maxTurns: 30             # turn limit for one answer, 1-1000
  docs:
    image:
      repository: <registry>/orka-guide-docs
      digest: sha256:<digest>
```

OpenCode isn't supported, because its read-only sessions disable Grep. When a
value is missing or doesn't fit, the chart fails the render and names it. The
controller checks its `--guide-*` flags again when it starts and exits on an
invalid one. The flags are listed in [Configuration](../reference/configuration.md).

After the upgrade, check that the Agent exists and is ready:

```bash
kubectl get agent orka-guide -n <namespace>
```

## Ask

```bash
orka ask -n <namespace> "how do I mount a toolbox into an Agent?"
echo "which runtimes can mount toolboxes?" | orka ask -n <namespace> -
```

`orka ask` creates an agent Task for the `orka-guide` Agent and waits for it.
On a terminal it prints the answer as rendered Markdown.

- `--timeout` sets how long to wait. The default is 5 minutes. When the wait
  ends, the Task keeps running, and `orka task result <task>` reads the answer
  later.
- `--no-wait` prints the Task name and returns.
- `-o json` prints the Task, its phase and the answer.
- `--agent` asks another Agent.

Each question is an ordinary Task, so asking needs the same Orka API
permissions as any agent Task: `create` and `get` on `tasks` in
`core.orka.ai`, in the guide's namespace. With `get` on `agents` too,
`orka ask` can tell you when the guide isn't enabled. Each question starts a
runtime session and spends model tokens.

## Limits

- **One version.** The guide sees one version and no history. It can't tell
  you whether something is on main or in a newer release, and it can't search
  issues or PRs. It gives you a GitHub search link instead.
- **No cluster access.** For questions about your installation, it answers with
  the read-only commands that settle them.
- **No implementation code.** The snapshot has no Go code outside `api/`. When
  the docs and the types disagree, the guide says so.
- **Model answers.** Answers can be wrong. They cite the files they come from,
  so check the citations.
