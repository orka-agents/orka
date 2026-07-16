package cliwrapper

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/workerenv"
)

func TestProtectRuntimeAuthTurnUsesLoopbackProxy(t *testing.T) {
	previousBoundary := runtimeAuthChildBoundaryAvailable
	runtimeAuthChildBoundaryAvailable = func() bool { return true }
	t.Cleanup(func() { runtimeAuthChildBoundaryAvailable = previousBoundary })
	tests := []struct {
		name          string
		runtimeName   string
		baseField     string
		authField     string
		requestHeader string
		upstreamPath  string
	}{
		{
			name: "codex", runtimeName: RuntimeCodex,
			baseField: workerenv.OpenAIBaseURL, authField: workerenv.OpenAIAPIKey,
			requestHeader: "Authorization", upstreamPath: "/v1/responses",
		},
		{
			name: "claude", runtimeName: RuntimeClaude,
			baseField: workerenv.AnthropicBaseURL, authField: workerenv.AnthropicAPIKey,
			requestHeader: "x-api-key", upstreamPath: "/v1/messages",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstreamValue := "upstream-model-value"
			type observedRequest struct {
				path            string
				authorization   string
				anthropicHeader string
			}
			observed := make(chan observedRequest, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				observed <- observedRequest{
					path: r.URL.Path, authorization: r.Header.Get("Authorization"),
					anthropicHeader: r.Header.Get("x-api-key"),
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"ok":true}`)
			}))
			t.Cleanup(upstream.Close)
			basePath := strings.TrimSuffix(tt.upstreamPath, strings.TrimPrefix(tt.upstreamPath, "/v1"))
			turn := TurnContext{
				RuntimeName: tt.runtimeName,
				Metadata:    map[string]string{"runtimeAuthOnly": "true"},
				Env: []string{
					tt.baseField + "=" + upstream.URL + basePath,
					tt.authField + "=" + upstreamValue,
					"NO_PROXY=existing.internal",
					"no_proxy=lower.internal",
				},
			}
			protected, closeProxy, err := protectRuntimeAuthTurn(turn)
			if err != nil {
				t.Fatalf("protectRuntimeAuthTurn() error = %v", err)
			}
			defer closeProxy()
			if strings.Contains(strings.Join(protected.Env, "\n"), upstreamValue) {
				t.Fatalf("protected child environment retained upstream value: %#v", protected.Env)
			}
			for _, name := range []string{"NO_PROXY", "no_proxy"} {
				value := envEntryValue(protected.Env, name)
				for _, want := range []string{"existing.internal", "lower.internal", "127.0.0.1", "localhost"} {
					if !slices.Contains(strings.Split(value, ","), want) {
						t.Fatalf("%s = %q, missing %q", name, value, want)
					}
				}
			}
			localBase := envEntryValue(protected.Env, tt.baseField)
			parsed, err := url.Parse(localBase)
			if err != nil || parsed.Hostname() != "127.0.0.1" {
				t.Fatalf("local base = %q err=%v, want loopback proxy", localBase, err)
			}
			requestURL := strings.TrimRight(localBase, "/") + strings.TrimPrefix(tt.upstreamPath, basePath)
			unauthorized, err := http.Post(requestURL, "application/json", strings.NewReader(`{}`))
			if err != nil {
				t.Fatalf("unauthorized proxy request error = %v", err)
			}
			_ = unauthorized.Body.Close()
			if unauthorized.StatusCode != http.StatusUnauthorized {
				t.Fatalf("unauthorized status = %d, want %d", unauthorized.StatusCode, http.StatusUnauthorized)
			}
			request, err := http.NewRequest(http.MethodPost, requestURL, strings.NewReader(`{}`))
			if err != nil {
				t.Fatalf("NewRequest() error = %v", err)
			}
			proxyValue := envEntryValue(protected.Env, tt.authField)
			if tt.requestHeader == "Authorization" {
				request.Header.Set(tt.requestHeader, "Bearer "+proxyValue)
			} else {
				request.Header.Set(tt.requestHeader, proxyValue)
			}
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatalf("proxy request error = %v", err)
			}
			_ = response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Fatalf("proxy status = %d, want %d", response.StatusCode, http.StatusOK)
			}
			got := <-observed
			if got.path != tt.upstreamPath {
				t.Fatalf("upstream path = %q, want %q", got.path, tt.upstreamPath)
			}
			if tt.runtimeName == RuntimeCodex && got.authorization != "Bearer "+upstreamValue {
				t.Fatalf("upstream authorization was not injected")
			}
			if tt.runtimeName == RuntimeClaude && got.anthropicHeader != upstreamValue {
				t.Fatalf("upstream x-api-key was not injected")
			}
		})
	}
}

func TestRuntimeAuthProxyAddNoProxyHostsMergesCaseVariants(t *testing.T) {
	for _, tt := range []struct {
		name string
		env  string
		want string
	}{
		{name: "uppercase only", env: "NO_PROXY=upper.internal", want: "upper.internal"},
		{name: "lowercase only", env: "no_proxy=lower.internal", want: "lower.internal"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			env := runtimeAuthProxyAddNoProxyHosts([]string{tt.env}, "127.0.0.1", "localhost")
			for _, name := range []string{"NO_PROXY", "no_proxy"} {
				value := envEntryValue(env, name)
				for _, want := range []string{tt.want, "127.0.0.1", "localhost"} {
					if !slices.Contains(strings.Split(value, ","), want) {
						t.Fatalf("%s = %q, missing %q", name, value, want)
					}
				}
			}
		})
	}
}

func TestRuntimeAuthProxyAddNoProxyHostsCollapsesDuplicateKeys(t *testing.T) {
	env := runtimeAuthProxyAddNoProxyHosts([]string{
		"NO_PROXY=base.internal",
		"NO_PROXY=override.internal",
		"no_proxy=lower.internal",
		"OTHER=value",
	}, "127.0.0.1", "localhost")
	for _, name := range []string{"NO_PROXY", "no_proxy"} {
		prefix := name + "="
		count := 0
		for _, entry := range env {
			if strings.HasPrefix(entry, prefix) {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("env = %#v, want exactly one %s entry", env, name)
		}
		value := envEntryValue(env, name)
		for _, want := range []string{"override.internal", "lower.internal", "127.0.0.1", "localhost"} {
			if !slices.Contains(strings.Split(value, ","), want) {
				t.Fatalf("%s = %q, missing %q", name, value, want)
			}
		}
		if slices.Contains(strings.Split(value, ","), "base.internal") {
			t.Fatalf("%s = %q, resurrected overridden base.internal entry", name, value)
		}
	}
}

func TestRuntimeAuthProxyAddNoProxyHostsPreservesProcessEnvironment(t *testing.T) {
	t.Setenv("NO_PROXY", ".svc")
	t.Setenv("no_proxy", ".cluster.local")
	env := runtimeAuthProxyAddNoProxyHosts(nil, "127.0.0.1", "localhost")
	for _, name := range []string{"NO_PROXY", "no_proxy"} {
		value := envEntryValue(env, name)
		for _, want := range []string{".svc", ".cluster.local", "127.0.0.1", "localhost"} {
			if !slices.Contains(strings.Split(value, ","), want) {
				t.Fatalf("%s = %q, missing inherited %q", name, value, want)
			}
		}
	}
}

func TestProtectRuntimeAuthTurnProtectsReadOnlyCodexCredentials(t *testing.T) {
	previousBoundary := runtimeAuthChildBoundaryAvailable
	runtimeAuthChildBoundaryAvailable = func() bool { return true }
	t.Cleanup(func() { runtimeAuthChildBoundaryAvailable = previousBoundary })
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(upstream.Close)
	turn := TurnContext{
		RuntimeName: RuntimeCodex,
		Metadata:    map[string]string{"readOnly": "true"},
		Env: []string{
			workerenv.OpenAIBaseURL + "=" + upstream.URL + "/v1",
			workerenv.OpenAIAPIKey + "=upstream-value",
		},
	}
	protected, closeProxy, err := protectRuntimeAuthTurn(turn)
	if err != nil {
		t.Fatalf("protectRuntimeAuthTurn() error = %v", err)
	}
	defer closeProxy()
	if strings.Contains(strings.Join(protected.Env, "\n"), "upstream-value") {
		t.Fatalf("read-only child environment retained upstream credential: %#v", protected.Env)
	}
	parsed, err := url.Parse(envEntryValue(protected.Env, workerenv.OpenAIBaseURL))
	if err != nil || parsed.Hostname() != "127.0.0.1" {
		t.Fatalf("protected base URL = %q err=%v, want loopback proxy", parsed, err)
	}
}

func TestRuntimeAuthProxyTransportDisablesEnvironmentProxy(t *testing.T) {
	transport := runtimeAuthProxyTransport()
	if transport.Proxy != nil {
		t.Fatal("runtime-auth proxy transport inherited environment proxy routing")
	}
}

func TestProtectRuntimeAuthTurnRejectsClaudeFoundry(t *testing.T) {
	turn := TurnContext{RuntimeName: RuntimeClaude, Metadata: map[string]string{"runtimeAuthOnly": "true"}, Env: []string{
		"CLAUDE_CODE_USE_FOUNDRY=1",
		workerenv.AnthropicAPIKey + "=upstream-value",
	}}
	_, _, err := protectRuntimeAuthTurn(turn)
	if err == nil || !strings.Contains(err.Error(), "does not support Azure AI Foundry") {
		t.Fatalf("protectRuntimeAuthTurn() error = %v, want Foundry rejection", err)
	}
}

func TestRuntimeAuthProxyRestrictsBasePathAndFixedQuery(t *testing.T) {
	previousBoundary := runtimeAuthChildBoundaryAvailable
	runtimeAuthChildBoundaryAvailable = func() bool { return true }
	t.Cleanup(func() { runtimeAuthChildBoundaryAvailable = previousBoundary })
	observed := make(chan *http.Request, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed <- r.Clone(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(upstream.Close)
	turn := TurnContext{
		RuntimeName: RuntimeCodex,
		Metadata:    map[string]string{"runtimeAuthOnly": "true"},
		Env: []string{
			workerenv.OpenAIBaseURL + "=" + upstream.URL + "/v1?fixed=one",
			workerenv.OpenAIAPIKey + "=upstream-value",
		},
	}
	protected, closeProxy, err := protectRuntimeAuthTurn(turn)
	if err != nil {
		t.Fatalf("protectRuntimeAuthTurn() error = %v", err)
	}
	defer closeProxy()
	localBase, err := url.Parse(envEntryValue(protected.Env, workerenv.OpenAIBaseURL))
	if err != nil {
		t.Fatalf("Parse local base error = %v", err)
	}
	proxyValue := envEntryValue(protected.Env, workerenv.OpenAIAPIKey)
	outside := *localBase
	outside.Path = "/admin"
	request, _ := http.NewRequest(http.MethodPost, outside.String(), nil)
	request.Header.Set("Authorization", "Bearer "+proxyValue)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("outside-path request error = %v", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNotFound || len(observed) != 0 {
		t.Fatalf("outside-path status=%d upstreamCalls=%d, want local 404", response.StatusCode, len(observed))
	}
	allowed := *localBase
	allowed.Path = "/v1/responses"
	allowed.RawQuery = "fixed=override&extra=two"
	request, _ = http.NewRequest(http.MethodPost, allowed.String(), nil)
	request.Header.Set("Authorization", "Bearer "+proxyValue)
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("allowed-path request error = %v", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusBadRequest || len(observed) != 0 {
		t.Fatalf("query status=%d upstreamCalls=%d, want local rejection", response.StatusCode, len(observed))
	}
	allowed.RawQuery = ""
	request, _ = http.NewRequest(http.MethodPost, allowed.String(), nil)
	request.Header.Set("Authorization", "Bearer "+proxyValue)
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("allowed-path request error = %v", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("allowed-path status=%d, want %d", response.StatusCode, http.StatusNoContent)
	}
	got := <-observed
	if got.URL.Path != "/v1/responses" || got.URL.Query().Get("fixed") != "one" || got.URL.Query().Get("extra") != "" {
		t.Fatalf("upstream URL = %s, want fixed configured query only", got.URL.String())
	}
}

func TestProtectRuntimeAuthTurnRequiresChildIdentityBoundary(t *testing.T) {
	previousBoundary := runtimeAuthChildBoundaryAvailable
	runtimeAuthChildBoundaryAvailable = func() bool { return false }
	t.Cleanup(func() { runtimeAuthChildBoundaryAvailable = previousBoundary })
	turn := TurnContext{RuntimeName: RuntimeCodex, Metadata: map[string]string{"runtimeAuthOnly": "true"}, Env: []string{
		workerenv.OpenAIAPIKey + "=upstream-value",
	}}
	_, _, err := protectRuntimeAuthTurn(turn)
	if err == nil || !strings.Contains(err.Error(), "dedicated non-root child UID/GID") {
		t.Fatalf("protectRuntimeAuthTurn() error = %v, want child identity boundary rejection", err)
	}
}

func TestProtectRuntimeAuthTurnRejectsUnsupportedRuntime(t *testing.T) {
	turn := TurnContext{RuntimeName: RuntimeGeneric, Metadata: map[string]string{"runtimeAuthOnly": "true"}}
	_, _, err := protectRuntimeAuthTurn(turn)
	if err == nil || !strings.Contains(err.Error(), "does not support runtime") {
		t.Fatalf("protectRuntimeAuthTurn() error = %v, want unsupported runtime rejection", err)
	}
}
