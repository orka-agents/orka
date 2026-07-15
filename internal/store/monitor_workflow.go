package store

import (
	"fmt"
	"strings"
)

const (
	repositoryMonitorDesiredActionDecompose = "decompose"
	repositoryMonitorDesiredActionRepair    = "repair"

	// RepositoryMonitorWorkActionStatusRetryPending marks durable command handoffs
	// that must be retried before a MonitorRun can safely proceed.
	RepositoryMonitorWorkActionStatusRetryPending = "retry_pending"
	// RepositoryMonitorWorkActionBlockedReasonRunSignalFailed identifies retryable
	// failures between durable command intake and MonitorRun signaling.
	RepositoryMonitorWorkActionBlockedReasonRunSignalFailed = "run_signal_failed"
)

// RepositoryMonitorDesiredActionForIntent maps public command intents to durable workflow action names.
func RepositoryMonitorDesiredActionForIntent(intent string) string {
	switch strings.TrimSpace(intent) {
	case "approve_plan":
		return "approve"
	case "triage", "research", "plan", "implement", "review", repositoryMonitorDesiredActionRepair, "fix", "fix_ci", "update_branch", "readiness", "automerge", "stop", "resume", repositoryMonitorDesiredActionDecompose:
		if intent == "fix" {
			return repositoryMonitorDesiredActionRepair
		}
		if intent == "decompose" {
			return "decompose"
		}
		return strings.TrimSpace(intent)
	default:
		return strings.TrimSpace(intent)
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
	case "issue_approve_plan":
		return "approve"
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
