/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package labels

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

const (
	maxLabelValueLength = 63
)

const (
	// Finalizer
	TaskFinalizer = "orka.ai/cleanup"

	// Labels
	LabelTask              = "orka.ai/task"
	LabelTaskType          = "orka.ai/task-type"
	LabelParentTask        = "orka.ai/parent-task"
	LabelScheduledRun      = "orka.ai/scheduled-run"
	LabelCreatedBy         = "orka.ai/created-by"
	LabelAgentRole         = "orka.ai/agent-role"
	LabelCoordinator       = "orka.ai/coordinator"
	LabelDelegatedAgent    = "orka.ai/delegated-agent"
	LabelIteration         = "orka.ai/iteration"
	LabelIterationGroup    = "orka.ai/iteration-group"
	LabelPurpose           = "orka.ai/purpose"
	LabelManaged           = "orka.ai/managed"
	LabelChatSession       = "orka.ai/chat-session"
	LabelSecurityTarget    = "orka.ai/security-target"
	LabelSecurityScanID    = "orka.ai/security-scan-id"
	LabelSecurityMode      = "orka.ai/security-scan-mode"
	LabelSecurityStage     = "orka.ai/security-stage"
	LabelSecurityScope     = "orka.ai/security-scope"
	LabelSecurityFindingID = "orka.ai/security-finding-id"
	LabelGitHubEvent       = "orka.ai/github-event"
	LabelGitHubAction      = "orka.ai/github-action"
	LabelGitHubRepository  = "orka.ai/github-repository"
	LabelGitHubTarget      = "orka.ai/github-target"
	LabelGitHubNumber      = "orka.ai/github-number"
	LabelTransactionID     = "orka.ai/transaction-id"
	LabelAuthProfile       = "orka.ai/auth-profile"

	// Annotations
	AnnotationCoordinationDepth             = "orka.ai/coordination-depth"
	AnnotationTransactionID                 = "orka.ai/transaction-id"
	AnnotationContextTokenProfile           = "orka.ai/context-token-profile"
	AnnotationTransactionIssuer             = "orka.ai/transaction-issuer"
	AnnotationTransactionSubject            = "orka.ai/transaction-subject"
	AnnotationTransactionRequestingWorkload = "orka.ai/transaction-requesting-workload"
	AnnotationTransactionScope              = "orka.ai/transaction-scope"
	AnnotationTransactionContextDigest      = "orka.ai/transaction-context-digest"
	AnnotationRequesterContextDigest        = "orka.ai/requester-context-digest"
	AnnotationTransactionTokenSecret        = "orka.ai/transaction-token-secret"
	AnnotationTransactionTokenPending       = "orka.ai/transaction-token-pending"
	AnnotationTransactionTokenPendingSince  = "orka.ai/transaction-token-pending-since"
	AnnotationAutoRetry                     = "orka.ai/auto-retry"
	AnnotationMaxRetries                    = "orka.ai/max-retries"
	AnnotationRetryCount                    = "orka.ai/retry-count"
	AnnotationOriginalPrompt                = "orka.ai/original-prompt"
	AnnotationParentTaskName                = "orka.ai/parent-task-name"
	AnnotationGitHubDelivery                = "orka.ai/github-delivery"
	AnnotationGitHubLabel                   = "orka.ai/github-label"
	AnnotationGitHubAction                  = "orka.ai/github-action"
	AnnotationGitHubRepository              = "orka.ai/github-repository"
	AnnotationGitHubNumber                  = "orka.ai/github-number"
	AnnotationGitHubURL                     = "orka.ai/github-url"
	AnnotationGitHubSender                  = "orka.ai/github-sender"
)

// SelectorValue returns a Kubernetes label-safe value derived from the input.
// Short, already-valid values are preserved as-is; longer values keep a readable
// prefix and add a stable hash suffix so selectors remain deterministic.
func SelectorValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}

	if len(value) <= maxLabelValueLength && isValidLabelValue(value) {
		return value
	}

	normalized := normalizeLabelValue(value)
	if len(normalized) <= maxLabelValueLength && isValidLabelValue(normalized) {
		return normalized
	}

	hash := shortHash(value)
	maxPrefixLength := max(1, maxLabelValueLength-len(hash)-1)
	prefix := normalized
	if len(prefix) > maxPrefixLength {
		prefix = prefix[:maxPrefixLength]
	}
	prefix = trimLabelValueEdges(prefix)
	if prefix == "" {
		prefix = "label"
	}

	return prefix + "-" + hash
}

// ParentTaskName returns the full parent task name for child Task/Agent objects.
// New objects store the full name in an annotation, while older objects may only
// have the label value.
func ParentTaskName(labelSet, annotationSet map[string]string) string {
	if annotationSet != nil {
		if parentName := strings.TrimSpace(annotationSet[AnnotationParentTaskName]); parentName != "" {
			return parentName
		}
	}
	if labelSet == nil {
		return ""
	}
	return strings.TrimSpace(labelSet[LabelParentTask])
}

func shortHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:12]
}

func normalizeLabelValue(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var out strings.Builder
	out.Grow(len(value))

	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			out.WriteRune(r)
		case r == '-', r == '_', r == '.':
			out.WriteRune(r)
		default:
			out.WriteRune('-')
		}
	}

	result := trimLabelValueEdges(out.String())
	if result == "" {
		return "label"
	}
	return result
}

func trimLabelValueEdges(value string) string {
	return strings.TrimFunc(value, func(r rune) bool {
		return !isAlphaNum(r)
	})
}

func isValidLabelValue(value string) bool {
	if len(value) > maxLabelValueLength {
		return false
	}
	if value == "" {
		return true
	}

	for i, r := range value {
		if i == 0 || i == len(value)-1 {
			if !isAlphaNum(r) {
				return false
			}
			continue
		}

		if !isAlphaNum(r) && r != '-' && r != '_' && r != '.' {
			return false
		}
	}

	return true
}

func isAlphaNum(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}
