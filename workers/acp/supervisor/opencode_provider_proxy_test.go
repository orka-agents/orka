package supervisor

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testOpenCodeProxyModel = "test-model"

func TestProviderProxyAcceptsOpenCodeChatCompletions(t *testing.T) {
	observed := make(chan *http.Request, 1)
	observedBody := make(chan []byte, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		observed <- r.Clone(r.Context())
		observedBody <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()
	_, session, binding := activeTestProviderProxySession(t, ProviderProxyConfig{
		UpstreamBaseURL: upstream.URL, UpstreamBearerToken: "placeholder",
		ProviderKind: providerKindOpencode, Model: "openai/" + testOpenCodeProxyModel, ModelOutputLimit: 4096,
	})
	defer session.close()
	response := doProviderProxyRequest(
		t, http.MethodPost, binding.BaseURL+"/v1/chat/completions", binding.Credential,
		[]byte(`{
			"model":"openai/test-model",
			"max_tokens":32000,
			"max_completion_tokens":16000,
			"reasoning_effort":"medium",
			"verbosity":"low",
			"stream":true,
			"stream_options":{"include_usage":true},
			"messages":[{"role":"user","content":"inspect the workspace"}],
			"tool_choice":"auto",
			"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object"}}}]
		}`), nil,
	)
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("OpenCode chat completions status = %d body=%s", response.StatusCode, body)
	}
	request := <-observed
	if request.URL.Path != "/v1/chat/completions" || request.Method != http.MethodPost {
		t.Fatalf("OpenCode upstream request = %s %s", request.Method, request.URL.Path)
	}
	var payload map[string]any
	if err := json.Unmarshal(<-observedBody, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["model"] != testOpenCodeProxyModel {
		t.Fatalf("OpenCode upstream model = %#v, want %s", payload["model"], testOpenCodeProxyModel)
	}
	if _, exists := payload["max_tokens"]; exists {
		t.Fatalf("OpenCode upstream request retained max_tokens alongside max_completion_tokens: %#v", payload)
	}
	if payload["max_completion_tokens"] != float64(4096) {
		t.Fatalf("OpenCode upstream output limit = %#v, want 4096", payload["max_completion_tokens"])
	}
	if _, exists := payload[providerVerbosityField]; exists {
		t.Fatalf("OpenCode upstream request retained unsupported verbosity: %#v", payload)
	}
	if _, exists := payload[providerReasoningEffortField]; exists {
		t.Fatalf("OpenCode upstream tool request retained unsupported reasoning effort: %#v", payload)
	}
	if payload["stream"] != true {
		t.Fatalf("OpenCode upstream request lost stream setting: %#v", payload)
	}
	streamOptions, ok := payload["stream_options"].(map[string]any)
	if !ok || streamOptions["include_usage"] != true {
		t.Fatalf("OpenCode upstream request lost stream usage setting: %#v", payload)
	}
	tools, ok := payload["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("OpenCode upstream request lost tools: %#v", payload)
	}
	tool, ok := tools[0].(map[string]any)
	if !ok || tool["type"] != "function" {
		t.Fatalf("OpenCode upstream request changed tool declaration: %#v", payload)
	}
	if payload["tool_choice"] != "auto" {
		t.Fatalf("OpenCode upstream request changed tool choice: %#v", payload)
	}
	messages, ok := payload["messages"].([]any)
	if !ok || len(messages) != 1 {
		t.Fatalf("OpenCode upstream request lost messages: %#v", payload)
	}
}

func TestProviderProxyRejectsOpenCodeResponsesAPI(t *testing.T) {
	if _, err := validateProviderRequest(
		providerKindOpencode, testOpenCodeProxyModel, "/v1/responses", http.MethodPost, []byte(`{"model":"test-model"}`),
	); err == nil {
		t.Fatal("OpenCode Responses API request unexpectedly accepted")
	}
}

func TestProviderProxyRejectsOpenCodeModelsAPI(t *testing.T) {
	for _, path := range []string{"/models", providerModelsV1Path} {
		if _, err := validateProviderRequest(providerKindOpencode, "openai/"+testOpenCodeProxyModel, path, http.MethodGet, nil); err == nil {
			t.Fatalf("OpenCode models request %q unexpectedly accepted", path)
		}
	}
}

func TestProviderProxyClassifiesOpenCodeChatAsInference(t *testing.T) {
	class, err := validateProviderRequest(
		providerKindOpencode,
		"openai/"+testOpenCodeProxyModel,
		providerOpenAIChatCompletionsV1Path,
		http.MethodPost,
		[]byte(`{"model":"openai/test-model"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	if class != providerRequestInference {
		t.Fatalf("OpenCode request class = %v, want inference", class)
	}
}

func TestNormalizeProviderRequestClampsOutputLimitForAllProviders(t *testing.T) {
	for _, test := range []struct {
		name      string
		provider  string
		path      string
		limit     int64
		body      string
		wantField string
		want      int64
		wantErr   bool
		unchanged bool
	}{
		{name: "claude clamps oversized max_tokens", provider: "claude", path: "/v1/messages", limit: 4096, body: `{"model":"m","max_tokens":900000}`, wantField: "max_tokens", want: 4096},
		{name: "claude preserves lower max_tokens", provider: "claude", path: "/v1/messages", limit: 4096, body: `{"model":"m","max_tokens":1024}`, wantField: "max_tokens", want: 1024},
		{name: "claude injects missing max_tokens", provider: "claude", path: "/v1/messages", limit: 4096, body: `{"model":"m"}`, wantField: "max_tokens", want: 4096},
		{name: "codex clamps responses max_output_tokens", provider: providerKindCodex, path: providerOpenAIResponsesV1Path, limit: 2048, body: `{"model":"m","max_output_tokens":900000}`, wantField: "max_output_tokens", want: 2048},
		{name: "codex injects responses limit", provider: providerKindCodex, path: "/responses", limit: 2048, body: `{"model":"m"}`, wantField: "max_output_tokens", want: 2048},
		{name: "copilot clamps chat completions", provider: providerKindCopilot, path: providerOpenAIChatCompletionsV1Path, limit: 2048, body: `{"model":"m","max_completion_tokens":900000}`, wantField: "max_completion_tokens", want: 2048},
		{name: "copilot injects chat completions limit", provider: providerKindCopilot, path: providerOpenAIChatCompletionsPath, limit: 2048, body: `{"model":"m"}`, wantField: "max_tokens", want: 2048},
		{name: "claude rejects non-positive max_tokens", provider: "claude", path: "/v1/messages", limit: 4096, body: `{"model":"m","max_tokens":0}`, wantErr: true},
		{name: "claude count_tokens is untouched", provider: "claude", path: "/v1/messages/count_tokens", limit: 4096, body: `{"model":"m"}`, unchanged: true},
		{name: "no configured limit is untouched", provider: providerKindCodex, path: providerOpenAIResponsesV1Path, limit: 0, body: `{"model":"m","max_output_tokens":900000}`, unchanged: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := normalizeProviderRequestBody(test.provider, "m", test.path, test.limit, []byte(test.body))
			if test.wantErr {
				if err == nil {
					t.Fatal("normalizeProviderRequestBody() error = nil, want rejection")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if test.unchanged {
				if string(got) != test.body {
					t.Fatalf("body = %s, want unchanged %s", got, test.body)
				}
				return
			}
			var payload map[string]any
			if err := json.Unmarshal(got, &payload); err != nil {
				t.Fatal(err)
			}
			if payload[test.wantField] != float64(test.want) {
				t.Fatalf("%s = %#v, want %d", test.wantField, payload[test.wantField], test.want)
			}
		})
	}
}

func TestNormalizeOpenCodeProviderRequestEnforcesOutputLimit(t *testing.T) {
	for _, test := range []struct {
		name      string
		model     string
		path      string
		body      string
		wantField string
		want      int64
		verbosity string
		reasoning string
		wantErr   bool
	}{
		{name: "OpenAI translates max tokens", model: "openai/gpt-5.4", body: `{"model":"openai/gpt-5.4","max_tokens":1024}`, wantField: "max_completion_tokens", want: 1024},
		{name: "OpenAI strips unsupported verbosity", model: "openai/gpt-5.4", body: `{"model":"openai/gpt-5.4","max_tokens":1024,"reasoning_effort":"medium","verbosity":"low","stream":true}`, wantField: "max_completion_tokens", want: 1024, reasoning: "medium"},
		{name: "OpenAI strips reasoning effort with tools", model: "openai/gpt-5.4", body: `{"model":"openai/gpt-5.4","max_tokens":1024,"reasoning_effort":"medium","tools":[{"type":"function","function":{"name":"read_file"}}]}`, wantField: "max_completion_tokens", want: 1024},
		{name: "mixed-case OpenAI strips reasoning effort on unversioned path", model: "OpEnAi/gpt-5.4", path: providerOpenAIChatCompletionsPath, body: `{"model":"OpEnAi/gpt-5.4","max_tokens":1024,"reasoning_effort":"medium","tools":[{"type":"function","function":{"name":"read_file"}}]}`, wantField: "max_completion_tokens", want: 1024},
		{name: "OpenAI preserves reasoning effort without tools", model: "openai/gpt-5.4", body: `{"model":"openai/gpt-5.4","max_tokens":1024,"reasoning_effort":"high"}`, wantField: "max_completion_tokens", want: 1024, reasoning: "high"},
		{name: "OpenAI preserves reasoning effort with empty tools", model: "openai/gpt-5.4", body: `{"model":"openai/gpt-5.4","max_tokens":1024,"reasoning_effort":"low","tools":[]}`, wantField: "max_completion_tokens", want: 1024, reasoning: "low"},
		{name: "OpenAI preserves reasoning effort with null tools", model: "openai/gpt-5.4", body: `{"model":"openai/gpt-5.4","max_tokens":1024,"reasoning_effort":"low","tools":null}`, wantField: "max_completion_tokens", want: 1024, reasoning: "low"},
		{name: "OpenAI preserves reasoning effort with malformed tools", model: "openai/gpt-5.4", body: `{"model":"openai/gpt-5.4","max_tokens":1024,"reasoning_effort":"low","tools":{"unexpected":true}}`, wantField: "max_completion_tokens", want: 1024, reasoning: "low"},
		{name: "OpenAI preserves reasoning effort with tool choice only", model: "openai/gpt-5.4", body: `{"model":"openai/gpt-5.4","max_tokens":1024,"reasoning_effort":"low","tool_choice":"auto"}`, wantField: "max_completion_tokens", want: 1024, reasoning: "low"},
		{name: "OpenAI tools do not insert reasoning effort", model: "openai/gpt-5.4", body: `{"model":"openai/gpt-5.4","max_tokens":1024,"tools":[null]}`, wantField: "max_completion_tokens", want: 1024},
		{name: "preserves non-OpenAI compatibility fields", model: "openrouter/openai/gpt-5.4", body: `{"model":"openrouter/openai/gpt-5.4","max_tokens":1024,"reasoning_effort":"medium","verbosity":"high","tools":[{"type":"function","function":{"name":"read_file"}}]}`, wantField: "max_tokens", want: 1024, verbosity: "high", reasoning: "medium"},
		{name: "injects missing limit", model: "openrouter/anthropic/test-model", body: `{"model":"openrouter/anthropic/test-model"}`, wantField: "max_tokens", want: 4096},
		{name: "clamps max tokens", model: "openrouter/anthropic/test-model", body: `{"model":"openrouter/anthropic/test-model","max_tokens":9000}`, wantField: "max_tokens", want: 4096},
		{name: "preserves lower max tokens", model: "openrouter/anthropic/test-model", body: `{"model":"openrouter/anthropic/test-model","max_tokens":1024}`, wantField: "max_tokens", want: 1024},
		{name: "prefers completion field", model: "openrouter/anthropic/test-model", body: `{"model":"openrouter/anthropic/test-model","max_tokens":3000,"max_completion_tokens":2000}`, wantField: "max_completion_tokens", want: 2000},
		{name: "rejects zero", model: "openrouter/anthropic/test-model", body: `{"model":"openrouter/anthropic/test-model","max_tokens":0}`, wantErr: true},
		{name: "rejects fractional", model: "openrouter/anthropic/test-model", body: `{"model":"openrouter/anthropic/test-model","max_tokens":1.5}`, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			requestPath := test.path
			if requestPath == "" {
				requestPath = providerOpenAIChatCompletionsV1Path
			}
			got, err := normalizeProviderRequestBody(
				providerKindOpencode,
				test.model,
				requestPath,
				4096,
				[]byte(test.body),
			)
			if test.wantErr {
				if err == nil {
					t.Fatal("normalizeProviderRequestBody() error = nil, want rejection")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var payload map[string]any
			if err := json.Unmarshal(got, &payload); err != nil {
				t.Fatal(err)
			}
			providerID, wantModel, _ := strings.Cut(test.model, "/")
			if payload["model"] != wantModel {
				t.Fatalf("model = %#v, want %s", payload["model"], wantModel)
			}
			if payload[test.wantField] != float64(test.want) {
				t.Fatalf("%s = %#v, want %d", test.wantField, payload[test.wantField], test.want)
			}
			other := "max_completion_tokens"
			if test.wantField == other {
				other = "max_tokens"
			}
			if _, exists := payload[other]; exists {
				t.Fatalf("unexpected alternate output limit %s in %#v", other, payload)
			}
			verbosity, exists := payload[providerVerbosityField]
			if test.verbosity != "" {
				if !exists || verbosity != test.verbosity {
					t.Fatalf("OpenCode upstream request verbosity = %#v, want %q in %#v", verbosity, test.verbosity, payload)
				}
			} else if exists {
				t.Fatalf("OpenAI-backed OpenCode request retained unsupported verbosity: %#v", payload)
			}
			reasoning, exists := payload[providerReasoningEffortField]
			if test.reasoning != "" {
				if !exists || reasoning != test.reasoning {
					t.Fatalf("OpenCode upstream reasoning effort = %#v, want %q in %#v", reasoning, test.reasoning, payload)
				}
			} else if exists {
				t.Fatalf("OpenAI tool-bearing request retained unsupported reasoning effort: %#v", payload)
			}
			if strings.EqualFold(providerID, "openai") && strings.Contains(test.body, providerVerbosityField) && payload["stream"] != true {
				t.Fatalf("OpenCode upstream request lost stream setting: %#v", payload)
			}
		})
	}
}
