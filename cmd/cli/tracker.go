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

// Verbosity levels for the tracker.
const (
	VerbosityDefault = 0
	VerbosityV       = 1
	VerbosityVV      = 2
)

// Tool names used in tracker event handling.
const (
	toolCheckTaskProgress = "check_task_progress"
	toolWaitForTask       = "wait_for_task"
	toolFetchTaskOutput   = "fetch_task_output"
	statusDone            = "done"
)

// agentEntry tracks one delegated sub-agent.
type agentEntry struct {
	name      string
	taskName  string
	prompt    string
	status    string
	startTime time.Time
	done      bool
	success   bool
	result    string
}

// tracker manages the live subagent status panel.
type tracker struct {
	mu        sync.Mutex
	agents    []*agentEntry
	verbosity int
	w         io.Writer
	isTTY     bool
	spinIdx   int
	stopCh    chan struct{}
	stopped   bool

	// styles
	spinnerStyle lipgloss.Style
	nameStyle    lipgloss.Style
	successStyle lipgloss.Style
	failStyle    lipgloss.Style
	dimStyle     lipgloss.Style
	timeStyle    lipgloss.Style
}

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// newTracker creates a new agent tracker.
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

// startSpinner begins the spinner animation goroutine.
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

// stop stops the spinner goroutine.
func (t *tracker) stop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.stopped {
		t.stopped = true
		close(t.stopCh)
		// Mark remaining agents as done (stream completed normally)
		for _, a := range t.agents {
			if !a.done {
				a.done = true
				a.success = true
				a.status = statusDone
			}
		}
		// Clear spinner lines and render final state
		if t.isTTY && len(t.agents) > 0 {
			t.clearLines(len(t.agents))
			t.renderFinal()
		}
	}
}

// handleEvent processes SSE events for agent tracking.
func (t *tracker) handleEvent(evt client.SSEEvent) {
	var data client.SSEEventData
	if err := json.Unmarshal([]byte(evt.Data), &data); err != nil {
		return
	}

	switch evt.Event {
	case "tool_call":
		switch data.Name {
		case "delegate_task", "create_agent_task":
			t.addAgent(data)
		case toolCheckTaskProgress, toolWaitForTask:
			// Show that we're polling a task
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

// addAgent adds a new agent to the tracker.
func (t *tracker) addAgent(data client.SSEEventData) {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Parse agent name from args
	var args map[string]any
	agentName := "agent"
	taskName := ""
	prompt := ""
	if err := json.Unmarshal(data.Args, &args); err == nil {
		// delegate_task uses "agent", create_agent_task uses "agentRef"
		if name, ok := args["agent"].(string); ok {
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
		name:      agentName,
		taskName:  taskName,
		prompt:    prompt,
		status:    "starting",
		startTime: time.Now(),
	}
	t.agents = append(t.agents, entry)

	if t.isTTY {
		// Will be rendered by spinner
	} else {
		// Static line for non-TTY
		line := fmt.Sprintf("├─ ◦ %s", agentName)
		if t.verbosity >= VerbosityV && prompt != "" {
			line += fmt.Sprintf("  %s", prompt)
		}
		fmt.Fprintln(t.w, line) //nolint:errcheck
	}
}

// completeAgents marks all pending agents as done.
func (t *tracker) completeAgents(_ client.SSEEventData) {
	t.mu.Lock()
	defer t.mu.Unlock()

	for _, a := range t.agents {
		if !a.done {
			a.done = true
			a.success = true // Default success; real status parsed from result
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

// updateAgentStatus updates the status text for a running agent.
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

// updateAgentFromProgress updates agent status from check_task_progress result.
func (t *tracker) updateAgentFromProgress(data client.SSEEventData) {
	t.mu.Lock()
	defer t.mu.Unlock()

	var result map[string]any
	if err := json.Unmarshal(data.Result, &result); err == nil {
		phase, _ := result["phase"].(string)
		taskName, _ := result["name"].(string)
		if phase == "" {
			return
		}
		for _, a := range t.agents {
			if !a.done && (a.taskName == taskName || taskName == "") {
				a.status = phase
				switch phase {
				case "Succeeded":
					a.done = true
					a.success = true
				case "Failed":
					a.done = true
					a.success = false
				}
				break
			}
		}
	}
}

// completeAgentByName marks an agent as done when its output is fetched.
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

// render draws the current state (called by spinner goroutine, must hold mu).
func (t *tracker) render() {
	if len(t.agents) == 0 {
		return
	}

	// Move cursor up to overwrite previous render
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

// renderFinal draws the completed state.
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

// clearLines clears n lines above the cursor.
func (t *tracker) clearLines(n int) {
	for range n {
		fmt.Fprint(t.w, "\033[A\033[2K") //nolint:errcheck
	}
}

// isTTYCheck returns true if the given writer is a TTY.
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
