package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/orka-agents/orka/internal/store"
)

const (
	// repositoryMonitorAuditMaxPages bounds the event pages one audit request
	// reads to fill a page of decisions; the continue token resumes the rest.
	repositoryMonitorAuditMaxPages = 20
	repositoryMonitorAuditMaxLimit = 500
)

// RepositoryMonitorAuditRecord is one monitor decision joined with the
// command, Task and GitHub mutation it names.
type RepositoryMonitorAuditRecord struct {
	ID         string                              `json:"id"`
	CreatedAt  time.Time                           `json:"createdAt"`
	EventType  string                              `json:"eventType"`
	Decision   string                              `json:"decision"`
	Reason     string                              `json:"reason,omitempty"`
	Policy     string                              `json:"policy,omitempty"`
	RunID      string                              `json:"runID,omitempty"`
	ItemKind   string                              `json:"itemKind,omitempty"`
	ItemNumber int64                               `json:"itemNumber,omitempty"`
	ItemSHA    string                              `json:"itemSHA,omitempty"`
	Actor      string                              `json:"actor,omitempty"`
	Summary    string                              `json:"summary,omitempty"`
	Command    *RepositoryMonitorAuditCommandRef   `json:"command,omitempty"`
	Task       *RepositoryMonitorAuditTaskRef      `json:"task,omitempty"`
	Mutations  []RepositoryMonitorAuditMutationRef `json:"mutations,omitempty"`
}

// RepositoryMonitorAuditCommandRef names the command behind a decision.
type RepositoryMonitorAuditCommandRef struct {
	ID     string `json:"id"`
	Intent string `json:"intent,omitempty"`
	Source string `json:"source,omitempty"`
	Author string `json:"author,omitempty"`
	Status string `json:"status,omitempty"`
}

// RepositoryMonitorAuditTaskRef names the Task a decision created or settled.
type RepositoryMonitorAuditTaskRef struct {
	Name string `json:"name"`
}

// RepositoryMonitorAuditMutationRef names a GitHub write a decision made.
type RepositoryMonitorAuditMutationRef struct {
	ID        string `json:"id"`
	Operation string `json:"operation,omitempty"`
	Status    string `json:"status,omitempty"`
	GitHubURL string `json:"githubURL,omitempty"`
}

// ListRepositoryMonitorAudit lists a monitor's decisions in a time window,
// newest first, each joined with its command, Task and GitHub mutation.
func (h *Handlers) ListRepositoryMonitorAudit(c fiber.Ctx) error {
	if err := h.ensureRepositoryMonitorStore(); err != nil {
		return err
	}
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "listRepositoryMonitorAudit", h.contextTokenAuthorization.MonitorReadScopes); err != nil {
		return err
	}
	monitor, err := h.fetchRepositoryMonitor(c, namespace, c.Params("name"))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenRepositoryMonitor(c, "listRepositoryMonitorAudit", monitor); err != nil {
		return err
	}
	now := time.Now().UTC()
	since, err := parseRepositoryMonitorAuditTime(c.Query("since"), now)
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid since: "+err.Error())
	}
	until, err := parseRepositoryMonitorAuditTime(c.Query("until"), now)
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid until: "+err.Error())
	}
	if !since.IsZero() && !until.IsZero() && !since.Before(until) {
		return fiber.NewError(fiber.StatusBadRequest, "since must be before until")
	}
	limit, err := strconv.Atoi(c.Query("limit", "50"))
	if err != nil || limit < 1 || limit > repositoryMonitorAuditMaxLimit {
		return fiber.NewError(fiber.StatusBadRequest, fmt.Sprintf("limit must be between 1 and %d", repositoryMonitorAuditMaxLimit))
	}
	decisions, err := parseRepositoryMonitorAuditDecisions(c.Query("decision"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}

	joiner := &repositoryMonitorAuditJoiner{store: h.repositoryMonitorStore, namespace: namespace, monitorName: monitor.Name}
	records := make([]RepositoryMonitorAuditRecord, 0, limit)
	cursor := monitorListCursor(c)
	for range repositoryMonitorAuditMaxPages {
		// Each read asks for the room left on the page, so the store's own
		// cursor resumes exactly where this page stops.
		events, next, err := h.repositoryMonitorStore.ListMonitorEvents(c.Context(), store.MonitorEventFilter{
			Namespace:   namespace,
			MonitorName: monitor.Name,
			Since:       since,
			Until:       until,
			Limit:       limit - len(records),
			Cursor:      cursor,
		})
		if err != nil {
			return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list monitor events: %v", err))
		}
		for i := range events {
			record, ok, err := joiner.record(c.Context(), &events[i], decisions)
			if err != nil {
				return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to join monitor audit record: %v", err))
			}
			if ok {
				records = append(records, record)
			}
		}
		cursor = next
		if cursor == "" || len(records) >= limit {
			break
		}
	}
	return c.JSON(fiber.Map{apiFieldItems: records, apiFieldMetadata: fiber.Map{commandIntentContinue: cursor}})
}

// parseRepositoryMonitorAuditTime reads a window bound as a duration before
// now ("24h") or an RFC 3339 time. An empty value is unbounded.
func parseRepositoryMonitorAuditTime(value string, now time.Time) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, nil
	}
	if ago, err := time.ParseDuration(value); err == nil {
		if ago < 0 {
			return time.Time{}, errors.New("duration must not be negative")
		}
		return now.Add(-ago), nil
	}
	at, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, errors.New("use a duration such as 24h or an RFC 3339 time")
	}
	return at.UTC(), nil
}

func parseRepositoryMonitorAuditDecisions(value string) (map[string]struct{}, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	decisions := map[string]struct{}{}
	for decision := range strings.SplitSeq(value, ",") {
		decision = strings.ToLower(strings.TrimSpace(decision))
		switch decision {
		case store.RepositoryMonitorDecisionAccepted, store.RepositoryMonitorDecisionRefused, store.RepositoryMonitorDecisionHeld,
			store.RepositoryMonitorDecisionBlocked, store.RepositoryMonitorDecisionActed, store.RepositoryMonitorDecisionFailed:
			decisions[decision] = struct{}{}
		default:
			return nil, fmt.Errorf("unknown decision %q", decision)
		}
	}
	return decisions, nil
}

// repositoryMonitorAuditJoiner resolves the records a decision names, once
// per request for each command and mutation.
type repositoryMonitorAuditJoiner struct {
	store       store.RepositoryMonitorStore
	namespace   string
	monitorName string
	commands    map[string]*RepositoryMonitorAuditCommandRef
	mutations   map[string]RepositoryMonitorAuditMutationRef
}

func (j *repositoryMonitorAuditJoiner) record(ctx context.Context, event *store.MonitorEvent, decisions map[string]struct{}) (RepositoryMonitorAuditRecord, bool, error) {
	var metadata map[string]any
	if strings.TrimSpace(event.MetadataJSON) != "" {
		if err := json.Unmarshal([]byte(event.MetadataJSON), &metadata); err != nil {
			// A record that cannot be read records no decision.
			return RepositoryMonitorAuditRecord{}, false, nil
		}
	}
	decision := auditMetadataString(metadata, store.RepositoryMonitorEventDecisionField)
	if decision == "" {
		return RepositoryMonitorAuditRecord{}, false, nil
	}
	if _, ok := decisions[decision]; decisions != nil && !ok {
		return RepositoryMonitorAuditRecord{}, false, nil
	}
	record := RepositoryMonitorAuditRecord{
		ID:         event.ID,
		CreatedAt:  event.CreatedAt,
		EventType:  event.EventType,
		Decision:   decision,
		Reason:     auditMetadataString(metadata, store.RepositoryMonitorEventReasonField),
		Policy:     auditMetadataString(metadata, store.RepositoryMonitorEventPolicyField),
		RunID:      event.RunID,
		ItemKind:   event.ItemKind,
		ItemNumber: event.ItemNumber,
		ItemSHA:    event.ItemSHA,
		Actor:      event.Actor,
		Summary:    event.Summary,
	}
	if taskName := auditMetadataString(metadata, "taskName"); taskName != "" {
		record.Task = &RepositoryMonitorAuditTaskRef{Name: taskName}
	}
	if commandID := auditMetadataString(metadata, "commandEventID"); commandID != "" {
		command, err := j.command(ctx, commandID)
		if err != nil {
			return RepositoryMonitorAuditRecord{}, false, err
		}
		record.Command = command
	}
	if mutationID := auditMetadataString(metadata, "mutationID"); mutationID != "" {
		mutation, err := j.mutation(ctx, mutationID)
		if err != nil {
			return RepositoryMonitorAuditRecord{}, false, err
		}
		record.Mutations = []RepositoryMonitorAuditMutationRef{mutation}
	}
	return record, true, nil
}

func (j *repositoryMonitorAuditJoiner) command(ctx context.Context, id string) (*RepositoryMonitorAuditCommandRef, error) {
	if ref, ok := j.commands[id]; ok {
		return ref, nil
	}
	ref := &RepositoryMonitorAuditCommandRef{ID: id}
	command, err := j.store.GetCommandEvent(ctx, j.namespace, id)
	switch {
	case err == nil && command.MonitorName == j.monitorName:
		ref.Intent, ref.Source, ref.Author, ref.Status = command.Intent, command.Source, command.Author, command.Status
	case err == nil:
		// A record of another monitor is not this monitor's to show.
	case !errors.Is(err, store.ErrNotFound):
		return nil, err
	}
	if j.commands == nil {
		j.commands = map[string]*RepositoryMonitorAuditCommandRef{}
	}
	j.commands[id] = ref
	return ref, nil
}

func (j *repositoryMonitorAuditJoiner) mutation(ctx context.Context, id string) (RepositoryMonitorAuditMutationRef, error) {
	if ref, ok := j.mutations[id]; ok {
		return ref, nil
	}
	ref := RepositoryMonitorAuditMutationRef{ID: id}
	mutation, err := j.store.GetGitHubMutationRecord(ctx, j.namespace, id)
	switch {
	case err == nil && mutation.MonitorName == j.monitorName:
		ref.Operation, ref.Status, ref.GitHubURL = mutation.Operation, mutation.Status, mutation.GitHubURL
	case err == nil:
		// A record of another monitor is not this monitor's to show.
	case !errors.Is(err, store.ErrNotFound):
		return RepositoryMonitorAuditMutationRef{}, err
	}
	if j.mutations == nil {
		j.mutations = map[string]RepositoryMonitorAuditMutationRef{}
	}
	j.mutations[id] = ref
	return ref, nil
}

func auditMetadataString(metadata map[string]any, key string) string {
	value, _ := metadata[key].(string)
	return strings.TrimSpace(value)
}
