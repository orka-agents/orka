package usage

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/store"
)

func TaskTokens(observations []store.UsageObservation, namespace, taskUID, attemptID string, now time.Time) (int64, int64, error) {
	if len(observations) == 0 {
		return 0, 0, nil
	}
	asOf := now.UTC().Add(time.Hour)
	report, err := Build(store.UsageData{Observations: observations}, store.UsageFilter{Until: asOf, AsOf: asOf})
	if err != nil {
		return 0, 0, err
	}
	for _, group := range report.OtherWork {
		for _, task := range group.Tasks {
			if task.Namespace != namespace || task.TaskUID != taskUID {
				continue
			}
			if attemptID == "" {
				return task.Totals.InputTokens, task.Totals.OutputTokens, nil
			}
			var inputTokens, outputTokens int64
			for _, measurement := range task.Measurements {
				if measurement.AttemptID != attemptID {
					continue
				}
				if measurement.InputTokens != nil {
					inputTokens += *measurement.InputTokens
				}
				if measurement.OutputTokens != nil {
					outputTokens += *measurement.OutputTokens
				}
			}
			return inputTokens, outputTokens, nil
		}
	}
	return 0, 0, nil
}

func hasFieldFold(fields map[string]json.RawMessage, name string) bool {
	for key, value := range fields {
		if value != nil && strings.EqualFold(key, name) {
			return true
		}
	}
	return false
}

func ExecutionEventObservation(event store.ExecutionEvent, attribution store.UsageAttribution) (*store.ExecutionEventUsage, error) {
	var content struct {
		Usage   *store.UsageObservation `json:"usage"`
		Harness *struct {
			TaskUID           string `json:"taskUID"`
			TaskAttempt       uint32 `json:"taskAttempt"`
			PromptID          string `json:"promptID"`
			SessionUID        string `json:"runtimeSessionUID"`
			SessionGeneration uint64 `json:"runtimeSessionGeneration"`
			Sequence          uint64 `json:"sequence"`
		} `json:"harnessV2"`
		Input         *int64 `json:"inputTokens"`
		Output        *int64 `json:"outputTokens"`
		Cached        *int64 `json:"cachedInputTokens"`
		CacheWrite    *int64 `json:"cacheWriteInputTokens"`
		Provider      string `json:"provider"`
		Model         string `json:"model"`
		Scope         string `json:"usageScope"`
		Reported      bool   `json:"usageReported"`
		Complete      bool   `json:"usageComplete"`
		JournalKind   string `json:"journalKind"`
		TerminalEvent string `json:"terminalEvent"`
	}
	if len(event.Content) == 0 {
		return nil, nil
	}
	if err := json.Unmarshal(event.Content, &content); err != nil {
		var fields map[string]json.RawMessage
		if json.Unmarshal(event.Content, &fields) == nil && hasFieldFold(fields, "usage") && attribution.TaskUID != "" {
			return nil, store.ValidationErrorf("invalid model usage event")
		}
		return nil, nil
	}
	var observation store.UsageObservation
	keepFirstObservedAt := false
	switch {
	case content.Usage != nil:
		uid := attribution.TaskUID
		if uid == "" {
			return nil, nil
		}
		observation = *content.Usage
		observation.NamespaceUID = ""
		observation.TaskUID = uid
		observation.ID = uid + "/" + observation.ID
		observation.CounterID = uid + "/" + observation.CounterID
		observation.Scope = store.UsageScopeCall
		observation.Source = store.UsageSourceProvider
		observation.AttemptID = ""
		switch observation.Status {
		case store.UsageStatusStarted:
			observation.InputTokens, observation.OutputTokens = nil, nil
			observation.CachedInputTokens, observation.CacheWriteInputTokens = nil, nil
		case "running", store.UsageStatusCompleted, store.UsageStatusFailed, store.UsageStatusCancelled:
		default:
			return nil, store.ValidationErrorf("unsupported worker usage status")
		}
		observation.Complete = observation.Complete && observation.Status == store.UsageStatusCompleted
		observation.ObservedAt = event.CreatedAt
		keepFirstObservedAt = true
	case content.Harness != nil:
		h := content.Harness
		if h.TaskUID == "" || h.PromptID == "" || h.Sequence == 0 {
			return nil, nil
		}
		model := content.Model
		if attribution.Model != "" {
			model = attribution.Model
		}
		observation = store.UsageObservation{
			ID:      fmt.Sprintf("acp/%s/%s/%d/%s", h.TaskUID, h.PromptID, h.Sequence, content.JournalKind),
			TaskUID: h.TaskUID, AttemptID: fmt.Sprintf("%d/%s", h.TaskAttempt, h.PromptID),
			CounterID: h.TaskUID + "/" + h.PromptID, Scope: store.UsageScopeAttempt,
			Source: store.UsageSourceAgent, Provider: content.Provider, Model: model,
		}
		if content.Scope == store.UsageScopeSession {
			observation.Scope = store.UsageScopeSession
			observation.CounterID = fmt.Sprintf("acp-session/%s/%d", h.SessionUID, h.SessionGeneration)
		}
		switch event.Type {
		case events.ExecutionEventTypeModelRequestStarted:
			observation.Status = store.UsageStatusStarted
		case events.ExecutionEventTypeModelRequestCompleted:
			observation.Status = store.UsageStatusCompleted
		case events.ExecutionEventTypeModelRequestFailed:
			observation.Status = store.UsageStatusFailed
			if content.TerminalEvent == "cancelled" {
				observation.Status = store.UsageStatusCancelled
			}
		case events.ExecutionEventTypeModelUsageUpdated:
			observation.Status = "running"
			if content.Reported || nonzeroUsage(content.Input, content.Output, content.Cached, content.CacheWrite) {
				observation.InputTokens, observation.OutputTokens = content.Input, content.Output
				observation.CachedInputTokens, observation.CacheWriteInputTokens = content.Cached, content.CacheWrite
				observation.Complete = content.Complete
			}
		default:
			return nil, nil
		}
	default:
		return nil, nil
	}
	observation.Namespace, observation.TaskName, observation.SessionName = event.Namespace, event.TaskName, event.SessionName
	if observation.ObservedAt.IsZero() {
		observation.ObservedAt = event.CreatedAt
	}
	if err := store.ValidateUsageObservation(observation); err != nil {
		if content.Usage == nil {
			return nil, nil
		}
		return nil, err
	}
	return &store.ExecutionEventUsage{Observation: observation, KeepFirstObservedAt: keepFirstObservedAt}, nil
}

func nonzeroUsage(counts ...*int64) bool {
	for _, count := range counts {
		if count != nil && *count > 0 {
			return true
		}
	}
	return false
}
