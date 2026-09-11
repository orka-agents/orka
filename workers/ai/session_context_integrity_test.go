package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/llm"
	"github.com/orka-agents/orka/internal/redact"
	"github.com/orka-agents/orka/internal/sessioncontext"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/worker"
	"github.com/orka-agents/orka/internal/workerenv"
	"github.com/orka-agents/orka/workers/common"
)

func TestSessionContextRedactsCredentialsLoadedDuringExecution(t *testing.T) {
	f := newWorkerContextFixture(t, 32768)
	const providerValue = "mounted-provider-placeholder"
	const toolValue = "opaque-tool-placeholder"
	require.Equal(t, providerValue, sanitizeCheckpointText(providerValue))
	require.Equal(t, toolValue, sanitizeCheckpointText(toolValue))
	_, err := f.store.AppendContextMessages(context.Background(), f.write, []store.SessionMessage{
		{ID: "prior-result", Role: "assistant", Content: "Earlier output for " + toolValue},
	})
	require.NoError(t, err)
	ctx := redact.WithTrackedSecrets(context.Background())
	redact.TrackSecrets(ctx, providerValue)
	current := llm.Message{Role: "user", Content: "Inspect the record for " + providerValue}
	state, active, err := newWorkerSessionContext(ctx, []llm.Message{current})
	require.NoError(t, err)
	require.Equal(t, current.Content, active[len(active)-1].Content)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+toolValue {
			t.Error("custom tool did not receive its configured credential")
		}
		_, _ = fmt.Fprint(w, "Result for "+toolValue)
	}))
	t.Cleanup(server.Close)
	executor := worker.NewToolExecutor()
	executor.SetAuthSecretValue("http-auth", "authref", toolValue)
	tool := &corev1alpha1.Tool{Spec: corev1alpha1.ToolSpec{HTTP: &corev1alpha1.HTTPExecution{
		URL: server.URL, AuthSecretRef: &corev1alpha1.SecretKeySelector{Name: "http-auth", Key: "authref"},
	}}}
	output, err := executor.Execute(ctx, tool, nil)
	require.NoError(t, err)
	require.NotContains(t, output, toolValue, "the HTTP executor also redacts its immediate result")
	require.Contains(t, redact.TrackedSecrets(ctx), toolValue)
	require.Empty(t, redact.TrackedSecrets(redact.WithTrackedSecrets(context.Background())))

	arguments := json.RawMessage(`{"value":"\u006fpaque-tool-placeholder"}`)
	message := llm.Message{Role: "assistant", Content: providerValue + " " + toolValue,
		ToolCalls: []llm.ToolCall{{ID: "loaded-call", Name: "inspect", Arguments: arguments}}}
	saved, err := state.persist(ctx, message)
	require.NoError(t, err)
	require.Equal(t, arguments, message.ToolCalls[0].Arguments)
	require.JSONEq(t, `{"value":"[REDACTED]"}`, string(saved.ToolCalls[0].Arguments))
	result, err := state.persist(ctx, llm.Message{Role: "tool", ToolCallID: "loaded-call",
		Name: "inspect", Content: providerValue + " " + toolValue})
	require.NoError(t, err)
	active = append(active, saved, result)
	for _, id := range []string{state.current.ID, saved.ID, result.ID} {
		page, readErr := f.store.ReadSessionHistory(ctx, store.SessionHistoryRead{
			Namespace: f.write.Namespace, SessionName: f.write.SessionName, MessageID: id, Limit: 4096,
		})
		require.NoError(t, readErr)
		require.NotContains(t, page.Data, providerValue)
		require.NotContains(t, page.Data, toolValue)
		require.Contains(t, page.Data, "[REDACTED]")
	}
	for _, value := range []string{providerValue, toolValue} {
		draft, marshalErr := json.Marshal(checkpointDraft{Goal: checkpointClaim{
			Text: value, Sources: []string{state.current.ID},
		}})
		require.NoError(t, marshalErr)
		escaped := strings.ReplaceAll(string(draft), value, fmt.Sprintf(`\u%04x`, value[0])+value[1:])
		provider := contextFixtureProvider{complete: func(_ context.Context,
			req *llm.CompletionRequest) (*llm.CompletionResponse, error) {
			for _, input := range req.Messages {
				require.NotContains(t, input.Content, providerValue)
				require.NotContains(t, input.Content, toolValue)
			}
			return &llm.CompletionResponse{Content: escaped, StopReason: "stop"}, nil
		}}
		_, err = state.makeCheckpoint(ctx, provider, &llm.CompletionRequest{
			Model: "fixture", Messages: active, ContextWindow: 32768, MaxTokens: 512,
		}, common.NoopEventRecorder{})
		require.ErrorContains(t, err, "secret content")
	}
	_, err = f.store.LoadSessionCheckpoint(ctx, f.write.Namespace, f.write.SessionName, "")
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestSessionContextRedactsHeadersLoadedBeforeExecution(t *testing.T) {
	f := newWorkerContextFixture(t, 32768)
	const bearerValue = "opaque-loaded-header-placeholder"
	const keyValue = "opaque-loaded-key-placeholder"
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "header-tool", Namespace: "default"},
		Spec: corev1alpha1.ToolSpec{HTTP: &corev1alpha1.HTTPExecution{
			URL: "https://example.invalid", Headers: map[string]string{
				"Authorization": "Bearer " + bearerValue, "X-API-Key": keyValue,
			},
		}},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tool).Build()
	ctx := redact.WithTrackedSecrets(context.Background())
	require.Len(t, loadCustomTools(ctx, fakeClient, "default", []string{tool.Name}), 1)
	prompt := "Inspect " + bearerValue + " and " + keyValue
	state, active, err := newWorkerSessionContext(ctx, []llm.Message{{Role: "user", Content: prompt}})
	require.NoError(t, err)
	require.Equal(t, prompt, active[len(active)-1].Content)
	arguments, err := json.Marshal(map[string]string{"record": bearerValue, "selection": keyValue})
	require.NoError(t, err)
	message := llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{
		ID: "header-call", Name: tool.Name, Arguments: arguments,
	}}}
	saved, err := state.persist(ctx, message)
	require.NoError(t, err)
	require.Equal(t, json.RawMessage(arguments), message.ToolCalls[0].Arguments)
	// No tool has executed yet. Both values were already loaded with its CRD.
	for _, id := range []string{state.current.ID, saved.ID} {
		page, err := f.store.ReadSessionHistory(ctx, store.SessionHistoryRead{
			Namespace: f.write.Namespace, SessionName: f.write.SessionName, MessageID: id, Limit: 4096,
		})
		require.NoError(t, err)
		require.NotContains(t, page.Data, bearerValue)
		require.NotContains(t, page.Data, keyValue)
		require.Contains(t, page.Data, "[REDACTED]")
	}
}

func TestSessionContextRejectsHistoryPageWithLoadedCredential(t *testing.T) {
	f := newWorkerContextFixture(t, 32768)
	const value = "opaque-history-placeholder"
	_, err := f.store.AppendContextMessages(context.Background(), f.write, []store.SessionMessage{
		{ID: "older-source", Role: "assistant", Content: "Prior output for " + value},
	})
	require.NoError(t, err)
	ctx := redact.WithTrackedSecrets(context.Background())
	state, _, err := newWorkerSessionContext(ctx, []llm.Message{{Role: "user", Content: "Continue."}})
	require.NoError(t, err)
	redact.TrackSecrets(ctx, value)
	page, err := f.store.ReadSessionHistory(ctx, store.SessionHistoryRead{
		Namespace: f.write.Namespace, SessionName: f.write.SessionName, MessageID: "older-source", Limit: 4096,
	})
	require.NoError(t, err)
	data, err := json.Marshal(page)
	require.NoError(t, err)
	_, err = state.persist(ctx, llm.Message{Role: "tool", ToolCallID: "history-call",
		Name: readSessionHistoryTool, Content: string(data)})
	require.ErrorContains(t, err, "byte cursor cannot be safely rewritten")
}

func TestSessionCheckpointRedactsBeforeClippingSource(t *testing.T) {
	f := newWorkerContextFixture(t, 32768)
	const value = "opaque-clipping-placeholder"
	content := strings.Repeat(" ", 2048-len(value)+1) + value + " trailing reference"
	_, err := f.store.AppendContextMessages(context.Background(), f.write, []store.SessionMessage{
		{ID: "reference-source", Role: "assistant", Content: content},
	})
	require.NoError(t, err)
	ctx := redact.WithTrackedSecrets(context.Background())
	state, active, err := newWorkerSessionContext(ctx, []llm.Message{{Role: "user", Content: "Continue safely."}})
	require.NoError(t, err)
	redact.TrackSecrets(ctx, value)
	note, err := json.Marshal(checkpointDraft{Goal: checkpointClaim{
		Text: "Continue safely.", Sources: []string{state.current.ID},
	}})
	require.NoError(t, err)
	provider := contextFixtureProvider{complete: func(_ context.Context,
		req *llm.CompletionRequest) (*llm.CompletionResponse, error) {
		require.False(t, strings.Contains(req.Messages[0].Content, value[:len(value)-1]),
			"clipping must not expose all but the final byte of a tracked credential")
		require.Contains(t, req.Messages[0].Content, "[REDACTED]")
		return &llm.CompletionResponse{Content: string(note), StopReason: "stop"}, nil
	}}
	_, err = state.makeCheckpoint(ctx, provider, &llm.CompletionRequest{
		Model: "fixture", Messages: active, ContextWindow: 32768, MaxTokens: 512,
	}, common.NoopEventRecorder{})
	require.NoError(t, err)
}

func TestSessionCheckpointRedactsNestedHistoryCredentials(t *testing.T) {
	for _, test := range []struct {
		name  string
		value string
	}{
		{"quote", `opaque"quoted-placeholder`},
		{"backslash", `opaque\slash-placeholder`},
		{"both", `opaque"quoted\slash-placeholder`},
	} {
		for _, large := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/preview=%t", test.name, large), func(t *testing.T) {
				f := newWorkerContextFixture(t, 32768)
				content := "Earlier output: " + test.value
				if large {
					content += strings.Repeat(" padding", 1200)
				}
				_, err := f.store.AppendContextMessages(context.Background(), f.write, []store.SessionMessage{
					{ID: "reference-source", Role: "assistant", Content: content},
				})
				require.NoError(t, err)
				ctx := redact.WithTrackedSecrets(context.Background())
				state, active, err := newWorkerSessionContext(ctx,
					[]llm.Message{{Role: "user", Content: "Continue safely."}})
				require.NoError(t, err)
				call, err := state.persist(ctx, llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{
					ID: "history-call", Name: readSessionHistoryTool,
					Arguments: json.RawMessage(`{"message_id":"reference-source"}`),
				}}})
				require.NoError(t, err)
				page, err := f.store.ReadSessionHistory(ctx, store.SessionHistoryRead{
					Namespace: f.write.Namespace, SessionName: f.write.SessionName,
					MessageID: "reference-source", Limit: store.MaxSessionHistoryReadBytes,
				})
				require.NoError(t, err)
				data, err := json.Marshal(page)
				require.NoError(t, err)
				history, err := state.persist(ctx, llm.Message{Role: "tool", ToolCallID: "history-call",
					Name: readSessionHistoryTool, Content: string(data)})
				require.NoError(t, err)
				active = append(active, call, history)
				if large {
					require.Less(t, len(state.sources[history.ID].Content), len(data))
				}
				// The credential is learned only after the page has been committed.
				redact.TrackSecrets(ctx, test.value)
				nested := test.value
				for range 2 {
					encoded, marshalErr := json.Marshal(nested)
					require.NoError(t, marshalErr)
					nested = string(encoded[1 : len(encoded)-1])
				}
				note, err := json.Marshal(checkpointDraft{Goal: checkpointClaim{
					Text: "Continue safely.", Sources: []string{state.current.ID},
				}})
				require.NoError(t, err)
				provider := contextFixtureProvider{complete: func(_ context.Context,
					req *llm.CompletionRequest) (*llm.CompletionResponse, error) {
					var payload struct {
						Sources []store.SessionMessage `json:"sources"`
					}
					require.NoError(t, json.Unmarshal([]byte(req.Messages[0].Content), &payload))
					found := false
					for _, source := range payload.Sources {
						if source.ID != history.ID {
							continue
						}
						found = true
						require.False(t, strings.Contains(source.Content, nested),
							"checkpoint input retained a tracked credential inside encoded history data")
						require.Contains(t, source.Content, "[REDACTED]")
						if !large {
							// This is a sanitized checkpoint reference, not a new
							// history receipt with a rewritten byte cursor.
							var cleanPage store.SessionHistoryResult
							require.NoError(t, json.Unmarshal([]byte(source.Content), &cleanPage))
							var original store.SessionMessage
							require.NoError(t, json.Unmarshal([]byte(cleanPage.Data), &original))
							require.NotContains(t, original.Content, test.value)
						}
					}
					require.True(t, found, "the history reference must still be included")
					return &llm.CompletionResponse{Content: string(note), StopReason: "stop"}, nil
				}}
				_, err = state.makeCheckpoint(ctx, provider, &llm.CompletionRequest{
					Model: "fixture", Messages: active, ContextWindow: 32768, MaxTokens: 512,
				}, common.NoopEventRecorder{})
				require.NoError(t, err)
				require.Equal(t, string(data), history.Content, "active history receipt must remain unchanged")
				nestedNote, err := json.Marshal(checkpointDraft{Goal: checkpointClaim{
					Text: string(data), Sources: []string{state.current.ID},
				}})
				require.NoError(t, err)
				_, _, err = parseCheckpointNote(string(nestedNote), map[string]bool{state.current.ID: true}, test.value)
				require.ErrorContains(t, err, "secret content")
			})
		}
	}
}

func TestSessionContextRedactsConfiguredSecretsInToolArguments(t *testing.T) {
	f := newWorkerContextFixture(t, 32768)
	const fixtureValue = "context-placeholder-value"
	t.Setenv("CONTEXT_TEST_SECRET", fixtureValue)
	t.Setenv("CONTEXT_TEST_NUMERIC_SECRET", "3141592653589793")
	state, _, err := newWorkerSessionContext(context.Background(),
		[]llm.Message{{Role: "user", Content: "Inspect the selected record."}})
	require.NoError(t, err)
	arguments := json.RawMessage(`{"plain":"context-placeholder-value","selection":3141592653589793,` +
		`"nested":[{"escaped":"\u0063ontext-placeholder-value","record_id":9007199254740993}]}`)
	message := llm.Message{Role: "assistant",
		ToolCalls: []llm.ToolCall{{ID: "call-" + fixtureValue, Name: "inspect", Arguments: arguments}}}
	saved, err := state.persist(context.Background(), message)
	require.NoError(t, err)
	require.Equal(t, arguments, message.ToolCalls[0].Arguments,
		"saving a redacted source must not change execution arguments")
	page, err := f.store.ReadSessionHistory(context.Background(), store.SessionHistoryRead{
		Namespace: f.write.Namespace, SessionName: f.write.SessionName, MessageID: saved.ID, Limit: 4096,
	})
	require.NoError(t, err)
	require.NotContains(t, page.Data, fixtureValue)
	require.NotContains(t, page.Data, "3141592653589793")
	require.Contains(t, page.Data, "[REDACTED]")
	require.Contains(t, page.Data, "9007199254740993")
	require.Len(t, saved.ToolCalls, 1)
	require.JSONEq(t, `{"plain":"[REDACTED]","selection":"[REDACTED]",`+
		`"nested":[{"escaped":"[REDACTED]","record_id":9007199254740993}]}`, string(saved.ToolCalls[0].Arguments))
	require.Contains(t, string(saved.ToolCalls[0].Arguments), "9007199254740993")
	result, err := state.persist(context.Background(), llm.Message{
		Role: "tool", ToolCallID: message.ToolCalls[0].ID, Name: "inspect", Content: "Found the record.",
	})
	require.NoError(t, err)
	require.Equal(t, saved.ToolCalls[0].ID, result.ToolCallID)
	_, err = llm.FitMessages([]llm.Message{saved, result}, 1000)
	require.NoError(t, err, "redaction must preserve the saved call/result relationship")
}

func TestSessionCheckpointRejectsEscapedSecretsBeforeCommit(t *testing.T) {
	for _, field := range []string{"goal", "constraint", "finding", "remaining", "question", "source", "quoted value"} {
		t.Run(field, func(t *testing.T) {
			f := newWorkerContextFixture(t, 32768)
			fixtureValue := "context-placeholder-value"
			if field == "quoted value" {
				fixtureValue += "\"\nextra"
			}
			t.Setenv("CONTEXT_TEST_SECRET", fixtureValue)
			state, active, err := newWorkerSessionContext(context.Background(),
				[]llm.Message{{Role: "user", Content: "Continue the investigation."}})
			require.NoError(t, err)
			prior, err := state.persist(context.Background(),
				llm.Message{Role: "assistant", Content: "The parser is the next step."})
			require.NoError(t, err)
			active = append(active, prior)
			draft := checkpointDraft{Goal: checkpointClaim{
				Text: "Continue the investigation.", Sources: []string{state.current.ID},
			}}
			switch field {
			case "goal", "quoted value":
				draft.Goal.Text = fixtureValue
			case "constraint":
				draft.Constraints = []checkpointClaim{{Text: fixtureValue, Sources: []string{state.current.ID}}}
			case "finding":
				draft.Findings = []checkpointClaim{{Text: fixtureValue, Sources: []string{prior.ID}}}
			case "remaining":
				draft.Remaining = []string{fixtureValue}
			case "question":
				draft.Questions = []string{fixtureValue}
			case "source":
				draft.Goal.Sources = []string{fixtureValue}
			}
			data, err := json.Marshal(draft)
			require.NoError(t, err)
			escaped := strings.ReplaceAll(string(data), "context-placeholder", `\u0063ontext-placeholder`)
			require.Equal(t, escaped, sanitizeCheckpointText(escaped), "fixture must exercise decoded-string validation")
			provider := contextFixtureProvider{complete: func(context.Context,
				*llm.CompletionRequest) (*llm.CompletionResponse, error) {
				return &llm.CompletionResponse{Content: escaped, StopReason: "stop"}, nil
			}}
			_, err = state.makeCheckpoint(context.Background(), provider, &llm.CompletionRequest{
				Model: "fixture", Messages: active, ContextWindow: 32768, MaxTokens: 512,
			}, common.NoopEventRecorder{})
			require.ErrorContains(t, err, "secret content")
			_, err = f.store.LoadSessionCheckpoint(context.Background(), f.write.Namespace, f.write.SessionName, "")
			require.ErrorIs(t, err, store.ErrNotFound)
		})
	}
	_, _, err := parseCheckpointNote(`{"goal":{"text":"\u0043ontinue work.","sources":["source"]}}`,
		map[string]bool{"source": true})
	require.NoError(t, err, "safe JSON escapes remain valid")
}

func TestSessionContextBootstrapPreservesNumericArguments(t *testing.T) {
	f := newWorkerContextFixture(t, 32768)
	arguments := json.RawMessage(`{"record_id":9007199254740993}`)
	_, err := f.store.AppendContextMessages(context.Background(), f.write, []store.SessionMessage{
		{ID: "prior-call", Role: "assistant",
			ToolCalls: []llm.ToolCall{{ID: "numeric-call", Name: "inspect", Arguments: arguments}}},
		{ID: "prior-result", Role: "tool", ToolCallID: "numeric-call", Name: "inspect", Content: "Found the record."},
	})
	require.NoError(t, err)
	_, active, err := newWorkerSessionContext(context.Background(),
		[]llm.Message{{Role: "user", Content: "Continue the investigation."}})
	require.NoError(t, err)
	require.Len(t, active, 3)
	require.Len(t, active[0].ToolCalls, 1)
	require.Equal(t, arguments, active[0].ToolCalls[0].Arguments)
}

func TestSessionCheckpointIncludesCurrentRequestOnce(t *testing.T) {
	f := newWorkerContextFixture(t, 8192)
	_, err := f.store.AppendContextMessages(context.Background(), f.write, []store.SessionMessage{
		{ID: "prior-user", Role: "user", Content: strings.Repeat("u", 4000)},
		{ID: "prior-assistant", Role: "assistant", Content: strings.Repeat("a", 8000)},
	})
	require.NoError(t, err)
	current := strings.Repeat("c", 12000)
	state, active, err := newWorkerSessionContext(context.Background(), []llm.Message{{Role: "user", Content: current}})
	require.NoError(t, err)
	recent, err := state.persist(context.Background(), llm.Message{Role: "assistant", Content: strings.Repeat("r", 8000)})
	require.NoError(t, err)
	req := &llm.CompletionRequest{Model: "fixture", Messages: append(active, recent),
		SystemPrompt: "Instructions", ContextWindow: 8192, MaxTokens: 512}
	require.ErrorIs(t, llm.CheckContextWindow(req), llm.ErrContextLimit)
	calls := 0
	provider := contextFixtureProvider{complete: func(_ context.Context,
		request *llm.CompletionRequest) (*llm.CompletionResponse, error) {
		calls++
		require.NoError(t, llm.CheckContextWindow(request))
		var payload struct {
			CurrentRequestID string                 `json:"currentRequestMessageID"`
			Sources          []store.SessionMessage `json:"sources"`
		}
		require.NoError(t, json.Unmarshal([]byte(request.Messages[0].Content), &payload))
		require.Equal(t, state.current.ID, payload.CurrentRequestID)
		require.Equal(t, 1, strings.Count(request.Messages[0].Content, current))
		require.True(t, slices.ContainsFunc(payload.Sources, func(source store.SessionMessage) bool {
			return source.ID == state.current.ID && source.Role == "user" && source.Content == current
		}))
		return fixtureCheckpoint(request), nil
	}}
	require.NoError(t, state.fit(context.Background(), provider, req, true, common.NoopEventRecorder{}))
	require.Equal(t, 1, calls)
	require.NoError(t, llm.CheckContextWindow(req))
	require.True(t, slices.ContainsFunc(req.Messages, func(message llm.Message) bool {
		return message.ID == state.current.ID && message.Content == current
	}))
}

func TestSessionHistoryPagesReachModelWithCompleteDataAndCursor(t *testing.T) {
	for _, window := range []int{32768, 10000} {
		for _, padded := range []bool{false, true} {
			t.Run(fmt.Sprintf("window-%d-padded-%t", window, padded), func(t *testing.T) {
				name := readSessionHistoryTool
				if padded {
					name = " " + name + "\t"
				}
				testSessionHistoryPagesReachModel(t, window, name)
			})
		}
	}
}

func testSessionHistoryPagesReachModel(t *testing.T, window int, toolName string) {
	t.Helper()
	f := newWorkerContextFixture(t, window)
	content := strings.Repeat("evidence \n", 4000)
	_, err := f.store.AppendContextMessages(context.Background(), f.write, []store.SessionMessage{
		{ID: "prior-source", Role: "assistant", Content: content},
	})
	require.NoError(t, err)
	var assembled strings.Builder
	pages := 0
	checkpoints := 0
	provider := contextFixtureProvider{complete: func(_ context.Context,
		req *llm.CompletionRequest) (*llm.CompletionResponse, error) {
		require.NoError(t, llm.CheckContextWindow(req))
		if req.SystemPrompt == checkpointInstructions {
			checkpoints++
			return fixtureCheckpoint(req), nil
		}
		last := req.Messages[len(req.Messages)-1]
		if last.Role == "tool" {
			var page store.SessionHistoryResult
			require.NoError(t, json.Unmarshal([]byte(last.Content), &page))
			require.Equal(t, "prior-source", page.MessageID)
			require.Equal(t, assembled.Len(), page.Offset)
			require.Equal(t, page.Offset+len(page.Data), page.NextOffset)
			require.LessOrEqual(t, len(page.Data), store.MaxSessionHistoryReadBytes)
			assembled.WriteString(page.Data)
			pages++
			if page.NextOffset == page.TotalBytes {
				var original store.SessionMessage
				require.NoError(t, json.Unmarshal([]byte(assembled.String()), &original))
				require.Equal(t, content, original.Content)
				return &llm.CompletionResponse{Content: "Read all saved evidence.", StopReason: "stop"}, nil
			}
			require.Greater(t, len(last.Content), store.MaxSessionContextPreviewBytes)
		}
		arguments, marshalErr := json.Marshal(map[string]any{
			"message_id": "prior-source", "offset": assembled.Len(), "limit": store.MaxSessionHistoryReadBytes,
		})
		require.NoError(t, marshalErr)
		return &llm.CompletionResponse{StopReason: "tool_calls", ToolCalls: []llm.ToolCall{{
			ID: fmt.Sprintf("history-%d", assembled.Len()), Name: toolName, Arguments: arguments,
		}}}, nil
	}}
	result, err := executeAgentLoopWithEvents(context.Background(), provider,
		[]llm.Message{{Role: "user", Content: "Read all earlier evidence."}}, "Instructions", "fixture",
		modelSettings{maxTokens: 512}, nil, nil, nil, common.NoopEventRecorder{})
	require.NoError(t, err)
	require.Equal(t, "Read all saved evidence.", result)
	require.Greater(t, pages, 1)
	if window == 10000 {
		require.Positive(t, checkpoints, "already consumed pages must remain eligible for compaction")
	}
	transcript, err := f.store.LoadTranscript(context.Background(), f.write.Namespace, f.write.SessionName, 0)
	require.NoError(t, err)
	for _, message := range transcript {
		require.LessOrEqual(t, len(message.Content), store.MaxSessionContextPreviewBytes)
	}
}

func TestSessionHistoryRedactedPagesKeepSourceAndEnvelope(t *testing.T) {
	f := newWorkerContextFixture(t, 32768)
	content := strings.Repeat("evidence ", 4000) + "token is [REDACTED]"
	_, err := f.store.AppendContextMessages(context.Background(), f.write, []store.SessionMessage{
		{ID: "redacted-source", Role: "assistant", Content: content},
	})
	require.NoError(t, err)
	state, _, err := newWorkerSessionContext(context.Background(),
		[]llm.Message{{Role: "user", Content: "Read saved evidence."}})
	require.NoError(t, err)
	var assembled strings.Builder
	for {
		arguments, marshalErr := json.Marshal(map[string]any{
			"message_id": "redacted-source", "offset": assembled.Len(), "limit": store.MaxSessionHistoryReadBytes,
		})
		require.NoError(t, marshalErr)
		content, readErr := state.readHistory(context.Background(), arguments)
		require.NoError(t, readErr)
		var original store.SessionHistoryResult
		require.NoError(t, json.Unmarshal([]byte(content), &original))
		saved, saveErr := state.persist(context.Background(), llm.Message{
			Role: "tool", Name: readSessionHistoryTool, ToolCallID: "history-call", Content: content,
		})
		require.NoError(t, saveErr)
		var delivered store.SessionHistoryResult
		require.NoError(t, json.Unmarshal([]byte(saved.Content), &delivered))
		require.Equal(t, original, delivered, "redaction must leave the saved fragment and its byte cursor intact")
		var savedSource strings.Builder
		for {
			page, pageErr := f.store.ReadSessionHistory(context.Background(), store.SessionHistoryRead{
				Namespace: f.write.Namespace, SessionName: f.write.SessionName,
				MessageID: saved.ID, Offset: savedSource.Len(), Limit: store.MaxSessionHistoryReadBytes,
			})
			require.NoError(t, pageErr)
			savedSource.WriteString(page.Data)
			if page.NextOffset == page.TotalBytes {
				break
			}
		}
		var archived store.SessionMessage
		require.NoError(t, json.Unmarshal([]byte(savedSource.String()), &archived))
		require.JSONEq(t, content, archived.Content)
		assembled.WriteString(delivered.Data)
		if delivered.NextOffset == delivered.TotalBytes {
			break
		}
	}
	var recovered store.SessionMessage
	require.NoError(t, json.Unmarshal([]byte(assembled.String()), &recovered))
	require.Equal(t, content, recovered.Content)
}

func TestSessionCheckpointDoesNotEvictUnreadHistoryPage(t *testing.T) {
	f := newWorkerContextFixture(t, 32768)
	_, err := f.store.AppendContextMessages(context.Background(), f.write, []store.SessionMessage{
		{ID: "read-source", Role: "assistant", Content: strings.Repeat("evidence ", 2000)},
	})
	require.NoError(t, err)
	state, _, err := newWorkerSessionContext(context.Background(),
		[]llm.Message{{Role: "user", Content: strings.Repeat("current request ", 256)}})
	require.NoError(t, err)
	arguments := json.RawMessage(`{"message_id":"read-source","limit":16384}`)
	call, err := state.persist(context.Background(), llm.Message{
		Role:      "assistant",
		ToolCalls: []llm.ToolCall{{ID: "history-call", Name: readSessionHistoryTool, Arguments: arguments}},
	})
	require.NoError(t, err)
	content, err := state.readHistory(context.Background(), arguments)
	require.NoError(t, err)
	result, err := state.persist(context.Background(), llm.Message{
		Role: "tool", ToolCallID: "history-call", Name: readSessionHistoryTool, Content: content,
	})
	require.NoError(t, err)
	req := &llm.CompletionRequest{Model: "fixture", SystemPrompt: "Instructions", MaxTokens: 512,
		Tools: []llm.Tool{sessionHistoryToolDefinition()}, Messages: []llm.Message{state.current, call, result}}
	req.ContextWindow = llm.EstimateRequestTokens(req) + llm.ResponseTokenReserve(req) + 32
	provider := contextFixtureProvider{complete: func(_ context.Context,
		req *llm.CompletionRequest) (*llm.CompletionResponse, error) {
		return fixtureCheckpoint(req), nil
	}}
	require.NoError(t, state.fit(context.Background(), provider, req, false, common.NoopEventRecorder{}))
	require.True(t, slices.ContainsFunc(req.Messages, func(message llm.Message) bool {
		return message.ID == result.ID && message.Content == content
	}), "the normal model must receive the complete requested page before compaction may remove it")
	require.NoError(t, llm.CheckContextWindow(req))
	require.False(t, slices.ContainsFunc(req.Messages, func(message llm.Message) bool {
		return message.Name == sessioncontext.CheckpointName
	}))

	// Required reduction may discard older history while preserving the entire
	// unread call/result. Its framing and data also remain fixed during recovery.
	older, err := sessioncontext.ModelMessage(state.sources["read-source"])
	require.NoError(t, err)
	req.Messages = append([]llm.Message{older}, req.Messages...)
	req.ContextWindow += 1024
	require.ErrorIs(t, llm.CheckContextWindow(req), llm.ErrContextLimit)
	recoveryWindow := contextRecoveryWindow(req, fmt.Errorf("context length exceeded"),
		state.currentIndex(req.Messages), state.requiredHistoryIndices(req.Messages)...)
	require.Greater(t, recoveryWindow, llm.EstimateRequestTokens(&llm.CompletionRequest{
		Messages: []llm.Message{state.current, call, result},
	}))
	require.NoError(t, state.fit(context.Background(), provider, req, true, common.NoopEventRecorder{}))
	require.True(t, slices.ContainsFunc(req.Messages, func(message llm.Message) bool {
		return message.ID == result.ID && message.Content == content
	}))
	require.False(t, slices.ContainsFunc(req.Messages, func(message llm.Message) bool { return message.ID == older.ID }))
	require.NoError(t, llm.CheckContextWindow(req))

	original := slices.Clone(req.Messages)
	req.ContextWindow = llm.EstimateRequestTokens(req) + llm.ResponseTokenReserve(req) - 128
	require.Error(t, state.fit(context.Background(), provider, req, true, common.NoopEventRecorder{}))
	require.Equal(t, original, req.Messages, "an impossible fit must not silently discard the unread page")
}

func TestContextRecoveryPreservesRequiredInputs(t *testing.T) {
	for _, fixedInput := range []string{"system prompt", "system message", "tool schema", "current request"} {
		t.Run(fixedInput, func(t *testing.T) {
			t.Setenv(workerenv.SessionCheckpointsEnabled, "false")
			const providerWindow = 9000
			large := strings.Repeat("s", 24000)
			messages := []llm.Message{
				{Role: "user", Content: strings.Repeat("u", 8000)},
				{Role: "assistant", Content: strings.Repeat("a", 8000)},
				{ID: "current", Role: "user", Content: "Continue investigating."},
			}
			var system string
			var requestTools []llm.Tool
			switch fixedInput {
			case "system prompt":
				system = large
			case "system message":
				messages = append([]llm.Message{{Role: "system", Content: large}}, messages...)
			case "tool schema":
				requestTools = []llm.Tool{{Name: "inspect",
					Parameters: json.RawMessage(`{"type":"object","description":"` + large + `"}`)}}
			case "current request":
				messages[len(messages)-1].Content = large
			}
			current := messages[len(messages)-1]
			calls := 0
			provider := contextFixtureProvider{complete: func(_ context.Context,
				req *llm.CompletionRequest) (*llm.CompletionResponse, error) {
				calls++
				require.Equal(t, system, req.SystemPrompt)
				require.Equal(t, requestTools, req.Tools)
				require.True(t, slices.ContainsFunc(req.Messages, func(message llm.Message) bool {
					return message.ID == current.ID && message.Role == current.Role && message.Content == current.Content
				}))
				if fixedInput == "system message" {
					require.Equal(t, messages[0], req.Messages[0])
				}
				if llm.EstimateRequestTokens(req)+llm.ResponseTokenReserve(req) > providerWindow {
					return nil, &llm.ProviderError{StatusCode: http.StatusBadRequest, Message: "context window too long"}
				}
				return &llm.CompletionResponse{Content: "Finished the investigation.", StopReason: "stop"}, nil
			}}
			result, err := executeAgentLoopWithEvents(context.Background(), provider, messages, system, "fixture",
				modelSettings{maxTokens: 512}, requestTools, nil, nil, common.NoopEventRecorder{})
			require.NoError(t, err)
			require.Equal(t, "Finished the investigation.", result)
			require.Equal(t, 2, calls)
		})
	}
}

func TestSessionContextRedactsTrackedQuotedAssignmentBeforePatterns(t *testing.T) {
	f := newWorkerContextFixture(t, 32768)
	const value = `a"opaque-credential-tail-placeholder`
	ctx := redact.WithTrackedSecrets(context.Background())
	redact.TrackSecrets(ctx, value)
	state, _, err := newWorkerSessionContext(ctx, []llm.Message{{Role: "user", Content: "Continue safely."}})
	require.NoError(t, err)
	content, err := json.Marshal(map[string]string{"api_key": value})
	require.NoError(t, err)
	saved, err := state.persist(ctx, llm.Message{Role: "assistant", Content: string(content)})
	require.NoError(t, err)
	require.NotContains(t, saved.Content, "opaque-credential-tail-placeholder")
	page, err := f.store.ReadSessionHistory(ctx, store.SessionHistoryRead{
		Namespace: f.write.Namespace, SessionName: f.write.SessionName, MessageID: saved.ID, Limit: 4096,
	})
	require.NoError(t, err)
	require.NotContains(t, page.Data, "opaque-credential-tail-placeholder")
}

func TestSessionContextRedactsKnownValuesOverlappingCredentialLabels(t *testing.T) {
	f := newWorkerContextFixture(t, 32768)
	t.Setenv(workerenv.TransactionCredentialSecret, "password")
	const value = "opaque-assignment-placeholder"
	content := "password=" + value
	ctx := context.Background()
	_, err := f.store.AppendContextMessages(ctx, f.write, []store.SessionMessage{
		{ID: "reference-source", Role: "assistant", Content: content},
	})
	require.NoError(t, err)
	state, active, err := newWorkerSessionContext(ctx, []llm.Message{{Role: "user", Content: content}})
	require.NoError(t, err)
	require.Equal(t, content, active[len(active)-1].Content, "the active request must stay exact")
	saved, err := state.persist(ctx, llm.Message{Role: "assistant", Content: content})
	require.NoError(t, err)
	active = append(active, saved)
	for _, id := range []string{state.current.ID, saved.ID} {
		page, err := f.store.ReadSessionHistory(ctx, store.SessionHistoryRead{
			Namespace: f.write.Namespace, SessionName: f.write.SessionName, MessageID: id, Limit: 4096,
		})
		require.NoError(t, err)
		if strings.Contains(page.Data, value) {
			t.Error("a configured value hid a credential label before persistence redaction")
		}
	}
	note, err := json.Marshal(checkpointDraft{Goal: checkpointClaim{
		Text: "Continue safely.", Sources: []string{state.current.ID},
	}})
	require.NoError(t, err)
	provider := contextFixtureProvider{complete: func(_ context.Context,
		req *llm.CompletionRequest) (*llm.CompletionResponse, error) {
		if strings.Contains(req.Messages[0].Content, value) {
			t.Error("a configured value hid a credential label in checkpoint references")
		}
		return &llm.CompletionResponse{Content: string(note), StopReason: "stop"}, nil
	}}
	_, err = state.makeCheckpoint(ctx, provider, &llm.CompletionRequest{
		Model: "fixture", Messages: active, ContextWindow: 32768, MaxTokens: 512,
	}, common.NoopEventRecorder{})
	require.NoError(t, err)
	unsafeNote, err := json.Marshal(checkpointDraft{Goal: checkpointClaim{
		Text: content, Sources: []string{state.current.ID},
	}})
	require.NoError(t, err)
	_, _, err = parseCheckpointNote(string(unsafeNote), map[string]bool{state.current.ID: true})
	require.ErrorContains(t, err, "secret content")
}

func TestSessionContextRedactsCredentialFieldsBeforeMaskingNames(t *testing.T) {
	for _, field := range []string{"password", "authorization", "api_key"} {
		for _, value := range []string{
			`"opaque-field-placeholder"`, `{"nested":"opaque-field-placeholder"}`, `3141592653589793`,
		} {
			t.Run(field+"/"+value[:1], func(t *testing.T) {
				f := newWorkerContextFixture(t, 32768)
				ctx := redact.WithTrackedSecrets(context.Background())
				redact.TrackSecrets(ctx, field)
				state, _, err := newWorkerSessionContext(ctx, []llm.Message{{Role: "user", Content: "Continue safely."}})
				require.NoError(t, err)
				arguments, err := json.Marshal(map[string]any{
					"outer": map[string]any{field: json.RawMessage(value)}, "record_id": json.Number("9007199254740993"),
				})
				require.NoError(t, err)
				message := llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{
					ID: "field-call", Name: "inspect", Arguments: arguments,
				}}}
				saved, err := state.persist(ctx, message)
				require.NoError(t, err)
				require.Equal(t, json.RawMessage(arguments), message.ToolCalls[0].Arguments)
				require.JSONEq(t, `{"outer":{"[REDACTED]":"[REDACTED]"},"record_id":9007199254740993}`,
					string(saved.ToolCalls[0].Arguments))
				page, err := f.store.ReadSessionHistory(ctx, store.SessionHistoryRead{
					Namespace: f.write.Namespace, SessionName: f.write.SessionName, MessageID: saved.ID, Limit: 4096,
				})
				require.NoError(t, err)
				require.NotContains(t, page.Data, "opaque-field-placeholder")
				require.NotContains(t, page.Data, "3141592653589793")
				require.Contains(t, page.Data, "9007199254740993")
			})
		}
	}
	_, changed, err := sanitizeCheckpointJSON(map[string]any{"password": "[REDACTED]"})
	require.NoError(t, err)
	require.False(t, changed, "an already redacted field must remain stable")
}

func TestSessionContextRedactsQuotedHTTPToolResponse(t *testing.T) {
	for _, field := range []string{"api_key", "output"} {
		t.Run(field, func(t *testing.T) {
			newWorkerContextFixture(t, 32768)
			const value = `a"opaque-response-tail-placeholder`
			ctx := redact.WithTrackedSecrets(context.Background())
			state, _, err := newWorkerSessionContext(ctx, []llm.Message{{Role: "user", Content: "Continue safely."}})
			require.NoError(t, err)
			responseValue := value
			if field == "output" {
				responseValue = "Authorization: Bearer " + value
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]string{field: responseValue})
			}))
			defer server.Close()
			executor := worker.NewToolExecutor()
			executor.SetAuthSecretValue("http-auth", "authref", value)
			tool := &corev1alpha1.Tool{Spec: corev1alpha1.ToolSpec{HTTP: &corev1alpha1.HTTPExecution{
				URL: server.URL, AuthSecretRef: &corev1alpha1.SecretKeySelector{Name: "http-auth", Key: "authref"},
			}}}
			output, err := executor.Execute(ctx, tool, nil)
			require.NoError(t, err)
			saved, err := state.persist(ctx, llm.Message{
				Role: "tool", Name: "inspect", ToolCallID: "http-call", Content: output,
			})
			require.NoError(t, err)
			require.NotContains(t, saved.Content, "opaque-response-tail-placeholder")
		})
	}
}

func TestSessionContextRejectsCredentialSplitAcrossHistoryPages(t *testing.T) {
	for _, fixture := range []struct{ name, value string }{
		{"plain", "opaque-boundary-placeholder"},
		{"quote", `a"opaque-boundary-placeholder`},
		{"backslash", `a\opaque-boundary-placeholder`},
	} {
		for _, section := range []string{"prefix", "suffix", "before", "after"} {
			t.Run(fixture.name+"/"+section, func(t *testing.T) {
				f := newWorkerContextFixture(t, 32768)
				_, err := f.store.AppendContextMessages(context.Background(), f.write, []store.SessionMessage{
					{
						ID: "reference-source", Role: "assistant",
						Content: "Earlier output for " + fixture.value + " with safe trailing output.",
					},
				})
				require.NoError(t, err)
				ctx := redact.WithTrackedSecrets(context.Background())
				state, _, err := newWorkerSessionContext(ctx, []llm.Message{{Role: "user", Content: "Continue safely."}})
				require.NoError(t, err)
				full, err := f.store.ReadSessionHistory(ctx, store.SessionHistoryRead{
					Namespace: f.write.Namespace, SessionName: f.write.SessionName, MessageID: "reference-source", Limit: 4096,
				})
				require.NoError(t, err)
				encoded, err := json.Marshal(fixture.value)
				require.NoError(t, err)
				form := string(encoded[1 : len(encoded)-1])
				offset := strings.Index(full.Data, form)
				require.GreaterOrEqual(t, offset, 0)
				limit := len(form) - 1
				switch section {
				case "suffix":
					offset++
				case "before":
					limit, offset = offset, 0
				case "after":
					offset += len(form)
					limit = len(full.Data) - offset
				}
				page, err := f.store.ReadSessionHistory(ctx, store.SessionHistoryRead{
					Namespace: f.write.Namespace, SessionName: f.write.SessionName,
					MessageID: "reference-source", Offset: offset, Limit: limit,
				})
				require.NoError(t, err)
				data, err := json.Marshal(page)
				require.NoError(t, err)
				args, err := json.Marshal(map[string]any{"message_id": "reference-source", "offset": offset, "limit": limit})
				require.NoError(t, err)
				_, err = state.persist(ctx, llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{
					ID: "history-call", Name: readSessionHistoryTool, Arguments: args,
				}}})
				require.NoError(t, err)
				redact.TrackSecrets(ctx, fixture.value)
				output, readErr := state.readHistory(ctx, args)
				saved, writeErr := state.persist(ctx, llm.Message{Role: "tool", ToolCallID: "history-call",
					Name: readSessionHistoryTool, Content: string(data)})
				if section == "prefix" || section == "suffix" {
					require.ErrorContains(t, readErr, "configured secret")
					require.Empty(t, output)
					require.ErrorContains(t, writeErr, "configured secret")
				} else {
					require.NoError(t, readErr)
					require.NoError(t, writeErr)
					require.Equal(t, string(data), output)
					require.Equal(t, string(data), saved.Content)
				}
			})
		}
	}
}

func TestSessionHistoryRechecksNestedReceiptsAfterCredentialLoading(t *testing.T) {
	for _, fixture := range []struct{ name, value string }{
		{"plain", "opaque-nested-placeholder"},
		{"escaped", `a"opaque-nested-placeholder`},
	} {
		for _, reload := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/reload=%t", fixture.name, reload), func(t *testing.T) {
				f := newWorkerContextFixture(t, 32768)
				ctx := redact.WithTrackedSecrets(context.Background())
				_, err := f.store.AppendContextMessages(ctx, f.write, []store.SessionMessage{
					{ID: "reference-source", Role: "assistant", Content: "Earlier output for " + fixture.value},
				})
				require.NoError(t, err)
				state, active, err := newWorkerSessionContext(ctx, []llm.Message{{Role: "user", Content: "Continue safely."}})
				require.NoError(t, err)
				full, err := f.store.ReadSessionHistory(ctx, store.SessionHistoryRead{
					Namespace: f.write.Namespace, SessionName: f.write.SessionName, MessageID: "reference-source", Limit: 4096,
				})
				require.NoError(t, err)
				encoded, err := json.Marshal(fixture.value)
				require.NoError(t, err)
				form := string(encoded[1 : len(encoded)-1])
				offset := strings.Index(full.Data, form) + 1
				require.Greater(t, offset, 0)
				args, err := json.Marshal(map[string]any{
					"message_id": "reference-source", "offset": offset, "limit": len(form) - 2,
				})
				require.NoError(t, err)
				receipts := make([]llm.Message, 0, 3)
				for i := range 3 {
					callID := fmt.Sprintf("nested-call-%d", i)
					call, err := state.persist(ctx, llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{
						ID: callID, Name: readSessionHistoryTool, Arguments: args,
					}}})
					require.NoError(t, err)
					data, err := state.readHistory(ctx, args)
					require.NoError(t, err, "safe nested receipts remain readable")
					receipt, err := state.persist(ctx, llm.Message{
						Role: "tool", Name: readSessionHistoryTool, ToolCallID: callID, Content: data,
					})
					require.NoError(t, err)
					active = append(active, call, receipt)
					receipts = append(receipts, receipt)
					args, err = json.Marshal(map[string]any{"message_id": receipt.ID, "limit": store.MaxSessionHistoryReadBytes})
					require.NoError(t, err)
				}
				if reload {
					state.historyPages, state.historySource = nil, nil
				}
				redact.TrackSecrets(ctx, fixture.value)
				_, err = state.readHistory(ctx, args)
				require.ErrorContains(t, err, "configured secret")
				_, err = state.persist(ctx, receipts[len(receipts)-1])
				require.ErrorContains(t, err, "configured secret")
				note, err := json.Marshal(checkpointDraft{Goal: checkpointClaim{
					Text: "Continue safely.", Sources: []string{state.current.ID},
				}})
				require.NoError(t, err)
				provider := contextFixtureProvider{complete: func(_ context.Context,
					req *llm.CompletionRequest) (*llm.CompletionResponse, error) {
					require.NotContains(t, req.Messages[0].Content, "nested-placeholde")
					return &llm.CompletionResponse{Content: string(note), StopReason: "stop"}, nil
				}}
				_, err = state.makeCheckpoint(ctx, provider, &llm.CompletionRequest{
					Model: "fixture", Messages: active, ContextWindow: 32768, MaxTokens: 512,
				}, common.NoopEventRecorder{})
				require.NoError(t, err)
				for _, receipt := range receipts {
					page, err := f.store.ReadSessionHistory(ctx, store.SessionHistoryRead{
						Namespace: f.write.Namespace, SessionName: f.write.SessionName, MessageID: receipt.ID, Limit: 16384,
					})
					require.NoError(t, err)
					var persisted struct{ Content string }
					require.NoError(t, json.Unmarshal([]byte(page.Data), &persisted))
					require.Equal(t, receipt.Content, persisted.Content, "saved receipts must remain exact")
				}
			})
		}
	}
}

func TestSessionCheckpointRechecksCopiedHistoryReceipts(t *testing.T) {
	for _, kind := range []string{"assistant", "tool", "user", "current"} {
		for _, preview := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/preview=%t", kind, preview), func(t *testing.T) {
				f := newWorkerContextFixture(t, 32768)
				ctx := redact.WithTrackedSecrets(context.Background())
				const value = "opaque-copied-receipt-placeholder"
				content := "Earlier output for " + value
				if preview {
					content += strings.Repeat(" trailing output", 1600)
				}
				_, err := f.store.AppendContextMessages(ctx, f.write, []store.SessionMessage{
					{ID: "reference-source", Role: "assistant", Content: content},
				})
				require.NoError(t, err)
				full, err := f.store.ReadSessionHistory(ctx, store.SessionHistoryRead{
					Namespace: f.write.Namespace, SessionName: f.write.SessionName, MessageID: "reference-source", Limit: 4096,
				})
				require.NoError(t, err)
				offset := strings.Index(full.Data, value) + 1
				require.Greater(t, offset, 0)
				limit := len(value) - 1
				if preview {
					limit = store.MaxSessionHistoryReadBytes
				}
				page, err := f.store.ReadSessionHistory(ctx, store.SessionHistoryRead{
					Namespace: f.write.Namespace, SessionName: f.write.SessionName,
					MessageID: "reference-source", Offset: offset, Limit: limit,
				})
				require.NoError(t, err)
				data, err := json.Marshal(page)
				require.NoError(t, err)
				prompt := "Continue safely."
				if kind == "current" {
					prompt = string(data)
				}
				state, active, err := newWorkerSessionContext(ctx, []llm.Message{{Role: "user", Content: prompt}})
				require.NoError(t, err)
				copied := state.current
				if kind != "current" {
					message := llm.Message{Role: kind, Content: string(data)}
					if kind == "tool" {
						message.Name, message.ToolCallID = "inspect", "copy-call"
						call, err := state.persist(ctx, llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{
							ID: "copy-call", Name: "inspect", Arguments: json.RawMessage(`{}`),
						}}})
						require.NoError(t, err)
						active = append(active, call)
					}
					copied, err = state.persist(ctx, message)
					require.NoError(t, err)
					active = append(active, copied)
				}
				if preview {
					require.NotEmpty(t, state.sources[copied.ID].Metadata[store.SessionContextOutputRefKey])
					require.Less(t, len(state.sources[copied.ID].Content), len(data))
				}
				redact.TrackSecrets(ctx, value)
				note, err := json.Marshal(checkpointDraft{Goal: checkpointClaim{
					Text: "Continue safely.", Sources: []string{state.current.ID},
				}})
				require.NoError(t, err)
				provider := contextFixtureProvider{complete: func(_ context.Context,
					req *llm.CompletionRequest) (*llm.CompletionResponse, error) {
					require.NotContains(t, req.Messages[0].Content, value[1:])
					var payload struct{ Sources []store.SessionMessage }
					require.NoError(t, json.Unmarshal([]byte(req.Messages[0].Content), &payload))
					found := false
					for _, source := range payload.Sources {
						if source.ID == copied.ID {
							found = true
							require.Contains(t, source.Content, "[REDACTED]")
						}
					}
					require.True(t, found)
					return &llm.CompletionResponse{Content: string(note), StopReason: "stop"}, nil
				}}
				_, err = state.makeCheckpoint(ctx, provider, &llm.CompletionRequest{
					Model: "fixture", Messages: active, ContextWindow: 32768, MaxTokens: 512,
				}, common.NoopEventRecorder{})
				require.NoError(t, err)
				require.Equal(t, prompt, state.current.Content, "the active current request must remain exact")
				stored, err := state.loadHistorySource(ctx, copied.ID)
				require.NoError(t, err)
				var persisted struct{ Content string }
				require.NoError(t, json.Unmarshal([]byte(stored.data), &persisted))
				require.Equal(t, string(data), persisted.Content, "checkpoint validation must not rewrite saved copies")
			})
		}
	}
}

func TestSessionHistoryVerifiesCompleteReceiptData(t *testing.T) {
	f := newWorkerContextFixture(t, 32768)
	ctx := context.Background()
	_, err := f.store.AppendContextMessages(ctx, f.write, []store.SessionMessage{
		{ID: "reference-source", Role: "assistant", Content: "Approved record."},
	})
	require.NoError(t, err)
	state, _, err := newWorkerSessionContext(ctx, []llm.Message{{Role: "user", Content: "Continue safely."}})
	require.NoError(t, err)
	page, err := f.store.ReadSessionHistory(ctx, store.SessionHistoryRead{
		Namespace: f.write.Namespace, SessionName: f.write.SessionName, MessageID: "reference-source", Limit: 4096,
	})
	require.NoError(t, err)
	page.Data = strings.ReplaceAll(page.Data, "Approved", "Replaced")
	require.ErrorContains(t, state.validateHistoryPage(ctx, *page), "does not match")
}

func TestSessionHistoryRejectsCredentialInReceiptIdentity(t *testing.T) {
	f := newWorkerContextFixture(t, 32768)
	ctx := redact.WithTrackedSecrets(context.Background())
	redact.TrackSecrets(ctx, readSessionHistoryTool)
	state, _, err := newWorkerSessionContext(ctx, []llm.Message{{Role: "user", Content: "Continue safely."}})
	require.NoError(t, err)
	args, err := json.Marshal(map[string]any{"message_id": state.current.ID})
	require.NoError(t, err)
	data, err := state.readHistory(ctx, args)
	require.NoError(t, err)
	receipt, err := state.persist(ctx, llm.Message{Role: "tool", Name: readSessionHistoryTool, Content: data})
	require.ErrorContains(t, err, "configured secret in its tool identity")
	_, err = f.store.ReadSessionHistory(ctx, store.SessionHistoryRead{
		Namespace: f.write.Namespace, SessionName: f.write.SessionName, MessageID: receipt.ID, Limit: 4096,
	})
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestSessionContextValidatesCopiedReceiptsBeforeRedaction(t *testing.T) {
	for _, kind := range []string{"assistant", "tool", "user", "current"} {
		t.Run(kind, func(t *testing.T) {
			f := newWorkerContextFixture(t, 32768)
			ctx := redact.WithTrackedSecrets(context.Background())
			const first = "opaque-complete-copy-placeholder"
			const second = "opaque-split-copy-placeholder"
			_, err := f.store.AppendContextMessages(ctx, f.write, []store.SessionMessage{
				{ID: "reference-source", Role: "assistant", Content: "Earlier output: " + first + " followed by " + second},
			})
			require.NoError(t, err)
			full, err := f.store.ReadSessionHistory(ctx, store.SessionHistoryRead{
				Namespace: f.write.Namespace, SessionName: f.write.SessionName, MessageID: "reference-source", Limit: 4096,
			})
			require.NoError(t, err)
			cut := strings.Index(full.Data, second) + len(second) - 1
			require.Greater(t, cut, 0)
			page, err := f.store.ReadSessionHistory(ctx, store.SessionHistoryRead{
				Namespace: f.write.Namespace, SessionName: f.write.SessionName, MessageID: "reference-source", Limit: cut,
			})
			require.NoError(t, err)
			require.Contains(t, page.Data, first)
			require.Contains(t, page.Data, second[:len(second)-1])
			require.NotContains(t, page.Data, second)
			content, err := json.Marshal(page)
			require.NoError(t, err)
			redact.TrackSecrets(ctx, first, second)
			if kind == "current" {
				_, _, err = newWorkerSessionContext(ctx, []llm.Message{{Role: "user", Content: string(content)}})
				require.ErrorContains(t, err, "configured secret")
				return
			}
			state, _, err := newWorkerSessionContext(ctx, []llm.Message{{Role: "user", Content: "Continue safely."}})
			require.NoError(t, err)
			message := llm.Message{Role: kind, Content: string(content)}
			if kind == "tool" {
				message.Name, message.ToolCallID = "inspect", "copy-call"
				_, err = state.persist(ctx, llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{
					ID: message.ToolCallID, Name: message.Name, Arguments: json.RawMessage(`{}`),
				}}})
				require.NoError(t, err)
			}
			attempted, err := state.persist(ctx, message)
			require.ErrorContains(t, err, "configured secret")
			_, err = f.store.ReadSessionHistory(ctx, store.SessionHistoryRead{
				Namespace: f.write.Namespace, SessionName: f.write.SessionName, MessageID: attempted.ID, Limit: 4096,
			})
			require.ErrorIs(t, err, store.ErrNotFound)
		})
	}
}

func TestSessionCheckpointRedactsOrdinaryPreviewBeforeItsCut(t *testing.T) {
	for _, longCredential := range []bool{false, true} {
		t.Run(fmt.Sprintf("longCredential=%t", longCredential), func(t *testing.T) {
			f := newWorkerContextFixture(t, 32768)
			ctx := redact.WithTrackedSecrets(context.Background())
			value := "opaque-ordinary-preview-placeholder"
			const id = "reference-source"
			notice := "\n[Full saved message: " + id + ". Use read_session_history for details.]"
			cut := store.MaxSessionContextPreviewBytes - len(notice)
			prefix, fragment := strings.Repeat("x", cut-len(value)+1), value[:len(value)-1]
			if longCredential {
				value = strings.Repeat(value, 400)
				prefix, fragment = "Earlier output: ", value[:512]
			}
			content := prefix + value + strings.Repeat(" trailing output", 1000)
			saved, err := f.store.AppendContextMessages(ctx, f.write, []store.SessionMessage{
				{ID: id, Role: "user", Content: content},
			})
			require.NoError(t, err)
			require.Contains(t, saved[0].Content, fragment)
			require.NotContains(t, saved[0].Content, value)
			state, active, err := newWorkerSessionContext(ctx, []llm.Message{{Role: "user", Content: "Continue safely."}})
			require.NoError(t, err)
			redact.TrackSecrets(ctx, value)
			note, err := json.Marshal(checkpointDraft{Goal: checkpointClaim{
				Text: "Continue safely.", Sources: []string{state.current.ID},
			}})
			require.NoError(t, err)
			provider := contextFixtureProvider{complete: func(_ context.Context,
				req *llm.CompletionRequest) (*llm.CompletionResponse, error) {
				require.NotContains(t, req.Messages[0].Content, fragment)
				var payload struct{ Sources []store.SessionMessage }
				require.NoError(t, json.Unmarshal([]byte(req.Messages[0].Content), &payload))
				found := false
				for _, source := range payload.Sources {
					if source.ID == id {
						found = true
						require.Contains(t, source.Content, "[REDACTED]")
						require.LessOrEqual(t, len(source.Content), store.MaxSessionContextPreviewBytes)
						require.Equal(t, id, source.Metadata[store.SessionContextOutputRefKey])
					}
				}
				require.True(t, found)
				return &llm.CompletionResponse{Content: string(note), StopReason: "stop"}, nil
			}}
			_, err = state.makeCheckpoint(ctx, provider, &llm.CompletionRequest{
				Model: "fixture", Messages: active, ContextWindow: 32768, MaxTokens: 512,
			}, common.NoopEventRecorder{})
			require.NoError(t, err)
			require.Equal(t, saved[0].Content, state.sources[id].Content)
			original, err := state.loadHistorySource(ctx, id)
			require.NoError(t, err)
			var stored store.SessionMessage
			require.NoError(t, json.Unmarshal([]byte(original.data), &stored))
			require.Equal(t, content, stored.Content, "checkpoint preparation must not rewrite the saved source")
		})
	}
}

func TestSessionCheckpointCanonicalizesCurrentReceiptReference(t *testing.T) {
	f := newWorkerContextFixture(t, 32768)
	ctx := redact.WithTrackedSecrets(context.Background())
	_, err := f.store.AppendContextMessages(ctx, f.write, []store.SessionMessage{
		{ID: "reference-source", Role: "assistant", Content: "Approved record."},
	})
	require.NoError(t, err)
	page, err := f.store.ReadSessionHistory(ctx, store.SessionHistoryRead{
		Namespace: f.write.Namespace, SessionName: f.write.SessionName, MessageID: "reference-source", Limit: 4096,
	})
	require.NoError(t, err)
	data, err := json.Marshal(page)
	require.NoError(t, err)
	const value = "opaque-shadowed-copy-placeholder"
	fragment := value[:len(value)-1]
	prompt := `{"data":"` + fragment + `",` + string(data[1:])
	state, active, err := newWorkerSessionContext(ctx, []llm.Message{{Role: "user", Content: prompt}})
	require.NoError(t, err)
	redact.TrackSecrets(ctx, value)
	note, err := json.Marshal(checkpointDraft{Goal: checkpointClaim{
		Text: "Continue safely.", Sources: []string{state.current.ID},
	}})
	require.NoError(t, err)
	provider := contextFixtureProvider{complete: func(_ context.Context,
		req *llm.CompletionRequest) (*llm.CompletionResponse, error) {
		require.NotContains(t, req.Messages[0].Content, fragment)
		return &llm.CompletionResponse{Content: string(note), StopReason: "stop"}, nil
	}}
	_, err = state.makeCheckpoint(ctx, provider, &llm.CompletionRequest{
		Model: "fixture", Messages: active, ContextWindow: 32768, MaxTokens: 512,
	}, common.NoopEventRecorder{})
	require.NoError(t, err)
	require.Equal(t, prompt, state.current.Content)
	stored, err := state.loadHistorySource(ctx, state.current.ID)
	require.NoError(t, err)
	require.NotContains(t, stored.data, fragment)
}

func TestSessionHistoryBoundsReceiptTraversal(t *testing.T) {
	t.Run("depth", func(t *testing.T) {
		newWorkerContextFixture(t, 32768)
		ctx := redact.WithTrackedSecrets(context.Background())
		state, _, err := newWorkerSessionContext(ctx, []llm.Message{{Role: "user", Content: "Continue safely."}})
		require.NoError(t, err)
		id := state.current.ID
		for range maxHistoryReceiptDepth {
			args, err := json.Marshal(map[string]any{"message_id": id, "limit": store.MaxSessionHistoryReadBytes})
			require.NoError(t, err)
			data, err := state.readHistory(ctx, args)
			require.NoError(t, err)
			receipt, err := state.persist(ctx, llm.Message{Role: "tool", Name: readSessionHistoryTool, Content: data})
			require.NoError(t, err)
			id = receipt.ID
		}
		args, err := json.Marshal(map[string]any{"message_id": id})
		require.NoError(t, err)
		_, err = state.readHistory(ctx, args)
		require.ErrorContains(t, err, "nesting exceeds")
	})
	t.Run("cycle and cancellation", func(t *testing.T) {
		page := store.SessionHistoryResult{MessageID: "cycle-source", Role: "assistant", NextOffset: 1, Data: "{"}
		var data []byte
		for range 4 {
			page.TotalBytes = len(data)
			content, err := json.Marshal(page)
			require.NoError(t, err)
			data, err = json.Marshal(store.SessionMessage{ID: page.MessageID, Role: page.Role, Content: string(content)})
			require.NoError(t, err)
		}
		require.Equal(t, len(data), page.TotalBytes)
		page.Data, page.NextOffset = string(data), len(data)
		state := &workerSessionContext{
			historySource: &workerHistorySource{id: page.MessageID, role: page.Role, data: string(data)},
		}
		_, err := state.historyPageCrossesSecret(context.Background(), page)
		require.ErrorContains(t, err, "cycle")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err = state.historyPageCrossesSecret(ctx, page)
		require.ErrorIs(t, err, context.Canceled)
	})
}

func TestSessionCheckpointRechecksSavedHistoryFragmentsAfterCredentialLoading(t *testing.T) {
	for _, reload := range []bool{false, true} {
		t.Run(fmt.Sprintf("reload=%t", reload), func(t *testing.T) {
			f := newWorkerContextFixture(t, 32768)
			const value = "opaque-late-boundary-placeholder"
			_, err := f.store.AppendContextMessages(context.Background(), f.write, []store.SessionMessage{
				{
					ID: "reference-source", Role: "assistant",
					Content: "Prefix " + value + strings.Repeat(" trailing reference", 600),
				},
			})
			require.NoError(t, err)
			ctx := redact.WithTrackedSecrets(context.Background())
			state, active, err := newWorkerSessionContext(ctx, []llm.Message{{Role: "user", Content: "Continue safely."}})
			require.NoError(t, err)
			full, err := f.store.ReadSessionHistory(ctx, store.SessionHistoryRead{
				Namespace: f.write.Namespace, SessionName: f.write.SessionName,
				MessageID: "reference-source", Limit: store.MaxSessionHistoryReadBytes,
			})
			require.NoError(t, err)
			offset := strings.Index(full.Data, value) + 1
			require.Greater(t, offset, 0)
			args, err := json.Marshal(map[string]any{
				"message_id": "reference-source", "offset": offset, "limit": store.MaxSessionHistoryReadBytes,
			})
			require.NoError(t, err)
			call, err := state.persist(ctx, llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{
				ID: "history-call", Name: readSessionHistoryTool, Arguments: args,
			}}})
			require.NoError(t, err)
			data, err := state.readHistory(ctx, args)
			require.NoError(t, err)
			history, err := state.persist(ctx, llm.Message{Role: "tool", ToolCallID: "history-call",
				Name: readSessionHistoryTool, Content: data})
			require.NoError(t, err)
			active = append(active, call, history)
			require.NotEmpty(t, state.sources[history.ID].Metadata[store.SessionContextOutputRefKey])
			require.Less(t, len(state.sources[history.ID].Content), len(data))
			if reload {
				// Check the persisted preview path as well as locally retained
				// receipt coordinates. Neither cache may authorize a fragment.
				state.historyPages = nil
				state.historySource = nil
			}
			redact.TrackSecrets(ctx, value)
			_, err = state.readHistory(ctx, args)
			require.ErrorContains(t, err, "configured secret")
			note, err := json.Marshal(checkpointDraft{Goal: checkpointClaim{
				Text: "Continue safely.", Sources: []string{state.current.ID},
			}})
			require.NoError(t, err)
			provider := contextFixtureProvider{complete: func(_ context.Context,
				req *llm.CompletionRequest) (*llm.CompletionResponse, error) {
				require.NotContains(t, req.Messages[0].Content, value[1:])
				var payload struct {
					Sources []store.SessionMessage `json:"sources"`
				}
				require.NoError(t, json.Unmarshal([]byte(req.Messages[0].Content), &payload))
				found := false
				for _, source := range payload.Sources {
					if source.ID == history.ID {
						found = true
						require.Contains(t, source.Content, "[REDACTED]")
					}
				}
				require.True(t, found, "checkpoint must preserve the source reference")
				return &llm.CompletionResponse{Content: string(note), StopReason: "stop"}, nil
			}}
			_, err = state.makeCheckpoint(ctx, provider, &llm.CompletionRequest{
				Model: "fixture", Messages: active, ContextWindow: 32768, MaxTokens: 512,
			}, common.NoopEventRecorder{})
			require.NoError(t, err)
			require.Equal(t, data, history.Content, "saved history receipt must remain unchanged")
		})
	}
}

func TestSessionCheckpointRedactsReceiptPreviewFromCompleteSource(t *testing.T) {
	for _, role := range []string{"user", "assistant", "tool", "history"} {
		t.Run(role, func(t *testing.T) {
			f := newWorkerContextFixture(t, 32768)
			ctx := redact.WithTrackedSecrets(context.Background())
			value := strings.Repeat("opaque-page-value-", 600)
			fragment := value[:512]
			_, err := f.store.AppendContextMessages(ctx, f.write, []store.SessionMessage{
				{ID: "reference-source", Role: "assistant",
					Content: "Earlier output: " + value + strings.Repeat(" trailing output", 100)},
			})
			require.NoError(t, err)
			page, err := f.store.ReadSessionHistory(ctx, store.SessionHistoryRead{
				Namespace: f.write.Namespace, SessionName: f.write.SessionName,
				MessageID: "reference-source", Limit: store.MaxSessionHistoryReadBytes,
			})
			require.NoError(t, err)
			require.Equal(t, page.TotalBytes, page.NextOffset)
			require.Contains(t, page.Data, value)
			content, err := json.Marshal(page)
			require.NoError(t, err)
			state, active, err := newWorkerSessionContext(ctx, []llm.Message{{Role: "user", Content: "Continue safely."}})
			require.NoError(t, err)
			message := llm.Message{Role: role, Content: string(content)}
			if role == "history" {
				message.Role = "tool"
			}
			if message.Role == "tool" {
				message.Name, message.ToolCallID = "inspect", "copy-call"
				if role == "history" {
					message.Name = readSessionHistoryTool
				}
				call, err := state.persist(ctx, llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{
					ID: message.ToolCallID, Name: message.Name, Arguments: json.RawMessage(`{}`),
				}}})
				require.NoError(t, err)
				active = append(active, call)
			}
			copied, err := state.persist(ctx, message)
			require.NoError(t, err)
			require.NotEmpty(t, state.sources[copied.ID].Metadata[store.SessionContextOutputRefKey])
			require.Contains(t, state.sources[copied.ID].Content, fragment)
			require.NotContains(t, state.sources[copied.ID].Content, value)
			active = append(active, copied)
			redact.TrackSecrets(ctx, value)
			note, err := json.Marshal(checkpointDraft{Goal: checkpointClaim{
				Text: "Continue safely.", Sources: []string{state.current.ID},
			}})
			require.NoError(t, err)
			provider := contextFixtureProvider{complete: func(_ context.Context,
				req *llm.CompletionRequest) (*llm.CompletionResponse, error) {
				if strings.Contains(req.Messages[0].Content, fragment) {
					t.Error("checkpoint references retained a credential split by the receipt preview")
				}
				return &llm.CompletionResponse{Content: string(note), StopReason: "stop"}, nil
			}}
			_, err = state.makeCheckpoint(ctx, provider, &llm.CompletionRequest{
				Model: "fixture", Messages: active, ContextWindow: 32768, MaxTokens: 512,
			}, common.NoopEventRecorder{})
			require.NoError(t, err)
			original, err := state.loadHistorySource(ctx, copied.ID)
			require.NoError(t, err)
			var stored store.SessionMessage
			require.NoError(t, json.Unmarshal([]byte(original.data), &stored))
			require.Equal(t, string(content), stored.Content, "checkpoint preparation must preserve saved receipt bytes")
			require.Equal(t, copied.Content, active[len(active)-1].Content)
		})
	}
}

func TestSessionCheckpointRedactsShortCredentialAtReceiptPreviewCut(t *testing.T) {
	f := newWorkerContextFixture(t, 32768)
	ctx := redact.WithTrackedSecrets(context.Background())
	const value = "opaque-short-page-placeholder"
	_, err := f.store.AppendContextMessages(ctx, f.write, []store.SessionMessage{
		{ID: "reference-source", Role: "assistant",
			Content: strings.Repeat("x", 9*1024) + value + strings.Repeat(" trailing output", 100)},
	})
	require.NoError(t, err)
	full, err := f.store.ReadSessionHistory(ctx, store.SessionHistoryRead{
		Namespace: f.write.Namespace, SessionName: f.write.SessionName,
		MessageID: "reference-source", Limit: store.MaxSessionHistoryReadBytes,
	})
	require.NoError(t, err)
	state, active, err := newWorkerSessionContext(ctx, []llm.Message{{Role: "user", Content: "Continue safely."}})
	require.NoError(t, err)
	copyID := sessioncontext.TaskMessagePrefix(state.taskUID) + "short-page-preview"
	notice := "\n[Full saved message: " + copyID + ". Use read_session_history for details.]"
	cut := store.MaxSessionContextPreviewBytes - len(notice)
	empty, err := json.Marshal(store.SessionHistoryResult{MessageID: full.MessageID, Role: full.Role})
	require.NoError(t, err)
	dataStart := strings.Index(string(empty), `"data":"`) + len(`"data":"`)
	offset := strings.Index(full.Data, value) - (cut - dataStart - len(value) + 1)
	require.Greater(t, offset, 0)
	page, err := f.store.ReadSessionHistory(ctx, store.SessionHistoryRead{
		Namespace: f.write.Namespace, SessionName: f.write.SessionName,
		MessageID: full.MessageID, Offset: offset, Limit: store.MaxSessionHistoryReadBytes,
	})
	require.NoError(t, err)
	require.Contains(t, page.Data, value)
	content, err := json.Marshal(page)
	require.NoError(t, err)
	require.Equal(t, cut, strings.Index(string(content), value)+len(value)-1)
	copied, err := state.persist(ctx, llm.Message{ID: copyID, Role: "user", Content: string(content)})
	require.NoError(t, err)
	fragment := value[:len(value)-1]
	require.Contains(t, state.sources[copyID].Content, fragment)
	require.NotContains(t, state.sources[copyID].Content, value)
	active = append(active, copied)
	redact.TrackSecrets(ctx, value)
	note, err := json.Marshal(checkpointDraft{Goal: checkpointClaim{
		Text: "Continue safely.", Sources: []string{state.current.ID},
	}})
	require.NoError(t, err)
	provider := contextFixtureProvider{complete: func(_ context.Context,
		req *llm.CompletionRequest) (*llm.CompletionResponse, error) {
		if strings.Contains(req.Messages[0].Content, fragment) {
			t.Error("checkpoint references retained all but the final credential byte")
		}
		return &llm.CompletionResponse{Content: string(note), StopReason: "stop"}, nil
	}}
	_, err = state.makeCheckpoint(ctx, provider, &llm.CompletionRequest{
		Model: "fixture", Messages: active, ContextWindow: 32768, MaxTokens: 512,
	}, common.NoopEventRecorder{})
	require.NoError(t, err)
	original, err := state.loadHistorySource(ctx, copyID)
	require.NoError(t, err)
	var stored store.SessionMessage
	require.NoError(t, json.Unmarshal([]byte(original.data), &stored))
	require.Equal(t, string(content), stored.Content, "checkpoint preparation must preserve saved receipt bytes")
}

func TestSessionContextPreservesCallIdentityAcrossLateCredentialLoading(t *testing.T) {
	f := newWorkerContextFixture(t, 32768)
	const fixtureValue = "opaque-tool-placeholder"
	ctx := redact.WithTrackedSecrets(context.Background())
	state, _, err := newWorkerSessionContext(ctx,
		[]llm.Message{{Role: "user", Content: "Inspect the selected record."}})
	require.NoError(t, err)
	original := llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{
		ID: "call-" + fixtureValue, Name: "inspect",
		Arguments: json.RawMessage(`{"record_id":9007199254740993}`),
	}}}
	assistant, err := state.persist(ctx, original)
	require.NoError(t, err)
	require.NotContains(t, redact.TrackedSecrets(ctx), fixtureValue)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+fixtureValue {
			t.Error("custom tool did not receive its configured credential")
		}
		_, _ = fmt.Fprint(w, "Found the record.")
	}))
	t.Cleanup(server.Close)
	executor := worker.NewToolExecutor()
	executor.SetAuthSecretValue("http-auth", "authref", fixtureValue)
	tool := &corev1alpha1.Tool{Spec: corev1alpha1.ToolSpec{HTTP: &corev1alpha1.HTTPExecution{
		URL: server.URL, AuthSecretRef: &corev1alpha1.SecretKeySelector{Name: "http-auth", Key: "authref"},
	}}}
	output, err := executor.Execute(ctx, tool, nil)
	require.NoError(t, err)
	require.Contains(t, redact.TrackedSecrets(ctx), fixtureValue)
	result, err := state.persist(ctx, llm.Message{
		Role: "tool", ToolCallID: original.ToolCalls[0].ID, Name: "inspect", Content: output,
	})
	require.NoError(t, err)
	require.Equal(t, assistant.ToolCalls[0].ID, result.ToolCallID,
		"credentials learned during execution must not split the call/result identity")
	require.Equal(t, "call-"+fixtureValue, original.ToolCalls[0].ID)
	require.Equal(t, original.ToolCalls[0].Arguments, assistant.ToolCalls[0].Arguments)
	_, err = llm.FitMessages([]llm.Message{assistant, result}, 1000)
	require.NoError(t, err)
	for _, id := range []string{assistant.ID, result.ID} {
		page, readErr := f.store.ReadSessionHistory(ctx, store.SessionHistoryRead{
			Namespace: f.write.Namespace, SessionName: f.write.SessionName, MessageID: id, Limit: 4096,
		})
		require.NoError(t, readErr)
		require.NotContains(t, page.Data, fixtureValue)
	}
}

func TestSessionContextKeepsToolCallIDsDistinctAfterRedaction(t *testing.T) {
	newWorkerContextFixture(t, 32768)
	ctx := redact.WithTrackedSecrets(context.Background())
	values := []string{"first-call-placeholder", "second-call-placeholder"}
	redact.TrackSecrets(ctx, values...)
	state, active, err := newWorkerSessionContext(ctx, []llm.Message{{Role: "user", Content: "Inspect both records."}})
	require.NoError(t, err)
	message := llm.Message{Role: "assistant"}
	for _, value := range values {
		message.ToolCalls = append(message.ToolCalls, llm.ToolCall{
			ID: "call-" + value, Name: "inspect", Arguments: json.RawMessage(`{}`),
		})
	}
	var previousID string
	for range 2 {
		saved, saveErr := state.persist(ctx, message)
		require.NoError(t, saveErr)
		require.NotEqual(t, saved.ToolCalls[0].ID, saved.ToolCalls[1].ID,
			"redacting distinct provider IDs must not collapse them into one identity")
		require.NotEqual(t, previousID, saved.ToolCalls[0].ID,
			"a later call using the same provider ID has its own source identity")
		previousID = saved.ToolCalls[0].ID
		retry := message
		retry.ID = saved.ID
		retried, saveErr := state.persist(ctx, retry)
		require.NoError(t, saveErr)
		require.Equal(t, saved, retried, "an identical save retains its tool identities")
		active = append(active, saved)
		for i, call := range message.ToolCalls {
			result, resultErr := state.persist(ctx, llm.Message{
				Role: "tool", ToolCallID: call.ID, Name: call.Name, Content: "Found the record.",
			})
			require.NoError(t, resultErr)
			require.Equal(t, saved.ToolCalls[i].ID, result.ToolCallID)
			active = append(active, result)
		}
	}
	_, err = llm.FitMessages(active, 1000)
	require.NoError(t, err)
}

func TestSessionContextFailedCallSaveKeepsConfirmedIdentity(t *testing.T) {
	f := newWorkerContextFixture(t, 32768)
	ctx := context.Background()
	state, _, err := newWorkerSessionContext(ctx, []llm.Message{{Role: "user", Content: "Inspect the record."}})
	require.NoError(t, err)
	message := llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{
		ID: "provider-call", Name: "inspect", Arguments: json.RawMessage(`{}`),
	}}}
	confirmed, err := state.persist(ctx, message)
	require.NoError(t, err)
	message.ID = sessioncontext.TaskMessagePrefix(state.taskUID) + "failed"
	f.sourceErrorID.Store(message.ID)
	_, err = state.persist(ctx, message)
	require.Error(t, err)
	result, err := state.persist(ctx, llm.Message{
		Role: "tool", ToolCallID: message.ToolCalls[0].ID, Name: "inspect", Content: "Found the record.",
	})
	require.NoError(t, err)
	require.Equal(t, confirmed.ToolCalls[0].ID, result.ToolCallID,
		"an unconfirmed call must not replace the identity of a saved call")
	_, err = llm.FitMessages([]llm.Message{confirmed, result}, 1000)
	require.NoError(t, err)
}

func TestSessionContextRejectsAmbiguousToolCallIDsBeforeSave(t *testing.T) {
	for _, ids := range [][]string{{""}, {"duplicate", "duplicate"}} {
		t.Run(strings.Join(ids, "/"), func(t *testing.T) {
			f := newWorkerContextFixture(t, 32768)
			ctx := context.Background()
			state, _, err := newWorkerSessionContext(ctx, []llm.Message{{Role: "user", Content: "Inspect the record."}})
			require.NoError(t, err)
			message := llm.Message{Role: "assistant"}
			for _, id := range ids {
				message.ToolCalls = append(message.ToolCalls, llm.ToolCall{
					ID: id, Name: "inspect", Arguments: json.RawMessage(`{}`),
				})
			}
			_, err = state.persist(ctx, message)
			require.ErrorContains(t, err, "distinct, nonempty IDs")
			saved, err := f.store.LoadTranscript(ctx, f.write.Namespace, f.write.SessionName, 0)
			require.NoError(t, err)
			require.Len(t, saved, 1, "the ambiguous call batch must not be committed")
		})
	}
}

func TestSessionCheckpointRebuildsPreviewsWithLargeJSONNumbers(t *testing.T) {
	newWorkerContextFixture(t, 32768)
	ctx := context.Background()
	state, active, err := newWorkerSessionContext(ctx,
		[]llm.Message{{Role: "user", Content: "Continue the numeric investigation."}})
	require.NoError(t, err)
	arguments := json.RawMessage(`{"huge":1e1000,"record_id":9007199254740993}`)
	assistant, err := state.persist(ctx, llm.Message{Role: "assistant",
		Content:   strings.Repeat("Earlier numeric evidence. ", 600),
		ToolCalls: []llm.ToolCall{{ID: "numeric-call", Name: "inspect", Arguments: arguments}},
	})
	require.NoError(t, err, "valid JSON numbers are accepted by persistence")
	require.Equal(t, assistant.ID, state.sources[assistant.ID].Metadata[store.SessionContextOutputRefKey])
	result, err := state.persist(ctx, llm.Message{
		Role: "tool", ToolCallID: "numeric-call", Name: "inspect", Content: "The numeric record is available.",
	})
	require.NoError(t, err)
	active = append(active, assistant, result)
	note, err := json.Marshal(checkpointDraft{Goal: checkpointClaim{
		Text: "Continue the numeric investigation.", Sources: []string{state.current.ID},
	}})
	require.NoError(t, err)
	calls := 0
	provider := contextFixtureProvider{complete: func(_ context.Context,
		req *llm.CompletionRequest) (*llm.CompletionResponse, error) {
		calls++
		require.Contains(t, req.Messages[0].Content, "1e1000")
		require.Contains(t, req.Messages[0].Content, "9007199254740993")
		return &llm.CompletionResponse{Content: string(note), StopReason: "stop"}, nil
	}}
	_, err = state.makeCheckpoint(ctx, provider, &llm.CompletionRequest{
		Model: "fixture", Messages: active, ContextWindow: 32768, MaxTokens: 512,
	}, common.NoopEventRecorder{})
	require.NoError(t, err, "restoring the preview must accept numbers that persistence preserved")
	require.Equal(t, 1, calls)
	require.Equal(t, arguments, assistant.ToolCalls[0].Arguments)
}
