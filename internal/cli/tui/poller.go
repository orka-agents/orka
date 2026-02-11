/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tui

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sozercan/mercan/internal/cli/client"
)

// Poller polls the Mercan API for child task statuses.
type Poller struct {
	client     *client.Client
	namespace  string
	parentTask string
	interval   time.Duration

	// hash-based change detection
	lastHash   string
	lastPhases map[string]string // name -> phase
}

// NewPoller creates a new agent status poller.
func NewPoller(c *client.Client, namespace, parentTask string) *Poller {
	return &Poller{
		client:     c,
		namespace:  namespace,
		parentTask: parentTask,
		interval:   2 * time.Second,
		lastPhases: make(map[string]string),
	}
}

// Poll returns a tea.Cmd that polls for agent statuses once.
// Call this repeatedly (via tea.Tick or after each msg) to keep polling.
func (p *Poller) Poll() tea.Cmd {
	return tea.Tick(p.interval, func(_ time.Time) tea.Msg {
		return pollTickMsg{}
	})
}

type pollTickMsg struct{}

// FetchAgents fetches child task statuses from the API.
// Returns agentPollResult or nil if nothing changed.
func (p *Poller) FetchAgents(ctx context.Context) tea.Msg {
	resp, err := p.client.ListTasks(ctx, p.namespace, 100, "")
	if err != nil {
		return nil
	}

	var items []map[string]any
	if err := json.Unmarshal(resp.Items, &items); err != nil {
		return nil
	}

	// Filter for child tasks of this parent
	var agents []AgentInfo
	for _, item := range items {
		labels := nestedMap(item, "metadata", "labels")
		if labels == nil {
			continue
		}
		parentLabel, _ := labels["mercan.ai/parent-task"].(string)
		if parentLabel != p.parentTask {
			continue
		}

		name := nestedString(item, "metadata", "name")
		ns := nestedString(item, "metadata", "namespace")
		phase := nestedString(item, "status", "phase")

		// Calculate duration
		startStr := nestedString(item, "status", "startTime")
		duration := ""
		if startStr != "" {
			if t, err := time.Parse(time.RFC3339, startStr); err == nil {
				duration = fmt.Sprintf("%ds", int(time.Since(t).Seconds()))
			}
		}

		// Get agent name from label
		agentLabel, _ := labels["mercan.ai/delegated-agent"].(string)
		displayName := agentLabel
		if displayName == "" {
			displayName = name
		}

		// Get summary from status message
		summary := nestedString(item, "status", "message")

		agents = append(agents, AgentInfo{
			Name:      displayName,
			Namespace: ns,
			Phase:     phase,
			Duration:  duration,
			Summary:   summary,
		})
	}

	// Compute hash for change detection
	hashData, _ := json.Marshal(agents)
	hash := fmt.Sprintf("%x", sha256.Sum256(hashData))
	if hash == p.lastHash {
		return nil
	}
	p.lastHash = hash

	// Check for phase transitions
	var transitions []AgentTransitionMsg
	for _, a := range agents {
		oldPhase, exists := p.lastPhases[a.Name]
		if exists && oldPhase != a.Phase {
			transitions = append(transitions, AgentTransitionMsg{
				Name:     a.Name,
				OldPhase: oldPhase,
				NewPhase: a.Phase,
			})
		}
		p.lastPhases[a.Name] = a.Phase
	}

	return agentPollResult{
		Status:      AgentStatusMsg{Agents: agents},
		Transitions: transitions,
	}
}

// agentPollResult is a composite message containing status and transitions.
type agentPollResult struct {
	Status      AgentStatusMsg
	Transitions []AgentTransitionMsg
}

// nestedMap gets a nested map value by traversing keys.
func nestedMap(m map[string]any, keys ...string) map[string]any {
	current := m
	for _, k := range keys {
		v, ok := current[k]
		if !ok {
			return nil
		}
		next, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		current = next
	}
	return current
}

// nestedString gets a nested string value by traversing keys.
func nestedString(m map[string]any, keys ...string) string {
	if len(keys) == 0 {
		return ""
	}
	if len(keys) == 1 {
		s, _ := m[keys[0]].(string)
		return s
	}
	sub := nestedMap(m, keys[:len(keys)-1]...)
	if sub == nil {
		return ""
	}
	s, _ := sub[keys[len(keys)-1]].(string)
	return s
}
