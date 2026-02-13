/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	"github.com/charmbracelet/glamour"
	"github.com/spf13/cobra"

	"github.com/sozercan/mercan/internal/cli/client"
)

func newRunCmd() *cobra.Command {
	var agent, session, model, provider string
	var verbose int

	cmd := &cobra.Command{
		Use:   "run [prompt]",
		Short: "Chat with the Mercan AI assistant",
		Long: `Ollama-style chat interface.

  One-shot:    mercan run "explain kubernetes pods"
  Interactive: mercan run
  Piped:       echo "fix bugs" | mercan run`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if agent != "" && provider != "" {
				return fmt.Errorf("--agent and --provider are mutually exclusive")
			}

			c := newClientFromCmd(cmd)

			// Pre-flight check
			cfg, err := c.GetChatConfig(context.Background())
			if err != nil {
				return fmt.Errorf("cannot reach server: %w", err)
			}
			if !cfg.Enabled {
				return fmt.Errorf("chat is disabled on this server")
			}

			// Determine provider/model info
			if agent == "" && provider == "" {
				provider = cfg.Provider
			}
			displayModel := cfg.Model
			if model != "" {
				displayModel = model
			}
			displayProvider := provider
			if agent != "" {
				displayProvider = "agent:" + agent
			}

			// Generate session ID if not provided
			if session == "" {
				b := make([]byte, 8)
				_, _ = rand.Read(b)
				session = "chat-" + hex.EncodeToString(b)
			}

			stderrTTY := isTTYCheck(os.Stderr)
			stdoutTTY := isTTYCheck(os.Stdout)

			fmt.Fprintf(os.Stderr, "Using %s/%s\n", displayProvider, displayModel)
			fmt.Fprintf(os.Stderr, "Session: %s (use --session to continue)\n\n", session)

			// Determine prompt source
			prompt := strings.Join(args, " ")

			// Check stdin
			if prompt == "" {
				stdinStat, _ := os.Stdin.Stat()
				if stdinStat.Mode()&os.ModeCharDevice == 0 {
					// Piped stdin
					data, err := io.ReadAll(os.Stdin)
					if err != nil {
						return fmt.Errorf("read stdin: %w", err)
					}
					prompt = strings.TrimSpace(string(data))
				}
			}

			if prompt != "" {
				// One-shot mode
				return runOneShot(c, client.ChatRequest{
					Message:   prompt,
					SessionID: session,
					Namespace: c.Namespace,
					Agent:     agent,
					Model:     model,
					Provider:  provider,
				}, verbose, stderrTTY, stdoutTTY)
			}

			// Interactive REPL mode
			return runREPL(c, session, agent, model, provider, verbose, stderrTTY, stdoutTTY)
		},
	}

	cmd.Flags().StringVar(&agent, "agent", "", "Agent to use for the task")
	cmd.Flags().StringVar(&session, "session", "", "Resume a specific session")
	cmd.Flags().StringVar(&model, "model", "", "Model to use")
	cmd.Flags().StringVar(&provider, "provider", "", "Provider to use")
	cmd.Flags().CountVarP(&verbose, "verbose", "v", "Verbosity level (-v, -vv)")

	return cmd
}

func runOneShot(c *client.Client, req client.ChatRequest, verbosity int, stderrTTY, stdoutTTY bool) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	code := streamChat(ctx, c, req, verbosity, stderrTTY, stdoutTTY)
	if code != 0 {
		os.Exit(code)
	}
	return nil
}

func runREPL(c *client.Client, session, agent, model, provider string, verbosity int, stderrTTY, stdoutTTY bool) error {
	fmt.Fprintln(os.Stderr, "Type /help for commands, /quit to exit.")
	fmt.Fprintln(os.Stderr)

	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Fprint(os.Stderr, ">>> ")
		if !scanner.Scan() {
			break
		}

		input := strings.TrimSpace(scanner.Text())
		if input == "" {
			continue
		}

		switch input {
		case "/quit", "/exit":
			fmt.Fprintln(os.Stderr, "Goodbye!")
			return nil
		case "/clear":
			b := make([]byte, 8)
			_, _ = rand.Read(b)
			session = "chat-" + hex.EncodeToString(b)
			fmt.Fprintf(os.Stderr, "New session: %s\n\n", session)
			continue
		case "/help":
			fmt.Fprintln(os.Stderr, "Commands:")
			fmt.Fprintln(os.Stderr, "  /help      Show this help")
			fmt.Fprintln(os.Stderr, "  /clear     Start a new session")
			fmt.Fprintln(os.Stderr, "  /session   Show current session ID")
			fmt.Fprintln(os.Stderr, "  /quit      Exit")
			fmt.Fprintln(os.Stderr)
			continue
		case "/session":
			fmt.Fprintf(os.Stderr, "Session: %s\n\n", session)
			continue
		}

		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)

		req := client.ChatRequest{
			Message:   input,
			SessionID: session,
			Namespace: c.Namespace,
			Agent:     agent,
			Model:     model,
			Provider:  provider,
		}

		code := streamChat(ctx, c, req, verbosity, stderrTTY, stdoutTTY)
		cancel()

		if code == 2 {
			fmt.Fprintln(os.Stderr, "\nLLM error occurred, but you can continue chatting.")
		}
		fmt.Fprintln(os.Stdout)
	}
	return nil
}

func streamChat(ctx context.Context, c *client.Client, req client.ChatRequest, verbosity int, stderrTTY, stdoutTTY bool) int {
	reader, resp, err := c.StreamChat(ctx, req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	defer resp.Body.Close() //nolint:errcheck

	t := newTracker(os.Stderr, verbosity, stderrTTY)
	t.startSpinner()
	defer t.stop()

	var contentBuf strings.Builder
	hadContent := false
	thinking := true

	// Show a thinking indicator
	if stderrTTY {
		fmt.Fprint(os.Stderr, "⠋ Thinking…")
	}

	for {
		evt, ok := reader.Next()
		if !ok {
			break
		}

		// Clear thinking indicator on first real event
		if thinking {
			if stderrTTY {
				fmt.Fprint(os.Stderr, "\r\033[2K")
			}
			thinking = false
		}

		// Feed to tracker
		t.handleEvent(evt)

		var data client.SSEEventData
		if err := json.Unmarshal([]byte(evt.Data), &data); err != nil {
			continue
		}

		switch evt.Event {
		case "status":
			if data.SessionID != "" {
				// Session acknowledged
			}
		case "message":
			if data.Content != "" {
				fmt.Fprint(os.Stdout, data.Content)
				contentBuf.WriteString(data.Content)
				hadContent = true
			}
		case "tool_call":
			if hadContent {
				fmt.Fprintln(os.Stdout)
				hadContent = false
			}
			// Show tool activity (tracker handles agent-specific display)
			switch data.Name {
			case "create_agent_task", "delegate_task":
				// Handled by tracker — show a brief note
				var args map[string]any
				if json.Unmarshal([]byte(data.Args), &args) == nil {
					agentName, _ := args["agentRef"].(string)
					if agentName == "" {
						agentName, _ = args["agent"].(string)
					}
					if verbosity < VerbosityVV {
						fmt.Fprintf(os.Stderr, "⚙ Delegating to %s\n", agentName)
					}
				}
			case "check_task_progress":
				// Suppressed — tracker shows status
			case "fetch_task_output":
				if verbosity >= VerbosityV {
					fmt.Fprintf(os.Stderr, "⚙ Fetching result\n")
				}
			default:
				if verbosity < VerbosityVV {
					fmt.Fprintf(os.Stderr, "⚙ %s\n", data.Name)
				}
			}
		case "tool_result":
			switch data.Name {
			case "check_task_progress":
				// Show task phase update inline
				var result map[string]any
				if json.Unmarshal(data.Result, &result) == nil {
					if phase, ok := result["phase"].(string); ok {
						taskName, _ := result["name"].(string)
						elapsed, _ := result["duration"].(string)
						if taskName == "" {
							taskName = "task"
						}
						if verbosity < VerbosityVV {
							fmt.Fprintf(os.Stderr, "  ↻ %s: %s %s\n", taskName, phase, elapsed)
						}
					}
				}
			case "fetch_task_output":
				if verbosity >= VerbosityV {
					fmt.Fprintf(os.Stderr, "✓ Result received\n")
				}
			default:
				if verbosity < VerbosityVV && verbosity >= VerbosityV {
					fmt.Fprintf(os.Stderr, "✓ %s\n", data.Name)
				}
			}
		case "error":
			fmt.Fprintf(os.Stderr, "\n✗ Error: %s\n", data.Error)
			return 2
		case "done":
			if hadContent {
				fmt.Fprintln(os.Stdout)
			}
			// Re-render with glamour if TTY and we have content
			if stdoutTTY && contentBuf.Len() > 0 {
				rendered := renderMarkdown(contentBuf.String())
				if rendered != "" {
					// Move up and clear the raw output, then print rendered
					lines := strings.Count(contentBuf.String(), "\n") + 1
					for i := 0; i < lines; i++ {
						fmt.Fprint(os.Stdout, "\033[A\033[2K")
					}
					fmt.Fprint(os.Stdout, rendered)
				}
			}
			return 0
		}
	}

	if err := reader.Err(); err != nil && ctx.Err() == nil {
		fmt.Fprintf(os.Stderr, "\nStream error: %v\n", err)
		return 1
	}

	if hadContent {
		fmt.Fprintln(os.Stdout)
	}
	return 0
}

// renderMarkdown renders Markdown text using glamour.
func renderMarkdown(text string) string {
	r, err := glamour.NewTermRenderer(
		glamour.WithAutoStyle(),
		glamour.WithWordWrap(100),
	)
	if err != nil {
		return ""
	}
	out, err := r.Render(text)
	if err != nil {
		return ""
	}
	return out
}
