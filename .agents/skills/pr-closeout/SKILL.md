---
name: pr-closeout
description: Drive a GitHub pull request to merge-ready after implementation or review. Use when the user asks to fix merge conflicts, make CI green, handle unresolved PR review comments, reply to or resolve review threads, push PR branch updates, or repeat until a PR is green and review-clean.
---

# PR Closeout

Drive the current GitHub PR from “has feedback or failing checks” to “currently merge-ready.” This is an orchestration workflow over git, GitHub review threads, CI logs, local verification, and optional `$autoreview`; it is not a replacement for `$autoreview`.

## Guardrails

- Treat the user’s closeout request as the scope. Fix merge conflicts, CI failures, and unresolved actionable review feedback; avoid unrelated cleanup.
- Do not reply on GitHub, resolve threads, submit reviews, merge, enable auto-merge, or push unless the user explicitly authorized that write. A request like “reply and resolve each comment,” “push updates,” or “drive this PR until green” authorizes the matching writes.
- Never push directly to `main`. For PR branches, commit with `git commit -s` when a commit is needed, then push the current branch.
- Do not amend, rebase, force-push, retarget, or merge the PR unless the user specifically asks or the branch owner’s workflow clearly requires it.
- Redact secrets from logs and summaries. Do not paste tokens, auth URLs, JWTs, TxTokens, cookies, or credentials.
- Prefer evidence over deference: push back on review comments that are invalid, stale, duplicate, or would introduce a regression, and explain why with code/test evidence.
- Say “currently no unresolved actionable review threads remain,” not “reviewers will have no more comments.” Future review activity cannot be guaranteed.

## Workflow

1. Resolve the PR and branch state.
   - Use the PR supplied by the user, otherwise resolve the current branch PR with `gh pr view --json number,url,baseRefName,headRefName,headRepositoryOwner,mergeStateStatus,reviewDecision`.
   - Run `git status --short --branch` and `git remote -v`; stop before destructive operations if the checkout is dirty with unrelated user changes.
   - Fetch the PR base and head. Use the PR’s actual base branch, not an assumed `main`.

2. Build a live closeout snapshot.
   - Inspect mergeability, current review decision, unresolved review threads, and required/failing checks.
   - For CI, use `github:gh-fix-ci` guidance when available: inspect GitHub Actions checks and logs with `gh`; treat external providers as report-only unless their logs are accessible and relevant.
   - For review comments, use `github:gh-address-comments` guidance when available: use thread-aware review data (`reviewThreads`, `isResolved`, `isOutdated`, path/line anchors), not only flat PR comments.
   - Separate blockers into: merge conflicts, failing PR-tied checks, unresolved actionable threads, stale/invalid threads needing a reply, ambiguous threads needing human clarification, and external/human-only blockers.

3. Resolve merge conflicts first.
   - Prefer the least surprising branch update for the repository. If no project convention is clear, merge the latest PR base into the PR branch rather than rebasing/force-pushing.
   - Resolve conflicts narrowly, preserving both sides’ intended behavior where possible.
   - Run focused verification for conflicted areas before addressing unrelated CI or review feedback.

4. Address actionable review threads.
   - Cluster related threads by behavior or file so one focused fix can close multiple comments.
   - Keep each change traceable to a thread or feedback cluster.
   - If the right response is explanation rather than code, draft or post a concise reply with evidence.
   - If a comment is outdated because later code already fixed it, reply with the commit/file evidence before resolving.
   - If comments conflict or imply a product/design change, surface the tradeoff instead of guessing.

5. Fix CI failures.
   - Inspect failing job logs before editing. Do not infer the cause from the check name alone.
   - Prefer the smallest fix that addresses the observed failure and the PR diff.
   - If a failure is flaky, external, or unrelated to the PR, document the evidence and do not create speculative code churn.
   - Run the focused local command that corresponds to the failed job when practical.

6. Verify locally.
   - Follow repo-specific verification from `AGENTS.md` for the files changed.
   - After Go edits, normally run `make lint-fix && make test` or a justified focused equivalent.
   - After UI edits, run `cd ui && bun run lint && bun run test` or a justified focused equivalent.
   - After workflow edits, run actionlint as specified by the repo.
   - If fixes are non-trivial code changes and the user did not opt out, run `$autoreview` according to repo policy. Do not run `$autoreview` merely because this skill was invoked.

7. Commit and push PR updates when authorized.
   - Review `git diff` and `git status` before committing.
   - Use a Conventional Commit subject and `git commit -s`.
   - Push only the PR branch, never `main`.
   - After pushing, re-fetch PR state instead of assuming GitHub accepted the update.

8. Reply and resolve threads when authorized.
   - For each addressed thread, reply with a short, specific note: what changed, where, and what verification ran.
   - For invalid/stale/no-longer-valid comments, reply with the reason and evidence.
   - Resolve only threads that are fixed, stale, invalid, duplicates, or intentionally superseded. Leave ambiguous or product-decision threads open and report them.
   - Re-query thread state after replies/resolutions; do not rely on local bookkeeping.

9. Repeat until no current blockers remain.
   - Re-check mergeability, required checks, review decision, and unresolved review threads after every push or GitHub write batch.
   - If checks are queued or running, wait for the current run set to finish. Poll at reasonable intervals; do not spin indefinitely after a stable pass/fail state.
   - If new reviewer comments arrive during the loop, classify and address them like the first batch.
   - Stop when the PR currently has no merge conflicts, required checks are green, and no unresolved actionable review threads remain.
   - If blocked, report the exact blocker: failing external check, missing GitHub auth, ambiguous review request, required human approval, branch protection, or reviewer decision not yet updated.

## Final Report

Include:

- PR URL/number and branch handled.
- Commits pushed, if any.
- Merge conflict summary, if applicable.
- CI checks inspected and final status.
- Review threads addressed, pushed back, resolved, or left open with reasons.
- Local verification commands run.
- `$autoreview` status if it was required and run, or why it was intentionally not run.
- Current merge-readiness status and any remaining human/external blockers.
