/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
)

// ChatOptions holds configuration for the chat command.
type ChatOptions struct {
	Server    string
	Token     string
	Namespace string
	SessionID string
}

// chatRequest is the request body for POST /api/v1/chat.
type chatRequest struct {
	Message   string `json:"message"`
	SessionID string `json:"sessionId,omitempty"`
	Namespace string `json:"namespace,omitempty"`
}

// sseEventData holds the parsed SSE event data from the server.
type sseEventData struct {
	// status event fields
	SessionID string `json:"sessionId,omitempty"`

	// message event fields
	Content string `json:"content,omitempty"`

	// tool_call event fields
	Name string `json:"name,omitempty"`

	// error event fields
	Error string `json:"error,omitempty"`
}

// RunChat starts an interactive terminal chat loop.
func RunChat(opts ChatOptions) {
	if opts.SessionID == "" {
		opts.SessionID = generateSessionID()
	}
	if opts.Server == "" {
		opts.Server = defaultServer
	}
	if opts.Namespace == "" {
		opts.Namespace = defaultNS
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	fmt.Println("🐟 Mercan Chat")
	fmt.Printf("   Server:    %s\n", opts.Server)
	fmt.Printf("   Session:   %s\n", opts.SessionID)
	fmt.Printf("   Namespace: %s\n", opts.Namespace)
	fmt.Println()
	fmt.Println("Type /help for commands, /quit to exit.")
	fmt.Println()

	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("> ")
		if !scanner.Scan() {
			break
		}

		input := strings.TrimSpace(scanner.Text())
		if input == "" {
			continue
		}

		switch input {
		case "/quit", "/exit":
			fmt.Println("Goodbye!")
			return
		case "/clear":
			opts.SessionID = generateSessionID()
			fmt.Printf("\033[2m✓ New session: %s\033[0m\n\n", opts.SessionID)
			continue
		case "/help":
			printChatHelp()
			continue
		case "/session":
			fmt.Printf("\033[2mSession: %s\033[0m\n\n", opts.SessionID)
			continue
		}

		if err := sendChatMessage(ctx, opts, input); err != nil {
			if ctx.Err() != nil {
				fmt.Println("\nInterrupted.")
				return
			}
			fmt.Printf("\033[31mError: %s\033[0m\n\n", err)
		}
	}
}

func sendChatMessage(ctx context.Context, opts ChatOptions, message string) error {
	body, err := json.Marshal(chatRequest{
		Message:   message,
		SessionID: opts.SessionID,
		Namespace: opts.Namespace,
	})
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, opts.Server+"/api/v1/chat", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if opts.Token != "" {
		req.Header.Set("Authorization", "Bearer "+opts.Token)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("server returned %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	return readSSEStream(resp.Body)
}

func readSSEStream(body io.Reader) error {
	scanner := bufio.NewScanner(body)
	// Increase buffer for large tool results.
	scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024)

	var currentEvent string
	hadContent := false

	for scanner.Scan() {
		line := scanner.Text()

		if after, ok := strings.CutPrefix(line, "event: "); ok {
			currentEvent = after
			continue
		}

		if after, ok := strings.CutPrefix(line, "data: "); ok {
			data := after
			handleSSEEvent(currentEvent, data, &hadContent)
			continue
		}
	}

	if hadContent {
		fmt.Println()
		fmt.Println()
	}

	return scanner.Err()
}

func handleSSEEvent(event, data string, hadContent *bool) {
	var evt sseEventData
	if err := json.Unmarshal([]byte(data), &evt); err != nil {
		return
	}

	switch event {
	case "status":
		// silently acknowledged
	case "message":
		if evt.Content != "" {
			fmt.Print(evt.Content)
			*hadContent = true
		}
	case "tool_call":
		if *hadContent {
			fmt.Println()
			*hadContent = false
		}
		fmt.Printf("\033[2m⚙ Calling %s...\033[0m\n", evt.Name)
	case "tool_result":
		fmt.Printf("\033[2m✓ %s completed\033[0m\n", evt.Name)
	case "error":
		fmt.Printf("\033[31m✗ Error: %s\033[0m\n", evt.Error)
	case "done":
		// stream complete
	}
}

func printChatHelp() {
	fmt.Println("\033[2mCommands:")
	fmt.Println("  /help      Show this help message")
	fmt.Println("  /clear     Start a new session")
	fmt.Println("  /session   Show current session ID")
	fmt.Println("  /quit      Exit chat\033[0m")
	fmt.Println()
}

func generateSessionID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "chat-" + hex.EncodeToString(b)
}
