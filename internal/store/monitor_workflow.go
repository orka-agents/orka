package store

import (
	"fmt"
	"strings"
)

const (
	repositoryMonitorDesiredActionDecompose = "decompose"
	repositoryMonitorDesiredActionRepair    = "repair"

	// WorkAction statuses. Active statuses hold the action's dedupe key;
	// terminal statuses are final and must not be reopened.
	//
	//	queued → (leased →) running → succeeded | failed | blocked | cancelled
	//	retry_pending: a command handoff that failed to signal its MonitorRun
	//	completed: an action coalesced into another active action at intake
	RepositoryMonitorWorkActionStatusQueued    = "queued"
	RepositoryMonitorWorkActionStatusLeased    = "leased"
	RepositoryMonitorWorkActionStatusRunning   = "running"
	RepositoryMonitorWorkActionStatusSucceeded = "succeeded"
	RepositoryMonitorWorkActionStatusFailed    = "failed"
	RepositoryMonitorWorkActionStatusBlocked   = "blocked"
	RepositoryMonitorWorkActionStatusCancelled = "cancelled"
	RepositoryMonitorWorkActionStatusCompleted = "completed"
	// RepositoryMonitorWorkActionStatusRetryPending marks durable command handoffs
	// that must be retried before a MonitorRun can safely proceed.
	RepositoryMonitorWorkActionStatusRetryPending = "retry_pending"
	// RepositoryMonitorWorkActionBlockedReasonRunSignalFailed identifies retryable
	// failures between durable command intake and MonitorRun signaling.
	RepositoryMonitorWorkActionBlockedReasonRunSignalFailed = "run_signal_failed"
)

// RepositoryMonitorWorkActionActiveStatuses lists the statuses of a WorkAction
// that still owns its dedupe key, in lookup order.
func RepositoryMonitorWorkActionActiveStatuses() []string {
	return []string{
		RepositoryMonitorWorkActionStatusQueued,
		RepositoryMonitorWorkActionStatusLeased,
		RepositoryMonitorWorkActionStatusRunning,
		RepositoryMonitorWorkActionStatusRetryPending,
	}
}

// IsRepositoryMonitorWorkActionActive reports whether status still owns its dedupe key.
func IsRepositoryMonitorWorkActionActive(status string) bool {
	switch status {
	case RepositoryMonitorWorkActionStatusQueued, RepositoryMonitorWorkActionStatusLeased,
		RepositoryMonitorWorkActionStatusRunning, RepositoryMonitorWorkActionStatusRetryPending:
		return true
	default:
		return false
	}
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
