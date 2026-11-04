# ADR 0040: Separate proposer, evaluator and promoter identities for self-improvement

Date: 2026-11-01

## Status

Proposed. Defines the trust boundaries for the self-improvement loop in #985 and is the
design source for #992 to #997. Builds on the EvalRun API proposed in #976, which is
being built in parallel; where this record adds to EvalRun, the additions are optional
fields on that resource, not a new one. Depends on #986 for repair path limits. Leaves
the scout (#987 to #990) unchanged: it needs no new boundary.

## Context

The loop in #985 lets agents propose changes to Orka and prove them with evidence, while
people decide what ships. If the agent that proposes a change can also grade or approve
it, it will learn to game the grade. The roles must be separated by mechanisms Orka or
the forge enforces, not by prompt instructions.

What Orka already enforces, verified on `main` at 2fc0e83a1:

- **No Kubernetes credential in the agent process.** RuntimePool Pods set
  `AutomountServiceAccountToken: false` (`runtimePoolPodTemplate` in
  `internal/controller/runtime_pool_controller.go`). Their NetworkPolicies deny all
  traffic except DNS, the provider proxy and the controller (`runtimePoolNetworkPolicies`,
  same file). Web access exists only through the brokered `web_search` and `web_fetch`.
- **No forge credential in the agent process.** `readCredentialRef`,
  `publicationCredentialRef`, `publicationReadCredentialRef` and `forgeCredentialRef`
  are resolved only by the clean-room workspace and publication boundary
  (`api/v1alpha1/task_types.go`, `internal/publisher/doc.go`). The publisher pushes one
  Orka-owned commit with an exact compare-and-swap to a branch claimed by one Task or
  Session (`api/v1alpha1/branch_claim_types.go`).
- **Brokered tools are an allowlist frozen per prompt.** The Task's allowed tools are
  digested into the runtime profile (`buildMCPPolicyConfigurationWithRegistry` in
  `internal/controller/acp_mcp_policy.go`), and the broker rejects a call whose policy,
  fence or operation capability does not match (`ACPMCPBroker.ServeHTTP` in
  `internal/controller/acp_mcp_broker.go`). The production broker registry is fixed in
  `cmd/main.go`: web tools, coordination tools (`delegate_task`, `wait_for_tasks`,
  `run_validation`, messages, memory, `search_transcript`), `reply_in_conversation`, and,
  with connectors on, GitHub tools that act as the requester's linked account.
- **Publication path checks run outside the agent.** The ACP supervisor, which runs
  under a different UID from the agent's processes, rejects a workspace delta that
  leaves `allowedPaths` or, with `denyRepositoryControlPaths`, touches `.github/workflows/`, `config/rbac/` or chart
  secrets (`workers/acp/supervisor/prompt_handlers.go`). The RepositoryMonitor
  implementation result is checked again by the controller and rejected with
  `patch_path_denied` (`internal/controller/repositorymonitor_issue_actions.go`). The
  control-path list is hard-coded in both places. Repair Tasks set neither field (#986).
- **Agent configuration is pinned per Task.** Every agent Task gets a write-once
  `AgentExecutionBinding` with the Agent's UID and generation, an immutable snapshot
  digest and the runtime profile digest
  (`api/v1alpha1/agent_execution_binding_types.go`,
  `internal/controller/agent_execution_binding.go`). The profile digest covers the
  resolved Agent configuration, including the system prompt, the model and the tool
  policy (`internal/controller/acp_runtime_profile.go`), and RuntimePools are keyed by
  it. Editing an Agent therefore sends new Tasks to a new pool while running Tasks finish
  on the old one, which drains before it is removed. Runtime images are digest-pinned per
  provider by controller configuration, not per Agent.
- **Issue commands are bound to a snapshot.** A RepositoryMonitor command records the
  actor, their permission and the issue snapshot digest. If the issue changed since,
  the command is blocked with `stale_command_snapshot`. Mutating commands need
  `requireActorPermission`, `write` by default (`internal/store/monitor_types.go`,
  `internal/controller/repositorymonitor_issue_actions.go`,
  `api/v1alpha1/repositorymonitor_types.go`).
- **Custom verbs authorize store-backed actions.** The API authorizes
  `POST /api/v1/memory-proposals/:id/apply` with a SubjectAccessReview for the verb
  `apply` on `memoryproposals` (`internal/api/external_authorization.go`).
- **Controller-only Task metadata.** Admission rejects writes to controller-managed Task
  annotations from anyone but the controller (`internal/admission/task_provenance.go`).

What is missing: a protected set wider than the three hard-coded control paths, any way
to keep evaluation cases from a proposer, evidence that ties a decision to exactly what
was tested, and a promotion path for Agent configuration with a canary and rollback.

## Decision

### Three identities

| Identity | Who | Can | Cannot |
|---|---|---|---|
| Proposer | An agent Task the controller creates and marks with the controller-managed annotation `orka.ai/improvement-role: proposer` | Read a pinned clone; return one structured candidate; publish to its own task branch when the flow allows it (#997); create memory proposals | Hold any Kubernetes or forge credential; read held-out cases, held-out trial rows or any evaluation transcript (it receives only the controller-rendered training-split summaries described under #994); publish a change to the protected set; start Tasks, evaluations or promotions; post to the forge |
| Evaluator | The EvalRun controller and API server, running as the controller ServiceAccount | Create candidate Agents from a validated candidate; run trials; write trial and experiment records; compute the gate | Change a candidate after the run starts; record a decision; promote |
| Promoter | A person bound to the new `orka-eval-promoter` role: a user from the cluster's user authentication or Orka's sign-in, in a configured promoter group; never a ServiceAccount, a workload identity, or a caller authenticated by a transaction or context token | Approve or reject one experiment; start, widen, finish or abort a canary; roll back | Edit evidence; promote without an approved experiment that still matches |

Every rule has one enforcement point:

| Rule | Enforcement point | State |
|---|---|---|
| No Kubernetes credential in a proposer | RuntimePool Pod spec, `runtime_pool_controller.go` | Exists |
| No forge credential in a proposer | Clean-room credential refs, `task_types.go`, `internal/publisher` | Exists |
| No external network except the broker; only DNS, provider-proxy and controller API egress are allowed | RuntimePool NetworkPolicies | Exists |
| Proposer cannot start Tasks, evaluations, validations or forge writes | Proposer allowlist (below) frozen into the profile, enforced by `ACPMCPBroker.ServeHTTP` | Allowlist is new, #992 |
| Proposer annotation is set only by the controller | `controllerManagedTaskAnnotationKeys`, `internal/admission/task_provenance.go` | New key, #992 |
| A proposer Task that lost its constraints does not run | Dispatch-time check in `queueACPRuntimeTask` (`acp_task_queue.go`), the earliest point with the frozen tool policy; a second gate in `task_controller.go` fails marked Tasks that would run as Jobs | New, #992 |
| Proposer cannot publish protected paths | Supervisor delta check, and again in the publisher's prepare step (`internal/publisher`) before any commit or bundle exists | New `deniedPaths`, #992 |
| Proposer cannot push to the default or release branches | Controller-derived push branch under the fixed prefix `orka/`, branch claims, plus forge branch protection | Prefix check new, #992 |
| Proposer cannot read cases or trial transcripts | Separate namespace; no web, memory-recall or transcript tools; private fixture repository | New, #992 |
| Only the evaluator writes trial and experiment records | Store writes only from the controller; no write route in the API | #976, #993 |
| Decisions are human and append-only | `decide` verb SubjectAccessReview, only human promoters accepted (see the Promoter row), insert-only table; handlers in `internal/api/eval*.go`, routes in `internal/api/server.go` | New, #993 |
| Promotion needs a matching approved experiment | `promote` verb SubjectAccessReview and a digest check in the rollout handler (`internal/api/agent_rollout*.go`, routed in `internal/api/server.go`) | New, #995 |
| Only the controller routes canary traffic | `Agent.status.rollout`; the editor role has only `get` on `agents/status` (`config/rbac/agent_editor_role.yaml`) | New, #995 |

Kept human-only: changes to the protected set, new promotable fields or candidate kinds,
and raising any cost cap. Each is a reviewed code or configuration change, and the paths
that hold them are in the protected set.

### Protected set (#992)

**Declaration.** One ConfigMap in the controller namespace, named by the new controller
flag `--protected-set-configmap`, owned by cluster operators and changed only through
the reviewed deployment configuration. The declaration lives under the data key
`protectedSet.yaml`, and every section, including `gate`, is required: an incomplete
declaration is invalid, and with no valid declaration no proposer Task runs. Patterns
match case-insensitively, like the built-in repository-control paths. Format, with
example values:

```yaml
schemaVersion: orka.protectedSet.v1
paths:                         # always denied to proposers, in every repository
  - .github/**
  - config/rbac/**
  - charts/**
repositories:
  - repository: github.com/orka-agents/orka
    paths:                     # added for this repository
      - internal/evals/**      # graders and the gate
      - test/evals/**
      - internal/api/*authoriz*
      - internal/api/auth*.go                          # authentication
      - internal/api/context_token*.go                 # transaction and context tokens
      - Makefile                                       # the test and lint targets CI runs
      - .golangci.yml
      - internal/api/server.go                         # route registration
      - internal/api/eval*.go                          # proposal and decision handlers
      - internal/api/agent_rollout*.go                 # rollout handler
      - internal/admission/**
      - internal/outboundaccess/**
      - internal/tools/**                              # brokered tool registry and implementations
      - cmd/**                                         # wiring of the registry, broker and flags
      - internal/controller/acp_mcp_broker*.go         # proposer tool allowlist
      - internal/controller/acp_mcp_policy*.go         # compiles the tool policy
      - internal/controller/acp_runtime_profile*.go    # pins the policy into the profile
      - internal/controller/acp_dispatcher*.go         # dispatch-time checks
      - internal/controller/acp_task_queue.go          # where the dispatch check runs
      - internal/controller/acp_publication*.go        # publication handoff
      - internal/controller/task_controller.go         # Job-path gate
      - internal/protectedset/**                       # protected-set parsing
      - internal/workspacedelta/**                     # path matching
      - internal/harness/v2/workspace.go               # limits carried to the supervisor
      - internal/acp/runtime_defaults.go
      - internal/labels/labels.go                      # the role annotation key
      - config/samples/orka_protected_set.yaml         # this declaration
      - docs/adr/**                                    # the table the drift test reads
      - internal/controller/agent_execution_binding*.go # canary routing
      - internal/controller/evalrun*.go                # evaluator
      - internal/controller/runtime_pool_controller*.go # token and network isolation
      - internal/controller/repositorymonitor_issue_actions.go # implementation limits
      - internal/controller/repositorymonitor_repair.go # repair limits
      - internal/publisher/**                          # credential and publication boundary
      - internal/store/**                              # trial, decision and promotion records
      - workers/acp/supervisor/**                      # deniedPaths check
      - api/v1alpha1/**                                # the fields that carry these rules
      - AGENTS.md
caseSets:                      # case ConfigMaps by split
  - {namespace: orka-evals, name: pr-review-train,   split: train}
  - {namespace: orka-evals, name: pr-review-heldout, split: heldOut}
gate:                          # improvement thresholds, see #994
  maxInfraErrorPercent: 10
  maxCandidatesPerBaseline: 3
  maxExperimentsPerHeldOutSet: 10   # then the held-out case set must be rotated
  minTargetImprovementPoints: 5      # target pass@1 must rise by at least this many points
  significance: 0.05                 # one-sided paired test, divided by maxExperimentsPerHeldOutSet
```

Every file that implements an enforcement point in the table above belongs in the
protected set for the Orka repository. #992 adds a test that fails when an enforcement
point's file is missing from the shipped example, so the two lists cannot drift apart. The
test reads the file paths straight from the table's "Enforcement point" column, so adding a
row without protecting its file fails the build.

The controller validates it and records its digest. A missing or invalid ConfigMap
disables proposer flows; it never means "nothing is protected". Resources are protected
by construction, since the proposer holds no Kubernetes credential: Agents, EvalRuns,
case ConfigMaps, RBAC, Secrets, OutboundAccessPolicies and RuntimePools.

**New Task field: `spec.workspace.deniedPaths`.** A list of globs with the same syntax
as `allowedPaths`, rejected before publication even when `allowedPaths` matches. It is
needed because `allowedPaths` cannot express "everything except". It is carried to the
supervisor in the harness workspace limits (`internal/harness/v2/workspace.go`) and
checked next to `denyRepositoryControlPaths` in
`workers/acp/supervisor/prompt_handlers.go`. The publisher checks the changed paths
again in its prepare step, before any commit or bundle exists, so a compromised runtime
Pod cannot publish a protected path on its own. (The controller never reads the delta
artifact, so it cannot do this check before creating the Publication.) A rejection uses `patch_path_denied`.

**Proposer Tasks.** The controller creates them, sets the role annotation, and sets:

- `denyRepositoryControlPaths: true` and `deniedPaths` from the protected set, for every
  write-intent proposer;
- an explicit allowed-tools list: native workspace tools and `propose_memory`, nothing
  else brokered. No `delegate_task`, `run_validation`, web tools, `recall_memory`,
  `search_transcript` or connector tools;
- credential references only as the existing builders set them today. They stay with
  the clean-room publication boundary and never reach the agent;
- for every write-intent proposer, a push branch under the fixed prefix `orka/`. No
  monitor is known at dispatch, so a monitor whose `branchPrefix` is outside `orka/`
  cannot run proposers until it changes. A read-only proposer, such as the #994 pilot,
  gets no push branch and no publication credential, and its workspace changes are
  never published;
- no Session continuation and no fork of another Task, so no earlier transcript reaches
  the proposer.

At dispatch, a Task with the role annotation that lacks any of these that apply to its
intent (read-only or write), or whose
`deniedPaths` no longer covers the current protected set, fails with
`ProposerConstraintViolation` instead of running.

**Held-out cases.** Three rules keep them unreadable:

1. The controller refuses to create a proposer Task in a namespace that holds an EvalRun
   or a case set listed in the protected set. Memory and transcript tools are scoped to
   a namespace, so even a misconfigured tool list cannot reach evaluation transcripts.
   For #997 this means the RepositoryMonitor that implements issues lives in a different
   namespace from the one whose reviewer is evaluated.
2. Held-out fixtures live in a private fixture repository. Only trial Tasks get a
   `readCredentialRef` for it, resolved by the clean-room boundary. #976's public fixture
   repository holds training cases only.
3. The proposer prompt is built by the controller from training-split trial records.
   Held-out records are never rendered into it.

**Branch protection.** Before enabling a proposer flow on a repository, the controller
reads the branch rules for the default branch with its existing forge read credential
and refuses unless a pull request with review is required **and** stale approvals are
dismissed when new commits are pushed. Independently of that setting, the controller
treats a code candidate as approved only when an approving review's commit equals the
current pull request head, so an approval never carries over to a later push. Bypass
lists cannot be read that way; people audit them when enabling the flow.

### Evidence records (#993)

An experiment is two EvalRuns, not a new resource: a baseline run and a candidate run
over the same held-out cases, created together so they run under the same provider and
infrastructure conditions. The controller computes one regression trial plan (the
ordered list of case ID and trial index pairs over the cases both runs share) for the
experiment and writes the same plan and its digest into both runs, so the two runs can
never disagree on assignment. For a new capability the candidate run also gets a
separate criteria trial plan over the criteria cases only, with its own digest; the
baseline run has none. Two optional fields are added to the EvalRun spec from #976. Both are
immutable, like the rest of the spec:

```yaml
spec:
  subject:
    type: repositoryMonitorReview
    repositoryMonitorReview:
      monitorRef: {name: orka-monitor}
      agentRef: {name: reviewer-c-3f9a1c2b7d}   # new: use this Agent instead of the monitor's reviewer
  experiment:                                   # new
    hypothesis: "The reviewer misses defects in generated code paths."
    baselineRunRef: {name: pr-review-x-baseline}   # always set; see "New capabilities" below
    target: {grader: localization, tag: defect}    # improvement experiments only
    criteria:                                   # frozen copy of the thresholds or approved criteria
      source: protectedSet | issue
      digest: sha256:...                        # protectedSet: its `gate`; issue: the normalized criteria block
      # new capabilities only, frozen by the maintainer who writes the criteria cases:
      caseSetRef: {name: issue-1234-criteria}   # held-out ConfigMap with the cases and thresholds
      caseSetDigest: sha256:...                 # digest over that ConfigMap's cases and thresholds
      thresholds:                               # copied from the ConfigMap; schema-validated
      - {tag: criteria, grader: contract, minPassAt1Percent: 90, minPassHatKPercent: 80}
```

The controller fills `status.experiment`:

```yaml
status:
  snapshot:          # from #976, plus two digests
    graderSetDigest: sha256:...      # each Go grader declares a version; digest over (name, version)
    protectedSetDigest: sha256:...
  experiment:
    candidateDigest: sha256:...      # digest of the candidate's promotable fields
    result: Passed | Failed | Incomparable
    reasons: [...]
```

- **Incomparable** when the two runs differ in anything other than the candidate's
  promotable fields: case-set, grader-set or criteria digest, regression trial-plan digest (the
  trial count and the assignment of shared case IDs to trial indexes; outcomes are
  excluded; the candidate-only criteria plan is not compared), monitor
  UID and generation, review prompt template digest, runtime image, or the subject. For a new capability the case sets differ by design:
  the check instead requires the candidate's cases to be exactly the baseline's case set
  plus the cases in `criteria.caseSetRef`, whose content matches `criteria.caseSetDigest`, and compares the two
  runs only on the shared regression subset. Subjects are compared after removing
  `repositoryMonitorReview.agentRef`, the one field that is expected to differ; every
  other subject field must match. Fail closed.
- **New capabilities** have nothing to improve on, so their experiment differs in two
  ways. The paired baseline run covers only the existing regression case sets, and the
  candidate run covers those plus the held-out criteria cases. The regression cases are
  gated against the baseline as below; the criteria cases are gated against the absolute
  `criteria.thresholds`. The maintainer who writes the criteria cases also writes these
  thresholds, per grader and tag, into the same held-out ConfigMap, and the controller
  validates them against a schema when the candidate run starts. The protected set's
  `gate` applies only to improvement experiments. `target` is unset.
- **The bound tuple.** Everything an approval depends on, recorded at decision time and
  recomputed every time the approval is used:
  - the candidate Agent's UID, generation and promotable-field digest;
  - the stable Agent's UID, generation and promotable-field digest, as the paired
    baseline run snapshotted them;
  - the resolved configuration digest of each arm: the prompt text after resolving any
    `configMapRef`, the model, the provider and the profile digest;
  - the runtime image digest both runs used;
  - the monitor's UID and generation and the review prompt template digest that #976
    snapshots, since the controller-built review prompt and validation settings shape
    behavior outside the Agent;
  - the case-set, grader-set, criteria text and protected-set digests, and for new
    capabilities the criteria case-set digest, which covers the thresholds.

  To keep the candidate's resolved configuration fixed, the controller always writes the
  candidate's prompt inline, even for a model-only patch, so a mutable `configMapRef`
  cannot change what was tested.
- **What is kept.** The trial records from #976, plus an append-only `eval_decisions`
  table in the same store: id, experiment (the candidate run), decision (`approved` or
  `rejected`), actor, reason, time, and the bound tuple. Records of experiment runs survive EvalRun deletion and its TTL, so rejected
  experiments stay queryable. Later proposers see only each earlier experiment's
  hypothesis, its training-split results and whether it was approved or rejected. They
  never see held-out trial rows or held-out aggregate numbers, which would otherwise let
  repeated experiments tune candidates to the held-out set. For the same reason a
  held-out case set has a usage budget, `gate.maxExperimentsPerHeldOutSet`, counted
  across all baselines; once spent, the controller refuses new experiments on that set
  until an operator rotates in a new one. The budget is keyed by a **case-content digest**
  over the canonical cases alone, never thresholds or other metadata, so editing
  thresholds or renaming the ConfigMap does not reset it; rotation means new case content.
- **Who writes.** Only the EvalRun controller writes trial records and `status`. A
  decision is written through `POST /api/v1/evals/runs/:name/decisions`, authorized by a
  SubjectAccessReview for the verb `decide` on `evalruns`. Every promoter route
  (`proposals`, `decisions`, `rollout`) accepts only a human promoter: it refuses
  `system:serviceaccount:` usernames, any request authenticated by a transaction token
  or context token, and any user not in a group listed by the new controller flag
  `--eval-promoter-groups`. An empty list refuses everyone, so the feature is off until
  an operator names the groups. It refuses an approval when the gate did not pass
  or the recomputed tuple no longer matches; a rejection is always accepted. A new ClusterRole, `orka-eval-promoter`, grants the `propose` and `decide`
  verbs on `evalruns` and `promote` on `agents`, and is not aggregated to edit or
  admin. A decision is never updated; a later decision supersedes it. **The effective
  decision for an experiment is always its latest one.** An approval is usable only
  while it is the latest decision for its experiment, so a later rejection revokes it
  even though the earlier row remains.
- **What invalidates an approval.** A later decision on the same experiment, or any
  change to an element of the bound tuple: a new generation of either Agent, different
  promotable fields or resolved configuration on either arm, a different runtime image,
  case set, grader set, criteria or protected set. For code candidates, a new commit on
  the pull request head, enforced by the exact-head check above.

### Proposer flow (#994)

Piloted on the PR reviewer's Agent configuration only.

1. A person runs a baseline EvalRun on the training split.
2. A promoter calls `POST /api/v1/evals/runs/:name/proposals` (SubjectAccessReview verb
   `propose` on `evalruns`; like every promoter route, it refuses
   `system:serviceaccount:` usernames even if the role is bound to one). The controller creates one read-only proposer Task in the
   proposer namespace. Its prompt holds the failed training trials' case summaries and
   grader reasons, earlier experiment summaries (training-split results and decisions
   only), and the current promotable fields. It
   refuses after `gate.maxCandidatesPerBaseline` proposals for the same baseline.
3. The proposer returns `orka.improvement.candidate.v1`:
   `{hypothesis, agentPatch: {systemPrompt?, model?}}`. The promotable fields in v1 are
   `spec.systemPrompt` (inline) and `spec.model.name` under the same provider. The
   controller validates the result as it validates `orka.prReview.v1`. A result that
   touches anything else is dropped and recorded.
4. The controller creates the candidate Agent: the stable Agent with the patch applied,
   named `<agent>-c-<digest prefix>`, labelled `orka.ai/candidate-of`, and
   owner-referenced to the stable Agent. It then creates the paired baseline and
   candidate EvalRuns on the held-out split, with `criteria` copied from the protected
   set's `gate`.
5. Gate: `Failed` if, for any grader and tag shared with the baseline, candidate pass@1
   or pass^k is below the baseline; if the target does not improve with enough evidence
   (improvement experiments: target pass@1 must rise by at least
   `gate.minTargetImprovementPoints`, and a one-sided paired sign test over the target
   cases, comparing each case's candidate and baseline pass counts, must be significant
   at `gate.significance / gate.maxExperimentsPerHeldOutSet`. Every experiment on a
   held-out case set, across all baselines, spends from that one budget, and proposers
   see earlier decisions, so the correction covers the held-out set's whole lifetime
   rather than one baseline's candidates; a lucky candidate does not become more likely
   to pass by trying again. Too few cases to
   reach significance fails with `insufficient_evidence`) or any criteria tag misses its threshold (new
   capabilities); or if infrastructure errors exceed the threshold. A person then records a decision either way.

Cost: one proposer Task per call with the controller's timeout and turn limit, the
per-baseline candidate cap, and #976's 500-trial cap per run. Orka has no per-Task
token cap today; spend is recorded from `/api/v1/usage/work/:id`, not enforced.

Code candidates, such as changes to the review prompt built in
`buildRepositoryMonitorReviewPrompt`, are not promoted in-cluster. They go through #997
and a normal pull request. Their evidence is an EvalRun run by #976's workflow against
the pull request head, linked from the pull request.

### Promotion with canary and rollback (#995)

Agent generations, profile digests and draining pools already give pinning, isolation
and drain-then-replace. Three pieces are missing, all added without a new resource:

- **New endpoint** `POST /api/v1/agents/:name/rollout` with
  `{action: canary | promote | abort | rollback, decisionID, percent}`, authorized by a
  SubjectAccessReview for the verb `promote` on `agents`, granted by
  `orka-eval-promoter`. ServiceAccount usernames are refused. `canary` and `promote`
  require that `decisionID` is the latest decision for its experiment, that it approves,
  and that its recomputed bound tuple still matches. The destination must be the
  candidate's owner (`orka.ai/candidate-of` and the owner reference) and must still
  match the baseline half of the tuple: if anyone edited the stable Agent after the
  baseline run, the request is refused and a new experiment is needed. `promote` updates
  the stable Agent with a `resourceVersion` precondition, so a concurrent edit makes it
  fail instead of merging into a configuration neither arm tested.
- **New status field `Agent.status.rollout`**:
  `{candidate: {name, uid, generation, digest}, percent, decisionID, recordID}`. Only the
  controller can write it. At binding (`agent_execution_binding.go`), the arm is chosen
  once per Session: a Task that starts a new Session, or a standalone Task, goes to the
  candidate when a hash of that Session's or Task's UID modulo 100 is below `percent`. A
  Task that continues an existing Session always takes the arm its Session already has,
  because Session configuration is frozen (`internal/store/kube/session_control.go`
  rejects a different configuration digest). After an abort or rollback, continuing a
  candidate-arm Session therefore fails with the existing `SessionConfigurationMismatch`
  and needs a new Session. Tasks owned by an EvalRun are never routed: the evaluator
  binds each trial directly to the Agent its EvalRun froze, so a canary cannot
  contaminate a baseline or candidate run. The binding then records the candidate's UID and generation through
  the existing fields, and the controller-managed annotation `orka.ai/rollout-arm`
  records which arm the Task took. Before routing each new Task to the candidate,
  binding checks that `status.rollout.decisionID` is still the latest decision for its
  experiment, that it approves, and that the full bound tuple still matches, including
  the candidate's UID. If any check fails, binding sends the Task to the stable Agent,
  sets a `RolloutInvalid` condition, and keeps doing so until a promoter aborts or
  starts a new canary. A protected-set or criteria change, or a later rejection,
  therefore stops a canary even though no Agent changed.
- **Promotion log** in the store. Every rollout action (`canary`, `promote`, `abort`,
  `rollback`) is a record that moves through `pending → applying → complete`, or ends
  `failed` or `canceled`; every transition is a compare-and-swap on the record's state.
  A CAS on one record does not fence a second record, so the store also allows at most
  one open (`pending` or `applying`) record per stable Agent UID, enforced by a unique
  partial index. An action first inserts its record; if another action on the same Agent
  is open, the insert fails and the request is refused with a conflict. This serializes
  canary changes, promotions, aborts, rollbacks and `status.rollout` cleanup per Agent.
  The one exception is `abort` against an open promotion, which acts on that record
  instead of inserting its own, as described below.
  Every action marks its own write so a restart can tell whether it committed: `promote`
  and `rollback` set the controller-managed annotation `orka.ai/promotion-record: <record
  id>` in the same fenced spec update, and `canary` and `abort` set
  `Agent.status.rollout.recordID` in the same status update (`abort` clears the rest of
  `status.rollout` but keeps that field). Admission rejects writes to the annotation from
  anyone but the controller, as it does for controller-managed Task annotations. On
  restart, an open record whose ID the Agent carries in that place has committed, and
  the controller marks it `complete`; for `promote` and `rollback` it records the applied
  generation as the stored generation plus one and the applied digest as the target
  digest, without reading values a later edit may have changed. An open record whose ID
  the Agent does not carry is retried only if every precondition of its own action still
  holds; otherwise it is marked `failed` without changing the Agent. No record stays open
  across a restart, so the per-Agent index cannot wedge rollouts.
  `promote` refuses, before inserting its record, a candidate whose resolved promotable
  values equal the stable Agent's current spec: a spec-neutral update would change only
  the marker and leave the generation where it was. Every recorded promotion therefore
  changes the spec and bumps the generation by exactly one, and so does its rollback,
  whose compare-and-swap requires the live Agent to still hold the promoted values,
  which differ from the previous ones.
  `promote` runs in three durable steps so a crash cannot lose the rollback record:
  (1) insert a `pending` record holding the decision, the Agent's `resourceVersion` and generation,
  the previous values with the prompt *resolved to text* (so rollback does not depend on
  a ConfigMap that may change or disappear), and the target values; (2) move the record
  `pending → applying`, then update the stable Agent with a `resourceVersion`
  precondition, writing the candidate's inline prompt and the record's marker;
  (3) clear `status.rollout` and mark the record `complete` with the resulting generation
  and digest. On restart the controller reconciles any `pending` or `applying` record
  idempotently, deciding whether step 2 committed from the marker, never from the
  Agent's current values, which a direct edit could make look applied or unapplied. If
  the marker names this record, step 2 has committed and the change is live, so it
  always finishes step 3 and records the promotion as `complete`, even if a later
  rejection or a changed `status.rollout` has arrived since. It never marks such a record
  `failed`, because that would leave a live change without a record; a later rejection
  sets a `PromotedBeforeRejection` condition, and undoing the change is a `rollback`.
  Otherwise it never
  retries on the strength of the stored values alone: it re-runs every precondition of
  the original request (the decision is still the latest and approving, the full bound
  tuple matches, the Agent's `resourceVersion` equals the one stored in the pending
  record, and the live `status.rollout` still names the same decision and candidate) and
  retries step 2 only if all pass. If any fails, it marks the record `failed`
  without changing the Agent and sets `RolloutInvalid`. One consequence: after a promotion or rollback the stable Agent
  always carries an inline prompt, even if it used `configMapRef` before. `rollback`
  writes back the recorded resolved previous values, with its own marker. It needs no new evidence, but it
  is a compare-and-swap: the live stable Agent's generation and
  promotable-field digest must equal what the latest promotion record wrote, and the
  update carries a `resourceVersion` precondition. If anyone edited the Agent after the
  promotion, rollback is refused rather than overwriting that edit. `abort` clears the
  canary and moves a `pending` promotion record for that rollout to `canceled`, so a
  restart can never resume a promotion the promoter aborted. `abort` cannot cancel a
  record that is already `applying`: the CAS fails, abort waits for the record to reach
  `complete` or `failed`, and reports which. If the promotion completed, the change is
  live and recorded, and undoing it is a `rollback`, never a silent cancel.

Running Tasks keep their binding, so they finish on whatever they started with. Runtime
image changes stay on the release path, since images are per provider, not per Agent.

### Frozen acceptance criteria (#996)

No new API. The criteria are a fenced block in the issue body:

````markdown
```orka-acceptance-criteria
- ...
```
````

Approval is the existing implement command label (`orka:implement` by default), applied
by an actor with `requireActorPermission`. The command already records the actor and
the issue snapshot digest, and any later edit to the issue blocks it with
`stale_command_snapshot`. The new parts:

- implementation of an issue with the scout's `capability` kind label requires a
  criteria block, or it is blocked with `criteria_missing`;
- the controller records the criteria digest, the SHA-256 of the normalized block, in
  the command's action record and in a controller-managed annotation on the
  implementation Task;
- a maintainer, never the builder, writes cases and per-grader thresholds for the
  criteria into a held-out ConfigMap; creating that ConfigMap is the moment they are
  frozen. The candidate EvalRun sets `criteria.source: issue` with the criteria text
  digest, `caseSetRef` and `caseSetDigest`, and also
  runs the existing regression case sets. If the issue's current block no longer
  matches the recorded digest, the result is `Incomparable`. This needs an EvalRun
  Subject that fits the capability; #976 lists monitor-fix and generic Task Subjects
  as follow-ups, and #996 builds the first one it needs;
- proposals of kind `direction` are never built.

### Handoff to the implementation workflow (#997)

The implementation Task stays the one `createRepositoryMonitorIssueActionTask` builds,
with three changes: it carries the proposer role and constraints above, its prompt
includes the approved criteria block (never cases), and the pull request body includes
the criteria digest and a link to the evidence. People merge as usual. Requiring a plan
before implementation (`requirePlan`) stays on by default.

### Scout (#987 to #990)

No new boundary. The scout is a scheduled read-only Task (`orka.ai/agent-read-only`,
read workspace intent) whose allowlist names only workspace read tools, `web_search` and
`web_fetch`. Without a Kubernetes token, a network path or a forge credential, it can
only return a digest. The filer (#988) is trusted code outside the agent. It validates
`orka.scout.proposal.v1` and holds the only `issues: write` credential, a GitHub App
installation token for one repository. The filer drops any proposal whose sketch touches
the protected set.

## Alternatives considered

- **A new `ImprovementExperiment` resource.** It would duplicate EvalRun's snapshot,
  trial store and invalidation rules. Two optional EvalRun fields and one table cover it.
- **A new `AgentRollout` or versioned Agent resource.** Agent generations, binding
  snapshots and profile-keyed pools already pin and isolate configurations. Only the
  traffic share and the rollback target were missing.
- **Protected paths as a file in each repository.** The proposer edits that repository;
  the controller would have to read the file from a trusted ref and still need an
  operator-owned default. A ConfigMap the proposer cannot reach is simpler.
- **Enforcing the boundary with prompt instructions or reviewer judgment.** Not a
  boundary: the proposer is the party being checked.
- **Letting the proposer open its own pull requests through a connector tool.** That
  would give the agent process a forge write path; the monitor already holds that
  credential in the controller.
- **Automatic promotion for low-risk configuration.** Out of scope for #985. A narrow
  controller promoter can be considered later, and only through a new ADR.

## Consequences

- Proposer Tasks need a second namespace, a private fixture repository for held-out
  cases, and the protected-set ConfigMap before any proposer flow turns on. All flows
  are off by default behind controller flags.
- The Task CRD gains `spec.workspace.deniedPaths`; the harness workspace limits gain the
  same list, so supervisor and controller must be upgraded together.
- EvalRun gains `subject.repositoryMonitorReview.agentRef` and `experiment`; Agent gains
  `status.rollout`. The store gains `eval_decisions` and a promotion log, each needing a
  SQLite migration step.
- Experiment records do not expire. A retention policy is follow-up work.
- People with plain edit on Agents can still change the stable Agent directly. That is
  a human decision outside the promotion log, not a proposer path.
- Spend is recorded but not capped by tokens until Orka has a per-Task token budget.
- Only Agent configuration promotes in-cluster. Code changes, including the reviewer's
  built-in prompt and runtime images, promote through pull requests and releases.

## Implementation plan

1. #986: repair Tasks get path limits. Prerequisite for every write-intent proposer.
2. #976: EvalRun, trial store and grader versions (`graderSetDigest`), in parallel.
3. #992: the protected-set ConfigMap and flag, `deniedPaths` in the CRD, harness limits,
   supervisor and controller re-check, the role annotation in admission, the dispatch
   check, the proposer allowlist, the namespace rule and the branch-rule check. Tests
   cover each protected category and each read path for held-out cases.
4. #993: the EvalRun `agentRef` and `experiment` fields, comparability and gate
   computation, `eval_decisions`, the `decide` route, the `orka-eval-promoter` role and
   retention of experiment records.
5. #994: the `propose` route, the proposer Task builder, the candidate schema and
   validation, candidate Agent creation, paired runs. Pilot on the PR reviewer.
6. #995: `Agent.status.rollout`, binding-time routing, the rollout endpoint and the
   promotion log.
7. #996: the criteria block, `criteria_missing`, the criteria digest on the action
   record and the Task.
8. #997: the proposer constraints on implementation Tasks, criteria in the prompt, and
   the evidence link in the pull request body.

## Open questions

- Who besides maintainers holds `orka-eval-promoter`?
- Should a promoter be barred from deciding on an experiment they started?
- Should Agents under a rollout reject direct spec edits by anyone but the controller?
  Today such an edit is safe but disruptive: it invalidates the canary and any pending
  promotion, which then need a new experiment.
- Retention for experiment records, and whether to cap spend by tokens once Orka can.
