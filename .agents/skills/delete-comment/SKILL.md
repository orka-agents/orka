---
name: delete-comment
description: "Run a round of Orka's comment cleanup: classify every comment in a scope as Tautology, Stale, Excuse, or Decision Record, delete them, file issues for Excuses, move Decision Records into an ADR, and lower the comment ratchet. Use when asked to delete comments, shrink the comment baseline, or continue the comment cleanup."
---

# Delete comments

Orka bans code comments ([ADR 0062](../../../docs/adr/0062-code-carries-no-comments.md)).
Comments that existed before the ban are held by a ratchet that only goes down:

- Go, shell, and Python: `hack/commentlint/baseline.json`, enforced by
  `go test -count=1 ./hack/repocheck` (CI: Repository Rules) and `make lint-comments`.
- UI TypeScript: the `orka/no-comments` ESLint rule (`ui/eslint-rules/no-comments.js`)
  with ESLint bulk suppressions in `ui/eslint-suppressions.json`, enforced by `bun run lint`.

Both fail when a file gains a comment and when a file loses one without the baseline
being lowered. `make comment-baseline` lowers both and refuses to raise either.

Exempt, never counted: compiler and tool directives (`//go:`, `//nolint`, `// +kubebuilder`
and other `// +` markers, `#nosec`, `# shellcheck`, `# noqa`, `eslint-disable`, ESLint `/* eslint */` and `/* global */` config, `@ts-expect-error`,
`/// <reference>`), license headers, `Deprecated:` notices, cgo preambles, Go example `Output:` blocks, generated files, `testdata/`, the vendored `.agents/skills/autoreview`, and doc
comments on types and fields under `api/`, because controller-gen turns those into CRD
descriptions that `kubectl explain` shows users.

## 1. Pick a scope

```bash
go run ./hack/commentlint stats                     # whole repository
go run ./hack/commentlint stats internal/llm cmd/cli  # candidate scope
```

Take whole packages, about 400–600 comment lines per reviewer. In zsh, split a
space-separated variable with `${=paths}`. For UI files, list the comments with
`cd ui && echo '{}' > /tmp/no-suppressions.json && bunx eslint --suppressions-location /tmp/no-suppressions.json -f json <files>`
and filter on `orka/no-comments`.

## 2. Classify every comment

```bash
mkdir -p /tmp/delete-comment
go run ./hack/commentlint list -context 4 <paths...> > /tmp/delete-comment/<scope>-comments.jsonl
```

Each line has `file`, `line` (first line of the group), `endLine`, `kind` (`doc`, `line`,
`trailing`), `text`, and `context` (the next code lines). Read the surrounding code, not just
the context, before deciding. Choose exactly one category:

| Category | Means | Action |
| --- | --- | --- |
| `tautology` | Restates what the code, name, or signature already says ("Foo returns the foo"; "// Labels" above a label block; "increment i"). | Delete. |
| `stale` | Describes code, flags, files, behavior, or plans that no longer exist or no longer match. Confirm with `rg`/`git log -S` before choosing this. | Delete. |
| `excuse` | Admits or rationalizes a shortcut: TODO/FIXME/HACK/XXX, "for now", "temporary", "workaround", "best effort", "good enough", "we don't bother", "ignore the error because", "should eventually", a duplicated copy "kept in sync by hand". Name the concrete hack in the code. | Delete and file an issue for the hack. |
| `decision` | Records a non-obvious, load-bearing *why*: a security or fail-closed invariant, a protocol or compatibility constraint, an ordering or fencing requirement, a deliberately rejected alternative, a limit and its reason. Removing it would let a future change silently break the system. | Move into an ADR, then delete. |
| `functional` | Deleting it changes behavior: tools or tests read its text, or it is a directive the scanner does not exempt yet. | Keep; report it so the exemption list in `hack/commentlint/scan/scan.go` or `ui/eslint-rules/no-comments.js` grows. |

Doubt between `tautology` and `decision`: ask whether a careful engineer reading only the code
would make a wrong change without this comment. If yes, it is a `decision`. Doubt between
`decision` and `excuse`: if the comment defends behavior that the code should not have, it is
an `excuse`.

Write one JSON object per comment to `/tmp/delete-comment/<scope>.jsonl` (outside the repo):

```json
{"file":"internal/x/y.go","line":42,"category":"tautology","reason":"restates the function name"}
{"file":"internal/x/y.go","line":57,"category":"excuse","reason":"...","issue":{"title":"x: retry Foo instead of sleeping 2s","body":"..."}}
{"file":"internal/x/y.go","line":90,"category":"decision","reason":"...","decision":{"symbol":"x.Reconciler.claim","title":"Claims are fenced by generation","text":"Standalone prose; names the symbol and the constraint, and does not depend on the deleted comment."}}
```

## 3. File issues for Excuses

Merge Excuses that describe the same hack. Search for duplicates first
(`gh issue list --state all --search "<keywords>"`), then file one issue per distinct hack with
`gh issue create --label refactor` (add an area label such as `llm`, `gateway`, `workers`, or
`storage` when one fits). The body names the file and symbol, quotes the deleted comment, says
what the shortcut is and what fixing it means, and links a permalink pinned to the current
commit SHA. Never paste secrets, tokens, or private repro details into an issue.

## 4. Move Decision Records into an ADR

ADRs are never rewritten, so each round adds a new record. Find the next free number across
every ref, since unmerged branches already hold numbers:

```bash
git fetch -q origin
git for-each-ref --format='%(refname)' refs/heads refs/remotes \
  | xargs -I{} git ls-tree --name-only {} docs/adr/ 2>/dev/null \
  | grep -oE '^docs/adr/[0-9]{4}' | sort -u | tail -1
gh pr list --state open --limit 500 --json number,files \
  --jq '.[] | .number as $n | .files[].path | select(startswith("docs/adr/0")) | "\($n) \(.)"' \
  | sort -k2 | tail -3   # numbers held by fork PRs
```

Write `docs/adr/NNNN-code-comment-decisions-<scope>.md` in the existing ADR format (Date,
Status: Accepted, Context, Decision, Consequences). Under Decision, add one subsection per
package with one bullet per recovered decision, each naming its symbol. Add the record to
the index in `docs/adr/README.md`. If a decision obviously belongs to an existing ADR, still
record it in the new ADR and link the older one; do not edit the older ADR.

## 5. Delete and lower the ratchet

```bash
go run ./hack/commentlint delete -from /tmp/delete-comment/<scope>.jsonl   # skips functional
make comment-baseline
```

`delete` removes each comment group that starts at `file:line`, keeps directives in the same
group, and gofmts Go files. It finds shell comments with the `mvdan.cc/sh` parser and Python
comments with `python3`'s `tokenize`, so Python 3.12 or newer must be on `PATH` as `python3`. A file is written only if
its comment-free form is unchanged: the same Go tokens, the same minified shell syntax tree,
or the same Python tokens and AST. It also refuses:

- symlinked paths
- two comments that start on the same line
- a comment that follows a line continuation
- any shell script that mentions `alias` or runs `alias`, `source`, `.`, or `eval` under any
  quoting, because an alias can change how the script parses
- any shell script with a command name built from an expansion such as `$cmd` or `$(...)`,
  because it cannot be resolved statically and might be `source`

Delete a refused comment by hand and check the code around it. Run it once per scope against an unmodified tree, because line
numbers shift after deletion. Delete UI comments by hand, keeping any JSX expression valid
(remove the whole `{/* ... */}`), then run `make comment-baseline`.

## 6. Verify

```bash
gofmt -l <paths>; go build ./... && go vet ./...
go test -count=1 ./hack/repocheck
go test ./<scope packages>/...
bash -n <changed scripts>; cd ui && bun run lint && bun run test   # when UI changed
```

`git diff` must show only removed comment lines, gofmt realignment, the ADR, its index entry,
and the lowered baselines. Then follow the repository's review and PR closeout rules.
