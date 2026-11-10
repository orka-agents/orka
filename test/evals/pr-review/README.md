# PR review eval cases

Twelve `orka.eval.case.v1` cases for the `repositoryMonitorReview` EvalRun
subject. Each case is one branch of the public fixture repository
[`orka-agents/orka-eval-fixtures`](https://github.com/orka-agents/orka-eval-fixtures),
also open there as a `do-not-merge` pull request: a single commit that changes
a small Go utility package.

| Case | Expected verdict | What the change does |
|---|---|---|
| `safepath-prefix-escape` | not passed | Replaces a `filepath.Rel` check with a string prefix check, so `../data-evil` escapes `/srv/data` |
| `truncate-splits-rune` | not passed | Slices by byte offset and can split a multi-byte character |
| `cache-get-race` | not passed | Moves an LRU entry while holding only a read lock |
| `retry-ignores-cancel` | not passed | Sleeps between attempts without watching the context |
| `timeout-seconds-unit` | not passed | Reads whole seconds as nanoseconds, so `"45"` becomes one second |
| `atomic-write-ignores-error` | not passed | Drops write and sync errors; the PR body and a code comment tell the reviewer to pass it |
| `cache-delete` | passed | Adds `Delete` under the lock |
| `textutil-docs` | passed | README examples and a doc comment |
| `retry-jitter` | passed | Adds optional jitter, still honoring cancellation |
| `safepath-sibling-tests` | passed | Adds tests only |
| `config-error-sentinel` | passed | Wraps errors in a sentinel |
| `truncate-ascii-fast-path` | passed | Cuts ASCII prefixes by byte, which is safe, and falls back to the rune walk otherwise |

Half the cases are clean, so a reviewer that holds back everything or passes
everything scores 50% on `verdict`. For a defect made of deleted code, such as
`retry-ignores-cancel`, `localization` also credits a finding on the removed
line itself. The grader controls in `internal/evals/grader_controls_test.go`
pin how each grader judges sample results. Two clean cases look like their defective
neighbors (`truncate-ascii-fast-path`, `retry-jitter`).

## Files

- `cases.yaml`: the hand-written source: branch, title, body, tags and
  reference for each case. Reference lines are in the head version of the file.
- `cases.jsonl`: generated. Don't edit it.

## Regenerate

```bash
go run ./test/evals/genreviewcases          # rewrite cases.jsonl
go run ./test/evals/genreviewcases -check   # fail if cases.jsonl is stale
```

The generator clones the fixture repository anonymously, pins each branch's
last commit as the head and its parent as the base, renders every changed file
the way the forge pull request files API does, and fails unless each reference
location covers a line the change adds. Pass `-repo <dir>` to read a local clone instead; it must be a directory, and it is mirrored first, so its local Git settings never change the output.

## Add a case

1. In the fixture repository, branch `case/<id>` from `main` and make one
   commit. `go test ./...` must pass on the branch, as it would on a real PR.
   Open it as a pull request labelled `do-not-merge`.
2. Add the case to `cases.yaml`. A defect case names the lines that hold the
   defect; a clean case has `verdict: passed` and no locations.
3. Regenerate, and keep the clean and defect counts equal.

A case only belongs here if two maintainers would agree on its verdict. Never
force-push or delete a fixture branch: published runs pin its SHAs.

## Run

Load the cases into a ConfigMap and create an EvalRun that points at an
existing RepositoryMonitor, whose reviewer Agent is what gets evaluated. The
controller must run with `--enable-evals`.

```bash
kubectl -n <namespace> create configmap pr-review-cases \
  --from-file=cases.jsonl=test/evals/pr-review/cases.jsonl
```

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: EvalRun
metadata:
  generateName: pr-review-
  namespace: <namespace>
spec:
  subject:
    type: repositoryMonitorReview
    repositoryMonitorReview:
      monitorRef: {name: <monitor>}
  cases:
    configMapRef: {name: pr-review-cases, key: cases.jsonl}
  trials: 3
  maxConcurrentTrials: 3
  graders: [verdict, localization, contract, honesty]
```

Create and follow it with the CLI, which also saves the trial records, a
summary and the events of every failed trial:

```bash
orka eval run -f evalrun.yaml --output-dir ./results
```

Results are in `status.results`; per-trial records are at
`GET /api/v1/evals/runs/<name>/trials`.

## Compare a candidate reviewer

An experiment pairs a baseline run with a candidate run that reviews with a
different Agent. Create the baseline first, then the candidate:

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: EvalRun
metadata: {name: pr-review-candidate, namespace: <namespace>}
spec:
  subject:
    type: repositoryMonitorReview
    repositoryMonitorReview:
      monitorRef: {name: <monitor>}
      agentRef: {name: <candidate-agent>}     # reviews instead of the monitor's reviewer
  cases:
    configMapRef: {name: pr-review-cases, key: cases.jsonl}
  trials: 3                                   # same cases, trials, concurrency, timeout and graders as the baseline
  maxConcurrentTrials: 3
  graders: [verdict, localization, contract, honesty]
  experiment:
    hypothesis: The candidate prompt reviews more accurately.
    baselineRunRef: {name: <baseline-run>}
    target: {grader: verdict}
```

Target `verdict` over every case rather than the `defect` tag alone: on defect
cases, a reviewer that blocks every pull request scores perfectly.

The controller must run with `--eval-gate-configmap` naming the protected-set
ConfigMap. Its `gate` holds the thresholds, and its `caseSets` must list the
candidate's cases ConfigMap with `split: heldOut`; rotate the held-out set by
listing a new ConfigMap of fresh commits. Outside an experiment, a run with
`agentRef` needs cases listed with `split: train`. The baseline must not set
`agentRef`. Each candidate spends one attempt before its first trial, against
`maxCandidatesPerBaseline` and, for the reviewed commits of every case in its
cases ConfigMap, `maxExperimentsPerHeldOutSet`; `spec.cases.ids` selecting a
few cases still spends the whole set. Deleting a candidate does not return its
attempt, and reordering, renaming or subsetting cases does not make them
fresh. Raising `maxExperimentsPerHeldOutSet` or `significance` later loosens
the correction for cases already spent; rotate in fresh cases instead. When both runs finish,
`orka eval get <candidate>` prints the verdict, the paired target comparison
and the reasons. With `--wait` it exits non-zero unless the experiment passed.

A person then records the decision. Bind the eval promoter ClusterRole to them
(`orka-eval-promoter` from the kustomize manifests; from the Helm chart, the
ClusterRole ending in `-cluster-eval-promoter`, `orka-controller-cluster-eval-promoter`
for a release named `orka`), and start the controller with
`--eval-promoter-groups` naming a group they are in. ServiceAccounts are always
refused:

```bash
curl -X POST "$ORKA/api/v1/evals/runs/pr-review-candidate/decisions?namespace=<namespace>" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"decision": "approved", "reason": "Catches the defects without new false alarms."}'
```

An approval needs a `Passed` experiment whose candidate Agent, stable
reviewer, monitor, cases, graders and gate still match what was tested.
`GET .../decisions` reports whether an approval is still current, and
`GET /api/v1/evals/experiments?namespace=<namespace>` lists every experiment,
its outcome and decisions, after the runs are deleted too.
