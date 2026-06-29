package harness_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sozercan/orka/internal/events"
	"github.com/sozercan/orka/internal/harness"
	"github.com/sozercan/orka/internal/store"
	"github.com/sozercan/orka/internal/tools"
)

const brokeredApprovalRequiredCode = "approval_required"

func TestBrokeredToolRegistryToolSucceedsAndEmitsEvents(t *testing.T) {
	eventStore := store.NewFakeExecutionEventStore()
	registry := tools.NewRegistry()
	registry.Register(staticHarnessTool{name: "echo"})
	broker := harness.ToolBroker{
		Registry:              registry,
		EventStore:            eventStore,
		ToolContext:           &tools.ToolContext{Namespace: "default", TaskID: "task-a", SessionID: "session-a"},
		AllowedTools:          map[string]struct{}{"echo": {}},
		AllowInsecureLoopback: true,
		Now:                   func() time.Time { return time.Date(2026, 6, 13, 10, 0, 0, 0, time.UTC) },
	}
	req := brokeredToolRequest("call-a", json.RawMessage(`{"message":"hello from broker"}`))
	result, err := broker.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.Error != nil || !result.Approved || !strings.Contains(string(result.Output), "hello from broker") {
		t.Fatalf("result = %#v output=%s, want approved echo content", result, result.Output)
	}
	listed, err := eventStore.ListExecutionEvents(context.Background(), store.ExecutionEventFilter{
		Namespace: "default", StreamID: "task-a", Limit: 10,
	})
	if err != nil {
		t.Fatalf("ListExecutionEvents: %v", err)
	}
	if !hasHarnessBrokerEvent(listed, events.ExecutionEventTypeToolCallStarted) || !hasHarnessBrokerEvent(listed, events.ExecutionEventTypeToolCallCompleted) {
		t.Fatalf("events = %#v, want tool started/completed", listed)
	}
	restarted := harness.ToolBroker{
		Registry:     registry,
		EventStore:   eventStore,
		ToolContext:  &tools.ToolContext{Namespace: "default", TaskID: "task-a", SessionID: "session-a"},
		AllowedTools: map[string]struct{}{"echo": {}},
	}
	afterRestart, err := restarted.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("restart Execute() error = %v", err)
	}
	if afterRestart.Error == nil || afterRestart.Error.Code != "idempotency_already_processed" {
		t.Fatalf("restart result = %#v, want already processed conflict", afterRestart)
	}
}

func TestBrokeredToolDuplicateIsIdempotentAndConflictIsRejected(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(staticHarnessTool{name: "echo"})
	broker := harness.ToolBroker{
		Registry:     registry,
		EventStore:   store.NewFakeExecutionEventStore(),
		ToolContext:  &tools.ToolContext{Namespace: "default", TaskID: "task-a"},
		AllowedTools: map[string]struct{}{"echo": {}},
	}
	req := brokeredToolRequest("call-a", json.RawMessage(`{"message":"one"}`))
	first, err := broker.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("first Execute() error = %v", err)
	}
	second, err := broker.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("second Execute() error = %v", err)
	}
	if string(first.Output) != string(second.Output) {
		t.Fatalf("duplicate output = %s, want %s", second.Output, first.Output)
	}
	conflictReq := req
	conflictReq.Input = json.RawMessage(`{"message":"two"}`)
	conflict, err := broker.Execute(context.Background(), conflictReq)
	if err != nil {
		t.Fatalf("conflict Execute() error = %v", err)
	}
	if conflict.Error == nil || conflict.Error.Code != "idempotency_conflict" {
		t.Fatalf("conflict result = %#v, want idempotency conflict", conflict)
	}
}

func TestBrokeredToolConcurrentDuplicateExecutesOnce(t *testing.T) {
	eventStore := store.NewFakeExecutionEventStore()
	var executions atomic.Int32
	registry := tools.NewRegistry()
	registry.Register(blockingHarnessTool{name: "slow", executions: &executions})
	broker := harness.ToolBroker{
		Registry:     registry,
		EventStore:   eventStore,
		ToolContext:  &tools.ToolContext{Namespace: "default", TaskID: "task-a"},
		AllowedTools: map[string]struct{}{"slow": {}},
	}
	req := brokeredToolRequest("call-concurrent", json.RawMessage(`{"message":"one"}`))
	req.ToolName = "slow"
	var wg sync.WaitGroup
	results := make([]harness.ToolCallResult, 20)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			result, err := broker.Execute(context.Background(), req)
			if err != nil {
				t.Errorf("Execute() error = %v", err)
				return
			}
			results[i] = result
		}(i)
	}
	wg.Wait()
	if executions.Load() != 1 {
		t.Fatalf("executions = %d, want 1", executions.Load())
	}
	started, err := eventStore.ListExecutionEvents(context.Background(), store.ExecutionEventFilter{
		Namespace: "default", StreamID: "task-a", EventTypes: []string{events.ExecutionEventTypeToolCallStarted}, Limit: 10,
	})
	if err != nil {
		t.Fatalf("ListExecutionEvents: %v", err)
	}
	if len(started) != 1 {
		t.Fatalf("started events = %d, want 1", len(started))
	}
}

func TestBrokeredToolDisabledAndApprovalRequiredAreSafe(t *testing.T) {
	eventStore := store.NewFakeExecutionEventStore()
	registry := tools.NewRegistry()
	registry.Register(staticHarnessTool{name: "echo"})
	broker := harness.ToolBroker{
		Registry:     registry,
		EventStore:   eventStore,
		ToolContext:  &tools.ToolContext{Namespace: "default", TaskID: "task-a", SessionID: "session-a"},
		AllowedTools: map[string]struct{}{"other": {}},
	}
	disabled, err := broker.Execute(context.Background(), brokeredToolRequest("call-disabled", json.RawMessage(`{}`)))
	if err != nil {
		t.Fatalf("disabled Execute() error = %v", err)
	}
	if disabled.Error == nil || disabled.Error.Code != "tool_disabled" {
		t.Fatalf("disabled result = %#v, want tool_disabled", disabled)
	}
	approvedReq := brokeredToolRequest("call-approval", json.RawMessage(`{}`))
	approvedReq.RequiresApproval = true
	broker.AllowedTools = map[string]struct{}{"echo": {}}
	approval, err := broker.Execute(context.Background(), approvedReq)
	if err != nil {
		t.Fatalf("approval Execute() error = %v", err)
	}
	if approval.Error == nil || approval.Error.Code != brokeredApprovalRequiredCode || approval.Approved {
		t.Fatalf("approval result = %#v, want approval_required and not approved", approval)
	}
	listed, err := eventStore.ListExecutionEvents(context.Background(), store.ExecutionEventFilter{
		Namespace: "default", StreamID: "task-a", EventTypes: []string{events.ExecutionEventTypeApprovalRequested}, Limit: 10,
	})
	if err != nil {
		t.Fatalf("ListExecutionEvents: %v", err)
	}
	if len(listed) != 1 || !strings.Contains(listed[0].Summary, "approval") {
		t.Fatalf("approval events = %#v, want one request", listed)
	}
	again, err := broker.Execute(context.Background(), approvedReq)
	if err != nil {
		t.Fatalf("approval retry Execute() error = %v", err)
	}
	if again.Error == nil || again.Error.Code != brokeredApprovalRequiredCode {
		t.Fatalf("approval retry result = %#v, want still approval_required", again)
	}
	listed, err = eventStore.ListExecutionEvents(context.Background(), store.ExecutionEventFilter{
		Namespace: "default", StreamID: "task-a", EventTypes: []string{events.ExecutionEventTypeApprovalRequested}, Limit: 10,
	})
	if err != nil {
		t.Fatalf("ListExecutionEvents retry: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("approval request count after retry = %d, want 1", len(listed))
	}
	changed := approvedReq
	changed.Input = json.RawMessage(`{"changed":true}`)
	conflict, err := broker.Execute(context.Background(), changed)
	if err != nil {
		t.Fatalf("changed approval Execute() error = %v", err)
	}
	if conflict.Error == nil || conflict.Error.Code != "idempotency_conflict" {
		t.Fatalf("changed approval result = %#v, want idempotency conflict", conflict)
	}
}

func TestBrokeredToolApprovalRequestCompactsOversizedIdentifiers(t *testing.T) {
	eventStore := store.NewFakeExecutionEventStore()
	registry := tools.NewRegistry()
	registry.Register(staticHarnessTool{name: "echo"})
	broker := harness.ToolBroker{
		Registry:     registry,
		EventStore:   eventStore,
		ToolContext:  &tools.ToolContext{Namespace: "default", TaskID: "task-a", SessionID: "session-a"},
		AllowedTools: map[string]struct{}{"echo": {}},
	}
	req := brokeredToolRequest("call-"+strings.Repeat("x", events.MaxExecutionEventContentJSONBytes), json.RawMessage(`{}`))
	req.RequiresApproval = true
	req.IdempotencyKey = harness.ToolRequestIdempotencyKey(req.RuntimeSessionID, req.TurnID, req.ToolCallID)
	result, err := broker.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.Error == nil || result.Error.Code != brokeredApprovalRequiredCode {
		t.Fatalf("result = %#v, want approval required", result)
	}
	listed, err := eventStore.ListExecutionEvents(context.Background(), store.ExecutionEventFilter{
		Namespace: "default", StreamID: "task-a", EventTypes: []string{events.ExecutionEventTypeApprovalRequested}, Limit: 10,
	})
	if err != nil {
		t.Fatalf("ListExecutionEvents: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("approval events = %d, want 1", len(listed))
	}
	if len(listed[0].Content) > events.MaxExecutionEventContentJSONBytes {
		t.Fatalf("approval content bytes = %d, want <= %d", len(listed[0].Content), events.MaxExecutionEventContentJSONBytes)
	}
	var content map[string]any
	if err := json.Unmarshal(listed[0].Content, &content); err != nil {
		t.Fatalf("unmarshal content: %v", err)
	}
	if content["idempotencyKeyDigest"] == "" || content["toolCallIDDigest"] == "" {
		t.Fatalf("content = %#v, want identifier digests", content)
	}
}

func TestBrokeredToolApprovalRequiredExecutesAfterApproval(t *testing.T) {
	eventStore := store.NewFakeExecutionEventStore()
	registry := tools.NewRegistry()
	registry.Register(staticHarnessTool{name: "echo"})
	broker := harness.ToolBroker{
		Registry:     registry,
		EventStore:   eventStore,
		ToolContext:  &tools.ToolContext{Namespace: "default", TaskID: "task-a", SessionID: "session-a"},
		AllowedTools: map[string]struct{}{"echo": {}},
	}
	req := brokeredToolRequest("call-approved", json.RawMessage(`{"message":"approved"}`))
	req.RequiresApproval = true
	first, err := broker.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("first Execute() error = %v", err)
	}
	if first.Error == nil || first.Error.Code != brokeredApprovalRequiredCode {
		t.Fatalf("first result = %#v, want approval_required", first)
	}
	requests, err := eventStore.ListExecutionEvents(context.Background(), store.ExecutionEventFilter{
		Namespace: "default", StreamID: "task-a", EventTypes: []string{events.ExecutionEventTypeApprovalRequested}, Limit: 10,
	})
	if err != nil || len(requests) != 1 {
		t.Fatalf("approval requests=%#v err=%v, want one request", requests, err)
	}
	var content map[string]any
	if err := json.Unmarshal(requests[0].Content, &content); err != nil {
		t.Fatalf("unmarshal approval request: %v", err)
	}
	approvalID, _ := content["approvalID"].(string)
	approvedContent, _ := json.Marshal(map[string]string{"approvalID": approvalID, "decision": "approve"})
	if _, err := eventStore.AppendExecutionEvent(context.Background(), &store.ExecutionEvent{
		Namespace: "default", StreamType: store.ExecutionEventStreamTypeTask, StreamID: "task-a",
		TaskName: "task-a", SessionName: "session-a", ToolName: "echo", ToolCallID: req.ToolCallID,
		Type: events.ExecutionEventTypeApprovalApproved, Severity: events.ExecutionEventSeverityInfo, Content: approvedContent,
	}); err != nil {
		t.Fatalf("append approved: %v", err)
	}
	approved, err := broker.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("approved Execute() error = %v", err)
	}
	if approved.Error != nil || !approved.Approved || !strings.Contains(string(approved.Output), "approved") {
		t.Fatalf("approved result=%#v output=%s, want execution", approved, approved.Output)
	}
	retry, err := broker.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("approved retry Execute() error = %v", err)
	}
	if retry.Error != nil || string(retry.Output) != string(approved.Output) {
		t.Fatalf("retry result=%#v output=%s, want cached approved output %s", retry, retry.Output, approved.Output)
	}
}

func brokeredToolRequest(callID string, input json.RawMessage) harness.ToolCallRequest {
	runtimeID := harness.RuntimeSessionID("runtime-a")
	turnID := harness.HarnessTurnID("turn-a")
	return harness.ToolCallRequest{
		Version:          harness.ProtocolVersion,
		RuntimeSessionID: runtimeID,
		TurnID:           turnID,
		ToolCallID:       callID,
		ToolName:         "echo",
		IdempotencyKey:   harness.ToolRequestIdempotencyKey(runtimeID, turnID, callID),
		Input:            input,
	}
}

func hasHarnessBrokerEvent(listed []store.ExecutionEvent, typ string) bool {
	for _, event := range listed {
		if event.Type == typ {
			return true
		}
	}
	return false
}

type blockingHarnessTool struct {
	name       string
	executions *atomic.Int32
}

func (t blockingHarnessTool) Name() string        { return t.name }
func (t blockingHarnessTool) Description() string { return "blocking harness test tool" }
func (t blockingHarnessTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object"}`)
}
func (t blockingHarnessTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	t.executions.Add(1)
	time.Sleep(25 * time.Millisecond)
	return string(args), nil
}

type staticHarnessTool struct{ name string }

func (t staticHarnessTool) Name() string                { return t.name }
func (t staticHarnessTool) Description() string         { return "static harness test tool" }
func (t staticHarnessTool) Parameters() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t staticHarnessTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	return string(args), nil
}
