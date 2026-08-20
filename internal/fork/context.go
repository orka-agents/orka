package fork

import (
	"encoding/json"
	"sort"
	"strings"
	"unicode/utf8"

	executionevents "github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/store"
)

const (
	DefaultMaxEvents       = 200
	DefaultMaxContextBytes = 256 * 1024
)

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

// CoalesceAdjacentModelMessages joins streamed harness v2 chunks from the same
// prompt before fork retention is applied. Stored events remain immutable; the
// returned slice is a fork-context read model whose sequence/content identity
// points at the newest included chunk.
func CoalesceAdjacentModelMessages(values []store.ExecutionEvent) []store.ExecutionEvent {
	coalesced := make([]store.ExecutionEvent, 0, len(values))
	for _, event := range values {
		if len(coalesced) > 0 && SameHarnessV2ModelMessage(coalesced[len(coalesced)-1], event) {
			previous := &coalesced[len(coalesced)-1]
			combinedOriginalChars := executionEventContentTextChars(*previous) + executionEventContentTextChars(event)
			previous.Seq = event.Seq
			previous.CreatedAt = event.CreatedAt
			previous.Content = cloneRaw(event.Content)
			previous.Truncation = store.MergeExecutionEventTruncation(previous.Truncation, event.Truncation)
			if previous.Truncation != nil && previous.Truncation.ContentTextTruncated {
				previous.ContentText = ""
				previous.Truncation = store.MergeExecutionEventTruncation(previous.Truncation, &executionevents.ExecutionEventTruncation{
					ContentTextTruncated:     true,
					ContentTextOriginalChars: combinedOriginalChars,
				})
				continue
			}
			contentText, truncated, originalChars := executionevents.RedactAndTruncateExecutionEventText(
				previous.ContentText+event.ContentText,
				executionevents.MaxExecutionEventContentTextChars,
			)
			if truncated {
				previous.ContentText = ""
				previous.Truncation = store.MergeExecutionEventTruncation(previous.Truncation, &executionevents.ExecutionEventTruncation{
					ContentTextTruncated:     true,
					ContentTextOriginalChars: originalChars,
				})
			} else {
				previous.ContentText = contentText
			}
			continue
		}
		coalesced = append(coalesced, event)
	}
	return coalesced
}

func executionEventContentTextChars(event store.ExecutionEvent) int {
	if event.Truncation != nil && event.Truncation.ContentTextTruncated && event.Truncation.ContentTextOriginalChars > 0 {
		return event.Truncation.ContentTextOriginalChars
	}
	return utf8.RuneCountInString(event.ContentText)
}

// SameHarnessV2ModelMessage reports whether two model-message events belong to
// the same harness v2 prompt and can therefore be adjacent stream chunks.
func SameHarnessV2ModelMessage(left, right store.ExecutionEvent) bool {
	if left.Type != executionevents.ExecutionEventTypeModelMessage || right.Type != executionevents.ExecutionEventTypeModelMessage {
		return false
	}
	leftIdentity, leftOK := harnessV2PromptIdentity(left.Content)
	rightIdentity, rightOK := harnessV2PromptIdentity(right.Content)
	return leftOK && rightOK && leftIdentity == rightIdentity
}

type promptIdentity struct {
	TaskUID     string
	TaskAttempt uint32
	PromptID    string
}

func harnessV2PromptIdentity(content json.RawMessage) (promptIdentity, bool) {
	var envelope struct {
		HarnessV2 struct {
			TaskUID     string `json:"taskUID"`
			TaskAttempt uint32 `json:"taskAttempt"`
			PromptID    string `json:"promptID"`
		} `json:"harnessV2"`
	}
	if len(content) == 0 || json.Unmarshal(content, &envelope) != nil {
		return promptIdentity{}, false
	}
	identity := promptIdentity{
		TaskUID: envelope.HarnessV2.TaskUID, TaskAttempt: envelope.HarnessV2.TaskAttempt, PromptID: envelope.HarnessV2.PromptID,
	}
	return identity, identity.TaskUID != "" && identity.TaskAttempt > 0 && identity.PromptID != ""
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

func marshaledContextLen(ctx Context) int {
	data, err := json.Marshal(ctx)
	if err != nil {
		return maxInt
	}
	return len(data)
}

const maxInt = int(^uint(0) >> 1)

func truncateForkContextText(value string, maxChars int) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if maxChars <= 0 || len(runes) <= maxChars {
		return value
	}
	return string(runes[:maxChars]) + "...[truncated]"
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
