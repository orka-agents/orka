package fork

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/sozercan/orka/internal/store"
)

const (
	DefaultMaxEvents       = 200
	DefaultMaxContextBytes = 256 * 1024
)

type Context struct {
	SourceNamespace string         `json:"sourceNamespace"`
	SourceTask      string         `json:"sourceTask,omitempty"`
	SourceSession   string         `json:"sourceSession,omitempty"`
	AfterSeq        int64          `json:"afterSeq"`
	Events          []EventSummary `json:"events"`
	Truncated       bool           `json:"truncated"`
}

type EventSummary struct {
	Seq         int64           `json:"seq"`
	Type        string          `json:"type"`
	Severity    string          `json:"severity"`
	Summary     string          `json:"summary,omitempty"`
	TaskName    string          `json:"taskName,omitempty"`
	TaskSeq     int64           `json:"taskSeq,omitempty"`
	ToolName    string          `json:"toolName,omitempty"`
	ToolCallID  string          `json:"toolCallID,omitempty"`
	Content     json.RawMessage `json:"content,omitempty"`
	ContentText string          `json:"contentText,omitempty"`
}

// BuildContext returns a bounded, already-sanitized summary of events up to afterSeq.
func BuildContext(namespace, taskName string, afterSeq int64, events []store.ExecutionEvent, maxEvents int) Context {
	return BuildContextWithLimits(namespace, taskName, afterSeq, events, maxEvents, DefaultMaxContextBytes)
}

// BuildContextWithLimits returns a bounded, already-sanitized summary of events up to afterSeq.
// Events are selected newest-first, then returned in chronological order. The byte limit bounds
// the marshaled Context payload that may be embedded into a forked Task prompt.
func BuildContextWithLimits(
	namespace,
	taskName string,
	afterSeq int64,
	events []store.ExecutionEvent,
	maxEvents int,
	maxBytes int,
) Context {
	if maxEvents <= 0 {
		maxEvents = DefaultMaxEvents
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxContextBytes
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
	for i := len(ordered) - 1; i >= 0; i-- {
		summary := eventSummaryFromStore(ordered[i])
		kept, compacted := prependEventWithinLimit(&ctx, summary, maxBytes)
		if !kept {
			truncated = true
			continue
		}
		if compacted {
			truncated = true
		}
	}
	ctx.Truncated = truncated
	return ctx
}

// BuildSessionContext returns a bounded, already-sanitized summary of session
// events up to a session-level checkpoint sequence.
func BuildSessionContext(namespace, sessionName string, afterSeq int64, events []store.SessionExecutionEvent, maxEvents int) Context {
	return BuildSessionContextWithLimits(namespace, sessionName, afterSeq, events, maxEvents, DefaultMaxContextBytes)
}

func BuildSessionContextWithLimits(
	namespace,
	sessionName string,
	afterSeq int64,
	events []store.SessionExecutionEvent,
	maxEvents int,
	maxBytes int,
) Context {
	if maxEvents <= 0 {
		maxEvents = DefaultMaxEvents
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxContextBytes
	}
	ordered := make([]store.SessionExecutionEvent, 0, len(events))
	for _, event := range events {
		if event.SessionSeq <= afterSeq {
			ordered = append(ordered, event)
		}
	}
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].SessionSeq < ordered[j].SessionSeq })
	truncated := false
	if len(ordered) > maxEvents {
		truncated = true
		ordered = ordered[len(ordered)-maxEvents:]
	}
	ctx := Context{SourceNamespace: namespace, SourceSession: sessionName, AfterSeq: afterSeq, Truncated: truncated}
	for i := len(ordered) - 1; i >= 0; i-- {
		kept, compacted := prependEventWithinLimit(&ctx, eventSummaryFromSession(ordered[i]), maxBytes)
		if !kept || compacted {
			truncated = true
		}
	}
	ctx.Truncated = truncated
	return ctx
}

func prependEventWithinLimit(ctx *Context, summary EventSummary, maxBytes int) (kept bool, compacted bool) {
	candidate := append([]EventSummary{summary}, ctx.Events...)
	ctx.Events = candidate
	if marshaledContextLen(*ctx) <= maxBytes {
		return true, false
	}
	compact := summary
	compact.Content = nil
	compact.ContentText = ""
	candidate[0] = compact
	ctx.Events = candidate
	if marshaledContextLen(*ctx) <= maxBytes {
		return true, true
	}
	compact.Summary = truncateForkContextText(compact.Summary, 1024)
	candidate[0] = compact
	ctx.Events = candidate
	if marshaledContextLen(*ctx) <= maxBytes {
		return true, true
	}
	ctx.Events = candidate[1:]
	return false, true
}

func eventSummaryFromStore(event store.ExecutionEvent) EventSummary {
	return EventSummary{
		Seq:         event.Seq,
		Type:        event.Type,
		Severity:    event.Severity,
		Summary:     event.Summary,
		ToolName:    event.ToolName,
		ToolCallID:  event.ToolCallID,
		Content:     cloneRaw(event.Content),
		ContentText: event.ContentText,
	}
}

func eventSummaryFromSession(event store.SessionExecutionEvent) EventSummary {
	return EventSummary{
		Seq:         event.SessionSeq,
		Type:        event.Type,
		Severity:    event.Severity,
		Summary:     event.Summary,
		TaskName:    event.TaskName,
		TaskSeq:     event.TaskSeq,
		ToolName:    event.ToolName,
		ToolCallID:  event.ToolCallID,
		Content:     cloneRaw(event.Content),
		ContentText: event.ContentText,
	}
}

func marshaledContextLen(ctx Context) int {
	encoded, err := json.Marshal(ctx)
	if err != nil {
		return maxInt
	}
	return len(encoded)
}

const maxInt = int(^uint(0) >> 1)

func truncateForkContextText(value string, maxChars int) string {
	if maxChars <= 0 {
		return ""
	}
	runes := []rune(strings.TrimSpace(value))
	if len(runes) <= maxChars {
		return string(runes)
	}
	marker := "...[truncated]"
	markerRunes := []rune(marker)
	if maxChars <= len(markerRunes) {
		return string(markerRunes[:maxChars])
	}
	return string(runes[:maxChars-len(markerRunes)]) + marker
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
