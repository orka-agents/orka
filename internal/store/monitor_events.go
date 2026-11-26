package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"
)

// Decisions a repository monitor records on its audit events. An event that
// records a decision carries it in its metadata under
// RepositoryMonitorEventDecisionField, together with a reason code and, when a
// RepositoryMonitor spec clause decided, the clause under
// RepositoryMonitorEventPolicyField.
const (
	// RepositoryMonitorDecisionAccepted records work the monitor took on.
	RepositoryMonitorDecisionAccepted = "accepted"
	// RepositoryMonitorDecisionRefused records a request that an actor,
	// scope or budget rule turned down.
	RepositoryMonitorDecisionRefused = "refused"
	// RepositoryMonitorDecisionHeld records an item the monitor waits on
	// until its state changes.
	RepositoryMonitorDecisionHeld = "held"
	// RepositoryMonitorDecisionBlocked records work that configuration or a
	// guard stops from running.
	RepositoryMonitorDecisionBlocked = "blocked"
	// RepositoryMonitorDecisionActed records a completed change on GitHub.
	RepositoryMonitorDecisionActed = "acted"
	// RepositoryMonitorDecisionFailed records work that did not complete.
	RepositoryMonitorDecisionFailed = "failed"

	RepositoryMonitorEventDecisionField = "decision"
	RepositoryMonitorEventReasonField   = "reason"
	RepositoryMonitorEventPolicyField   = "policy"

	repositoryMonitorEventCommandIDField = "commandEventID"
	repositoryMonitorEventTaskNameField  = "taskName"
)

// Event types for command intake decisions.
const (
	RepositoryMonitorEventCommandAccepted = "command_accepted"
	RepositoryMonitorEventCommandRejected = "command_rejected"
	RepositoryMonitorEventCommandBlocked  = "command_blocked"
)

// repositoryMonitorEventDecisions maps each event type that records a
// decision to that decision. Event types not listed describe the monitor's
// own lifecycle (runs, inventory bookkeeping, ingestion) and carry none.
var repositoryMonitorEventDecisions = map[string]string{
	RepositoryMonitorEventCommandAccepted: RepositoryMonitorDecisionAccepted,
	"command_run_queued":                  RepositoryMonitorDecisionAccepted,
	"exact_event_run_queued":              RepositoryMonitorDecisionAccepted,
	"pause_label_changed":                 RepositoryMonitorDecisionAccepted,
	"review_task_created":                 RepositoryMonitorDecisionAccepted,
	"repair_task_created":                 RepositoryMonitorDecisionAccepted,
	"issue_action_task_created":           RepositoryMonitorDecisionAccepted,
	"update_branch_started":               RepositoryMonitorDecisionAccepted,

	RepositoryMonitorEventCommandRejected: RepositoryMonitorDecisionRefused,
	"issue_action_refused":                RepositoryMonitorDecisionRefused,
	"pause_label_rejected":                RepositoryMonitorDecisionRefused,
	"repair_refused":                      RepositoryMonitorDecisionRefused,
	"review_result_rejected":              RepositoryMonitorDecisionRefused,

	"item_skipped":              RepositoryMonitorDecisionHeld,
	"issue_action_skipped":      RepositoryMonitorDecisionHeld,
	"review_publish_skipped":    RepositoryMonitorDecisionHeld,
	"review_verdict_downgraded": RepositoryMonitorDecisionHeld,
	"review_verdict_escalated":  RepositoryMonitorDecisionHeld,

	RepositoryMonitorEventCommandBlocked: RepositoryMonitorDecisionBlocked,
	"command_target_blocked":             RepositoryMonitorDecisionBlocked,
	"issue_action_blocked":               RepositoryMonitorDecisionBlocked,
	"repair_blocked":                     RepositoryMonitorDecisionBlocked,
	"repair_withdrawn":                   RepositoryMonitorDecisionBlocked,

	"issue_action_recorded":    RepositoryMonitorDecisionActed,
	"repair_succeeded":         RepositoryMonitorDecisionActed,
	"review_publish_succeeded": RepositoryMonitorDecisionActed,
	"review_publish_updated":   RepositoryMonitorDecisionActed,
	"update_branch_succeeded":  RepositoryMonitorDecisionActed,

	"repair_failed":         RepositoryMonitorDecisionFailed,
	"review_publish_failed": RepositoryMonitorDecisionFailed,
	"run_failed":            RepositoryMonitorDecisionFailed,
	"update_branch_failed":  RepositoryMonitorDecisionFailed,
}

// RepositoryMonitorEventDecision returns the decision an event type records,
// or "" when the event type records none.
func RepositoryMonitorEventDecision(eventType string) string {
	return repositoryMonitorEventDecisions[strings.TrimSpace(eventType)]
}

// RepositoryMonitorEventInput describes one monitor audit event before it is
// stamped with its decision and identity.
type RepositoryMonitorEventInput struct {
	// ID overrides the derived event ID. Writers that already key an event
	// by another durable record set it so earlier events keep their identity.
	ID               string
	MonitorNamespace string
	MonitorName      string
	RunID            string
	ItemKind         string
	ItemNumber       int64
	ItemSHA          string
	EventType        string
	Actor            string
	Summary          string
	Metadata         map[string]any
	// Policy names the RepositoryMonitor spec clause that decided, if any.
	Policy string
}

// NewRepositoryMonitorEvent builds a monitor audit event. It records the
// decision for the event type and the policy clause in the metadata, and
// derives a deterministic ID unless input.ID is set.
//
// A held or refused decision is identified by its item, head, event type,
// reason, policy and command, but not by its run: polling the same item
// again records it once, and a new head, reason or policy records it anew.
// An accepted event that names a Task is identified by its run and Task, so
// creating the Task and adopting it after a restart record it once. Other
// events are identified by their run, summary and metadata, so a
// replay of the same step after a restart records it once while each new
// step is kept. Writers of events that legitimately repeat within one run,
// such as run_failed on each retry, put the attempt in the metadata.
func NewRepositoryMonitorEvent(input RepositoryMonitorEventInput) (*MonitorEvent, error) {
	metadata := maps.Clone(input.Metadata)
	if metadata == nil {
		metadata = map[string]any{}
	}
	decision := RepositoryMonitorEventDecision(input.EventType)
	if decision != "" {
		metadata[RepositoryMonitorEventDecisionField] = decision
	}
	policy := strings.TrimSpace(input.Policy)
	if policy != "" {
		metadata[RepositoryMonitorEventPolicyField] = policy
	}
	metadataJSON, err := json.Marshal(metadata)
	if err != nil {
		return nil, err
	}
	id := strings.TrimSpace(input.ID)
	if id == "" {
		var occurrence []string
		taskName := repositoryMonitorEventMetadataString(metadata, repositoryMonitorEventTaskNameField)
		switch {
		case decision == RepositoryMonitorDecisionHeld || decision == RepositoryMonitorDecisionRefused:
			occurrence = []string{
				repositoryMonitorEventMetadataString(metadata, RepositoryMonitorEventReasonField),
				policy,
				repositoryMonitorEventMetadataString(metadata, repositoryMonitorEventCommandIDField),
			}
		case decision == RepositoryMonitorDecisionAccepted && taskName != "":
			// Creating a Task and adopting it after a restart accept the same
			// step, so the Task, not how it was obtained, identifies it.
			occurrence = []string{input.RunID, taskName}
		default:
			occurrence = []string{input.RunID, input.Summary, string(metadataJSON)}
		}
		id = RepositoryMonitorEventID(input.MonitorNamespace, input.MonitorName, input.ItemKind, input.ItemNumber, input.ItemSHA, input.EventType, occurrence...)
	}
	return &MonitorEvent{
		ID:               id,
		MonitorNamespace: input.MonitorNamespace,
		MonitorName:      input.MonitorName,
		RunID:            input.RunID,
		ItemKind:         input.ItemKind,
		ItemNumber:       input.ItemNumber,
		ItemSHA:          input.ItemSHA,
		EventType:        input.EventType,
		Actor:            input.Actor,
		Summary:          input.Summary,
		MetadataJSON:     string(metadataJSON),
	}, nil
}

// RepositoryMonitorEventID derives a monitor event ID from the parts that
// identify one occurrence of an event.
func RepositoryMonitorEventID(namespace, monitorName, itemKind string, itemNumber int64, itemSHA, eventType string, occurrence ...string) string {
	hash := sha256.New()
	parts := append([]string{namespace, monitorName, itemKind, strconv.FormatInt(itemNumber, 10), itemSHA, eventType}, occurrence...)
	for _, part := range parts {
		// The length prefix keeps adjacent parts unambiguous.
		_, _ = fmt.Fprintf(hash, "%d:%s|", len(part), part)
	}
	return "mevt-" + hex.EncodeToString(hash.Sum(nil))[:32]
}

// RepositoryMonitorEventWriter stores monitor audit events.
type RepositoryMonitorEventWriter interface {
	CreateMonitorEvent(ctx context.Context, event *MonitorEvent) error
}

// RecordRepositoryMonitorEvent stores event once. An event whose ID is
// already stored was recorded by an earlier attempt, so the conflict is not
// an error.
func RecordRepositoryMonitorEvent(ctx context.Context, writer RepositoryMonitorEventWriter, event *MonitorEvent) error {
	if err := writer.CreateMonitorEvent(ctx, event); err != nil && !errors.Is(err, ErrConflict) {
		return err
	}
	return nil
}

func repositoryMonitorEventMetadataString(metadata map[string]any, key string) string {
	value, _ := metadata[key].(string)
	return strings.TrimSpace(value)
}
