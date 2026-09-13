---
description: Qualify an ACP release candidate with deployed publication, independent GitHub verification, and safe cleanup.
---

# Release automation and ACP qualification

Start a release with **Prepare Release**, approve credentialed qualification,
then approve publication of the qualified candidate. Preparation, workflow
dispatch, tagging, image promotion, chart publication, and release evidence
archival use only `GITHUB_TOKEN`. The organization can keep its policy that prohibits Actions
from creating or approving PRs. Release preparation does not create a PR.

The flow is:

1. Dispatch `release-pr.yml` from `main` with `release_version`, such as `v0.2.0`.
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
4. Release dispatches **Live ACP Release Gate** with the exact candidate
   bundle. A `live-acp-release-gate` environment reviewer must approve the
   candidate source and workflow before the job can receive canary credentials.
   The gate installs the packaged chart using the built image digests,
   verifies durable results after controller replacement, rejects an
   opposite-mode upgrade, and runs canonical ACP acceptance and canary cleanup.
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
line. Runtime backports can differ, but executable release tooling must match
the dispatched `main` commit. Only the literal Makefile version assignment is
excluded from that comparison.

Create a `release` environment with:

- A required reviewer and administrator bypass disabled.
- Selected deployment branches, with an exact branch rule for each release
  line, such as `release-0.2`. Wildcards are insufficient.
- Prevent self-review disabled if the person dispatching the release will also
  approve it. Enable it when another reviewer is available and required.

No release environment secret is needed. Add the same exact release branch to
`live-acp-release-gate`, retaining `main` for standalone qualification. These
settings are prerequisites; the workflows check them but do not change them.
The `live-acp-release-gate` environment also requires a reviewer with
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
gh workflow run release-pr.yml --repo orka-agents/orka --ref main \
  -f release_version=v0.2.0
```

Preparation links the source and generated commit diffs and the Release run in
its job summary. Review the candidate source, workflows, generated changes, and
build bundle before approving the waiting `live-acp-release-gate` deployment.
After qualification succeeds, review the acceptance evidence and approve the
`release` deployment. The per-release human actions are dispatch, approval to
use the canary credentials, and approval to publish.

Finish or cancel any existing live gate before qualification. The release
refuses to enqueue behind an active or approval-pending standalone run. Approve
the new live gate within 90 minutes so its four-hour execution budget fits
inside the parent workflow's six-hour job limit. If that budget expires, cancel
the pending gate and retry the failed qualification job.

Before a tag exists, use **Re-run all jobs** if an interrupted build left partial
artifacts. This makes a new artifact set and requires fresh qualification. If
only qualification failed, **Re-run failed jobs** reuses the successful build
and dispatches a new gate attempt.

After tagging has started, use **Re-run failed jobs** in the original Release
run. It retains the original candidate and qualification evidence, asks for
approval again, and resumes publication. Existing version tags, image tags,
chart archives, and release assets must match the original bytes. The workflow
refuses to replace them. A full rebuild or a new preparation run cannot reuse
an already tagged version. Retry while the Actions artifacts remain available.

A changed release-branch head invalidates the candidate, even after approval.
Prepare and qualify the new head. Only the newest stable release updates
`latest`; an older release line cannot move it backwards. Beta and RC versions
never update `latest` or the minor-version alias.

## Canary and environment

The source is `orka-agents/orka`. The dedicated publication target is
[`sozercan/orka-acp-release-gate`](https://github.com/sozercan/orka-acp-release-gate),
an actual fork of that source. Do not use it for development branches. Each
attempt creates a unique `orka/acp-release-gate-*` branch and a temporary PR
against the dispatched source branch. The validator checks the fork relationship
through GitHub before submitting work.

Configure the `live-acp-release-gate` environment in `orka-agents/orka`:

- Select deployment branches by name. Allow `main` and explicitly selected
  release lines, such as `release-0.2`. Do not allow tags, PR refs, or arbitrary
  branches.
- Require a trusted reviewer and disable administrator bypass. This approval
  protects the canary credentials from unreviewed release-branch workflows.
  Keep Prevent self-review disabled if the trusted release maintainer also
  dispatches and approves the run. Final publication has a separate approval
  in the `release` environment.
- Set the environment variable `ACP_E2E_WRITE_PUBLICATION_REPO` to
  `https://github.com/sozercan/orka-acp-release-gate.git`. An optional dispatch
  override must identify this same repository.
- Configure the following secrets. The four GitHub credentials must have
  distinct values. Each Kubernetes copy uses the `token` key.

| Secret | Required access and use |
| --- | --- |
| `COPILOT_GITHUB_TOKEN` | Provider authentication for the configured Codex, OpenCode, Claude, and Copilot models through Vekil. The repository secret can supply this value. It is not Git publication authority. |
| `ACP_E2E_WRITE_READ_CREDENTIAL_TOKEN` | Source repository Contents read and metadata read. Used by the source clone boundary. |
| `ACP_E2E_WRITE_TARGET_READ_CREDENTIAL_TOKEN` | Canary fork Contents read and metadata read. Used for target preflight and publication verification. |
| `ACP_E2E_WRITE_CREDENTIAL_TOKEN` | Canary fork Contents write and metadata read. Used by the separate publisher for the branch compare-and-swap push. No source write or PR authority is needed. |
| `ACP_E2E_WRITE_FORGE_CREDENTIAL_TOKEN` | Source Pull requests write, source and fork Contents read, and fork Contents write for branch cleanup. Used for PR reconciliation and by the independent `gh`/Git observer to verify both repositories, close the exact unmerged PR, and delete its branch with an exact-head lease. |

These are the existing ACP canary credentials, separate from release
orchestration. The forge/observer credential must work across the source
organization and fork owner. None of these credentials is used to prepare,
tag, or publish an Orka release, and the release automation adds no PAT or
GitHub App.

Set values through the environment settings or the interactive `gh secret set`
prompt. Do not put values in dispatch inputs, shell command literals, reports,
or PR descriptions. Git and forge credentials never enter the ACP child process
tree. The publisher obtains operation-scoped authority through its existing
broker and must continue to fail the gate if its ServiceAccount can read these
Secrets directly. Reports record only Secret names, namespaces, and resource
versions.

## Standalone ACP qualification

The release workflow dispatches and verifies the gate automatically. A
standalone gate run remains useful for diagnosis. It builds local images from
the candidate and does not supply the packaged-chart evidence required by the
release publication job.

Dispatch from `main` or an explicitly permitted `release-X.Y` branch. The
workflow SHA, source SHA, current branch head, and canary PR base must agree.
For example:

```bash
candidate="$(gh api repos/orka-agents/orka/commits/main --jq .sha)"
gh workflow run live-acp-release-gate.yml --repo orka-agents/orka --ref main \
  -f source_ref="${candidate}" \
  -f source_repository=https://github.com/orka-agents/orka.git \
  -f pr_base=main

gh run list --repo orka-agents/orka --workflow live-acp-release-gate.yml \
  --commit "${candidate}" --event workflow_dispatch \
  --json databaseId,headSha,status,conclusion,url

gh run watch RUN_ID --repo orka-agents/orka --exit-status
bash scripts/verify-acp-release-qualification.sh "${candidate}" RUN_ID main
```

The verifier's optional third argument is the expected branch and defaults to
`main`. For a release-line run, pass that exact branch. The verifier requires a
successful dispatch from this repository at the exact candidate SHA. It
downloads the current attempt's final report and checks workflow identity,
publication evidence, observed images, and cleanup. Missing, expired,
incomplete, or mismatched evidence fails verification. A local run is useful
for diagnosis; release qualification requires a trusted workflow run.

## Evidence and failures

The job uploads `live-acp-release-evidence-RUN_ID-ATTEMPT` before tearing down
Kind, then `live-acp-release-acceptance-RUN_ID-ATTEMPT` after teardown. Both
contain only `acceptance.json` and remain available for 90 days. Successful
release publication archives the verified report, candidate and qualification
manifests, and exact chart archive as GitHub Release assets.
The first artifact is diagnostic evidence and cannot qualify a release while
cluster cleanup is pending.

A bundled release report also records the build run and artifact attempt,
candidate-manifest hash, chart hash, retained PVC identities, controller Pod
replacement, completed Task identity, and chart acceptance results.

The report includes the dispatched and checked-out SHAs, built image references,
actual controller, publisher, and runtime Pod image digests, Task UID and attempt
fences, publication and PR receipts, independently observed remote head and PR
identity, frozen credential versions, and separate cleanup outcomes. It omits
Task prompts/results, free-form messages, Pod environment values, and Secret
contents. A failure before deployment records the candidate and failed stage;
unobserved fields remain absent or incomplete.

The report also retains GitHub's independently observed publication commit and
tree. The final Task receipt must match both; `VerifiedExact` additionally
requires the remote head to equal that commit. Cleanup cannot replace this
evidence with a different receipt and retain a qualified result.

One Codex Task must create exactly the requested new file. The gate compares its
bytes at the expected commit and independently observed remote head, verifies
the commit parent/tree and one-file diff, and reads the open PR from GitHub.
`checks.publication: true` records successful publication verification;
`result: qualified` additionally requires the remaining runtime checks and all
cleanup to succeed. A preserved cluster, failed branch deletion, skipped
publication, or missing credential cannot produce a qualified report.

The source base must still equal the candidate at preflight, Task submission,
PR verification, and completion. If the selected branch moves, the report shows the observed
base SHA and the candidate remains unqualified. Let safe cleanup finish, then
dispatch the new full head SHA. Do not edit the report or reuse an earlier
report for the new candidate.

Cleanup first proves that the Task and publisher can no longer write. It closes
only the exact unmerged canary PR, then deletes the unique branch only if its
head still equals the independently observed head, using `--force-with-lease`.
It rereads both effects before removing owned Kubernetes resources and temporary
credentials. A changed head, ambiguous receipt, merged PR, or failed read fails
the gate and identifies preserved resources in the report. Inspect those exact
resources, establish writer quiescence and their current identities, and remove
only confirmed canary effects. Never replace the lease with unconditional branch
deletion or force-remove Task finalizers to get a passing result.

After the validator starts, cluster teardown requires an explicit completed
remote-cleanup result, or confirmation that no write Task started. A timeout or
interrupted cleanup with incomplete evidence preserves the cluster and registry
and fails qualification, even if the validator could not finish its exit trap.

Local preserved clusters remain available through the run's `kindctl` tag.
GitHub-hosted runners are disposable, so download the failure artifact for
inspection; runner disposal does not count as verified cleanup. After resolving
a preserved canary, start a new attempt with a new branch.
