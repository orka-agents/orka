package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/harness"
	"github.com/orka-agents/orka/internal/harness/conformance"
)

func TestFoundryAdapterObservedTurnCompletes(t *testing.T) {
	foundry := newFakeFoundry(t, "completed")
	adapter := newTestFoundryAdapter(foundry.URL)
	client := newHarnessClient(t, adapter)

	request := foundryStartTurnRequest("foundry-observed")
	if _, err := client.StartTurn(context.Background(), request); err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	var frames []harness.HarnessEventFrame
	if err := client.StreamFrames(context.Background(), request.TurnID, 0, func(frame harness.HarnessEventFrame) error {
		frames = append(frames, frame)
		return nil
	}); err != nil {
		t.Fatalf("StreamFrames: %v", err)
	}
	if !hasFrameType(frames, harness.FrameTurnStarted) || !hasFrameType(frames, harness.FrameTurnCompleted) {
		t.Fatalf("frames = %#v, want started and completed", frames)
	}
	if got := frames[len(frames)-1].Completed.Result; got != "foundry final answer" {
		t.Fatalf("result = %q", got)
	}
	if !foundry.sawEmptyTools.Load() {
		t.Fatalf("observed Foundry run did not disable persisted Foundry tools with tools=[]")
	}
}

func TestFoundryAdapterBrokeredReadContinuation(t *testing.T) {
	foundry := newFakeFoundry(t, "requires_action")
	adapter := newTestFoundryAdapter(foundry.URL)
	client := newHarnessClient(t, adapter)

	request := foundryStartTurnRequest("foundry-brokered")
	request.ToolExecutionMode = harness.ToolExecutionModeBrokered
	request.Input.Tools = []harness.ToolDefinition{{
		Name:          "support-ticket-lookup",
		Description:   "Look up support ticket",
		BrokeredClass: harness.BrokeredToolClassRead,
		Parameters:    json.RawMessage(`{"type":"object","properties":{"incident":{"type":"string"}}}`),
	}}
	if _, err := client.StartTurn(context.Background(), request); err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	var frames []harness.HarnessEventFrame
	if err := client.StreamFrames(context.Background(), request.TurnID, 0, func(frame harness.HarnessEventFrame) error {
		frames = append(frames, frame)
		return nil
	}); err != nil {
		t.Fatalf("StreamFrames before continue: %v", err)
	}
	requested := findFrame(frames, harness.FrameToolCallRequested)
	if requested == nil {
		t.Fatalf("frames = %#v, want tool request", frames)
	}
	if requested.ToolName != "support-ticket-lookup" || requested.ToolCallID != "call-1" {
		t.Fatalf("tool request = %#v", requested)
	}
	if !foundry.sawSafeToolSchema.Load() {
		t.Fatalf("Foundry run did not receive safe Orka tool schema")
	}
	_, err := client.ContinueTurn(context.Background(), harness.ContinueTurnRequest{
		Version:          harness.ProtocolVersion,
		Namespace:        request.Namespace,
		TaskName:         request.TaskName,
		SessionName:      request.SessionName,
		RuntimeSessionID: request.RuntimeSessionID,
		TurnID:           request.TurnID,
		CorrelationID:    request.CorrelationID,
		ToolResults: []harness.ToolCallResult{{
			Version:          harness.ProtocolVersion,
			RuntimeSessionID: request.RuntimeSessionID,
			TurnID:           request.TurnID,
			ToolCallID:       requested.ToolCallID,
			IdempotencyKey:   harness.ToolRequestIdempotencyKey(request.RuntimeSessionID, request.TurnID, requested.ToolCallID),
			Approved:         true,
			Output:           json.RawMessage(`{"success":true,"data":{"status":"ok"}}`),
		}},
	})
	if err != nil {
		t.Fatalf("ContinueTurn: %v", err)
	}
	frames = nil
	if err := client.StreamFrames(context.Background(), request.TurnID, requested.Seq, func(frame harness.HarnessEventFrame) error { //nolint:lll
		frames = append(frames, frame)
		return nil
	}); err != nil {
		t.Fatalf("StreamFrames after continue: %v", err)
	}
	if !hasFrameType(frames, harness.FrameToolResultReceived) || !hasFrameType(frames, harness.FrameTurnCompleted) {
		t.Fatalf("frames = %#v, want tool result and completion", frames)
	}
	if foundry.submittedToolOutput.Load() != 1 {
		t.Fatalf("submitted tool outputs = %d, want 1", foundry.submittedToolOutput.Load())
	}
}

func TestFoundryAdapterPassesObservedConformance(t *testing.T) {
	foundry := newFakeFoundry(t, "completed")
	adapter := newTestFoundryAdapter(foundry.URL)
	defer adapter.Close()
	result := conformance.Check(context.Background(), conformance.Target{
		BaseURL:        adapter.URL,
		BearerToken:    "adapter-token",
		ControlTimeout: 2 * time.Second,
		ProbeTurn:      true,
		RequireAuth:    true,
	})
	if !result.Passed {
		t.Fatalf("observed conformance failed: %s", result.Message)
	}
}

func TestFoundryAdapterPassesBrokeredReadConformance(t *testing.T) {
	foundry := newFakeFoundry(t, "requires_action")
	adapter := newTestFoundryAdapter(foundry.URL)
	defer adapter.Close()
	result := conformance.Check(context.Background(), conformance.Target{
		BaseURL:           adapter.URL,
		BearerToken:       "adapter-token",
		ControlTimeout:    2 * time.Second,
		ProbeBrokeredRead: true,
		RequireAuth:       true,
	})
	if !result.Passed {
		t.Fatalf("brokered read conformance failed: %s", result.Message)
	}
}

type fakeFoundry struct {
	*httptest.Server
	status              string
	submittedToolOutput atomic.Int32
	sawSafeToolSchema   atomic.Bool
	sawEmptyTools       atomic.Bool
	toolName            atomic.Value
}

func newFakeFoundry(t *testing.T, status string) *fakeFoundry {
	t.Helper()
	f := &fakeFoundry{status: status}
	mux := http.NewServeMux()
	mux.HandleFunc("/threads", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, map[string]string{"id": "thread-1"})
	})
	mux.HandleFunc("/threads/thread-1/runs", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if tools, ok := body["tools"].([]any); ok && len(tools) == 0 {
			f.sawEmptyTools.Store(true)
		}
		if tools, ok := body["tools"].([]any); ok && len(tools) == 1 {
			encoded, _ := json.Marshal(tools[0])
			text := string(encoded)
			if !strings.Contains(text, "http://") && !strings.Contains(text, "Secret") {
				f.sawSafeToolSchema.Store(true)
			}
			if tool, ok := tools[0].(map[string]any); ok {
				if fn, ok := tool["function"].(map[string]any); ok {
					if name, ok := fn["name"].(string); ok {
						f.toolName.Store(name)
					}
				}
			}
		}
		writeJSON(w, map[string]string{"id": "run-1", "status": f.status})
	})
	mux.HandleFunc("/threads/thread-1/runs/run-1", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			if f.status == "requires_action" && f.submittedToolOutput.Load() == 0 {
				writeJSON(w, map[string]any{"id": "run-1", "status": "requires_action", "required_action": map[string]any{"submit_tool_outputs": map[string]any{"tool_calls": []any{map[string]any{"id": "call-1", "type": "function", "function": map[string]any{"name": f.currentToolName(), "arguments": json.RawMessage(`{"incident":"inc-1"}`)}}}}}}) //nolint:lll
				return
			}
			writeJSON(w, map[string]string{"id": "run-1", "status": "completed"})
		case http.MethodPost:
			if strings.HasSuffix(r.URL.Path, "/cancel") {
				writeJSON(w, map[string]string{"id": "run-1", "status": "cancelled"})
				return
			}
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	})
	mux.HandleFunc("/threads/thread-1/runs/run-1/submit_tool_outputs", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		f.submittedToolOutput.Add(1)
		writeJSON(w, map[string]string{"id": "run-1", "status": "queued"})
	})
	mux.HandleFunc("/threads/thread-1/messages", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"data": []any{map[string]any{"role": "assistant", "content": "foundry final answer"}}})
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func (f *fakeFoundry) currentToolName() string {
	if value, ok := f.toolName.Load().(string); ok && value != "" {
		return value
	}
	return "support-ticket-lookup"
}

func newTestFoundryAdapter(endpoint string) *httptest.Server {
	s := &server{cfg: config{addr: ":0", runtimeName: "foundry-test", adapterBearer: "adapter-token", endpoint: endpoint, foundryKey: "foundry-key", agentID: "agent-1", apiVersion: "", pollTimeout: time.Second, pollInterval: time.Millisecond}, client: &http.Client{Timeout: time.Second}, turns: map[harness.HarnessTurnID]*turnState{}, runtimeThreads: map[harness.RuntimeSessionID]string{}, runtimeThreadSeen: map[harness.RuntimeSessionID]time.Time{}} //nolint:lll
	return httptest.NewServer(s.handler())
}

func newHarnessClient(t *testing.T, adapter *httptest.Server) *harness.Client {
	t.Helper()
	t.Cleanup(adapter.Close)
	client, err := harness.NewClient(adapter.URL, harness.WithBearerToken("adapter-token"), harness.WithControlTimeout(2*time.Second)) //nolint:lll
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return client
}

func foundryStartTurnRequest(name string) harness.StartTurnRequest {
	return harness.StartTurnRequest{Version: harness.ProtocolVersion, Namespace: "default", TaskName: name, SessionName: name, RuntimeSessionID: harness.RuntimeSessionID(name + "-runtime"), TurnID: harness.HarnessTurnID(name + "-turn"), CorrelationID: name + "-corr", Deadline: time.Now().UTC().Add(time.Minute), AuthIdentity: harness.AuthIdentity{Subject: "task:default/" + name}, ToolExecutionMode: harness.ToolExecutionModeObserved, Input: harness.TurnInput{Prompt: "Investigate incident"}} //nolint:lll
}

func hasFrameType(frames []harness.HarnessEventFrame, typ harness.FrameType) bool {
	return findFrame(frames, typ) != nil
}
func findFrame(frames []harness.HarnessEventFrame, typ harness.FrameType) *harness.HarnessEventFrame {
	for i := range frames {
		if frames[i].Type == typ {
			return &frames[i]
		}
	}
	return nil
}
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
