---
description: Qualify a release candidate with chart installation, recovery, agent execution, and verified cleanup.
---

# Release automation and qualification

Start a release with **Prepare Release**, approve qualification,
then approve publication of the qualified candidate. Preparation, workflow
dispatch, tagging, image promotion, chart publication, and release evidence
archival use only `GITHUB_TOKEN`. The organization can keep its policy that prohibits Actions
from creating or approving PRs. Release preparation does not create a PR.

The flow is:

1. Dispatch `release-prepare.yml` from `main` with `release_version`, such as `v0.2.0`.
2. Preparation updates the version, generates staging, promotes the release
   snapshots, and commits them on `release-0.2`. A new release line starts from
   the dispatched `main` commit. An existing line starts from its own head.
   Before running that line's commands, preparation verifies its workflows,
   scripts, generator code, and toolchain against the dispatched `main` commit.
   Beta and RC versions use the same `release-X.Y` branch. Preparation never
   pushes to `main`.
3. Preparation explicitly dispatches `release.yml` at that generated commit.
   Release runs full Go/UI lint and tests, Helm checks, and govulncheck. It
   builds all nine images for amd64 and arm64, runs Trivy, generates platform
   SBOMs, and signs the images. Trivy findings remain advisory; scanner and
   SARIF-upload failures still fail the run.
4. Release dispatches **Release Qualification** with the exact candidate
   bundle. A `release-qualification` environment reviewer must approve the
   candidate source and workflow before the job can receive model-provider credentials.
   The gate runs Git publication and GitHub API fixture tests from that checkout,
   then installs the packaged chart using the built image digests,
   verifies durable results after controller replacement, rejects an
   opposite-mode upgrade, and runs agent runtime acceptance and cleanup.
   It does not rebuild the release images.
5. The **Approve and publish the qualified candidate** job waits for an
   environment reviewer. After approval it rechecks the branch, build,
   qualification, and artifact hashes. It creates an annotated version tag,
   promotes the qualified image digests, copies the exact tested chart to
   `gh-pages/charts`, requests and verifies a Pages build, and creates a GitHub
   Release with the candidate manifest, chart, and acceptance evidence.

A tag push does not start publication. Publication stays in the dispatched
workflow because `GITHUB_TOKEN` pushes do not trigger another push workflow.
See GitHub's [workflow trigger rules](https://docs.github.com/en/actions/how-tos/writing-workflows/choosing-when-your-workflow-runs/triggering-a-workflow).

## One-time configuration

The automation must first be merged into `main`. Backport the current release
tooling to an existing release line before preparing another version on that
line. Runtime backports can differ, but executable release tooling and static
chart inputs must match the dispatched `main` commit. The comparison allows
only the literal Makefile version assignment, chart version and appVersion,
and the five Orka image tags updated by release preparation to differ.
Template changes, other chart values, and file mode changes require backporting
the trusted inputs first.

Create a `release` environment with:

- A required reviewer and administrator bypass disabled.
- Selected deployment branches, with an exact branch rule for each release
  line, such as `release-0.2`. Wildcard, tag, and unrelated branch rules are
  rejected even when an exact release-branch rule is also present.
- Prevent self-review disabled if the person dispatching the release will also
  approve it. Enable it when another reviewer is available and required.

No release environment secret is needed. Add the same exact release branch to
`release-qualification`, retaining `main` for standalone qualification. These
settings are prerequisites; the workflows check them but do not change them.
The `release-qualification` environment also requires a reviewer with
administrator bypass disabled. Review approval must protect credentials before
any release-branch workflow runs, including a direct standalone dispatch.
GitHub documents [environment protection rules](https://docs.github.com/en/actions/reference/workflows-and-actions/deployments-and-environments).

Allow the native workflow token to push to the release branches and `gh-pages`.
A ruleset requiring PRs on those branches would block this flow. The PR-creation
policy can remain disabled. Pages must continue serving the `gh-pages` branch
root, as configured for this repository.

## Start and approve a release

Use **Actions → Prepare Release → Run workflow**, choose `main`, and enter the
version, or run:

```bash
gh workflow run release-prepare.yml --repo orka-agents/orka --ref main \
  -f release_version=v0.2.0
```

Preparation links the source and generated commit diffs and the Release run in
its job summary. Review the candidate source, workflows, generated changes, and
build bundle before approving the waiting `release-qualification` deployment.
After qualification succeeds, review the acceptance evidence and approve the
`release` deployment. The per-release human actions are dispatch, approval to
run qualification with model-provider access, and approval to publish.

Finish or cancel any existing qualification run before starting another. The
release checks for active or approval-pending runs before dispatch. If a
simultaneous dispatch takes the concurrency slot first, the release requests
cancellation of its queued child and fails promptly. Finish the active run,
then retry the failed qualification job.
Approve the new qualification run within 90 minutes so its four-hour execution
budget fits inside the parent workflow's six-hour job limit. The parent requests
cancellation of its qualification run when the approval or overall wait budget
expires. Confirm that the child run has stopped, then retry the failed
qualification job.

Before a tag exists, use **Re-run all jobs** if an interrupted build left partial
artifacts. This makes a new artifact set and requires fresh qualification. If
only qualification failed, **Re-run failed jobs** reuses the successful build
and dispatches a new gate attempt.

After tagging has started, use **Re-run failed jobs** in the original Release
run. It retains the original candidate and qualification evidence, asks for
approval again, and resumes publication. Existing version tags, image tags,
chart archives, and release assets must match the original bytes. An existing
GitHub Release must also match the candidate SHA, version, prerelease flag, and
evidence links. The workflow refuses to reuse mismatched metadata or replace
existing bytes. A full rebuild or a new preparation run cannot reuse
an already tagged version. Retry while the Actions artifacts remain available.

A changed release-branch head invalidates the candidate, even after approval.
Prepare and qualify the new head. Only the newest stable release updates
`latest`; an older release line cannot move it backwards. Beta and RC versions
never update `latest` or the minor-version alias.

## Coverage and environment

Release qualification needs no stored GitHub repository token or custom GitHub
App. GitHub observations use the job's temporary `GITHUB_TOKEN` with read-only
access. Runtime Tasks clone the public candidate repository without a stored
Git credential. Qualification creates no GitHub branch or PR.

| Check | Required coverage |
| --- | --- |
| Codex, OpenCode, Claude, and Copilot | Live workspace reads, API results, Session continuation, and Task forks |
| Runtime lifecycle | Concurrency, cancellation, timeout, controller restart, pool replacement, and scale-to-zero recovery |
| Isolation and deployment | Unsafe workspace rejection, read policy, publisher broker configuration, and exact deployed image digests |
| Branch publication | Publisher and service tests against real temporary Git repositories, including exact-head pushes, conflicts, retries, and cleanup |
| PR reconciliation | Local GitHub API test servers exercise creation, reuse, head changes, ambiguous responses, Session ownership, and credential boundaries |
| Release artifacts | The exact packaged chart and image digests, durable results after controller replacement, no Task replay, and opposite-mode rejection |

Publication tests execute the publisher code with fixtures. They do not prove
the complete cluster-to-GitHub publication path, live GitHub authentication,
organization permissions, or a cross-repository PR round trip. The report
explicitly records `coverage.liveGitHub: not_tested`.

Configure the `release-qualification` environment in `orka-agents/orka`:

- Select deployment branches by name. Allow `main` and explicitly selected
  release lines, such as `release-0.2`. Do not allow tags, PR refs, or arbitrary
  branches.
- Require a trusted reviewer and disable administrator bypass. This approval
  protects model-provider credentials from unreviewed release-branch workflows.
  Keep Prevent self-review disabled if the trusted release maintainer also
  dispatches and approves the run. Final publication has a separate approval
  in the `release` environment.
- Supply `COPILOT_GITHUB_TOKEN` for model-provider authentication through Vekil.
  The existing repository secret can supply it. Qualification does not use it
  for Git publication, and the local publication tests receive no workflow,
  provider, or canary tokens.

GitHub creates a [token for each job](https://docs.github.com/en/actions/concepts/security/github_token)
and expires it when the job ends. Do not save it as an environment secret.

After both replacement workflows pass, remove the unused
`ACP_E2E_WRITE_TARGET_READ_CREDENTIAL_TOKEN`, `ACP_E2E_WRITE_CREDENTIAL_TOKEN`, and
`ACP_E2E_WRITE_FORGE_CREDENTIAL_TOKEN` environment secrets, and the
`ACP_E2E_WRITE_PUBLICATION_REPO` variable. Remove the old stored source-read
secret too if it still exists. Deleting a secret does not revoke its token;
revoke unused tokens separately. Retire `live-acp-release-gate` and
`live-acp-runtime-smoke` only after replacement validation, preserving deployment
history.

## Standalone qualification

The release workflow dispatches and verifies the gate automatically. A
standalone gate run remains useful for diagnosis. It builds local images from
the candidate and does not supply the packaged-chart evidence required by the
release publication job.

Dispatch from `main` or an explicitly permitted `release-X.Y` branch. The
workflow SHA, source SHA, and current branch head must agree. The existing
`pr_base` input selects that candidate branch; qualification does not create a PR.
For example:

```bash
candidate="$(gh api repos/orka-agents/orka/commits/main --jq .sha)"
gh workflow run release-qualification.yml --repo orka-agents/orka --ref main \
  -f source_ref="${candidate}" \
  -f source_repository=https://github.com/orka-agents/orka.git \
  -f pr_base=main

gh run list --repo orka-agents/orka --workflow release-qualification.yml \
  --commit "${candidate}" --event workflow_dispatch \
  --json databaseId,headSha,status,conclusion,url

gh run watch RUN_ID --repo orka-agents/orka --exit-status
bash scripts/verify-release-qualification.sh "${candidate}" RUN_ID main
```

The verifier's optional third argument is the expected branch and defaults to
`main`. For a release-line run, pass that exact branch. The verifier requires a
successful dispatch from this repository at the exact candidate SHA. It
downloads the current attempt's final report and checks workflow identity,
fixture test evidence, runtime checks, observed images, and cleanup. Missing, expired,
incomplete, or mismatched evidence fails verification. A local run is useful
for diagnosis; release qualification requires a trusted workflow run.

## Evidence and failures

The job uploads `release-qualification-evidence-RUN_ID-ATTEMPT` before tearing down
Kind, then `release-qualification-acceptance-RUN_ID-ATTEMPT` after teardown. Both
contain only `acceptance.json` and remain available for 90 days. Successful
release publication archives the verified report, candidate and qualification
manifests, and exact chart archive as GitHub Release assets.
The first artifact is diagnostic evidence and cannot qualify a release while
cluster cleanup is pending.

A bundled release report also records the build run and artifact attempt,
candidate-manifest hash, chart hash, retained PVC identities, controller Pod
replacement, completed Task identity, and chart acceptance results.

Schema version 2 includes the dispatched and checked-out SHAs, required test
names and package outcomes from an uncached Go test run, runtime scenario
results, actual controller, publisher, and runtime Pod image digests, candidate
branch observations, and separate cleanup outcomes. It omits
Task prompts/results, free-form messages, Pod environment values, and Secret
contents. A failure before deployment records the candidate and failed stage;
unobserved fields remain absent or incomplete.

`coverage.publication: local_git` and `coverage.pullRequests: github_api_fixtures`
describe the required publication coverage. Both publisher packages must pass,
and every required behavioral test must actually run and pass. A successful Go
exit with missing or skipped required tests cannot qualify a candidate. Older
schema-version-1 reports cannot satisfy the new contract.

The source branch must still equal the candidate at preflight and completion,
including the final check after cluster teardown. If it moves, the report shows
the observed SHA and the candidate remains unqualified. Let cleanup finish, then
dispatch the new full head SHA. Do not edit the report or reuse an earlier
report for the new candidate.

Cleanup settles run-owned Tasks and removes their Kubernetes resources,
temporary credentials, cluster, and registry. Remote GitHub cleanup is
`not_required`. An interrupted validator still fails qualification; its
disposable cluster can be removed because this mode cannot publish to GitHub.
Missing cleanup evidence, preserved resources, or `--keep-cluster` prevent a
qualified result. Runner disposal does not count as verified cleanup.

## Optional local live GitHub diagnostic

The deployed-cluster validator retains its existing live canary behind
`RELEASE_GATE=1 ACP_E2E_WRITE_CREATE_PR=1`. Its additional credentials and distinct
fork requirements are listed in `scripts/agent-runtime-e2e.sh --help`. Use only
separately authorized test infrastructure. The release workflow fixes this flag
to `0`, exposes no canary credentials, and cannot dispatch this diagnostic.

This opt-in still requires exact publication and PR receipts, four distinct
credential roles, and independent remote verification. Cleanup settles the
writer, closes only the exact unmerged PR, and deletes its branch with an
exact-head lease. Unknown remote effects preserve the cluster for investigation
and fail the run. Fixture tests cannot substitute for those live receipts.
