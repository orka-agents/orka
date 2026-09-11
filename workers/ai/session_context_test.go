package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/llm"
	"github.com/orka-agents/orka/internal/sessioncontext"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
	toolspkg "github.com/orka-agents/orka/internal/tools"
	"github.com/orka-agents/orka/internal/workerenv"
	"github.com/orka-agents/orka/workers/common"
)

const contextFixtureUID = "context-worker-uid"

type workerContextFixture struct {
	store           *sqlite.Store
	write           store.SessionContextWrite
	checkpointError atomic.Bool
	sourceErrorID   atomic.Value
}

func newWorkerContextFixture(t *testing.T, window int) *workerContextFixture {
	t.Helper()
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	f := &workerContextFixture{
		store: sqlite.NewStore(db, ":memory:"),
		write: store.SessionContextWrite{Namespace: "default", SessionName: "context-session",
			OwnerName: "context-worker", OwnerUID: contextFixtureUID},
	}
	f.sourceErrorID.Store("")
	require.NoError(t, f.store.CreateSession(context.Background(), &store.SessionRecord{
		Namespace: f.write.Namespace, Name: f.write.SessionName, SessionType: "task",
		ActiveTask: f.write.OwnerName, ActiveTaskUID: f.write.OwnerUID,
	}))
	server := httptest.NewServer(http.HandlerFunc(f.serveHTTP))
	t.Cleanup(server.Close)
	t.Setenv(workerenv.SessionCheckpointsEnabled, "true")
	t.Setenv(workerenv.AIContextWindow, strconv.Itoa(window))
	t.Setenv(workerenv.ControllerURL, server.URL)
	t.Setenv(workerenv.TaskName, f.write.OwnerName)
	t.Setenv(workerenv.TaskUID, f.write.OwnerUID)
	t.Setenv(workerenv.TaskNamespace, f.write.Namespace)
	t.Setenv(workerenv.ServiceAccountToken, "context-test-auth")
	return f
}

func (f *workerContextFixture) serveHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	path := r.URL.Path
	var result any
	var err error
	switch {
	case strings.HasSuffix(path, "/session-context"):
		messages, loadErr := f.store.LoadTranscript(ctx, f.write.Namespace, f.write.SessionName, 64)
		if loadErr != nil {
			err = loadErr
			break
		}
		bootstrap := sessioncontext.Bootstrap{SessionName: f.write.SessionName, Writable: true, Messages: messages}
		for _, message := range messages {
			if message.SourceRef == f.write.OwnerUID && message.ID != sessioncontext.PromptMessageID(f.write.OwnerUID) {
				bootstrap.TaskHistoryExists = true
			}
		}
		bootstrap.Checkpoint, err = f.store.LoadSessionCheckpoint(ctx, f.write.Namespace, f.write.SessionName, "")
		if errors.Is(err, store.ErrNotFound) {
			err = nil
		}
		result = bootstrap
	case strings.HasSuffix(path, "/messages"):
		var messages []store.SessionMessage
		decoder := json.NewDecoder(r.Body)
		decoder.UseNumber()
		err = decoder.Decode(&messages)
		for i := range messages {
			messages[i].SourceType, messages[i].SourceRef = sessioncontext.SourceType, f.write.OwnerUID
			if messages[i].ID == f.sourceErrorID.Load().(string) {
				err = errors.New("injected source failure")
			}
		}
		if err == nil {
			result, err = f.store.AppendContextMessages(ctx, f.write, messages)
		}
	case strings.HasSuffix(path, "/checkpoints"):
		if f.checkpointError.Load() {
			err = errors.New("injected checkpoint failure")
			break
		}
		var checkpoint store.SessionCheckpoint
		err = json.NewDecoder(r.Body).Decode(&checkpoint)
		if err == nil {
			err = f.store.SaveSessionCheckpoint(ctx, f.write, checkpoint)
		}
		if err == nil {
			result, err = f.store.LoadSessionCheckpoint(ctx, f.write.Namespace, f.write.SessionName,
				checkpoint.LastMessageID)
		}
	case strings.Contains(path, "/history/"):
		_, messageID, _ := strings.Cut(path, "/history/")
		messageID, err = url.PathUnescape(messageID)
		if err == nil {
			offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
			limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
			result, err = f.store.ReadSessionHistory(ctx, store.SessionHistoryRead{
				Namespace: f.write.Namespace, SessionName: f.write.SessionName,
				MessageID: messageID, Offset: offset, Limit: limit,
			})
		}
	default:
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

type contextFixtureProvider struct {
	complete func(context.Context, *llm.CompletionRequest) (*llm.CompletionResponse, error)
}

func (p contextFixtureProvider) Name() string { return "context-fixture" }
func (p contextFixtureProvider) Complete(ctx context.Context,
	req *llm.CompletionRequest) (*llm.CompletionResponse, error) {
	return p.complete(ctx, req)
}
func (p contextFixtureProvider) Stream(context.Context, *llm.CompletionRequest) (<-chan llm.StreamChunk, error) {
	return nil, errors.New("stream is not used by checkpoint creation")
}

func fixtureCheckpoint(req *llm.CompletionRequest) *llm.CompletionResponse {
	var input struct {
		Sources []store.SessionMessage `json:"sources"`
	}
	_ = json.Unmarshal([]byte(req.Messages[0].Content), &input)
	ids := make([]string, 0, len(input.Sources))
	for _, source := range input.Sources {
		ids = append(ids, source.ID)
	}
	if len(ids) == 0 {
		return &llm.CompletionResponse{Content: "{}", StopReason: "stop"}
	}
	draft := checkpointDraft{
		Goal:        checkpointClaim{Text: "Finish investigating the failure.", Sources: ids[:1]},
		Constraints: []checkpointClaim{{Text: "Keep the public API unchanged.", Sources: ids[:1]}},
		Findings: []checkpointClaim{{Text: "The cache was ruled out; inspect the parser next.",
			Sources: ids[len(ids)-1:]}},
		Remaining: []string{"Inspect the parser."},
	}
	data, _ := json.Marshal(draft)
	return &llm.CompletionResponse{Content: string(data), StopReason: "stop",
		InputTokens: llm.EstimateRequestTokens(req), OutputTokens: 150}
}

type contextFixtureTool struct {
	run func(context.Context) (string, error)
}

func (contextFixtureTool) Name() string                { return "context_fixture_read" }
func (contextFixtureTool) Description() string         { return "Read fixture evidence" }
func (contextFixtureTool) Parameters() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t contextFixtureTool) Execute(ctx context.Context, _ json.RawMessage) (string, error) {
	return t.run(ctx)
}

func TestSessionCheckpointRepeatedReductionPersistsCompleteSources(t *testing.T) {
	f := newWorkerContextFixture(t, 6000)
	defer replaceDefaultToolRegistryForTest(t)()
	toolRuns := 0
	fullResult := "The cache was ruled out; inspect the parser next.\n" + strings.Repeat("saved evidence ",
		4000) + "recoverable tail"
	toolspkg.DefaultRegistry.Register(contextFixtureTool{run: func(context.Context) (string, error) {
		toolRuns++
		return fullResult, nil
	}})
	current := "Keep the public API unchanged. Investigate the parser failure exactly as requested.\nDo not publish."
	modelCalls, checkpointCalls := 0, 0
	provider := contextFixtureProvider{complete: func(ctx context.Context,
		req *llm.CompletionRequest) (*llm.CompletionResponse, error) {
		require.NoError(t, llm.CheckContextWindow(req))
		if req.SystemPrompt == checkpointInstructions {
			checkpointCalls++
			deadline, ok := ctx.Deadline()
			require.True(t, ok)
			require.LessOrEqual(t, time.Until(deadline), checkpointTimeout)
			require.Equal(t, checkpointOutputTokens, req.MaxTokens)
			require.Empty(t, req.Tools)
			return fixtureCheckpoint(req), nil
		}
		modelCalls++
		require.Equal(t, "Normal system instructions", req.SystemPrompt)
		var exactRequest bool
		for _, message := range req.Messages {
			if message.ID == sessioncontext.PromptMessageID(contextFixtureUID) {
				require.Equal(t, current, message.Content)
				require.Equal(t, "user", message.Role)
				exactRequest = true
			}
			if message.Name == sessioncontext.CheckpointName {
				require.Equal(t, "assistant", message.Role)
			}
		}
		require.True(t, exactRequest)
		_, fitErr := llm.FitMessagesKeeping(req.Messages, 100000, currentUserMessageIndex(req.Messages))
		require.NoError(t, fitErr)
		if modelCalls <= 5 {
			return &llm.CompletionResponse{StopReason: "tool_calls", ToolCalls: []llm.ToolCall{{
				ID: fmt.Sprintf("call-%d", modelCalls), Name: "context_fixture_read", Arguments: json.RawMessage(`{}`),
			}}}, nil
		}
		return &llm.CompletionResponse{Content: "The investigation is complete.", StopReason: "stop"}, nil
	}}
	result, err := executeAgentLoopWithEvents(context.Background(), provider, []llm.Message{{Role: "user",
		Content: current}},
		"Normal system instructions", "fixture", modelSettings{maxTokens: 512}, []llm.Tool{{
			Name: "context_fixture_read"}}, nil, nil, common.NoopEventRecorder{})
	require.NoError(t, err)
	require.Equal(t, "The investigation is complete.", result)
	require.Equal(t, 5, toolRuns)
	require.GreaterOrEqual(t, checkpointCalls, 2)
	messages, err := f.store.LoadTranscript(context.Background(), f.write.Namespace, f.write.SessionName, 0)
	require.NoError(t, err)
	require.Len(t, messages, 12)
	require.Equal(t, sessioncontext.PromptMessageID(contextFixtureUID), messages[0].ID)
	require.Equal(t, current, messages[0].Content)
	require.Equal(t, sessioncontext.FinalMessageID(contextFixtureUID), messages[len(messages)-1].ID)
	for _, message := range messages {
		if message.Role != "tool" {
			continue
		}
		require.LessOrEqual(t, len(message.Content), store.MaxSessionContextPreviewBytes)
		var original strings.Builder
		for offset := 0; ; {
			page, readErr := f.store.ReadSessionHistory(context.Background(), store.SessionHistoryRead{
				Namespace: f.write.Namespace, SessionName: f.write.SessionName, MessageID: message.ID,
				Offset: offset, Limit: 4096,
			})
			require.NoError(t, readErr)
			original.WriteString(page.Data)
			offset = page.NextOffset
			if offset == page.TotalBytes {
				break
			}
		}
		var saved store.SessionMessage
		require.NoError(t, json.Unmarshal([]byte(original.String()), &saved))
		require.Equal(t, fullResult, saved.Content)
	}
	checkpoint, err := f.store.LoadSessionCheckpoint(context.Background(), f.write.Namespace, f.write.SessionName, "")
	require.NoError(t, err)
	for _, id := range checkpoint.SourceMessageIDs {
		_, readErr := f.store.ReadSessionHistory(context.Background(), store.SessionHistoryRead{
			Namespace: f.write.Namespace, SessionName: f.write.SessionName, MessageID: id, Limit: 100,
		})
		require.NoError(t, readErr)
	}
}

func TestSessionCheckpointSourceFailureStopsToolsAndRestart(t *testing.T) {
	f := newWorkerContextFixture(t, 10000)
	defer replaceDefaultToolRegistryForTest(t)()
	toolRuns := 0
	toolspkg.DefaultRegistry.Register(contextFixtureTool{run: func(context.Context) (string, error) {
		toolRuns++
		return "action result", nil
	}})
	f.sourceErrorID.Store(sessioncontext.TaskMessagePrefix(contextFixtureUID) + "2")
	provider := &mockProvider{response: &llm.CompletionResponse{StopReason: "tool_calls", ToolCalls: []llm.ToolCall{
		{ID: "first", Name: "context_fixture_read", Arguments: json.RawMessage(`{}`)},
		{ID: "second", Name: "context_fixture_read", Arguments: json.RawMessage(`{}`)},
	}}}
	current := []llm.Message{{Role: "user", Content: "Do the requested work."}}
	_, err := executeAgentLoopWithEvents(context.Background(), provider, current, "", "fixture",
		modelSettings{maxTokens: 512},
		[]llm.Tool{{Name: "context_fixture_read"}}, nil, nil, common.NoopEventRecorder{})
	require.ErrorContains(t, err, "save Session source")
	require.Equal(t, 1, toolRuns)
	messages, loadErr := f.store.LoadTranscript(context.Background(), f.write.Namespace, f.write.SessionName, 0)
	require.NoError(t, loadErr)
	require.Len(t, messages, 2)
	restarted := &mockProvider{}
	_, err = executeAgentLoopWithEvents(context.Background(), restarted, current, "", "fixture",
		modelSettings{maxTokens: 512}, nil, nil, nil, common.NoopEventRecorder{})
	require.ErrorContains(t, err, "automatic action replay is disabled")
	require.Empty(t, restarted.requests)
	require.Equal(t, 1, toolRuns)
}

func TestSessionCheckpointCancellationCommitsResultBeforeStopping(t *testing.T) {
	f := newWorkerContextFixture(t, 10000)
	defer replaceDefaultToolRegistryForTest(t)()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	toolRuns := 0
	toolspkg.DefaultRegistry.Register(contextFixtureTool{run: func(context.Context) (string, error) {
		toolRuns++
		cancel()
		return "completed before cancellation", nil
	}})
	provider := &mockProvider{response: &llm.CompletionResponse{StopReason: "tool_calls", ToolCalls: []llm.ToolCall{
		{ID: "first", Name: "context_fixture_read", Arguments: json.RawMessage(`{}`)},
		{ID: "second", Name: "context_fixture_read", Arguments: json.RawMessage(`{}`)},
	}}}
	_, err := executeAgentLoopWithEvents(ctx, provider, []llm.Message{{Role: "user",
		Content: "Do the requested work."}}, "", "fixture", modelSettings{maxTokens: 512},
		[]llm.Tool{{Name: "context_fixture_read"}}, nil, nil, common.NoopEventRecorder{})
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 1, toolRuns)
	messages, loadErr := f.store.LoadTranscript(context.Background(), f.write.Namespace, f.write.SessionName, 0)
	require.NoError(t, loadErr)
	require.Len(t, messages, 3)
	require.Equal(t, "completed before cancellation", messages[2].Content)
}

func TestSessionCheckpointSmallerFallbackFitsBeforeDispatch(t *testing.T) {
	f := newWorkerContextFixture(t, 14000)
	for i := range 5 {
		require.NoError(t, f.store.AppendMessages(context.Background(), f.write.Namespace, f.write.SessionName,
			[]store.SessionMessage{{
				ID: fmt.Sprintf("prior-%d", i), Role: "assistant", Content: strings.Repeat("earlier evidence ", 500),
			}}))
	}
	primary := &mockProvider{err: &llm.ProviderError{StatusCode: 503, Message: "unavailable"}}
	concreteCalls := 0
	fallback := contextFixtureProvider{complete: func(_ context.Context,
		req *llm.CompletionRequest) (*llm.CompletionResponse, error) {
		concreteCalls++
		require.Equal(t, "smaller", req.Model)
		require.LessOrEqual(t, llm.EstimateRequestTokens(req)+llm.ResponseTokenReserve(req), 6000)
		if req.SystemPrompt == checkpointInstructions {
			return fixtureCheckpoint(req), nil
		}
		require.True(t, slices.ContainsFunc(req.Messages, func(message llm.Message) bool {
			return message.Name == sessioncontext.CheckpointName
		}))
		return &llm.CompletionResponse{Content: "finished", StopReason: "stop"}, nil
	}}
	provider := llm.NewFallbackProvider(primary, []llm.FallbackEntry{{Provider: fallback, Model: "smaller",
		ContextWindow: 6000}})
	result, err := executeAgentLoopWithEvents(context.Background(), provider,
		[]llm.Message{{Role: "user", Content: "Keep the public API unchanged. Finish the investigation."}},
		"Instructions", "larger", modelSettings{maxTokens: 512}, nil, nil, nil, common.NoopEventRecorder{})
	require.NoError(t, err)
	require.Equal(t, "finished", result)
	require.GreaterOrEqual(t, concreteCalls, 2)
}

func TestSessionCheckpointHistoryToolEnforcesBounds(t *testing.T) {
	f := newWorkerContextFixture(t, 10000)
	state, _, err := newWorkerSessionContext(context.Background(), []llm.Message{{Role: "user",
		Content: "current request"}})
	require.NoError(t, err)
	message, err := state.persist(context.Background(), llm.Message{Role: "assistant",
		Content: strings.Repeat("évidence ", 4000)})
	require.NoError(t, err)
	args, err := json.Marshal(map[string]any{"message_id": message.ID, "limit": 300})
	require.NoError(t, err)
	result, err := state.readHistory(context.Background(), args)
	require.NoError(t, err)
	var page store.SessionHistoryResult
	require.NoError(t, json.Unmarshal([]byte(result), &page))
	require.LessOrEqual(t, len(page.Data), 300)
	require.Equal(t, "assistant", page.Role)
	for _, arguments := range []string{
		`{"message_id":"x","limit":16385}`, `{"message_id":"x","limit":0}`,
		`{"message_id":"x","offset":-1}`, `{"message_id":"x","session_name":"other"}`,
		`{"message_id":"x","offset":2097153}`, `{"message_id":"x"} {}`,
	} {
		_, err = state.readHistory(context.Background(), json.RawMessage(arguments))
		require.Error(t, err)
	}
	_, err = f.store.ReadSessionHistory(context.Background(), store.SessionHistoryRead{Namespace: "other",
		SessionName: f.write.SessionName, MessageID: message.ID, Limit: 100})
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestSessionCheckpointFailureKeepsCommittedActiveHistory(t *testing.T) {
	for _, failure := range []string{"save", "missing reference", "oversized output", "cancelled"} {
		t.Run(failure, func(t *testing.T) {
			f := newWorkerContextFixture(t, 6000)
			state, active, err := newWorkerSessionContext(context.Background(), []llm.Message{{Role: "user",
				Content: "Keep the public API unchanged."}})
			require.NoError(t, err)
			for range 4 {
				message, saveErr := state.persist(context.Background(), llm.Message{Role: "assistant",
					Content: strings.Repeat("source evidence ", 500)})
				require.NoError(t, saveErr)
				active = append(active, message)
			}
			before := slices.Clone(active)
			if failure == "save" {
				f.checkpointError.Store(true)
			}
			provider := contextFixtureProvider{complete: func(context.Context,
				*llm.CompletionRequest) (*llm.CompletionResponse, error) {
				switch failure {
				case "missing reference":
					return &llm.CompletionResponse{Content: `{"goal":{"text":"claim","sources":["fabricated"]}}`,
						StopReason: "stop"}, nil
				case "oversized output":
					return &llm.CompletionResponse{Content: strings.Repeat("x", 6001), StopReason: "stop"}, nil
				case "cancelled":
					return nil, context.Canceled
				default:
					data, _ := json.Marshal(checkpointDraft{Goal: checkpointClaim{Text: "Continue work",
						Sources: []string{active[0].ID}}})
					return &llm.CompletionResponse{Content: string(data), StopReason: "stop"}, nil
				}
			}}
			req := &llm.CompletionRequest{Model: "fixture", Messages: active, ContextWindow: 6000, MaxTokens: 512}
			err = state.fit(context.Background(), provider, req, true, common.NoopEventRecorder{})
			require.Error(t, err)
			require.Equal(t, before, req.Messages)
			saved, loadErr := f.store.LoadTranscript(context.Background(), f.write.Namespace, f.write.SessionName, 0)
			require.NoError(t, loadErr)
			require.Len(t, saved, 5)
		})
	}
}

func TestSessionCheckpointRequiredInputFailsWithoutModelCall(t *testing.T) {
	newWorkerContextFixture(t, 2000)
	provider := &mockProvider{}
	current := strings.Repeat("Exact required instruction. ", 500)
	_, err := executeAgentLoopWithEvents(context.Background(), provider, []llm.Message{{Role: "user",
		Content: current}}, "normal instructions", "fixture", modelSettings{maxTokens: 512}, nil, nil, nil,
		common.NoopEventRecorder{})
	require.ErrorContains(t, err, "none were shortened")
	require.Empty(t, provider.requests)
}

func TestSessionCheckpointClientRejectsOversizedReadReceipt(t *testing.T) {
	f := newWorkerContextFixture(t, 10000)
	_ = f
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("x", 1025))
	}))
	defer server.Close()
	c := &sessionContextClient{endpoint: server.URL, client: server.Client()}
	var output any
	err := c.call(context.Background(), http.MethodGet, "", nil, &output, 1024)
	require.ErrorContains(t, err, "byte allowance")
}
