package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
		observation.NamespaceUID = ""
		observation.TaskUID = uid
		observation.ID = uid + "/" + observation.ID
		observation.CounterID = uid + "/" + observation.CounterID
		// A worker cannot choose another Task/session's accounting counter.
		observation.Scope = store.UsageScopeCall
		observation.Source = store.UsageSourceProvider
		observation.AttemptID = ""
		switch observation.Status {
		case store.UsageStatusStarted:
			observation.InputTokens, observation.OutputTokens = nil, nil
			observation.CachedInputTokens, observation.CacheWriteInputTokens = nil, nil
		case "running", store.UsageStatusCompleted, store.UsageStatusFailed, store.UsageStatusCancelled:
		default:
			return store.ValidationErrorf("unsupported worker usage status")
		}
		observation.Complete = observation.Complete && observation.Status == store.UsageStatusCompleted
		// Use first receipt time, not a worker-selected accounting date. Replays
		// retain that time so transport retries remain idempotent after cleanup.
		observation.ObservedAt = event.CreatedAt
		var existing store.UsageObservation
		err := readUsageJSON(ctx, db, &existing, `SELECT data FROM usage_observations WHERE namespace = ? AND id = ?`, event.Namespace, observation.ID)
		if err == nil {
			observation.ObservedAt = existing.ObservedAt
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	} else if content.Harness != nil {
		h := content.Harness
		if h.TaskUID == "" || h.PromptID == "" || h.Sequence == 0 {
			return nil
		}
		observation = store.UsageObservation{
			ID:      fmt.Sprintf("acp/%s/%s/%d/%s", h.TaskUID, h.PromptID, h.Sequence, content.JournalKind),
			TaskUID: h.TaskUID, AttemptID: fmt.Sprintf("%d/%s", h.TaskAttempt, h.PromptID),
			CounterID: h.TaskUID + "/" + h.PromptID, Scope: store.UsageScopeAttempt,
			Source: store.UsageSourceAgent, Provider: content.Provider, Model: harnessUsageModel(event, content.Model),
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
	err := recordUsage(ctx, db, observation)
	// Invalid runtime accounting must not abort the execution journal. Counts
	// outside the reporting range are unavailable; provider worker events still
	// receive validation errors, and persistence failures still propagate.
	if content.Usage == nil && content.Harness != nil && errors.Is(err, store.ErrValidation) {
		return nil
	}
	return err
}

func harnessUsageModel(event store.ExecutionEvent, publicModel string) string {
	// The controller journal supplies the frozen profile model privately.
	// Public event fields remain redacted; recordUsage still sanitizes this
	// identifier before retaining it independently of the event stream.
	if model, ok := event.Internal["harnessV2UsageModel"].(string); ok && model != "" {
		return model
	}
	return publicModel
}

func nonzeroUsage(counts ...*int64) bool {
	for _, count := range counts {
		if count != nil && *count > 0 {
			return true
		}
	}
	return false
}
