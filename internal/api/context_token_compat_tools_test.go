/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	dto "github.com/prometheus/client_model/go"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/llm"
	"github.com/orka-agents/orka/internal/metrics"
	toolspkg "github.com/orka-agents/orka/internal/tools"
)

func TestContextTokenAllowedToolsFiltersInjectedProxyTools(t *testing.T) {
	provider := newTestOIDCProvider(t)
	ctxTokenConfig := testContextTokenConfig(t, provider, "")
	handler, app := setupTestOpenAIHandler()
	authz, err := NewContextTokenAuthorizationConfig(ContextTokenAuthorizationConfigOptions{Mode: ContextTokenAuthorizationModeEnforce})
	if err != nil {
		t.Fatalf("NewContextTokenAuthorizationConfig returned error: %v", err)
	}
	handler.contextTokenAuthorization = authz

	var gotNames []string
	app.Use(NewAuthMiddleware(handler.client, AuthConfig{ContextTokens: ctxTokenConfig}))
	app.Get("/filter", func(c fiber.Ctx) error {
		compReq := &llm.CompletionRequest{}
		injectOrkaTools(compReq)
		gotNames = completionToolNames(filterCompletionToolsForContextToken(c, handler.contextTokenAuthorization, compReq.Tools))
		return c.SendStatus(http.StatusNoContent)
	})

	token := issueTestContextToken(t, provider, nil, map[string]any{
		"scope": ContextTokenScopeToolsUse,
		"tctx": map[string]any{
			"allowedTools": []string{"file_read"},
		},
	})
	req := httptest.NewRequest(http.MethodGet, "/filter", nil)
	req.Header.Set(TransactionTokenHeaderName, token)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}
	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("StatusCode = %d, want %d. body: %s", resp.StatusCode, http.StatusNoContent, string(body))
	}
	assertCompatToolNames(t, gotNames, []string{"file_read"})
}

func TestOpenAICompat_ContextTokenAllowedToolsFiltersInjectedProxyTools(t *testing.T) {
	captured := make(chan map[string]any, 1)
	mockAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/responses"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprint(w, `{"error":{"message":"not found","type":"invalid_request_error","code":"invalid_url"}}`)
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			select {
			case captured <- body:
			default:
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"id":"chatcmpl-test","object":"chat.completion","created":0,"model":"gpt-4o-mini","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer mockAPI.Close()

	llmProvider := &corev1alpha1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: "openai", Namespace: "default"},
		Spec: corev1alpha1.ProviderSpec{
			Type:         corev1alpha1.ProviderTypeOpenAI,
			DefaultModel: "gpt-4o-mini",
			BaseURL:      mockAPI.URL,
			SecretRef:    corev1alpha1.ProviderSecretRef{Name: "openai-secret", Key: "api-key"},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "openai-secret", Namespace: "default"},
		Data:       map[string][]byte{"api-key": []byte("test-key")},
	}

	provider := newTestOIDCProvider(t)
	ctxTokenConfig := testContextTokenConfig(t, provider, "")
	handler, app := setupTestOpenAIHandler(llmProvider, secret)
	authz, err := NewContextTokenAuthorizationConfig(ContextTokenAuthorizationConfigOptions{Mode: ContextTokenAuthorizationModeEnforce})
	if err != nil {
		t.Fatalf("NewContextTokenAuthorizationConfig returned error: %v", err)
	}
	handler.contextTokenAuthorization = authz

	app.Use(NewAuthMiddleware(handler.client, AuthConfig{ContextTokens: ctxTokenConfig}))
	app.Post("/openai/v1/chat/completions", handler.HandleChatCompletions)

	token := issueTestContextToken(t, provider, nil, map[string]any{
		"scope": ContextTokenScopeProvidersUse + " " + ContextTokenScopeToolsUse,
		"tctx": map[string]any{
			"allowedProviders": []string{"openai"},
			"allowedTools":     []string{"file_read"},
		},
	})
	body := []byte(`{"model":"openai/gpt-4o-mini","messages":[{"role":"user","content":"hello"}],"max_tokens":100}`)
	req := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set(TransactionTokenHeaderName, token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want %d. body: %s", resp.StatusCode, http.StatusOK, string(respBody))
	}

	var upstream map[string]any
	select {
	case upstream = <-captured:
	default:
		t.Fatal("expected OpenAI upstream request to be captured")
	}
	assertCompatToolNames(t, compatRequestToolNames(t, upstream), []string{"file_read"})
}

func TestAnthropicCompat_ContextTokenAllowedToolsFiltersInjectedProxyTools(t *testing.T) {
	captured := make(chan map[string]any, 1)
	mockAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/messages") {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		select {
		case captured <- body:
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"msg_test","type":"message","role":"assistant","model":"claude-sonnet-4-20250514","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer mockAPI.Close()

	llmProvider := &corev1alpha1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: "anthropic", Namespace: "default"},
		Spec: corev1alpha1.ProviderSpec{
			Type:         corev1alpha1.ProviderTypeAnthropic,
			DefaultModel: "claude-sonnet-4-20250514",
			BaseURL:      mockAPI.URL,
			SecretRef:    corev1alpha1.ProviderSecretRef{Name: "anthropic-secret", Key: "api-key"},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "anthropic-secret", Namespace: "default"},
		Data:       map[string][]byte{"api-key": []byte("test-key")},
	}

	provider := newTestOIDCProvider(t)
	ctxTokenConfig := testContextTokenConfig(t, provider, "")
	handler, app := setupTestAnthropicHandler(llmProvider, secret)
	authz, err := NewContextTokenAuthorizationConfig(ContextTokenAuthorizationConfigOptions{Mode: ContextTokenAuthorizationModeEnforce})
	if err != nil {
		t.Fatalf("NewContextTokenAuthorizationConfig returned error: %v", err)
	}
	handler.contextTokenAuthorization = authz

	app.Use(NewAuthMiddleware(handler.client, AuthConfig{ContextTokens: ctxTokenConfig}))
	app.Post("/anthropic/v1/messages", handler.HandleMessages)

	token := issueTestContextToken(t, provider, nil, map[string]any{
		"scope": ContextTokenScopeProvidersUse + " " + ContextTokenScopeToolsUse,
		"tctx": map[string]any{
			"allowedProviders": []string{"anthropic"},
			"allowedTools":     []string{"file_read"},
		},
	})
	body := []byte(`{"model":"anthropic/claude-sonnet-4-20250514","messages":[{"role":"user","content":"hello"}],"max_tokens":100}`)
	req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", bytes.NewReader(body))
	req.Header.Set(TransactionTokenHeaderName, token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want %d. body: %s", resp.StatusCode, http.StatusOK, string(respBody))
	}

	var upstream map[string]any
	select {
	case upstream = <-captured:
	default:
		t.Fatal("expected Anthropic upstream request to be captured")
	}
	assertCompatToolNames(t, compatRequestToolNames(t, upstream), []string{"file_read"})
}

func compatRequestToolNames(t *testing.T, req map[string]any) []string {
	t.Helper()
	toolsAny, ok := req["tools"]
	if !ok {
		t.Fatalf("request did not include tools: %#v", req)
	}
	tools, ok := toolsAny.([]any)
	if !ok {
		t.Fatalf("request tools = %T, want []any", toolsAny)
	}

	names := make([]string, 0, len(tools))
	for _, toolAny := range tools {
		tool, ok := toolAny.(map[string]any)
		if !ok {
			t.Fatalf("tool = %T, want map[string]any", toolAny)
		}
		if name, ok := tool["name"].(string); ok {
			names = append(names, name)
			continue
		}
		function, ok := tool["function"].(map[string]any)
		if !ok {
			t.Fatalf("tool did not include name or function.name: %#v", tool)
		}
		name, ok := function["name"].(string)
		if !ok {
			t.Fatalf("tool function.name = %T, want string", function["name"])
		}
		names = append(names, name)
	}
	return names
}

func assertCompatToolNames(t *testing.T, got, want []string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tool names = %#v, want %#v", got, want)
	}
}

func TestContextTokenConnectorReadScopeGatesListConnections(t *testing.T) {
	provider := newTestOIDCProvider(t)
	ctxTokenConfig := testContextTokenConfig(t, provider, "")
	handler, app := setupTestOpenAIHandler()
	authz, err := NewContextTokenAuthorizationConfig(ContextTokenAuthorizationConfigOptions{Mode: ContextTokenAuthorizationModeEnforce})
	if err != nil {
		t.Fatalf("NewContextTokenAuthorizationConfig returned error: %v", err)
	}
	handler.contextTokenAuthorization = authz

	var gotNames []string
	var gateDenied bool
	app.Use(NewAuthMiddleware(handler.client, AuthConfig{ContextTokens: ctxTokenConfig}))
	app.Get("/filter", func(c fiber.Ctx) error {
		compReq := &llm.CompletionRequest{}
		injectOrkaTools(compReq)
		gotNames = completionToolNames(filterCompletionToolsForContextToken(c, handler.contextTokenAuthorization, compReq.Tools))
		gateDenied = connectorReadToolAuthorizer(GetUserInfo(c), handler.contextTokenAuthorization, true) != nil
		if connectorReadToolAuthorizer(GetUserInfo(c), handler.contextTokenAuthorization, false) == nil {
			t.Error("with connectors disabled the gate must refuse every caller")
		}
		return c.SendStatus(http.StatusNoContent)
	})
	probe := func(scope string) {
		t.Helper()
		token := issueTestContextToken(t, provider, nil, map[string]any{"scope": scope})
		req := httptest.NewRequest(http.MethodGet, "/filter", nil)
		req.Header.Set(TransactionTokenHeaderName, token)
		resp, err := app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second})
		if err != nil || resp.StatusCode != http.StatusNoContent {
			t.Fatalf("probe(%q): status = %v err = %v", scope, resp, err)
		}
	}

	// tools:use alone offers the coordinator tools but not the person's
	// linked accounts; execution is refused the same way.
	probe(ContextTokenScopeToolsUse)
	if slices.Contains(gotNames, "list_connections") || !slices.Contains(gotNames, "create_agent_task") || !gateDenied {
		t.Fatalf("tools:use only: names = %v denied = %t", gotNames, gateDenied)
	}
	probe(ContextTokenScopeToolsUse + " " + ContextTokenScopeConnectorsRead)
	if !slices.Contains(gotNames, "list_connections") || gateDenied {
		t.Fatalf("with connectors:read: names = %v denied = %t", gotNames, gateDenied)
	}
}

// TestConnectorReadToolAuthorizerRecordsAuditFailures allows list_connections
// in audit mode for a token without the connector-read scope but records the
// failure, as the connector routes do; enforce mode refuses and records it.
func TestConnectorReadToolAuthorizerRecordsAuditFailures(t *testing.T) {
	ui := &UserInfo{AuthType: AuthTypeContextToken, Subject: "alice", Issuer: "https://issuer.example.test",
		ContextToken: &ContextToken{Subject: "alice", Issuer: "https://issuer.example.test", Scopes: []string{ContextTokenScopeToolsUse}}}
	counter := func(result string) float64 {
		var m dto.Metric
		if err := metrics.ContextTokenAuthorizationTotal.WithLabelValues("connectorsRead", result, "missing_scope").Write(&m); err != nil {
			t.Fatal(err)
		}
		return m.GetCounter().GetValue()
	}
	for _, tc := range []struct {
		mode   string
		result string
		denied bool
	}{
		{mode: ContextTokenAuthorizationModeAudit, result: "audit"},
		{mode: ContextTokenAuthorizationModeEnforce, result: "denied", denied: true},
	} {
		authz, err := NewContextTokenAuthorizationConfig(ContextTokenAuthorizationConfigOptions{Mode: tc.mode})
		if err != nil {
			t.Fatal(err)
		}
		gate := connectorReadToolAuthorizer(ui, authz, true)
		if gate == nil {
			t.Fatalf("%s: a token without the connector-read scope must be checked at call time", tc.mode)
		}
		before := counter(tc.result)
		if denied := gate(); (denied != nil) != tc.denied {
			t.Fatalf("%s: denied = %+v, want denied = %t", tc.mode, denied, tc.denied)
		}
		if after := counter(tc.result); after != before+1 {
			t.Fatalf("%s: %s failures recorded = %v, want %v", tc.mode, tc.result, after, before+1)
		}
		withScope := *ui
		withScope.ContextToken = &ContextToken{Subject: "alice", Issuer: "https://issuer.example.test", Scopes: []string{ContextTokenScopeConnectorsRead}}
		if connectorReadToolAuthorizer(&withScope, authz, true) != nil {
			t.Fatalf("%s: a token with the connector-read scope needs no check", tc.mode)
		}
	}
}

// fakeLinkedAccounts reports every catalog built-in as bound and counts
// the resolutions that reached it.
type fakeLinkedAccounts struct{ calls *int }

func (f fakeLinkedAccounts) BuiltinToolCredential(context.Context, string) (toolspkg.LinkedAccountCredential, bool, error) {
	*f.calls++
	return toolspkg.LinkedAccountCredential{AccessToken: "linked"}, true, nil
}

// TestScopedLinkedAccountsFollowConnectorReadBoundary covers the linked
// GitHub tools of chat and the compatibility proxies: under enforcement a
// delegated token without the connector-read scope uses no linked account
// and never reaches the resolver, audit mode resolves the link and records
// the missing scope, and a token carrying the scope is not narrowed.
func TestScopedLinkedAccountsFollowConnectorReadBoundary(t *testing.T) {
	requester := &corev1alpha1.RequestedBy{Issuer: "https://issuer.example.test", Subject: "alice"}
	narrowed := &UserInfo{AuthType: AuthTypeContextToken, Subject: "alice", Issuer: requester.Issuer,
		ContextToken: &ContextToken{Subject: "alice", Issuer: requester.Issuer, Scopes: []string{ContextTokenScopeToolsUse}}}
	scoped := &UserInfo{AuthType: AuthTypeContextToken, Subject: "alice", Issuer: requester.Issuer,
		ContextToken: &ContextToken{Subject: "alice", Issuer: requester.Issuer, Scopes: []string{ContextTokenScopeConnectorsRead}}}
	for _, tc := range []struct {
		mode  string
		ui    *UserInfo
		bound bool
	}{
		{mode: ContextTokenAuthorizationModeEnforce, ui: narrowed, bound: false},
		{mode: ContextTokenAuthorizationModeAudit, ui: narrowed, bound: true},
		{mode: ContextTokenAuthorizationModeEnforce, ui: scoped, bound: true},
	} {
		authz, err := NewContextTokenAuthorizationConfig(ContextTokenAuthorizationConfigOptions{Mode: tc.mode})
		if err != nil {
			t.Fatal(err)
		}
		calls := 0
		factory := func(string, *corev1alpha1.RequestedBy) toolspkg.LinkedAccountCredentials {
			return fakeLinkedAccounts{calls: &calls}
		}
		linked := scopedLinkedAccounts(factory, "default", requester, tc.ui, authz)
		if linked == nil {
			t.Fatalf("%s: a resolver is attached for every verified person", tc.mode)
		}
		credential, bound, err := linked.BuiltinToolCredential(context.Background(), "list_pull_requests")
		if err != nil || bound != tc.bound || (bound && credential.AccessToken != "linked") {
			t.Fatalf("%s %v: credential = %+v bound = %t err = %v, want bound = %t", tc.mode, tc.ui.ContextToken.Scopes, credential, bound, err, tc.bound)
		}
		if wantCalls := map[bool]int{true: 1, false: 0}[tc.bound]; calls != wantCalls {
			t.Fatalf("%s: resolver calls = %d, want %d", tc.mode, calls, wantCalls)
		}
	}
	if scopedLinkedAccounts(nil, "default", requester, narrowed, ContextTokenAuthorizationConfig{}) != nil {
		t.Fatal("without a factory there is no resolver")
	}
}

// TestResponsesWiresConnectorSettings covers the OpenAI Responses endpoint:
// it offers list_connections and resolves linked accounts exactly as Chat
// Completions does when connectors are enabled.
func TestResponsesWiresConnectorSettings(t *testing.T) {
	handler, app := setupTestOpenAIHandler()
	handler.config.ConnectorsEnabled = true
	var factoryCalls int
	handler.config.LinkedAccounts = func(string, *corev1alpha1.RequestedBy) toolspkg.LinkedAccountCredentials {
		factoryCalls++
		return nil
	}
	if setup := handler.responsesCoordinatorSetup("default"); !setup.ConnectorsEnabled {
		t.Fatal("the Responses coordinator setup must carry ConnectorsEnabled")
	}
	app.Get("/ctx", func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, &UserInfo{AuthType: AuthTypeOIDC, Subject: "alice", Issuer: "https://issuer.example.test"})
		toolCtx := handler.responsesToolContext(c, "default", ProviderResolutionInfo{})
		if toolCtx == nil || toolCtx.AuthorizeConnectorRead != nil {
			t.Errorf("tool context = %+v, want connector reads allowed", toolCtx)
		}
		return c.SendStatus(http.StatusNoContent)
	})
	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/ctx", nil))
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("probe = %v %v", resp, err)
	}
	if factoryCalls != 1 {
		t.Fatalf("linked-account factory calls = %d, want 1", factoryCalls)
	}
}
