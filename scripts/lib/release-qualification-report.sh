#!/usr/bin/env bash
# shellcheck disable=SC2016 # acp_report_update arguments are jq programs.
# Source-only, allowlisted evidence for release qualification.
# Never persist Task prompts/results, messages, annotations, or Secret data.

acp_report_enabled() {
  [[ "${RELEASE_GATE:-0}" == "1" && -n "${ACP_E2E_REPORT_FILE:-}" ]]
}

# The existing live canary is an explicit local opt-in. The release workflow
# fixes this to 0 and needs no GitHub publication credentials.
acp_live_github_enabled() {
  [[ "${ACP_E2E_WRITE_CREATE_PR:-0}" == "1" ]]
}

# Require named behavioral tests as well as successful package exits. A skipped,
# filtered, renamed, or empty suite must not count as publication coverage.
acp_publication_test_requirements() {
  cat <<'JSON'
{
  "github.com/orka-agents/orka/internal/publisher": [
    "TestPrepareDeterministicTrustedCommitAndDurableBundle",
    "TestPublishExactCASForkVerifyAndNoForcePush",
    "TestPublishDetectsMovementBetweenPreflightAndCAS",
    "TestExactLeaseRejectsRaceThatRemainsFastForward",
    "TestPublishRejectsNonFastForwardEvenWhenClaimMatches",
    "TestVerifySupersededConflictAndIdempotency",
    "TestAmbiguousPublishReconcilesWithoutRepush",
    "TestPublishCancellationBoundary",
    "TestPublicationReclaimRejectsGenerationMismatchAndRemovesCacheIdempotently"
  ],
  "github.com/orka-agents/orka/internal/publisher/service": [
    "TestPublicationPreparePublishVerifyLocalBareRepository",
    "TestExactPullRequestIntentAndCredentialFileBoundary",
    "TestGitHubPRReconcilerCreatesExactPullRequest",
    "TestGitHubPRReconcilerReturnsExactExistingOpenPullRequest",
    "TestGitHubPRReconcilerRejectsWrongHeadSHA",
    "TestGitHubPRReconcilerRejectsMovedHeadBeforeCreate",
    "TestGitHubPRReconcilerRecoversAmbiguousCreateByExactTuple",
    "TestGitHubPRReconcilerContinuesSessionAcrossPublicationsAndRetries",
    "TestGitHubPRReconcilerRejectsConflictingSessionOwnership",
    "TestGitHubPRReconcilerDoesNotRecreateClosedSessionPullRequests",
    "TestGitHubPRReconcilerDeniesRedirects",
    "TestGitHubPRReconcilerSanitizesAPIErrors"
  ]
}
JSON
}

acp_report_publication_tests_qualified() {
  jq -e --argjson required "$(acp_publication_test_requirements)" '
    . as $r | .publicationTests as $tests
    | .coverage.publication == "local_git" and .coverage.pullRequests == "github_api_fixtures"
      and $tests.candidateSHA == .candidateSHA and $tests.candidateSHA == .checkoutSHA
      and $tests.status == "passed" and $tests.exitCode == 0 and $tests.failedEvents == 0
      and ($tests.suites | keys) == ($required | keys)
      and all($required | keys[];
        . as $package | $tests.suites[$package] as $suite
        | $suite.passed == true
          and ($suite.passedTests | type == "array")
          and ($suite.passedTests | length) == ($suite.passedTests | unique | length)
          and ($suite.testCount | type == "number" and . >= ($required[$package] | length))
          and all($required[$package][]; . as $test | $suite.passedTests | index($test) != null))
  ' "$1" >/dev/null
}

acp_report_update() {
  acp_report_enabled || return 0
  local filter="$1" candidate
  shift
  candidate="$(mktemp "${ACP_E2E_REPORT_FILE}.XXXXXX")" || return 1
  if ! jq "$@" "${filter}" "${ACP_E2E_REPORT_FILE}" >"${candidate}"; then
    rm -f "${candidate}"
    return 1
  fi
  chmod 600 "${candidate}" && mv "${candidate}" "${ACP_E2E_REPORT_FILE}"
}

acp_report_init() {
  acp_report_enabled || return 0
  local checkout_sha live_github=false
  if acp_live_github_enabled; then live_github=true; fi
  checkout_sha="$(git -C "$1" rev-parse HEAD)" || return 1
  mkdir -p "$(dirname "${ACP_E2E_REPORT_FILE}")" || return 1
  jq -n \
    --arg candidate "${ACP_E2E_REF:-${ACP_E2E_WRITE_SOURCE_REF:-}}" \
    --arg checkout "${checkout_sha}" \
    --arg source "${ACP_E2E_REPO:-${ACP_E2E_WRITE_SOURCE_REPO:-}}" \
    --arg publication "${ACP_E2E_WRITE_PUBLICATION_REPO:-}" \
    --arg base "${ACP_E2E_BASE_BRANCH:-${ACP_E2E_WRITE_PR_BASE:-main}}" \
    --argjson liveGitHub "${live_github}" \
    --arg repository "${GITHUB_REPOSITORY:-}" \
    --arg ref "${GITHUB_REF:-}" \
    --arg workflowSHA "${GITHUB_SHA:-}" \
    --arg runID "${GITHUB_RUN_ID:-}" \
    --arg attempt "${GITHUB_RUN_ATTEMPT:-}" \
    --arg event "${GITHUB_EVENT_NAME:-}" \
    --arg run "${ACP_E2E_RUN_ID:-}" '
      def sha: if test("^[a-fA-F0-9]{40}$") then ascii_downcase else null end;
      def repo:
        sub("\\.git$"; "")
        | if test("^https://github\\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$")
          then ascii_downcase else null end;
      {
        schemaVersion: 2, gate: "release-qualification", mode: "release",
        candidateSHA: ($candidate | sha), checkoutSHA: ($checkout | sha),
        sourceRepository: ($source | repo),
        publicationRepository: (if $liveGitHub then ($publication | repo) else null end),
        coverage: {publication:"local_git", pullRequests:"github_api_fixtures",
          liveGitHub:(if $liveGitHub then "live" else "not_tested" end)},
        publicationTests: {status:"not_started"},
        runtime: {providers:{}, checks:{}},
        baseBranch: $base, expectedBranch: null, run: $run,
        workflow: {repository: $repository, ref: $ref, sha: ($workflowSHA | sha),
          runID: $runID, runAttempt: $attempt, event: $event},
        startedAt: (now | todateiso8601), finishedAt: null,
        result: "not_qualified", stage: "preflight", validation: "not_started",
        validatorStarted: false, validatorExitCode: null, bootstrapExitCode: null,
        checks: ({publisherBrokeredAuthority: false, baseUnchanged: false}
          + (if $liveGitHub then {publication:false,credentialsFrozen:false,publisherSecretReadDenied:false} else {} end)),
        builtImages: {}, images: {}, task: null, credentials: [],
        observations: {baseHeads: {}},
        cleanup: {remote: (if $liveGitHub then "not_started" else "not_required" end), kubernetes: "not_started",
          validatorCredentials: "not_started", bootstrapCredentials: "not_started",
          cluster: "not_started", registry: "not_started"},
        preserved: null
      }
    ' >"${ACP_E2E_REPORT_FILE}" || return 1
  chmod 600 "${ACP_E2E_REPORT_FILE}"
}

acp_report_task() {
  acp_report_enabled || return 0
  local projection
  projection="$(jq -c '{
    namespace: .metadata.namespace, name: .metadata.name, uid: .metadata.uid,
    phase: .status.phase,
    execution: (.status.execution // {} | {
      state, outcome, attempt, promptID, runtimePoolName, runtimePoolUID,
      runtimeInstanceID, runtimeSessionUID, runtimeSessionGeneration,
      requestDigest, controllerEpoch, readCredentialResourceVersion,
      publicationReadCredentialResourceVersion, publicationCredentialResourceVersion,
      forgeCredentialResourceVersion
    }),
    delivery: (.status.delivery // {} | {
      state, outcome, publicationID,
      sourceRepository: (.sourceRepository // {} | {provider, id}),
      publicationRepository: (.publicationRepository // {} | {provider, id}),
      branch, startingSHA, remoteBeforeSHA, treeSHA, expectedCommitSHA,
      verifiedRemoteSHA, supersedingRemoteSHA, artifactDigest,
      prReceipt: (.prReceipt // {} | {id, number, url, state, baseBranch, headBranch, headSHA})
    })
  }' <<<"$1")" || return 1
  acp_report_update '.task = $task' --argjson task "${projection}"
}

# Called only after the validator has checked the exact Pod and resolved its
# requested OCI index to the observed platform image digest.
acp_report_image() {
  acp_report_enabled || return 0
  local role="$1" pod="$2" container="$3" projection
  projection="$(jq -c --arg container "${container}" '
    . as $pod
    | (.spec.containers[] | select(.name == $container)) as $spec
    | (.status.containerStatuses[] | select(.name == $container)) as $status
    | {namespace: $pod.metadata.namespace, pod: $pod.metadata.name, podUID: $pod.metadata.uid,
       container: $container, requestedImage: $spec.image, imageID: $status.imageID,
       actualDigest: ($status.imageID | capture("(?<digest>sha256:[a-f0-9]{64})$").digest),
       verified: true}
  ' <<<"${pod}")" || return 1
  [[ -n "${projection}" ]] || return 1
  acp_report_update '.images[$role] = $image' --arg role "${role}" --argjson image "${projection}"
}

acp_report_qualified() {
  acp_report_publication_tests_qualified "$1" || return 1
  jq -e '
    def sha: type == "string" and test("^[a-f0-9]{40}$");
    def digest: type == "string" and test("^sha256:[a-f0-9]{64}$");
    def present: type == "string" and length > 0;
    def hash: type == "string" and test("^[a-f0-9]{64}$");
    def run: type == "string" and test("^[1-9][0-9]*$");
    def live_canary:
      . as $r
      | (.publicationRepository | present) and .publicationRepository != .sourceRepository
      and .checks.publication == true and .checks.credentialsFrozen == true
      and .checks.publisherSecretReadDenied == true
      and .canary.expectedCommitBytesMatched == true and .canary.remoteBytesMatched == true
      and .canary.singleAddedFile == true and (.canary.path | present)
      and (["preflight", "submission", "publication", "completion"] | all(.[];
        $r.observations.baseHeads[.] == $r.candidateSHA))
      and (.task.uid | present) and .task.phase == "Succeeded"
      and .task.execution.state == "Succeeded" and .task.execution.outcome == "Succeeded"
      and .task.execution.attempt == 1 and (.task.execution.promptID | present)
      and (.task.delivery.publicationID | present)
      and (.task.delivery.outcome == "VerifiedExact" or .task.delivery.outcome == "DeliveredSuperseded")
      and .task.delivery.startingSHA == .candidateSHA
      and (.expectedBranch | present) and .task.delivery.branch == .expectedBranch
      and (.task.delivery.expectedCommitSHA | sha) and (.task.delivery.treeSHA | sha)
      and .task.delivery.expectedCommitSHA == .observations.expectedCommit.sha
      and .task.delivery.treeSHA == .observations.expectedCommit.treeSHA
      and (.task.delivery.artifactDigest | digest)
      and (.observations.remoteHead | sha)
      and .observations.remoteHead == (.task.delivery.supersedingRemoteSHA | select(. != null and . != "") // $r.task.delivery.verifiedRemoteSHA)
      and (if .task.delivery.outcome == "VerifiedExact"
        then .observations.remoteHead == .task.delivery.expectedCommitSHA
        else .observations.remoteHead != .task.delivery.expectedCommitSHA end)
      and .observations.pullRequest.number == .task.delivery.prReceipt.number
      and (.observations.pullRequest.number | type == "number" and . > 0)
      and .observations.pullRequest.state == "open"
      and .observations.pullRequest.headSHA == .observations.remoteHead
      and .observations.pullRequest.baseSHA == .candidateSHA
      and .observations.pullRequest.baseBranch == .baseBranch
      and .observations.pullRequest.headBranch == .task.delivery.branch
      and .task.delivery.prReceipt.state == "Open"
      and .task.delivery.prReceipt.headSHA == .observations.remoteHead
      and .task.delivery.prReceipt.headBranch == .task.delivery.branch
      and .task.delivery.prReceipt.baseBranch == .baseBranch
      and .observations.pullRequest.sourceRepository == .sourceRepository
      and .observations.pullRequest.publicationRepository == .publicationRepository
      and (.credentials | length) == 4
      and (.credentials | map(.role) | sort) == ["forge", "sourceRead", "targetRead", "targetWrite"]
      and all(.credentials[]; (.namespace | present) and (.name | present) and (.resourceVersion | present))
      and (.credentials | map(.namespace + "/" + .name) | unique | length) == 4
      and all(.credentials[];
        .resourceVersion == ($r.task.execution | if . == null then null else
          {sourceRead:.readCredentialResourceVersion, targetRead:.publicationReadCredentialResourceVersion,
           targetWrite:.publicationCredentialResourceVersion, forge:.forgeCredentialResourceVersion} end)[.role])
      and .cleanup.remote == "passed";
    . as $r
    | .schemaVersion == 2 and .gate == "release-qualification" and .mode == "release"
      and (.candidateSHA | sha) and .candidateSHA == .checkoutSHA
      and .sourceRepository == "https://github.com/orka-agents/orka"
      and .validation == "passed" and .validatorStarted == true
      and .validatorExitCode == 0 and .bootstrapExitCode == 0
      and .checks.publisherBrokeredAuthority == true and .checks.baseUnchanged == true
      and (["preflight", "completion"] | all(.[]; $r.observations.baseHeads[.] == $r.candidateSHA))
      and (["codex", "opencode", "claude", "copilot"] | all(.[];
        $r.runtime.providers[.] | .read == true and .continuation == true and .result == true and .fork == true))
      and (["unsafeWorkspace", "concurrency", "timeout", "cancellation", "controllerRestart",
        "poolReplacement", "scaleToZeroRecovery", "opencodeReadPolicy"] | all(.[]; $r.runtime.checks[.] == true))
      and (["controller", "publisher", "codex", "opencode", "claude", "copilot"] | all(.[];
        . as $role | $r.images[$role] | .verified == true
        and (.actualDigest | digest) and (.podUID | present)
        and (.requestedImage | test("@sha256:[a-f0-9]{64}$"))
        and .requestedImage == $r.builtImages[$role]))
      and (if .coverage.liveGitHub == "not_tested" then
        .publicationRepository == null and .expectedBranch == null and .task == null
        and .credentials == [] and .cleanup.remote == "not_required"
      elif .coverage.liveGitHub == "live" then live_canary
      else false end)
      and .cleanup.kubernetes == "passed"
      and .cleanup.validatorCredentials == "passed" and .cleanup.bootstrapCredentials == "passed"
      and .cleanup.cluster == "passed" and .cleanup.registry == "passed"
      and .preserved == null
      and (if has("release") then
        (.release.buildRunID | run) and (.release.buildRunAttempt | run)
        and (.release.bundleSHA256 | hash) and (.release.version | present)
        and (.chart.packageSHA256 | hash)
        and .chart.install == true and .chart.containerTask == true
        and .chart.recovery == true and .chart.noReplay == true
        and .chart.oppositeModeRejected == true and .chart.jobRemovedBeforeRestart == true
        and .chart.noReplayObservationSeconds >= 10
        and (.chart.pvcs | length) == 2
        and (.chart.pvcs | map(.name) | sort) == ["orka-store", "orka-workspace-publisher"]
        and all(.chart.pvcs[]; (.uid | present) and (.volumeName | present) and .phase == "Bound")
        and (.chart.controllerPodUIDBefore | present) and (.chart.controllerPodUIDAfter | present)
        and .chart.controllerPodUIDBefore != .chart.controllerPodUIDAfter
        and (.chart.task.uid | present) and .chart.task.phase == "Succeeded"
        and .chart.task.attempts == 1 and .chart.task.resultAvailable == true
        and (.chart.task.jobUID | present)
      else true end)
  ' "$1" >/dev/null
}

acp_report_finish() {
  acp_report_enabled || return 0
  acp_report_update '.finishedAt = (now | todateiso8601) | .result = "not_qualified"' || return 1
  if acp_report_qualified "${ACP_E2E_REPORT_FILE}"; then
    acp_report_update '.result = "qualified"'
  else
    printf '%s\n' 'Release candidate is not qualified; inspect acceptance.json.' >&2
    return 1
  fi
}
