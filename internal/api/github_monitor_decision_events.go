package api

import (
	"context"
	"fmt"
	"strings"

	"github.com/gofiber/fiber/v3"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

// Reason codes for command intake decisions.
const (
	repositoryMonitorCommandReasonPermission = "actor_permission_not_allowed"
	repositoryMonitorCommandReasonGuardLabel = "guard_label"

	// Reason codes for label commands outside a monitor's target scope.
	repositoryMonitorCommandReasonTargetNotOpen        = "target_not_open"
	repositoryMonitorCommandReasonIssueTargetsDisabled = "issue_targets_disabled"
	repositoryMonitorCommandReasonExcludedLabel        = "target_excluded_label"
	repositoryMonitorCommandReasonMissingLabel         = "target_missing_required_label"

	repositoryMonitorWebhookEventActor  = "github-webhook"
	repositoryMonitorAPIEventActor      = "orka-api"
	repositoryMonitorEventSenderField   = "sender"
	repositoryMonitorEventSourceField   = "source"
	repositoryMonitorEventDeliveryField = "delivery"
	repositoryMonitorEventIntentField   = "intent"

	// repositoryMonitorReadOnlyCommandPermissionPolicy names the fixed
	// triage-or-better rule for read-only commands, which no monitor setting
	// changes.
	repositoryMonitorReadOnlyCommandPermissionPolicy = "builtin:read-only-command-permission"
)

// recordRepositoryMonitorCommandDecision records the intake decision for
// command as a monitor event. Its ID is derived from the command, so a
// redelivered command records it once. labels are the target's current
// labels, which name the guard behind a blocked command.
func (h *Handlers) recordRepositoryMonitorCommandDecision(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, command *store.CommandEvent, labels []string) error {
	if monitor == nil || command == nil || h.repositoryMonitorStore == nil {
		return nil
	}
	var eventType, reason, policy, outcome string
	switch command.Status {
	case githubCommandStatusAccepted:
		eventType, outcome = store.RepositoryMonitorEventCommandAccepted, "accepted"
	case githubCommandStatusRejected:
		eventType, outcome = store.RepositoryMonitorEventCommandRejected, "rejected"
		reason = repositoryMonitorCommandReasonPermission
		policy = repositoryMonitorPermissionPolicy(monitor, command.Permission, command.Intent)
	case githubCommandStatusBlocked:
		eventType, outcome = store.RepositoryMonitorEventCommandBlocked, "blocked"
		reason = repositoryMonitorCommandReasonGuardLabel
		policy = repositoryMonitorGuardLabelPolicy(monitor, labels)
	default:
		// Other statuses settle work an earlier decision already recorded.
		return nil
	}
	metadata := map[string]any{
		"commandEventID":                  command.ID,
		repositoryMonitorEventIntentField: command.Intent,
		repositoryMonitorEventSourceField: command.Source,
		repositoryMonitorEventSenderField: command.Author,
	}
	if reason != "" {
		metadata[store.RepositoryMonitorEventReasonField] = reason
		metadata["permission"] = command.Permission
	}
	itemSHA := strings.TrimSpace(command.HeadSHA)
	if itemSHA == "" {
		itemSHA = strings.TrimSpace(command.IssueSnapshotDigest)
	}
	event, err := store.NewRepositoryMonitorEvent(store.RepositoryMonitorEventInput{
		MonitorNamespace: monitor.Namespace,
		MonitorName:      monitor.Name,
		ItemKind:         command.Kind,
		ItemNumber:       command.Number,
		ItemSHA:          itemSHA,
		EventType:        eventType,
		Actor:            repositoryMonitorCommandDecisionActor(command),
		Summary:          fmt.Sprintf("Command %s %s %s", command.ID, command.Intent, outcome),
		Metadata:         metadata,
		Policy:           policy,
	})
	if err != nil {
		return err
	}
	return store.RecordRepositoryMonitorEvent(ctx, h.repositoryMonitorStore, event)
}

func repositoryMonitorCommandDecisionActor(command *store.CommandEvent) string {
	if command.Source == "api" {
		return repositoryMonitorAPIEventActor
	}
	return repositoryMonitorWebhookEventActor
}

// repositoryMonitorPermissionPolicy names the clause that refused permission
// for intent, mirroring repositoryMonitorPermissionAllowedForIntent.
func repositoryMonitorPermissionPolicy(monitor *corev1alpha1.RepositoryMonitor, permission, intent string) string {
	readOnly := repositoryMonitorReadOnlyCommandIntent(intent) && (strings.TrimSpace(intent) != githubActionReview || !monitor.Spec.Review.Publish.Enabled)
	if readOnly {
		return repositoryMonitorReadOnlyCommandPermissionPolicy
	}
	minimum := strings.ToLower(strings.TrimSpace(monitor.Spec.Triggers.GitHub.Labels.RequireActorPermission))
	if minimum == "" {
		minimum = githubPermissionWrite
	}
	if !repositoryMonitorPermissionInList(strings.ToLower(strings.TrimSpace(permission)), repositoryMonitorPermissionsAtLeast(minimum)) {
		return "spec.triggers.github.labels.requireActorPermission=" + minimum
	}
	return "spec.policy.allowedRepositoryPermissions"
}

// repositoryMonitorGuardLabelPolicy names the label list that holds the
// target's guard label.
func repositoryMonitorGuardLabelPolicy(monitor *corev1alpha1.RepositoryMonitor, labels []string) string {
	guard := repositoryMonitorCommandGuardLabel(monitor, labels)
	if guard == "" {
		return ""
	}
	if repositoryMonitorWebhookMatchingLabel(monitor.Spec.Policy.ProtectedLabels, []string{guard}) != "" {
		return "spec.policy.protectedLabels"
	}
	return "spec.policy.pauseLabels"
}

// recordRepositoryMonitorOutOfScopeLabelCommand records a label command that
// reached a monitor for its repository but falls outside the monitor's target
// scope. No command is recorded for it, so the event is keyed by the delivery
// and label: a redelivery records it once.
func (h *Handlers) recordRepositoryMonitorOutOfScopeLabelCommand(c fiber.Ctx, monitor *corev1alpha1.RepositoryMonitor, payload githubLabelWebhookPayload, target githubLabelTarget, intent, delivery string) error {
	reason, policy := repositoryMonitorLabelCommandScopeRejection(monitor, payload.Repository, target, intent)
	if reason == "" {
		return nil
	}
	label := strings.ToLower(strings.TrimSpace(payload.Label.Name))
	itemSHA := strings.TrimSpace(target.HeadSHA)
	if target.Kind == repositoryMonitorTargetKindIssue {
		itemSHA = githubIssueSnapshotDigest(monitor, target)
	}
	event, err := store.NewRepositoryMonitorEvent(store.RepositoryMonitorEventInput{
		ID:               store.RepositoryMonitorEventID(monitor.Namespace, monitor.Name, target.Kind, int64(target.Number), "", store.RepositoryMonitorEventCommandRejected, delivery, label),
		MonitorNamespace: monitor.Namespace,
		MonitorName:      monitor.Name,
		ItemKind:         target.Kind,
		ItemNumber:       int64(target.Number),
		ItemSHA:          itemSHA,
		EventType:        store.RepositoryMonitorEventCommandRejected,
		Actor:            repositoryMonitorWebhookEventActor,
		Summary:          fmt.Sprintf("Label command %s on %s #%d not run: %s", label, strings.ReplaceAll(target.Kind, "_", " "), target.Number, reason),
		Metadata: map[string]any{
			store.RepositoryMonitorEventReasonField: reason,
			repositoryMonitorEventIntentField:       intent,
			repositoryMonitorEventSourceField:       githubCommandEventSourceLabel,
			repositoryMonitorEventSenderField:       payload.Sender.Login,
			repositoryMonitorEventDeliveryField:     delivery,
			apiFieldLabel:                           label,
		},
		Policy: policy,
	})
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to encode repository monitor command rejection: %v", err))
	}
	if err := store.RecordRepositoryMonitorEvent(c.Context(), h.repositoryMonitorStore, event); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to record repository monitor command rejection: %v", err))
	}
	return nil
}

// repositoryMonitorLabelCommandScopeRejection returns why a label command
// that repositoryMonitorAcceptsLabelCommand refused falls outside the
// monitor's target scope, and the clause that decided. It returns no reason
// when the command was not addressed to the monitor: label intake is off,
// the monitor is suspended, or the webhook is for another repository.
func repositoryMonitorLabelCommandScopeRejection(monitor *corev1alpha1.RepositoryMonitor, repo githubWebhookRepository, target githubLabelTarget, intent string) (string, string) {
	if monitor == nil || repositoryMonitorWebhookSuspended(monitor) || !monitor.Spec.Triggers.GitHub.Labels.Enabled {
		return "", ""
	}
	owner, repository, err := parseRepositoryMonitorGitHubURL(monitor.Spec.RepoURL)
	if err != nil || !strings.EqualFold(strings.TrimSpace(repo.FullName), owner+"/"+repository) {
		return "", ""
	}
	if repositoryMonitorAcceptsLabelCommand(monitor, repo, target, intent) || target.IsPR || target.Kind != repositoryMonitorTargetKindIssue {
		return "", ""
	}
	if !repositoryMonitorControlCommandIntent(intent) && strings.TrimSpace(target.State) != "" && !strings.EqualFold(strings.TrimSpace(target.State), "open") {
		return repositoryMonitorCommandReasonTargetNotOpen, ""
	}
	issues := monitor.Spec.Targets.Issues
	if !issues.Enabled {
		return repositoryMonitorCommandReasonIssueTargetsDisabled, "spec.targets.issues.enabled=false"
	}
	if repositoryMonitorWebhookMatchingLabel(issues.ExcludeLabels, target.Labels) != "" {
		return repositoryMonitorCommandReasonExcludedLabel, "spec.targets.issues.excludeLabels"
	}
	if len(issues.IncludeLabels) > 0 && repositoryMonitorWebhookMatchingLabel(issues.IncludeLabels, target.Labels) == "" {
		return repositoryMonitorCommandReasonMissingLabel, "spec.targets.issues.includeLabels"
	}
	return "", ""
}
