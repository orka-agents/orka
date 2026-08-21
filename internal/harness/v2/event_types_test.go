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
