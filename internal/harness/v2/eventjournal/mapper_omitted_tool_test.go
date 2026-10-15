package eventjournal

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	executionevents "github.com/orka-agents/orka/internal/events"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

func TestMapOmittedToolOutputUsesOnlyPublishedCopies(t *testing.T) {
	for _, test := range []struct {
		name, title, output, summary string
		maskLater                    bool
	}{
		{name: "titled hidden marker", title: "Inspect", output: "pwd", summary: "Inspect"},
		{name: "titled hidden assignment", title: "pwd", output: "=fixture-value", summary: "pwd", maskLater: true},
		{name: "untitled one summary copy", output: "ken=X\tto", summary: "ken=X to"},
		{name: "untitled hidden tail", output: strings.Repeat("界", 4096) + "pwd", summary: strings.Repeat("界", 4095) + "…"},
		{name: "untitled visible summary", output: "pwd\u00a0", summary: "pwd", maskLater: true},
	} {
		for _, upstream := range []bool{false, true} {
			mode := "truncated"
			if upstream {
				mode = "upstream omitted"
			}
			t.Run(test.name+"/"+mode, func(t *testing.T) {
				event := testUpdateEvent(1, time.Now().UTC(), harnessv2.UpdateEvent{
					Kind: harnessv2.UpdateToolCallUpdate,
					ToolCall: &harnessv2.ToolCallUpdate{
						ToolCallID: "omitted", Title: test.title, Kind: "shell",
						Status: harnessv2.ToolCallStatusCompleted, ContentOmitted: upstream,
					},
				})
				mapped, fields, err := mapToolUpdateWithHistory(event, testMapContext(), &test.output, !upstream, false, "", nil, false)
				if err != nil {
					t.Fatal(err)
				}
				direct, err := mapUpdate(event, testMapContext(), mapUpdateOptions{toolContentText: &test.output, toolContentTruncated: !upstream})
				if err != nil {
					t.Fatal(err)
				}
				for _, got := range []string{mapped.Summary, direct.Summary} {
					if got != test.summary {
						t.Errorf("public summary differs from retained copy: got %q, want %q", got, test.summary)
					}
				}
				var content struct{ Title, ToolKind, ContentOmitted string }
				if err := json.Unmarshal(mapped.Content, &content); err != nil {
					t.Fatal(err)
				}
				if content.Title != test.title || content.ToolKind != "shell" || mapped.ToolName != "shell" {
					t.Error("unpublished output changed public title/kind metadata")
				}
				if mapped.ContentText != "" || direct.ContentText != "" || mapped.Truncation == nil ||
					!mapped.Truncation.ContentTextTruncated || content.ContentOmitted != streamedTextTruncatedOrOmittedReason {
					t.Error("omitted content or truncation metadata changed")
				}
				later := "=fixture-value"
				event.Update.ToolCall.Title = "Inspect"
				event.Update.ToolCall.ContentOmitted = false
				next, _, err := mapToolUpdateWithHistory(event, testMapContext(), &later, false, false, "", fields, false)
				if err != nil {
					t.Fatal(err)
				}
				want := later
				if test.maskLater {
					want = executionevents.ExecutionEventRedactedValue
				}
				if next.ContentText != want {
					t.Errorf("later output = %q, want %q from actual published history", next.ContentText, want)
				}
			})
		}
	}
}

func TestMapToolMultipleBlocksOmittedKeepsPublishedOutputHistory(t *testing.T) {
	output := "pwd"
	event := testUpdateEvent(1, time.Now().UTC(), harnessv2.UpdateEvent{
		Kind: harnessv2.UpdateToolCallUpdate, ToolCall: &harnessv2.ToolCallUpdate{
			ToolCallID: "multiple", Title: "Inspect", Kind: "shell", Status: harnessv2.ToolCallStatusCompleted,
		},
	})
	mapped, fields, err := mapToolUpdateWithHistory(event, testMapContext(), &output, false, true, "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if mapped.ContentText != output || mapped.Summary != "Inspect" || mapped.Truncation != nil ||
		!strings.Contains(string(mapped.Content), toolContentMultipleBlocksOmittedReason) {
		t.Fatal("multiple-block omission lost the retained output")
	}
	later := "=fixture-value"
	mapped, _, err = mapToolUpdateWithHistory(event, testMapContext(), &later, false, false, "", fields, false)
	if err != nil || mapped.ContentText != executionevents.ExecutionEventRedactedValue {
		t.Fatalf("retained output must protect later assignments: err=%v", err)
	}
}
