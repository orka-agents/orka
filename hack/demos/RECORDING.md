# Demo recording design

This is the design doc for turning `hack/demos/` from a presenter rehearsal kit
into a small, tasteful library of recorded terminal demos. The goal is to
publish a handful of artifacts that hold up on the README, in the docs, and on
social timelines, while keeping the same auditable shell scripts as the
underlying source of truth.

Status: design ready for implementation. Phases 1, 2, and 3 (see §11.5)
can begin without resolving any [Open questions](#open-questions). Phase 4
requires resolving Q1 (default request preset) and Q2 (committed artifact
format). Q3 and Q4 are decided: both kontxt and agent-sandbox are
provisioned by a shared `make demo-cluster-up` kind target built in Phase
1. Q5 is dissolved by §11.5 — both polish and infra get done, in that
order.

---

## 1. Goals and non-goals

**Goals**

- Produce a small, named set of terminal recordings that show Orka doing one
  legible thing per artifact.
- Reuse the existing `hack/demos/*.sh` scripts as the recording source rather
  than maintaining a second, parallel "demo for the camera" tree.
- Add two new scenarios — `kontxt` and `agent-sandbox` — alongside the four
  existing ones, recorded the same way.
- Keep every recording deterministic enough that a re-record after a code
  change produces visually similar output.

**Non-goals**

- No UI screen captures. The UI is great, but the user wants terminal-only.
  When the demo wants to point at the UI, it does so via a one-line URL hint,
  not a screenshot.
- No live agent screencasts. Everything is asciinema + `agg`. No OBS, no Loom,
  no narrated voice-over.
- No new product features. This is a presentation-layer effort only.
- No edits to the workflow contracts (`assert_real_pr_result`,
  `summarize_task_run`, the API verbs). Those are correctness checks; the
  recordings sit on top of them.

---

## 2. Audience and surface map

Every recording is built for exactly one of three placements. That placement
dictates length, pacing, and what gets cut.

| Surface | Length | Pacing | Cut ruthlessly |
|---|---|---|---|
| `README` hero | 45–75 s | Fast, one beat per second | Anything that isn't "chat → PR URL" |
| Docs page embed | 2–4 min | Presenter pace | Skip rehearsal, keep narration cues |
| Social post (X, LinkedIn) | 30–60 s | Punchy, single payoff | Skip preflight, skip cleanup, single chapter |

Each script in `hack/demos/` produces **one recording per surface** by toggling
a `DEMO_RECORD_PROFILE` env var (see [§4](#4-recording-stack)). The same
script, run three different ways, yields the README cast, the docs cast, and
the social cast — instead of three near-identical script copies.

---

## 3. Scenario inventory

Six demos total. Four exist; two are new.

| # | Name | Script | Status | Headline |
|---|------|--------|--------|----------|
| 10 | Chat-to-PR | `10-chat-pr.sh` | exists, needs polish | One chat turn → coordinator → coder + reviewer → CI → PR |
| 20 | YAML workflow | `20-manual-workflow.sh` | exists, needs polish | Same payload, declarative `Task` CR — GitOps-friendly |
| 30 | Scheduled workflow | `30-cron-workflow.sh` | exists, needs polish | Cron-scheduled stale-PR triage report |
| 40 | Security remediation | `40-security-scanning.sh` | exists, needs polish | Finding → patch proposal → reviewable PR |
| 50 | **Kontxt transaction tokens** | `50-kontxt.sh` | **new** | Caller Pod proves identity → kontxt mints TxToken → Orka stamps immutable provenance |
| 60 | **Agent sandbox workspaces** | `60-agent-sandbox.sh` | **new** | One session, two agents, three turns — Scout, Builder, and a CI fixup share one warm sandbox |
| 70 | **Agent Substrate workspaces** | `70-agent-substrate.sh` | **new** | Real gpt-5.4 agent in a gVisor Actor clones + edits + opens a PR; warm reuse with no cold start |

Demos 50 and 60 are designed in [§7](#7-new-scenario-storyboards).

The README hero is **always Demo 10**. Docs pages embed the matching demo
(`docs/chat.md` → 10, `docs/anthropic-compat.md` → 10, `docs/kontxt.md` → 50,
`docs/agent-sandbox.md` → 60, etc.). Social posts pick whichever demo we're
launching that week.

---

## 4. Recording stack

We standardize on:

- **`asciinema rec`** captures the terminal session deterministically. Output
  is a `.cast` (asciicast v2) file: tiny, diffable, replayable.
- **`agg`** renders the cast to GIF and SVG. GIFs go on the README and social;
  the SVG embeds in docs pages without bandwidth pain.
- **`gum`** (`charmbracelet/gum`) styles the in-script "card" output — payoff
  panels, chapter markers, framed PR URLs.
- **`glow`** renders prompt files (`chat-request.txt`, `manual-story.txt`)
  with markdown styling instead of raw `cat`.

None of these tools are dependencies for *running* the demos; they're only
required to *record* them. The existing scripts must keep working under bare
`bash + kubectl + curl + jq` for presenter use.

### `DEMO_RECORD_PROFILE`

A single env var selects pacing, output style, and which beats fire.

| Profile | Used for | Effect |
|---|---|---|
| `presenter` (default) | Live presenting, today's behavior | `pe` typewriter on, full transparency blocks, all chapters |
| `docs` | Docs-embed cast | Typewriter off for `kubectl`/`jq`, transparency collapsed to one-line file path, narration cues printed as `> ...` |
| `social` | Short cast | Skip preflight, skip cleanup, hard-cap at three chapters, single payoff card |
| `hero` | README hero | Like `social` but caps at 60 s of real time by pre-warming everything and replaying compressed phases |

Profiles are implemented in `lib/common.sh` as a tiny dispatcher around the
existing `p`/`pe` calls; scripts don't fork.

### Recording wrapper

A new `hack/demos/record.sh <demo-number> <profile>` runs:

```bash
asciinema rec \
  --overwrite \
  --idle-time-limit 1.5 \
  --cols 110 --rows 30 \
  --title "Orka — ${title}" \
  --command "DEMO_RECORD_PROFILE=${profile} ./${script}" \
  out/${demo}-${profile}.cast

agg --cols 110 --rows 30 \
  --theme github-dark \
  --font-family "JetBrains Mono" \
  --speed 1.4 \
  out/${demo}-${profile}.cast out/${demo}-${profile}.gif
```

Notes on the choices:

- `--idle-time-limit 1.5` is the single highest-leverage flag in the whole
  pipeline. It silently fast-forwards the dead air during `wait_for_*`. The
  audience sees the workflow advance every second instead of watching nothing
  for 90 s.
- `--cols 110 --rows 30` is chosen to fit a GitHub README image without
  horizontal scroll. The narrowest line is the framed payoff card; everything
  else either fits or gets shortened to fit.
- `--speed 1.4` slightly accelerates playback. Real-time pacing reads as slow
  on a recording; humans expect demos to be a beat faster than life.

---

## 5. Visual style guide

The point of this section is that *every* demo looks like it came from the
same hand.

### Prompt

```
orka ❯
```

Set via `DEMO_PROMPT="${BOLD}orka${COLOR_RESET} ❯ "`. Drops the redundant
`\W` working-directory segment (demos don't `cd`). The chevron reads better
than `$` for non-engineers.

### Color palette

Three colors, used consistently:

- **Cyan** for *commands the audience runs.*
- **Green** for *successful state* (Task `Succeeded`, PR `merged`, scope
  allowed).
- **Magenta** for *names the audience should remember* (Task names, PR URLs,
  transaction IDs).

Anything outside this palette is either inherited from `kubectl`/`jq` or a
mistake.

### Chapter markers

Each script gains `chapter "Watch the coordinator delegate"` calls that print:

```
━━━ Chapter 2/4 — Watch the coordinator delegate ━━━
```

`agg` renders these as visually obvious dividers, which means the resulting
GIF has clean "moments" that translate to YouTube/Loom chapter markers if we
later cross-post. Also makes the demos skimmable when the viewer pauses.

### Payoff cards

The single biggest taste fix. Replace this:

```
orka ❯ orka_api GET ".../result?namespace=demo-magic" | jq '{result:.result}'
{"result":"Implementation succeeded. PR: https://github.com/.../pull/131 ..."}
```

with this, via `gum style --border rounded --padding "1 3" --foreground 212`:

```
╭─ Pull request opened ───────────────────────────────────────╮
│                                                             │
│  sozercan/vekil #131                                        │
│  Add Prometheus /metrics endpoint                           │
│                                                             │
│  +428 −12 · 4 child tasks · 1 review cycle · CI green       │
│  https://github.com/sozercan/vekil/pull/131                 │
│                                                             │
╰─────────────────────────────────────────────────────────────╯
```

Implementation: a `payoff_card pr_url children review_cycles ci_status`
helper in `lib/common.sh` that wraps `gum style`. Every demo ends on one.

### Narration cues

In `docs` profile only, each chapter prints a single `>`-prefixed sentence
before the command:

```
> Notice the coordinator hasn't started yet — Orka first creates the agents the request asked for.
orka ❯ kubectl get agents -n demo-magic ...
```

This gives the human re-recording a demo something to read aloud over the GIF
on social, even though the GIF itself has no audio.

### What gets cut

- Multiline `jq` projections in `pe` calls. Replace with `jq -c` and a single
  field, or a `payoff_card`.
- Stringified JSON results dumped to screen. Use `payoff_card`.
- `printf 'client=%s\\nendpoint=%s...'` setup blocks. Replace with a single
  `gum style --foreground 6 "Connecting to ${endpoint} as ${model}"` line.
- The full `transparency mode` cat-the-prompt block. Keep the file on disk,
  print the path; show the first eight lines via `glow` instead of all 90.

---

## 5.5. Helper API reference

This section pins the contract for every helper §6 and §7 invoke. New helpers
land in `lib/style.sh` (visual) or `lib/common.sh` (data + assertions); both
files are sourced by every script via the existing top-of-script include.

All helpers MUST:

- Print to stdout (so asciinema captures them). Diagnostics go to stderr.
- Exit non-zero on failure with a one-line `>&2` error so the recording
  fails fast — the wrapper aborts the cast on first non-zero exit.
- Be no-ops in `presenter` profile when their purpose is purely cosmetic
  (chapter dividers are kept; payoff cards are kept; narration cues are
  presenter-only suppressed).
- Tolerate missing optional inputs (`""`, `0`) by printing a hyphen rather
  than erroring.

### `chapter` — section divider

```
chapter "<title>"
```

Auto-numbers within a single script run by reading a private
`__DEMO_CHAPTER_INDEX` counter, and auto-computes the total by reading the
script-set `DEMO_CHAPTER_TOTAL` (default 7). Prints:

```
━━━ Chapter <n>/<N> — <title> ━━━
```

In `social` profile, suppress all chapters past index 3. In `hero` profile,
suppress chapters entirely (one continuous arc). In `docs` profile, also
emit the `>` narration line registered for the current chapter (see
`narrate`).

Exit codes: always 0 unless the underlying `echo` fails.

### `narrate` — docs-profile narration

```
narrate "<one-sentence cue>"
```

Records the cue against the *next* `chapter` call. In `docs` profile, the
cue is printed as `> <cue>` immediately after the chapter divider. In all
other profiles it is silently discarded so presenter rehearsals stay
uncluttered.

### `payoff_card` — generic PR-result card

```
payoff_card --title "<header>" \
            --pr <url> \
            --children <int> \
            --reviews <int> \
            --ci <green|red|pending|->
```

Flag-style invocation so callers don't have to memorize positional order.
Internally calls `gum style --border rounded --padding "1 3" --foreground 212`
on the rendered body. Wraps lines to fit the 110-column cast width.

Exit codes: 1 if `--pr` is empty (every payoff requires a URL); 0 otherwise.

### `payoff_card_kontxt` — kontxt scenario card

```
payoff_card_kontxt --task <name> --denied-job <name>
```

Reads the safe transaction ID off `task/<name>` annotation
`orka.ai/transaction-id`, reads the denied caller's printed transaction ID
out of `job/<name>` logs (grep `^denied .* transactionID=`), and renders the
two-row "one identity, two outcomes" card sketched in §7. Hard-asserts both
IDs are present and distinct; exit 1 on mismatch with a stderr explanation.

Never reads the raw `Txn-Token` from anywhere — only the safe
`transaction-id` digest. Enforced by a `grep -v` guard in the helper itself.

### `payoff_card_sandbox` — sandbox scenario card

```
payoff_card_sandbox --session <name> \
                    --turns demo-scout-turn-1,demo-builder-turn-2,demo-builder-turn-3
```

For each Task in `--turns`:
1. Read `status.startedAt` and `status.finishedAt` to compute duration.
2. Read the claim name from the worker Pod logs by grepping
   `'completed in sandbox workspace [a-z0-9-]+'` and taking the last
   whitespace-delimited token (the claim name). Verified format per
   `workers/common/agent_runtime.go:424`.

Hard-asserts that all three claim names are byte-identical. Exit 1 with a
stderr message listing the divergent names if not. Renders the cold/warm/warm
timings + the single PR URL card from §7.

The PR URL is read from `task/<last-turn>` `status.result` JSON via
`jq -r .pull_request_url`. If absent, the card prints `—` for the URL line
but does not exit non-zero (the assertion that matters is claim-name
identity).

### Profile dispatcher

```
demo_profile               # prints presenter|docs|hero|social
demo_profile_is presenter  # exit 0 if current profile matches, 1 otherwise
```

Single source of truth for `${DEMO_RECORD_PROFILE:-presenter}`. The existing
`pe` and `p` from `lib/demo-magic.sh` are *not* modified. Instead, the
dispatcher provides a wrapper:

```
demo_pe "<command>"   # pe in presenter; runs without typewriter in docs/social/hero
demo_show <path>      # bat in presenter; bat --style=plain in docs; head -8 in social/hero
```

Scripts call `demo_pe` / `demo_show` going forward; legacy `pe` calls in the
existing four scripts are migrated lazily as each script is tightened.

### Logging discipline

None of the above helpers may log raw secrets. Specifically forbidden in
log output (and enforced by `grep -v` guards in the kontxt helpers):

- `Txn-Token:` header values
- `Authorization: Bearer` values
- Subject token file *contents* (paths are fine)
- Anything matching `eyJ[A-Za-z0-9_=-]{20,}` (JWT prefix)

This matches the redaction rules in `docs/kontxt.md` §"Redaction rules" and
`AGENTS.md` "Constraints".

---

## 6. Existing-scenario rewrites

The four current scripts keep their structure (`Brief → Show → Run → Follow →
Inspect → Summary`) but each beat tightens. Per-script changes:

### `10-chat-pr.sh` — Chat-to-PR

| Beat | Today | After |
|---|---|---|
| Brief | `printf 'client=...\\nendpoint=...\\nmodel=...\\norchestration=...'` | `gum style` one-line connection summary |
| Show prompt | `cat ${chat-request.txt}` (~90 lines) | `glow chat-request.txt \| head -20`; full file path printed below |
| Run | `run_demo_chat_request_file ...` | Unchanged; the `claude` invocation is the demo |
| Follow (parent) | `wait_for_chat_parent_task` (silent) | Wrapped in `chapter "Coordinator appears"`; idle frames stripped by `--idle-time-limit` |
| Follow (succeed) | `wait_for_task_succeeded` (silent, up to 3 h) | Same wrapper + a `tail` of last lines from the parent Pod, so something visible happens |
| Inspect | 4 `pe` calls with `-o json \| jq '{...}'` | 1 `kubectl get task` table, 1 `kubectl get agents` table |
| Summary | `summarize_task_run` JSON | `payoff_card` with PR URL, child count, review cycles |

Also: trim the hero request. The Vekil-metrics-#77 wall is fine for
correctness rehearsals but recorded demos use one of two short alternatives,
controlled by `DEMO_REQUEST_PRESET`:

- `quiet-flag` — *"Add a `--quiet` flag that suppresses non-error output. Add
  a test."* (~3 lines, fits on screen)
- `readme-fix` — *"Fix the broken link to docs/configuration.md in the
  README."* (trivially understandable, useful for hero)
- `vekil-metrics` — today's full request, kept for the long-form docs cast

### `20-manual-workflow.sh` — YAML workflow

Tightening:

- Lead with `glow manual-story.txt` (one paragraph) before the YAML dump.
- Replace `cat manual-task.yaml` with `bat --style=plain manual-task.yaml`
  so YAML syntax-highlights for the recording.
- Same payoff card. End with a one-liner that compares to Demo 10: *"Same PR.
  Same agents. Just YAML."*

### `30-cron-workflow.sh` — Scheduled workflow

- Already pre-warms; keep that.
- Replace the "completed child task" `printf` with a `gum` chip:
  `gum style --background 22 " ✓ scheduled run completed "`.
- End on a payoff card showing `next run in 47 seconds` (computed from the
  cron spec) so the audience sees the schedule is real.

### `40-security-scanning.sh` — Security remediation

**Target repo.** `github.com/sozercan/nodejs-goof` — a personal fork of
`snyk-labs/nodejs-goof`, the canonical intentionally-vulnerable Node.js demo
app. The fork is owned, so the demo can open real public PRs without
touching upstream and without exposing a private repo URL the audience can't
click.

**Why this target.**

- Pre-existing, well-known among security folks (Snyk uses it in their own
  demos), so "is this real?" objections are pre-handled.
- Has a *catalog* of clean, textbook vulnerabilities. The scan surfaces
  several; the presenter doesn't need to know in advance which class will
  land on top.
- Forked, so any PRs we open are public, link-clickable, and don't pollute
  upstream history. Re-arming is automatic because we never merge PRs into
  our fork's `main`.

**The demo lets Orka pick the finding.** The whole point of the security
demo is that *Orka* surfaces the vulnerability — not that a presenter picks
which bug to "discover." The script must not pin `DEMO_SECURITY_FINDING_ID`;
it relies on the existing `first_security_finding_id` helper, which sorts
findings by severity rank and then by ID. Same repo, same commit, same
analysis agent prompt → same top finding across recordings. nodejs-goof is
stable enough that the top finding doesn't drift.

If the top finding *does* shift between recordings — e.g. after the
analysis agent's prompt is tuned — that's information: the recording
honestly reflects what the scanner currently considers most important.
Re-record, don't pin.

**What the audience sees** (in order):

1. The scan completes. The recording reveals the *full* findings list, not
   a curated subset, so the audience sees Orka surfaced multiple vulns.
2. Orka selects the top-severity finding and starts the patch flow on it.
   Whatever class it is — NoSQL injection in the login handler, the
   hardcoded session secret, the open redirect, the Handlebars layout
   traversal — the demo treats it as the headline.
3. The patch + PR lands against the fork, and the audience can click
   through.

**Beat tweaks** (relative to today's script):

- Before the patch step, `glow` a rendering of the finding description
  (severity, CWE, why it's bad) as the analysis agent wrote it. Audience
  reads the vuln before they see the diff. This is the analysis agent's
  work, not a pre-written script.
- After the patch lands, render the diff with
  `bat --style=plain --language=diff` so the before/after is syntax-colored.
- Two-card payoff: the diff snippet card + the PR card (which links to the
  real fork PR audiences can open).
- The pre-warm phase must check that no prior PR from this demo run already
  exists for the same finding — if it does, the recording uses it instead
  of generating a duplicate. (`nodejs-goof` will accumulate PRs across
  recordings; we close them periodically, but don't depend on a clean
  slate.)

**Env changes.** Update `lib/common.sh` defaults:

```bash
: "${DEMO_SECURITY_GIT_REPO:=https://github.com/sozercan/nodejs-goof.git}"
: "${DEMO_SECURITY_GIT_BRANCH:=main}"
# No DEMO_SECURITY_GIT_FORK_REPO needed — the target is already the fork,
# and the patch agent opens PRs against the fork's main branch.
# Do NOT set DEMO_SECURITY_FINDING_ID; the demo lets Orka rank findings.
```

The previous default (`sozercan/actions-test`, branch
`demo/security-python-command-injection`) was private and the branch name
encoded a Python scenario. Both go away; the nodejs-goof scan finds plenty
on its own.

---

## 7. New-scenario storyboards

Both new demos follow the same six-beat rhythm. Each is sketched here at the
level of detail an implementer would need to write the script.

### Demo 50 — Kontxt transaction tokens (`50-kontxt.sh`)

**Why this matters.** Today the README and chat demos show *what* Orka does.
Demo 50 shows *who is allowed to ask it*, and proves the answer is
cryptographically stamped onto every Task, Job, and Pod. That's the
governance story enterprise reviewers want, and it's invisible from the other
demos.

**Headline.** *"A Pod with zero secrets calls Orka, earns a one-shot
transaction token, and gets shut down the instant it asks for something
outside its scope."*

**Prerequisites the recording assumes.** kontxt TTS already installed in
`kontxt-system` (see `docs/kontxt-quickstart.md`). Orka already configured
with `ORKA_CONTEXT_TOKEN_PROFILE=kontxt` **and
`ORKA_CONTEXT_TOKEN_AUTHZ_MODE=enforce`**. The demo does *not* re-install
kontxt or restart Orka — that's a setup exercise, not a demo.

**Why enforce, not audit.** The kontxt-quickstart guide recommends rolling
out in audit mode first, and that's correct for production. For *this
recording* we run enforce-only because:

- The happy path is byte-identical in both modes — a valid TxToken gets a
  Task created either way. Audit mode contributes nothing visible.
- The denial is the climax. A live `403` in front of the audience is the
  moment the policy story lands. In audit mode the denial is a controller
  log line nobody can see, which is correct for a rollout and useless for a
  demo.
- Production callers should follow the quickstart guide and start in audit.
  The recording's job is to show what the steady state looks like, not the
  rollout path.

If the demo cluster is fresh from following the quickstart and is sitting
in audit mode, the recording wrapper flips it to enforce as part of
pre-warm, then leaves it there.

**Beats:**

| # | Chapter | Show | Run | Visible payoff |
|---|---|---|---|---|
| 1 | Brief | One-line: *"Zero-secret Pod earns a transaction token. Then watch us try to abuse it."* | — | `gum` panel |
| 2 | Show identity | `kubectl get sa orka-kontxt-caller -o yaml` | — | Audience sees zero secrets attached |
| 3 | Show config | `kubectl -n orka-system get deploy ... \| grep CONTEXT_TOKEN` | — | One line proving `PROFILE=kontxt` and `AUTHZ_MODE=enforce` |
| 4 | Run valid caller | `kubectl apply -f kontxt-caller-job.yaml`; tail logs | wait for Job | The 3-step caller output: `1/3 exchange... 2/3 create task... 3/3 wait for result...` |
| 5 | Inspect provenance | One combined view: `spec.transaction` on the Task **and** the same `orka.ai/transaction-id` label on the worker Pod | — | Compact `transaction { id, profile: kontxt, contextDigest: sha256:... }` plus matching Pod label |
| 6 | Try to abuse it | `kubectl apply -f kontxt-denied-caller-job.yaml`; same identity, but asks for a Task in `namespace: not-default` | wait for Job (fast — no worker spawns) | `denied status=403` printed by the caller |
| 7 | Payoff | — | — | Payoff card: `Valid scope → sealed Task. Wrong scope → 403. Same identity, both times.` |

Beat 6 is intentionally fast. The denied caller never spawns a worker Pod —
Orka rejects the request inline, the Job logs the `403`, and we're done in
seconds. The 7-beat structure stays the same length as the original; we just
trade the audit→enforce switching beat for a denial that visibly fires.

**Bash skeleton** (illustrative; full implementation deferred):

```bash
chapter "1/7 Brief"
p "A Pod with no Orka token will earn one — then we'll see what happens when it asks for too much."

chapter "2/7 The caller has no secrets"
pe "kubectl -n default get sa orka-kontxt-caller -o yaml | yq '.secrets, .imagePullSecrets'"

chapter "3/7 Orka enforces kontxt scopes"
pe "kubectl -n orka-system get deploy orka-controller-manager -o jsonpath='{.spec.template.spec.containers[0].env[*]}' | jq -r 'select(.name | startswith(\"ORKA_CONTEXT_TOKEN_\")) | \"\\(.name)=\\(.value)\"'"

chapter "4/7 Run the in-cluster caller"
pe "kubectl apply -f \${DEMO_WORKDIR}/kontxt-caller-job.yaml"
pe "kubectl -n default wait --for=condition=complete job/orka-kontxt-caller --timeout=5m"
pe "kubectl -n default logs job/orka-kontxt-caller | grep -E '^[0-9]/3|status=|task=|transactionID='"

chapter "5/7 Provenance is stamped end-to-end"
pe "kubectl -n default get task kontxt-mit-license-check -o json | jq '{transaction: .spec.transaction, podLabels: .metadata.labels}'"
pe "kubectl -n default get pods -l orka.ai/task=kontxt-mit-license-check -o jsonpath='{.items[0].metadata.labels.orka\\.ai/transaction-id}'"

chapter "6/7 Try to abuse the same identity"
pe "kubectl apply -f \${DEMO_WORKDIR}/kontxt-denied-caller-job.yaml"
pe "kubectl -n default wait --for=condition=complete job/orka-kontxt-denied-caller --timeout=2m"
pe "kubectl -n default logs job/orka-kontxt-denied-caller | grep 'denied status='"

chapter "7/7 Summary"
payoff_card_kontxt
```

`payoff_card_kontxt` is a small helper that pulls the transaction ID from
the successful Task and renders:

```
╭─ One identity, two outcomes ────────────────────────────────╮
│                                                             │
│  txn-1a2b3c4d   ✓ scoped ok   → Task sealed                 │
│  txn-5e6f7a8b   ✗ wrong namespace → 403 in 1.2s             │
│                                                             │
│  ServiceAccount → kontxt → Txn-Token → Orka                 │
│                                                             │
╰─────────────────────────────────────────────────────────────╯
```

### Demo 60 — Agent sandbox workspaces (`60-agent-sandbox.sh`)

**Why this matters.** The other demos all show one-shot work. Demo 60 shows
*continuity*: a coding session's repo, dependency cache, and built artifacts
survive across turns and across agents. This is the difference between "AI
ran a command" and "AI is working in a real coding environment that respects
how humans actually iterate."

**Headline.** *"One session, two agents, three turns. Scout analyzes.
Builder ships. Builder comes back when CI fails. Same warm workspace through
all of it."*

**Prerequisites the recording assumes.** Upstream `agent-sandbox` installed.
Controller running with `ORKA_AGENT_SANDBOX_ENABLED=true` and a default
template called `orka-live-template`. Demo does not install the sandbox.
Target repo: `github.com/sozercan/vekil`.

**What the demo highlights.** A single arc covers three real upstream
capabilities Orka already ships, none of which need new platform work:

- **Session-scoped reuse.** `reusePolicy: session` + `cleanupPolicy: retain`
  with `sessionRef.name: vekil-metrics-77`. Deterministic claim name; later
  Tasks reattach.
- **Multi-agent on one workspace.** Two `Agent` CRs — `scout` (read-only,
  no `repo_write`/`open_pr` tools) and `builder` (write + PR tools) — share
  one `SandboxClaim`. Principle of least privilege per role, single
  filesystem.
- **Cache survival across review loops.** Turn 3 is the "CI flagged a
  missing test" follow-up. Same claim, same `go build` cache, fixup commit
  on the same PR. The pattern every developer has felt.

**Why not pause/resume or live-workspace port-forwards.** KEP 119 (sandbox
suspended state) isn't wired into the Orka controller today: `Task.Spec.
Suspend` exists but only gates scheduled/autonomous task iteration, not
sandbox lifecycle. Live workspace inspection (code-server, CDP) also has no
Orka-side UX. Both are interesting future demos; both would require shipping
a feature first. This demo stays within today's capabilities.

**Beats:**

| # | Chapter | Show | Run | Visible payoff |
|---|---|---|---|---|
| 1 | Brief | One-line: *"One session. Two agents. Three turns. One warm workspace through all of it."* | — | `gum` panel |
| 2 | Apply session + agents | `kubectl apply -f sandbox-session.yaml` then `bat scout.yaml builder.yaml` highlighting `tools:` difference | `kubectl apply -f sandbox-scout-agent.yaml sandbox-builder-agent.yaml` | Side-by-side: scout has `[file_read, web_search]`; builder has `[file_read, file_write, open_pr]` |
| 3 | Turn 1 — Scout | `bat sandbox-turn-1-scout.yaml` (prompt: *"Clone sozercan/vekil. Profile the metrics gap from issue #77. Write the proposal to `/workspace/scout-report.md`."*) | `kubectl apply -f sandbox-turn-1-scout.yaml`; wait for Succeeded | Log line: `Task demo-magic/demo-scout-turn-1 completed in sandbox workspace orka-vekil-metrics-77` + Task duration ~50 s |
| 4 | The claim survives | `kubectl get sandboxclaim -n demo-magic` | — | Claim `orka-vekil-metrics-77` status `Ready`, age older than Task end time, `cleanupPolicy=retain` annotation visible |
| 5 | Turn 2 — Builder | `bat sandbox-turn-2-builder.yaml` (same `sessionRef`, prompt: *"Read `/workspace/scout-report.md`. Implement the counters. Open a PR against sozercan/vekil."*) | `kubectl apply ...`; wait for Succeeded | Log line: **same** `orka-vekil-metrics-77` claim name reattached + `go build` cache hit + PR URL |
| 6 | Turn 3 — Builder fixup | `bat sandbox-turn-3-fixup.yaml` (same `sessionRef`, prompt: *"CI flagged a missing test in `metrics_handler_test.go`. Add it and push to the same branch."*) | `kubectl apply ...`; wait for Succeeded | Same claim reattached again + fixup commit URL on the same PR |
| 7 | Payoff | — | — | Payoff card: cold/warm/warm timings + one PR, one session, one sandbox |

**Bash skeleton** (illustrative; full implementation deferred):

```bash
chapter "1/7 Brief"
p "One session. Two agents. Three turns. One warm workspace through all of it."

chapter "2/7 Two agents, one session"
pe "kubectl apply -f \${DEMO_WORKDIR}/sandbox-session.yaml"
pe "kubectl apply -f \${DEMO_WORKDIR}/sandbox-scout-agent.yaml -f \${DEMO_WORKDIR}/sandbox-builder-agent.yaml"
pe "kubectl get agent demo-scout demo-builder -n \${DEMO_NAMESPACE} -o custom-columns=NAME:.metadata.name,TOOLS:.spec.tools"

chapter "3/7 Turn 1 — Scout clones, profiles, writes a report"
pe "bat --style=plain \${DEMO_WORKDIR}/sandbox-turn-1-scout.yaml"
pe "kubectl apply -f \${DEMO_WORKDIR}/sandbox-turn-1-scout.yaml"
pe "wait_for_task_succeeded demo-scout-turn-1"
pe "kubectl logs -n \${DEMO_NAMESPACE} -l orka.ai/task=demo-scout-turn-1 --tail=30 | grep -E 'completed in sandbox workspace|scout-report'"

chapter "4/7 Claim retained after Scout finished"
pe "kubectl get sandboxclaim -n \${DEMO_NAMESPACE} -l orka.ai/session=vekil-metrics-77"

chapter "5/7 Turn 2 — Builder reads the report, implements, opens PR"
pe "bat --style=plain \${DEMO_WORKDIR}/sandbox-turn-2-builder.yaml"
pe "kubectl apply -f \${DEMO_WORKDIR}/sandbox-turn-2-builder.yaml"
pe "wait_for_task_succeeded demo-builder-turn-2"
pe "kubectl logs -n \${DEMO_NAMESPACE} -l orka.ai/task=demo-builder-turn-2 --tail=20 | grep -E 'completed in sandbox workspace|cache|pull/'"

chapter "6/7 Turn 3 — Builder comes back to fix CI"
pe "bat --style=plain \${DEMO_WORKDIR}/sandbox-turn-3-fixup.yaml"
pe "kubectl apply -f \${DEMO_WORKDIR}/sandbox-turn-3-fixup.yaml"
pe "wait_for_task_succeeded demo-builder-turn-3"
pe "kubectl logs -n \${DEMO_NAMESPACE} -l orka.ai/task=demo-builder-turn-3 --tail=15 | grep -E 'completed in sandbox workspace|commit'"

chapter "7/7 Summary"
payoff_card_sandbox  # prints cold-start vs reattach timings + the single PR URL
```

`payoff_card_sandbox` renders:

```
╭─ One session, three turns ──────────────────────────────────╮
│                                                             │
│  Session  vekil-metrics-77                                  │
│  Claim    orka-vekil-metrics-77  (retained)                 │
│                                                             │
│  Scout    52s  cold   · cloned + profiled                   │
│  Builder   8s  warm   · implemented + PR #131               │
│  Fixup    11s  warm   · test added on same PR               │
│                                                             │
│  One workspace. Two agents. CI-feedback friendly.           │
│  https://github.com/sozercan/vekil/pull/131                 │
│                                                             │
╰─────────────────────────────────────────────────────────────╯
```

**Risk.** Sandbox claims today don't surface in Task status (per
`docs/agent-sandbox.md`). The demo therefore reads the claim name out of
worker logs. If that log format changes, the demo breaks silently.
Mitigation: `payoff_card_sandbox` hard-asserts that turns 2 and 3 reattach
the same claim name that turn 1 created, and fails the recording if not.

**Determinism.** The `sessionRef.name` (`vekil-metrics-77`) is what makes
the claim name reproducible. Pre-warm in `record.sh` must `kubectl delete
sandboxclaim -l orka.ai/session=vekil-metrics-77` so turn 1 is genuinely
cold. The recording timings in the payoff card are pulled from real Task
durations per recording — they'll vary; that's fine, as long as warm < cold
by an order of magnitude.

---

### Demo 70 — Agent Substrate workspaces (`70-agent-substrate.sh`)

**Why this matters.** Orka's execution workspace is provider-neutral. Demo 60
shows agent-sandbox; Demo 70 shows the *same Orka agent Task API* backed by a
second provider — **Agent Substrate** — where each workspace is a
gVisor-isolated Actor drawn from a pre-warmed WorkerPool and kept warm between
turns. A **real `gpt-5.4` codex agent** runs inside the gVisor sandbox: it
clones a repo, makes a change, and a real PR is opened. The message: swap one
field (`execution.workspace.provider: substrate`) and the entire agent Task
contract — model call, git push, PR — is unchanged. Orka abstracts the
execution substrate.

**Headline.** "One Task API, two execution substrates. A real agent clones,
edits, and opens a PR from inside a gVisor sandbox — then reuses the warm
workspace with no cold start."

**Distinct from Demo 60.** agent-sandbox and Substrate are *different providers*
(`agents.x-k8s.io` vs `ate.dev`). Demo 60 runs a multi-turn scout→builder→fixup
session on agent-sandbox; Demo 70 runs the same kind of real agentic work on
Substrate's **gVisor** isolation and shows warm-workspace reuse. Complementary,
not duplicates.

**Prerequisites.** Demo 70 runs on its **own** kind cluster — Substrate needs a
custom registry + gVisor node config, so it cannot attach to the shared
demo-magic cluster. Stand it up with `make demo-substrate-up`
(`hack/demos/cluster/install-substrate.sh`), which: (1) runs the CI-proven
`scripts/agent-substrate-e2e.sh` standup (`KEEP_CLUSTER=1`) — Substrate control
plane in `ate-system`, a `WorkerPool` + gVisor `ActorTemplate` (`orka-codex-ci`
in `ate-demo`), Orka wired with `--substrate-*` flags; (2) builds a
**codex-capable Actor image** (the production `agent-worker-codex` — daemon +
codex CLI + git) and points the ActorTemplate at it; (3) deploys the **vekil**
model proxy (one-time GitHub **device-code** login — the operator completes it
from the pod logs, since a plain `gho_` gh token has no Copilot entitlement);
(4) creates the model Secret (endpoint → vekil) and the git Secret. Requires
`kind`, `ko`, `docker`, `go`, `git`, `jq`, `kubectl`, `gh`. A
Copilot-enabled GitHub account is required for the proxy login; the git token
comes from `GIT_TOKEN`/`GITHUB_TOKEN` or the local `gh` CLI.

**Beats.**

| # | Beat | What the audience sees |
|---|------|------------------------|
| 1 | Cold | A Task with `provider: substrate` + `reusePolicy: session` + `sessionRef.create: true`. A fresh gVisor Actor; a real `gpt-5.4` agent clones the repo, edits a file, stops. Orka pushes the branch; the demo opens a real PR. `status.executionWorkspace.provider == substrate`. |
| 2 | PR | The demo opens the pull request via `gh` (the agent edited only — clean exit; Orka pushed). The real PR URL appears. |
| 3 | Warm | A second Task, same `sessionRef` (`create: false`). Reattaches the retained workspace: `status.executionWorkspace.reused == true` — repo already cloned, no cold start. A follow-up commit lands on the same PR. |

**Clean-exit contract (load-bearing).** The agent **edits files only** and
stops; Orka's `pushBranch` pushes the branch; the **demo script** opens/updates
the PR via `gh`. If the agent runs post-edit commands itself (git status, a PR
curl), a nonzero one makes the codex CLI exit 1 even though the work succeeded —
so the prompt forbids it.

**gVisor contract (load-bearing).** The Task sets
`ORKA_CODEX_DISABLE_SANDBOX=true`. Codex's inner bubblewrap sandbox cannot nest
under runsc (`bwrap: uid map: Operation not permitted`); gVisor **is** the
isolation boundary, so codex runs `danger-full-access`. The worker's
auto-fallback does not match this error pattern, so the env is set explicitly.

**Payoff card.** `payoff_card_substrate <cold-task> <warm-task> <pr-url>` reads
both Tasks' `status.executionWorkspace` and **hard-asserts** `provider ==
substrate` on both, `reused == true` on the warm Task, and a non-empty PR URL;
it fails the recording otherwise. ASCII-only card bodies (byte-width alignment;
the PR URL is shown via `__card_line`, which truncates rather than break the
border).

**Risk.** Real model runs are minutes-scale and non-deterministic — recordings
may need retakes. The end-to-end flow was verified live on darwin/arm64 +
Docker Desktop (a real agent opened vekil PRs #173–#175). `create_pull_request`
/ the demo's PR step handle an already-open PR, so reruns are safe. The
`sessionRef.create` flag is load-bearing: the cold Task sets `create: true`, the
warm Task `create: false`.

**Determinism aids.** The agent edits are tiny, fixed README markers (cold vs.
warm), so diffs are predictable. The model endpoint is an in-cluster proxy
(vekil) so there is no public-API flakiness beyond the upstream. The demo's
cleanup closes any prior demo PR + deletes the branch so the cold beat opens
fresh.

---

## 7.5. Manifest specifications

Each new beat references YAML files an implementer will otherwise have to
guess at. The render functions live in `lib/manifests.sh` (alongside the
existing `render_security_repository_scan_manifest`) so the same single file
owns all CR templating.

### Kontxt manifests (Demo 50)

**`lib/manifests.sh: render_kontxt_caller_sa()`** — emits a ServiceAccount
with zero secrets and zero image pull secrets:

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: orka-kontxt-caller
  namespace: default
  labels:
    orka.ai/demo: kontxt
automountServiceAccountToken: true
```

Plus a projected token Volume the Job will mount with audience
`${ORKA_CONTEXT_TOKEN_TTS_AUDIENCE:-kontxt-tts}`. The audience is what makes
this a *kontxt* subject token, not a Kubernetes bearer.

**`lib/manifests.sh: render_kontxt_caller_job()`** — the valid caller. Runs
a busybox-shaped image (use `ghcr.io/sozercan/orka-kontxt-demo:latest`,
built from `hack/demos/images/kontxt-caller/`) with the literal 3-step
loop that beat 4's grep expects:

```
1/3 exchange subject token for TxToken... ok status=200 transactionID=<id>
2/3 create task kontxt-mit-license-check... ok status=201 task=kontxt-mit-license-check
3/3 wait for result... ok result="MIT"
```

The caller's request body asks for a `type: agent` Task in `namespace:
default` with `prompt: "Read LICENSE from sozercan/vekil@main and reply
with the SPDX identifier only."` — a deterministic, fast, model-call-free
prompt that the existing `claude-agent` can answer in one turn.

**`lib/manifests.sh: render_kontxt_denied_caller_job()`** — identical to
the valid caller in *every way except* the body asks for `namespace:
not-default`, violating the signed `tctx.namespace` constraint. Expected
output (the literal string the beat-6 grep matches):

```
1/3 exchange subject token for TxToken... ok status=200 transactionID=<id>
2/3 create task kontxt-cross-ns-attempt... denied status=403 reason=context_token_namespace_mismatch
```

No turn-3 line — the job exits non-zero after the deny, but the Job
`spec.backoffLimit: 0` prevents retry and the `kubectl wait` in beat 6 is
`--for=jsonpath='{.status.failed}'=1` instead of `complete`.

**Image.** A new `hack/demos/images/kontxt-caller/` directory ships:

- `Dockerfile` — Alpine + curl + jq + the 30-line `caller.sh` script
- `caller.sh` — reads `${SUBJECT_TOKEN_PATH}`, POSTs to
  `${ORKA_CONTEXT_TOKEN_TTS_URL}/token`, attaches the returned TxToken as
  `Txn-Token` to the Orka API call, prints the literal lines above

The image is built and pushed by `make demo-images` (new Makefile target,
added in the same change as `record.sh`). It is *not* part of `make
docker-build-all` — demo images are recording infrastructure, not product.

### Sandbox manifests (Demo 60)

All under namespace `demo-magic` (existing demo namespace), labeled
`orka.ai/demo: sandbox` and `orka.ai/session: vekil-metrics-77`.

**No separate Session CR.** Orka sessions are persisted by the controller's
session store (SQLite by default), not as Kubernetes CRs. Turn 1's
`sessionRef.create: true` is sufficient to materialize the session;
turns 2 and 3 reference the same `sessionRef.name` without `create`. Drop
any `render_sandbox_session` helper from the manifest pile — it's not
needed.

**`render_sandbox_scout_agent()`** — read-only tool set:

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Agent
metadata:
  name: demo-scout
  namespace: demo-magic
spec:
  provider:
    name: <default>
  tools:
    - name: file_read
    - name: web_search
  systemPrompt: |
    You are a read-only scout. You analyze code and write notes to
    /workspace/scout-report.md. You never write code outside /workspace
    and never open pull requests.
```

**`render_sandbox_builder_agent()`** — write + exec tools:

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Agent
metadata:
  name: demo-builder
  namespace: demo-magic
spec:
  provider:
    name: <default>
  tools:
    - name: file_read
    - name: file_write
    - name: code_exec
  systemPrompt: |
    You implement changes proposed by the scout. You read
    /workspace/scout-report.md, apply the changes, run tests via
    code_exec, and use the in-workspace git CLI (cloned and authenticated
    by the agent runtime) to push branches and open pull requests
    against sozercan/vekil.
```

**Tool-name caveat.** Built-in Orka tools verified against
`internal/tools/common_constants.go` and `workers/ai/main_test.go`:
`file_read`, `file_write`, `code_exec`, `web_search`, `web_fetch` are
real. There is *no* `open_pr` built-in tool — PR creation is done by the
agent runtime using `git` + `gh` (or the GitHub HTTP API) from inside the
sandbox workspace. The scout/builder split is enforced by `file_write` +
`code_exec` *presence on builder, absence on scout*, not by an `open_pr`
tool.

The visible diff between the two agents on screen is the `tools:` list —
beat 2's `-o custom-columns=NAME,TOOLS` makes the difference legible.

**`render_sandbox_turn_task()` — single helper, three call sites:**

```
render_sandbox_turn_task <name> <agent> <prompt-file> [--create-session]
```

Emits a Task CR with the common workspace block:

```yaml
spec:
  type: agent
  agentRef:
    name: <agent>
  sessionRef:
    name: vekil-metrics-77
    create: <true on turn 1 with --create-session, false on turns 2 & 3>
  prompt: <contents of prompt-file>
  execution:
    workspace:
      enabled: true
      templateRef:
        name: orka-live-template
      reusePolicy: session
      cleanupPolicy: retain
```

Turn 1 passes `--create-session`; turns 2 and 3 omit it. The session store
materializes on first reference; reuse on subsequent turns is implicit.

Prompt files in `hack/demos/prompts/`:

- `sandbox-turn-1-scout.txt` — *"Clone sozercan/vekil at main. Look at
  issue #77 (Prometheus metrics gap). Profile what's missing. Write your
  proposal to `/workspace/scout-report.md` with: counter names, where they
  go, test outline. Do not modify any vekil source."*
- `sandbox-turn-2-builder.txt` — *"Read `/workspace/scout-report.md`.
  Implement the counters and tests in the vekil checkout under
  `/workspace/vekil`. Push the branch with `git` and open a pull request
  against sozercan/vekil (using `gh pr create`) with title 'Add Prometheus
  /metrics endpoint (closes #77)'."*
- `sandbox-turn-3-fixup.txt` — *"CI on the open PR flagged that
  `metrics_handler_test.go` is missing a test for the `error_total`
  counter. Add it. Push as a fixup commit on the same branch. Do not
  open a new PR."*

### Substrate manifests (Demo 70)

Rendered by `render_substrate_*` in `lib/manifests.sh`. The Substrate control
plane + `WorkerPool` + `ActorTemplate` (on a codex-capable image) + the model
proxy + Secrets are installed out-of-band by `cluster/install-substrate.sh`; the
demo only applies the Orka `Agent` + two `Task`s and opens the PR. All carry
`orka.ai/demo: substrate` / `demo.orka.ai/scenario: substrate`.

- **Agent** (`render_substrate_agent`) — `core.orka.ai/v1alpha1` Agent, runtime
  `codex`, model `gpt-5.4`. A real model run: `secretRef` (NOT `providerRef` —
  mutually exclusive with `runtime`) points at a Secret carrying
  `OPENAI_BASE_URL` (→ the in-cluster vekil proxy) + a placeholder
  `OPENAI_API_KEY`. The system prompt tells the agent to edit files only and
  stop (Orka pushes; the demo opens the PR).
- **Task** (`render_substrate_task <name> <none|session> <create> <prompt>`) —
  agent Task whose `execution.workspace` selects `provider: substrate` with
  `templateRef` → `ate-demo/orka-codex-ci`, plus `agentRuntime.workspace`
  (`gitRepo`, `branch`, `pushBranch`, `gitSecretRef`) and
  `env: ORKA_CODEX_DISABLE_SANDBOX=true` (gVisor is the sandbox). `reusePolicy`
  defaults to `session`; `cleanupPolicy` is `retain` for session tasks so the
  workspace stays warm. The 3rd arg sets `sessionRef.create`.
  - Cold beat: `render_substrate_task <n> session true  "<prompt>"` (**creates**
    the session — `create: true`).
  - Warm beat: `render_substrate_task <n> session false "<prompt>"` (reattaches;
    the card asserts `status.executionWorkspace.reused == true`).

The payoff card reads **Task status** (`status.executionWorkspace.{provider,
phase,reused}` via `kubectl jsonpath`) and the **PR URL** passed in by the demo
script (resolved via `gh pr list/create`) — no worker-log coupling.

### Worker log format the beats grep against

Verified against `workers/common/agent_runtime.go:424`:

```
Task <namespace>/<task-name> completed in sandbox workspace <claim-name>
```

That is the **only** line guaranteed to contain the claim name on the
happy path. All beat-3/5/6 log greps and `payoff_card_sandbox`'s claim-name
extraction must use this pattern:

```bash
grep -oE 'completed in sandbox workspace [a-z0-9-]+' \
  | awk '{print $NF}'
```

Do not grep for the literal `claimed sandbox workspace` — that phrase does
not appear in worker output (it was a guess in earlier drafts of this doc).

### Label selectors used by beat greps

So the implementer doesn't have to derive them from prose. All Pod labels
are defined as constants in `internal/labels/labels.go`:

| Selector | Constant | Used by | Resources |
|---|---|---|---|
| `orka.ai/demo=kontxt` | (new, demo-only) | reset.sh | SA, Job, Task |
| `orka.ai/demo=sandbox` | (new, demo-only) | reset.sh | Agent, Task, SandboxClaim |
| `orka.ai/session=vekil-metrics-77` | (new, demo-only) | beat 4, pre-warm | SandboxClaim |
| `orka.ai/task=demo-scout-turn-1` (etc.) | `labels.LabelTask` | beat 3,5,6 log greps | Pod |

The `orka.ai/demo` and `orka.ai/session` labels are demo-scoped only and do
not need to be added to `internal/labels/labels.go`. The `orka.ai/task`
label is shipped — `LabelTask = "orka.ai/task"`. Render functions set it
explicitly on every Task CR they emit so worker Pods inherit it.

`reset.sh` extends its existing cleanup to delete every resource with
`orka.ai/demo` in `(kontxt, sandbox)`.

---

## 8. File layout and tooling

```
hack/demos/
├── README.md              # how to *run* the demos (today's purpose)
├── RECORDING.md           # this file — how to *record* and design them
├── record.sh              # NEW — asciinema wrapper, emits .cast + .gif + .svg
├── cluster/               # NEW — kind bootstrap for recording-grade env
│   ├── cluster-up.sh      # creates kind cluster, builds + loads Orka image, helm install
│   ├── cluster-down.sh    # kind delete cluster --name orka-demo
│   ├── install-kontxt.sh  # in-cluster kontxt-TTS + Orka env vars
│   ├── install-agent-sandbox.sh   # upstream operator + SandboxTemplate
│   ├── install-substrate.sh       # Agent Substrate on a dedicated kind cluster (Demo 70)
│   ├── install-demo-model.sh      # Provider + model/git Secrets for demos 10/20/30/40
│   ├── demo-env.sh                # sourceable consolidated DEMO_* env for the SDLC demos
│   └── templates/
│       └── orka-live-template.yaml
├── images/                # NEW — demo-only container images
│   └── kontxt-caller/
│       ├── Dockerfile
│       └── caller.sh
├── prompts/               # NEW — sandbox turn prompt files
│   ├── sandbox-turn-1-scout.txt
│   ├── sandbox-turn-2-builder.txt
│   └── sandbox-turn-3-fixup.txt
├── lib/
│   ├── common.sh          # existing; gains chapter(), payoff_card(), profile dispatch
│   ├── demo-magic.sh      # existing; unchanged
│   ├── manifests.sh       # existing; gains render_kontxt_*, render_sandbox_*
│   ├── style.sh           # NEW — color palette, gum helpers, prompt
│   └── test/              # NEW — bash -e smoke tests for helpers
├── 00-preflight.sh        # existing
├── 10-chat-pr.sh          # existing, tightened
├── 20-manual-workflow.sh  # existing, tightened
├── 30-cron-workflow.sh    # existing, tightened
├── 40-security-scanning.sh# existing, tightened
├── 50-kontxt.sh           # NEW
├── 60-agent-sandbox.sh    # NEW
├── 70-agent-substrate.sh  # NEW (runs on its own kind cluster)
├── reset.sh               # existing, extended for kontxt/sandbox/substrate resources
└── out/                   # gitignored; .cast/.gif/.svg artifacts land here
```

Published artifacts (committed) live under `docs/images/demos/`:

```
docs/images/demos/
├── 10-chat-pr.gif         # hero profile, README
├── 10-chat-pr.svg         # docs profile, embedded in docs/chat.md
├── 20-yaml.svg
├── 30-cron.svg
├── 40-security.svg
├── 50-kontxt.svg          # embedded in docs/kontxt.md
└── 60-agent-sandbox.svg   # embedded in docs/agent-sandbox.md
```

GIFs are large; we publish *one* GIF (the hero) and use SVG everywhere else.
SVG renders crisp on hi-DPI and is ~10× smaller than equivalent GIFs.

### Makefile targets

```makefile
demo-record-%:                   ## Record one demo: make demo-record-10
	hack/demos/record.sh $* docs

demo-record-hero:                ## Record the README hero
	hack/demos/record.sh 10 hero

demo-record-all:                 ## Re-record everything (run nightly?)
	for n in 10 20 30 40 50 60; do hack/demos/record.sh $$n docs; done
	hack/demos/record.sh 10 hero
```

CI does *not* run these. Recording requires a live cluster, a live model
provider, and minutes of wall-clock time. They're operator-run.

### `record.sh` CLI contract

```
hack/demos/record.sh <demo-number> <profile> [--no-preflight] [--keep-cast]
```

Positional arguments are required. `<demo-number>` must be one of
`10|20|30|40|50|60`; `<profile>` must be one of `presenter|docs|hero|social`.

Behavior, in order:

1. **Resolve script path.** `script=$(printf '%s-*.sh' "$demo-number")`,
   error and exit 64 (`EX_USAGE`) if zero or multiple matches.
2. **Manifest digest.** Write `out/<demo>-<profile>.manifest`:
   - `sha256sum hack/demos/<script>`
   - `sha256sum hack/demos/lib/{common,style,manifests,demo-magic}.sh`
   - `sha256sum hack/demos/prompts/*.txt | grep <demo>`
   - `ORKA_IMAGE=$(kubectl -n orka-system get deploy orka-controller-manager -o jsonpath=...)`
   - `MODEL_NAME=${DEMO_DEFAULT_MODEL:-unset}`
   - `REQUEST_PRESET=${DEMO_REQUEST_PRESET:-vekil-metrics}`
3. **Preflight** (unless `--no-preflight`): runs `00-preflight.sh` and
   exits 70 (`EX_SOFTWARE`) on failure with `>&2` "preflight failed; cluster
   not ready".
4. **Record cast.** Runs the asciinema invocation from §4 with the chosen
   profile. If the inner script exits non-zero, asciinema still writes the
   cast but `record.sh` exits 75 (`EX_TEMPFAIL`) so the cast is preserved
   for debugging.
5. **Render artifacts.** Always emits `.gif` and `.svg` from the cast:

   ```
   agg --theme github-dark --speed 1.4 out/<demo>-<profile>.cast out/<demo>-<profile>.gif
   agg --theme github-dark --speed 1.4 --renderer svg out/<demo>-<profile>.cast out/<demo>-<profile>.svg
   ```

   Renderer failures exit 73 (`EX_CANTCREAT`).
6. **Cast cleanup.** `out/<demo>-<profile>.cast` is removed at the end
   unless `--keep-cast` is passed. The committed artifact policy lives in
   §Open Question 2; the default until that's resolved is "do not keep
   `.cast`, since `.svg` is the canonical archive."

Exit code summary:

| Code | Meaning |
|---|---|
| 0 | Cast + .gif + .svg all written |
| 64 | Invalid arguments |
| 70 | Preflight failed |
| 73 | agg render failed |
| 75 | Demo script exited non-zero; cast preserved |

`make demo-diff` invokes `agg --renderer svg` against each cast in `out/`
and compares the resulting SVG byte size against the committed
`docs/images/demos/*.svg`. A drift of more than 20% prints
`DRIFT: <name> (<delta>%)`; under 20% prints `ok: <name> (<delta>%)`. Exit
non-zero only if any file shows >20% drift, so the target can be wired into
a nightly check later.

---

## 9. Determinism and freshness

Recordings will drift the moment Orka changes. We accept that; the question
is how we detect it.

- **Manifest digests.** The `record.sh` wrapper writes a `out/<demo>.manifest`
  file alongside the cast: SHAs of all referenced YAML, the Orka image tag,
  the model name, and the prompt preset. Re-recording with the same manifest
  should produce visually identical output (modulo timing).
- **Visual diff.** A `make demo-diff` target re-renders the current
  `out/*.svg` against the committed `docs/images/demos/*.svg` and prints
  byte-difference percentages. We re-record anything that drifted more than
  20 %.
- **Real run, redacted timings.** No "fake it for the camera" — timestamps,
  Task names, and IDs are real per-recording. They only need to *look* stable
  in framing, not be byte-identical.

---

## 10. Naming, request presets, and copy

Two new env-var families:

```bash
DEMO_REQUEST_PRESET=quiet-flag    # quiet-flag | readme-fix | vekil-metrics
DEMO_RECORD_PROFILE=docs           # presenter | docs | hero | social
```

Default request presets are recorded in `lib/common.sh` alongside the
existing `DEMO_VEKIL_METRICS_REQUEST` block. The current Vekil-metrics request
stays available but is no longer the *default* for recorded demos — only for
presenter rehearsals where the long-form story is the point.

Title and copy templates (used by the recording wrapper for asciicast title
and by the payoff cards):

| Demo | Title | One-line description |
|---|---|---|
| 10 | "Chat to PR" | "One chat turn becomes a coordinator, specialists, review, CI, and a real PR." |
| 20 | "GitOps workflow" | "Same workflow from YAML. The agent isn't magic — it's a CR." |
| 30 | "Scheduled work" | "Recurring AI triage queue — same auditable Task model, just add a `schedule:`." |
| 40 | "Security remediation" | "Finding → patch proposal → reviewable PR. No human triage required." |
| 50 | "Kontxt transaction tokens" | "Zero-secret caller, one-shot transaction token, sealed Kubernetes provenance." |
| 60 | "Warm agent sandboxes" | "One session, two agents, three turns. Scout, Builder, CI fixup — one warm workspace." |
| 70 | "Agent Substrate workspaces" | "A real agent clones, edits, and opens a PR from inside a gVisor sandbox — then reuses the warm workspace with no cold start." |

---

## 11. What this doc does *not* decide

These are deliberately left for follow-ups:

- **Audio narration.** The doc assumes silent recordings. If we want
  voiceover later, asciicasts are silent by nature — we'd need a parallel
  Loom/OBS pipeline.
- **The Vekil-metrics long-form demo.** It's the most impressive demo Orka
  can run, but it doesn't fit any of the three published surfaces. Likely
  becomes a separate `docs/case-study-vekil-metrics.md` with embedded clips,
  not a hero artifact.
- **Localization.** Demo text is English-only; chapter strings live in the
  scripts. Not worth abstracting until there's a non-English audience.
- **Performance regressions visible in recordings.** If Demo 10 used to take
  90 s and now takes 4 min, the GIF will be noticeably longer even with
  `--idle-time-limit`. Worth tracking, but not a blocker for v1.

---

## 11.5. Implementation work order

An implementer picking this doc up should work in four phases. Phases must
land in this order — each one assumes the previous is in place. Within a
phase, individual items can land independently.

### Schema assumptions verified at time of writing

The following were confirmed against source on the date this doc was last
revised. If the implementer's HEAD has drifted, re-verify before relying
on them:

| Assumption | Source of truth | Verified |
|---|---|---|
| `Agent.spec.tools` is `[]ToolReference{Name, Enabled}` | `api/v1alpha1/agent_types.go:160` | yes |
| `Task.spec.sessionRef` is `SessionReference{Name, Create, Append, MaxMessages}` | `api/v1alpha1/task_types.go:289` | yes |
| Sessions are NOT a CRD — stored in `store.SessionStore` (SQLite) | `internal/controller/session_manager.go`; no `session_types.go` in `api/v1alpha1/` | yes |
| Pod label for Task ownership: `orka.ai/task` | `internal/labels/labels.go:24` (`LabelTask`) | yes |
| Worker log line containing claim name: `Task <ns>/<name> completed in sandbox workspace <claim>` | `workers/common/agent_runtime.go:424` | yes |
| Built-in tools that exist: `file_read`, `file_write`, `code_exec`, `web_search`, `web_fetch` | `internal/tools/common_constants.go`, `workers/ai/main_test.go` | yes |
| Built-in `open_pr` tool: does NOT exist (agent runtime uses `git`/`gh` inside the sandbox) | `grep -rn open_pr internal/tools/ workers/` returns no tool definition | yes |

### Known unknowns (verify before Phase 3)

These have NOT been verified and the implementer should confirm before
writing the relevant render functions:

- **Provider CR name.** Storyboards use `provider: { name: <default> }`
  as a placeholder. The implementer must inspect what `00-preflight.sh`
  applies (or what `cluster/cluster-up.sh` installs) and use that exact
  name in the scout/builder Agent specs.
- **`gh` is preinstalled in the sandbox image.** The builder prompt
  assumes `gh pr create` works inside the sandbox. Phase 1's
  `cluster/templates/orka-live-template.yaml` is the implementer's
  source of truth — make sure the template's image bundles `git` + `gh`
  + a writable workspace. If not, either add them to the template image
  or swap the builder prompt to use the GitHub REST API (`curl + jq`).
- **Sandbox claim name shape.** §7.5's `payoff_card_sandbox` extracts the
  claim name from `completed in sandbox workspace <name>` — that line is
  verified, but the literal claim *name* (`orka-vekil-metrics-77` in the
  payoff card) is illustrative. The actual deterministic name produced by
  `reusePolicy: session` should be confirmed during Phase 3 dry-run
  (`kubectl get sandboxclaim -n demo-magic` after turn 1) and the card
  example updated to match if needed.

### Phase 1 — Recording infrastructure (no new recordings)

- [ ] `lib/style.sh` — color palette, prompt definition, `chapter`,
      `narrate`, `payoff_card` (generic), profile dispatcher (`demo_profile`,
      `demo_profile_is`, `demo_pe`, `demo_show`). See §5.5.
- [ ] `lib/common.sh` — append `DEMO_REQUEST_PRESET` resolution, register
      the three preset strings (`quiet-flag`, `readme-fix`, `vekil-metrics`).
- [ ] `lib/manifests.sh` — add stubs for `render_kontxt_*` and
      `render_sandbox_*` (empty body, just the function signatures), so
      Phase 3 can fill them without re-touching the file structure.
- [ ] `hack/demos/record.sh` — full CLI per §8 "`record.sh` CLI contract".
- [ ] `hack/demos/cluster/` — new directory holding the kind bootstrap:
   - `cluster-up.sh` — creates a kind cluster named `orka-demo`, builds
     and loads the Orka controller image, installs Orka via the existing
     Helm chart with `controller.agentSandbox.enabled=true` and
     `defaultTemplate: orka-live-template`.
   - `install-kontxt.sh` — installs kontxt-TTS into `kontxt-system`
     namespace, configures Orka with `ORKA_CONTEXT_TOKEN_PROFILE=kontxt`,
     `ORKA_CONTEXT_TOKEN_AUTHZ_MODE=enforce`, issuer + audience pointed
     at the in-cluster TTS service. Reuses the ephemeral RSA key/JWKS
     pattern from `scripts/live-github-oidc-e2e.sh` so no external secrets
     are needed.
   - `install-agent-sandbox.sh` — installs the upstream `agent-sandbox`
     operator via its published manifests, then applies
     `cluster/templates/orka-live-template.yaml` (a `SandboxTemplate`
     containing the agent CLI runtime image + git/gh + workspace dirs).
   - `cluster-down.sh` — `kind delete cluster --name orka-demo`.
- [ ] Makefile — add `demo-record-%`, `demo-record-hero`, `demo-record-all`,
      `demo-diff`, `demo-images`, **`demo-cluster-up`**, **`demo-cluster-down`**
      targets. `demo-cluster-up` invokes the three install scripts in
      order: cluster → kontxt → agent-sandbox. Idempotent — re-running
      against an existing `orka-demo` cluster only re-applies what changed.
- [ ] `hack/demos/README.md` — add a short "Recording" section pointing at
      `RECORDING.md` for design, at `record.sh --help` for usage, and at
      `make demo-cluster-up` for environment bootstrap.

**Acceptance (cluster-free).** All scripts pass `bash -n
hack/demos/*.sh hack/demos/lib/*.sh hack/demos/cluster/*.sh`. The helpers
in `lib/style.sh` have unit-style smoke tests under
`hack/demos/lib/test/` (no Go test framework; just `bash -e` scripts that
assert exit codes and stdout patterns). `make demo-record-10
--no-preflight` exits 70 (preflight required) when no cluster is
reachable, exits 64 on bad arguments, and prints `--help`.

**Acceptance (cluster-available).** `make demo-cluster-up` brings up a
fresh kind cluster from zero in under 10 minutes with Orka + kontxt-TTS +
agent-sandbox all running and healthy. `make demo-record-10` then runs
the existing `10-chat-pr.sh` end-to-end and writes
`out/10-docs.{gif,svg,manifest}`. The recording looks the same as today
because no script has been tightened yet — that's fine. The infra works.

If no recording-grade machine is available (kind + Docker + a model
provider API key), an implementer can complete all of Phase 1 except the
cluster-available acceptance check and hand that check off to whoever
owns the demo cluster.

### Phase 2 — Tighten existing scripts

Per §6, in this order (lowest-risk first, so a broken phase-1 helper is
caught on demo 30 not on the hero):

- [ ] `30-cron-workflow.sh` — fewest beats, smallest blast radius
- [ ] `20-manual-workflow.sh`
- [ ] `40-security-scanning.sh` — also flips defaults to
      `nodejs-goof` per §6 Demo 40 notes; remove
      `DEMO_SECURITY_GIT_FORK_REPO` and any pinned `DEMO_SECURITY_FINDING_ID`
- [ ] `10-chat-pr.sh` — last, since it's the README hero

After each tightening, re-record into `out/` and visually compare against
the previous take. No `docs/images/demos/` commits yet — those land at the
end of Phase 4.

**Acceptance:** all four scripts run under all four profiles without
helper errors. Casts under 4 minutes in `docs` profile; under 75 seconds in
`hero` profile (for demo 10).

### Phase 3 — New scenarios

The kind cluster from Phase 1's `make demo-cluster-up` already provides
kontxt-TTS and agent-sandbox; no extra environment setup needed.

- [ ] Run the **Known unknowns** checks above against the bootstrapped
      cluster and update §7.5 placeholders (provider name, claim name
      shape) accordingly. The template name `orka-live-template` is fixed
      by Phase 1's `install-agent-sandbox.sh`.
- [ ] `lib/manifests.sh` — fill in `render_kontxt_*` and
      `render_sandbox_*` per §7.5.
- [ ] `hack/demos/images/kontxt-caller/{Dockerfile,caller.sh}` per §7.5.
      Build with `make demo-images` (publishes to the kind cluster's
      local registry; no external registry push required).
- [ ] `hack/demos/prompts/sandbox-turn-{1-scout,2-builder,3-fixup}.txt`
      with the literal strings in §7.5.
- [ ] `50-kontxt.sh` per §7 storyboard. Includes the
      `payoff_card_kontxt` helper (new addition to `lib/style.sh` since
      it's scenario-specific but reuses generic gum framing).
- [ ] `60-agent-sandbox.sh` per §7 storyboard. Includes the
      `payoff_card_sandbox` helper.
- [ ] `reset.sh` — extend the existing cleanup to delete
      `orka.ai/demo in (kontxt, sandbox)` per §7.5 label-selector table.

**Acceptance:** `make demo-record-50` and `make demo-record-60` both
produce green `.svg` artifacts. Kontxt's deny path actually denies
(beat 6 logs `denied status=403`). Sandbox claim names are byte-identical
across the three sandbox turns (asserted by `payoff_card_sandbox`). Both
demos run against a fresh `make demo-cluster-up` cluster with no manual
setup steps in between.

### Phase 4 — Publish

**Prerequisite:** resolve [Open Questions](#open-questions) 1 (default
request preset) and 2 (committed artifact format).

- [ ] Copy the six "canonical" recordings into `docs/images/demos/` in the
      format chosen by Q2 (`.svg` only, `.gif` + `.svg`, or both + `.cast`).
- [ ] Update `docs/chat.md`, `docs/kontxt.md`, `docs/agent-sandbox.md`,
      and `README.md` with the embeds.
- [ ] Add a one-paragraph "How these were recorded" note to
      `docs/development.md` pointing at `RECORDING.md`.
- [ ] Wire `make demo-diff` into a nightly job *or* document that it's a
      manual check before each release. Open Question 5's answer to "infra
      first vs polish first" is now moot — both are done.

**Acceptance:** README hero animates. The three docs pages embed their
demos. `make demo-diff` returns clean against the committed artifacts.

### Cross-cutting checks every phase

After any `*.sh` edit, run `bash -n` per `AGENTS.md`:

```bash
bash -n hack/demos/*.sh hack/demos/lib/*.sh
```

After any `*_types.go` edit (none planned in this work — flag it as scope
creep if encountered): `make manifests generate`.

After any `.github/workflows/*.yml` edit (also not planned): actionlint.

---

## Open questions

Decisions are tracked here. Items marked **decided** are no longer blocking
and have been folded into §11.5.

1. **Vekil-metrics is the default `DEMO_CHAT_REQUEST` today.** Switching the
   recorded demos to `quiet-flag` or `readme-fix` means the recordings cover
   a 3-line ask. Is that the right first impression, or do we want the hero
   to be a meatier change even at the cost of length? *Open — blocks Phase
   4 only.*
2. **Do we commit the `.cast` files**, the `.gif`/`.svg`, or both? `.cast`
   files are tiny and lossless but require the asciinema player to view.
   `.gif`/`.svg` render anywhere but are opaque artifacts. *Open — blocks
   Phase 4 only.*
3. **kontxt setup approach.** **Decided: kind bootstrap.** Phase 1 ships a
   `make demo-cluster-up` target that builds a self-contained kind cluster
   with kontxt-TTS installed and configured. Recording runs against that
   cluster; no external/manually-managed kontxt is required. See §11.5
   Phase 1 for the deliverable.
4. **agent-sandbox setup approach.** **Decided: kind bootstrap.** Same
   `make demo-cluster-up` target also installs the upstream `agent-sandbox`
   operator and provisions an `orka-live-template` `SandboxTemplate`. One
   `make` target brings up Orka + kontxt + agent-sandbox in one go.
5. **Order of work.** *Dissolved by §11.5* — both polish and infra are
   sequenced (infra first, then polish, then new scenarios).
