---
name: pr-closeout
description: Drive a GitHub pull request to merge-ready after implementation or review. Use automatically after creating or updating an agent-authored PR unless the user opts out or the PR is intentionally draft/WIP; also use when asked to fix merge conflicts, make CI green, handle unresolved PR review comments, reply to or resolve review threads, push PR branch updates, or repeat until a PR is green and review-clean.
---

# PR Closeout

Drive the current GitHub PR from “has feedback or failing checks” to “currently merge-ready.” Run this automatically after creating or updating an agent-authored PR unless the user opts out or the PR is intentionally draft/WIP. This orchestrates git, GitHub review threads, CI logs, local verification, and optional `$autoreview`; it is not a replacement for `$autoreview`.

## Contract

- Treat CI failures and review comments as signals, not truth. Verify each against the real code path, workflow logs, and current PR diff before changing code.
- Reject stale, duplicate, speculative, invalid, or regression-causing feedback with concise GitHub evidence instead of making unnecessary changes.
- Prefer small fixes at the right ownership boundary. Do not do unrelated cleanup or broad refactors.
- If CI/comment-triggered fixes change code, rerun relevant local checks, push, and rerun the closeout snapshot until no current actionable blockers remain.
- Stop when the PR currently has no merge conflicts, required checks are green, no unresolved actionable review threads remain, and `reviewDecision` is non-blocking. Do not keep polling just to get nicer wording.

## Guardrails

- Treat automatic post-PR closeout or the user’s closeout request as the scope. Fix merge conflicts, CI failures, and unresolved actionable review feedback only.
- Creating or updating an agent-authored PR authorizes normal closeout writes for that PR: push fixes to the non-main PR branch, reply on GitHub with fix/pushback evidence, and resolve review threads after replying when they are addressed. A request like “reply and resolve each comment,” “push updates,” or “drive this PR until green” authorizes the same writes.
- Do not submit reviews, merge, enable auto-merge, retarget the PR, force-push, amend, rebase, or perform destructive git operations unless explicitly asked or the branch owner’s workflow clearly requires it.
- Never push directly to `main`. For PR branches, commit with `git commit -s` when a commit is needed, then push the current branch.
- Redact secrets from logs and summaries. Do not paste tokens, auth URLs, JWTs, TxTokens, cookies, or credentials.
- Say “currently no unresolved actionable review threads remain,” not “reviewers will have no more comments.” Future review activity cannot be guaranteed.

## Snapshot Helper

Use the Bash helper for the first state read and after every push or GitHub write batch:

```bash
.agents/skills/pr-closeout/scripts/pr_snapshot.sh --summary
```

For a specific PR:

```bash
.agents/skills/pr-closeout/scripts/pr_snapshot.sh --pr 123 --summary
```

The helper is read-only and reports local branch/head state, PR metadata, unresolved review threads, all-check buckets, required-check buckets, PR state, and blocker booleans. Required-check buckets drive `failingChecks`/`pendingChecks`; optional check states are informational until classified. Review-comment bodies are intentionally omitted from helper output to avoid logging secrets. If all-check or required-check lookup reports no checks on a recently updated PR, the helper treats that as pending/unknown so post-push CI attachment races do not look green. Non-open PRs are blockers. It requires `git`, `gh`, and `jq`. If it fails because of auth, missing tools, or unsupported `gh` behavior, report the blocker and fall back to explicit `gh pr view`, `gh pr checks`, and GraphQL review-thread reads.

## Pick PR Target

1. Use the PR supplied by the user when provided.
2. Otherwise resolve the current branch PR.
3. Fetch the PR base and head, using the PR’s actual base branch rather than assuming `main`.
4. Before editing, confirm the checkout is on the PR head branch and commit: `git branch --show-current` should match `headRefName`, and `git rev-parse HEAD` should match `headRefOid` after fetch. If not, check out the PR head branch or stop and report why it cannot be checked out safely.
5. If the checkout has dirty unrelated user changes, stop before destructive operations and ask for scope.
6. For fork PRs or branches that cannot be pushed by the current remote, report the limitation before editing.

## Workflow

1. Build a live closeout snapshot.
   - Use `.agents/skills/pr-closeout/scripts/pr_snapshot.sh --summary` first.
   - Separate blockers into: wrong local branch/head, dirty worktree, merge conflicts, failing PR-tied checks, pending/queued checks, unresolved actionable threads, stale/invalid threads needing a reply, ambiguous threads needing human clarification, blocking review decision, and external/human-only blockers.
   - For CI, use `github:gh-fix-ci` guidance when available: inspect GitHub Actions checks and logs with `gh`; treat external providers as report-only unless their logs are accessible and relevant.
   - For review comments, use `github:gh-address-comments` guidance when available: use thread-aware review data (`reviewThreads`, `isResolved`, `isOutdated`, path/line anchors), not only flat PR comments.

2. Resolve merge conflicts first.
   - Prefer the least surprising branch update for the repository. If no project convention is clear, merge the latest PR base into the PR branch rather than rebasing/force-pushing.
   - Resolve conflicts narrowly, preserving both sides’ intended behavior where possible.
   - Run focused verification for conflicted areas before addressing unrelated CI or review feedback.

3. Address actionable review threads.
   - Cluster related threads by behavior or file so one focused fix can close multiple comments.
   - Keep each change traceable to a thread or feedback cluster.
   - If the right response is explanation rather than code, draft or post a concise reply with evidence.
   - If a comment is outdated because later code already fixed it, reply with the commit/file evidence before resolving.
   - If comments conflict or imply a product/design change, surface the tradeoff instead of guessing.

4. Fix CI failures.
   - Inspect failing job logs before editing. Do not infer the cause from the check name alone.
   - Prefer the smallest fix that addresses the observed failure and the PR diff.
   - If a failure is flaky, external, or unrelated to the PR, document the evidence and do not create speculative code churn.
   - Run the focused local command that corresponds to the failed job when practical.

5. Verify locally.
   - Follow repo-specific verification from `AGENTS.md` for the files changed.
   - After Go edits, normally run `make lint-fix && make test` or a justified focused equivalent.
   - After UI edits, run `cd ui && bun run lint && bun run test` or a justified focused equivalent.
   - After workflow edits, run actionlint as specified by the repo.
   - If fixes are non-trivial code changes and the user did not opt out, run `$autoreview` according to repo policy. Do not run `$autoreview` merely because this skill was invoked.

6. Commit and push PR updates when authorized.
   - Review `git diff` and `git status` before committing.
   - Use a Conventional Commit subject and `git commit -s`.
   - Push only the PR branch, never `main`.
   - After pushing, rerun `.agents/skills/pr-closeout/scripts/pr_snapshot.sh --summary`; do not assume GitHub accepted the update or that checks attached to the new head.

7. Reply and resolve threads when authorized.
   - Reply on GitHub before resolving each thread so reviewers can see what happened.
   - For each addressed thread, reply with a short, specific note: what changed, where, and what verification ran.
   - For invalid/stale/no-longer-valid comments, reply with the reason and evidence.
   - Resolve only threads after replying, and only when they are fixed, stale, invalid, duplicates, or intentionally superseded. Leave ambiguous or product-decision threads open and report them.
   - Re-query thread state after replies/resolutions; do not rely on local bookkeeping.

8. Repeat until no current blockers remain.
   - Rerun the snapshot after every push or GitHub write batch.
   - If new reviewer comments arrive during the loop, classify and address them like the first batch.
   - If blocked, report the exact blocker: failing external check, missing GitHub auth, ambiguous review request, required human approval, branch protection, or reviewer decision not yet updated.

## Bounded CI Waiting

- Treat queued, pending, or running checks as healthy progress when timestamps or job state are moving.
- Poll at reasonable intervals while the current run set is active. Do not spin indefinitely after checks stay queued with no logs, after only external/human-only blockers remain, or after a stable pass/fail state is reached.
- If a check is still queued/pending after a bounded wait and has no failure logs, report it as a current GitHub runner/external pending blocker rather than making speculative code changes.
- Do not push empty commits or rerun expensive/live checks just for reassurance unless the user asks.

## Final Report

Include:

- PR URL/number and branch handled.
- Head SHA and commits pushed, if any.
- Merge conflict summary, if applicable.
- CI checks inspected and final status, including pending/queued external blockers.
- Review threads addressed, pushed back, resolved, or left open with reasons.
- Local verification commands run.
- `$autoreview` status if it was required and run, or why it was intentionally not run.
- Current merge-readiness status, including review-decision state, and any remaining human/external blockers.
