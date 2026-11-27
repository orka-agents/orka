package store

import (
	"fmt"
	"strings"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

const (
	repositoryMonitorDesiredActionDecompose = "decompose"
	repositoryMonitorDesiredActionRepair    = "repair"

	RepositoryMonitorWorkActionStatusQueued                 = storagev1.WorkActionStatusQueued
	RepositoryMonitorWorkActionStatusLeased                 = storagev1.WorkActionStatusLeased
	RepositoryMonitorWorkActionStatusRunning                = storagev1.WorkActionStatusRunning
	RepositoryMonitorWorkActionStatusRetryPending           = storagev1.WorkActionStatusRetryPending
	RepositoryMonitorWorkActionStatusSucceeded              = storagev1.WorkActionStatusSucceeded
	RepositoryMonitorWorkActionStatusFailed                 = storagev1.WorkActionStatusFailed
	RepositoryMonitorWorkActionStatusBlocked                = storagev1.WorkActionStatusBlocked
	RepositoryMonitorWorkActionStatusCancelled              = storagev1.WorkActionStatusCancelled
	RepositoryMonitorWorkActionStatusCompleted              = storagev1.WorkActionStatusCompleted
	RepositoryMonitorWorkActionBlockedReasonRunSignalFailed = "run_signal_failed"
)

func RepositoryMonitorWorkActionActiveStatuses() []string {
	return storagev1.WorkActionActiveStatuses()
}

func IsRepositoryMonitorWorkActionActive(status string) bool {
	return storagev1.IsWorkActionActive(status)
}

func RepositoryMonitorCoalescedCommandError(activeActionID string) string {
	return "coalesced with active workflow action " + activeActionID
}

func RepositoryMonitorDesiredActionForIntent(intent string) string {
	intent = strings.TrimSpace(intent)
	switch intent {
	case "fix":
		return repositoryMonitorDesiredActionRepair
	default:
		return intent
	}
}

func RepositoryMonitorDesiredActionForActionKind(actionKind string) string {
	switch strings.TrimSpace(actionKind) {
	case "issue_triage":
		return "triage"
	case "issue_research":
		return "research"
	case "issue_plan":
		return "plan"
	case "issue_implementation":
		return "implement"
	case "issue_decompose":
		return repositoryMonitorDesiredActionDecompose
	case "mutate_to_pr":
		return "mutate_to_pr"
	case "pr_review":
		return "review"
	case "pr_repair":
		return repositoryMonitorDesiredActionRepair
	case "pr_automerge":
		return "automerge"
	default:
		return strings.TrimSpace(actionKind)
	}
}

func RepositoryMonitorWorkActionID(commandID, desiredAction string) string {
	commandID = strings.TrimSpace(commandID)
	desiredAction = strings.TrimSpace(desiredAction)
	return fmt.Sprintf("wa-%d-%s-%s", len(commandID), commandID, desiredAction)
}

func RepositoryMonitorWorkActionDedupeKey(monitorNamespace, monitorName string, generation int64, targetKind string, targetNumber int64, targetSHA, snapshotDigest, desiredAction string) string {
	return fmt.Sprintf("%s/%s|gen:%d|%s:%d|sha:%s|snap:%s|action:%s", monitorNamespace, monitorName, generation, strings.TrimSpace(targetKind), targetNumber, strings.TrimSpace(targetSHA), strings.TrimSpace(snapshotDigest), strings.TrimSpace(desiredAction))
}
