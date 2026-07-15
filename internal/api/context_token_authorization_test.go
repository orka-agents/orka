package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
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

	t.Run("allows matching ref-only workspace with branch and ref context", func(t *testing.T) {
		token := &ContextToken{
			Scopes: []string{ContextTokenScopeTaskCreate},
			TransactionContext: map[string]any{
				"namespace": "team-a",
				"taskType":  string(corev1alpha1.TaskTypeAgent),
				"agent":     "team-a/codex",
				"provider":  "team-a/openai-prod",
				"model":     "gpt-4o",
				"repo":      "https://github.com/example/repo",
				"branch":    "main",
				"ref":       "abc123",
			},
		}
		authzCtx := testTaskCreateAuthorizationContext()
		authzCtx.Request.AgentRuntime.Workspace.Branch = ""

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

	t.Run("rejects unrestricted agent runtime when token restricts tools", func(t *testing.T) {
		token := &ContextToken{
			Scopes: []string{ContextTokenScopeTaskCreate},
			TransactionContext: map[string]any{
				"allowedTools": []any{"Bash"},
			},
		}
		authzCtx := testTaskCreateAuthorizationContext()
		authzCtx.RuntimeAllowedTools = nil

		failures := contextTokenTaskCreateFailures(token, cfg, authzCtx)
		require.Contains(t, strings.Join(failures, "\n"), "agent runtime tools are unrestricted by task or agent while token context restricts allowedTools")
	})

	t.Run("rejects blank agent runtime allowlist when token restricts tools", func(t *testing.T) {
		token := &ContextToken{
			Scopes: []string{ContextTokenScopeTaskCreate},
			TransactionContext: map[string]any{
				"allowedTools": []any{"Bash"},
			},
		}
		authzCtx := testTaskCreateAuthorizationContext()
		authzCtx.RuntimeAllowedTools = []string{" "}

		failures := contextTokenTaskCreateFailures(token, cfg, authzCtx)
		require.Contains(t, strings.Join(failures, "\n"), "agent runtime tools are unrestricted by task or agent while token context restricts allowedTools")
	})

	t.Run("rejects enabled bash when token restricts tools", func(t *testing.T) {
		token := &ContextToken{
			Scopes: []string{ContextTokenScopeTaskCreate},
			TransactionContext: map[string]any{
				"allowedTools": []any{"Read"},
			},
		}
		authzCtx := testTaskCreateAuthorizationContext()
		authzCtx.RuntimeAllowedTools = []string{"Read"}
		authzCtx.RuntimeAllowBash = true

		failures := contextTokenTaskCreateFailures(token, cfg, authzCtx)
		require.Contains(t, strings.Join(failures, "\n"), `tool "Bash" is not allowed by token context`)
	})
}

func TestAuthorizeContextTokenToolAgentCreateRejectsSpecOutsideTokenConstraints(t *testing.T) {
	cfg := enforceContextTokenAuthorizationConfig()
	token := &ContextToken{
		Scopes: []string{ContextTokenScopeAgentsWrite},
		TransactionContext: map[string]any{
			"namespace":        "team-a",
			"allowedProviders": []any{"openai"},
			"allowedModels":    []any{"openai/gpt-4o"},
			"allowedTools":     []any{"Read"},
		},
	}
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: "team-a"},
		Spec: corev1alpha1.AgentSpec{
			Model: &corev1alpha1.ModelConfig{
				Provider: "anthropic",
				Name:     "claude-3-5-sonnet",
			},
			Tools: []corev1alpha1.ToolReference{{Name: "web_search"}},
			Runtime: &corev1alpha1.AgentCLIRuntime{
				Type:                corev1alpha1.AgentRuntimeCodex,
				DefaultAllowedTools: []string{"Read"},
			},
		},
	}

	err := authorizeContextTokenToolAgentCreate(context.Background(), nil, token, cfg, "chatToolCreateAgent", agent)
	require.Error(t, err)
	msg := err.Error()
	require.Contains(t, msg, "context token is not authorized")

	failures, failureErr := contextTokenAgentSpecFailures(context.Background(), nil, token, agent)
	require.NoError(t, failureErr)
	joined := strings.Join(failures, "\n")
	require.Contains(t, joined, `agent provider "anthropic" is not allowed by token context`)
	require.Contains(t, joined, `agent model "claude-3-5-sonnet" is not allowed by token context`)
	require.Contains(t, joined, `agent tool "web_search" is not allowed by token context`)
	require.Contains(t, joined, `agent tool "Bash" is not allowed by token context`)
}

func TestContextTokenAgentSpecFailuresRejectsCrossNamespaceProviderRef(t *testing.T) {
	token := &ContextToken{
		TransactionContext: map[string]any{
			"namespace": "team-a",
		},
	}
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: "team-a"},
		Spec: corev1alpha1.AgentSpec{
			ProviderRef: &corev1alpha1.ProviderReference{Name: "llm", Namespace: "team-b"},
		},
	}

	failures, err := contextTokenAgentSpecFailures(context.Background(), nil, token, agent)
	require.NoError(t, err)
	require.Contains(t, strings.Join(failures, "\n"), `agent provider namespace "team-b" does not match token context "team-a"`)
}

func TestContextTokenTaskReadFailures(t *testing.T) {
	cfg := enforceContextTokenAuthorizationConfig()

	t.Run("allows matching task name context", func(t *testing.T) {
		token := &ContextToken{
			Scopes: []string{ContextTokenScopeTaskGet},
			TransactionContext: map[string]any{
				"namespace": "team-a",
				"taskName":  "task-1",
			},
		}

		failures := contextTokenTaskReadFailures(token, cfg, "team-a", "task-1")
		require.Empty(t, failures)
	})

	t.Run("allows matching namespaced task context", func(t *testing.T) {
		token := &ContextToken{
			Scopes: []string{ContextTokenScopeTaskGet},
			TransactionContext: map[string]any{
				"task": "team-a/task-1",
			},
		}

		failures := contextTokenTaskReadFailures(token, cfg, "team-a", "task-1")
		require.Empty(t, failures)
	})

	t.Run("allows matching bare task context", func(t *testing.T) {
		token := &ContextToken{
			Scopes: []string{ContextTokenScopeTaskGet},
			TransactionContext: map[string]any{
				"task": "task-1",
			},
		}

		failures := contextTokenTaskReadFailures(token, cfg, "team-a", "task-1")
		require.Empty(t, failures)
	})

	t.Run("reports scope namespace and task mismatches", func(t *testing.T) {
		token := &ContextToken{
			Scopes: []string{ContextTokenScopeTaskList},
			TransactionContext: map[string]any{
				"namespace": "team-b",
				"taskName":  "task-2",
				"task":      "team-b/task-2",
			},
		}

		failures := contextTokenTaskReadFailures(token, cfg, "team-a", "task-1")
		joined := strings.Join(failures, "\n")
		require.Contains(t, joined, `missing one of required scopes "orka:tasks:get"`)
		require.Contains(t, joined, `namespace "team-a" does not match token context "team-b"`)
		require.Contains(t, joined, `task name "task-1" does not match token context "task-2"`)
		require.Contains(t, joined, `task "team-a/task-1" does not match token context "team-b/task-2"`)
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

func TestAuthorizeAndStampToolTaskCreateStampsContextTokenProvenance(t *testing.T) {
	cfg := enforceContextTokenAuthorizationConfig()
	token := &ContextToken{
		Profile:            ContextTokenProfileTransactionToken,
		Issuer:             "https://issuer.example.test",
		Subject:            testContextTokenSubject,
		Audience:           []string{"orka"},
		TransactionID:      testContextTokenTransactionID,
		Scope:              ContextTokenScopeTaskCreate,
		Scopes:             []string{ContextTokenScopeTaskCreate},
		RequestingWorkload: "spiffe://example.test/ns/default/sa/client",
		TransactionContext: map[string]any{
			"trace_id": testContextTokenTraceID,
		},
		RequesterContext: map[string]any{
			"user": "alice",
		},
	}
	ui := &UserInfo{
		AuthType:     AuthTypeContextToken,
		Subject:      token.Subject,
		Issuer:       token.Issuer,
		Roles:        token.Scopes,
		ContextToken: token,
	}
	task := &corev1alpha1.Task{
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeContainer,
		},
	}

	err := authorizeAndStampToolTaskCreate(context.Background(), nil, nil, token, cfg, "chatToolCreateTask", ui, task)
	require.NoError(t, err)
	require.NotNil(t, task.Spec.RequestedBy)
	require.Equal(t, testContextTokenSubject, task.Spec.RequestedBy.Subject)
	require.NotNil(t, task.Spec.Transaction)
	require.Equal(t, testContextTokenTransactionID, task.Spec.Transaction.ID)
	require.Equal(t, ContextTokenScopeTaskCreate, task.Spec.Transaction.Scope)
	require.Equal(t, labels.SelectorValue(testContextTokenTransactionID), task.Labels[labels.LabelTransactionID])
	require.Equal(t, testContextTokenTransactionID, task.Annotations[labels.AnnotationTransactionID])
}

func enforceContextTokenAuthorizationConfig() ContextTokenAuthorizationConfig {
	return ContextTokenAuthorizationConfig{
		Mode:                ContextTokenAuthorizationModeEnforce,
		TaskCreateScopes:    []string{ContextTokenScopeTaskCreate},
		TaskReadScopes:      []string{ContextTokenScopeTaskGet},
		TaskListScopes:      []string{ContextTokenScopeTaskList},
		ToolUseScopes:       []string{ContextTokenScopeToolsUse},
		ProviderUseScopes:   []string{ContextTokenScopeProvidersUse},
		SecretReadScopeList: []string{ContextTokenScopeSecretsRead},
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
		RuntimeAllowBash:    true,
	}
}

func TestContextTokenTaskCreateEffectiveAIToolsSkipsDisabledCoordinationInjection(t *testing.T) {
	agent := &corev1alpha1.Agent{
		Spec: corev1alpha1.AgentSpec{
			Coordination: &corev1alpha1.CoordinationConfig{Enabled: true},
		},
	}
	req := CreateTaskRequest{
		Type:        corev1alpha1.TaskTypeAI,
		Annotations: map[string]string{labels.AnnotationDisableCoordinationToolInject: "true"},
		AI: &corev1alpha1.AISpec{
			Tools: []string{"list_pull_requests", "check_pr_review_marker"},
		},
	}

	got := contextTokenTaskCreateEffectiveAITools(req, agent)
	require.Contains(t, got, "list_pull_requests")
	require.Contains(t, got, "check_pr_review_marker")
	require.Contains(t, got, "recall_memory")
	require.Contains(t, got, "remember")
	require.Contains(t, got, "propose_memory")
	require.Contains(t, got, "search_transcript")
	require.NotContains(t, got, "delegate_task")
	require.NotContains(t, got, "merge_pull_request")
	require.NotContains(t, got, "auto_merge_pull_request")
}

func TestContextTokenTaskCreateEffectiveAIToolsIncludesPRReviewCoordinationTools(t *testing.T) {
	agent := &corev1alpha1.Agent{
		Spec: corev1alpha1.AgentSpec{
			Coordination: &corev1alpha1.CoordinationConfig{Enabled: true},
		},
	}
	req := CreateTaskRequest{
		Type: corev1alpha1.TaskTypeAI,
	}

	got := contextTokenTaskCreateEffectiveAITools(req, agent)
	require.Contains(t, got, "list_pull_requests")
	require.Contains(t, got, "check_pr_review_marker")
}

func TestRedactedContextTokenAuthorizationFailuresRedactsRepositoryCredentials(t *testing.T) {
	got := redactedContextTokenAuthorizationFailures([]string{
		`workspace repo "https://user:embedded-secret@example.com/org/repo.git" does not match token context "https://github.com/org/repo"`,
		`token ghp_abcdefghijklmnopqrstuvwxyz1234567890 should not leak`,
	})
	if strings.Contains(got, "embedded-secret") || strings.Contains(got, "ghp_abcdefghijklmnopqrstuvwxyz1234567890") {
		t.Fatalf("redacted failures leaked secret material: %q", got)
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("redacted failures = %q, want redaction marker", got)
	}
}

func TestContextTokenTaskToolCredentialFailuresForOutboundAccessPolicy(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	policy := readyContextTokenOutboundPolicy("resource-api", corev1alpha1.OutboundAccessPolicySpec{Direct: &corev1alpha1.DirectOutboundAccess{
		Subject: corev1alpha1.OutboundTokenSource{
			Source:    corev1alpha1.OutboundTokenSourceSecretRef,
			TokenType: "urn:example:assertion",
			SecretRef: &corev1alpha1.NamespacedSecretKeySelector{Name: "resource-assertion", Key: "token"},
		},
		TokenEndpoint:           corev1alpha1.OutboundTokenEndpoint{URL: "https://identity.example.test/token"},
		ExpectedIssuedTokenType: "urn:example:resource",
	}})
	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "search", Namespace: "team-a"},
		Spec: corev1alpha1.ToolSpec{HTTP: &corev1alpha1.HTTPExecution{
			OutboundAccessPolicyRef: &corev1alpha1.LocalObjectReference{Name: "resource-api"},
		}},
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(policy, tool).Build()
	cfg := enforceContextTokenAuthorizationConfig()
	cfg.SecretCredentialReadScopeList = []string{ContextTokenScopeSecretsCredentialsRead}
	authzCtx := contextTokenTaskCreateAuthorizationContext{Namespace: "team-a", EffectiveAITools: []string{"search"}}

	token := &ContextToken{Scopes: []string{ContextTokenScopeTaskCreate}}
	failures, err := contextTokenTaskToolCredentialFailures(context.Background(), client, token, cfg, authzCtx)
	require.NoError(t, err)
	require.Len(t, failures, 1)
	require.Contains(t, failures[0], ContextTokenScopeSecretsCredentialsRead)

	token.Scopes = append(token.Scopes, ContextTokenScopeSecretsCredentialsRead)
	failures, err = contextTokenTaskToolCredentialFailures(context.Background(), client, token, cfg, authzCtx)
	require.NoError(t, err)
	require.Empty(t, failures)

	token.TransactionContext = map[string]any{"secret": "different-secret"}
	failures, err = contextTokenTaskToolCredentialFailures(context.Background(), client, token, cfg, authzCtx)
	require.NoError(t, err)
	require.Len(t, failures, 1)
	require.Contains(t, failures[0], "resource-assertion")
}

func TestContextTokenTaskToolCredentialFailuresRejectsUnresolvedOutboundAccessPolicy(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "search", Namespace: "team-a"},
		Spec: corev1alpha1.ToolSpec{HTTP: &corev1alpha1.HTTPExecution{
			OutboundAccessPolicyRef: &corev1alpha1.LocalObjectReference{Name: "resource-api"},
		}},
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tool).Build()
	cfg := enforceContextTokenAuthorizationConfig()
	authzCtx := contextTokenTaskCreateAuthorizationContext{Namespace: "team-a", EffectiveAITools: []string{"search"}}
	token := &ContextToken{Scopes: []string{ContextTokenScopeTaskCreate}}

	failures, err := contextTokenTaskToolCredentialFailures(context.Background(), client, token, cfg, authzCtx)
	require.NoError(t, err)
	require.Len(t, failures, 1)
	require.Contains(t, failures[0], "search")
	require.Contains(t, failures[0], "resource-api")
}

func TestContextTokenTaskToolCredentialFailuresForServiceAccountSources(t *testing.T) {
	serviceAccountSource := func() corev1alpha1.OutboundTokenSource {
		return corev1alpha1.OutboundTokenSource{
			Source: corev1alpha1.OutboundTokenSourceServiceAccount,
			ServiceAccountRef: &corev1alpha1.OutboundServiceAccountReference{
				Name: "workload",
			},
		}
	}
	transactionTokenSource := corev1alpha1.OutboundTokenSource{Source: corev1alpha1.OutboundTokenSourceTransactionToken}

	tests := []struct {
		name                    string
		direct                  corev1alpha1.DirectOutboundAccess
		requiresCredentialScope bool
	}{
		{
			name: "subject ServiceAccount",
			direct: corev1alpha1.DirectOutboundAccess{
				Subject: serviceAccountSource(),
			},
			requiresCredentialScope: true,
		},
		{
			name: "actor ServiceAccount",
			direct: corev1alpha1.DirectOutboundAccess{
				Subject: transactionTokenSource,
				Actor: func() *corev1alpha1.OutboundTokenSource {
					source := serviceAccountSource()
					return &source
				}(),
			},
			requiresCredentialScope: true,
		},
		{
			name: "transaction token only",
			direct: corev1alpha1.DirectOutboundAccess{
				Subject: transactionTokenSource,
			},
			requiresCredentialScope: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, corev1alpha1.AddToScheme(scheme))
			policy := readyContextTokenOutboundPolicy(
				"resource-api",
				corev1alpha1.OutboundAccessPolicySpec{Direct: &tt.direct},
			)
			tool := &corev1alpha1.Tool{
				ObjectMeta: metav1.ObjectMeta{Name: "search", Namespace: "team-a"},
				Spec: corev1alpha1.ToolSpec{HTTP: &corev1alpha1.HTTPExecution{
					OutboundAccessPolicyRef: &corev1alpha1.LocalObjectReference{Name: "resource-api"},
				}},
			}
			client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(policy, tool).Build()
			cfg := enforceContextTokenAuthorizationConfig()
			cfg.SecretCredentialReadScopeList = []string{ContextTokenScopeSecretsCredentialsRead}
			authzCtx := contextTokenTaskCreateAuthorizationContext{Namespace: "team-a", EffectiveAITools: []string{"search"}}
			token := &ContextToken{Scopes: []string{ContextTokenScopeTaskCreate}}

			failures, err := contextTokenTaskToolCredentialFailures(context.Background(), client, token, cfg, authzCtx)
			require.NoError(t, err)
			if !tt.requiresCredentialScope {
				require.Empty(t, failures)
				return
			}
			require.Len(t, failures, 1)
			require.Contains(t, failures[0], ContextTokenScopeSecretsCredentialsRead)

			token.Scopes = append(token.Scopes, ContextTokenScopeSecretsCredentialsRead)
			failures, err = contextTokenTaskToolCredentialFailures(context.Background(), client, token, cfg, authzCtx)
			require.NoError(t, err)
			require.Empty(t, failures)
		})
	}
}

func TestContextTokenTaskToolCredentialFailuresRejectsStaleOrRejectedOutboundAccessPolicy(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*corev1alpha1.OutboundAccessPolicy)
	}{
		{
			name: "stale observed generation",
			mutate: func(policy *corev1alpha1.OutboundAccessPolicy) {
				policy.Status.ObservedGeneration--
			},
		},
		{
			name: "rejected",
			mutate: func(policy *corev1alpha1.OutboundAccessPolicy) {
				policy.Status.Conditions[0].Status = metav1.ConditionFalse
			},
		},
		{
			name: "unresolved references",
			mutate: func(policy *corev1alpha1.OutboundAccessPolicy) {
				policy.Status.Conditions[1].Status = metav1.ConditionFalse
			},
		},
		{
			name: "stale condition",
			mutate: func(policy *corev1alpha1.OutboundAccessPolicy) {
				policy.Status.Conditions[0].ObservedGeneration--
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, corev1alpha1.AddToScheme(scheme))
			policy := readyContextTokenOutboundPolicy("resource-api", corev1alpha1.OutboundAccessPolicySpec{
				Direct: &corev1alpha1.DirectOutboundAccess{
					Subject: corev1alpha1.OutboundTokenSource{Source: corev1alpha1.OutboundTokenSourceTransactionToken},
				},
			})
			tt.mutate(policy)
			tool := &corev1alpha1.Tool{
				ObjectMeta: metav1.ObjectMeta{Name: "search", Namespace: "team-a"},
				Spec: corev1alpha1.ToolSpec{HTTP: &corev1alpha1.HTTPExecution{
					OutboundAccessPolicyRef: &corev1alpha1.LocalObjectReference{Name: policy.Name},
				}},
			}
			client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(policy, tool).Build()
			authzCtx := contextTokenTaskCreateAuthorizationContext{Namespace: "team-a", EffectiveAITools: []string{"search"}}
			token := &ContextToken{Scopes: []string{ContextTokenScopeTaskCreate, ContextTokenScopeSecretsCredentialsRead}}

			failures, err := contextTokenTaskToolCredentialFailures(
				context.Background(),
				client,
				token,
				enforceContextTokenAuthorizationConfig(),
				authzCtx,
			)
			require.NoError(t, err)
			require.Len(t, failures, 1)
			require.Contains(t, failures[0], "unresolved OutboundAccessPolicy")
		})
	}
}

func readyContextTokenOutboundPolicy(
	name string,
	spec corev1alpha1.OutboundAccessPolicySpec,
) *corev1alpha1.OutboundAccessPolicy {
	generation := int64(2)
	return &corev1alpha1.OutboundAccessPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team-a", Generation: generation},
		Spec:       spec,
		Status: corev1alpha1.OutboundAccessPolicyStatus{
			ObservedGeneration: generation,
			Conditions: []metav1.Condition{
				{
					Type: corev1alpha1.OutboundAccessPolicyConditionAccepted, Status: metav1.ConditionTrue,
					ObservedGeneration: generation,
				},
				{
					Type: corev1alpha1.OutboundAccessPolicyConditionResolvedRefs, Status: metav1.ConditionTrue,
					ObservedGeneration: generation,
				},
			},
		},
	}
}

func TestContextTokenTaskToolCredentialFailuresRejectsUnresolvedCustomTool(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	authzCtx := contextTokenTaskCreateAuthorizationContext{Namespace: "team-a", EffectiveAITools: []string{"custom-search"}}
	failures, err := contextTokenTaskToolCredentialFailures(
		context.Background(), client, &ContextToken{}, enforceContextTokenAuthorizationConfig(), authzCtx,
	)
	require.NoError(t, err)
	require.Equal(t, []string{`Tool "custom-search" is unresolved`}, failures)
}

func TestContextTokenTaskToolCredentialFailuresAllowsUnresolvedBuiltinAndRuntimeTools(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	authzCtx := contextTokenTaskCreateAuthorizationContext{
		Namespace: "team-a", EffectiveAITools: []string{"web_search"}, RuntimeAllowedTools: []string{"Bash"},
	}
	failures, err := contextTokenTaskToolCredentialFailures(
		context.Background(), client, &ContextToken{}, enforceContextTokenAuthorizationConfig(), authzCtx,
	)
	require.NoError(t, err)
	require.Empty(t, failures)
}

func TestContextTokenTaskToolCredentialFailuresRejectsUnresolvedBrokeredRuntimeTool(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	authzCtx := contextTokenTaskCreateAuthorizationContext{Namespace: "team-a", RuntimeAllowedTools: []string{"read_incident"}}
	failures, err := contextTokenTaskToolCredentialFailures(context.Background(), client, &ContextToken{}, enforceContextTokenAuthorizationConfig(), authzCtx)
	require.NoError(t, err)
	require.Equal(t, []string{`Tool "read_incident" is unresolved`}, failures)
}

func TestContextTokenTaskToolCredentialFailuresUsesToolProvenance(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	builtinAgent := &corev1alpha1.Agent{Spec: corev1alpha1.AgentSpec{Runtime: &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeClaude}}}
	remoteAgent := &corev1alpha1.Agent{Spec: corev1alpha1.AgentSpec{Runtime: &corev1alpha1.AgentCLIRuntime{RuntimeRef: &corev1alpha1.AgentRuntimeReference{Name: "remote"}}}}
	tests := []struct {
		name        string
		ctx         contextTokenTaskCreateAuthorizationContext
		wantFailure string
	}{
		{name: "AI name matching native tool still resolves", ctx: contextTokenTaskCreateAuthorizationContext{Namespace: "team-a", EffectiveAITools: []string{"Read"}}, wantFailure: "Read"},
		{name: "AI coordination tool is builtin when coordination is enabled", ctx: contextTokenTaskCreateAuthorizationContext{Namespace: "team-a", Agent: &corev1alpha1.Agent{Spec: corev1alpha1.AgentSpec{Coordination: &corev1alpha1.CoordinationConfig{Enabled: true}}}, EffectiveAITools: []string{"list_issues"}}},
		{name: "AI coordination name resolves as custom without coordination", ctx: contextTokenTaskCreateAuthorizationContext{Namespace: "team-a", EffectiveAITools: []string{"list_issues"}}, wantFailure: "list_issues"},
		{name: "AI explicit coordination tool remains builtin when injection disabled", ctx: contextTokenTaskCreateAuthorizationContext{Namespace: "team-a", Agent: &corev1alpha1.Agent{Spec: corev1alpha1.AgentSpec{Coordination: &corev1alpha1.CoordinationConfig{Enabled: true}}}, Request: CreateTaskRequest{Annotations: map[string]string{labels.AnnotationDisableCoordinationToolInject: queryTrue}}, EffectiveAITools: []string{"list_pull_requests"}}},
		{name: "controller proxy coordination name resolves as custom when coordination disabled", ctx: contextTokenTaskCreateAuthorizationContext{Namespace: "team-a", EffectiveAITools: []string{"create_pull_request"}}, wantFailure: "create_pull_request"},
		{name: "dual-registered coordination name remains builtin when coordination disabled", ctx: contextTokenTaskCreateAuthorizationContext{Namespace: "team-a", EffectiveAITools: []string{"cancel_task"}}},
		{name: "built-in runtime accepts scoped native syntax", ctx: contextTokenTaskCreateAuthorizationContext{Namespace: "team-a", Agent: builtinAgent, RuntimeAllowedTools: []string{"Read(/workspace/**)"}}},
		{name: "runtimeRef observed defaults remain backend-owned", ctx: contextTokenTaskCreateAuthorizationContext{Namespace: "team-a", Agent: remoteAgent, RuntimeAllowedTools: []string{"analyze"}}},
		{name: "runtimeRef brokered coordination tool is builtin", ctx: contextTokenTaskCreateAuthorizationContext{Namespace: "team-a", Agent: remoteAgent, Request: CreateTaskRequest{Type: corev1alpha1.TaskTypeAgent, AgentRuntime: &corev1alpha1.AgentRuntimeSpec{AllowedTools: []string{"delegate_task"}}}, RuntimeAllowedTools: []string{"delegate_task"}, RuntimeAllowBash: true}},
		{name: "runtimeRef brokered override must resolve", ctx: contextTokenTaskCreateAuthorizationContext{Namespace: "team-a", Agent: remoteAgent, Request: CreateTaskRequest{Type: corev1alpha1.TaskTypeAgent, AgentRuntime: &corev1alpha1.AgentRuntimeSpec{AllowedTools: []string{"read_incident"}}}, RuntimeAllowedTools: []string{"read_incident"}, RuntimeAllowBash: true}, wantFailure: "read_incident"},
		{name: "runtimeRef brokered registry collision must resolve", ctx: contextTokenTaskCreateAuthorizationContext{Namespace: "team-a", Agent: remoteAgent, Request: CreateTaskRequest{Type: corev1alpha1.TaskTypeAgent, AgentRuntime: &corev1alpha1.AgentRuntimeSpec{AllowedTools: []string{"web_search"}}}, RuntimeAllowedTools: []string{"web_search"}}, wantFailure: "web_search"},
		{name: "runtimeRef explicit Bash must resolve", ctx: contextTokenTaskCreateAuthorizationContext{Namespace: "team-a", Agent: remoteAgent, Request: CreateTaskRequest{Type: corev1alpha1.TaskTypeAgent, AgentRuntime: &corev1alpha1.AgentRuntimeSpec{AllowedTools: []string{"Bash"}}}, RuntimeAllowedTools: []string{"Bash"}, RuntimeAllowBash: true}, wantFailure: "Bash"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			failures, err := contextTokenTaskToolCredentialFailures(context.Background(), client, &ContextToken{}, enforceContextTokenAuthorizationConfig(), tt.ctx)
			require.NoError(t, err)
			if tt.wantFailure == "" {
				require.Empty(t, failures)
			} else {
				require.Len(t, failures, 1)
				require.Contains(t, failures[0], tt.wantFailure)
			}
		})
	}
}

func TestContextTokenTaskToolCredentialFailuresAllowsLowercaseBrokeredBashWithSyntheticNativeBash(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	bashTool := &corev1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{Name: "bash", Namespace: "team-a"}}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(bashTool).Build()
	remoteAgent := &corev1alpha1.Agent{Spec: corev1alpha1.AgentSpec{Runtime: &corev1alpha1.AgentCLIRuntime{RuntimeRef: &corev1alpha1.AgentRuntimeReference{Name: "remote"}}}}
	authzCtx := contextTokenTaskCreateAuthorizationContext{
		Namespace: "team-a", Agent: remoteAgent,
		Request:             CreateTaskRequest{Type: corev1alpha1.TaskTypeAgent, AgentRuntime: &corev1alpha1.AgentRuntimeSpec{AllowedTools: []string{"bash"}}},
		RuntimeAllowedTools: []string{"bash"}, RuntimeAllowBash: true,
	}
	failures, err := contextTokenTaskToolCredentialFailures(context.Background(), client, &ContextToken{}, enforceContextTokenAuthorizationConfig(), authzCtx)
	require.NoError(t, err)
	require.Empty(t, failures)
}
