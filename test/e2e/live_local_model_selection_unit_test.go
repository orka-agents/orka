//go:build e2e
// +build e2e

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLiveProxyModelCandidates(t *testing.T) {
	catalog := allModelsFromPayload(map[string]any{
		"data": []any{
			map[string]any{"id": "gpt-5-mini"},
			map[string]any{"id": "claude-haiku-4.5"},
			map[string]any{"id": "qwen-3.5-2b-extra"},
			map[string]any{"id": "qwen-3.5-2b"},
		},
	})
	for _, tc := range []struct {
		name  string
		local string
		want  []string
	}{
		{"cloud defaults", "", []string{"gpt-5-mini", "claude-haiku-4.5"}},
		{"exact local ID", "qwen-3.5-2b", []string{"qwen-3.5-2b"}},
		{"trim local configuration", " qwen-3.5-2b\n", []string{"qwen-3.5-2b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("E2E_LOCAL_MODEL", tc.local)
			got, err := liveProxyModelCandidates(catalog, liveCopilotProxyChatModelPreferences(), "gpt-", "claude-")
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("candidates = %v, error = %v; want %v", got, err, tc.want)
			}
		})
	}
}

func TestLiveLocalModelMissingFails(t *testing.T) {
	t.Setenv("E2E_LOCAL_MODEL", "qwen-3.5-2b")
	for _, ids := range [][]string{
		nil,
		{"gpt-5-mini", "claude-haiku-4.5"},
		{"qwen-3.5-2b-extra"},
		{"QWEN-3.5-2B"},
	} {
		t.Run(fmt.Sprint(ids), func(t *testing.T) {
			catalog := proxyModelCatalog{AllModelIDs: ids}
			// A missing exact ID must fail before making any inference request.
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				t.Error("missing local model must not probe another model")
				w.WriteHeader(http.StatusInternalServerError)
			}))
			defer server.Close()

			for _, selectModel := range []struct {
				name string
				call func() (string, error)
			}{
				{"OpenAI provider", func() (string, error) {
					return firstUsableProxyOpenAIModel(server.URL, catalog, liveProxyOpenAIModelPreferences, "gpt-")
				}},
				{"Anthropic messages", func() (string, error) {
					return firstUsableProxyAnthropicMessagesModel(server.URL, catalog, liveCopilotProxyClaudeModelPreferences, "claude-")
				}},
				{"chat completions", func() (string, error) {
					model, skipReason, err := firstLiveCopilotProxyChatCompletionModel(server.URL, "", catalog, liveCopilotProxyChatModelPreferences(), "gpt-", "claude-")
					if skipReason != "" {
						t.Errorf("local model failure returned skip reason %q", skipReason)
					}
					return model, err
				}},
			} {
				t.Run(selectModel.name, func(t *testing.T) {
					model, err := selectModel.call()
					if model != "" || err == nil || !strings.Contains(err.Error(), `"qwen-3.5-2b" is missing`) {
						t.Fatalf("model = %q, error = %v; want missing exact model error", model, err)
					}
				})
			}
		})
	}
}

func TestFetchProxyModelLocalCatalog(t *testing.T) {
	for _, tc := range []struct {
		name    string
		local   string
		status  int
		body    string
		want    string
		wantErr bool
	}{
		{"local basic catalog", "qwen-3.5-2b", 200, `{"data":[{"id":"gpt-5-mini"},{"id":"qwen-3.5-2b"}]}`, "qwen-3.5-2b", false},
		{"missing local ID", "qwen-3.5-2b", 200, `{"data":[{"id":"gpt-5-mini"}]}`, "", true},
		{"forbidden catalog", "qwen-3.5-2b", 403, `{"error":"forbidden"}`, "", true},
		{"malformed catalog", "qwen-3.5-2b", 200, `not-json`, "", true},
		{"cloud first model", "", 200, `{"data":[{"id":"gpt-5-mini"},{"id":"qwen-3.5-2b"}]}`, "gpt-5-mini", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("E2E_LOCAL_MODEL", tc.local)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Path != "/v1/models" {
					t.Errorf("path = %q, want /v1/models", req.URL.Path)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			for _, baseURL := range []string{server.URL, server.URL + "/v1/"} {
				model, err := fetchProxyModel(baseURL)
				if model != tc.want || (err != nil) != tc.wantErr {
					t.Fatalf("model = %q, error = %v; want %q, error = %t", model, err, tc.want, tc.wantErr)
				}
			}
		})
	}
}

func TestLiveProxyChatCompletionSelection(t *testing.T) {
	for _, tc := range []struct {
		name     string
		local    string
		status   int
		wantErr  bool
		wantSkip bool
	}{
		{"local success", "qwen-3.5-2b", 200, false, false},
		{"local bad request", "qwen-3.5-2b", 400, true, false},
		{"local forbidden", "qwen-3.5-2b", 403, true, false},
		{"local missing endpoint", "qwen-3.5-2b", 404, true, false},
		{"local server error", "qwen-3.5-2b", 500, true, false},
		{"cloud forbidden still skips", "", 403, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("E2E_LOCAL_MODEL", tc.local)
			var requestedModels []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Path != "/v1/chat/completions" {
					t.Errorf("path = %q, want /v1/chat/completions", req.URL.Path)
				}
				if got := req.Header.Get("Authorization"); got != "Bearer "+liveProxyProbeAPIKey {
					t.Error("chat probe did not preserve Authorization header")
				}
				var payload struct {
					Model string `json:"model"`
				}
				if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
					t.Errorf("decode request: %v", err)
				}
				requestedModels = append(requestedModels, payload.Model)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"OK"}}]}`))
			}))
			defer server.Close()
			model, skipReason, err := firstLiveCopilotProxyChatCompletionModel(
				server.URL, liveProxyProbeAPIKey,
				proxyModelCatalog{AllModelIDs: []string{"gpt-5-mini", "qwen-3.5-2b"}},
				liveCopilotProxyChatModelPreferences(), "gpt-", "claude-",
			)
			if (err != nil) != tc.wantErr || (skipReason != "") != tc.wantSkip {
				t.Fatalf("model = %q, skip = %q, error = %v; want error = %t, skip = %t", model, skipReason, err, tc.wantErr, tc.wantSkip)
			}
			wantModel := "gpt-5-mini"
			if tc.local != "" {
				wantModel = tc.local
			}
			if !reflect.DeepEqual(requestedModels, []string{wantModel}) {
				t.Fatalf("requested models = %v, want only %q", requestedModels, wantModel)
			}
			if tc.wantErr || tc.wantSkip {
				if model != "" {
					t.Fatalf("rejected model = %q, want empty", model)
				}
			} else if model != wantModel {
				t.Fatalf("selected model = %q, want %q", model, wantModel)
			}
		})
	}
}

func TestLiveProxyLocalOpenAIModelRequiresTools(t *testing.T) {
	t.Setenv("E2E_LOCAL_MODEL", "qwen-3.5-2b")
	for _, tc := range []struct {
		name    string
		tool    bool
		status  int
		wantErr bool
	}{
		{"completion and tool calls", true, 200, false},
		{"missing tool call", false, 200, true},
		{"forbidden tool completion", true, 403, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			completionCalls, toolCalls := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				var payload struct {
					Model string            `json:"model"`
					Tools []json.RawMessage `json:"tools"`
				}
				if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
					t.Errorf("decode request: %v", err)
				}
				if payload.Model != "qwen-3.5-2b" {
					t.Errorf("requested model = %q, want exact local model", payload.Model)
				}
				if req.Header.Get("Authorization") != "Bearer "+liveProxyProbeAPIKey {
					t.Error("OpenAI provider probe did not preserve Authorization header")
				}
				if req.URL.Path == "/v1/responses" {
					http.NotFound(w, req)
					return
				}
				if req.URL.Path != "/v1/chat/completions" {
					t.Errorf("unexpected path %q", req.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				if len(payload.Tools) == 0 {
					completionCalls++
				} else {
					toolCalls++
					if tc.status != http.StatusOK {
						w.WriteHeader(tc.status)
						_, _ = w.Write([]byte(`{"error":{"message":"permission denied","type":"permission_error"}}`))
						return
					}
					if tc.tool {
						_, _ = w.Write([]byte(`{"id":"tool_probe","model":"qwen-3.5-2b","choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"noop_tool","arguments":"{}"}}]}}]}`))
						return
					}
				}
				_, _ = w.Write([]byte(`{"id":"completion_probe","model":"qwen-3.5-2b","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"OK"}}]}`))
			}))
			defer server.Close()
			model, err := firstUsableProxyOpenAIModel(
				server.URL, proxyModelCatalog{AllModelIDs: []string{"gpt-5-mini", "qwen-3.5-2b"}},
				liveProxyOpenAIModelPreferences, "gpt-",
			)
			if (err != nil) != tc.wantErr {
				t.Fatalf("model = %q, error = %v; want error = %t", model, err, tc.wantErr)
			}
			if !tc.wantErr && model != "qwen-3.5-2b" {
				t.Fatalf("model = %q, want exact local model", model)
			}
			if tc.wantErr && model != "" {
				t.Fatalf("rejected model = %q, want empty", model)
			}
			if completionCalls != 1 || toolCalls != 1 {
				t.Fatalf("completion probes = %d, tool probes = %d; want one of each", completionCalls, toolCalls)
			}
		})
	}
}

func TestLiveProxyOpenAIProbeTimeout(t *testing.T) {
	for _, tc := range []struct {
		name  string
		local string
		want  time.Duration
	}{
		{"cloud budget unchanged", "", 30 * time.Second},
		{"CPU inference budget", "qwen-3.5-2b", 3 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("E2E_LOCAL_MODEL", tc.local)
			if got := liveProxyOpenAIProbeTimeout(); got != tc.want {
				t.Fatalf("probe timeout = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestProbeProxyOpenAIProviderCompletionTimeout(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout time.Duration
		wantErr bool
	}{
		{"deadline expires", 25 * time.Millisecond, true},
		{"completion within budget", time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Path == "/v1/responses" {
					http.NotFound(w, req)
					return
				}
				if req.URL.Path != "/v1/chat/completions" {
					t.Errorf("unexpected path %q", req.URL.Path)
				}
				select {
				case <-req.Context().Done():
					return
				case <-time.After(100 * time.Millisecond):
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"completion_probe","model":"qwen-3.5-2b","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"OK"}}]}`))
			}))
			defer server.Close()
			resp, err := probeProxyOpenAIProviderCompletion(server.URL, "qwen-3.5-2b", "Reply with exactly OK.", nil, tc.timeout)
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "context deadline exceeded") {
					t.Fatalf("completion error = %v, want deadline exceeded", err)
				}
				return
			}
			if err != nil || resp == nil || resp.Content != "OK" {
				t.Fatalf("completion = %+v, error = %v; want OK", resp, err)
			}
		})
	}
}

func TestNormalizeProxyReadyBody(t *testing.T) {
	for _, tc := range []struct {
		name    string
		local   string
		body    string
		status  string
		wantErr bool
	}{
		{"local plain OK", "qwen-3.5-2b", "OK\n", "ready", false},
		{"local empty success", "qwen-3.5-2b", "", "ready", false},
		{"local JSON success", "qwen-3.5-2b", `{"message":"OK"}`, "ready", false},
		{"cloud Vekil ready", "", `{"status":"ready"}`, "ready", false},
		{"cloud Vekil not ready", "", `{"status":"not_ready","error":"unavailable"}`, "not_ready", false},
		{"cloud rejects plain success", "", "OK", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("E2E_LOCAL_MODEL", tc.local)
			ready, err := normalizeProxyReadyBody(tc.body)
			if ready.Status != tc.status || (err != nil) != tc.wantErr {
				t.Fatalf("ready = %+v, error = %v; want status %q, error = %t", ready, err, tc.status, tc.wantErr)
			}
			wantPath := "/readyz"
			if tc.local != "" {
				wantPath = "/healthz"
			}
			if got := proxyReadinessPath(); got != wantPath {
				t.Fatalf("readiness path = %q, want %q", got, wantPath)
			}
		})
	}
}

func TestSelectLiveRuntimeModels(t *testing.T) {
	for _, envVar := range []string{"E2E_LOCAL_MODEL", "E2E_LIVE_CODEX_RUNTIME_MODEL", "E2E_LIVE_OPENCODE_RUNTIME_MODEL", "E2E_LIVE_CLAUDE_RUNTIME_MODEL"} {
		t.Setenv(envVar, "")
	}
	cloudCatalog := proxyModelCatalog{
		AllModelIDs: []string{"gpt-5-mini", "gpt-5.3-codex", "claude-haiku-4.5", "qwen-3.5-2b"},
		SupportedEndpointsByModel: map[string][]string{
			"gpt-5.3-codex": {"/responses"},
			"gpt-5-mini":    {"/chat/completions"},
		},
	}
	localCatalog := allModelsFromPayload(map[string]any{
		"data": []any{
			map[string]any{"id": "gpt-5-mini"},
			map[string]any{"id": "claude-haiku-4.5"},
			map[string]any{"id": "qwen-3.5-2b"},
			map[string]any{"id": "other-local-model"},
			map[string]any{"id": "org/qwen-3.5-2b"},
		},
	})
	for _, tc := range []struct {
		name     string
		local    string
		codex    string
		opencode string
		claude   string
		catalog  proxyModelCatalog
		want     liveRuntimeModels
		wantErr  bool
	}{
		{
			name: "local defaults share actual model", local: "qwen-3.5-2b", catalog: localCatalog,
			want: liveRuntimeModels{codex: "qwen-3.5-2b", opencode: "openai/qwen-3.5-2b", claude: "qwen-3.5-2b"},
		},
		{
			name: "local namespaced actual model", local: "org/qwen-3.5-2b", catalog: localCatalog,
			want: liveRuntimeModels{codex: "org/qwen-3.5-2b", opencode: "openai/org/qwen-3.5-2b", claude: "org/qwen-3.5-2b"},
		},
		{
			name: "explicit local overrides", local: "qwen-3.5-2b", catalog: localCatalog,
			codex: "other-local-model", opencode: "openai/other-local-model", claude: "other-local-model",
			want: liveRuntimeModels{codex: "other-local-model", opencode: "openai/other-local-model", claude: "other-local-model"},
		},
		{name: "missing exact local model", local: "missing", catalog: cloudCatalog, wantErr: true},
		{name: "missing Codex override", local: "qwen-3.5-2b", codex: "missing", catalog: localCatalog, wantErr: true},
		{name: "missing OpenCode override", local: "qwen-3.5-2b", opencode: "openai/missing", catalog: localCatalog, wantErr: true},
		{name: "missing Claude override", local: "qwen-3.5-2b", claude: "missing", catalog: localCatalog, wantErr: true},
		{
			name: "cloud defaults unchanged", catalog: cloudCatalog,
			want: liveRuntimeModels{codex: "gpt-5.3-codex", opencode: "openai/gpt-5-mini", claude: "claude-haiku-4.5"},
		},
		{
			name: "cloud requires advertised endpoints", catalog: localCatalog,
			want: liveRuntimeModels{
				codexSkipReason:    "no Codex-family GPT model with /responses support exposed",
				opencodeSkipReason: "no GPT-family model with /chat/completions support exposed",
				claude:             "claude-haiku-4.5",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("E2E_LOCAL_MODEL", tc.local)
			t.Setenv("E2E_LIVE_CODEX_RUNTIME_MODEL", tc.codex)
			t.Setenv("E2E_LIVE_OPENCODE_RUNTIME_MODEL", tc.opencode)
			t.Setenv("E2E_LIVE_CLAUDE_RUNTIME_MODEL", tc.claude)
			models, err := selectLiveRuntimeModels(tc.catalog)
			if (err != nil) != tc.wantErr || models != tc.want {
				t.Fatalf("models = %+v, error = %v; want %+v, error = %t", models, err, tc.want, tc.wantErr)
			}
		})
	}
}
