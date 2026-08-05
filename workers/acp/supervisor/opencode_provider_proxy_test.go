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
		[]byte(`{"model":"openai/test-model","max_tokens":32000,"max_completion_tokens":16000}`), nil,
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

func TestNormalizeOpenCodeProviderRequestEnforcesOutputLimit(t *testing.T) {
	for _, test := range []struct {
		name      string
		model     string
		body      string
		wantField string
		want      int64
		wantErr   bool
	}{
		{name: "OpenAI translates max tokens", model: "openai/gpt-5.4", body: `{"model":"openai/gpt-5.4","max_tokens":1024}`, wantField: "max_completion_tokens", want: 1024},
		{name: "injects missing limit", model: "openrouter/anthropic/test-model", body: `{"model":"openrouter/anthropic/test-model"}`, wantField: "max_tokens", want: 4096},
		{name: "clamps max tokens", model: "openrouter/anthropic/test-model", body: `{"model":"openrouter/anthropic/test-model","max_tokens":9000}`, wantField: "max_tokens", want: 4096},
		{name: "preserves lower max tokens", model: "openrouter/anthropic/test-model", body: `{"model":"openrouter/anthropic/test-model","max_tokens":1024}`, wantField: "max_tokens", want: 1024},
		{name: "prefers completion field", model: "openrouter/anthropic/test-model", body: `{"model":"openrouter/anthropic/test-model","max_tokens":3000,"max_completion_tokens":2000}`, wantField: "max_completion_tokens", want: 2000},
		{name: "rejects zero", model: "openrouter/anthropic/test-model", body: `{"model":"openrouter/anthropic/test-model","max_tokens":0}`, wantErr: true},
		{name: "rejects fractional", model: "openrouter/anthropic/test-model", body: `{"model":"openrouter/anthropic/test-model","max_tokens":1.5}`, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := normalizeProviderRequestBody(
				providerKindOpencode,
				test.model,
				providerOpenAIChatCompletionsV1Path,
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
			_, wantModel, _ := strings.Cut(test.model, "/")
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
		})
	}
}
