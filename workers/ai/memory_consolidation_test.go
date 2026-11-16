/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/llm"
	"github.com/orka-agents/orka/internal/memoryconsolidation"
)

// recordingProvider returns responses in order and records each request.
type recordingProvider struct {
	sequenceProvider
	requests []*llm.CompletionRequest
}

func (p *recordingProvider) Complete(ctx context.Context, req *llm.CompletionRequest) (*llm.CompletionResponse, error) {
	p.requests = append(p.requests, req)
	return p.sequenceProvider.Complete(ctx, req)
}

func consolidationController(
	t *testing.T, begin memoryconsolidation.BeginResponse, submitted *memoryconsolidation.Submission,
) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		switch r.URL.Path {
		case "/internal/v1/memory-consolidation/test-ns/begin":
			require.NoError(t, json.NewEncoder(w).Encode(begin))
		case "/internal/v1/memory-consolidation/test-ns/complete":
			require.NoError(t, json.NewDecoder(r.Body).Decode(submitted))
			require.NoError(t, json.NewEncoder(w).Encode(memoryconsolidation.Report{Status: memoryconsolidation.StatusCompleted,
				Operations: submitted.Operations, Summary: "validated"}))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	t.Setenv("ORKA_CONTROLLER_URL", server.URL)
	t.Setenv("ORKA_TASK_NAMESPACE", "test-ns")
}

func TestRunMemoryConsolidationSubmitsTheModelResult(t *testing.T) {
	input := memoryconsolidation.Input{
		Memories: []memoryconsolidation.InputMemory{{ID: "mem-a", Generation: 2, Content: "Release on Fridays"}},
		Sessions: []memoryconsolidation.InputSession{{Name: "s",
			Messages: []memoryconsolidation.InputMessage{{Role: "user", Content: "hi"}}}},
	}
	var submitted memoryconsolidation.Submission
	consolidationController(t, memoryconsolidation.BeginResponse{Status: memoryconsolidation.StatusRunning,
		Settings: memoryconsolidation.Settings{MaxOperations: 5}, Input: &input}, &submitted)
	args := `{"operations":[{"type":"retire","targets":[{"memoryId":"mem-a","generation":2}],"reason":"stale"}],` +
		`"summary":"one stale memory"}`
	provider := &recordingProvider{sequenceProvider: sequenceProvider{responses: []*llm.CompletionResponse{
		{Content: "I think memory is fine."},
		{ToolCalls: []llm.ToolCall{{ID: "call-1", Name: memoryconsolidation.SubmitToolName,
			Arguments: json.RawMessage(args)}}},
	}}}

	result, err := runMemoryConsolidation(t.Context(), provider, "model", modelSettings{maxTokens: 4096},
		"Focus on releases")
	require.NoError(t, err)
	require.Len(t, provider.requests, 2, "a reply without the tool gets one reminder")
	first := provider.requests[0]
	require.Equal(t, memoryconsolidation.SystemPrompt, first.SystemPrompt, "a run uses Orka's fixed instruction")
	require.Len(t, first.Tools, 1)
	require.Equal(t, memoryconsolidation.SubmitToolName, first.Tools[0].Name, "the result tool is the only tool")
	require.Contains(t, first.Messages[0].Content, "Focus on releases")
	require.Contains(t, first.Messages[0].Content, `"mem-a"`)
	require.Len(t, submitted.Operations, 1)
	require.Equal(t, "retire", submitted.Operations[0].Type)
	var report memoryconsolidation.Report
	require.NoError(t, json.Unmarshal([]byte(result), &report), "the Task result is the controller's report")
	require.Equal(t, "validated", report.Summary)
}

func TestRunMemoryConsolidationFailsWithoutAResult(t *testing.T) {
	input := memoryconsolidation.Input{}
	var submitted memoryconsolidation.Submission
	consolidationController(t, memoryconsolidation.BeginResponse{Status: memoryconsolidation.StatusRunning,
		Input: &input}, &submitted)
	provider := &recordingProvider{sequenceProvider: sequenceProvider{
		responses: []*llm.CompletionResponse{{Content: "no"}, {Content: "still no"}}}}
	_, err := runMemoryConsolidation(t.Context(), provider, "model", modelSettings{}, "")
	require.ErrorContains(t, err, "did not submit", "a run without a result fails, so its Sessions are read again")
}

func TestRunMemoryConsolidationReturnsAFinishedRunsReport(t *testing.T) {
	var submitted memoryconsolidation.Submission
	consolidationController(t, memoryconsolidation.BeginResponse{Status: memoryconsolidation.StatusSkipped,
		Report: &memoryconsolidation.Report{Status: memoryconsolidation.StatusSkipped,
			SkippedReason: "2 Sessions changed"}}, &submitted)
	provider := &recordingProvider{}
	result, err := runMemoryConsolidation(t.Context(), provider, "model", modelSettings{}, "")
	require.NoError(t, err)
	require.Empty(t, provider.requests, "a skipped run never calls the model")
	require.True(t, strings.Contains(result, "2 Sessions changed"))
}

// A consolidation Task needs no prompt: startup passes validation and reaches
// the provider credentials, where an ordinary Task without a prompt stops.
func TestRunAcceptsAConsolidationTaskWithoutAPrompt(t *testing.T) {
	t.Setenv("ORKA_AI_PROVIDER", "nonexistent-provider")
	t.Setenv("ORKA_AI_MODEL", "test-model")
	t.Setenv("ORKA_AI_PROMPT", "")
	err := run("")
	require.ErrorContains(t, err, "ORKA_AI_PROMPT", "an ordinary Task still needs a prompt")

	t.Setenv("ORKA_MEMORY_CONSOLIDATION_MODE", "DryRun")
	err = run("")
	require.Error(t, err)
	require.NotContains(t, err.Error(), "ORKA_AI_PROMPT", "a consolidation run's prompt is optional")
	require.Contains(t, err.Error(), "API key", "startup goes on to the provider")
}

// The controller answers 503 until it has published a worker's Job; a run
// waits for it instead of failing.
func TestMemoryConsolidationClientRetriesWhileTheControllerIsUnavailable(t *testing.T) {
	previous := memoryConsolidationRetryDelay
	memoryConsolidationRetryDelay = time.Millisecond
	t.Cleanup(func() { memoryConsolidationRetryDelay = previous })
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls < 3 {
			http.Error(w, "caller job is not yet published", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"status":"skipped","report":{"status":"skipped"}}`))
	}))
	t.Cleanup(server.Close)
	t.Setenv("ORKA_CONTROLLER_URL", server.URL)
	t.Setenv("ORKA_TASK_NAMESPACE", "test-ns")
	client, err := newMemoryConsolidationClient()
	require.NoError(t, err)
	var begin memoryconsolidation.BeginResponse
	require.NoError(t, client.post(t.Context(), "/begin", struct{}{}, &begin))
	require.Equal(t, 3, calls)
	require.Equal(t, memoryconsolidation.StatusSkipped, begin.Status)

	calls = -100
	err = client.post(t.Context(), "/begin", struct{}{}, &begin)
	require.ErrorContains(t, err, "HTTP 503", "a controller that stays unavailable fails the run")
}
