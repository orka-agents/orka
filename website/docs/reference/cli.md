---
slug: /cli-reference
description: "The orka CLI: installing it, pointing it at a controller, and what it can do."
---

# CLI reference

The `orka` CLI talks to the Orka controller REST API and is intended for day-to-day task inspection, resource CRUD, and operator workflows. It uses the same authentication and namespace rules as the API.

## Installation

On macOS or Linux, if your installation runs the latest stable release, install the CLI
with [Homebrew](https://brew.sh). Use the fully qualified name; `brew install orka`
resolves to an unrelated cask:

```bash
brew install orka-agents/tap/orka
orka version
```

The formula follows the latest stable release, possibly a few hours after it is published,
and installs shell completions. Check `orka version` after installing; for any other
version, use the release archives below.

Download the CLI from [GitHub Releases](https://github.com/orka-agents/orka/releases).
Choose a published release that matches your installed Helm chart and controller version.
The `v0.3.0` value below is an example. Replace it with your installation's release tag,
including the leading `v`.

Select the archive for the machine where you will run the CLI, not the cluster nodes:

| Platform | Archive |
| --- | --- |
| Linux, x86-64 | `orka_<version>_linux_amd64.tar.gz` |
| Linux, ARM64 | `orka_<version>_linux_arm64.tar.gz` |
| macOS, Intel | `orka_<version>_darwin_amd64.tar.gz` |
| macOS, Apple silicon | `orka_<version>_darwin_arm64.tar.gz` |
| Windows, x86-64 | `orka_<version>_windows_amd64.zip` |

Each archive contains `orka` or `orka.exe`, `LICENSE`, and `README`.

On Linux or macOS, start in an empty directory and set `OS` and `ARCH` for your machine:

```bash
VERSION=v0.3.0 # Example only; replace with your installed Orka release.
OS=linux      # linux or darwin
ARCH=amd64    # amd64 or arm64
ARCHIVE="orka_${VERSION}_${OS}_${ARCH}.tar.gz"
CHECKSUMS="orka_${VERSION}_checksums.txt"
RELEASE_URL="https://github.com/orka-agents/orka/releases/download/${VERSION}"

curl -fsSLO "${RELEASE_URL}/${ARCHIVE}"
curl -fsSLO "${RELEASE_URL}/${CHECKSUMS}"
```

To also authenticate the checksum file, download its keyless signature bundle and
verify it with [Cosign](https://docs.sigstore.dev/cosign/verifying/verify/).
The signer must be the matching `release-X.Y` branch of Orka's release workflow:

```bash
RELEASE_BRANCH="release-$(printf '%s' "${VERSION#v}" | cut -d. -f1,2)"
curl -fsSLO "${RELEASE_URL}/${CHECKSUMS}.bundle"
cosign verify-blob \
  --bundle "${CHECKSUMS}.bundle" \
  --certificate-identity "https://github.com/orka-agents/orka/.github/workflows/release.yml@refs/heads/${RELEASE_BRANCH}" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
  "$CHECKSUMS"
```

Check the selected archive's SHA-256 checksum before extracting it. On Linux:

```bash
set -o pipefail
grep "  ${ARCHIVE}$" "$CHECKSUMS" | sha256sum -c -
```

On macOS:

```bash
set -o pipefail
grep "  ${ARCHIVE}$" "$CHECKSUMS" | shasum -a 256 -c -
```

Continue only if the checksum check reports `OK` and any signature verification you
ran succeeds. Then extract the archive and install the binary on your `PATH`:

```bash
tar -xzf "$ARCHIVE"
mkdir -p "$HOME/.local/bin"
install -m 0755 orka "$HOME/.local/bin/orka"
export PATH="$HOME/.local/bin:$PATH"
orka version
orka --help
```

Keep `$HOME/.local/bin` on your `PATH` in new terminals by adding the export to your
shell's startup file.

On Windows, download the `windows_amd64.zip` archive and checksum file for the same
version. In PowerShell, run `Get-FileHash <archive> -Algorithm SHA256` and compare its
hash with that archive's entry in the checksum file before extracting. Put `orka.exe`
in a directory on your `PATH`, then run `orka version`.

`orka version` reports the local CLI version. Confirm that it matches your installed
Helm/controller release before using the CLI.

### Building from source

With the [Go toolchain](../development/development.md#prerequisites) installed, run
these commands from the root of an Orka source checkout:

```bash
make build-cli
bin/orka --help
```

Install or copy `bin/orka` wherever you keep developer tools if you want `orka` on your `PATH`. For exhaustive flag and subcommand help generated from Cobra, see [CLI Command Reference](./cli-commands.md).

## Connection and authentication

Most commands accept these global flags:

| Flag | Description |
| --- | --- |
| `--server`, `-s` | Orka API server URL. Defaults to `http://localhost:8080`. |
| `--namespace`, `-n` | Kubernetes namespace. Defaults to `default` unless config or kubeconfig sets one. |
| `--token`, `-t` | Bearer token for API authentication. Prefer config or kubeconfig over passing real tokens in shell history. |
| `--txn-token` | Transaction token sent with the `Txn-Token` header. |
| `--txn-token-file` | Read a transaction token from a file, or `-` for stdin. |
| `--kubeconfig` | Kubeconfig path used for local discovery/token extraction fallback. |

The CLI reads persistent config from `~/.orka/config.yaml`:

```bash
orka config set-server http://127.0.0.1:8080
orka config set-namespace orka-system
kubectl create token orka-client -n orka-system | orka config set-token --file -
orka config view
```

`config view` masks the token. Prefer `config set-token --file <path>` or `--file -` over passing real tokens as process arguments. If you use `config set-token <token>` directly, use short-lived tokens and avoid pasting long-lived secrets into shell history.

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
orka session get SESSION_ID -o json
orka task artifacts example-task -o json
```

Do not rely on full table layouts in automation; table output is optimized for people. `orka task artifacts` keeps the table as its default and accepts `-o json` or `-o yaml` when a script needs artifact metadata (filename, content type, byte size, and creation time).

Commands that show one object (`orka task get`, `orka agent get`, `orka provider get`,
`orka security finding get`, `orka memory proposal get`, `orka tool get`, and the rest)
print a short readable view by default: one field per line, the important fields first,
long text wrapped to the terminal, empty fields omitted. `-o json` and `-o yaml` print the
full API object for scripts.

```console
$ orka security finding get fnd_a9d4f27383dc
Title:      Zip-slip via AdmZip.extractAllTo on POST /import
Severity:   critical
Validation: validated
State:      open
Location:   routes/import.js:42
Summary:    Archive entries are extracted without checking for path traversal, so a
            crafted archive can write outside the upload directory.
Category:   path-traversal
Confidence: high
Repository: nodejs-goof
ID:         fnd_a9d4f27383dc
```

`orka security threat-model get` prints the Markdown document as text under a short
header, and `orka workspace status` and `orka auth whoami` print one field per line.

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

Create an ACP agent task with explicit read-only workspace intent:

```bash
orka task create "Inspect the repository" \
  --type agent \
  --agent codex-reviewer \
  --workspace-intent read \
  --git-repo https://github.com/example/project \
  --read-credential repository-read
```

Create a write-intent task with independently scoped publication credentials and pull-request reconciliation:

```bash
orka task create "Implement the requested change" \
  --type agent \
  --agent codex-builder \
  --workspace-intent write \
  --git-repo https://github.com/example/project \
  --read-credential repository-read \
  --publication-git-repo https://github.com/example/project \
  --publication-read-credential repository-publication-read \
  --publication-credential repository-publish \
  --forge-credential repository-forge \
  --pr-base-branch main \
  --create-pr
```

The four roles are independent: source read for clone, target read for
preflight/verification, target write for the exact branch update, and forge for
PR reconciliation. Values are brokered only to the clean-room Publisher and
are never delivered to the ACP process tree.

Common task commands:

| Command | Purpose |
| --- | --- |
| `orka task create -f FILE` | Create a Task from YAML/JSON. |
| `orka task create --type container ...` | Create a Task directly from flags. |
| `orka task list [-l SELECTOR] [--since 10m] [--status PHASE] [--watch]` | List tasks oldest first with an `AGENT` column; filter by label, start time, or phase; `--watch` reprints on change. |
| `orka task get NAME [-o json|-o yaml]` | Show a Task's phase, agent, delivery, and result; `-o json` for the full object. |
| `orka task status NAME [--verbose]` | Show whether the Task finished and where the change went; `--verbose` adds runtime-pool details. |
| `orka task events NAME [--type TYPE] [--tail N] [--wide]` | List execution events; `--tail 1` shows the last one, `--wide` prints full messages. |
| `orka task approvals NAME [ID] [--wide]` | Show what each approval request asks for; pass an ID to see every argument. |
| `orka task approvals NAME --watch [--timeout DURATION]` | Wait until the Task has a pending request, then print it; exits nonzero if the Task finishes first. |
| `orka task approve NAME ID --reason TEXT` / `decline` | Decide a request; `ID` may be a unique prefix of the short or full ID. |
| `orka task wait NAME --timeout DURATION` | Wait for completion; exits nonzero for failed/cancelled tasks. |
| `orka task result NAME` | Print stored task result. |
| `orka task logs NAME` | Print completed task logs/result-store output, or live pod logs when available. |
| `orka task children NAME` | List child tasks. |
| `orka task plan NAME` | Read autonomous plan state. |
| `orka task artifacts NAME` | List task artifacts. |
| `orka task download NAME [FILENAME] --output PATH` | Download task artifact content. |
| `orka task delete NAME` | Delete/cancel a task. |

### Following tasks

`orka task list` shows who is running each Task and reprints the table while Tasks
progress:

```console
$ orka task list -l orka.ai/source=anthropic-proxy --since 10m --watch
NAME            TYPE       AGENT        STATUS     AGE
proxy-78055b2e  agent      coder        Running    12s
proxy-df604959  agent      coder        Running    10s
proxy-9a3b0c88  container  golang:1.27  Pending    2s
```

`-l` uses `kubectl` label selector syntax (`key=value`, `key!=value`, `key in (a,b)`,
comma-separated) and is applied by the server. `--since` accepts a duration (`10m`, `2h`)
or an RFC 3339 timestamp and keeps Tasks created after that point. Both work with
`--watch`, which exits on Ctrl-C. Orka labels the Tasks it creates for you:

| Label | Set on |
| --- | --- |
| `orka.ai/source=anthropic-proxy` | Tasks created through the Anthropic-compatible API, for example from Claude Code |
| `orka.ai/security-target=<repository>` | Tasks created by a security scan of that repository |
| `gateway.orka.ai/gateway=<gateway>` | Tasks created from a message that came in through a gateway |

`orka task status` answers "did it finish, and where did the change go?" in a few rows.
Delivery rows appear for write-intent workspaces, and a failed Task shows its reason:

```console
$ orka task status proxy-78055b2e
FIELD               VALUE
Task                proxy-78055b2e
Phase               Succeeded
Delivery            VerifiedExact
Publication branch  orka/add-healthz-endpoint-a7f2b1e9
```

`--verbose` prints the full table with the execution state, attempt, RuntimePool, runtime
instance, session generation, delivery message, and verified remote commit.

To see what an agent said last, filter events by type and keep the last one. Rows are cut
to the terminal width; `--wide` prints the whole message and `-o json` is never truncated.
`orka task events --help` lists the event type names `--type` accepts.

```console
$ orka task events proxy-78055b2e --type ModelMessage --tail 1
SEQ  TYPE          SEVERITY  SUMMARY
418  ModelMessage  info      Added GET /healthz returning 200 with the build version, plus a…
```

Approval requests show the tool and its arguments so a reviewer can see what they are
approving. IDs are shortened to 12 characters; `approve` and `decline` accept any unique
prefix and send the full ID to the server:

```console
$ orka task approvals fibey-0923
ID            STATUS   TOOL               ARGUMENTS                                                 EXPIRES
8a8d1a7d418d  pending  create-work-order  asset=pump-1 summary="Inspect the pressure transmitter."  in 9m

$ orka task approvals fibey-0923 8a8d1a7d418d      # every argument on its own line
$ orka task approve fibey-0923 8a8d1a7d418d --reason "Inspect the transmitter."
```

`--wide` adds severity, the risk summary, and who decided and why.

A Task that is waiting for approval is paused, not finished, so `orka task wait` does not
return at that moment. `--watch` does: it polls the approvals list (`--interval`, default
5s) until a request is pending, prints the list, and exits 0. It exits nonzero if the Task
finishes first, naming the phase it ended in, or when `--timeout` elapses. `-o json` prints
the approvals response instead of the table, and Ctrl-C stops the wait cleanly:

```console
$ orka task approvals fibey-0923 --watch
Waiting for an approval request...
ID            STATUS   TOOL               ARGUMENTS                                                 EXPIRES
8a8d1a7d418d  pending  create-work-order  asset=pump-1 summary="Inspect the pressure transmitter."  in 9m
```

## Chat and dashboard helpers

`orka run` is a chat interface backed by Orka chat and provider configuration:

```bash
orka run "explain this task failure"
orka run --session incident-123
orka run --agent reviewer "review this diff"
```

It may depend on live provider credentials, model configuration, and server-side chat support.

`orka dashboard` creates a ServiceAccount token, or uses `--token`, and opens the dashboard signed in with it, passing the token in the URL fragment. It does not save the token or sign the CLI in; other commands authenticate on their own. It needs a controller that serves the dashboard (`--dashboard-enabled`); against one that does not, it stops before creating a token:

```bash
orka dashboard --service-account orka-client --namespace orka-system
```

The printed URL shows `<redacted>` in place of the token, while the browser still gets the full URL. `--no-open` skips the browser launch. When no browser can open, such as over SSH, `--show-token` prints the full URL so you can copy it:

```bash
orka dashboard \
  --service-account orka-client \
  --namespace orka-system \
  --no-open \
  --show-token
```

Use `--show-token` only in a trusted terminal: logs, CI output, and shared sessions would capture the token.

## Linked accounts

`orka connect <provider>` links one of your own accounts (see the
[Connectors guide](../guides/connectors.md)) as the signed-in person: it starts
the consent flow, opens the provider's consent page, and waits until the link is
ready. `orka connection` lists, shows, and disconnects your links and shows the
providers an operator made available:

```bash
orka connect github --mode readWrite --token "$OIDC_TOKEN"
orka connection list
orka connection get github-<digest>
orka connection delete github-<digest>
orka connection providers
orka connection complete github-<digest> --completion <value>   # when no dashboard signed in as you finishes the link
```

These commands need a personal identity (OIDC or context token). A
ServiceAccount token, such as the one `orka dashboard` creates, has no accounts to
link; the CLI explains the resulting `403`. `--no-open` prints the consent URL
without opening a browser and `--no-wait` returns once consent has started.

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

`orka session migrate import` stages a stopped Codex thread for a fresh Orka
Session. `orka session migrate export` installs a saved checkpoint in a fresh
local home. Both require a private journal for retries and support Codex 0.160.0
paginated sessions on Linux and macOS. See
[Codex session migration](../guides/codex-session-migration.md).

```bash
orka session list -o json
orka session get SESSION_ID -o json
orka session delete SESSION_ID

orka memory create --content "stable project fact" --source cli --tags docs,cli
orka memory list -o json --query "project fact"
orka memory get MEMORY_ID -o json
orka memory disable MEMORY_ID
orka memory enable MEMORY_ID
orka memory update MEMORY_ID --content "updated fact"
orka memory delete MEMORY_ID
orka memory proposal list -o json
orka memory proposal get PROPOSAL_ID -o json
orka memory proposal review PROPOSAL_ID --status accepted --note "Matches the operations note."
orka memory proposal apply PROPOSAL_ID
orka memory proposal archive PROPOSAL_ID
```

Memory governance is explicit: reviewing a memory proposal does not automatically create durable memory. Use `orka memory proposal apply PROPOSAL_ID` only for an accepted proposal that should become durable memory.

The server records your signed-in identity as the reviewer and the applier; neither can be set from the CLI. `review` prints the recorded status and reviewer, and `apply` prints the new memory ID and who applied the proposal.

## Security scans and repository monitors

Repository security scan configuration:

```bash
orka security repo create -f repository-scan.yaml
orka security repo get my-repo
orka security repo list
orka security threat-model update my-repo --content "Threat model" --source cli
orka security threat-model get my-repo            # prints the Markdown as text
orka security scan run my-repo
orka security scan status my-repo --watch         # stage-by-stage progress; exits when the scan ends
orka security scan list my-repo                   # phase, slices reviewed, findings kept and dropped
orka security finding list my-repo --recommended  # severity, validation, id, title, file
orka security finding get FINDING_ID
orka security slice list my-repo -o json
orka security dropped-findings list my-repo --layer filter --reason contains=rate-limit
orka security repo delete my-repo
```

The findings table is sorted critical-first, then validated before unvalidated:

```console
$ orka security finding list nodejs-goof --recommended
SEVERITY  VALIDATED  ID                TITLE                                                  FILE
critical  yes        fnd_a9d4f27383dc  Zip-slip via AdmZip.extractAllTo on POST /import       routes/import.js:42
critical  yes        fnd_4ceb0dc790e6  Unauthenticated command injection via exec('identify…  routes/index.js:118
high      no         fnd_11a364071e0b  Hard-coded express-session secret enables cookie for…  app.js:31
```

`orka security scan status` shows the latest run (or `--scan ID`) with one row per
pipeline stage, and lists failed Tasks by name; with `--watch` it exits 0 when the scan
succeeds and 1 when it fails, so scripts can wait on it. See the
[security scanning guide](../guides/repository-security-scanning.md#following-a-scan-from-the-cli).

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

Manual run and action commands such as `orka security scan run`, `orka monitor run`, and finding patch and pull request actions create downstream Tasks, and need working GitHub, provider, and agent configuration.

## Usage and PR outcomes

Usage commands read retained model measurements and verified PR outcomes through
the controller API. Each installation reports only its own Kubernetes namespace;
reporting requires no CRD.

```bash
orka usage summary -n payments
orka usage summary --teams payments --repository example/project -o json
orka usage work WORK_ID -n payments
orka usage other review_only -n payments -o yaml
```

`summary` shows full-selection totals and a page of work IDs. `work` loads one
work request's Tasks, sessions, measurements, and PR outcomes. `other` inspects
`review_only`, `other_requests`, or `unassociated` usage.

All three commands support `--from`, `--until`, `--as-of`, `--teams`,
`--repository`, `--model`, `--kind issue|pull_request`, and `-o table|json|yaml`.
Dates accept UTC dates or RFC3339 timestamps, and `--until` is exclusive. Summary
and other-usage commands default to the current UTC month; work details default
to all retained history. A model filter selects whole requests, including their
other models' usage.

Summary and other-usage commands support `--limit` and `--offset`, with a default
page size of 25 and an API cap of 100. Copy the report's `--as-of` timestamp and
reuse the same filters for later pages and work details. Paging does not change
aggregate totals.

Tables distinguish missing measurements from reported zero and include coverage
and cache availability. JSON and YAML preserve the full response, including
nulls. See [Usage and PR outcomes](../guides/usage-and-pr-outcomes.md) for examples,
retention, and access requirements.

## ACP runtime management

Controller-owned pools and external v2 registrations use separate command groups:

```bash
orka runtime-pool list
orka runtime-pool get codex-read -o yaml

orka agent-runtime list
orka agent-runtime get external-codex -o yaml
orka agent-runtime create -f agent-runtime.yaml
orka agent-runtime update external-codex -f agent-runtime.yaml
orka agent-runtime delete external-codex
```

RuntimePool commands are read-only because pools are controller-owned. Table output reports lifecycle, admission, Pod count, resident-session capacity, prompt capacity, and queued demand. AgentRuntime table output reports only the `orka.harness.v2` identity/profile surface; legacy continuation and turn-capability fields are not rendered.

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

Several flags offer value completion, so Tab suggests the valid choices without consulting the server:

```bash
orka task list --output <TAB>   # table, json, yaml
orka task create --type <TAB>   # ai, container, agent
orka task list --status <TAB>   # Pending, Running, Finalizing, ...
```

`task download --output` is left out on purpose: there the flag names a destination file, so completion keeps suggesting files.

## Other utility commands

| Command | Purpose |
| --- | --- |
| `orka status` | Show health, readiness, task counts, and agent count. |
| `orka models list --compat openai` / `anthropic` | List model IDs in provider-compatible formats. |
| `orka workspace status TASK` | Inspect canonical workspace policy and delivery status without credential references, one field per line. |
| `orka task status TASK [--verbose]` | Show whether a Task finished and where the change went; `--verbose` adds RuntimePool identity and publication verification. |
| `orka audit trace TRANSACTION_ID` | Show Tasks correlated by [transaction](../concepts/transaction-tokens.md) ID. |
