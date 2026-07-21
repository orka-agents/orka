package tasktrace

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/store"
)

const (
	StatusRunning   = "running"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
	StatusSkipped   = "skipped"
)

// TaskMetadata supplies task summary state that is not part of the event stream.
type TaskMetadata struct {
	Namespace       string
	Name            string
	Type            string
	Phase           string
	AgentName       string
	SessionName     string
	ResultAvailable bool
	ChildTasks      []ChildTaskMetadata
}

type ChildTaskMetadata struct {
	Name   string `json:"name"`
	Agent  string `json:"agent,omitempty"`
	Phase  string `json:"phase,omitempty"`
	Result string `json:"result,omitempty"`
}

type TaskTrace struct {
	Task          TaskSummary         `json:"task"`
	LatestSeq     int64               `json:"latestSeq"`
	GeneratedAt   time.Time           `json:"generatedAt"`
	Timeline      []TraceEvent        `json:"timeline"`
	ModelRequests []ModelRequestTrace `json:"modelRequests"`
	ToolCalls     []ToolCallTrace     `json:"toolCalls"`
	ChildTasks    []ChildTaskTrace    `json:"childTasks"`
	Workspace     []WorkspaceTrace    `json:"workspace"`
	Artifacts     []ArtifactTrace     `json:"artifacts"`
	Errors        []TraceIssue        `json:"errors"`
	Warnings      []TraceIssue        `json:"warnings"`
	TerminalEvent *TraceEvent         `json:"terminalEvent,omitempty"`
	RawUnpaired   []TraceEvent        `json:"rawUnpaired,omitempty"`
}

type TaskSummary struct {
	Namespace       string `json:"namespace"`
	Name            string `json:"name"`
	Type            string `json:"type,omitempty"`
	Phase           string `json:"phase,omitempty"`
	AgentName       string `json:"agentName,omitempty"`
	SessionName     string `json:"sessionName,omitempty"`
	ResultAvailable bool   `json:"resultAvailable"`
}

type TraceEvent struct {
	Seq         int64                            `json:"seq"`
	Type        string                           `json:"type"`
	Severity    string                           `json:"severity"`
	Summary     string                           `json:"summary,omitempty"`
	TaskName    string                           `json:"taskName,omitempty"`
	AgentName   string                           `json:"agentName,omitempty"`
	ToolName    string                           `json:"toolName,omitempty"`
	ToolCallID  string                           `json:"toolCallID,omitempty"`
	Content     json.RawMessage                  `json:"content,omitempty"`
	ContentText string                           `json:"contentText,omitempty"`
	Truncation  *events.ExecutionEventTruncation `json:"truncation,omitempty"`
	CreatedAt   time.Time                        `json:"createdAt"`
}

type ModelRequestTrace struct {
	ID        string     `json:"id"`
	Status    string     `json:"status"`
	StartSeq  int64      `json:"startSeq,omitempty"`
	EndSeq    int64      `json:"endSeq,omitempty"`
	StartedAt *time.Time `json:"startedAt,omitempty"`
	EndedAt   *time.Time `json:"endedAt,omitempty"`
	Summary   string     `json:"summary,omitempty"`
	Error     string     `json:"error,omitempty"`
}

type ToolCallTrace struct {
	ID        string     `json:"id"`
	Name      string     `json:"name,omitempty"`
	Status    string     `json:"status"`
	StartSeq  int64      `json:"startSeq,omitempty"`
	EndSeq    int64      `json:"endSeq,omitempty"`
	StartedAt *time.Time `json:"startedAt,omitempty"`
	EndedAt   *time.Time `json:"endedAt,omitempty"`
	Summary   string     `json:"summary,omitempty"`
	Error     string     `json:"error,omitempty"`
}

type ChildTaskTrace struct {
	Name      string     `json:"name"`
	Agent     string     `json:"agent,omitempty"`
	Status    string     `json:"status,omitempty"`
	StartSeq  int64      `json:"startSeq,omitempty"`
	EndSeq    int64      `json:"endSeq,omitempty"`
	StartedAt *time.Time `json:"startedAt,omitempty"`
	EndedAt   *time.Time `json:"endedAt,omitempty"`
	Summary   string     `json:"summary,omitempty"`
	Result    string     `json:"result,omitempty"`
}

type WorkspaceTrace struct {
	Status    string    `json:"status"`
	Seq       int64     `json:"seq"`
	Summary   string    `json:"summary,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

type ArtifactTrace struct {
	Name      string    `json:"name,omitempty"`
	Status    string    `json:"status"`
	Seq       int64     `json:"seq"`
	Summary   string    `json:"summary,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

type TraceIssue struct {
	Seq      int64  `json:"seq,omitempty"`
	Type     string `json:"type,omitempty"`
	Severity string `json:"severity,omitempty"`
	Message  string `json:"message"`
}

// MetadataFromTask builds trace metadata from a Task CR without exposing secrets.
func MetadataFromTask(task *corev1alpha1.Task) TaskMetadata {
	if task == nil {
		return TaskMetadata{}
	}
	meta := TaskMetadata{
		Namespace: task.Namespace,
		Name:      task.Name,
		Type:      string(task.Spec.Type),
		Phase:     string(task.Status.Phase),
	}
	if task.Spec.AgentRef != nil {
		meta.AgentName = task.Spec.AgentRef.Name
	}
	if task.Spec.SessionRef != nil {
		meta.SessionName = task.Spec.SessionRef.Name
	}
	if task.Status.ResultRef != nil {
		meta.ResultAvailable = task.Status.ResultRef.Available
	}
	for _, child := range task.Status.ChildTasks {
		meta.ChildTasks = append(meta.ChildTasks, ChildTaskMetadata{
			Name:   child.Name,
			Agent:  child.Agent,
			Phase:  string(child.Phase),
			Result: child.Result,
		})
	}
	return meta
}

// BuildTaskTrace creates a deterministic trace read model from task events.
//
//nolint:gocyclo // Event-to-trace mapping is intentionally centralized to keep ordering and warnings deterministic.
func BuildTaskTrace(meta TaskMetadata, input []store.ExecutionEvent, generatedAt time.Time) TaskTrace {
	eventsCopy := make([]store.ExecutionEvent, 0, len(input))
	for _, event := range input {
		eventsCopy = append(eventsCopy, cloneEvent(event))
	}
	sort.SliceStable(eventsCopy, func(i, j int) bool { return eventsCopy[i].Seq < eventsCopy[j].Seq })

	trace := TaskTrace{
		Task: TaskSummary{
			Namespace:       meta.Namespace,
			Name:            meta.Name,
			Type:            meta.Type,
			Phase:           meta.Phase,
			AgentName:       meta.AgentName,
			SessionName:     meta.SessionName,
			ResultAvailable: meta.ResultAvailable,
		},
		GeneratedAt: generatedAt.UTC(),
	}

	toolByID := map[string]int{}
	openToolByName := map[string][]string{}
	modelByID := map[string]int{}
	openModelIDs := []string{}
	childByName := map[string]int{}
	for _, child := range meta.ChildTasks {
		if child.Name == "" {
			continue
		}
		childByName[child.Name] = len(trace.ChildTasks)
		trace.ChildTasks = append(trace.ChildTasks, ChildTaskTrace{
			Name:   child.Name,
			Agent:  child.Agent,
			Status: child.Phase,
			Result: child.Result,
		})
	}

	for _, event := range eventsCopy {
		if event.Seq > trace.LatestSeq {
			trace.LatestSeq = event.Seq
		}
		te := toTraceEvent(event)
		trace.Timeline = append(trace.Timeline, te)
		if events.IsTerminalTaskEventType(event.Type) {
			copy := te
			trace.TerminalEvent = &copy
		}
		if events.NormalizeExecutionEventSeverity(event.Severity) == events.ExecutionEventSeverityError || isFailureLike(event.Type) {
			trace.Errors = append(trace.Errors, TraceIssue{Seq: event.Seq, Type: event.Type, Severity: event.Severity, Message: issueMessage(event)})
		}

		switch event.Type {
		case events.ExecutionEventTypeModelRequestStarted:
			id := modelRequestID(event)
			if id == "" {
				id = fmt.Sprintf("model-request-%d", event.Seq)
			}
			modelByID[id] = len(trace.ModelRequests)
			openModelIDs = append(openModelIDs, id)
			createdAt := event.CreatedAt
			trace.ModelRequests = append(trace.ModelRequests, ModelRequestTrace{ID: id, Status: StatusRunning, StartSeq: event.Seq, StartedAt: &createdAt, Summary: event.Summary})
		case events.ExecutionEventTypeModelRequestCompleted, events.ExecutionEventTypeModelRequestFailed:
			id := modelRequestID(event)
			idx := -1
			if id != "" {
				if found, ok := modelByID[id]; ok {
					idx = found
					openModelIDs = removeOpenTraceID(openModelIDs, id)
				}
			} else if len(openModelIDs) > 0 {
				id = openModelIDs[0]
				idx = modelByID[id]
				openModelIDs = openModelIDs[1:]
			}
			if idx < 0 || id == "" {
				id = fmt.Sprintf("model-request-%d", event.Seq)
				idx = len(trace.ModelRequests)
				trace.ModelRequests = append(trace.ModelRequests, ModelRequestTrace{ID: id})
				trace.Warnings = append(trace.Warnings, TraceIssue{Seq: event.Seq, Type: event.Type, Message: "model completion/failure event had no matching start event"})
				trace.RawUnpaired = append(trace.RawUnpaired, te)
			}
			endedAt := event.CreatedAt
			trace.ModelRequests[idx].EndSeq = event.Seq
			trace.ModelRequests[idx].EndedAt = &endedAt
			trace.ModelRequests[idx].Summary = prefer(trace.ModelRequests[idx].Summary, event.Summary)
			if event.Type == events.ExecutionEventTypeModelRequestFailed {
				trace.ModelRequests[idx].Status = StatusFailed
				trace.ModelRequests[idx].Error = issueMessage(event)
			} else {
				trace.ModelRequests[idx].Status = StatusCompleted
			}
		case events.ExecutionEventTypeToolCallSkipped:
			id := toolCallID(event)
			if id == "" {
				id = fmt.Sprintf("tool-call-%d", event.Seq)
			}
			createdAt := event.CreatedAt
			trace.ToolCalls = append(trace.ToolCalls, ToolCallTrace{
				ID: id, Name: event.ToolName, Status: StatusSkipped,
				StartSeq: event.Seq, EndSeq: event.Seq,
				StartedAt: &createdAt, EndedAt: &createdAt, Summary: event.Summary,
			})
		case events.ExecutionEventTypeToolCallStarted:
			id := toolCallID(event)
			if id == "" {
				id = fmt.Sprintf("tool-call-%d", event.Seq)
			}
			toolByID[id] = len(trace.ToolCalls)
			openToolByName[event.ToolName] = append(openToolByName[event.ToolName], id)
			createdAt := event.CreatedAt
			trace.ToolCalls = append(trace.ToolCalls, ToolCallTrace{ID: id, Name: event.ToolName, Status: StatusRunning, StartSeq: event.Seq, StartedAt: &createdAt, Summary: event.Summary})
		case events.ExecutionEventTypeToolCallCompleted, events.ExecutionEventTypeToolCallFailed:
			id := toolCallID(event)
			idx := -1
			if id != "" {
				if found, ok := toolByID[id]; ok {
					idx = found
					toolName := event.ToolName
					if toolName == "" {
						toolName = trace.ToolCalls[idx].Name
					}
					openToolByName[toolName] = removeOpenTraceID(openToolByName[toolName], id)
				}
			} else if ids := openToolByName[event.ToolName]; len(ids) > 0 {
				id = ids[0]
				idx = toolByID[id]
				openToolByName[event.ToolName] = ids[1:]
			}
			if idx < 0 || id == "" {
				id = fmt.Sprintf("tool-call-%d", event.Seq)
				idx = len(trace.ToolCalls)
				trace.ToolCalls = append(trace.ToolCalls, ToolCallTrace{ID: id, Name: event.ToolName})
				trace.Warnings = append(trace.Warnings, TraceIssue{Seq: event.Seq, Type: event.Type, Message: "tool completion/failure event had no matching start event"})
				trace.RawUnpaired = append(trace.RawUnpaired, te)
			}
			endedAt := event.CreatedAt
			trace.ToolCalls[idx].EndSeq = event.Seq
			trace.ToolCalls[idx].EndedAt = &endedAt
			trace.ToolCalls[idx].Summary = prefer(trace.ToolCalls[idx].Summary, event.Summary)
			if trace.ToolCalls[idx].Name == "" {
				trace.ToolCalls[idx].Name = event.ToolName
			}
			if event.Type == events.ExecutionEventTypeToolCallFailed {
				trace.ToolCalls[idx].Status = StatusFailed
				trace.ToolCalls[idx].Error = issueMessage(event)
			} else {
				trace.ToolCalls[idx].Status = StatusCompleted
			}
		case events.ExecutionEventTypeWorkspacePreparationStarted, events.ExecutionEventTypeWorkspacePreparationCompleted, events.ExecutionEventTypeWorkspacePreparationFailed:
			trace.Workspace = append(trace.Workspace, WorkspaceTrace{Status: workspaceStatus(event.Type), Seq: event.Seq, Summary: event.Summary, CreatedAt: event.CreatedAt})
		case events.ExecutionEventTypeArtifactUploadCompleted, events.ExecutionEventTypeArtifactUploadFailed:
			trace.Artifacts = append(trace.Artifacts, ArtifactTrace{Name: firstContentString(event.Content, "filename", "artifact", "name"), Status: artifactStatus(event.Type), Seq: event.Seq, Summary: event.Summary, CreatedAt: event.CreatedAt})
		}

		if childName := firstContentString(event.Content, "childTaskName", "child_task_name", "childTask", "taskName"); childName != "" && strings.Contains(strings.ToLower(event.Summary+" "+event.Type), "child") {
			idx, ok := childByName[childName]
			if !ok {
				idx = len(trace.ChildTasks)
				childByName[childName] = idx
				trace.ChildTasks = append(trace.ChildTasks, ChildTaskTrace{Name: childName})
			}
			child := &trace.ChildTasks[idx]
			child.Summary = prefer(child.Summary, event.Summary)
			if child.StartSeq == 0 {
				child.StartSeq = event.Seq
				createdAt := event.CreatedAt
				child.StartedAt = &createdAt
			}
			if events.IsTerminalTaskEventType(event.Type) || strings.Contains(strings.ToLower(event.Type), "completed") || strings.Contains(strings.ToLower(event.Type), "failed") {
				child.EndSeq = event.Seq
				endedAt := event.CreatedAt
				child.EndedAt = &endedAt
				child.Status = terminalStatus(event.Type)
			}
		}
	}

	for _, id := range openModelIDs {
		if idx, ok := modelByID[id]; ok && trace.ModelRequests[idx].Status == StatusRunning {
			trace.Warnings = append(trace.Warnings, TraceIssue{Seq: trace.ModelRequests[idx].StartSeq, Type: events.ExecutionEventTypeModelRequestStarted, Message: "model request start has no completion/failure event"})
		}
	}
	for _, ids := range openToolByName {
		for _, id := range ids {
			if idx, ok := toolByID[id]; ok && trace.ToolCalls[idx].Status == StatusRunning {
				trace.Warnings = append(trace.Warnings, TraceIssue{Seq: trace.ToolCalls[idx].StartSeq, Type: events.ExecutionEventTypeToolCallStarted, Message: "tool call start has no completion/failure event"})
			}
		}
	}

	return trace
}

func cloneEvent(event store.ExecutionEvent) store.ExecutionEvent {
	if event.Content != nil {
		event.Content = append(json.RawMessage(nil), event.Content...)
	}
	if event.Truncation != nil {
		truncation := *event.Truncation
		event.Truncation = &truncation
	}
	return event
}

func toTraceEvent(event store.ExecutionEvent) TraceEvent {
	return TraceEvent{Seq: event.Seq, Type: event.Type, Severity: events.NormalizeExecutionEventSeverity(event.Severity), Summary: event.Summary, TaskName: event.TaskName, AgentName: event.AgentName, ToolName: event.ToolName, ToolCallID: event.ToolCallID, Content: append(json.RawMessage(nil), event.Content...), ContentText: event.ContentText, Truncation: cloneTruncation(event.Truncation), CreatedAt: event.CreatedAt}
}

func cloneTruncation(value *events.ExecutionEventTruncation) *events.ExecutionEventTruncation {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func modelRequestID(event store.ExecutionEvent) string {
	return firstContentString(event.Content, "modelRequestID", "model_request_id", "requestID", "id")
}

func toolCallID(event store.ExecutionEvent) string {
	if strings.TrimSpace(event.ToolCallID) != "" {
		return strings.TrimSpace(event.ToolCallID)
	}
	return firstContentString(event.Content, "toolCallID", "tool_call_id", "id")
}

func firstContentString(raw json.RawMessage, keys ...string) string {
	if len(raw) == 0 {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return ""
	}
	for _, key := range keys {
		if value, ok := obj[key]; ok {
			switch v := value.(type) {
			case string:
				return strings.TrimSpace(v)
			case fmt.Stringer:
				return strings.TrimSpace(v.String())
			}
		}
	}
	return ""
}

func removeOpenTraceID(ids []string, target string) []string {
	for i, id := range ids {
		if id == target {
			return append(ids[:i], ids[i+1:]...)
		}
	}
	return ids
}

func prefer(existing, next string) string {
	if strings.TrimSpace(next) != "" {
		return next
	}
	return existing
}

func issueMessage(event store.ExecutionEvent) string {
	if strings.TrimSpace(event.Summary) != "" {
		return event.Summary
	}
	if strings.TrimSpace(event.ContentText) != "" {
		return event.ContentText
	}
	return event.Type
}
func isFailureLike(eventType string) bool {
	return strings.HasSuffix(eventType, "Failed") || strings.HasSuffix(eventType, "Cancelled") || strings.HasSuffix(eventType, "Declined") || strings.HasSuffix(eventType, "Expired")
}

func terminalStatus(eventType string) string {
	switch eventType {
	case events.ExecutionEventTypeTaskSucceeded:
		return "succeeded"
	case events.ExecutionEventTypeTaskFailed:
		return StatusFailed
	case events.ExecutionEventTypeTaskCancelled:
		return "cancelled"
	default:
		return strings.ToLower(strings.TrimPrefix(eventType, "Task"))
	}
}

func workspaceStatus(eventType string) string {
	switch eventType {
	case events.ExecutionEventTypeWorkspacePreparationStarted:
		return StatusRunning
	case events.ExecutionEventTypeWorkspacePreparationCompleted:
		return StatusCompleted
	case events.ExecutionEventTypeWorkspacePreparationFailed:
		return StatusFailed
	default:
		return "unknown"
	}
}

func artifactStatus(eventType string) string {
	if eventType == events.ExecutionEventTypeArtifactUploadCompleted {
		return "uploaded"
	}
	return StatusFailed
}
