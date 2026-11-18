package store

import (
	"fmt"
	"strings"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

const (
	repositoryMonitorDesiredActionDecompose = "decompose"
	repositoryMonitorDesiredActionRepair    = "repair"

	// WorkAction statuses, defined by the storage contract; see
	// storagev1.WorkActionStatusQueued.
	RepositoryMonitorWorkActionStatusQueued       = storagev1.WorkActionStatusQueued
	RepositoryMonitorWorkActionStatusLeased       = storagev1.WorkActionStatusLeased
	RepositoryMonitorWorkActionStatusRunning      = storagev1.WorkActionStatusRunning
	RepositoryMonitorWorkActionStatusRetryPending = storagev1.WorkActionStatusRetryPending
	RepositoryMonitorWorkActionStatusSucceeded    = storagev1.WorkActionStatusSucceeded
	RepositoryMonitorWorkActionStatusFailed       = storagev1.WorkActionStatusFailed
	RepositoryMonitorWorkActionStatusBlocked      = storagev1.WorkActionStatusBlocked
	RepositoryMonitorWorkActionStatusCancelled    = storagev1.WorkActionStatusCancelled
	RepositoryMonitorWorkActionStatusCompleted    = storagev1.WorkActionStatusCompleted
	// RepositoryMonitorWorkActionBlockedReasonRunSignalFailed identifies retryable
	// failures between durable command intake and MonitorRun signaling.
	RepositoryMonitorWorkActionBlockedReasonRunSignalFailed = "run_signal_failed"
)

// RepositoryMonitorWorkActionActiveStatuses lists the statuses of a WorkAction
// that still owns its dedupe key, in lookup order.
func RepositoryMonitorWorkActionActiveStatuses() []string {
	return storagev1.WorkActionActiveStatuses()
}

// IsRepositoryMonitorWorkActionActive reports whether status still owns its dedupe key.
func IsRepositoryMonitorWorkActionActive(status string) bool {
	return storagev1.IsWorkActionActive(status)
}

// RepositoryMonitorCoalescedCommandError is the outcome recorded on a command
// whose work action was coalesced (status completed) into the active action
// activeActionID.
func RepositoryMonitorCoalescedCommandError(activeActionID string) string {
	return "coalesced with active workflow action " + activeActionID
}

// RepositoryMonitorDesiredActionForIntent maps public command intents to durable workflow action names.
func RepositoryMonitorDesiredActionForIntent(intent string) string {
	intent = strings.TrimSpace(intent)
	switch intent {
	case "fix":
		return repositoryMonitorDesiredActionRepair
	default:
		return intent
	}
}

// RepositoryMonitorDesiredActionForActionKind maps internal action record kinds to workflow action names.
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

// RepositoryMonitorWorkActionID deterministically identifies one command/action handoff.
func RepositoryMonitorWorkActionID(commandID, desiredAction string) string {
	commandID = strings.TrimSpace(commandID)
	desiredAction = strings.TrimSpace(desiredAction)
	// The length prefix keeps the two components unambiguous without treating
	// command identifiers as credentials that need password hashing.
	return fmt.Sprintf("wa-%d-%s-%s", len(commandID), commandID, desiredAction)
}

// RepositoryMonitorWorkActionDedupeKey returns a monitor-scoped coalescing key.
func RepositoryMonitorWorkActionDedupeKey(monitorNamespace, monitorName string, generation int64, targetKind string, targetNumber int64, targetSHA, snapshotDigest, desiredAction string) string {
	return fmt.Sprintf("%s/%s|gen:%d|%s:%d|sha:%s|snap:%s|action:%s", monitorNamespace, monitorName, generation, strings.TrimSpace(targetKind), targetNumber, strings.TrimSpace(targetSHA), strings.TrimSpace(snapshotDigest), strings.TrimSpace(desiredAction))
}
