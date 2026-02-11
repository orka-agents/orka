/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tui

import (
	"encoding/json"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sozercan/mercan/internal/cli/client"
)

func newTestModel() Model {
	ch := make(chan client.SSEEvent)
	close(ch) // won't be reading from it
	m := New(ch)
	m.width = 80
	m.height = 24
	return m
}

func TestModel_Init(t *testing.T) {
	ch := make(chan client.SSEEvent)
	defer close(ch)
	m := New(ch)
	cmd := m.Init()
	if cmd == nil {
		t.Fatal("expected Init to return a non-nil command")
	}
}

func TestModel_KeyQuit(t *testing.T) {
	m := newTestModel()

	newModel, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if newModel == nil {
		t.Fatal("expected non-nil model")
	}
	if cmd == nil {
		t.Fatal("expected quit command")
	}

	// Execute the command and check it returns a QuitMsg
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Fatalf("expected tea.QuitMsg, got %T", msg)
	}
}

func TestModel_KeyCtrlC(t *testing.T) {
	m := newTestModel()

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatal("expected quit command for ctrl+c")
	}

	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Fatalf("expected tea.QuitMsg, got %T", msg)
	}
}

func TestModel_KeyTab(t *testing.T) {
	m := newTestModel()
	if m.focus != focusCoordinator {
		t.Fatalf("expected initial focus on coordinator, got %d", m.focus)
	}

	newModel, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	model := newModel.(Model)
	if model.focus != focusAgents {
		t.Fatalf("expected focus to switch to agents, got %d", model.focus)
	}

	// Tab again to go back
	newModel, _ = model.Update(tea.KeyMsg{Type: tea.KeyTab})
	model = newModel.(Model)
	if model.focus != focusCoordinator {
		t.Fatalf("expected focus to switch back to coordinator, got %d", model.focus)
	}
}

func TestModel_SSEMessage(t *testing.T) {
	m := newTestModel()

	msgData, _ := json.Marshal(client.MessageEventData{Content: "Hello from agent"})
	sseMsg := SSEMsg{
		Event: client.SSEEvent{
			Type: client.EventMessage,
			Data: msgData,
		},
	}

	newModel, _ := m.Update(sseMsg)
	model := newModel.(Model)
	content := model.coordinatorContent.String()
	if !strings.Contains(content, "Hello from agent") {
		t.Fatalf("expected coordinator content to contain message, got %q", content)
	}
}

func TestModel_SSEToolCall(t *testing.T) {
	m := newTestModel()

	msgData, _ := json.Marshal(client.ToolCallEventData{
		ID:   "tc-1",
		Name: "search_files",
		Args: json.RawMessage(`{"query":"main.go"}`),
	})
	sseMsg := SSEMsg{
		Event: client.SSEEvent{
			Type: client.EventToolCall,
			Data: msgData,
		},
	}

	newModel, _ := m.Update(sseMsg)
	model := newModel.(Model)
	content := model.coordinatorContent.String()
	if !strings.Contains(content, "search_files") {
		t.Fatalf("expected coordinator content to contain tool name, got %q", content)
	}
}

func TestModel_SSEDone(t *testing.T) {
	m := newTestModel()

	msgData, _ := json.Marshal(client.DoneEventData{
		Usage: client.ChatUsage{
			InputTokens:  500,
			OutputTokens: 200,
			LLMCalls:     3,
			ToolCalls:    2,
			Duration:     "10s",
		},
	})
	sseMsg := SSEMsg{
		Event: client.SSEEvent{
			Type: client.EventDone,
			Data: msgData,
		},
	}

	newModel, _ := m.Update(sseMsg)
	model := newModel.(Model)
	if !model.IsDone() {
		t.Fatal("expected model to be done after done event")
	}
	if model.streaming {
		t.Fatal("expected streaming to be false after done event")
	}
	usage := model.Usage()
	if usage.InputTokens != 500 {
		t.Fatalf("expected inputTokens 500, got %d", usage.InputTokens)
	}
	if usage.OutputTokens != 200 {
		t.Fatalf("expected outputTokens 200, got %d", usage.OutputTokens)
	}
}

func TestModel_SSEStatus(t *testing.T) {
	m := newTestModel()

	msgData, _ := json.Marshal(client.StatusEventData{
		SessionID: "s1",
		Provider:  "anthropic",
		Model:     "claude-3",
	})
	sseMsg := SSEMsg{
		Event: client.SSEEvent{
			Type: client.EventStatus,
			Data: msgData,
		},
	}

	newModel, _ := m.Update(sseMsg)
	model := newModel.(Model)
	content := model.coordinatorContent.String()
	if !strings.Contains(content, "anthropic/claude-3") {
		t.Fatalf("expected coordinator content to contain provider/model, got %q", content)
	}
}

func TestModel_SSEError(t *testing.T) {
	m := newTestModel()

	msgData, _ := json.Marshal(client.ErrorEventData{Error: "something went wrong"})
	sseMsg := SSEMsg{
		Event: client.SSEEvent{
			Type: client.EventError,
			Data: msgData,
		},
	}

	newModel, _ := m.Update(sseMsg)
	model := newModel.(Model)
	content := model.coordinatorContent.String()
	if !strings.Contains(content, "something went wrong") {
		t.Fatalf("expected coordinator content to contain error message, got %q", content)
	}
}

func TestModel_AgentStatusMsg(t *testing.T) {
	m := newTestModel()

	agents := []AgentInfo{
		{Name: "agent-1", Phase: "Running", Duration: "5s"},
		{Name: "agent-2", Phase: "Succeeded", Duration: "10s", Summary: "completed"},
	}

	newModel, _ := m.Update(AgentStatusMsg{Agents: agents})
	model := newModel.(Model)
	if len(model.agents) != 2 {
		t.Fatalf("expected 2 agents, got %d", len(model.agents))
	}
	if model.agents[0].Name != "agent-1" {
		t.Fatalf("expected agent-1, got %s", model.agents[0].Name)
	}
	if model.agents[1].Phase != "Succeeded" {
		t.Fatalf("expected Succeeded phase, got %s", model.agents[1].Phase)
	}
}

func TestModel_AgentStatusMsg_CursorClamp(t *testing.T) {
	m := newTestModel()
	m.agentCursor = 5 // Out of bounds

	agents := []AgentInfo{
		{Name: "agent-1", Phase: "Running"},
	}

	newModel, _ := m.Update(AgentStatusMsg{Agents: agents})
	model := newModel.(Model)
	if model.agentCursor != 0 {
		t.Fatalf("expected cursor clamped to 0, got %d", model.agentCursor)
	}
}

func TestModel_AgentTransitionMsg(t *testing.T) {
	m := newTestModel()

	newModel, _ := m.Update(AgentTransitionMsg{
		Name:     "agent-1",
		OldPhase: "Pending",
		NewPhase: "Running",
	})
	model := newModel.(Model)
	content := model.coordinatorContent.String()
	if !strings.Contains(content, "agent-1") {
		t.Fatalf("expected transition notification to contain agent name, got %q", content)
	}
	if !strings.Contains(content, "Pending") || !strings.Contains(content, "Running") {
		t.Fatalf("expected transition phases in notification, got %q", content)
	}
}

func TestModel_StreamDoneMsg(t *testing.T) {
	m := newTestModel()
	m.streaming = true

	usage := client.ChatUsage{InputTokens: 100, OutputTokens: 50}
	newModel, _ := m.Update(StreamDoneMsg{Usage: usage})
	model := newModel.(Model)
	if !model.done {
		t.Fatal("expected done to be true")
	}
	if model.streaming {
		t.Fatal("expected streaming to be false")
	}
	if model.usage.InputTokens != 100 {
		t.Fatalf("expected inputTokens 100, got %d", model.usage.InputTokens)
	}
}

func TestModel_StreamErrMsg(t *testing.T) {
	m := newTestModel()
	m.streaming = true

	newModel, _ := m.Update(StreamErrMsg{Err: errTest})
	model := newModel.(Model)
	if !model.done {
		t.Fatal("expected done to be true after error")
	}
	if model.streaming {
		t.Fatal("expected streaming to be false after error")
	}
	content := model.coordinatorContent.String()
	if !strings.Contains(content, "test error") {
		t.Fatalf("expected error in coordinator content, got %q", content)
	}
}

var errTest = testError("test error")

type testError string

func (e testError) Error() string { return string(e) }

func TestModel_PeekOverlay(t *testing.T) {
	m := newTestModel()
	m.focus = focusAgents
	m.agents = []AgentInfo{
		{Name: "agent-1", Result: "task completed successfully"},
	}
	m.agentCursor = 0

	// Press enter to open peek
	newModel, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model := newModel.(Model)
	if !model.peekVisible {
		t.Fatal("expected peek to be visible after enter")
	}
	if model.peekContent != "task completed successfully" {
		t.Fatalf("expected peek content, got %q", model.peekContent)
	}

	// Press esc to close peek
	newModel, _ = model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	model = newModel.(Model)
	if model.peekVisible {
		t.Fatal("expected peek to be hidden after esc")
	}
}

func TestModel_PeekOverlay_NoResult(t *testing.T) {
	m := newTestModel()
	m.focus = focusAgents
	m.agents = []AgentInfo{
		{Name: "agent-1", Result: ""},
	}
	m.agentCursor = 0

	newModel, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model := newModel.(Model)
	if !model.peekVisible {
		t.Fatal("expected peek to be visible")
	}
	if model.peekContent != "(no result yet)" {
		t.Fatalf("expected '(no result yet)', got %q", model.peekContent)
	}
}

func TestModel_WindowSizeMsg(t *testing.T) {
	m := newTestModel()

	newModel, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	model := newModel.(Model)
	if model.width != 120 {
		t.Fatalf("expected width 120, got %d", model.width)
	}
	if model.height != 40 {
		t.Fatalf("expected height 40, got %d", model.height)
	}
}

func TestModel_View_Loading(t *testing.T) {
	ch := make(chan client.SSEEvent)
	close(ch)
	m := New(ch)
	// width is 0 → should show "Loading..."
	view := m.View()
	if view != "Loading..." {
		t.Fatalf("expected 'Loading...' for zero-width, got %q", view)
	}
}

func TestModel_View_Main(t *testing.T) {
	m := newTestModel()
	view := m.View()
	if view == "" {
		t.Fatal("expected non-empty view")
	}
	if !strings.Contains(view, "Coordinator") {
		t.Fatalf("expected view to contain 'Coordinator', got %q", view)
	}
}

func TestModel_ArrowKeys(t *testing.T) {
	m := newTestModel()
	m.focus = focusAgents
	m.agents = []AgentInfo{
		{Name: "agent-0"},
		{Name: "agent-1"},
		{Name: "agent-2"},
	}
	m.agentCursor = 0

	// Down
	newModel, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	model := newModel.(Model)
	if model.agentCursor != 1 {
		t.Fatalf("expected cursor 1 after down, got %d", model.agentCursor)
	}

	// Down again
	newModel, _ = model.Update(tea.KeyMsg{Type: tea.KeyDown})
	model = newModel.(Model)
	if model.agentCursor != 2 {
		t.Fatalf("expected cursor 2 after second down, got %d", model.agentCursor)
	}

	// Down at boundary — should not go beyond
	newModel, _ = model.Update(tea.KeyMsg{Type: tea.KeyDown})
	model = newModel.(Model)
	if model.agentCursor != 2 {
		t.Fatalf("expected cursor to stay at 2, got %d", model.agentCursor)
	}

	// Up
	newModel, _ = model.Update(tea.KeyMsg{Type: tea.KeyUp})
	model = newModel.(Model)
	if model.agentCursor != 1 {
		t.Fatalf("expected cursor 1 after up, got %d", model.agentCursor)
	}
}

func TestModel_IsDone(t *testing.T) {
	m := newTestModel()
	if m.IsDone() {
		t.Fatal("expected IsDone to be false initially")
	}
	m.done = true
	if !m.IsDone() {
		t.Fatal("expected IsDone to be true after setting done")
	}
}

func TestModel_Usage(t *testing.T) {
	m := newTestModel()
	m.usage = client.ChatUsage{InputTokens: 42, OutputTokens: 17}
	usage := m.Usage()
	if usage.InputTokens != 42 || usage.OutputTokens != 17 {
		t.Fatalf("unexpected usage: %+v", usage)
	}
}
