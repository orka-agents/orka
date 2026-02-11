/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/sozercan/mercan/internal/cli/client"
)

type focusPane int

const (
	focusCoordinator focusPane = iota
	focusAgents
)

// Model is the root Bubbletea model.
type Model struct {
	// Layout
	width, height int
	focus         focusPane

	// Coordinator pane
	coordinatorContent strings.Builder
	coordinatorVP      viewport.Model

	// Agent panel
	agents      []AgentInfo
	agentCursor int

	// Peek overlay
	peekVisible bool
	peekContent string
	peekVP      viewport.Model

	// State
	spinner   spinner.Model
	streaming bool
	done      bool
	usage     client.ChatUsage
	startTime time.Time

	// For event channel
	eventCh <-chan client.SSEEvent

	// Poller for child agent statuses
	poller *Poller
}

// New creates a new TUI model.
func New(eventCh <-chan client.SSEEvent) Model {
	return Model{
		spinner:       newSpinner(),
		streaming:     true,
		startTime:     time.Now(),
		eventCh:       eventCh,
		coordinatorVP: viewport.New(80, 20),
		peekVP:        viewport.New(80, 20),
	}
}

// SetPoller attaches a Poller for child agent status updates.
func (m *Model) SetPoller(p *Poller) {
	m.poller = p
}

// Init starts the spinner and begins reading SSE events.
func (m Model) Init() tea.Cmd {
	cmds := []tea.Cmd{m.spinner.Tick, waitForSSE(m.eventCh), tea.WindowSize()}
	if m.poller != nil {
		cmds = append(cmds, m.poller.Poll())
	}
	return tea.Batch(cmds...)
}

// waitForSSE reads one event from the SSE channel.
func waitForSSE(ch <-chan client.SSEEvent) tea.Cmd {
	return func() tea.Msg {
		event, ok := <-ch
		if !ok {
			return StreamDoneMsg{}
		}
		return SSEMsg{Event: event}
	}
}

// Update handles messages.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.resizeViewports()

	case tea.KeyMsg:
		if m.peekVisible {
			switch msg.String() {
			case "esc":
				m.peekVisible = false
				return m, nil
			default:
				var cmd tea.Cmd
				m.peekVP, cmd = m.peekVP.Update(msg)
				return m, cmd
			}
		}

		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "tab":
			if m.focus == focusCoordinator {
				m.focus = focusAgents
			} else {
				m.focus = focusCoordinator
			}
		case "up", "k":
			if m.focus == focusAgents && m.agentCursor > 0 {
				m.agentCursor--
			}
		case "down", "j":
			if m.focus == focusAgents && m.agentCursor < len(m.agents)-1 {
				m.agentCursor++
			}
		case "enter":
			if m.focus == focusAgents && len(m.agents) > 0 {
				agent := m.agents[m.agentCursor]
				m.peekContent = agent.Result
				if m.peekContent == "" {
					m.peekContent = "(no result yet)"
				}
				m.peekVP.SetContent(m.peekContent)
				m.peekVP.GotoTop()
				m.peekVisible = true
			}
		}

	case SSEMsg:
		m.handleSSEEvent(msg.Event)
		wrapped := lipgloss.NewStyle().Width(m.coordinatorVP.Width).Render(m.coordinatorContent.String())
		m.coordinatorVP.SetContent(wrapped)
		m.coordinatorVP.GotoBottom()
		cmds = append(cmds, waitForSSE(m.eventCh))

	case AgentStatusMsg:
		m.agents = msg.Agents
		if m.agentCursor >= len(m.agents) && len(m.agents) > 0 {
			m.agentCursor = len(m.agents) - 1
		}

	case AgentTransitionMsg:
		line := notificationStyle.Render(fmt.Sprintf("ℹ %s: %s → %s", msg.Name, msg.OldPhase, msg.NewPhase))
		m.coordinatorContent.WriteString(line + "\n")
		wrapped := lipgloss.NewStyle().Width(m.coordinatorVP.Width).Render(m.coordinatorContent.String())
		m.coordinatorVP.SetContent(wrapped)
		m.coordinatorVP.GotoBottom()

	case pollTickMsg:
		if m.poller != nil {
			poller := m.poller
			cmds = append(cmds, func() tea.Msg {
				return poller.FetchAgents(context.Background())
			})
		}

	case agentPollResult:
		m.agents = msg.Status.Agents
		if m.agentCursor >= len(m.agents) && len(m.agents) > 0 {
			m.agentCursor = len(m.agents) - 1
		}
		for _, t := range msg.Transitions {
			line := notificationStyle.Render(fmt.Sprintf("ℹ %s: %s → %s", t.Name, t.OldPhase, t.NewPhase))
			m.coordinatorContent.WriteString(line + "\n")
		}
		if len(msg.Transitions) > 0 {
			wrapped := lipgloss.NewStyle().Width(m.coordinatorVP.Width).Render(m.coordinatorContent.String())
			m.coordinatorVP.SetContent(wrapped)
			m.coordinatorVP.GotoBottom()
		}
		// Continue polling if streaming and agents are still active
		if m.streaming && m.poller != nil {
			hasActive := false
			for _, a := range m.agents {
				if a.Phase == "Pending" || a.Phase == "Running" {
					hasActive = true
					break
				}
			}
			if hasActive {
				cmds = append(cmds, m.poller.Poll())
			}
		}

	case StreamDoneMsg:
		m.done = true
		m.streaming = false
		m.usage = msg.Usage

	case StreamErrMsg:
		m.coordinatorContent.WriteString(fmt.Sprintf("Error: %v\n", msg.Err))
		wrapped := lipgloss.NewStyle().Width(m.coordinatorVP.Width).Render(m.coordinatorContent.String())
		m.coordinatorVP.SetContent(wrapped)
		m.coordinatorVP.GotoBottom()
		m.done = true
		m.streaming = false

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		cmds = append(cmds, cmd)
	}

	return m, tea.Batch(cmds...)
}

// handleSSEEvent processes an SSE event and appends to coordinator content.
func (m *Model) handleSSEEvent(event client.SSEEvent) {
	switch event.Type {
	case client.EventStatus:
		var data client.StatusEventData
		if json.Unmarshal(event.Data, &data) == nil {
			m.coordinatorContent.WriteString(fmt.Sprintf("Connected to %s/%s\n", data.Provider, data.Model))
		}

	case client.EventMessage:
		var data client.MessageEventData
		if json.Unmarshal(event.Data, &data) == nil {
			m.coordinatorContent.WriteString(data.Content)
		}

	case client.EventToolCall:
		var data client.ToolCallEventData
		if json.Unmarshal(event.Data, &data) == nil {
			line := toolCallStyle.Render("⚡ ") + toolNameStyle.Render(data.Name)
			m.coordinatorContent.WriteString(line + "\n")
		}

	case client.EventToolResult:
		var data client.ToolResultEventData
		if json.Unmarshal(event.Data, &data) == nil {
			line := toolCallStyle.Render("✓ ") + toolNameStyle.Render(data.Name)
			m.coordinatorContent.WriteString(line + "\n")
		}

	case client.EventError:
		var data client.ErrorEventData
		if json.Unmarshal(event.Data, &data) == nil {
			m.coordinatorContent.WriteString(fmt.Sprintf("Error: %s\n", data.Error))
		}

	case client.EventDone:
		var data client.DoneEventData
		if json.Unmarshal(event.Data, &data) == nil {
			m.done = true
			m.streaming = false
			m.usage = data.Usage
		}
	}
}

// resizeViewports adjusts viewport sizes to the terminal dimensions.
func (m *Model) resizeViewports() {
	// lipgloss Width includes padding but not borders.
	// Padding(0,1) = 2 horizontal, RoundedBorder = 2 horizontal.
	// .Width(w) → content area = w-2 (padding), rendered = w+2 (borders).
	// We want rendered = terminal width → w = m.width - 2.
	paneWidth := m.width - 2
	contentWidth := paneWidth - 2 // subtract horizontal padding

	// .Height(h) → content = h (no vertical padding), rendered = h+2 (borders).
	// Total: (coordH+2) + (agentH+2) + 1 (status bar) = m.height
	usable := m.height - 5
	coordH := usable * 60 / 100
	agentH := usable - coordH

	if coordH < 3 {
		coordH = 3
	}
	if agentH < 3 {
		agentH = 3
	}

	// Viewport height subtracts 1 for the title line inside each pane
	vpHeight := coordH - 1
	if vpHeight < 1 {
		vpHeight = 1
	}

	m.coordinatorVP.Width = contentWidth
	m.coordinatorVP.Height = vpHeight
	m.peekVP.Width = contentWidth - 6
	m.peekVP.Height = m.height - 8
}

// View renders the TUI.
func (m Model) View() string {
	if m.width == 0 {
		return "Loading..."
	}

	if m.peekVisible {
		return m.viewPeek()
	}

	return m.viewMain()
}

// viewPeek renders the peek overlay.
func (m Model) viewPeek() string {
	title := titleStyle.Render("Agent Result") + "  (esc to close)"
	content := title + "\n\n" + m.peekVP.View()
	return peekBorderStyle.Width(m.width - 6).Render(content)
}

// viewMain renders the two-pane layout.
func (m Model) viewMain() string {
	paneWidth := m.width - 2
	usable := m.height - 5
	coordH := usable * 60 / 100
	agentH := usable - coordH

	if coordH < 3 {
		coordH = 3
	}
	if agentH < 3 {
		agentH = 3
	}

	// Coordinator pane
	coordTitle := titleStyle.Render("🤖 Coordinator")
	coordContent := coordTitle + "\n" + m.coordinatorVP.View()
	coordPane := coordinatorBorderStyle.Width(paneWidth).Height(coordH).Render(coordContent)

	// Agent panel
	agentTitle := m.agentPanelTitle()
	agentContent := agentTitle + "\n" + m.renderAgentList(agentH-2)
	agentPane := agentBorderStyle.Width(paneWidth).Height(agentH).Render(agentContent)

	// Status bar
	statusBar := m.renderStatusBar()

	return lipgloss.JoinVertical(lipgloss.Left, coordPane, agentPane, statusBar)
}

// agentPanelTitle returns the agent panel title with counts.
func (m Model) agentPanelTitle() string {
	doneCount := 0
	for _, a := range m.agents {
		if a.Phase == "Succeeded" || a.Phase == "Failed" {
			doneCount++
		}
	}
	total := len(m.agents)
	if total == 0 {
		return titleStyle.Render("Agents")
	}
	return titleStyle.Render(fmt.Sprintf("Agents (%d/%d done)", doneCount, total))
}

// renderAgentList renders the scrollable agent list.
func (m Model) renderAgentList(maxLines int) string {
	if len(m.agents) == 0 {
		return notificationStyle.Render("  No agents delegated yet")
	}

	var lines []string
	for i, agent := range m.agents {
		indicator := statusIndicator(agent.Phase)
		name := agent.Name
		info := fmt.Sprintf("  %s  %s", agent.Phase, agent.Duration)
		if agent.Summary != "" {
			info += "  " + agent.Summary
		}

		line := fmt.Sprintf("%s %s%s", indicator, name, info)
		if i == m.agentCursor && m.focus == focusAgents {
			line = selectedStyle.Render(line)
		}
		lines = append(lines, line)
	}

	// Truncate to fit available height
	if len(lines) > maxLines && maxLines > 0 {
		lines = lines[:maxLines]
	}

	return strings.Join(lines, "\n")
}

// Usage returns the final usage statistics after the stream completes.
func (m Model) Usage() client.ChatUsage {
	return m.usage
}

// IsDone returns whether the stream has completed.
func (m Model) IsDone() bool {
	return m.done
}

// statusIndicator returns the status icon for an agent phase.
func statusIndicator(phase string) string {
	switch phase {
	case "Pending":
		return statusPending
	case "Running":
		return statusRunning
	case "Succeeded":
		return statusSucceeded
	case "Failed":
		return statusFailed
	default:
		return statusPending
	}
}

// renderStatusBar renders the bottom status bar.
func (m Model) renderStatusBar() string {
	var parts []string

	if m.streaming {
		parts = append(parts, m.spinner.View()+" streaming")
	} else if m.done {
		elapsed := time.Since(m.startTime).Truncate(time.Second)
		parts = append(parts, fmt.Sprintf("✓ done (%s)", elapsed))
		if m.usage.LLMCalls > 0 {
			parts = append(parts, fmt.Sprintf("tokens: %d→%d", m.usage.InputTokens, m.usage.OutputTokens))
		}
	}

	parts = append(parts, "q quit • ↑↓ select • enter peek • tab pane")

	return statusBarStyle.Render(strings.Join(parts, "  │  "))
}
