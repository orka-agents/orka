/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/cli/client"
)

const (
	statusStarting = "starting"
	statusChecking = "checking…"
)

func TestNewTracker(t *testing.T) {
	var buf bytes.Buffer
	tr := newTracker(&buf, VerbosityDefault, false)

	if tr == nil {
		t.Fatal("newTracker returned nil")
	}
	if tr.verbosity != VerbosityDefault {
		t.Errorf("verbosity = %d, want %d", tr.verbosity, VerbosityDefault)
	}
	if tr.isTTY {
		t.Error("expected isTTY to be false")
	}
	if len(tr.agents) != 0 {
		t.Errorf("agents len = %d, want 0", len(tr.agents))
	}
}

func TestNewTrackerVerbose(t *testing.T) {
	var buf bytes.Buffer
	tr := newTracker(&buf, VerbosityVV, true)

	if tr.verbosity != VerbosityVV {
		t.Errorf("verbosity = %d, want %d", tr.verbosity, VerbosityVV)
	}
	if !tr.isTTY {
		t.Error("expected isTTY to be true")
	}
}

func makeSSEEvent(event string, data client.SSEEventData) client.SSEEvent {
	d, _ := json.Marshal(data)
	return client.SSEEvent{Event: event, Data: string(d)}
}

func TestAddAgent(t *testing.T) {
	var buf bytes.Buffer
	tr := newTracker(&buf, VerbosityDefault, false)

	args, _ := json.Marshal(map[string]any{
		"agent":  "code-reviewer",
		"name":   "task-123",
		"prompt": "review the code",
	})

	data := client.SSEEventData{
		Name: "delegate_task",
		Args: args,
	}

	tr.addAgent(data)

	if len(tr.agents) != 1 {
		t.Fatalf("agents len = %d, want 1", len(tr.agents))
	}
	agent := tr.agents[0]
	if agent.name != "code-reviewer" {
		t.Errorf("name = %q, want %q", agent.name, "code-reviewer")
	}
	if agent.taskName != "task-123" {
		t.Errorf("taskName = %q, want %q", agent.taskName, "task-123")
	}
	if agent.prompt != "review the code" {
		t.Errorf("prompt = %q, want %q", agent.prompt, "review the code")
	}
	if agent.done {
		t.Error("expected agent not done")
	}
	if agent.status != statusStarting {
		t.Errorf("status = %q, want %q", agent.status, statusStarting)
	}

	// Non-TTY should write a static line
	output := buf.String()
	if !strings.Contains(output, "code-reviewer") {
		t.Errorf("expected output to contain agent name, got %q", output)
	}
}

func TestAddAgentWithAgentRef(t *testing.T) {
	var buf bytes.Buffer
	tr := newTracker(&buf, VerbosityDefault, false)

	args, _ := json.Marshal(map[string]any{
		"agentRef": "planner",
	})
	data := client.SSEEventData{
		Name: "create_agent_task",
		Args: args,
	}

	tr.addAgent(data)

	if len(tr.agents) != 1 {
		t.Fatalf("agents len = %d, want 1", len(tr.agents))
	}
	if tr.agents[0].name != "planner" {
		t.Errorf("name = %q, want %q", tr.agents[0].name, "planner")
	}
}

func TestAddAgentLongPromptTruncated(t *testing.T) {
	var buf bytes.Buffer
	tr := newTracker(&buf, VerbosityV, false)

	longPrompt := strings.Repeat("x", 100)
	args, _ := json.Marshal(map[string]any{
		"agent":  "a",
		"prompt": longPrompt,
	})
	data := client.SSEEventData{
		Name: "delegate_task",
		Args: args,
	}

	tr.addAgent(data)

	if len(tr.agents[0].prompt) != 83 { // 80 + "..."
		t.Errorf("prompt length = %d, want 83", len(tr.agents[0].prompt))
	}
}

func TestCompleteAgents(t *testing.T) {
	var buf bytes.Buffer
	tr := newTracker(&buf, VerbosityDefault, false)

	args, _ := json.Marshal(map[string]any{"agent": "worker"})
	tr.addAgent(client.SSEEventData{Name: "delegate_task", Args: args})

	buf.Reset()
	tr.completeAgents(client.SSEEventData{})

	if !tr.agents[0].done {
		t.Error("expected agent to be done")
	}
	if !tr.agents[0].success {
		t.Error("expected agent to be successful")
	}
	if tr.agents[0].status != statusDone {
		t.Errorf("status = %q, want %q", tr.agents[0].status, statusDone)
	}
	output := buf.String()
	if !strings.Contains(output, "✓") {
		t.Error("expected success icon in output")
	}
}

func TestUpdateAgentStatus(t *testing.T) {
	var buf bytes.Buffer
	tr := newTracker(&buf, VerbosityDefault, false)

	args, _ := json.Marshal(map[string]any{"agent": "worker", "name": "task-1"})
	tr.addAgent(client.SSEEventData{Name: "delegate_task", Args: args})

	statusArgs, _ := json.Marshal(map[string]any{"name": "task-1"})
	tr.updateAgentStatus(client.SSEEventData{Args: statusArgs}, statusChecking)

	if tr.agents[0].status != statusChecking {
		t.Errorf("status = %q, want %q", tr.agents[0].status, statusChecking)
	}
}

func TestUpdateAgentStatusNoMatch(t *testing.T) {
	var buf bytes.Buffer
	tr := newTracker(&buf, VerbosityDefault, false)

	args, _ := json.Marshal(map[string]any{"agent": "worker", "name": "task-1"})
	tr.addAgent(client.SSEEventData{Name: "delegate_task", Args: args})

	// Different task name
	statusArgs, _ := json.Marshal(map[string]any{"name": "task-2"})
	tr.updateAgentStatus(client.SSEEventData{Args: statusArgs}, statusChecking)

	if tr.agents[0].status != statusStarting {
		t.Errorf("status should remain %q, got %q", statusStarting, tr.agents[0].status)
	}
}

func TestUpdateAgentFromProgress(t *testing.T) {
	var buf bytes.Buffer
	tr := newTracker(&buf, VerbosityDefault, false)

	args, _ := json.Marshal(map[string]any{"agent": "worker", "name": "task-1"})
	tr.addAgent(client.SSEEventData{Name: "delegate_task", Args: args})

	result, _ := json.Marshal(map[string]any{"phase": "Running", "name": "task-1"})
	tr.updateAgentFromProgress(client.SSEEventData{Result: result})
	if tr.agents[0].status != "Running" {
		t.Errorf("status = %q, want %q", tr.agents[0].status, "Running")
	}
	if tr.agents[0].done {
		t.Error("should not be done while Running")
	}

	// Test Succeeded
	result2, _ := json.Marshal(map[string]any{"phase": "Succeeded", "name": "task-1"})
	tr.agents[0].done = false // reset for test
	tr.updateAgentFromProgress(client.SSEEventData{Result: result2})
	if !tr.agents[0].done {
		t.Error("expected done on Succeeded")
	}
	if !tr.agents[0].success {
		t.Error("expected success on Succeeded")
	}
}

func TestUpdateAgentFromProgressFailed(t *testing.T) {
	var buf bytes.Buffer
	tr := newTracker(&buf, VerbosityDefault, false)

	args, _ := json.Marshal(map[string]any{"agent": "worker", "name": "task-1"})
	tr.addAgent(client.SSEEventData{Name: "delegate_task", Args: args})

	result, _ := json.Marshal(map[string]any{"phase": "Failed", "name": "task-1"})
	tr.updateAgentFromProgress(client.SSEEventData{Result: result})
	if !tr.agents[0].done {
		t.Error("expected done on Failed")
	}
	if tr.agents[0].success {
		t.Error("expected no success on Failed")
	}
}

func TestCompleteAgentByName(t *testing.T) {
	var buf bytes.Buffer
	tr := newTracker(&buf, VerbosityDefault, false)

	args, _ := json.Marshal(map[string]any{"agent": "worker", "name": "task-1"})
	tr.addAgent(client.SSEEventData{Name: "delegate_task", Args: args})

	fetchArgs, _ := json.Marshal(map[string]any{"name": "task-1"})
	tr.completeAgentByName(client.SSEEventData{Args: fetchArgs})

	if !tr.agents[0].done {
		t.Error("expected agent done after completeAgentByName")
	}
	if !tr.agents[0].success {
		t.Error("expected success")
	}
}

func TestHandleEventToolCall(t *testing.T) {
	var buf bytes.Buffer
	tr := newTracker(&buf, VerbosityDefault, false)

	args, _ := json.Marshal(map[string]any{"agent": "reviewer", "name": "t1"})
	evt := makeSSEEvent("tool_call", client.SSEEventData{
		Name: "delegate_task",
		Args: args,
	})

	tr.handleEvent(evt)

	if len(tr.agents) != 1 {
		t.Fatalf("agents len = %d, want 1", len(tr.agents))
	}
	if tr.agents[0].name != "reviewer" {
		t.Errorf("name = %q, want %q", tr.agents[0].name, "reviewer")
	}
}

func TestHandleEventToolCallCheckProgress(t *testing.T) {
	var buf bytes.Buffer
	tr := newTracker(&buf, VerbosityDefault, false)

	// First add an agent
	addArgs, _ := json.Marshal(map[string]any{"agent": "worker", "name": "t1"})
	tr.handleEvent(makeSSEEvent("tool_call", client.SSEEventData{
		Name: "delegate_task",
		Args: addArgs,
	}))

	// Check progress event
	checkArgs, _ := json.Marshal(map[string]any{"name": "t1"})
	tr.handleEvent(makeSSEEvent("tool_call", client.SSEEventData{
		Name: toolCheckTaskProgress,
		Args: checkArgs,
	}))

	if tr.agents[0].status != statusChecking {
		t.Errorf("status = %q, want %q", tr.agents[0].status, statusChecking)
	}
}

func TestHandleEventToolResult(t *testing.T) {
	var buf bytes.Buffer
	tr := newTracker(&buf, VerbosityDefault, false)

	// Add an agent
	addArgs, _ := json.Marshal(map[string]any{"agent": "worker", "name": "t1"})
	tr.handleEvent(makeSSEEvent("tool_call", client.SSEEventData{
		Name: "delegate_task",
		Args: addArgs,
	}))

	// Wait for task result
	tr.handleEvent(makeSSEEvent("tool_result", client.SSEEventData{
		Name: toolWaitForTask,
	}))

	if !tr.agents[0].done {
		t.Error("expected agent done after wait_for_task result")
	}
}

func TestHandleEventInvalidJSON(t *testing.T) {
	var buf bytes.Buffer
	tr := newTracker(&buf, VerbosityDefault, false)

	// Invalid JSON should not panic
	evt := client.SSEEvent{Event: "tool_call", Data: "not-json"}
	tr.handleEvent(evt)

	if len(tr.agents) != 0 {
		t.Error("should have no agents after invalid JSON")
	}
}

func TestRenderNonTTY(t *testing.T) {
	var buf bytes.Buffer
	tr := newTracker(&buf, VerbosityDefault, false)

	// render with no agents should be no-op
	tr.render()
	if buf.Len() != 0 {
		t.Error("render with no agents should produce no output")
	}
}

func TestRenderFinal(t *testing.T) {
	var buf bytes.Buffer
	tr := newTracker(&buf, VerbosityDefault, false)

	args, _ := json.Marshal(map[string]any{"agent": "a1"})
	tr.addAgent(client.SSEEventData{Name: "delegate_task", Args: args})
	tr.agents[0].done = true
	tr.agents[0].success = true

	buf.Reset()
	tr.renderFinal()

	output := buf.String()
	if !strings.Contains(output, "✓") {
		t.Errorf("expected success icon, got %q", output)
	}
	if !strings.Contains(output, "a1") {
		t.Errorf("expected agent name, got %q", output)
	}
}

func TestRenderFinalFailed(t *testing.T) {
	var buf bytes.Buffer
	tr := newTracker(&buf, VerbosityDefault, false)

	args, _ := json.Marshal(map[string]any{"agent": "a1"})
	tr.addAgent(client.SSEEventData{Name: "delegate_task", Args: args})
	tr.agents[0].done = true
	tr.agents[0].success = false

	buf.Reset()
	tr.renderFinal()

	output := buf.String()
	if !strings.Contains(output, "✗") {
		t.Errorf("expected failure icon, got %q", output)
	}
}

func TestClearLines(t *testing.T) {
	var buf bytes.Buffer
	tr := newTracker(&buf, VerbosityDefault, true)

	tr.clearLines(3)

	output := buf.String()
	count := strings.Count(output, "\033[A\033[2K")
	if count != 3 {
		t.Errorf("expected 3 clear sequences, got %d", count)
	}
}

func TestIsTTYCheckWithNonFile(t *testing.T) {
	var buf bytes.Buffer
	if isTTYCheck(&buf) {
		t.Error("expected false for non-file writer")
	}
}

func TestIsTTYCheckWithDevNull(t *testing.T) {
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Skip("cannot open /dev/null")
	}
	defer f.Close() //nolint:errcheck

	// /dev/null is not a char device on all platforms, but should not panic
	_ = isTTYCheck(f)
}

func TestTrackerStop(t *testing.T) {
	var buf bytes.Buffer
	tr := newTracker(&buf, VerbosityDefault, false)

	args, _ := json.Marshal(map[string]any{"agent": "worker"})
	tr.addAgent(client.SSEEventData{Name: "delegate_task", Args: args})

	tr.stop()

	if !tr.stopped {
		t.Error("expected stopped to be true")
	}
	if !tr.agents[0].done {
		t.Error("expected agent to be marked done on stop")
	}
	if !tr.agents[0].success {
		t.Error("expected agent to be marked success on stop")
	}

	// Double stop should not panic
	tr.stop()
}

func TestHandleEventVerboseVV(t *testing.T) {
	var buf bytes.Buffer
	tr := newTracker(&buf, VerbosityVV, false)

	args, _ := json.Marshal(map[string]any{"agent": "reviewer"})
	evt := makeSSEEvent("tool_call", client.SSEEventData{
		Name: "delegate_task",
		Args: args,
	})

	tr.handleEvent(evt)

	output := buf.String()
	if !strings.Contains(output, "tool_call") {
		t.Error("expected verbose output with tool_call")
	}
}

func TestHandleEventToolResultVerbose(t *testing.T) {
	var buf bytes.Buffer
	tr := newTracker(&buf, VerbosityVV, false)

	// Add agent first
	addArgs, _ := json.Marshal(map[string]any{"agent": "worker", "name": "t1"})
	tr.handleEvent(makeSSEEvent("tool_call", client.SSEEventData{
		Name: "delegate_task",
		Args: addArgs,
	}))

	// Long result to test truncation
	longResult := strings.Repeat("x", 300)
	result, _ := json.Marshal(map[string]any{"output": longResult})
	tr.handleEvent(makeSSEEvent("tool_result", client.SSEEventData{
		Name:   toolCheckTaskProgress,
		Result: result,
	}))

	output := buf.String()
	if !strings.Contains(output, "tool_result") {
		t.Error("expected verbose output with tool_result")
	}
	if !strings.Contains(output, "...") {
		t.Error("expected truncation marker in verbose output")
	}
}

func TestStartSpinner_NonTTY(t *testing.T) {
	var buf bytes.Buffer
	tr := newTracker(&buf, VerbosityDefault, false)

	// Non-TTY: startSpinner should be a no-op
	tr.startSpinner()
	defer tr.stop()

	// No goroutine should be running, so this should be safe
	if tr.stopped {
		t.Error("tracker should not be stopped yet")
	}
}

func TestStartSpinner_TTY(t *testing.T) {
	var buf bytes.Buffer
	tr := newTracker(&buf, VerbosityDefault, true)

	// TTY: startSpinner should start the goroutine
	tr.startSpinner()

	// Add an agent to trigger rendering
	args, _ := json.Marshal(map[string]any{"agent": "spinner-test"})
	tr.addAgent(client.SSEEventData{Name: "delegate_task", Args: args})

	// Wait for at least one tick
	time.Sleep(200 * time.Millisecond)

	tr.stop()

	if !tr.stopped {
		t.Error("expected tracker to be stopped")
	}
}

func TestStartSpinner_NoActiveAgents(t *testing.T) { //nolint:unparam
	var buf bytes.Buffer
	tr := newTracker(&buf, VerbosityDefault, true)

	tr.startSpinner()
	// No agents — spinner should not render anything
	time.Sleep(200 * time.Millisecond)
	tr.stop()
}

func TestRenderTTY_ActiveAgent(t *testing.T) {
	var buf bytes.Buffer
	tr := newTracker(&buf, VerbosityDefault, true)

	args, _ := json.Marshal(map[string]any{"agent": "render-test", "name": "t1", "prompt": "do something"})
	tr.addAgent(client.SSEEventData{Name: "delegate_task", Args: args})

	// Render with an active (not done) agent
	tr.mu.Lock()
	tr.spinIdx = 0
	tr.render()
	tr.mu.Unlock()

	output := buf.String()
	if !strings.Contains(output, "render-test") {
		t.Errorf("expected agent name in render output, got %q", output)
	}
}

func TestRenderTTY_DoneAgent(t *testing.T) {
	var buf bytes.Buffer
	tr := newTracker(&buf, VerbosityDefault, true)

	args, _ := json.Marshal(map[string]any{"agent": "done-agent"})
	tr.addAgent(client.SSEEventData{Name: "delegate_task", Args: args})
	tr.agents[0].done = true
	tr.agents[0].success = true

	tr.mu.Lock()
	tr.render()
	tr.mu.Unlock()

	output := buf.String()
	if !strings.Contains(output, "done-agent") {
		t.Errorf("expected agent name in render output, got %q", output)
	}
}

func TestRenderTTY_FailedAgent(t *testing.T) {
	var buf bytes.Buffer
	tr := newTracker(&buf, VerbosityDefault, true)

	args, _ := json.Marshal(map[string]any{"agent": "fail-agent"})
	tr.addAgent(client.SSEEventData{Name: "delegate_task", Args: args})
	tr.agents[0].done = true
	tr.agents[0].success = false

	tr.mu.Lock()
	tr.render()
	tr.mu.Unlock()

	output := buf.String()
	if !strings.Contains(output, "fail-agent") {
		t.Errorf("expected agent name in render output, got %q", output)
	}
}

func TestRenderTTY_VerboseWithPrompt(t *testing.T) {
	var buf bytes.Buffer
	tr := newTracker(&buf, VerbosityV, true)

	args, _ := json.Marshal(map[string]any{"agent": "v-agent", "prompt": "do something cool"})
	tr.addAgent(client.SSEEventData{Name: "delegate_task", Args: args})

	tr.mu.Lock()
	tr.render()
	tr.mu.Unlock()

	output := buf.String()
	if !strings.Contains(output, "do something cool") {
		t.Errorf("expected prompt in verbose render output, got %q", output)
	}
}

func TestStopTTY_WithAgents(t *testing.T) {
	var buf bytes.Buffer
	tr := newTracker(&buf, VerbosityDefault, true)

	args, _ := json.Marshal(map[string]any{"agent": "stop-test"})
	tr.addAgent(client.SSEEventData{Name: "delegate_task", Args: args})

	tr.stop()

	if !tr.agents[0].done {
		t.Error("expected agent done after stop")
	}
	if !tr.agents[0].success {
		t.Error("expected agent success after stop")
	}
}

func TestRenderFinalVerboseWithResult(t *testing.T) {
	var buf bytes.Buffer
	tr := newTracker(&buf, VerbosityV, false)

	args, _ := json.Marshal(map[string]any{"agent": "result-agent"})
	tr.addAgent(client.SSEEventData{Name: "delegate_task", Args: args})
	tr.agents[0].done = true
	tr.agents[0].success = true
	tr.agents[0].result = "task completed successfully"

	buf.Reset()
	tr.renderFinal()

	output := buf.String()
	if !strings.Contains(output, "task completed successfully") {
		t.Errorf("expected result in verbose renderFinal, got %q", output)
	}
}

func TestHandleEventWaitForTasks(t *testing.T) {
	var buf bytes.Buffer
	tr := newTracker(&buf, VerbosityDefault, false)

	// Add agent
	addArgs, _ := json.Marshal(map[string]any{"agent": "w", "name": "t1"})
	tr.handleEvent(makeSSEEvent("tool_call", client.SSEEventData{
		Name: "delegate_task",
		Args: addArgs,
	}))

	// "wait_for_tasks" result should complete agents
	tr.handleEvent(makeSSEEvent("tool_result", client.SSEEventData{
		Name: "wait_for_tasks",
	}))

	if !tr.agents[0].done {
		t.Error("expected agent done after wait_for_tasks")
	}
}

func TestHandleEventFetchTaskOutput(t *testing.T) {
	var buf bytes.Buffer
	tr := newTracker(&buf, VerbosityDefault, false)

	addArgs, _ := json.Marshal(map[string]any{"agent": "w", "name": "t1"})
	tr.handleEvent(makeSSEEvent("tool_call", client.SSEEventData{
		Name: "delegate_task",
		Args: addArgs,
	}))

	fetchArgs, _ := json.Marshal(map[string]any{"name": "t1"})
	tr.handleEvent(makeSSEEvent("tool_result", client.SSEEventData{
		Name: toolFetchTaskOutput,
		Args: fetchArgs,
	}))

	if !tr.agents[0].done {
		t.Error("expected agent done after fetch_task_output")
	}
}

func TestHandleEventWaitForTask(t *testing.T) {
	var buf bytes.Buffer
	tr := newTracker(&buf, VerbosityDefault, false)

	addArgs, _ := json.Marshal(map[string]any{"agent": "w", "name": "t1"})
	tr.handleEvent(makeSSEEvent("tool_call", client.SSEEventData{
		Name: "delegate_task",
		Args: addArgs,
	}))

	// "wait_for_task" as a tool_call
	checkArgs, _ := json.Marshal(map[string]any{"name": "t1"})
	tr.handleEvent(makeSSEEvent("tool_call", client.SSEEventData{
		Name: toolWaitForTask,
		Args: checkArgs,
	}))

	if tr.agents[0].status != statusChecking {
		t.Errorf("status = %q, want %q", tr.agents[0].status, statusChecking)
	}
}

func TestCompleteAgentsTTY(t *testing.T) {
	var buf bytes.Buffer
	tr := newTracker(&buf, VerbosityDefault, true)

	args, _ := json.Marshal(map[string]any{"agent": "tty-agent"})
	tr.addAgent(client.SSEEventData{Name: "delegate_task", Args: args})

	buf.Reset()
	tr.completeAgents(client.SSEEventData{})

	if !tr.agents[0].done {
		t.Error("expected agent done")
	}
	// TTY output should include agent name in renderFinal
	output := buf.String()
	if !strings.Contains(output, "tty-agent") {
		t.Errorf("expected agent name in TTY output, got %q", output)
	}
}

func TestUpdateAgentFromProgressNoPhase(t *testing.T) {
	var buf bytes.Buffer
	tr := newTracker(&buf, VerbosityDefault, false)

	args, _ := json.Marshal(map[string]any{"agent": "w", "name": "t1"})
	tr.addAgent(client.SSEEventData{Name: "delegate_task", Args: args})

	// Result without phase — should not change status
	result, _ := json.Marshal(map[string]any{"name": "t1"})
	tr.updateAgentFromProgress(client.SSEEventData{Result: result})

	if tr.agents[0].status != statusStarting {
		t.Errorf("status = %q, want %q (should not change without phase)", tr.agents[0].status, statusStarting)
	}
}

func TestUpdateAgentStatusEmptyTaskName(t *testing.T) {
	var buf bytes.Buffer
	tr := newTracker(&buf, VerbosityDefault, false)

	args, _ := json.Marshal(map[string]any{"agent": "w", "name": "t1"})
	tr.addAgent(client.SSEEventData{Name: "delegate_task", Args: args})

	// Empty task name matches any
	statusArgs, _ := json.Marshal(map[string]any{})
	tr.updateAgentStatus(client.SSEEventData{Args: statusArgs}, "polling")

	if tr.agents[0].status != "polling" {
		t.Errorf("status = %q, want %q", tr.agents[0].status, "polling")
	}
}
