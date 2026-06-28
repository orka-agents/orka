/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"strings"
	"testing"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/tools"
)

const (
	compatToolContextNamespace     = "compat-ns"
	compatToolContextGeneratedName = "proxy-test"
	compatToolAuthFailed           = "authorization_failed"
	compatToolOpenAIProviderType   = "openai"
)

func TestCompatProxyToolContextOpenAIProfile(t *testing.T) {
	generated := 0
	ctx := newCompatProxyToolContext(compatProxyToolContextConfig{
		Namespace:        compatToolContextNamespace,
		Provider:         ProviderResolutionInfo{Name: "provider-a", Type: compatToolOpenAIProviderType},
		WatchNamespace:   "watch-ns",
		GenerateTaskName: func() string { generated++; return compatToolContextGeneratedName },
		Profile:          openAICompatProxyToolContextProfile,
	})

	if ctx.Namespace != compatToolContextNamespace || ctx.Tenant != compatToolContextNamespace || ctx.Provider != "provider-a" || ctx.ProviderType != compatToolOpenAIProviderType || ctx.WatchNamespace != "watch-ns" {
		t.Fatalf("tool context fields = %#v", ctx)
	}
	if got := ctx.TaskLabels()["orka.ai/source"]; got != "openai-proxy" {
		t.Fatalf("source label = %q, want openai-proxy", got)
	}
	if got := ctx.GenerateTaskName(); got != compatToolContextGeneratedName || generated != 1 {
		t.Fatalf("GenerateTaskName = %q generated=%d", got, generated)
	}
	if ctx.AuthorizeTaskCreate == nil || ctx.AuthorizeTaskDelete == nil || ctx.AuthorizeAgentCreate == nil || ctx.AuthorizeAgentUpdate == nil || ctx.AuthorizeAgentDelete == nil || ctx.AuthorizeSecretRead == nil {
		t.Fatalf("OpenAI authorizers not fully installed: %#v", ctx)
	}
	if !ctx.RequireSecretReadAuthorization {
		t.Fatal("RequireSecretReadAuthorization = false, want true")
	}
}

func TestCompatProxyToolContextAnthropicProfilePreservesAuthorizerSurface(t *testing.T) {
	ctx := newCompatProxyToolContext(compatProxyToolContextConfig{
		Namespace:        compatToolContextNamespace,
		Provider:         ProviderResolutionInfo{Name: "provider-a", Type: "anthropic"},
		GenerateTaskName: func() string { return compatToolContextGeneratedName },
		Profile:          anthropicCompatProxyToolContextProfile,
	})

	if got := ctx.TaskLabels()["orka.ai/source"]; got != "anthropic-proxy" {
		t.Fatalf("source label = %q, want anthropic-proxy", got)
	}
	if ctx.AuthorizeTaskCreate == nil || ctx.AuthorizeAgentCreate == nil || ctx.AuthorizeSecretRead == nil {
		t.Fatalf("Anthropic required authorizers missing: %#v", ctx)
	}
	if ctx.AuthorizeTaskDelete != nil || ctx.AuthorizeAgentUpdate != nil || ctx.AuthorizeAgentDelete != nil {
		t.Fatalf("Anthropic optional mutation authorizers = taskDelete:%v agentUpdate:%v agentDelete:%v; want nil to preserve behavior", ctx.AuthorizeTaskDelete != nil, ctx.AuthorizeAgentUpdate != nil, ctx.AuthorizeAgentDelete != nil)
	}
}

func TestCompatProxyToolContextTaskLimitIsPerContext(t *testing.T) {
	first := newCompatProxyToolContext(compatProxyToolContextConfig{Profile: openAICompatProxyToolContextProfile})
	second := newCompatProxyToolContext(compatProxyToolContextConfig{Profile: openAICompatProxyToolContextProfile})
	for i := range maxProxyCreatedTasks {
		if err := first.CheckTaskLimit(); err != nil {
			t.Fatalf("CheckTaskLimit before increment %d = %v", i, err)
		}
		first.IncrementTasks()
	}
	if err := first.CheckTaskLimit(); err == nil || err.Type != "limit_reached" || err.Message != "task creation limit reached (max 20)" || err.Suggestion != "Wait for existing tasks to complete" {
		t.Fatalf("CheckTaskLimit after max = %#v, want exact limit error", err)
	}
	if err := second.CheckTaskLimit(); err != nil {
		t.Fatalf("second context CheckTaskLimit = %v, want isolated counter", err)
	}
}

func TestCompatProxyToolContextTaskDeleteAuthorizationUsesContextToken(t *testing.T) {
	authz, err := NewContextTokenAuthorizationConfig(ContextTokenAuthorizationConfigOptions{Mode: ContextTokenAuthorizationModeEnforce})
	if err != nil {
		t.Fatalf("NewContextTokenAuthorizationConfig: %v", err)
	}
	ctx := newCompatProxyToolContext(compatProxyToolContextConfig{
		Profile:             openAICompatProxyToolContextProfile,
		AuthorizationConfig: authz,
		AuthContext:         &ContextToken{Scopes: []string{ContextTokenScopeTaskGet}},
	})

	toolErr := ctx.AuthorizeTaskDelete(context.Background(), &corev1alpha1.Task{})
	if toolErr == nil || toolErr.Type != compatToolAuthFailed || !strings.Contains(toolErr.Message, "openAIToolDeleteTask") || toolErr.Suggestion != "Use a task authorized by the context token" {
		t.Fatalf("AuthorizeTaskDelete = %#v, want OpenAI delete-task authorization error", toolErr)
	}
}

func TestCompatProxyToolContextTaskCreateAuthorizationSuggestion(t *testing.T) {
	authz, err := NewContextTokenAuthorizationConfig(ContextTokenAuthorizationConfigOptions{Mode: ContextTokenAuthorizationModeEnforce})
	if err != nil {
		t.Fatalf("NewContextTokenAuthorizationConfig: %v", err)
	}
	ctx := newCompatProxyToolContext(compatProxyToolContextConfig{
		Profile:             anthropicCompatProxyToolContextProfile,
		AuthorizationConfig: authz,
		AuthContext:         &ContextToken{Scopes: []string{}},
	})
	toolErr := ctx.AuthorizeTaskCreate(context.Background(), &corev1alpha1.Task{})
	if toolErr == nil || toolErr.Type != compatToolAuthFailed || toolErr.Suggestion != "Use a task configuration authorized by the context token" {
		t.Fatalf("AuthorizeTaskCreate = %#v, want task configuration suggestion", toolErr)
	}
}

func TestCompatProxyToolContextSecretReadAuthorizationUsesCredentialSuggestion(t *testing.T) {
	authz, err := NewContextTokenAuthorizationConfig(ContextTokenAuthorizationConfigOptions{Mode: ContextTokenAuthorizationModeEnforce})
	if err != nil {
		t.Fatalf("NewContextTokenAuthorizationConfig: %v", err)
	}
	ctx := newCompatProxyToolContext(compatProxyToolContextConfig{
		Profile:             openAICompatProxyToolContextProfile,
		AuthorizationConfig: authz,
		AuthContext:         &ContextToken{Scopes: []string{}},
	})
	toolErr := ctx.AuthorizeSecretRead(context.Background(), compatToolContextNamespace, "git-creds")
	if toolErr == nil || toolErr.Type != compatToolAuthFailed || !strings.Contains(toolErr.Suggestion, "git credential secret") {
		t.Fatalf("AuthorizeSecretRead = %#v, want credential-read suggestion", toolErr)
	}
}

var _ *tools.ToolContext = newCompatProxyToolContext(compatProxyToolContextConfig{})
