#!/usr/bin/env bash
set -euo pipefail

# Runs the fake-GitHub RepositoryMonitor integration scenarios that prove the
# durable label-command issue-to-PR loop and retained usage reports without
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

run_go_test ./internal/store/sqlite 'TestMonitorWorkflowStoresActionsJobsAndMutations'

run_go_test ./workers/ai '^TestUsageIssueToMergedPRWorkflowE2E$'

# Decision events: each monitor decision is recorded once, with its reason and
# the policy clause that decided it.
run_go_test ./internal/store 'Test(RepositoryMonitorEventDecisionCoversDecisionTypesOnly|NewRepositoryMonitorEventStampsDecisionAndPolicy|NewRepositoryMonitorEventIDs|RecordRepositoryMonitorEventTreatsConflictAsRecorded)$'

run_go_test ./internal/api 'Test(GitHubWebhook_LabelCommandIntakeRecordsOneDecisionEvent|GitHubWebhook_OutOfScopeLabelCommandRecordsRejectionWithoutCommand|RepositoryMonitorPermissionPolicyNamesTheDecidingClause|RepositoryMonitorCommandDecisionKeepsOneReceiptPerCommand)$'

run_go_test ./internal/controller 'TestRepositoryMonitor(ItemSkippedIsRecordedOncePerHeadAndReason|BlockedImplementationRecordsDecisionEvent|FailedUpdateBranchRecordsDecisionEvent|UpdateBranchFailurePathsRecordOneEvent|RunAttemptsKeepTheirOwnEvents|EventPolicyNamesSpecClauses|RepairAppliesImplementationPathLimits)$'

# Edit policy: implement and repair Tasks see which named files they may
# change, and a run that could change none of them is refused before it starts.
run_go_test ./internal/controller 'TestRepositoryMonitor(IssueNamedPaths|EditTargetsAreBoundedRelativePaths|EditPolicySplitsEditableAndReadOnlyTargets|ImplementationPathAllowedMatchesRuntimeGlobs|IssueActionPromptsCarryEditPolicy|ImplementationRefusesWhenNothingNamedIsEditable|FindingsRepairRefusesWhenNoFindingIsEditable|RepairEditPolicy|RepairPromptEditPolicyInstructionsMatch)$'
