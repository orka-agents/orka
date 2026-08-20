package v2

import "testing"

func TestUsageUpdateValidateAllowsZeroSnapshot(t *testing.T) {
	event := UpdateEvent{Kind: UpdateUsage, Usage: &UsageUpdate{}}
	if err := event.Validate(); err != nil {
		t.Fatalf("validate zero usage snapshot: %v", err)
	}
}

func TestPromptResultValidateRejectsInvalidContextWindowUsage(t *testing.T) {
	used, smallerSize := uint64(2), uint64(1)
	tests := []struct {
		name  string
		usage UsageUpdate
	}{
		{name: "missing size", usage: UsageUpdate{ContextWindowUsed: &used}},
		{name: "used exceeds size", usage: UsageUpdate{ContextWindowUsed: &used, ContextWindowSize: &smallerSize}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := PromptResult{
				Content: []ContentBlock{{Type: ContentBlockText, Text: "done"}},
				Usage:   test.usage,
			}
			if err := result.Validate(); err == nil {
				t.Fatal("PromptResult.Validate() error = nil, want invalid usage rejection")
			}
		})
	}
}
