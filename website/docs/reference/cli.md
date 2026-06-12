---
slug: /cli-reference
---

# CLI Reference

The `orka` CLI talks to the Orka controller REST API and is intended for day-to-day task inspection, resource CRUD, and operator workflows. It uses the same authentication and namespace rules as the API.

Build the CLI locally with:

```bash
make build-cli
bin/orka --help
```

Install or copy `bin/orka` wherever you keep developer tools if you want `orka` on your `PATH`.

## Connection and authentication

Most commands accept these global flags:

| Flag | Description |
| --- | --- |
| `--server`, `-s` | Orka API server URL. Defaults to `http://localhost:8080`. |
| `--namespace`, `-n` | Kubernetes namespace. Defaults to `default` unless config or kubeconfig sets one. |
| `--token`, `-t` | Bearer token for API authentication. Prefer config or kubeconfig over passing real tokens in shell history. |
| `--txn-token` | Kontxt transaction token sent with the `Txn-Token` header. |
| `--txn-token-file` | Read a Kontxt transaction token from a file, or `-` for stdin. |
| `--kubeconfig` | Kubeconfig path used for local discovery/token extraction fallback. |

The CLI reads persistent config from `~/.orka/config.yaml`:

```bash
orka config set-server http://127.0.0.1:8080
orka config set-token "$(kubectl create token orka-client -n orka-system)"
orka config view
```

`config view` masks the token. `config set-token` necessarily receives the token as a process argument, so use short-lived tokens and avoid pasting long-lived secrets into shell history.

Validate auth before running larger workflows:

```bash
orka auth validate
orka auth whoami -o json
```

## Output formats

Many read/list commands support `-o table`, `-o json`, and/or `-o yaml`. Prefer structured output for scripts:

```bash
orka task list -o json
orka provider list -o yaml
orka session get <session-id> -o json
```

Do not rely on full table layouts in automation; table output is optimized for people.

## Task workflows

Create tasks from manifests:

```bash
orka task create -f task.yaml
orka task wait my-task --timeout 5m
orka task result my-task
orka task logs my-task
orka task delete my-task
```

Create a simple container task from flags:

```bash
orka task create \
  --type container \
  --name hello-container \
  --image busybox:latest \
  --command sh \
  --command -c \
  --arg 'echo hello from orka'
```

Common task commands:

| Command | Purpose |
| --- | --- |
| `orka task create -f FILE` | Create a Task from YAML/JSON. |
| `orka task create --type container ...` | Create a Task directly from flags. |
| `orka task list [--status PHASE] [--transaction ID]` | List tasks, optionally client-side filtered. |
| `orka task get NAME [-o json|-o yaml]` | Read Task details. |
| `orka task wait NAME --timeout DURATION` | Wait for completion; exits nonzero for failed/cancelled tasks. |
| `orka task result NAME` | Print stored task result. |
| `orka task logs NAME` | Print completed task logs/result-store output, or live pod logs when available. |
| `orka task children NAME` | List child tasks. |
| `orka task plan NAME` | Read autonomous plan state. |
| `orka task artifacts NAME` | List task artifacts. |
| `orka task download NAME [filename] -o PATH` | Download task artifact content. |
| `orka task delete NAME` | Delete/cancel a task. |

## Chat and dashboard helpers

`orka run` is an Ollama-style chat interface backed by Orka chat/provider configuration:

```bash
orka run "explain this task failure"
orka run --session incident-123
orka run --agent reviewer "review this diff"
```

It may depend on live provider credentials, model configuration, and server-side chat support.

`orka login` creates a ServiceAccount token and opens the dashboard with that token in the URL fragment:

```bash
orka login --service-account orka-client --namespace orka-system
```

Do not use `login` in logs or shared terminals where the generated browser URL might be captured.

## Resource management commands

The CLI can create/read/list/update/delete the core resource types through the controller API:

| Resource | Typical commands |
| --- | --- |
| Providers | `orka provider create`, `get`, `list`, `update`, `delete` |
| Agents | `orka agent create`, `get`, `list`, `update`, `delete` |
| Tools | `orka tool create`, `get`, `list`, `update`, `delete` |
| Skills | `orka skill init`, `validate`, `import`, `content`, `get`, `list`, `update`, `delete` |
| Secrets | `orka secret list` (metadata only; no Secret data is printed) |

Examples:

```bash
orka provider create -f provider.yaml
orka agent list -o json
orka tool update my-tool -f tool-updated.yaml
orka skill init ./my-skill --name my-skill --description "My skill"
orka skill validate ./my-skill/SKILL.md
orka skill import ./my-skill/SKILL.md --name my-skill
orka secret list -o json
```

## Sessions and memory

Sessions and durable memory are store-backed workflows rather than Kubernetes CRDs.

```bash
orka session list -o json
orka session get <session-id> -o json
orka session delete <session-id>

orka memory create --content "stable project fact" --source cli --tags docs,cli
orka memory list -o json --query "project fact"
orka memory get <memory-id> -o json
orka memory disable <memory-id>
orka memory enable <memory-id>
orka memory update <memory-id> --content "updated fact"
orka memory delete <memory-id>
orka memory proposal list -o json
```

Memory governance is explicit: reviewing a memory proposal does not automatically create durable memory. Use `orka memory proposal apply ID` only for an accepted proposal that should become durable memory.

## Security scans and repository monitors

Repository security scan configuration:

```bash
orka security repo create -f repository-scan.yaml
orka security repo get my-repo -o json
orka security repo list -o json
orka security threat-model update my-repo --content "Threat model" --source cli
orka security threat-model get my-repo -o json
orka security scan list my-repo -o json
orka security finding list my-repo -o json
orka security slice list my-repo -o json
orka security dropped-findings list my-repo -o json
orka security repo delete my-repo
```

Repository monitor configuration:

```bash
orka monitor create -f repository-monitor.yaml
orka monitor get my-monitor -o json
orka monitor list -o json
orka monitor runs my-monitor -o json
orka monitor items my-monitor -o json
orka monitor events my-monitor -o json
orka monitor delete my-monitor
```

Manual run/action commands such as `orka security scan run`, `orka monitor run`, and finding patch/PR actions can create downstream Tasks and may require live GitHub, provider, and agent configuration.

## Substrate actor pools

Substrate actor pools are managed through the `substrate pool` command group:

```bash
orka substrate pool create -f pool.yaml
orka substrate pool get my-pool -o json
orka substrate pool list -o json
orka substrate pool update my-pool -f pool-updated.yaml
orka substrate pool delete my-pool
```

Pool manifests require at least `spec.templateRef.name`; pool reconciliation may depend on the configured Substrate environment.

## Shell completion

The CLI includes Cobra-generated shell completion. Generate the script for your shell with:

```bash
orka completion bash
orka completion zsh
orka completion fish
orka completion powershell
```

Install the generated script using your shell's standard completion path. Common examples:

```bash
# Bash, current session
source <(orka completion bash)

# Bash, user-local install on Linux
mkdir -p ~/.local/share/bash-completion/completions
orka completion bash > ~/.local/share/bash-completion/completions/orka

# Zsh, current session
source <(orka completion zsh)

# Zsh, user-local install
mkdir -p ~/.zsh/completions
orka completion zsh > ~/.zsh/completions/_orka
# Ensure ~/.zsh/completions is in fpath before compinit, for example:
# fpath=(~/.zsh/completions $fpath)
# autoload -Uz compinit && compinit

# Fish, user-local install
mkdir -p ~/.config/fish/completions
orka completion fish > ~/.config/fish/completions/orka.fish
```

Regenerate completions after upgrading `orka` if commands or flags change.

## Other utility commands

| Command | Purpose |
| --- | --- |
| `orka status` | Show health, readiness, task counts, and agent count. |
| `orka models list --compat openai` / `anthropic` | List model IDs in provider-compatible formats. |
| `orka workspace status TASK` | Inspect task workspace status. |
| `orka audit trace TRANSACTION_ID` | Show tasks correlated by Kontxt transaction ID. |
| `orka completion SHELL` | Generate shell completion scripts. |

## Binary e2e coverage matrix

Normal binary e2e tests build and invoke `bin/orka` directly with isolated config/home directories. They intentionally avoid real secrets in command arguments and assert stdout/stderr do not leak configured tokens or sentinel secret values.

| CLI area | Binary e2e status | Notes |
| --- | --- | --- |
| `auth` | Covered | `validate`, `whoami`; invalid-token negative path. |
| `models` | Covered | OpenAI and Anthropic compatibility list output. |
| `config` | Covered | Isolated `HOME`, `set-server`, fake `set-token`, masked `view`. |
| `status` | Covered | Health/readiness/task/agent summary. |
| `task` | Covered | Manifest create, flag create, list/filter, get, wait, result, logs, children, plan, artifacts, download, delete. |
| `workspace` | Covered | `workspace status`. |
| `provider` | Covered | CRUD and secret redaction expectations. |
| `agent` | Covered | CRUD. |
| `tool` | Covered | CRUD. |
| `skill` | Covered | init, validate, import, list, get, content, update, delete. |
| `secret` | Covered | Metadata-only list. |
| `audit` | Covered | Trace no-match path. |
| `session` | Covered | List/get/delete against a controlled fixture. |
| `memory` | Covered | Create/list/get/disable/enable/update/delete and proposal-list smoke. |
| `security` | Covered | Repository scan CRUD/read/list, threat model update/get, scan/finding/slice/dropped-finding list. |
| `monitor` | Covered | CRUD/read/list plus runs/items/events list. |
| `substrate` | Covered | Pool create/get/list/update/delete. |
| `run` | Deferred/live-gated | Depends on chat/SSE/provider fixtures. |
| `login` | Deferred | Browser-open behavior and token-bearing URL make normal e2e unsafe until a no-open/redacted test mode exists. |
| `completion` | Not custom-covered | Cobra-generated shell completion; add binary e2e only if project-specific completion behavior is added. |
| `security scan run` | Deferred/live-gated | Creates downstream scan Tasks and requires live agent/GitHub/provider setup. |
| `monitor run` | Deferred/live-gated | Creates downstream monitor work and can require GitHub/provider setup. |
| security finding actions | Deferred/live-gated | Need stable finding fixtures or live scan data. |

Use this matrix as a coverage guide when adding CLI commands: non-live command groups should have at least one compiled-binary e2e smoke path, and CRUD-style groups should cover create/get/list/update/delete where the API supports it.
