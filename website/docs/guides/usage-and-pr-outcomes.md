---
slug: /usage-and-pr-outcomes
description: "Report model token usage and verified pull request outcomes for each team."
---

# Usage and PR outcomes

Run `orka usage summary` or open **Usage** in the dashboard to see how much recorded
model usage went into issue-to-PR work. Teams are Kubernetes namespaces. The report
follows each original issue through planning, implementation, retries, delegated
Tasks, and reviews or repairs of its linked PRs. Failed and cancelled work stays in
the calculation.

Each Orka installation reports only its own team namespace. Connect to that
team's server to view its report. Combined reports across installations are not
available.

The main measure is **tokens per merged PR**. Tokens are the pieces of text a model
reads or generates. This measure describes observed model usage. It does not
measure developer productivity, hours saved, infrastructure spending, or human
review time.

## Use the CLI

The CLI reads the existing reporting API using your configured server,
authentication, and namespace. Reporting does not require a CRD.

```bash
orka usage summary -n payments
orka usage work WORK_ID -n payments
orka usage other unassociated -n payments -o json
```

Copy a work ID from the summary to inspect its Tasks, sessions, attempts,
measurements, PRs, and gaps. The other-usage categories are `review_only`,
`other_requests`, and `unassociated`.

All three commands accept `--from`, `--until`, `--as-of`, `--teams`,
`--repository`, `--model`, and `--kind`. For example:

```bash
orka usage summary --teams payments \
  --from 2026-09-01 --until 2026-10-01 \
  --repository example/project --model example-model -o yaml
```

The explicit `--teams` selector overrides `--namespace`, but must still select
this installation's namespace. Broader Kubernetes permissions do not allow
reports for other namespaces on that server.

The summary and other-usage commands default to requests started in the current
UTC month. Work details default to all retained history for that work. Pass the
same date filters when inspecting a summary's work request.

Summary and other-usage results page with `--limit` and `--offset`. The default
page size is 25, capped at 100 by the API. Totals cover the full selection, even
when only one page is shown. Reuse the timestamp printed after **Usage and
outcomes through**, along with the same filters, for later pages and work details:

```bash
orka usage summary -n payments --as-of 2026-09-18T10:00:00Z --limit 25
orka usage summary -n payments --as-of 2026-09-18T10:00:00Z --limit 25 --offset 25
orka usage work WORK_ID -n payments --as-of 2026-09-18T10:00:00Z --from 2026-09-01
```

Table output distinguishes `Unavailable` measurements from reported `0` counts
and `No model calls`. It includes coverage, cache availability, estimates, and
model-call versus agent-attempt counts. Use `-o json` or `-o yaml` for the full API
response, including null values and fields omitted from the table. Rejected
requests exit nonzero without printing a partial report.

## Read the report

Select requests by their start date, team, repository, work type, or model. The
dates use UTC and the end date is exclusive. A September cohort can include usage
and merges recorded in October. The report states the date through which it
includes usage and outcomes.

Both the token numerator and PR denominator belong to that same selected cohort.
Selecting a model includes every request that used that model and keeps all of
those requests' usage, including fallback models and failures. It does not divide
one model's tokens by outcomes that required other models too.

| Measure | What it counts |
| --- | --- |
| Requests | Original issue requests attempted, including those that produced no PR. |
| Recorded tokens | Input plus output tokens reported for the selected requests. Estimates, if present, are identified separately. |
| Cached reads and writes | Reported cache breakdowns, already included in input tokens. An unavailable breakdown is not zero. |
| Opened | Distinct PRs whose creation Orka confirmed through its publication path. |
| Ready now | Linked, open, nondraft PRs with current GitHub evidence that the head passes mergeability, status, and review requirements. |
| Merged | Distinct produced PRs with a merge confirmed by GitHub. |
| Tokens per opened PR | Recorded tokens divided by distinct produced PRs. |
| Tokens per merged PR | Recorded tokens divided by distinct merged PRs produced by the requests. |
| Model cost | **Price unavailable** in this version. A subscription or missing price does not mean a free call. Pricing is tracked in [#541](https://github.com/orka-agents/orka/issues/541). |

An aggregate cache breakdown is available only when every contributing
measurement reports it. Individual reported cache counts remain visible in work
details, and structured output retains known subtotals alongside availability flags.

Opened and merged are historical counts. Readiness is current. These counts
overlap, so do not add the columns together. With no merges, the report shows
**No PRs merged yet** and the usage spent; the merged-PR ratio is unavailable.

Work requests appear in pages of 25. Expand a request to load its Tasks, sessions,
measurements, PR links, current head, and measurement gaps. Detail lists also page
their contents. Paging preserves the report time and full cohort totals; apply
the filters again to refresh them. Shared review usage appears in every related request
with a shared-work label, but counts once in the team totals.
One request can produce several PRs without copying its usage onto each PR.
Several requests can contribute to one PR without increasing the team's distinct
PR count.

Existing developer-created PRs with a verified assistance link appear separately
from produced PRs. A recovered existing PR without a retained creation receipt is
classified as assistance. Review-only work, chat, and usage without a verified
work reference remain visible under **Other team usage**. Those sections select
activity started in the chosen period and explain why it is outside the delivery
cohort. Arbitrary conversation text or a caller-supplied team name cannot establish
a work or PR relationship.

## Understand measurement gaps

Usage recording stores counts and identifiers independently of tracing. It does
not require prompt, code, or transcript collection. Each provider request has a
durable start record before dispatch. If the final usage never arrives, the start
remains evidence of a missing measurement.

| Model path | Available measurements |
| --- | --- |
| Built-in AI worker | OpenAI Responses, OpenAI Chat Completions, Azure OpenAI, and Anthropic provider counts, including retries, API-detection probes, and fallback calls. |
| Native chat and compatibility endpoints | The same provider counts, including streamed responses. They appear as unassociated usage unless a controller-owned Task relationship exists. |
| ACP harness v2 agents | Agent-reported consumed input, output, and cache counts when supplied. Reports describe prompt attempts, not a verified number of model calls. |
| Agents reporting only context-window usage | Consumed-token usage is unavailable. Conversation size is not total tokens consumed. |
| Legacy harness v1 or agents with no usage report | An unavailable attempt measurement. |
| Container Tasks | No model calls are inferred. |

The managed Codex, Claude, Copilot, and OpenCode adapters may expose only context
usage. Do not assume their consumed-token counts are complete. Any reported
consumption appears with the `agent` source and its declared counter scope.
Subscriptions are subject to the same reporting gaps.

Repeated usage snapshots and final summaries update one cumulative counter. A
counter reporting 1,000 and later 1,500 tokens contributes 1,500, not 2,500.
Session counters allocate increases to the corresponding Tasks and attempts. If
the initial session baseline is missing, its first reported total appears
separately as unassociated usage; subsequent increases can be attributed, with
the gap still visible. A decreasing counter retains the earlier high-water total
and records a gap.

OpenAI input counts include cached tokens. Anthropic reports cache reads and
cache creation separately from uncached input, so Orka adds them once when
normalizing input. See the [OpenAI caching guide](https://developers.openai.com/api/docs/guides/prompt-caching)
and [Anthropic caching guide](https://platform.claude.com/docs/en/build-with-claude/prompt-caching).

A failed or cancelled stream can retain known input even when output usage is
missing. The dashboard shows complete, partial, and unavailable measurements,
with separate model-call and agent-attempt counts. A ratio based on partial data
uses only recorded tokens and can understate usage. An estimate retains the
`estimate` source. This version does not generate token estimates.

## PR state and retention

Orka refreshes retained PR links while their RepositoryMonitor is active, even
after the execution Tasks finish or are deleted. A confirmed merge ends polling
for that PR. Monitors without refreshable links keep their normal schedule.
Closed, unmerged PRs remain eligible because they can reopen.
Each reconcile checks up to 20 PRs that have not been checked in five minutes.
While eligible links remain, the monitor schedules another batch after 15 seconds.
Once the backlog is drained, it returns to the five-minute refresh interval.
Closed PRs are queried directly, so they need not appear in the open-PR inventory.
An unavailable GitHub response clears current readiness without inventing a merge
or removing a previously confirmed merge.

Readiness requires GitHub's `CLEAN` merge state, `MERGEABLE` status, a nondraft
open PR, and an approved review decision or no review requirement. The evidence
records the exact head revision. Unknown or blocked states are not ready, and
readiness expires after ten minutes without a fresh observation. This conservative
test can omit repositories using pre-receive hooks or other states that GitHub
allows a privileged user to bypass. It does not change checks, approval policy,
or merge settings. See [GitHub's merge-state definitions](https://docs.github.com/en/graphql/reference/enums#mergestatestatus).

Usage survives routine Task, session, and execution-event cleanup in the SQLite
store. Set the controller's `--usage-retention` duration to control reporting
history. The default is `2160h`, or 90 days; `0` keeps it indefinitely. Cleanup runs
hourly and expires whole inactive cohorts. A running Task, recent usage or state
transition, or an open linked PR keeps the cohort and its earlier attempts.
Deleting an unfinished Task records cancellation for retention, while preserving
an execution outcome that was already recorded. This keeps abandoned work from
remaining active forever after Task deletion.
An unknown PR state keeps a cohort only while its link or latest observation
falls within the retention period.
Shared reviews and cumulative-counter baselines are retained with the work that
needs them. The report marks the oldest period that may have expired.

Collection begins with this version; older Task logs are not backfilled. Persist
the SQLite store on a volume to retain reports across controller restarts.
This release adds reporting tables to Orka's current schema. As with other schema
changes, startup rejects an older layout without modifying it. Preserve the old
database and plan the upgrade according to the
[SQLite compatibility policy](../concepts/architecture.md#sqlite-store-internals).

## API and access

`GET /api/v1/usage` returns full cohort totals, team rows, a page of work summaries,
and other-usage totals. It omits Task and measurement details.
`GET /api/v1/usage/work/:id` loads one work request and its details using the same
calculations. The summary response contains each work ID.
`GET /api/v1/usage/other/:category` returns a page of Tasks and their measurements
for `review_only`, `other_requests`, or `unassociated` usage.

| Query | Meaning |
| --- | --- |
| `namespace` | The installation's team namespace; defaults to the watched namespace. |
| `teams` | Explicit team selector that overrides `namespace`. Must select the installation's namespace; combined-team reports are not supported. |
| `repository` | Exact `owner/repository`, case-insensitive. |
| `kind` | `issue` or `pull_request`; omit for all work. PR review and repair remain outside delivery totals. |
| `model` | Select whole requests that used the given model. |
| `from`, `until` | Request-start period, accepting UTC dates or RFC3339 timestamps. `until` is exclusive. The summary defaults to the current UTC month through the report time. Work details default to all retained history. |
| `asOf` | Include observations through this timestamp; defaults to now. Future report times are rejected. |
| `limit`, `offset` | Work-summary or other-usage Task page size and offset. The default limit is 25, capped at 100; offset defaults to 0. Pagination never changes aggregate totals. |

Paged responses include `page.limit`, `page.offset`, and `page.total`. Pass the
summary's `asOf` on later page and detail requests to preserve the report time.
Dates must fit signed 64-bit Unix nanoseconds; out-of-range values return HTTP 400.
Selections requiring more than 20,000 retained records return HTTP 422 with an
instruction to narrow the filters. Records include work, Task, measurement,
PR-link, and selected PR-state evidence, including required cumulative baselines.
Routine PR refresh history does not consume this budget; reports load the latest
state at `asOf` and retained merge evidence.
This limit applies even when retention is unlimited. The API never returns
truncated totals as a complete report.
Token counts and totals must also fit the exact JSON integer range through
9,007,199,254,740,991. A selection whose usage exceeds that range returns HTTP 422
instead of rounded or wrapped accounting values.

For example, request
`/api/v1/usage?namespace=payments&from=2026-09-01&until=2026-10-01&asOf=2026-10-15T12:00:00Z`
to inspect September requests with usage and outcomes known by October 15.

TokenReview callers need namespace-wide `list` grants for `tasks`,
`repositorymonitors`, and `sessions`. Gateway-owned Task usage also requires
access to that exact current Gateway and namespace identity, including after
Task cleanup. A delivery work request is omitted if any contributing Task is
inaccessible, including shared PR reviews. An object-constrained transaction token
cannot authorize an archive report; in enforce mode, use the Task-list,
monitor-read, and session-read scopes
with an optional namespace constraint. Existing OIDC and namespace-isolation
policies still apply. See [API authorization](../reference/api-authorization.md).

## Example

Payments attempted 20 requests and recorded 12 million tokens, including failed
and cancelled work. Those requests produced 15 distinct PRs, of which 10 merged.

| Calculation | Result |
| --- | ---: |
| 12,000,000 / 15 PRs opened | 800,000 tokens per opened PR |
| 12,000,000 / 10 PRs merged | 1,200,000 tokens per merged PR |

The failed requests stay in the numerator. Later review usage stays attached to
these same requests. Comparing this result with another team's requests also
requires considering their repositories, models, and kinds of work.
