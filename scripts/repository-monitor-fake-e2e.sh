#!/usr/bin/env bash
set -euo pipefail

# Runs the fake-GitHub RepositoryMonitor integration scenarios that prove the
# durable label-command issue-to-PR loop without requiring live GitHub secrets.
# Keep this script secret-free: all GitHub payloads and credentials are synthetic.

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

run_go_test() {
  local pkg="$1"
  local pattern="$2"
  echo "==> go test ${pkg} -run ${pattern}"
  go test "${pkg}" -run "${pattern}" -count=1 -v
}

run_go_test ./internal/api 'Test(GitHubWebhook_OrkaIssueLabelCreatesDurableCommandAndIssueRun|GitHubWebhook_OrkaClosedIssueLabelDoesNotCreateCommand|GitHubWebhook_OrkaGuardLabelBlocksCommandWithoutRun|GitHubWebhook_OrkaResumeDoesNotBypassGuardLabel|GitHubWebhook_OrkaEquivalentCommandsCoalesceActiveWorkAction|GitHubWebhook_DuplicateAcceptedCommandRetriesFailedRunSignal|GitHubWebhook_OrkaPRLabelRequiresExactBaseBranchCase|CreateRepositoryMonitorCommandEventBlocksGuardedTarget|CreateRepositoryMonitorCommandEventRejectsClosedTarget|CreateRepositoryMonitorCommandEventRejectsIssueOutsideLabelScope|CreateRepositoryMonitorCommandEventRejectsIssueTargetSHA|CreateRepositoryMonitorCommandEventRejectsStalePRTargetSHA|CreateRepositoryMonitorCommandEventRequiresInventoryForIssuePlan|CreateRepositoryMonitorCommandEventRequiresInventoryForPR|GetRepositoryMonitorImplementationPatchPreview)'

run_go_test ./internal/controller 'TestRepositoryMonitor(IssueImplementToPRFakeGitHubE2E|PRReviewRepairReadinessAutomergeFakeGitHubE2E|IssueStopPreventsLateImplementationMutation|AutomergeMergeableStatePolicy|RunFailureState|RequireGreenCIGatesReviewQueue)'

run_go_test ./internal/store/sqlite 'TestMonitorWorkflowStoresActionsJobsAndMutations'
