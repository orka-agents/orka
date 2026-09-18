package sqlite

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/store"
)

// projectUsageEvent runs in the same transaction as the authenticated event.
// Only the normalized provider recorder and the controller's ACP journal are
// accounting sources. Arbitrary timeline text and context-window sizes are not.
func projectUsageEvent(ctx context.Context, db taskDataExecutor, event store.ExecutionEvent) error {
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
		return nil
	}
	if err := json.Unmarshal(event.Content, &content); err != nil {
		var fields map[string]json.RawMessage
		if json.Unmarshal(event.Content, &fields) == nil && fields["usage"] != nil && event.Internal["usageTaskUID"] != nil {
			return store.ValidationErrorf("invalid model usage event")
		}
		return nil
	}
	var observation store.UsageObservation
	if content.Usage != nil {
		uid, ok := event.Internal["usageTaskUID"].(string)
		if !ok || uid == "" {
			return nil
		}
		observation = *content.Usage
		observation.TaskUID = uid
		observation.ID = uid + "/" + observation.ID
		observation.CounterID = uid + "/" + observation.CounterID
		// A worker cannot choose another Task/session's accounting counter.
		observation.Scope = store.UsageScopeCall
	} else if content.Harness != nil {
		h := content.Harness
		if h.TaskUID == "" || h.PromptID == "" || h.Sequence == 0 {
			return nil
		}
		observation = store.UsageObservation{
			ID:      fmt.Sprintf("acp/%s/%s/%d/%s", h.TaskUID, h.PromptID, h.Sequence, content.JournalKind),
			TaskUID: h.TaskUID, AttemptID: fmt.Sprintf("%d/%s", h.TaskAttempt, h.PromptID),
			CounterID: h.TaskUID + "/" + h.PromptID, Scope: store.UsageScopeAttempt,
			Source: store.UsageSourceAgent, Provider: content.Provider, Model: content.Model,
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
			return nil
		}
	} else {
		return nil
	}
	observation.Namespace, observation.TaskName, observation.SessionName = event.Namespace, event.TaskName, event.SessionName
	if observation.ObservedAt.IsZero() {
		observation.ObservedAt = event.CreatedAt
	}
	return recordUsage(ctx, db, observation)
}

func nonzeroUsage(counts ...*int64) bool {
	for _, count := range counts {
		if count != nil && *count > 0 {
			return true
		}
	}
	return false
}
