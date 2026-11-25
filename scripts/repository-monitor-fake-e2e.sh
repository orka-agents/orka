#!/usr/bin/env bash
set -euo pipefail

# Runs the fake-GitHub RepositoryMonitor integration scenarios that prove the
# durable label-command issue-to-PR loop, retained usage reports, and the
# monitor's repair, path and agent policy and its audit records without
# requiring live GitHub secrets.
# Keep this script secret-free: all GitHub payloads and credentials are synthetic.

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

run_go_test() {
  local pkg="$1"
  local pattern="$2"
  echo "==> go test ${pkg} -run ${pattern}"
  go test "${pkg}" -run "${pattern}" -count=1 -v
}

run_go_test ./internal/api 'Test(GitHubWebhook_OrkaIssueLabelCreatesDurableCommandAndIssueRun|GitHubWebhook_OrkaClosedIssueLabelDoesNotCreateCommand|GitHubWebhook_OrkaGuardLabelBlocksCommandWithoutRun|GitHubWebhookPauseRemovalQueuesInventoryWithoutACommand|GitHubWebhook_OrkaEquivalentCommandsCoalesceActiveWorkAction|GitHubWebhook_DuplicateAcceptedCommandRetriesFailedRunSignal|GitHubWebhook_OrkaPRLabelRequiresExactBaseBranchCase|CreateRepositoryMonitorCommandEventBlocksGuardedTarget|CreateRepositoryMonitorCommandEventRejectsClosedTarget|CreateRepositoryMonitorCommandEventRejectsIssueOutsideLabelScope|CreateRepositoryMonitorCommandEventRejectsIssueTargetSHA|CreateRepositoryMonitorCommandEventRejectsStalePRTargetSHA|CreateRepositoryMonitorCommandEventRequiresInventoryForIssuePlan|CreateRepositoryMonitorCommandEventRequiresInventoryForPR|GetRepositoryMonitorImplementationPatchPreview)'

run_go_test ./internal/controller 'TestRepositoryMonitor(IssueImplementToPRFakeGitHubE2E|AutomaticallyRepairsFailedCI|CIEvidenceNormalizesFailingChecks|CIEvidenceEnforcesCheckAndSizeLimits|CIRepairPromptDegradesWhenGitHubFails|CIRepairEvidenceIsFrozenPerAttempt|CIRepairPromptNamesTheChecksThePullRequestBroke|EvidenceTextRedactsCredentials|ReadinessStatusLifecycle|CIExcludesOnlyPersistedOwnedStatusIDs|IssueStopPreventsLateImplementationMutation|RunFailureState|RequireGreenCIGatesReviewQueue)'

# Policy: repair budgets and their once-per-head refusal records, patch path
# limits, and run-time blocks for monitors missing a repairer or implementer.
run_go_test ./internal/controller 'TestRepositoryMonitor(RepairBudgetRefusalIsRecordedOncePerHead|UpdateBranchConsumesRepairBudget|RepairPolicyBlocksDisabledAndBudgets|UpdateBranchBudgetAndOutcome|AutomaticRepairFollowsRepairLabels|RepairAppliesImplementationPathLimits|RepairWithoutAllowedPathsStillDeniesControlPaths|IssuePatchValidationArtifacts|IssuePatchValidationAppliesAllowedPaths|FixCommandWithoutRepairerIsBlocked|IssueCommandWithoutAgentIsBlocked|AutomaticAgentRepairRequiresRepairer)$'

run_go_test ./workers/acp/supervisor 'TestWorkspaceDelta(RepositoryControlAndContentPolicies|PathViolationReportsControlPathBeforeAllowlist|RepositoryControlPathAppliesWorkspaceRelativeRoot)$'

# Observability: readiness status writes per head, restart recovery without
# duplicate Tasks or GitHub mutations, and run failure on a GitHub timeout.
run_go_test ./internal/controller 'TestRepositoryMonitor(ReadinessDraftHoldIsWrittenOncePerHead|RepairRestartReusesTaskAndPushRecord|UpdateBranchRetriesCleanSubmittingMutation|UpdateBranchReconcilesAmbiguousTransportErrorWithoutResubmitting|ReconcileRecoversStaleRunningRun|GitHubTimeoutFailsRunWithBoundedRetries|TransientCommandRunStopsAfterRetryBudget)$'

run_go_test ./internal/store/sqlite 'TestMonitorWorkflowStoresActionsJobsAndMutations'

run_go_test ./workers/ai '^TestUsageIssueToMergedPRWorkflowE2E$'
