//go:build e2e

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/llm"
	openaiprovider "github.com/orka-agents/orka/internal/llm/openai"
	"github.com/orka-agents/orka/internal/tools"
)

// TestAIKitFullPromptProbe qualifies representative public API requests before
// CI builds the full stack. It has no default endpoint and never loads images.
func TestAIKitFullPromptProbe(t *testing.T) {
	endpoint := os.Getenv("AIKIT_PROBE_URL")
	if endpoint == "" {
		t.Skip("AIKit full-prompt probe requires an explicitly configured endpoint")
	}
	model := os.Getenv("AIKIT_PROBE_MODEL")
	if model == "" {
		t.Fatal("AIKIT_PROBE_MODEL is required")
	}
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	prompt, err := NewSystemPromptBuilder(client, "orka-system", ACPRuntimeAvailability{
		Codex: true, Claude: true, Copilot: true, OpenCode: true,
	}).BuildSystemPrompt(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	registry := tools.NewRegistry()
	tools.RegisterChatTools(registry)
	previousRegistry := tools.DefaultRegistry
	tools.DefaultRegistry = tools.NewRegistry()
	defer func() { tools.DefaultRegistry = previousRegistry }()
	tools.RegisterBuiltinTools()
	tools.RegisterChatToolsDefault()
	tools.RegisterProxyPRTools(client)
	compat := &llm.CompletionRequest{}
	injectOrkaTools(compat)
	provider, err := openaiprovider.NewProvider(llm.ProviderConfig{
		ProviderType: "openai", BaseURL: strings.TrimRight(endpoint, "/") + "/v1", APIKey: "e2e-local-placeholder",
	})
	if err != nil {
		t.Fatal("cannot create local probe provider")
	}
	type probeResult struct {
		Name         string `json:"name"`
		PromptBytes  int    `json:"promptBytes"`
		ToolCount    int    `json:"toolCount"`
		InputTokens  int    `json:"inputTokens"`
		OutputTokens int    `json:"outputTokens"`
		DurationMS   int64  `json:"durationMs"`
		Passed       bool   `json:"passed"`
		Startup      bool   `json:"startup"`
	}
	results := make([]probeResult, 0, 5)
	startupCtx, stopStartup := context.WithTimeout(t.Context(), 30*time.Minute)
	defer stopStartup()
	preload := os.Getenv("AIKIT_PROBE_PRELOAD_PREFIXES") == "true"
	defer func() {
		if report := os.Getenv("AIKIT_PROBE_REPORT"); report != "" {
			data, marshalErr := json.MarshalIndent(results, "", "  ")
			if marshalErr != nil || os.WriteFile(report, data, 0o600) != nil {
				t.Error("cannot preserve full-prompt probe counters")
			}
		}
	}()
	for _, probe := range []struct {
		name        string
		prompt      string
		tools       []llm.Tool
		message     string
		expected    string
		maxTokens   int
		temperature bool
		startup     bool
	}{
		{name: "chat prefix preload", prompt: prompt, tools: registry.ToLLMTools(tools.ChatToolNames()),
			message: "Reply with exactly ORKA_LIVE_CHAT_OK and nothing else.", expected: "ORKA_LIVE_CHAT_OK", maxTokens: 16, temperature: true, startup: true},
		{name: "chat prefilled", prompt: prompt, tools: registry.ToLLMTools(tools.ChatToolNames()),
			message: "Reply with exactly ORKA_LIVE_CHAT_OK and nothing else.", expected: "ORKA_LIVE_CHAT_OK", maxTokens: 16, temperature: true},
		{name: "chat warm", prompt: prompt, tools: registry.ToLLMTools(tools.ChatToolNames()),
			message: "Reply with exactly ORKA_LIVE_CHAT_OK and nothing else.", expected: "ORKA_LIVE_CHAT_OK", maxTokens: 16, temperature: true},
		{name: "compatibility prefix preload", prompt: coordinatorSystemPrompt("orka-system"), tools: compat.Tools,
			message:  "User request: perform this live Anthropic compatibility connectivity task. Reply with exactly <ORKA_GOAL_STATE_REACHED>\nORKA_LIVE_ANTHROPIC_OK and nothing else. Do not use any tools.",
			expected: "<ORKA_GOAL_STATE_REACHED>\nORKA_LIVE_ANTHROPIC_OK", maxTokens: 128, startup: true},
		{name: "compatibility", prompt: coordinatorSystemPrompt("orka-system"), tools: compat.Tools,
			message:  "User request: perform this live Anthropic compatibility connectivity task. Reply with exactly <ORKA_GOAL_STATE_REACHED>\nORKA_LIVE_ANTHROPIC_OK and nothing else. Do not use any tools.",
			expected: "<ORKA_GOAL_STATE_REACHED>\nORKA_LIVE_ANTHROPIC_OK", maxTokens: 128},
	} {
		if probe.startup && !preload {
			continue
		}
		parent, timeout := t.Context(), 170*time.Second
		if probe.startup {
			// Startup prefix preparation is separate from qualification. The
			// subsequent identical requests retain their 170-second budget.
			parent, timeout = startupCtx, 30*time.Minute
		}
		ctx, cancel := context.WithTimeout(parent, timeout)
		start := time.Now()
		response, callErr := provider.Complete(ctx, &llm.CompletionRequest{
			Model: model, SystemPrompt: probe.prompt,
			Messages:  []llm.Message{{Role: "user", Content: probe.message}},
			MaxTokens: probe.maxTokens, Tools: probe.tools, TemperatureSet: probe.temperature,
		})
		cancel()
		result := probeResult{Name: probe.name, PromptBytes: len(probe.prompt), ToolCount: len(probe.tools), DurationMS: time.Since(start).Milliseconds(), Startup: probe.startup}
		if response != nil {
			result.InputTokens, result.OutputTokens = response.InputTokens, response.OutputTokens
			result.Passed = callErr == nil && response.InputTokens > 0 && response.OutputTokens > 0
			if !probe.startup {
				result.Passed = result.Passed && aikitProbeMatchesResponse(response, probe.expected, probe.maxTokens == 128)
			}
		}
		results = append(results, result)
		t.Logf("%s: promptBytes=%d tools=%d inputTokens=%d outputTokens=%d durationMs=%d passed=%t",
			result.Name, result.PromptBytes, result.ToolCount, result.InputTokens, result.OutputTokens, result.DurationMS, result.Passed)
		if !result.Passed {
			// Caller/provider errors can contain request or response bodies. Keep
			// diagnostics limited to the counters above and a safe classification.
			if callErr != nil {
				t.Fatalf("%s full-prompt inference failed; provider error omitted", probe.name)
			}
			t.Fatalf("%s did not satisfy its completion contract; output omitted", probe.name)
		}
	}
}

// Startup proves completed inference, not test correctness. Qualification keeps
// Chat's exact text and the compatibility test's final-marker containment.
func aikitProbeMatchesResponse(response *llm.CompletionResponse, expected string, compatibility bool) bool {
	if response == nil || len(response.ToolCalls) != 0 {
		return false
	}
	if !compatibility {
		return strings.TrimSpace(response.Content) == expected
	}
	return response.StopReason == "stop" && hasGoalStateSentinelPrefix(response.Content) &&
		strings.Contains(response.Content, "ORKA_LIVE_ANTHROPIC_OK")
}

func TestAIKitFullPromptProbeContract(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" {
			t.Errorf("unexpected probe route: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var body struct {
			Model           string            `json:"model"`
			Instructions    string            `json:"instructions"`
			Tools           []json.RawMessage `json:"tools"`
			MaxOutputTokens int               `json:"max_output_tokens"`
			Temperature     *float64          `json:"temperature"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error("cannot decode synthetic probe request")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		calls.Add(1)
		if body.Model != "test-model" {
			t.Error("configured model was not preserved")
		}
		expected := "ORKA_LIVE_CHAT_OK"
		if body.MaxOutputTokens == 16 {
			if len(body.Instructions) < 15000 || len(body.Tools) != len(tools.ChatToolNames()) || body.MaxOutputTokens != 16 || body.Temperature == nil || *body.Temperature != 0 {
				t.Errorf("Chat probe dropped prompt/schema/limits: bytes=%d tools=%d cap=%d", len(body.Instructions), len(body.Tools), body.MaxOutputTokens)
			}
		} else {
			expected = "<ORKA_GOAL_STATE_REACHED>\nORKA_LIVE_ANTHROPIC_OK"
			if len(body.Instructions) < 30000 || len(body.Tools) != len(builtinProxyTools)+len(coordinatorProxyTools) || body.MaxOutputTokens != 128 || body.Temperature != nil {
				t.Errorf("compatibility probe dropped prompt/schema/limits: bytes=%d tools=%d cap=%d", len(body.Instructions), len(body.Tools), body.MaxOutputTokens)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		response := map[string]any{"id": "resp_probe", "object": "response", "created_at": 1700000000, "status": "completed", "model": "test-model", "output": []map[string]any{{"id": "msg_probe", "type": "message", "role": "assistant", "status": "completed", "content": []map[string]string{{"type": "output_text", "text": expected}}}}, "usage": map[string]int{"input_tokens": 9000, "output_tokens": 16, "total_tokens": 9016}}
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Error("cannot encode synthetic probe response")
		}
	}))
	defer server.Close()
	report := filepath.Join(t.TempDir(), "probe.json")
	t.Setenv("AIKIT_PROBE_URL", server.URL)
	t.Setenv("AIKIT_PROBE_MODEL", "test-model")
	t.Setenv("AIKIT_PROBE_REPORT", report)
	t.Setenv("AIKIT_PROBE_PRELOAD_PREFIXES", "false")
	t.Run("configured endpoint", TestAIKitFullPromptProbe)
	if calls.Load() != 3 {
		t.Fatalf("probe calls = %d, want 3", calls.Load())
	}
	calls.Store(0)
	t.Setenv("AIKIT_PROBE_PRELOAD_PREFIXES", "true")
	t.Run("startup prefix preparation", TestAIKitFullPromptProbe)
	if calls.Load() != 5 {
		t.Fatalf("prefilled probe calls = %d, want 5", calls.Load())
	}
	data, err := os.ReadFile(report)
	if err != nil {
		t.Fatal(err)
	}
	var results []struct {
		Name         string `json:"name"`
		Passed       bool   `json:"passed"`
		InputTokens  int    `json:"inputTokens"`
		OutputTokens int    `json:"outputTokens"`
		Startup      bool   `json:"startup"`
	}
	if err := json.Unmarshal(data, &results); err != nil || len(results) != 5 {
		t.Fatalf("invalid probe counter report: %v", err)
	}
	wantNames := []string{"chat prefix preload", "chat prefilled", "chat warm", "compatibility prefix preload", "compatibility"}
	for i, r := range results {
		if !r.Passed || r.InputTokens != 9000 || r.OutputTokens != 16 {
			t.Fatalf("probe result missing verified token usage: %+v", r)
		}
		if r.Name != wantNames[i] || r.Startup != (i == 0 || i == 3) {
			t.Fatalf("probe startup/qualification ordering mismatch at %d", i)
		}
	}
	var fields []map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	for _, row := range fields {
		for key := range row {
			switch key {
			case "name", "promptBytes", "toolCount", "inputTokens", "outputTokens", "durationMs", "passed", "startup":
			default:
				t.Fatalf("unexpected potentially sensitive report field %q", key)
			}
		}
	}
}

func TestAIKitProbeResponseContract(t *testing.T) {
	for _, tc := range []struct {
		name          string
		text          string
		stopReason    string
		compatibility bool
		toolCall      bool
		want          bool
	}{
		{name: "exact chat", text: "ORKA_LIVE_CHAT_OK", want: true},
		{name: "chat prose rejected", text: "Result: ORKA_LIVE_CHAT_OK"},
		{name: "compat final marker", text: goalStateSentinel + "\nORKA_LIVE_ANTHROPIC_OK", stopReason: "stop", compatibility: true, want: true},
		{name: "compat formatting", text: goalStateSentinel + "\n**ORKA_LIVE_ANTHROPIC_OK**", stopReason: "stop", compatibility: true, want: true},
		{name: "missing final sentinel", text: "ORKA_LIVE_ANTHROPIC_OK", stopReason: "stop", compatibility: true},
		{name: "quoted final sentinel", text: "Example: " + goalStateSentinel + "\nORKA_LIVE_ANTHROPIC_OK", stopReason: "stop", compatibility: true},
		{name: "missing connectivity marker", text: goalStateSentinel + "\nwrong", stopReason: "stop", compatibility: true},
		{name: "incomplete compat", text: goalStateSentinel + "\nORKA_LIVE_ANTHROPIC_OK", stopReason: "length", compatibility: true},
		{name: "tool call rejected", text: "ORKA_LIVE_CHAT_OK", toolCall: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := &llm.CompletionResponse{Content: tc.text, StopReason: tc.stopReason}
			if tc.toolCall {
				response.ToolCalls = []llm.ToolCall{{Name: "noop"}}
			}
			if got := aikitProbeMatchesResponse(response, "ORKA_LIVE_CHAT_OK", tc.compatibility); got != tc.want {
				t.Fatalf("completion contract = %t, want %t", got, tc.want)
			}
		})
	}
}
