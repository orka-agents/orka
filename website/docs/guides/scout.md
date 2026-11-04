---
sidebar_label: Scout
description: "A scheduled, read-only agent Task that reads a watchlist and proposes Orka improvements as a digest."
---

# Scout

The scout is a scheduled agent Task that reads sources you choose, compares what it finds with
Orka's code and issue tracker, and writes a digest of at most N ranked proposals (3 by
default). It only reads and proposes. It never writes code, never files issues and holds no
write credential. A person reads the digest and decides what happens next.

The scout is plain configuration, not a controller feature. The example in
[`examples/scout/`](https://github.com/orka-agents/orka/tree/main/examples/scout) contains
everything:

| File | What it is |
| --- | --- |
| `watchlist.yaml` | A ConfigMap of sources: GitHub repositories and topic searches, paper and blog feeds, and Orka tracker queries. It also sets the run window, N and read limits. People own it; it becomes the Agent's system prompt, so the scout can read it but cannot change it. |
| `agent.yaml` | A built-in `claude` Agent with no shell and only `Read`, `Glob`, `Grep`, `web_search` and `web_fetch`. |
| `task.yaml` | A weekly scheduled Task with `concurrencyPolicy: Forbid`, a 45-minute timeout, an 80-turn cap, a read-only clone of Orka and the scout prompt. It ships with `suspend: true`. |

## What a run does

1. Pins the Orka commit it reads, from the GitHub commits API.
2. Reads each watchlist source for the run window and reports each one as `ok`,
   `unreachable`, `rate-limited`, `skipped` or `error`.
3. Drops items that don't touch Orka, using `AGENTS.md`, the ADR index and open epics.
4. States each remaining gap in Orka's terms, citing file paths it opened in the clone and
   issues it fetched.
5. Searches open and closed issues for duplicates, and drops ideas that an open issue
   already covers or that were closed as not planned.
6. Ranks what is left and keeps at most N.

The answer ends with one fenced block tagged `orka-scout-digest` that holds an
`orka.scout.digest.v1` JSON object. Each proposal in it uses the `orka.scout.proposal.v1`
format:

| Field | Content |
| --- | --- |
| `title` | Conventional Commit style, at most 100 characters |
| `kind` | `improvement`, `capability` or `direction` |
| `problem` | The gap, in Orka's terms |
| `evidence` | https source links with optional short quotes, Orka paths and issues, and the pinned commit |
| `sketch` | The smallest change that would close the gap |
| `risks` | Security and fit concerns |
| `size` | `S`, `M` or `L` |
| `related` | Issues the duplicate search found, open or closed |
| `confidence` | `low`, `medium` or `high`, with one line of reasoning |

The digest also reports every source's status, whether the run was partial, counts of items
dropped by reason, and up to five suggested watchlist additions for a person to review.

The Go package `internal/scout` parses and checks this format. It rejects unknown, duplicate
or case-folded fields, enforces field lengths and counts, and accepts only https links to
public host names. It requires exactly one source report per watchlist id, taken from the
watchlist itself, and requires `run.partial` to match those reports. Its verifier rejects a
digest whose commit or run window doesn't match the run, and keeps a proposal only if every
Orka path exists at the pinned commit, every cited or related Orka issue exists and every
link resolves. Proposals that fail are dropped and counted, never passed on. Watchlist
suggestions whose link can't be confirmed are removed. The parser also rejects an answer that
uses longer backtick or tilde code fences, so a quoted example can't pass as the digest.

In this version nothing runs the parser and verifier for you: a scout Task's raw result is
the agent's unchecked answer. Whatever reads the result, whether a person or the later
issue filer, must run it through `internal/scout` and use only the verified digest.

## Safety properties

- **Read-only.** The workspace uses `intent: read`, and the Task carries no read,
  publication or forge credential. The Agent has no shell and no write tools. A test plans
  the example's real runtime profile and checks that every tool it exposes is read-only.
- **Bounded egress.** ACP runtime Pods are confined by a NetworkPolicy that allows only DNS,
  the provider proxy and the controller. The scout reaches the web only through the
  controller-brokered `web_search` and `web_fetch` tools, which dial public addresses only
  and cap response sizes. The provider-native `WebFetch` and `WebSearch` tools are not granted.
  An `OutboundAccessPolicy` doesn't apply here: it attaches credentials to Tool calls and
  doesn't restrict brokered web reads.
- **Fetched content is data.** The prompt tells the scout to ignore instructions in fetched
  text, and lets only links and short quotes through. The parser rejects HTML comments, which
  a forged issue-tracker marker would need, and hidden or bidirectional formatting
  characters. A second `orka-scout-digest` block, such as one copied from a fetched page,
  makes the whole answer invalid.
- **Fail closed.** The prompt tells the scout to report an unreachable or rate-limited
  source, never fill it in from memory. When a consumer runs the `internal/scout` checks, a
  digest that hides or omits a failed source, exceeds N, cites a different commit or reports
  a different run window is rejected as a whole.
- **No messaging tools.** Scheduled runs are child Tasks, and child Tasks normally get
  `send_message` and `check_messages` added to their tools. The example sets
  `orka.ai/disable-coordination-tool-injection: "true"`, which the scheduler copies to every
  run, so the tool list stays exactly the read-only allowlist.
- **Capped.** Each run has a timeout and a turn cap, and `concurrencyPolicy: Forbid` keeps
  runs from overlapping. Orka has no per-Task token or cost budget yet, so the timeout and
  turn cap are the only spend limits.
- **People own the inputs.** The watchlist and the prompt change only through review of the
  ConfigMap and the Task.

Unauthenticated GitHub API calls are rate-limited (search allows about 10 requests a minute).
When the scout hits that limit, it reports the affected sources as `rate-limited` and marks
the run partial.

## Run it in dry-run digest mode

Every run is a dry run for now: it writes a digest and files nothing.

1. Replace the placeholder repositories and feeds in `watchlist.yaml` with sources you
   trust, and set `spec.model.name` in `agent.yaml` to a model your provider proxy serves.
2. Apply the example into the controller's watched namespace:

   ```bash
   kubectl -n orka-system apply -k examples/scout
   ```

3. Start one run now. The applied Task is a suspended schedule template, so create a
   one-off copy without the schedule:

   ```bash
   kubectl -n orka-system get task orka-scout -o json \
     | jq '{apiVersion, kind, metadata: {generateName: "orka-scout-manual-"},
            spec: (.spec | del(.schedule, .timeZone, .suspend))}' \
     | kubectl -n orka-system create -f -
   ```

4. Read the digest once the run finishes:

   ```bash
   orka -n orka-system task result <task-name>
   ```

5. To run weekly, set `spec.suspend: false` on `orka-scout`. Each run becomes a child Task
   named `orka-scout-<unix-timestamp>`. List them with
   `orka -n orka-system task children orka-scout`. The last five successful runs are kept.

## Limits of this version

- The agent can't read Task history, so each run covers a fixed window (the 8 days before
  it starts) instead of the time since the last successful run.
- Orka doesn't put the run's start time in the prompt, so the scout reports its own window.
  The verifier compares it with the run's scheduled start (the child Task's
  `creationTimestamp`) and rejects the digest if either end is more than a day off.
- The agent can't see the commit of its own clone, so it pins the commit from the GitHub
  API. Before trusting a digest, compare `run.orkaCommit` with the run's
  `status.delivery.startingSHA`. The verifier in `internal/scout` requires that match.
- Filing proposals as issues and measuring proposal quality are separate, later steps.
