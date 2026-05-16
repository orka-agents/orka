package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
)

func TestContextTokenTaskCreateFailures(t *testing.T) {
	cfg := enforceContextTokenAuthorizationConfig()
	authzCtx := testTaskCreateAuthorizationContext()

	t.Run("allows matching task create context", func(t *testing.T) {
		token := &ContextToken{
			Scopes: []string{ContextTokenScopeTaskCreate},
			TransactionContext: map[string]any{
				"namespace":        "team-a",
				"taskType":         string(corev1alpha1.TaskTypeAgent),
				"agent":            "team-a/codex",
				"allowedAgents":    []any{"team-a/codex", "team-a/claude"},
				"provider":         "team-a/openai-prod",
				"allowedProviders": []any{"openai-prod", "anthropic-prod"},
				"model":            "gpt-4o",
				"allowedModels":    []any{"openai-prod/gpt-4o", "anthropic-prod/claude-sonnet-4"},
				"repo":             "https://github.com/example/repo",
				"branch":           "main",
				"ref":              "abc123",
				"allowedTools":     []any{"search", "Bash"},
			},
		}

		failures := contextTokenTaskCreateFailures(token, cfg, authzCtx)
		require.Empty(t, failures)
	})

	t.Run("reports scope and context mismatches", func(t *testing.T) {
		token := &ContextToken{
			Scopes: []string{ContextTokenScopeTaskGet},
			TransactionContext: map[string]any{
				"namespace":        "team-b",
				"taskType":         string(corev1alpha1.TaskTypeContainer),
				"agent":            "team-b/other-agent",
				"allowedAgents":    []any{"team-b/other-agent"},
				"provider":         "anthropic-prod",
				"allowedProviders": []any{"anthropic-prod"},
				"model":            "claude-sonnet-4",
				"allowedModels":    []any{"anthropic-prod/claude-sonnet-4"},
				"repo":             "https://github.com/example/other-repo",
				"branch":           "release",
				"ref":              "def456",
				"allowedTools":     []any{"search"},
			},
		}

		failures := contextTokenTaskCreateFailures(token, cfg, authzCtx)
		joined := strings.Join(failures, "\n")
		require.Contains(t, joined, `missing one of required scopes "orka:tasks:create"`)
		require.Contains(t, joined, `namespace "team-a" does not match token context "team-b"`)
		require.Contains(t, joined, `agent namespace "team-a" does not match token context "team-b"`)
		require.Contains(t, joined, `provider namespace "team-a" does not match token context "team-b"`)
		require.Contains(t, joined, `task type "agent" does not match token context "container"`)
		require.Contains(t, joined, `agent "team-a/codex" does not match token context "team-b/other-agent"`)
		require.Contains(t, joined, `provider "team-a/openai-prod" is not allowed by token context`)
		require.Contains(t, joined, `model "gpt-4o" does not match token context "claude-sonnet-4"`)
		require.Contains(t, joined, `workspace repo "https://github.com/example/repo" does not match token context "https://github.com/example/other-repo"`)
		require.Contains(t, joined, `workspace branch "main" does not match token context "release"`)
		require.Contains(t, joined, `workspace ref "abc123" does not match token context "def456"`)
		require.Contains(t, joined, `tool "Bash" is not allowed by token context`)
	})
}

func TestContextTokenProviderUseFailures(t *testing.T) {
	cfg := enforceContextTokenAuthorizationConfig()
	provider := ProviderResolutionInfo{Name: "openai-prod", Namespace: "team-a", Type: "openai"}

	t.Run("allows matching provider context", func(t *testing.T) {
		token := &ContextToken{
			Scopes: []string{ContextTokenScopeProvidersUse},
			TransactionContext: map[string]any{
				"namespace":        "team-a",
				"provider":         "team-a/openai-prod",
				"allowedProviders": "openai-prod,anthropic-prod",
				"model":            "gpt-4o",
				"allowedModels":    []string{"openai-prod/gpt-4o", "anthropic-prod/claude-sonnet-4"},
			},
		}

		failures := contextTokenProviderUseFailures(token, cfg, "team-a", provider, "gpt-4o")
		require.Empty(t, failures)
	})

	t.Run("reports provider context mismatches", func(t *testing.T) {
		token := &ContextToken{
			Scopes: []string{ContextTokenScopeTaskCreate},
			TransactionContext: map[string]any{
				"namespace":        "team-b",
				"provider":         "anthropic-prod",
				"allowedProviders": []any{"anthropic-prod"},
				"model":            "claude-sonnet-4",
				"allowedModels":    []any{"anthropic-prod/claude-sonnet-4"},
			},
		}

		failures := contextTokenProviderUseFailures(token, cfg, "team-a", provider, "gpt-4o")
		joined := strings.Join(failures, "\n")
		require.Contains(t, joined, `missing one of required scopes "orka:providers:use"`)
		require.Contains(t, joined, `namespace "team-a" does not match token context "team-b"`)
		require.Contains(t, joined, `provider namespace "team-a" does not match token context "team-b"`)
		require.Contains(t, joined, `provider "openai-prod" is not allowed by token context`)
		require.Contains(t, joined, `model "gpt-4o" does not match token context "claude-sonnet-4"`)
		require.Contains(t, joined, `model "gpt-4o" is not allowed by token context`)
	})
}

func TestAuthorizeContextTokenActionWithConfig(t *testing.T) {
	cfg := enforceContextTokenAuthorizationConfig()

	t.Run("allows matching scope and namespace", func(t *testing.T) {
		app := fiber.New()
		app.Use(func(c fiber.Ctx) error {
			c.Locals(UserInfoContextKey, &UserInfo{
				AuthType: AuthTypeContextToken,
				ContextToken: &ContextToken{
					Scopes: []string{ContextTokenScopeTaskGet},
					TransactionContext: map[string]any{
						"namespace": "team-a",
					},
				},
			})
			c.Locals(resolvedNamespaceLocalKey, "team-a")
			return c.Next()
		})
		app.Get("/test", func(c fiber.Ctx) error {
			if err := authorizeContextTokenActionWithConfig(c, cfg, "getTask", []string{ContextTokenScopeTaskGet}); err != nil {
				return err
			}
			return c.SendStatus(fiber.StatusNoContent)
		})

		resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/test", nil))
		require.NoError(t, err)
		require.Equal(t, http.StatusNoContent, resp.StatusCode)
	})

	t.Run("denies missing scope", func(t *testing.T) {
		app := fiber.New()
		app.Use(func(c fiber.Ctx) error {
			c.Locals(UserInfoContextKey, &UserInfo{
				AuthType: AuthTypeContextToken,
				ContextToken: &ContextToken{
					Scopes: []string{ContextTokenScopeTaskList},
					TransactionContext: map[string]any{
						"namespace": "team-a",
					},
				},
			})
			c.Locals(resolvedNamespaceLocalKey, "team-a")
			return c.Next()
		})
		app.Get("/test", func(c fiber.Ctx) error {
			return authorizeContextTokenActionWithConfig(c, cfg, "getTask", []string{ContextTokenScopeTaskGet})
		})

		resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/test", nil))
		require.NoError(t, err)
		require.Equal(t, http.StatusForbidden, resp.StatusCode)
	})

	t.Run("audits namespace mismatch without denying", func(t *testing.T) {
		auditCfg := cfg
		auditCfg.Mode = ContextTokenAuthorizationModeAudit
		app := fiber.New()
		app.Use(func(c fiber.Ctx) error {
			c.Locals(UserInfoContextKey, &UserInfo{
				AuthType: AuthTypeContextToken,
				ContextToken: &ContextToken{
					Scopes: []string{ContextTokenScopeTaskGet},
					TransactionContext: map[string]any{
						"namespace": "team-a",
					},
				},
			})
			c.Locals(resolvedNamespaceLocalKey, "team-b")
			return c.Next()
		})
		app.Get("/test", func(c fiber.Ctx) error {
			if err := authorizeContextTokenActionWithConfig(c, auditCfg, "getTask", []string{ContextTokenScopeTaskGet}); err != nil {
				return err
			}
			return c.SendStatus(fiber.StatusNoContent)
		})

		resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/test", nil))
		require.NoError(t, err)
		require.Equal(t, http.StatusNoContent, resp.StatusCode)
	})
}

func TestAuthorizeContextTokenToolUse(t *testing.T) {
	cfg := enforceContextTokenAuthorizationConfig()

	t.Run("allows permitted tools", func(t *testing.T) {
		app := fiber.New()
		app.Use(func(c fiber.Ctx) error {
			c.Locals(UserInfoContextKey, &UserInfo{
				AuthType: AuthTypeContextToken,
				ContextToken: &ContextToken{
					Scopes: []string{ContextTokenScopeToolsUse},
					TransactionContext: map[string]any{
						"allowedTools": "search,read_file",
					},
				},
			})
			return c.Next()
		})
		app.Get("/test", func(c fiber.Ctx) error {
			if err := authorizeContextTokenToolUse(c, cfg, "useTools", []string{"search", "read_file"}); err != nil {
				return err
			}
			return c.SendStatus(fiber.StatusNoContent)
		})

		resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/test", nil))
		require.NoError(t, err)
		require.Equal(t, http.StatusNoContent, resp.StatusCode)
	})

	t.Run("denies unpermitted tools", func(t *testing.T) {
		app := fiber.New()
		app.Use(func(c fiber.Ctx) error {
			c.Locals(UserInfoContextKey, &UserInfo{
				AuthType: AuthTypeContextToken,
				ContextToken: &ContextToken{
					Scopes: []string{ContextTokenScopeToolsUse},
					TransactionContext: map[string]any{
						"allowedTools": []any{"search", "read_file"},
					},
				},
			})
			return c.Next()
		})
		app.Get("/test", func(c fiber.Ctx) error {
			return authorizeContextTokenToolUse(c, cfg, "useTools", []string{"search", "bash"})
		})

		resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/test", nil))
		require.NoError(t, err)
		require.Equal(t, http.StatusForbidden, resp.StatusCode)
	})
}

func TestContextStringSupportsStructuredMaps(t *testing.T) {
	t.Run("map string string", func(t *testing.T) {
		ctx := map[string]string{"namespace": "team-a", "allowedTools": "search, read_file"}

		got, ok := contextString(ctx, "namespace")
		require.True(t, ok)
		require.Equal(t, "team-a", got)

		gotList, ok := contextStringList(ctx, "allowedTools")
		require.True(t, ok)
		require.Equal(t, []string{"search", "read_file"}, gotList)
	})

	t.Run("typed string keyed maps", func(t *testing.T) {
		type contextKey string
		type typedStringMap map[contextKey]string
		type typedListMap map[contextKey][]string
		type typedString string
		type typedStringSlice []typedString
		type typedAliasListMap map[contextKey]typedStringSlice

		got, ok := contextString(typedStringMap{"namespace": "team-b"}, "namespace")
		require.True(t, ok)
		require.Equal(t, "team-b", got)

		gotList, ok := contextStringList(typedListMap{"allowedTools": []string{"search", "read_file"}}, "allowedTools")
		require.True(t, ok)
		require.Equal(t, []string{"search", "read_file"}, gotList)

		gotAliasList, ok := contextStringList(typedAliasListMap{"allowedTools": typedStringSlice{"search", "read_file"}}, "allowedTools")
		require.True(t, ok)
		require.Equal(t, []string{"search", "read_file"}, gotAliasList)
	})

	t.Run("unsupported and empty values", func(t *testing.T) {
		_, ok := contextString(map[string]string{"namespace": "  "}, "namespace")
		require.False(t, ok)

		_, ok = contextStringList(map[string]any{"allowedTools": []any{"search", 42}}, "allowedTools")
		require.False(t, ok)

		_, ok = contextString(map[int]string{1: "team-a"}, "namespace")
		require.False(t, ok)
	})
}

func enforceContextTokenAuthorizationConfig() ContextTokenAuthorizationConfig {
	return ContextTokenAuthorizationConfig{
		Mode:              ContextTokenAuthorizationModeEnforce,
		TaskCreateScopes:  []string{ContextTokenScopeTaskCreate},
		TaskReadScopes:    []string{ContextTokenScopeTaskGet},
		TaskListScopes:    []string{ContextTokenScopeTaskList},
		ToolUseScopes:     []string{ContextTokenScopeToolsUse},
		ProviderUseScopes: []string{ContextTokenScopeProvidersUse},
	}
}

func testTaskCreateAuthorizationContext() contextTokenTaskCreateAuthorizationContext {
	return contextTokenTaskCreateAuthorizationContext{
		Request: CreateTaskRequest{
			Type: corev1alpha1.TaskTypeAgent,
			AI: &corev1alpha1.AISpec{
				Provider: "openai",
				Model:    "gpt-4o",
				Tools:    []string{"search"},
			},
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{
				Workspace: &corev1alpha1.WorkspaceConfig{
					GitRepo: "https://github.com/example/repo",
					Branch:  "main",
					Ref:     "abc123",
				},
				AllowedTools: []string{"Bash"},
			},
		},
		Namespace:           "team-a",
		AgentName:           "codex",
		AgentNamespace:      "team-a",
		EffectiveProvider:   ProviderResolutionInfo{Name: "openai-prod", Namespace: "team-a", Type: "openai"},
		EffectiveModel:      "gpt-4o",
		EffectiveAITools:    []string{"search"},
		RuntimeAllowedTools: []string{"Bash"},
	}
}
