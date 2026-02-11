/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tui

import (
	"github.com/sozercan/mercan/internal/cli/client"
)

// SSEMsg wraps an SSE event from the chat stream.
type SSEMsg struct {
	Event client.SSEEvent
}

// AgentInfo represents a child agent's status.
type AgentInfo struct {
	Name      string
	Namespace string
	Phase     string // Pending, Running, Succeeded, Failed
	Duration  string
	Summary   string
	Result    string
}

// AgentStatusMsg is sent when child agent statuses are polled.
type AgentStatusMsg struct {
	Agents []AgentInfo
}

// AgentTransitionMsg is sent when an agent changes phase.
type AgentTransitionMsg struct {
	Name     string
	OldPhase string
	NewPhase string
}

// StreamDoneMsg signals the SSE stream has ended.
type StreamDoneMsg struct {
	Usage client.ChatUsage
}

// StreamErrMsg signals a stream error.
type StreamErrMsg struct {
	Err error
}

// TickMsg is sent for periodic updates (spinner animation).
type TickMsg struct{}
