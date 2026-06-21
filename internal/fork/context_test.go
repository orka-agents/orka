package fork

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/sozercan/orka/internal/events"
	"github.com/sozercan/orka/internal/store"
)

func TestBuildContextWithLimitsBoundsMarshaledBytes(t *testing.T) {
	input := []store.ExecutionEvent{}
	for seq := int64(1); seq <= 8; seq++ {
		input = append(input, store.ExecutionEvent{
			Seq:         seq,
			Type:        events.ExecutionEventTypeModelMessage,
			Severity:    events.ExecutionEventSeverityInfo,
			Summary:     "event summary",
			Content:     json.RawMessage(`{"payload":"` + strings.Repeat("x", 512) + `"}`),
			ContentText: strings.Repeat("y", 512),
		})
	}

	ctx := BuildContextWithLimits("default", "source-task", 8, input, 8, 768)
	encoded, err := json.Marshal(ctx)
	if err != nil {
		t.Fatalf("Marshal context: %v", err)
	}
	if len(encoded) > 768 {
		t.Fatalf("marshaled context bytes = %d, want <= 768: %s", len(encoded), encoded)
	}
	if !ctx.Truncated {
		t.Fatalf("Truncated = false, want true when payloads/events are compacted to fit byte limit")
	}
	if len(ctx.Events) == 0 || len(ctx.Events) > len(input) {
		t.Fatalf("events len = %d, want bounded non-empty subset no larger than input", len(ctx.Events))
	}
	if got := ctx.Events[len(ctx.Events)-1].Seq; got != 8 {
		t.Fatalf("last retained seq = %d, want newest seq 8", got)
	}
	for _, event := range ctx.Events {
		if len(event.Content) != 0 || event.ContentText != "" {
			t.Fatalf("event seq %d retained bulky payload: content=%q contentTextLen=%d", event.Seq, string(event.Content), len(event.ContentText))
		}
	}
}

func TestBuildContextWithLimitsCompactionMarksTruncated(t *testing.T) {
	input := []store.ExecutionEvent{{
		Seq:         1,
		Type:        events.ExecutionEventTypeModelMessage,
		Severity:    events.ExecutionEventSeverityInfo,
		Summary:     "short summary",
		Content:     json.RawMessage(`{"payload":"` + strings.Repeat("x", 2048) + `"}`),
		ContentText: strings.Repeat("y", 2048),
	}}

	ctx := BuildContextWithLimits("default", "source-task", 1, input, 10, 512)
	encoded, err := json.Marshal(ctx)
	if err != nil {
		t.Fatalf("Marshal context: %v", err)
	}
	if len(encoded) > 512 {
		t.Fatalf("marshaled context bytes = %d, want <= 512: %s", len(encoded), encoded)
	}
	if !ctx.Truncated {
		t.Fatalf("Truncated = false, want true when payload fields are dropped")
	}
	if len(ctx.Events) != 1 || ctx.Events[0].Content != nil || ctx.Events[0].ContentText != "" {
		t.Fatalf("events = %#v, want compacted event without content payloads", ctx.Events)
	}
}

func TestTruncateForkContextTextPreservesUTF8(t *testing.T) {
	got := truncateForkContextText(strings.Repeat("🙂", 8), 5)
	if !utf8.ValidString(got) {
		t.Fatalf("truncated string is not valid UTF-8: %q", got)
	}
	if !strings.HasSuffix(got, "...[truncated]") {
		t.Fatalf("truncated string = %q, want marker", got)
	}
}
