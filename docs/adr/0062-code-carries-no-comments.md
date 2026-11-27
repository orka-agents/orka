# 62. Code carries no comments; decisions live in ADRs

Date: 2026-11-22

## Status

Accepted.

## Context

On 2026-11-22 the repository held about 48,000 lines of deletable comments in Go, shell,
and Python, and about 3,000 more in the UI's TypeScript. A sample of 1,338 comment groups
fell into four kinds:

- **Tautologies** (55%) repeated what the name, signature, or next line already said.
- **Decision records** (42%) explained a non-obvious, load-bearing choice, such as a
  fail-closed rule, an ordering requirement, or a protocol quirk. Each one sat beside a
  single function, so nobody saw it unless they were already editing that function, and no
  review process kept it true.
- **Excuses** (2%) justified a shortcut ("for compatibility", "best effort", a test that
  only checks for no panic). Reviewers accepted the shortcut because a comment explained it,
  and later code copied it.
- **Stale comments** (under 1%) described behavior the code no longer has.

Comments go stale silently, compilers and tests never check them, and agents treat them as
both instructions and permission. A comment that explains a hack lets the hack stay, while
a decision written only in a comment is invisible to anyone outside that file.

## Decision

- Code carries no comments. Names and structure explain *what*. Architecture decision
  records explain *why*. Issues track the shortcuts that remain.
- Some comments are exempt and never counted:
  - compiler and tool directives (`//go:`, `//nolint`, `// +kubebuilder` and other `// +`
    markers, `#nosec`, `# shellcheck`, `# noqa`, `eslint-disable`, ESLint's `/* eslint */`
    and `/* global */` configuration comments, `@ts-expect-error`, `/// <reference>`)
  - license headers, `Deprecated:` and TypeScript `@deprecated` notices, cgo preambles, and Go
    example `Output:` blocks
  - generated files, `testdata/`, and skills vendored from another repository
    (`.agents/skills/autoreview`)
  - doc comments on types and fields under `api/`, because controller-gen publishes those
    as CRD schema descriptions that users read through `kubectl explain`
- Enforcement ratchets down:
  - **Go, shell, and Python:** `hack/commentlint` counts each file's comment lines and
    compares them with `hack/commentlint/baseline.json`. `go test ./hack/repocheck` (the
    Repository Rules workflow) and `make lint-comments` fail when a file has more lines than
    its baseline, and also when it has fewer, until the reduction is recorded.
  - **UI TypeScript:** the `orka/no-comments` ESLint rule enforces the same ratchet, with
    ESLint bulk suppressions in `ui/eslint-suppressions.json`.
  - `make comment-baseline` lowers both baselines and refuses to raise either.
- Comments that existed before this decision are removed in rounds, using the
  `delete-comment` skill (`.agents/skills/delete-comment`):
  - a tautology or stale comment is deleted
  - an excuse is deleted and an issue is filed for the hack
  - a decision record is deleted and its decision is written into a new ADR for that round
- Round 1 covered `internal/llm`, `internal/gateway`, `internal/taskwebhook`,
  `internal/liveevents`, `internal/tracing`, `internal/metrics`, `internal/labels`,
  `internal/redact`, `internal/usage`, `internal/approvals`, `internal/githubauth`,
  `internal/tokenexchange`, `internal/artifactcap`, `internal/agentcontext`,
  `workers/common`, `workers/general`, `internal/workerenv`, `internal/aitools`,
  `internal/providerproxy`, `internal/publisher`, `internal/workspacedelta`,
  `internal/taskwait`, `internal/taskterminal`, and `internal/operatoraccess`. Its
  decisions are in [ADR 0063](0063-code-comment-decisions-round-1.md), and its excuses
  became issues #1248–#1266.

## Consequences

- A reader who needs the *why* has to leave the file and open an ADR. Each recovered decision
  names its symbol so that `rg` finds it.
- The ADR index grows by one record per cleanup round. These records are reference
  catalogs, not single decisions, and are never rewritten. When a later round contradicts
  one of them, it supersedes the affected entries.
- A pull request that deletes comments must also lower the baselines. That is the price of a
  ratchet that never loosens silently.
- Round 1 removed about 2,450 net lines. Deleting every comment that remains after it would
  remove about 45,500 more net lines from Go, shell, and Python, 12,700 of them from Go tests.
  `hack/commentlint delete` can remove about 44,700 of these on its own. The other 800 or so
  sit in 43 shell scripts that source other files; the tool refuses those, so they must be
  deleted by hand. The UI would lose about 3,000 more lines.
- New public API fields keep their doc comments, because those are the CRD's user-facing
  documentation. Every other new comment fails CI.
