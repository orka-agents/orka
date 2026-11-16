package tasktrace

import (
	"encoding/json"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/store"
)

func TestBuildTaskTraceGroupsModelToolChildAndErrors(t *testing.T) {
	now := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)
	content := func(v map[string]string) json.RawMessage {
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	trace := BuildTaskTrace(TaskMetadata{Namespace: "default", Name: "task", Type: string(corev1alpha1.TaskTypeAI), Phase: string(corev1alpha1.TaskPhaseSucceeded)}, []store.ExecutionEvent{
		{Seq: 1, Type: events.ExecutionEventTypeTaskStarted, Severity: events.ExecutionEventSeverityInfo, CreatedAt: now},
		{Seq: 2, Type: events.ExecutionEventTypeModelRequestStarted, Content: content(map[string]string{"modelRequestID": "m1"}), CreatedAt: now.Add(time.Second)},
		{Seq: 3, Type: events.ExecutionEventTypeModelRequestCompleted, Content: content(map[string]string{"modelRequestID": "m1"}), CreatedAt: now.Add(2 * time.Second)},
		{Seq: 4, Type: events.ExecutionEventTypeToolCallStarted, ToolName: "web_search", ToolCallID: "tool-1", CreatedAt: now.Add(3 * time.Second)},
		{Seq: 5, Type: events.ExecutionEventTypeToolCallFailed, Severity: events.ExecutionEventSeverityError, ToolName: "web_search", ToolCallID: "tool-1", Summary: "tool failed", CreatedAt: now.Add(4 * time.Second)},
		{Seq: 6, Type: events.ExecutionEventTypeTaskCreated, Summary: "child task created", Content: content(map[string]string{"childTaskName": "child-a"}), CreatedAt: now.Add(5 * time.Second)},
		{Seq: 7, Type: events.ExecutionEventTypeTaskSucceeded, CreatedAt: now.Add(6 * time.Second)},
	}, now)
	if trace.LatestSeq != 7 || trace.TerminalEvent == nil || trace.TerminalEvent.Seq != 7 {
		t.Fatalf("trace terminal/latest = %#v", trace)
	}
	if len(trace.ModelRequests) != 1 || trace.ModelRequests[0].Status != StatusCompleted {
		t.Fatalf("model requests = %#v", trace.ModelRequests)
	}
	if len(trace.ToolCalls) != 1 || trace.ToolCalls[0].Status != StatusFailed || trace.ToolCalls[0].Error != "tool failed" {
		t.Fatalf("tool calls = %#v", trace.ToolCalls)
	}
	if len(trace.ChildTasks) != 1 || trace.ChildTasks[0].Name != "child-a" {
		t.Fatalf("child tasks = %#v", trace.ChildTasks)
	}
	if len(trace.Errors) == 0 {
		t.Fatalf("expected error issue for failed tool")
	}
}

func TestBuildTaskTraceUnmatchedCompletionWarns(t *testing.T) {
	trace := BuildTaskTrace(TaskMetadata{Name: "task"}, []store.ExecutionEvent{{Seq: 1, Type: events.ExecutionEventTypeToolCallCompleted}}, time.Now())
	if len(trace.Warnings) != 1 || len(trace.RawUnpaired) != 1 {
		t.Fatalf("warnings=%#v raw=%#v, want unmatched warning", trace.Warnings, trace.RawUnpaired)
	}
}

func TestBuildTaskTraceExplicitToolCompletionDequeuesOpenCall(t *testing.T) {
	trace := BuildTaskTrace(TaskMetadata{Name: "task"}, []store.ExecutionEvent{
		{Seq: 1, Type: events.ExecutionEventTypeToolCallStarted, ToolName: "web_search", ToolCallID: "tool-1"},
		{Seq: 2, Type: events.ExecutionEventTypeToolCallStarted, ToolName: "web_search", ToolCallID: "tool-2"},
		{Seq: 3, Type: events.ExecutionEventTypeToolCallCompleted, ToolName: "web_search", ToolCallID: "tool-1"},
		{Seq: 4, Type: events.ExecutionEventTypeToolCallCompleted, ToolName: "web_search"},
	}, time.Now())
	if len(trace.ToolCalls) != 2 || trace.ToolCalls[0].EndSeq != 3 || trace.ToolCalls[1].EndSeq != 4 {
		t.Fatalf("tool calls = %#v, want explicit completion removed from open queue", trace.ToolCalls)
	}
	if len(trace.Warnings) != 0 {
		t.Fatalf("warnings = %#v, want none", trace.Warnings)
	}
}

func TestBuildTaskTraceCoalescesRepeatedToolStartUpdates(t *testing.T) {
	trace := BuildTaskTrace(TaskMetadata{Name: "task"}, []store.ExecutionEvent{
		{Seq: 1, Type: events.ExecutionEventTypeToolCallStarted, ToolName: "tool", ToolCallID: "tool-1", Summary: "pending"},
		{Seq: 2, Type: events.ExecutionEventTypeToolCallStarted, ToolName: "file_read", ToolCallID: "tool-1", Summary: "in progress"},
		{Seq: 3, Type: events.ExecutionEventTypeToolCallCompleted, ToolCallID: "tool-1"},
	}, time.Now())
	if len(trace.ToolCalls) != 1 {
		t.Fatalf("tool calls = %#v, want one coalesced lifecycle", trace.ToolCalls)
	}
	tool := trace.ToolCalls[0]
	if tool.StartSeq != 1 || tool.EndSeq != 3 || tool.Status != StatusCompleted || tool.Name != "file_read" || tool.Summary != "in progress" {
		t.Fatalf("coalesced tool call = %#v", tool)
	}
	if len(trace.Warnings) != 0 {
		t.Fatalf("warnings = %#v, want none", trace.Warnings)
	}
}

func TestBuildTaskTraceExplicitModelCompletionDequeuesOpenRequest(t *testing.T) {
	content := func(id string) json.RawMessage {
		data, _ := json.Marshal(map[string]string{"modelRequestID": id})
		return data
	}
	trace := BuildTaskTrace(TaskMetadata{Name: "task"}, []store.ExecutionEvent{
		{Seq: 1, Type: events.ExecutionEventTypeModelRequestStarted, Content: content("model-1")},
		{Seq: 2, Type: events.ExecutionEventTypeModelRequestStarted, Content: content("model-2")},
		{Seq: 3, Type: events.ExecutionEventTypeModelRequestCompleted, Content: content("model-1")},
		{Seq: 4, Type: events.ExecutionEventTypeModelRequestCompleted},
	}, time.Now())
	if len(trace.ModelRequests) != 2 || trace.ModelRequests[0].EndSeq != 3 || trace.ModelRequests[1].EndSeq != 4 {
		t.Fatalf("model requests = %#v, want explicit completion removed from open queue", trace.ModelRequests)
	}
	if len(trace.Warnings) != 0 {
		t.Fatalf("warnings = %#v, want none", trace.Warnings)
	}
}

func TestBuildTaskTraceRecordsModelAndProvider(t *testing.T) {
	content := func(v map[string]string) json.RawMessage {
		data, _ := json.Marshal(v)
		return data
	}
	trace := BuildTaskTrace(TaskMetadata{Name: "task"}, []store.ExecutionEvent{
		{Seq: 1, Type: events.ExecutionEventTypeModelRequestStarted, Content: content(map[string]string{"modelRequestID": "m1", "model": "gpt-4.1", "provider": "openai"})},
		{Seq: 2, Type: events.ExecutionEventTypeModelRequestCompleted, Content: content(map[string]string{"modelRequestID": "m1", "model": "gpt-4.1-2025-04-14"})},
		{Seq: 3, Type: events.ExecutionEventTypeModelRequestStarted, Content: content(map[string]string{"modelRequestID": "m2", "model": "claude-haiku-4-5", "provider": "anthropic"})},
		{Seq: 4, Type: events.ExecutionEventTypeModelRequestFailed, Content: content(map[string]string{"modelRequestID": "m2"})},
		{Seq: 5, Type: events.ExecutionEventTypeModelRequestCompleted, Content: content(map[string]string{"model": "gemini-2.5-flash", "provider": "gemini"})},
	}, time.Now())
	if len(trace.ModelRequests) != 3 {
		t.Fatalf("model requests = %#v, want 3", trace.ModelRequests)
	}
	want := []struct{ model, provider string }{
		{"gpt-4.1-2025-04-14", "openai"},
		{"claude-haiku-4-5", "anthropic"},
		{"gemini-2.5-flash", "gemini"},
	}
	for i, w := range want {
		if got := trace.ModelRequests[i]; got.Model != w.model || got.Provider != w.provider {
			t.Fatalf("model request %d = %#v, want model %q provider %q", i, got, w.model, w.provider)
		}
	}
}

func TestBuildTaskTraceReadsGenAIModelAliases(t *testing.T) {
	content := func(v map[string]string) json.RawMessage {
		data, _ := json.Marshal(v)
		return data
	}
	trace := BuildTaskTrace(TaskMetadata{Name: "task"}, []store.ExecutionEvent{
		{Seq: 1, Type: events.ExecutionEventTypeModelRequestStarted, Content: content(map[string]string{
			"modelRequestID": "m1", "gen_ai.request.model": "gpt-4.1", "gen_ai.response.model": "ignored-on-start", "gen_ai.provider.name": "openai",
		})},
		{Seq: 2, Type: events.ExecutionEventTypeModelRequestCompleted, Content: content(map[string]string{
			"modelRequestID": "m1", "gen_ai.request.model": "gpt-4.1", "gen_ai.response.model": "gpt-4.1-2025-04-14",
		})},
		{Seq: 3, Type: events.ExecutionEventTypeModelRequestStarted, Content: content(map[string]string{
			"modelRequestID": "m2", "gen_ai.request.model": "claude-haiku-4-5", "gen_ai.response.model": "ignored-on-start",
		})},
	}, time.Now())
	if len(trace.ModelRequests) != 2 {
		t.Fatalf("model requests = %#v, want 2", trace.ModelRequests)
	}
	if got := trace.ModelRequests[0]; got.Model != "gpt-4.1-2025-04-14" || got.Provider != "openai" {
		t.Fatalf("completed model request = %#v, want response model and semconv provider", got)
	}
	if got := trace.ModelRequests[1]; got.Model != "claude-haiku-4-5" {
		t.Fatalf("running model request = %#v, want request model", got)
	}
}
