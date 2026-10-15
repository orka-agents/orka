package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/events"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/harness/v2/eventjournal"
	"github.com/orka-agents/orka/internal/store"
)

func TestExecutionEventUsageSummarySameEvent(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		for _, contextOnly := range []bool{false, true} {
			for _, field := range []string{"provider", "model", "explicit-model"} {
				if field == "explicit-model" && !terminal {
					continue
				}
				for _, fixture := range []struct {
					name, value, normalized string
					benign                  bool
				}{
					{name: "assignment", value: "=fixture-value", normalized: "=fixture-value"},
					{name: "normalized-assignment", value: " \t=fixture-value ?x=1\t", normalized: "=fixture-value"},
					{name: "benign", value: " openai ?x=1\t", normalized: "openai", benign: true},
				} {
					t.Run(usageSummaryTestName(terminal, contextOnly)+"/"+field+"/"+fixture.name, func(t *testing.T) {
						var provider, model, explicitModel string
						switch field {
						case "provider":
							provider = fixture.value
						case "model":
							model = fixture.value
						case "explicit-model":
							model, explicitModel = modelHistoryBenign, fixture.value
						}
						journal, state := newProviderModelHistoryJournal(t, provider, model)
						appendUsageSummaryEvent(t, state, terminal, contextOnly, explicitModel)
						rows := modelHistoryRows(t, journal)
						if len(rows) != 1 {
							t.Fatalf("usage and replay persisted %d rows, want 1", len(rows))
						}
						dto := usageSummaryPublicDTO(t, rows[0], contextOnly, !fixture.benign && !contextOnly)
						providers, models := providerModelHistoryPublicCopies(t, rows[0])
						copies := models
						if field == "provider" {
							copies = providers
						}
						// Metadata is individually safe here; mask only the new
						// generated summary when their combination is sensitive.
						want := fixture.normalized
						for _, value := range copies {
							if value != want {
								t.Errorf("public metadata copy = %q, want %q", value, want)
							}
							joined := dto.Summary + value
							if events.RedactExecutionEventText(joined) != joined {
								t.Error("actual public usage summary and metadata reconstruct a protected assignment")
							}
						}
					})
				}
			}
		}
	}
}

func TestExecutionEventUsageSummaryProtectsLaterText(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		for _, contextOnly := range []bool{false, true} {
			for _, fixture := range []struct {
				name, provider, text string
				benign               bool
			}{
				{name: "no-metadata", text: "=fixture-value"},
				{name: "masked-summary", provider: "=earlier-fixture-value", text: "=fixture-value"},
				{name: "normalized-text", text: "\n=fixture-value\t"},
				{name: "benign", text: "ordinary output", benign: true},
			} {
				t.Run(usageSummaryTestName(terminal, contextOnly)+"/"+fixture.name, func(t *testing.T) {
					journal, state := newProviderModelHistoryJournal(t, fixture.provider, "")
					usage := appendUsageSummaryEvent(t, state, terminal, contextOnly, "")
					// Terminal usage and its transcript are distinct public records
					// even when they share the completed protocol event identity.
					if !terminal {
						usage = modelHistoryEvent(3)
						usage.Type = harnessv2.EventCompleted
						usage.Completed = &harnessv2.CompletedEvent{
							StopReason: harnessv2.ACPStopReasonEndTurn,
							Result: harnessv2.PromptResult{
								Content: []harnessv2.ContentBlock{{Type: harnessv2.ContentBlockText, Text: fixture.text}},
							},
						}
					}
					if err := usage.Validate(harnessv2.DefaultEventStreamLimits()); err != nil {
						t.Fatal(err)
					}
					if _, isNew, err := state.AppendAssistantTranscriptIfNew(context.Background(), usage, fixture.text, false); err != nil || !isNew {
						t.Fatalf("append transcript: new=%t err=%v", isNew, err)
					}
					rows := modelHistoryRows(t, journal)
					if len(rows) != 2 {
						t.Fatalf("persisted rows = %d, want 2", len(rows))
					}
					summaryRedacted := fixture.provider != "" && !contextOnly
					first := usageSummaryPublicDTO(t, rows[0], contextOnly, summaryRedacted)
					second := NewExecutionEventResponse(rows[1])
					wantText, wantSummary := fixture.text, strings.TrimSpace(fixture.text)
					if !fixture.benign && !contextOnly && !summaryRedacted {
						wantText, wantSummary = events.ExecutionEventRedactedValue, events.ExecutionEventRedactedValue
					}
					if second.ContentText != wantText || second.Summary != wantSummary {
						t.Errorf("later text = %q / %q, want %q / %q", second.ContentText, second.Summary, wantText, wantSummary)
					}
					for _, value := range []string{second.ContentText, second.Summary} {
						joined := first.Summary + value
						if events.RedactExecutionEventText(joined) != joined {
							t.Error("actual public usage summary and later text reconstruct a protected assignment")
						}
					}
				})
			}
		}
	}
}

func TestExecutionEventUsageSummaryAfterAcceptedMetadata(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(usageSummaryTestName(terminal, false), func(t *testing.T) {
			journal, state := newProviderModelHistoryJournal(t, "=fixture-value", "")
			appendModelHistoryAccepted(t, state, true)
			appendUsageSummaryEvent(t, state, terminal, false, "")
			rows := modelHistoryRows(t, journal)
			if len(rows) != 2 {
				t.Fatalf("persisted rows = %d, want 2", len(rows))
			}
			providers, _ := providerModelHistoryPublicCopies(t, rows[0])
			usage := usageSummaryPublicDTO(t, rows[1], false, true)
			for _, provider := range providers {
				joined := usage.Summary + provider
				if events.RedactExecutionEventText(joined) != joined {
					t.Error("later generated usage summary completes an assignment using already-published accepted metadata")
				}
			}
		})
	}
}

func TestExecutionEventUsageSummaryWithOverriddenHistoricalModel(t *testing.T) {
	journal, state := newProviderModelHistoryJournal(t, "", "=fixture-value")
	appendModelHistoryAccepted(t, state, true)
	appendUsageSummaryEvent(t, state, true, false, modelHistoryBenign)
	rows := modelHistoryRows(t, journal)
	if len(rows) != 2 {
		t.Fatalf("persisted rows = %d, want 2", len(rows))
	}
	_, models := providerModelHistoryPublicCopies(t, rows[0])
	usage := usageSummaryPublicDTO(t, rows[1], false, true)
	for _, model := range models {
		joined := usage.Summary + model
		if events.RedactExecutionEventText(joined) != joined {
			t.Error("usage summary completes an assignment using the overridden historical model")
		}
	}
}

func TestExecutionEventUsageSummaryOptionalCounts(t *testing.T) {
	for _, fixture := range []struct {
		name, summary string
		usage         harnessv2.UsageUpdate
	}{
		{
			name: "unavailable caches", summary: "Model usage updated: 120 input, 30 output tokens",
			usage: harnessv2.UsageUpdate{InputTokens: 120, OutputTokens: 30},
		},
		{
			name: "reported zero caches", summary: "Model usage updated: 120 input, 30 output, 0 cached input tokens",
			usage: harnessv2.UsageUpdate{InputTokens: 120, OutputTokens: 30,
				CachedInputTokens: new(uint64(0)), CacheWriteInputTokens: new(uint64(0))},
		},
		{
			name: "cache writes only", summary: "Model usage updated: 120 input, 30 output tokens",
			usage: harnessv2.UsageUpdate{InputTokens: 120, OutputTokens: 30, CacheWriteInputTokens: new(uint64(40))},
		},
		{
			name: "reported zero with context", summary: "Model usage updated: 0 input, 0 output tokens",
			usage: harnessv2.UsageUpdate{ContextWindowUsed: new(uint64(53_000)), ContextWindowSize: new(uint64(200_000))},
		},
	} {
		for _, terminal := range []bool{false, true} {
			for _, provider := range []string{"openai", "=fixture-value"} {
				t.Run(fixture.name+"/"+usageSummaryTestName(terminal, false)+"/"+provider, func(t *testing.T) {
					journal, state := newProviderModelHistoryJournal(t, provider, "")
					usage := fixture.usage
					usage.Scope, usage.Reported, usage.Complete = "attempt", true, true
					event := modelHistoryEvent(2)
					event.Type = harnessv2.EventUpdate
					event.Update = &harnessv2.UpdateEvent{Kind: harnessv2.UpdateUsage, Usage: &usage}
					appendEvent := state.AppendUpdateIfNew
					if terminal {
						event.Type, event.Update = harnessv2.EventCompleted, nil
						event.Completed = &harnessv2.CompletedEvent{StopReason: harnessv2.ACPStopReasonEndTurn,
							Result: harnessv2.PromptResult{Usage: usage,
								Content: []harnessv2.ContentBlock{{Type: harnessv2.ContentBlockText, Text: "."}}}}
						appendEvent = state.AppendTerminalUsageIfNew
					}
					require.NoError(t, event.Validate(harnessv2.DefaultEventStreamLimits()))
					_, added, err := appendEvent(t.Context(), event)
					require.NoError(t, err)
					require.True(t, added)
					rows := modelHistoryRows(t, journal)
					require.Len(t, rows, 1)
					dto := NewExecutionEventResponse(rows[0])
					require.Equal(t, events.ExecutionEventTypeModelUsageUpdated, dto.Type)
					wantSummary := fixture.summary
					if provider == "=fixture-value" {
						wantSummary = events.ExecutionEventRedactedValue
					}
					require.Equal(t, wantSummary, dto.Summary)
					var content struct {
						harnessv2.UsageUpdate
						Scope    string `json:"usageScope"`
						Reported bool   `json:"usageReported"`
						Complete bool   `json:"usageComplete"`
					}
					require.NoError(t, json.Unmarshal(dto.Content, &content))
					require.Equal(t, usage.InputTokens, content.InputTokens)
					require.Equal(t, usage.OutputTokens, content.OutputTokens)
					require.Equal(t, usage.CachedInputTokens, content.CachedInputTokens)
					require.Equal(t, usage.CacheWriteInputTokens, content.CacheWriteInputTokens)
					require.Equal(t, usage.ContextWindowUsed, content.ContextWindowUsed)
					require.Equal(t, usage.ContextWindowSize, content.ContextWindowSize)
					require.Equal(t, usage.Scope, content.Scope)
					require.True(t, content.Reported)
					require.True(t, content.Complete)
				})
			}
		}
	}
}

func usageSummaryTestName(terminal, contextOnly bool) string {
	name := "streamed"
	if terminal {
		name = "terminal"
	}
	if contextOnly {
		return name + "/context"
	}
	return name + "/tokens"
}

func appendUsageSummaryEvent(t *testing.T, state *eventjournal.State, terminal, contextOnly bool, explicitModel string) harnessv2.Event {
	t.Helper()
	used, size := uint64(53_000), uint64(200_000)
	usage := harnessv2.UsageUpdate{InputTokens: 120, OutputTokens: 30, CachedInputTokens: new(uint64(40))}
	if contextOnly {
		usage = harnessv2.UsageUpdate{ContextWindowUsed: &used, ContextWindowSize: &size}
	}
	event := modelHistoryEvent(2)
	event.Type = harnessv2.EventUpdate
	event.Update = &harnessv2.UpdateEvent{Kind: harnessv2.UpdateUsage, Usage: &usage}
	appendEvent := state.AppendUpdateIfNew
	if terminal {
		event.Type = harnessv2.EventCompleted
		event.Update = nil
		event.Completed = &harnessv2.CompletedEvent{
			StopReason: harnessv2.ACPStopReasonEndTurn,
			Result: harnessv2.PromptResult{
				Model: explicitModel, Usage: usage,
				Content: []harnessv2.ContentBlock{{Type: harnessv2.ContentBlockText, Text: "."}},
			},
		}
		appendEvent = state.AppendTerminalUsageIfNew
	}
	if err := event.Validate(harnessv2.DefaultEventStreamLimits()); err != nil {
		t.Fatal(err)
	}
	for _, wantNew := range []bool{true, false} {
		appended, isNew, err := appendEvent(context.Background(), event)
		if err != nil || isNew != wantNew || (appended != nil) != wantNew {
			t.Fatalf("append usage: new=%t want=%t err=%v", isNew, wantNew, err)
		}
	}
	return event
}

func usageSummaryPublicDTO(t *testing.T, row store.ExecutionEvent, contextOnly, summaryRedacted bool) ExecutionEventResponse {
	t.Helper()
	encoded, err := json.Marshal(NewExecutionEventResponse(row))
	if err != nil {
		t.Fatal(err)
	}
	var dto ExecutionEventResponse
	if err := json.Unmarshal(encoded, &dto); err != nil {
		t.Fatal(err)
	}
	var content struct {
		InputTokens       *int `json:"inputTokens"`
		OutputTokens      *int `json:"outputTokens"`
		CachedInputTokens *int `json:"cachedInputTokens"`
		ContextWindowUsed *int `json:"contextWindowUsed"`
		ContextWindowSize *int `json:"contextWindowSize"`
	}
	if err := json.Unmarshal(dto.Content, &content); err != nil {
		t.Fatal(err)
	}
	if contextOnly {
		if dto.Type != events.ExecutionEventTypeModelContextUpdated || dto.Summary != "Model context updated: 53000 of 200000 tokens used" {
			t.Errorf("context type/summary changed: %q / %q", dto.Type, dto.Summary)
		}
		for _, field := range []struct {
			root, content *int
			want          int
		}{
			{dto.ContextWindowUsed, content.ContextWindowUsed, 53_000},
			{dto.ContextWindowSize, content.ContextWindowSize, 200_000},
		} {
			if field.root == nil || field.content == nil || *field.root != field.want || *field.content != field.want {
				t.Error("context occupancy changed in root/content DTO fields")
			}
		}
		if dto.InputTokens != 0 || dto.OutputTokens != 0 || dto.CachedInputTokens != 0 || content.InputTokens != nil || content.OutputTokens != nil || content.CachedInputTokens != nil {
			t.Error("context-only usage was exposed as token accounting")
		}
	} else {
		wantSummary := "Model usage updated: 120 input, 30 output, 40 cached input tokens"
		if summaryRedacted {
			wantSummary = events.ExecutionEventRedactedValue
		}
		if dto.Type != events.ExecutionEventTypeModelUsageUpdated || dto.Summary != wantSummary {
			t.Errorf("usage type/summary changed: %q / %q", dto.Type, dto.Summary)
		}
		for _, field := range []struct {
			root    int
			content *int
			want    int
		}{
			{dto.InputTokens, content.InputTokens, 120},
			{dto.OutputTokens, content.OutputTokens, 30},
			{dto.CachedInputTokens, content.CachedInputTokens, 40},
		} {
			if field.root != field.want || field.content == nil || *field.content != field.want {
				t.Error("token accounting changed in root/content DTO fields")
			}
		}
		if dto.ContextWindowUsed != nil || dto.ContextWindowSize != nil || content.ContextWindowUsed != nil || content.ContextWindowSize != nil {
			t.Error("token-only usage acquired context occupancy")
		}
	}
	return dto
}
