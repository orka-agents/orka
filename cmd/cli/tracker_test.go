/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
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

func TestUpdateAgentFromProgressCancelledIsNotSuccess(t *testing.T) {
	var buf bytes.Buffer
	tr := newTracker(&buf, VerbosityDefault, false)

	args, _ := json.Marshal(map[string]any{"agent": "worker", "name": "task-1"})
	tr.addAgent(client.SSEEventData{Name: "delegate_task", Args: args})

	result, _ := json.Marshal(map[string]any{"phase": "Cancelled", "name": "task-1"})
	tr.updateAgentFromProgress(client.SSEEventData{Result: result})
	if !tr.agents[0].done {
		t.Error("expected done on Cancelled")
	}
	if tr.agents[0].success {
		t.Error("expected no success on Cancelled")
	}

	// Stream completion must not later relabel the cancelled task as successful.
	tr.completeAgents(client.SSEEventData{})
	tr.stop()
	if tr.agents[0].success {
		t.Error("cancelled agent was reported as successful after stream completion")
	}
	// Non-TTY output must show the terminal outcome exactly once.
	if out := buf.String(); strings.Count(out, "✗ worker") != 1 || strings.Contains(out, "✓") {
		t.Errorf("non-TTY output = %q, want one failure line and no success line", out)
	}
}

func TestTrackerAppliesWrappedToolResultPhases(t *testing.T) {
	// Results use the shapes the chat API streams: check_task_progress and
	// wait_for_task wrap their payload in the chat tool envelope, while
	// wait_for_tasks returns a results list.
	for _, test := range []struct {
		name   string
		tool   string
		result string
	}{
		{name: "check_task_progress", tool: toolCheckTaskProgress, result: `{"success":true,"data":{"name":"task-1","namespace":"default","phase":"Cancelled"}}`},
		{name: "wait_for_task", tool: toolWaitForTask, result: `{"success":true,"data":{"name":"task-1","phase":"Cancelled","message":"cancelled"}}`},
		{name: "wait_for_tasks", tool: "wait_for_tasks", result: `{"completed":true,"results":[{"task":"task-1","phase":"Cancelled"},{"task":"task-2","phase":"Succeeded"}]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var buf bytes.Buffer
			tr := newTracker(&buf, VerbosityDefault, false)
			for _, name := range []string{"task-1", "task-2"} {
				args, _ := json.Marshal(map[string]any{"agent": name, "name": name})
				tr.handleEvent(makeSSEEvent("tool_call", client.SSEEventData{Name: "delegate_task", Args: args}))
			}
			tr.handleEvent(makeSSEEvent("tool_result", client.SSEEventData{Name: test.tool, Result: json.RawMessage(test.result)}))
			tr.completeAgents(client.SSEEventData{})
			tr.stop()

			if !tr.agents[0].done || tr.agents[0].success || tr.agents[0].status != "Cancelled" {
				t.Fatalf("cancelled agent = done %v success %v status %q, want a failed Cancelled agent",
					tr.agents[0].done, tr.agents[0].success, tr.agents[0].status)
			}
			if !tr.agents[1].done || !tr.agents[1].success {
				t.Fatalf("other agent = done %v success %v, want successful", tr.agents[1].done, tr.agents[1].success)
			}
			if out := buf.String(); strings.Count(out, "✗ task-1") != 1 || strings.Count(out, "✓ task-2") != 1 || strings.Contains(out, "✓ task-1") {
				t.Fatalf("non-TTY output = %q, want one failure line for task-1 and one success line for task-2", out)
			}
		})
	}
}

func TestTrackerMatchesGeneratedTaskNames(t *testing.T) {
	// delegate_task has no name argument; the server generates the child name
	// and returns it, and later results must still reach the right agent.
	for _, withIDs := range []bool{true, false} {
		t.Run(fmt.Sprintf("tool-call-ids=%v", withIDs), func(t *testing.T) {
			var buf bytes.Buffer
			tr := newTracker(&buf, VerbosityDefault, false)
			for i, agent := range []string{"alpha", "beta"} {
				id := ""
				if withIDs {
					id = fmt.Sprintf("call-%d", i)
				}
				args, _ := json.Marshal(map[string]any{"agent": agent, "task": "work"})
				tr.handleEvent(makeSSEEvent("tool_call", client.SSEEventData{ID: id, Name: "delegate_task", Args: args}))
				result := fmt.Sprintf(`{"taskName":"parent-child-%s","status":"created"}`, agent)
				tr.handleEvent(makeSSEEvent("tool_result", client.SSEEventData{ID: id, Name: "delegate_task", Result: json.RawMessage(result)}))
			}
			tr.handleEvent(makeSSEEvent("tool_result", client.SSEEventData{
				Name:   "wait_for_tasks",
				Result: json.RawMessage(`{"completed":true,"results":[{"task":"parent-child-beta","phase":"Cancelled"},{"task":"parent-child-alpha","phase":"Succeeded"}]}`),
			}))
			tr.stop()

			alpha, beta := tr.agents[0], tr.agents[1]
			if alpha.taskName != "parent-child-alpha" || beta.taskName != "parent-child-beta" {
				t.Fatalf("task names = %q, %q, want the generated names", alpha.taskName, beta.taskName)
			}
			if !alpha.done || !alpha.success || !beta.done || beta.success || beta.status != "Cancelled" {
				t.Fatalf("alpha done %v success %v; beta done %v success %v status %q, want alpha succeeded and beta cancelled",
					alpha.done, alpha.success, beta.done, beta.success, beta.status)
			}
			if out := buf.String(); strings.Count(out, "✗ beta") != 1 || strings.Count(out, "✓ alpha") != 1 || strings.Contains(out, "✓ beta") {
				t.Fatalf("non-TTY output = %q, want alpha succeeded and beta failed exactly once", out)
			}
		})
	}
}

func TestTrackerWaitResultsKeepUnfinishedAgentsPending(t *testing.T) {
	var buf bytes.Buffer
	tr := newTracker(&buf, VerbosityDefault, false)
	for _, name := range []string{"task-1", "task-2"} {
		args, _ := json.Marshal(map[string]any{"agent": name, "name": name})
		tr.handleEvent(makeSSEEvent("tool_call", client.SSEEventData{Name: "delegate_task", Args: args}))
	}

	// A timed-out wait reports a running phase; neither agent is finished.
	tr.handleEvent(makeSSEEvent("tool_result", client.SSEEventData{Name: toolWaitForTask,
		Result: json.RawMessage(`{"success":true,"data":{"name":"task-1","phase":"Running","message":"Task is still running."}}`)}))
	if tr.agents[0].done || tr.agents[1].done {
		t.Fatal("a timed-out wait marked an agent finished")
	}
	// Waiting on task-1 alone must not finish task-2.
	tr.handleEvent(makeSSEEvent("tool_result", client.SSEEventData{Name: toolWaitForTask,
		Result: json.RawMessage(`{"success":true,"data":{"name":"task-1","phase":"Succeeded"}}`)}))
	if !tr.agents[0].done || !tr.agents[0].success || tr.agents[1].done {
		t.Fatal("waiting on task-1 did not finish only task-1")
	}
	// task-2's later cancellation is still recorded.
	tr.handleEvent(makeSSEEvent("tool_result", client.SSEEventData{Name: toolCheckTaskProgress,
		Result: json.RawMessage(`{"success":true,"data":{"name":"task-2","phase":"Cancelled"}}`)}))
	tr.stop()
	if !tr.agents[1].done || tr.agents[1].success {
		t.Fatalf("task-2 = done %v success %v, want cancelled", tr.agents[1].done, tr.agents[1].success)
	}
	if out := buf.String(); strings.Count(out, "✓ task-1") != 1 || strings.Count(out, "✗ task-2") != 1 || strings.Contains(out, "✓ task-2") {
		t.Fatalf("non-TTY output = %q, want task-1 succeeded and task-2 failed exactly once", out)
	}
}

func TestTrackerFailedWaitResultFinishesNothing(t *testing.T) {
	var buf bytes.Buffer
	tr := newTracker(&buf, VerbosityDefault, false)
	args, _ := json.Marshal(map[string]any{"agent": "worker", "name": "task-1"})
	tr.handleEvent(makeSSEEvent("tool_call", client.SSEEventData{Name: "delegate_task", Args: args}))

	// The payload a kind cluster returned for a wait_for_task with a rejected
	// timeout argument.
	tr.handleEvent(makeSSEEvent("tool_result", client.SSEEventData{Name: toolWaitForTask,
		Result: json.RawMessage(`{"success":false,"error":"invalid arguments: timeout must be a whole number, got a non-numeric string","errorType":"invalid_arguments","suggestion":"Resend the call with timeout as a whole number"}`)}))
	if tr.agents[0].done {
		t.Fatal("a failed wait_for_task call finished the agent")
	}
	tr.handleEvent(makeSSEEvent("tool_result", client.SSEEventData{Name: toolWaitForTask,
		Result: json.RawMessage(`{"success":true,"data":{"name":"task-1","phase":"Cancelled"}}`)}))
	if !tr.agents[0].done || tr.agents[0].success {
		t.Fatalf("agent = done %v success %v, want cancelled", tr.agents[0].done, tr.agents[0].success)
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
