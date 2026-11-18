package v2

import "testing"

const (
	eventTypesTestToolCallID = "call-1"
	eventTypesTestResultText = "done"
)

func TestToolCallUpdateValidateRequiresOmittedContentToBeEmpty(t *testing.T) {
	for _, update := range []ToolCallUpdate{
		{
			ToolCallID: eventTypesTestToolCallID, Status: ToolCallStatusCompleted, ContentOmitted: true,
			Content: []ContentBlock{{Type: ContentBlockText, Text: "partial"}},
		},
		{
			ToolCallID: eventTypesTestToolCallID, Status: ToolCallStatusCompleted, ContentOmitted: true, ContentReplace: true,
		},
	} {
		if err := update.Validate(); err == nil {
			t.Fatalf("ToolCallUpdate.Validate(%#v) error = nil, want invalid omitted content rejection", update)
		}
	}
	if err := (ToolCallUpdate{
		ToolCallID: eventTypesTestToolCallID, Status: ToolCallStatusCompleted, ContentOmitted: true,
	}).Validate(); err != nil {
		t.Fatalf("validate omitted tool content: %v", err)
	}
}

func TestUsageUpdateValidateAllowsEmptySnapshot(t *testing.T) {
	event := UpdateEvent{Kind: UpdateUsage, Usage: &UsageUpdate{}}
	if err := event.Validate(); err != nil {
		t.Fatalf("validate zero usage snapshot: %v", err)
	}
}

func TestUsageUpdateValidateCacheCountsWithinInput(t *testing.T) {
	for _, test := range []struct {
		name    string
		usage   UsageUpdate
		invalid bool
	}{
		{name: "unavailable cache", usage: UsageUpdate{Reported: true, Complete: true}},
		{name: "reported zero cache", usage: UsageUpdate{
			CachedInputTokens: new(uint64(0)), CacheWriteInputTokens: new(uint64(0)), Reported: true, Complete: true,
		}},
		{name: "cache below input", usage: UsageUpdate{
			InputTokens: 100, CachedInputTokens: new(uint64(60)), CacheWriteInputTokens: new(uint64(10)), Reported: true, Complete: true,
		}},
		{name: "cache equals input", usage: UsageUpdate{
			InputTokens: 100, CachedInputTokens: new(uint64(60)), CacheWriteInputTokens: new(uint64(40)), Reported: true, Complete: true,
		}},
		{name: "maximum inclusive input", usage: UsageUpdate{
			InputTokens: ^uint64(0), CachedInputTokens: new(^uint64(0)), CacheWriteInputTokens: new(uint64(0)),
		}},
		{name: "cache read exceeds zero input", usage: UsageUpdate{
			CachedInputTokens: new(uint64(100)), Reported: true, Complete: true,
		}, invalid: true},
		{name: "cache write exceeds input", usage: UsageUpdate{
			InputTokens: 50, CacheWriteInputTokens: new(uint64(100)), Reported: true, Complete: true,
		}, invalid: true},
		{name: "combined cache exceeds input", usage: UsageUpdate{
			InputTokens: 100, CachedInputTokens: new(uint64(60)), CacheWriteInputTokens: new(uint64(50)), Reported: true, Complete: true,
		}, invalid: true},
		{name: "combined cache overflows", usage: UsageUpdate{
			InputTokens: ^uint64(0), CachedInputTokens: new(^uint64(0)), CacheWriteInputTokens: new(uint64(1)),
		}, invalid: true},
		{name: "legacy cache exceeds input", usage: UsageUpdate{
			InputTokens: 10, CachedInputTokens: new(uint64(11)),
		}, invalid: true},
		{name: "partial cache exceeds input", usage: UsageUpdate{
			InputTokens: 10, CacheWriteInputTokens: new(uint64(11)), Reported: true,
		}, invalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			update := UpdateEvent{Kind: UpdateUsage, Usage: &test.usage}
			result := PromptResult{
				Content: []ContentBlock{{Type: ContentBlockText, Text: eventTypesTestResultText}},
				Usage:   test.usage,
			}
			for name, err := range map[string]error{"update": update.Validate(), "result": result.Validate()} {
				if (err != nil) != test.invalid {
					t.Errorf("%s validation error = %v, want invalid=%t", name, err, test.invalid)
				}
			}
		})
	}
}

func TestPromptResultValidateRejectsInvalidContextWindowUsage(t *testing.T) {
	used, smallerSize, zero := uint64(2), uint64(1), uint64(0)
	tests := []struct {
		name  string
		usage UsageUpdate
	}{
		{name: "missing size", usage: UsageUpdate{ContextWindowUsed: &used}},
		{name: "zero size", usage: UsageUpdate{ContextWindowUsed: &zero, ContextWindowSize: &zero}},
		{name: "used exceeds size", usage: UsageUpdate{ContextWindowUsed: &used, ContextWindowSize: &smallerSize}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := PromptResult{
				Content: []ContentBlock{{Type: ContentBlockText, Text: eventTypesTestResultText}},
				Usage:   test.usage,
			}
			if err := result.Validate(); err == nil {
				t.Fatal("PromptResult.Validate() error = nil, want invalid usage rejection")
			}
		})
	}
}
