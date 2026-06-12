package fork

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/sozercan/orka/internal/store"
)

const DefaultMaxEvents = 200

type Context struct {
	SourceNamespace string         `json:"sourceNamespace"`
	SourceTask      string         `json:"sourceTask"`
	AfterSeq        int64          `json:"afterSeq"`
	Events          []EventSummary `json:"events"`
	Truncated       bool           `json:"truncated"`
}

type EventSummary struct {
	Seq         int64           `json:"seq"`
	Type        string          `json:"type"`
	Severity    string          `json:"severity"`
	Summary     string          `json:"summary,omitempty"`
	ToolName    string          `json:"toolName,omitempty"`
	ToolCallID  string          `json:"toolCallID,omitempty"`
	Content     json.RawMessage `json:"content,omitempty"`
	ContentText string          `json:"contentText,omitempty"`
}

// BuildContext returns a bounded, already-sanitized summary of events up to afterSeq.
func BuildContext(namespace, taskName string, afterSeq int64, events []store.ExecutionEvent, maxEvents int) Context {
	if maxEvents <= 0 {
		maxEvents = DefaultMaxEvents
	}
	ordered := make([]store.ExecutionEvent, 0, len(events))
	for _, event := range events {
		if event.Seq <= afterSeq {
			ordered = append(ordered, event)
		}
	}
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Seq < ordered[j].Seq })
	truncated := false
	if len(ordered) > maxEvents {
		truncated = true
		ordered = ordered[len(ordered)-maxEvents:]
	}
	ctx := Context{SourceNamespace: namespace, SourceTask: taskName, AfterSeq: afterSeq, Truncated: truncated}
	for _, event := range ordered {
		ctx.Events = append(ctx.Events, EventSummary{
			Seq:         event.Seq,
			Type:        event.Type,
			Severity:    event.Severity,
			Summary:     event.Summary,
			ToolName:    event.ToolName,
			ToolCallID:  event.ToolCallID,
			Content:     cloneRaw(event.Content),
			ContentText: event.ContentText,
		})
	}
	return ctx
}

func ValidateAfterSeq(afterSeq int64, events []store.ExecutionEvent) bool {
	if afterSeq == 0 {
		return true
	}
	for _, event := range events {
		if event.Seq == afterSeq {
			return true
		}
	}
	return false
}

func LatestSeq(events []store.ExecutionEvent) int64 {
	var latest int64
	for _, event := range events {
		if event.Seq > latest {
			latest = event.Seq
		}
	}
	return latest
}

func cloneRaw(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}

func SanitizeTaskNamePrefix(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "task"
	}
	var b strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "task"
	}
	if len(out) > 42 {
		out = strings.Trim(out[:42], "-")
	}
	return out
}
