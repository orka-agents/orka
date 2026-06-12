package tasktrace

import (
	"encoding/json"
	"testing"
	"time"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/events"
	"github.com/sozercan/orka/internal/store"
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
