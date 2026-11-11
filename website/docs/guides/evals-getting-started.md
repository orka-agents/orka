---
slug: /evals-getting-started
description: "Prove that a change to the PR review agent is better, with evidence, and record who approved it."
---

# Evaluate a reviewer change

You want to change the Agent that reviews your pull requests: a new prompt, a different
model, other tools. This guide shows how to find out, with evidence, whether the change
is better than what runs today, and how to record a person's decision about it.

**Input**

- **A candidate Agent:** the changed reviewer, as an Agent next to the current one.
- **Cases:** pull requests with known answers. Orka ships twelve in
  `test/evals/pr-review/cases.jsonl`: six with a real defect and six clean changes.
- **Graders:** code that scores each review. `verdict` checks the review's decision,
  `localization` checks it points at the defect's lines, `contract` checks the output
  format, and `honesty` checks it doesn't claim tests it never ran.
- **A gate:** the thresholds a candidate must meet, set by an operator in a protected
  ConfigMap the candidate's author can't change.

**Outcome**

- **A verdict:** `Passed`, `Failed` or `Incomparable`.
- **An evidence record** that outlives the runs: every trial and grade, the token
  spend, and digests of the Agents, prompt, cases, graders and gate it used, including
  the reviewer it would replace, which is the rollback target.
- **A decision:** a person approves or rejects the experiment. Decisions are
  append-only, and an approval goes stale as soon as anything it was based on changes.

Approving records the decision; it doesn't change the reviewer. Promoting the
approved candidate writes its prompt and model onto the reviewer, and a rollback
writes back what the promotion replaced.

## 1. Turn on evals

Install or upgrade Orka with evals on, naming the ConfigMap that will hold the gate and
the group whose members may approve:

```bash
helm upgrade --install orka ./manifest_staging/charts/orka \
  --namespace orka-system --reuse-values \
  --set controller.evals.enabled=true \
  --set-string controller.evals.gateConfigMap=orka-protected-set \
  --set 'controller.evals.promoterGroups={orka-promoters}'
```

Approving needs a person signed in through OIDC whose token carries a `groups` claim.
Set `controller.oidc.issuer` and `.audience` (and `.jwksUrl` if the issuer serves no
discovery document), list who may sign in with `.allowedSubjects`, and set
`.namespace` to the namespace your evals run in; OIDC users can't reach any other.
See [API authorization](../reference/api-authorization.md). ServiceAccounts and
transaction-token callers can run evals but can never decide.

People who run and read evals need `create`, `get`, `list` and `delete` on `evalruns`
in that namespace. The chart's `orka-client` Role grants them:

```bash
kubectl -n orka-system create rolebinding eval-users --role=orka-client --user=alice --user=bob
```

## 2. Set the gate and the held-out cases

The protected set names the case ConfigMaps that judge candidates (`split: heldOut`)
and the gate. Only operators should be able to write it:

```yaml title="protected-set.yaml"
apiVersion: v1
kind: ConfigMap
metadata: {name: orka-protected-set, namespace: orka-system}
data:
  protectedSet.yaml: |
    schemaVersion: orka.protectedSet.v1
    paths: [".github/**"]
    caseSets:
      - namespace: orka-system
        name: pr-review-heldout
        split: heldOut
        baselineMonitors: [fixtures]  # monitors whose own reviewer may run these cases as a baseline
    gate:
      maxInfraErrorPercent: 10        # either run may lose at most this share of trials to infra errors
      maxCandidatesPerBaseline: 2     # candidates one baseline run may judge
      maxExperimentsPerHeldOutSet: 2  # experiments each held-out case may take part in
      minTargetImprovementPoints: 5   # the target pass@1 must rise by at least this much
      significance: 0.05              # one-sided paired sign test, shared across those experiments
      maxGuardrailDropPoints: 5       # how far any other grader's pass@1 may fall (default 0: any drop fails)
```

```bash
kubectl apply -f protected-set.yaml
kubectl -n orka-system create configmap pr-review-heldout \
  --from-file=cases.jsonl=test/evals/pr-review/cases.jsonl
```

Each held-out case can only judge `maxExperimentsPerHeldOutSet` experiments, counted by
the commits it reviews, so renaming, reordering or copying cases doesn't make them
fresh. When the budget is spent, add new cases in a new ConfigMap and list it instead.

A run without a candidate reviews with its monitor's own reviewer, which could be any
Agent. On held-out cases, including copies of them, such a run must use a monitor named
in `baselineMonitors`; any other monitor can only run training or unrelated cases.

## 3. Describe the current reviewer

Evals review with a RepositoryMonitor's settings and its reviewer Agent. A suspended
monitor is enough; it never polls:

```yaml title="reviewer.yaml"
apiVersion: core.orka.ai/v1alpha1
kind: Agent
metadata: {name: reviewer, namespace: orka-system}
spec:
  model: {name: "openai/gpt-4o-mini", contextWindow: 65536, maxTokens: 4096}
  systemPrompt:
    configMapRef: {name: reviewer-prompt, key: prompt.md}
  runtime:
    contractVersion: orka.harness.v2
    type: opencode
    defaultAllowBash: false
    defaultAllowedTools: [read, glob]
---
apiVersion: core.orka.ai/v1alpha1
kind: RepositoryMonitor
metadata: {name: fixtures, namespace: orka-system}
spec:
  provider: github
  repoURL: https://github.com/orka-agents/orka-eval-fixtures
  branch: main
  suspend: true
  agents:
    reviewer: {name: reviewer}
```

Trials clone the public fixture repository anonymously and read-only. They get no
repository, publication or forge credential, and post nothing.

## 4. Run the baseline

```yaml title="baseline.yaml"
apiVersion: core.orka.ai/v1alpha1
kind: EvalRun
metadata: {name: review-baseline, namespace: orka-system}
spec:
  subject:
    type: repositoryMonitorReview
    repositoryMonitorReview:
      monitorRef: {name: fixtures}
  cases:
    configMapRef: {name: pr-review-heldout, key: cases.jsonl}
  trials: 3
  maxConcurrentTrials: 6
  graders: [verdict, localization, contract, honesty]
```

```bash
orka eval run -f baseline.yaml
```

You don't need to wait for it before starting candidates.

## 5. Run a candidate

Create the changed Agent, then an EvalRun that reviews with it (`agentRef`) and names
the baseline. Everything else must match the baseline: cases, trials, concurrency,
timeout and graders.

```yaml title="candidate.yaml"
apiVersion: core.orka.ai/v1alpha1
kind: EvalRun
metadata: {name: review-careful, namespace: orka-system}
spec:
  subject:
    type: repositoryMonitorReview
    repositoryMonitorReview:
      monitorRef: {name: fixtures}
      agentRef: {name: reviewer-careful}
  cases:
    configMapRef: {name: pr-review-heldout, key: cases.jsonl}
  trials: 3
  maxConcurrentTrials: 6
  graders: [verdict, localization, contract, honesty]
  experiment:
    hypothesis: The careful prompt on a larger model reviews more accurately.
    baselineRunRef: {name: review-baseline}
    target: {grader: verdict}
```

Pick the target with care. `verdict` over every case rewards a reviewer that catches
defects *and* passes clean changes. A target on the `defect` tag alone rewards one that
blocks everything: a reviewer that flags every pull request scores perfectly there. The
other graders still guard against regressions, but only the target has to improve.

```bash
orka eval run -f candidate.yaml --wait
```

`--wait` follows the run, waits for the baseline and the decision, prints the verdict,
and exits non-zero unless the experiment passed. The candidate spends one attempt from
each budget before its first trial; deleting it doesn't give the attempt back.

A candidate passes when:

- the target pass@1, over trials graded in both runs, rose by at least
  `minTargetImprovementPoints`;
- a paired sign test over those trials is at or below
  `significance / maxExperimentsPerHeldOutSet`;
- no other grader's overall pass@1 fell by more than `maxGuardrailDropPoints`. Models
  occasionally produce a malformed review: one in 36 trials is a 2.8-point drop on
  `contract`, which a zero tolerance treats as a regression;
- neither run lost more than `maxInfraErrorPercent` of its trials to infrastructure
  errors.

Size the run for the change you want to detect. The sign test only counts trials where
the two reviewers disagree, and the level it must meet shrinks as
`maxExperimentsPerHeldOutSet` grows. With twelve cases and three trials each, even a
candidate that wins 19 disagreements and loses 8 isn't significant at `0.05 / 4`. More
trials per case, or more cases, buy that power.

If the runs can't be compared fairly, for example because their cases or trial plans
differ, the verdict is `Incomparable`.

### Or let Orka propose the candidate

Instead of writing the candidate yourself, a proposer can learn one from the cases the
current reviewer gets wrong. It reads only training cases, and its candidate is then
judged on held-out ones it never saw.

1. List a training case set in the protected set, next to the held-out one, and load it:

   ```yaml
   caseSets:
     - {namespace: orka-system, name: pr-review-train, split: train}
     - {namespace: orka-system, name: pr-review-heldout, split: heldOut, baselineMonitors: [fixtures]}
   ```

2. Create the proposer Agent, and turn proposals on:

   ```yaml title="proposer.yaml"
   apiVersion: core.orka.ai/v1alpha1
   kind: Agent
   metadata: {name: proposer, namespace: orka-system}
   spec:
     model: {name: "openai/gpt-4.1", contextWindow: 65536, maxTokens: 8192}
     runtime:
       contractVersion: orka.harness.v2
       type: opencode
       defaultAllowBash: false
       defaultAllowedTools: [read]
   ```

   ```bash
   helm upgrade orka ./manifest_staging/charts/orka -n orka-system --reuse-values \
     --set-string controller.protectedSetConfigMap=orka-protected-set \
     --set-string controller.evals.proposerAgent=proposer \
     --set 'controller.evals.proposerModels={openai/gpt-4.1}'
   ```

   Leave `proposerModels` empty to allow prompt changes only.

3. Run the current reviewer on the training cases, then ask for a proposal:

   ```bash
   orka eval run -f training.yaml --wait     # a plain run on pr-review-train
   orka eval propose review-training
   orka eval proposals review-training
   ```

The proposer is a read-only Task with no workspace, no Kubernetes or Orka API token, and
no tools beyond reading. It gets the reviewer's current prompt and model, the failed
training trials with their pull requests, expected verdicts and grader reasons, and the
hypotheses already tried. It answers with one hypothesis and a new system prompt or
model. Orka checks the answer strictly, then creates the candidate Agent,
`reviewer-c-<digest>`, and a baseline and a candidate run on the held-out set.
`orka eval proposals` shows them; follow the candidate run as in step 5. Each training
run allows `maxCandidatesPerBaseline` proposals.

## 6. Read the result

```bash
orka eval get review-careful        # per-grader results and the experiment verdict
orka eval experiments               # every experiment and the decision in force
orka eval decisions review-careful  # outcome, evidence, rollback target, decisions
```

Read the failed trials' transcripts before trusting a score:
`orka eval get review-careful --output-dir ./results` writes them under `events/`.

## 7. Approve or reject

A promoter needs two things: membership in a group named by
`controller.evals.promoterGroups`, and the `decide` verb on `evalruns`, which the
chart's `<release>-controller-cluster-eval-promoter` ClusterRole grants. The `admin`
role doesn't include it, and a ServiceAccount is refused even with it.

```bash
kubectl create clusterrolebinding alice-eval-promoter \
  --clusterrole=orka-controller-cluster-eval-promoter --user=alice
```

Signed in as that person:

```bash
orka eval approve review-careful --reason "Catches the path and unit bugs; no regressions."
orka eval reject review-skim --reason "Misses most defects."
```

An approval needs a `Passed` experiment that still matches the live configuration. If
anyone edits either Agent, the prompt, the monitor, the cases, the graders or the gate,
or moves the cases out of the held-out set, `orka eval decisions` reports the approval
as stale and names what changed. Run a new experiment for the changed configuration.

## 8. Roll it out

Start with a canary, then promote the approval. The same promoter role grants the
`promote` verb on `agents`, which covers every rollout action:

```bash
orka eval decisions review-careful                         # the approval's decision ID
orka agent canary reviewer --decision <decision-id> --percent 10
orka agent promotions reviewer                             # the canary, and every action so far
orka agent canary reviewer --decision <decision-id> --percent 50   # widen it
orka agent promote reviewer --decision <decision-id>
```

During a canary, each new Session, or review without one, goes to the candidate when a
hash of its name falls below the percent. A Session keeps the arm it started on, and a
conversation that was already running stays on the reviewer. EvalRun trials are never
routed. Before each review goes to the candidate, Orka checks again that the approval
is current. If that check fails, reviews run on the reviewer and `orka agent promotions`
shows `RolloutInvalid`. To stop a canary:

```bash
orka agent abort reviewer --reason "Noisier than the reviewer."
```

A Session that started on the candidate can't continue after an abort; start a new one.

Promotion writes the candidate's system prompt (inline) and model onto the reviewer
the experiment's baseline ran, and nothing else. It's refused if:

- the approval isn't the latest decision for its experiment;
- the approval has gone stale;
- the candidate EvalRun is gone;
- the reviewer changed after its baseline run;
- the candidate differs from the reviewer in more than its prompt and model;
- the candidate isn't labelled `orka.ai/candidate-of: <reviewer>` and owned by the
  reviewer. Proposals create candidates that way. For one you wrote yourself, add the
  label and owner reference before the experiment runs.

The reviewer's generation moves, so the same approval can't be promoted twice.
Running reviews finish on the configuration they started with.

To undo it:

```bash
orka agent rollback reviewer --reason "More false positives in production."
```

Rollback writes back the prompt and model the promotion replaced. If anyone edited
the reviewer after the promotion, rollback is refused rather than overwriting that
edit.

Experiments, outcomes and decisions stay in Orka's store after their EvalRuns are
deleted, so the record of what was tried, what passed and who approved it remains.
