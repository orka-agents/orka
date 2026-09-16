/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/llm"
	"github.com/stretchr/testify/require"
)

func TestResponsesPinnedSDKSchema(t *testing.T) {
	python := os.Getenv("ORKA_RESPONSES_INTEROP_PYTHON")
	if python == "" {
		t.Skip("set ORKA_RESPONSES_INTEROP_PYTHON to validate HTTP output with pinned SDK schemas")
	}
	var payloads []any
	for _, mode := range []string{"text", "tool", "failure"} {
		_, app := setupResponsesHTTP(t, func(w http.ResponseWriter, _ *http.Request) {
			switch mode {
			case "failure":
				upstreamSSE(w, []map[string]any{{"type": "response.output_text.delta", "delta": "partial"}, {"type": "response.failed", "response": map[string]any{"status": "failed"}}})
			case "text":
				upstreamSSE(w, []map[string]any{{"type": "response.output_text.delta", "delta": "hello"}, {"type": "response.completed", "response": map[string]any{"status": "completed"}}})
			case "tool":
				response := responsesFixtureResponse("", llm.ToolCall{ID: "client-call", Name: "client_tool", Arguments: json.RawMessage(`{}`)})
				upstreamSSE(w, []map[string]any{{"type": "response.completed", "response": response}})
			}
		})
		status, body := requestResponses(t, app, `{"model":"fixture/test-model","store":false,"stream":true,"input":"hello"}`, true)
		require.Equal(t, 200, status, string(body))
		for _, event := range parseResponsesSSE(t, body) {
			payloads = append(payloads, event)
		}
	}
	_, app := setupResponsesHTTP(t, func(w http.ResponseWriter, _ *http.Request) { upstreamResponse(w, "hello") })
	status, body := requestResponses(t, app, `{"model":"fixture/test-model","store":false,"input":"hello"}`, true)
	require.Equal(t, 200, status, string(body))
	var response map[string]any
	require.NoError(t, json.Unmarshal(body, &response))
	payloads = append(payloads, response)
	validator, err := filepath.Abs("../../scripts/fixtures/agent-framework-responses/validate.py")
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, python, validator)
	data, err := json.Marshal(payloads)
	require.NoError(t, err)
	command.Stdin = bytes.NewReader(data)
	output, err := command.CombinedOutput()
	require.NoError(t, err, string(output))
	t.Log(string(output))
}
