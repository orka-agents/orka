/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sozercan/mercan/internal/cli/client"
	"github.com/sozercan/mercan/internal/cli/tui"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func newRunCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run [prompt]",
		Short: "Run a task interactively",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			agent, _ := cmd.Flags().GetString("agent")
			session, _ := cmd.Flags().GetString("session")
			model, _ := cmd.Flags().GetString("model")
			provider, _ := cmd.Flags().GetString("provider")

			if agent == "" && provider == "" {
				return fmt.Errorf("either --agent or --provider is required")
			}

			prompt := args[0]
			if prompt == "" {
				return fmt.Errorf("prompt is required")
			}

			// Determine whether to use TUI mode
			isTTY := term.IsTerminal(int(os.Stdout.Fd()))
			useTUI, _ := cmd.Flags().GetBool("tui")
			if !cmd.Flags().Changed("tui") {
				useTUI = isTTY
			}

			c := newClientFromCmd(cmd)
			c.HTTPClient.Timeout = 0

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer stop()

			req := client.ChatRequest{
				Message:   prompt,
				SessionID: session,
				Provider:  provider,
				Model:     model,
				AgentRef:  agent,
				Namespace: c.Namespace,
			}

			events, err := c.StreamChat(ctx, req)
			if err != nil {
				return fmt.Errorf("connecting to chat: %w", err)
			}

			if useTUI {
				return runTUI(events)
			}

			return runPlainText(events)
		},
	}

	cmd.Flags().String("agent", "", "Agent name to use")
	cmd.Flags().String("session", "", "Session ID for conversation continuity")
	cmd.Flags().String("model", "", "Model name (e.g. gpt-4o)")
	cmd.Flags().String("provider", "", "Provider name (e.g. openai)")
	cmd.Flags().Bool("tui", false, "Force TUI mode on/off (auto-detects TTY by default)")

	return cmd
}

// runTUI runs the Bubbletea TUI and prints a summary after exit.
func runTUI(events <-chan client.SSEEvent) error {
	m := tui.New(events)
	p := tea.NewProgram(m, tea.WithAltScreen())

	finalModel, err := p.Run()
	if err != nil {
		return fmt.Errorf("TUI error: %w", err)
	}

	if fm, ok := finalModel.(tui.Model); ok && fm.IsDone() {
		usage := fm.Usage()
		fmt.Printf("Done in %s\n", usage.Duration)
		fmt.Printf("Tokens: %d in, %d out\n", usage.InputTokens, usage.OutputTokens)
		fmt.Printf("Tool calls: %d\n", usage.ToolCalls)
	}

	return nil
}

// runPlainText runs the existing plain-text streaming output.
func runPlainText(events <-chan client.SSEEvent) error {
	for ev := range events {
		switch ev.Type {
		case client.EventStatus:
			var d client.StatusEventData
			if err := json.Unmarshal(ev.Data, &d); err == nil {
				fmt.Printf("Connected to %s/%s (session: %s)\n", d.Provider, d.Model, d.SessionID)
			}

		case client.EventMessage:
			var d client.MessageEventData
			if err := json.Unmarshal(ev.Data, &d); err == nil {
				fmt.Print(d.Content)
			}

		case client.EventToolCall:
			var d client.ToolCallEventData
			if err := json.Unmarshal(ev.Data, &d); err == nil {
				summary := ""
				if d.Name == "delegate_task" {
					var a map[string]interface{}
					if err := json.Unmarshal(d.Args, &a); err == nil {
						if agentName, ok := a["agent"].(string); ok {
							summary = " → " + agentName
						}
					}
				}
				fmt.Printf("\n⚡ %s%s\n", d.Name, summary)
			}

		case client.EventToolResult:
			var d client.ToolResultEventData
			if err := json.Unmarshal(ev.Data, &d); err == nil {
				result := string(d.Result)
				if len(result) > 100 {
					result = result[:100] + "…"
				}
				fmt.Printf("✓ %s\n", d.Name)
			}

		case client.EventError:
			var d client.ErrorEventData
			if err := json.Unmarshal(ev.Data, &d); err == nil {
				fmt.Fprintf(os.Stderr, "Error: %s\n", d.Error)
			}

		case client.EventDone:
			var d client.DoneEventData
			if err := json.Unmarshal(ev.Data, &d); err == nil {
				fmt.Printf("\nDone in %s (%d in, %d out, %d tool calls)\n",
					d.Usage.Duration, d.Usage.InputTokens, d.Usage.OutputTokens, d.Usage.ToolCalls)
			}
		}
	}

	return nil
}
