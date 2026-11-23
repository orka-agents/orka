# Policy contract

This page states how Orka treats rules that are written ahead of a run: which tools an execution may use, which calls need a human approval, and the limits an operator places on them. It describes what the code does today and proposes principles for new policy work to follow. It adds no field, resource, setting or behaviour.

Each statement is marked **today** (it describes current code, with a reference) or **proposed** (a rule for new work), or both. Code references are at commit `d31cf6b`.

Only the two "today" parts of statement 7 are pinned by tests: a native AI Job's approval set is fixed when the Job is built, and is kept when that Job is recovered; and a new Job is built from the current approval list, unless a soul binding stops it from being issued. The [tests](#tests) section lists them. The rest of this page is proposed, and open to correction.

The current inputs behind statement 7 are described in [Policy inputs: approvals on the native AI path](policy-inputs.md).

## Terms

- A *choice* is what a writer asks for, such as a Task's tool list or an Agent's defaults.
- An *injected grant* is something Orka adds itself, such as the coordination, messaging and memory tools (`internal/aitools/resolver.go:271-285`).
- An *Orka default* is what an omitted choice resolves to, such as the installation's worker image. It is judged as part of the resolved choice.
- A *bound* is a *limit* (the most anything below it may be allowed) or a *requirement* (something that must hold below it).
- An *execution* is one dispatch of a Task. For a native AI Task it is one Job, identified by the Task UID and the Job UID.
- *Issuance* is a native Job being created or a coding-agent session being bound.

## Statements

1. **Bounds grant nothing.** *(proposed)* What an execution may do comes from choices and injected grants. A bound can only remove or condition.

2. **A limit is judged against the resolved choice.** *(today for the Agent ceiling; proposed for other limits)* A Task whose resolved choice exceeds a limit is accepted as an object and fails at issuance (for a coding agent, at binding), with a reason. This contract adds no refusal when the object is written; another design, such as the catalog in #159, may add one using the same evaluator. An over-limit choice is not trimmed to fit (`docs/adr/0048-agent-runtime-ceilings.md:28-35`).

   Two things are not "the choice": coordination tools Orka injected, which are not counted unless the Task asked for them by name (`internal/controller/agent_ceiling.go:139-148`), and an optional gateway reply grant, which is left out when adding it would break the ceiling (`internal/controller/acp_gateway_reply.go:49-62`).

   "Resolved" includes what a missing choice resolves to: a Task with no deadline is judged as having none. A limit never supplies a default. A `type: ai` Task that selects no tool still gets its Agent's selections and Orka's injected grants and nothing else, so treating an allowlist as a default would add tools nobody chose.

   *(proposed)* An injected grant that a namespace limit covers is left out and reported, as the optional gateway reply grant already is. A limit refuses, and never trims, a writer's choice.

3. **Every applicable requirement must hold.** *(proposed)* Requirements from several writers accumulate, and removing one leaves the others in force. There is no "stricter wins", because requirements are not ordered.

4. **A bound is a question an enforcement point answers with evidence.** *(proposed)* The answers are *satisfied*, *not satisfied*, *unsupported* (this execution kind cannot say) and *pending* (the evidence is not available yet).
   - These four are what an enforcement point returns. A report may keep the reason apart (for example "unsupported" as insufficient evidence or as an unclassified operation), and may add states where no answer was asked: started before the bound, outside its scope, or unknown.
   - At dispatch the decision is whether work may start. At use it concerns one action.
   - Enforced: *not satisfied* and *unsupported* stop it. *Pending* waits, and carries a reason, the condition that would end the wait, and an expiry after which it counts as not satisfied. Orka already does this for a missing tool policy (`internal/controller/task_controller.go:1652-1666`).
   - Audited: nothing is stopped. Repeated reconciles must not inflate what is reported. The unit that is counted is left to the audit design: two actions in one Job can get different answers under one rule, and a refusal before dispatch has no Job to count against.
   - When a pending answer expires and an enforced bound refuses, the reason reported is still "evidence unavailable", not a finding that the rule was broken.

5. **A bound is judged against the execution, not the request.** *(today for connector writes; proposed in general)* An operation the execution was never offered satisfies a requirement about that operation (`internal/controller/job_builder.go:1308-1315`). The absence of a tool *name* does not show the absence of a capability: the Codex runtime runs shell commands whatever its tool list says (`internal/controller/agent_ceiling.go:165-176`).

6. **Unevaluated operations are refused, where Orka mediates them.** *(proposed)* If an operation passes through an Orka enforcement point and has not been evaluated against an applicable enforced bound, it is refused. For an operation a runtime performs itself there is no such point, so the launcher needs trusted evidence of confinement, or the execution does not start under that bound. With no applicable enforced bound, this statement has no effect.

7. **The unit is the dispatch.**
   - *(today)* A native AI Job's approval set is fixed when the Job is built, from the Agent's approval list plus the connector write tools the Job is offered (`internal/controller/job_builder.go:1239-1241`, `:1294-1315`, `:1363-1367`). When reconciliation finds the Job already exists and adopts it, the original Job UID and its issued approval set are kept, even though a fresh candidate was built (`internal/controller/task_controller.go:1851-1948`, `internal/controller/task_job_identity.go:84-95`), and the connector API reads the approval list from that Job's environment (`internal/api/internal_connector_tools.go:1041-1077`).
   - *(today)* Whether a new Job can be issued at all is a separate question from what it would be issued. A Task with a soul binding is bound to an Agent revision: an incompatible change to that revision, including any Agent spec edit that advances its generation, fails preparation with "create a new Task" before a second Job exists (`internal/controller/ai_soul.go:79-89`; `internal/agentcontext/soul.go:230-242`). This page records that behaviour and does not propose keeping it. Where a new Job is issued, it is built from the current approval list.
   - *(proposed)* The revisions of every applicable bound are sampled at the last decision before a Job or binding is issued, after the Task's own bindings are honoured, and stamped on the issued object. An edit after that read applies to later decisions. If a bound cannot be satisfied, the refusal names it. If a Task binding failed first, that is what is reported. A dispatch with no stamp is "unknown", which is not "no bounds".

   Two limits on the "today" part:
   - An approval list only exists on an Agent with enabled autonomous coordination: any other `type: ai` Task whose Agent has one fails (`internal/controller/task_controller.go:4170-4172`), and such a Task is not offered connector write tools (`internal/controller/job_builder.go:1308-1315`). The list cannot name a built-in tool (`internal/controller/task_controller.go:4173-4175`). The statement is about the set a Job is issued, which can be empty.
   - A name in the issued set shows issuance, not that a call was held. An earlier matching decision can answer a call without a fresh prompt (`workers/ai/approval_gate.go:232-257`).

8. **A check about an action uses the dispatch's frozen facts and refreshes its live prerequisites.** *(proposed)* A check with no live prerequisite can be answered from the authenticated record alone. Some decisions happen before any dispatch exists, such as a capacity wait (`internal/controller/task_controller.go:971-992`), and have no record to consult.

9. **Scope follows the policy subject.** *(proposed)* A bound applies to executions whose Task or AgentHost object is in its namespace, whatever namespace the Pod runs in; an agent host's Pod runs in a separate one (`internal/controller/agenthost_controller.go:129-144`). A bound names the execution kinds it covers. "Outside its scope" and "in scope, compliance unknown" are different: under an enforced bound the second prevents a new execution.

10. **Bounds apply to every execution, however it was created.** *(proposed)* A child's authority comes from its Agent, its Task's choices, its credentials and any grant it carries, inside the bounds. It is not a subset of its parent's: a planner with no shell may ask a coder that has one. *(today)* The coordination settings a child is checked against come from its immediate parent's Agent, and the check also admits an Agent the parent created at run time (`internal/controller/task_controller.go:1277-1334`). Narrowing a delegation grant along a whole chain, as gateway passes do (`internal/controller/gateway_task_pass_lineage.go:159-188`), is separate design work and not part of this contract.

11. **Who asked is recorded where it can be verified, and "unknown" is not "nobody".** *(today, where a requester is sealed)* The requester is an identity chain sealed at each hop (`internal/api/internal_requester_stamp.go:22-65`). *(proposed)* A record names a verified initiator if there is one and the acting service if it is known, and otherwise says "no verified human requester". A person may have authorized a schedule; an identity that cannot be verified does not show that no person was involved.

12. **An unreadable source holds dispatch; an empty or disabled one does not.** *(proposed)* A policy source that is not enabled constrains nothing. A successful lookup that finds nothing constrains nothing. An enabled, enforcing source that cannot be read holds dispatch and says why. A source known to be audit-only never changes the outcome, readable or not.

## Not decided

- Lossless evidence. A record of what a dispatch was issued is best effort: a deduplicated event that can be rebuilt from the Job. A durable handoff before the last copy is deleted, and recording the principal that performed each action, belong to #1162.
- Whether a bound can follow delegated work into another namespace.
- What happens to executions already running when a bound starts being enforced, beyond reporting them as started before the bound.
- A bound on spend (tokens, cost) during a run. That needs a meter, and belongs to #159 and #541.

## Tests

These tests pin the "today" part of statement 7 for native AI Jobs. They use fake clients and change no behaviour.

| Case | Test | Expected |
| --- | --- | --- |
| A tool in the Agent's approval list | `TestJobBuilder_BuildWithOptions_NativeApprovalSet/tool_in_the_agent_approval_list_requires_approval` | it is in the Job's approval set |
| A connector write on a read-write link | `.../offered_connector_write_requires_approval` | it is offered and in the approval set |
| A connector write; the Agent is not autonomous | `.../non-autonomous_task_is_not_offered_the_connector_write` | it is not offered and not in the approval set |
| A connector write; the link is read-only | `.../read-only_link_hides_the_connector_write` | it is not offered |
| A connector write also in the Agent's list; the Agent's entry is removed | `.../connector_write_stays_required_after_the_agent_list_drops_it` | it stays in the approval set |
| A tool is added to the Agent's list between two Jobs for one Task | `TestJobBuilder_BuildWithOptions_NewJobUsesCurrentAgentApprovalList/tool_added_between_renders` | the first Job does not require it and the second does |
| A tool is removed between two Jobs | `.../tool_removed_between_renders` | the first Job requires it and the second does not |
| A soul-bound Task whose Agent spec is edited | `TestPrepareAISoul_AgentEditAfterBindingFailsPreparation` | preparation fails with "create a new Task" |
| `createTaskJob` finds the recorded Job after the Agent's list changed | `TestCreateTaskJob_AdoptsRecordedJobWithItsIssuedApprovalSet` | the original Job UID and its issued approval set are kept |

Run them with:

```bash
make ensure-ui-embed
go test ./internal/controller/ -count=1 -run \
  '^(TestJobBuilder_BuildWithOptions_NativeApprovalSet|TestJobBuilder_BuildWithOptions_NewJobUsesCurrentAgentApprovalList|TestPrepareAISoul_AgentEditAfterBindingFailsPreparation|TestCreateTaskJob_AdoptsRecordedJobWithItsIssuedApprovalSet)$'
```
