/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/orka-agents/orka/internal/cli/client"
)

const (
	VerbosityDefault = 0
	VerbosityV       = 1
	VerbosityVV      = 2
)

const (
	toolCheckTaskProgress = "check_task_progress"
	toolWaitForTask       = "wait_for_task"
	toolFetchTaskOutput   = "fetch_task_output"
	toolDelegateTask      = "delegate_task"
	toolCreateAgentTask   = "create_agent_task"
	statusDone            = "done"
)

type agentEntry struct {
	name       string
	taskName   string
	toolCallID string
	prompt     string
	status     string
	startTime  time.Time
	done       bool
	success    bool
	result     string
}

type tracker struct {
	mu        sync.Mutex
	agents    []*agentEntry
	verbosity int
	w         io.Writer
	isTTY     bool
	spinIdx   int
	stopCh    chan struct{}
	stopped   bool

	spinnerStyle lipgloss.Style
	nameStyle    lipgloss.Style
	successStyle lipgloss.Style
	failStyle    lipgloss.Style
	dimStyle     lipgloss.Style
	timeStyle    lipgloss.Style
}

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func newTracker(w io.Writer, verbosity int, isTTY bool) *tracker {
	t := &tracker{
		agents:       make([]*agentEntry, 0),
		verbosity:    verbosity,
		w:            w,
		isTTY:        isTTY,
		stopCh:       make(chan struct{}),
		spinnerStyle: lipgloss.NewStyle().Foreground(lipgloss.Color("6")),
		nameStyle:    lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("4")),
		successStyle: lipgloss.NewStyle().Foreground(lipgloss.Color("2")),
		failStyle:    lipgloss.NewStyle().Foreground(lipgloss.Color("1")),
		dimStyle:     lipgloss.NewStyle().Faint(true),
		timeStyle:    lipgloss.NewStyle().Faint(true),
	}
	return t
}

func (t *tracker) startSpinner() {
	if !t.isTTY {
		return
	}
	go func() {
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-t.stopCh:
				return
			case <-tick.C:
				t.mu.Lock()
				hasActive := false
				for _, a := range t.agents {
					if !a.done {
						hasActive = true
						break
					}
				}
				if hasActive {
					t.spinIdx = (t.spinIdx + 1) % len(spinnerFrames)
					t.render()
				}
				t.mu.Unlock()
			}
		}
	}()
}

func (t *tracker) stop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.stopped {
		t.stopped = true
		close(t.stopCh)
		for _, a := range t.agents {
			if !a.done {
				a.done = true
				a.success = true
				a.status = statusDone
			}
		}
		if t.isTTY && len(t.agents) > 0 {
			t.clearLines(len(t.agents))
			t.renderFinal()
		}
	}
}

func (t *tracker) handleEvent(evt client.SSEEvent) {
	var data client.SSEEventData
	if err := json.Unmarshal([]byte(evt.Data), &data); err != nil {
		return
	}

	switch evt.Event {
	case "tool_call":
		switch data.Name {
		case toolDelegateTask, toolCreateAgentTask:
			t.addAgent(data)
		case toolCheckTaskProgress, toolWaitForTask:
			t.updateAgentStatus(data, "checking…")
		}
		if t.verbosity >= VerbosityVV {
			t.mu.Lock()
			fmt.Fprintf(t.w, "%s tool_call: %s %s\n", //nolint:errcheck
				t.dimStyle.Render("│"), data.Name, t.dimStyle.Render(string(data.Args)))
			t.mu.Unlock()
		}
	case "tool_result":
		switch data.Name {
		case toolDelegateTask, toolCreateAgentTask:
			t.recordCreatedTaskName(data)
		case "wait_for_tasks", toolWaitForTask:
			t.completeAgents(data)
		case toolCheckTaskProgress:
			t.updateAgentFromProgress(data)
		case toolFetchTaskOutput:
			t.completeAgentByName(data)
		}
		if t.verbosity >= VerbosityVV {
			t.mu.Lock()
			truncated := string(data.Result)
			if len(truncated) > 200 {
				truncated = truncated[:200] + "..."
			}
			fmt.Fprintf(t.w, "%s tool_result: %s %s\n", //nolint:errcheck
				t.dimStyle.Render("│"), data.Name, t.dimStyle.Render(truncated))
			t.mu.Unlock()
		}
	}
}

func (t *tracker) addAgent(data client.SSEEventData) {
	t.mu.Lock()
	defer t.mu.Unlock()

	var args map[string]any
	agentName := cliTaskTypeAgent
	taskName := ""
	prompt := ""
	if err := json.Unmarshal(data.Args, &args); err == nil {
		if name, ok := args[cliTaskTypeAgent].(string); ok {
			agentName = name
		} else if name, ok := args["agentRef"].(string); ok {
			agentName = name
		}
		if name, ok := args["name"].(string); ok {
			taskName = name
		}
		if p, ok := args["prompt"].(string); ok {
			prompt = p
			if len(prompt) > 80 {
				prompt = prompt[:80] + "..."
			}
		}
	}

	entry := &agentEntry{
		name:       agentName,
		taskName:   taskName,
		toolCallID: data.ID,
		prompt:     prompt,
		status:     "starting",
		startTime:  time.Now(),
	}
	t.agents = append(t.agents, entry)

	if t.isTTY {
	} else {
		line := fmt.Sprintf("├─ ◦ %s", agentName)
		if t.verbosity >= VerbosityV && prompt != "" {
			line += fmt.Sprintf("  %s", prompt)
		}
		fmt.Fprintln(t.w, line) //nolint:errcheck
	}
}

func (t *tracker) recordCreatedTaskName(data client.SSEEventData) {
	var result struct {
		TaskName string `json:"taskName"`
		Name     string `json:"name"`
		Data     struct {
			Name string `json:"name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data.Result, &result); err != nil {
		return
	}
	taskName := result.TaskName
	if taskName == "" {
		taskName = result.Data.Name
	}
	if taskName == "" {
		taskName = result.Name
	}
	if taskName == "" {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	var target *agentEntry
	for _, a := range t.agents {
		if data.ID != "" && a.toolCallID == data.ID {
			target = a
			break
		}
		if data.ID == "" && a.taskName == "" {
			target = a
		}
	}
	if target != nil && target.taskName == "" {
		target.taskName = taskName
	}
}

func (t *tracker) completeAgents(data client.SSEEventData) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if toolResultFailed(data.Result) {
		return
	}
	reported := taskPhasesFromResult(data.Result)
	for _, r := range reported {
		t.applyTaskPhaseLocked(r.task, r.phase)
	}
	if len(reported) > 0 {
		if t.isTTY && t.allAgentsDoneLocked() {
			t.clearLines(len(t.agents))
			t.renderFinal()
		}
		return
	}
	for _, a := range t.agents {
		if !a.done {
			a.done = true
			a.success = true
			a.status = statusDone
			elapsed := time.Since(a.startTime).Round(time.Second)

			if !t.isTTY {
				status := "✓"
				fmt.Fprintf(t.w, "├─ %s %s  %s\n", status, a.name, elapsed) //nolint:errcheck
			}
		}
	}

	if t.isTTY {
		t.clearLines(len(t.agents))
		t.renderFinal()
	}
}

func (t *tracker) allAgentsDoneLocked() bool {
	for _, a := range t.agents {
		if !a.done {
			return false
		}
	}
	return true
}

func (t *tracker) updateAgentStatus(data client.SSEEventData, status string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	var args map[string]any
	if err := json.Unmarshal(data.Args, &args); err == nil {
		taskName, _ := args["name"].(string)
		for _, a := range t.agents {
			if !a.done && (a.taskName == taskName || taskName == "") {
				a.status = status
				break
			}
		}
	}
}

func (t *tracker) updateAgentFromProgress(data client.SSEEventData) {
	t.mu.Lock()
	defer t.mu.Unlock()

	for _, reported := range taskPhasesFromResult(data.Result) {
		t.applyTaskPhaseLocked(reported.task, reported.phase)
	}
}

type reportedTaskPhase struct {
	task  string
	phase string
}

func toolResultFailed(raw json.RawMessage) bool {
	var envelope struct {
		Success *bool `json:"success"`
	}
	return json.Unmarshal(raw, &envelope) == nil && envelope.Success != nil && !*envelope.Success
}

func taskPhasesFromResult(raw json.RawMessage) []reportedTaskPhase {
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	payload := raw
	if err := json.Unmarshal(raw, &envelope); err == nil && len(envelope.Data) > 0 {
		payload = envelope.Data
	}
	var result struct {
		Name    string `json:"name"`
		Phase   string `json:"phase"`
		Results []struct {
			Task  string `json:"task"`
			Phase string `json:"phase"`
		} `json:"results"`
	}
	if err := json.Unmarshal(payload, &result); err != nil {
		return nil
	}
	var phases []reportedTaskPhase
	for _, r := range result.Results {
		if r.Phase != "" {
			phases = append(phases, reportedTaskPhase{task: r.Task, phase: r.Phase})
		}
	}
	if result.Phase != "" {
		phases = append(phases, reportedTaskPhase{task: result.Name, phase: result.Phase})
	}
	return phases
}

func (t *tracker) applyTaskPhaseLocked(taskName, phase string) {
	for _, a := range t.agents {
		if !a.done && (a.taskName == taskName || taskName == "") {
			a.status = phase
			switch phase {
			case "Succeeded":
				a.done = true
				a.success = true
			case "Failed", "Cancelled":
				a.done = true
				a.success = false
			}
			if a.done && !t.isTTY {
				icon := "✓"
				if !a.success {
					icon = "✗"
				}
				fmt.Fprintf(t.w, "├─ %s %s  %s\n", icon, a.name, time.Since(a.startTime).Round(time.Second)) //nolint:errcheck
			}
			return
		}
	}
}

func (t *tracker) completeAgentByName(data client.SSEEventData) {
	t.mu.Lock()
	defer t.mu.Unlock()

	var args map[string]any
	if err := json.Unmarshal(data.Args, &args); err == nil {
		taskName, _ := args["name"].(string)
		for _, a := range t.agents {
			if !a.done && (a.taskName == taskName || taskName == "") {
				a.done = true
				a.success = true
				a.status = statusDone
				break
			}
		}
	}
}

func (t *tracker) render() {
	if len(t.agents) == 0 {
		return
	}

	if t.spinIdx > 0 || len(t.agents) > 0 {
		for range t.agents {
			fmt.Fprint(t.w, "\033[A\033[2K") //nolint:errcheck
		}
	}

	for _, a := range t.agents {
		if a.done {
			icon := t.successStyle.Render("✓")
			if !a.success {
				icon = t.failStyle.Render("✗")
			}
			elapsed := time.Since(a.startTime).Round(time.Second)
			fmt.Fprintf(t.w, "├─ %s %s  %s\n", //nolint:errcheck
				icon, t.nameStyle.Render(a.name), t.timeStyle.Render(elapsed.String()))
		} else {
			frame := t.spinnerStyle.Render(spinnerFrames[t.spinIdx])
			elapsed := time.Since(a.startTime).Round(time.Second)
			status := ""
			if a.status != "" {
				status = t.dimStyle.Render(" [" + a.status + "]")
			}
			line := fmt.Sprintf("├─ %s %s  %s%s",
				frame, t.nameStyle.Render(a.name), t.timeStyle.Render(elapsed.String()), status)
			if t.verbosity >= VerbosityV && a.prompt != "" {
				line += "  " + t.dimStyle.Render(a.prompt)
			}
			fmt.Fprintln(t.w, line) //nolint:errcheck
		}
	}
}

func (t *tracker) renderFinal() {
	for _, a := range t.agents {
		var icon string
		if a.success {
			icon = t.successStyle.Render("✓")
		} else {
			icon = t.failStyle.Render("✗")
		}
		elapsed := time.Since(a.startTime).Round(time.Second)
		line := fmt.Sprintf("├─ %s %s  %s", icon, t.nameStyle.Render(a.name), t.timeStyle.Render(elapsed.String()))
		if t.verbosity >= VerbosityV && a.result != "" {
			line += "  " + t.dimStyle.Render(a.result)
		}
		fmt.Fprintln(t.w, line) //nolint:errcheck
	}
}

func (t *tracker) clearLines(n int) {
	for range n {
		fmt.Fprint(t.w, "\033[A\033[2K") //nolint:errcheck
	}
}

func isTTYCheck(w io.Writer) bool {
	if f, ok := w.(*os.File); ok {
		fi, err := f.Stat()
		if err != nil {
			return false
		}
		return fi.Mode()&os.ModeCharDevice != 0
	}
	return false
}
