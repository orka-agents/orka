/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/tools"
)

func TestCompatProxyAgentReferenceDeniedBeforeStateAccess(t *testing.T) {
	for _, endpoint := range []string{"openai", "anthropic", "responses"} {
		t.Run(endpoint, func(t *testing.T) {
			for _, denial := range []struct {
				name   string
				scopes []string
				tctx   map[string]any
			}{
				{"allowed-agents", []string{ContextTokenScopeTaskCreate}, map[string]any{"namespace": "default", "allowedAgents": []any{"other-agent"}}},
				{"agent", []string{ContextTokenScopeTaskCreate}, map[string]any{"namespace": "default", "agent": "other-agent"}},
				{"namespace", []string{ContextTokenScopeTaskCreate}, map[string]any{"namespace": "other-namespace"}},
				{"creation-scope", []string{ContextTokenScopeTaskGet}, map[string]any{"namespace": "default", "allowedAgents": []any{"agentkit"}}},
			} {
				t.Run(denial.name, func(t *testing.T) {
					var deniedResult string
					for _, state := range []string{"existing-agent", "missing-agent", "missing-runtime"} {
						t.Run(state, func(t *testing.T) {
							agent, externalRuntime := testExternalRuntimeAuthorizationObjects([]string{"Read"})
							externalRuntime.Spec.Capabilities.Profile.WorkspaceIntent = "read"
							var objects []client.Object
							switch state {
							case "existing-agent":
								objects = []client.Object{agent, externalRuntime}
							case "missing-runtime":
								objects = []client.Object{agent}
							}
							fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(objects...).Build()
							backend := &externalToolCountingClient{Client: fc}
							reader := &externalToolCountingClient{Client: fc}
							authz, err := NewContextTokenAuthorizationConfig(ContextTokenAuthorizationConfigOptions{Mode: ContextTokenAuthorizationModeEnforce})
							require.NoError(t, err)
							token := &ContextToken{Scopes: denial.scopes, TransactionContext: denial.tctx}
							tc := compatAgentReferenceTestContext(t, endpoint, compatProxyToolContextConfig{
								Client: backend, AuthorizationReader: reader, Namespace: agent.Namespace,
								GenerateTaskName: func() string { return compatToolContextGeneratedName },
								AuthContext:      token, AuthorizationConfig: authz,
								UserInfo: &UserInfo{AuthType: AuthTypeContextToken, ContextToken: token},
							})
							result := executeCompatAgentReferenceTestTool(t, tc)
							require.Zero(t, backend.reads, "denied reference reached the resource client")
							require.Zero(t, backend.writes)
							require.Zero(t, reader.reads, "denied reference reached the authorization reader")
							require.Zero(t, reader.writes)
							var decoded tools.ChatToolResult
							require.NoError(t, json.Unmarshal([]byte(result), &decoded))
							require.False(t, decoded.Success)
							require.Equal(t, compatToolAuthFailed, decoded.ErrorType)
							require.Equal(t, "context token is not authorized for chatToolAgentReference", decoded.Error)
							require.Equal(t, "Use an authorized Agent reference", decoded.Suggestion)
							if deniedResult == "" {
								deniedResult = result
							} else {
								require.Equal(t, deniedResult, result, "Agent/runtime existence changed the denial")
							}
						})
					}
				})
			}
		})
	}
}

func TestCompatProxyAgentReferencePreservesExternalProfile(t *testing.T) {
	for _, endpoint := range []string{"openai", "anthropic", "responses"} {
		t.Run(endpoint, func(t *testing.T) {
			agent, externalRuntime := testExternalRuntimeAuthorizationObjects([]string{"Read"})
			externalRuntime.Spec.Capabilities.Profile.WorkspaceIntent = "read"
			fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(agent, externalRuntime).Build()
			backend := &externalToolCountingClient{Client: fc}
			reader := &externalToolCountingClient{Client: fc}
			authz, err := NewContextTokenAuthorizationConfig(ContextTokenAuthorizationConfigOptions{Mode: ContextTokenAuthorizationModeEnforce})
			require.NoError(t, err)
			token := &ContextToken{Scopes: []string{ContextTokenScopeTaskCreate}, TransactionContext: map[string]any{
				"namespace": agent.Namespace, "allowedAgents": []any{agent.Name}, "allowedTools": []any{"Read"},
			}}
			tc := compatAgentReferenceTestContext(t, endpoint, compatProxyToolContextConfig{
				Client: backend, AuthorizationReader: reader, Namespace: agent.Namespace,
				GenerateTaskName: func() string { return compatToolContextGeneratedName },
				AuthContext:      token, AuthorizationConfig: authz,
				UserInfo: &UserInfo{AuthType: AuthTypeContextToken, ContextToken: token},
			})
			require.NotNil(t, tc.AuthorizeAgentReference)
			referenceChecks, fullChecks := 0, 0
			referenceAuthorize := tc.AuthorizeAgentReference
			tc.AuthorizeAgentReference = func(ctx context.Context, reference *corev1alpha1.Agent) *tools.ChatToolError {
				referenceChecks++
				require.Zero(t, backend.reads)
				require.Zero(t, reader.reads)
				require.Zero(t, backend.writes)
				return referenceAuthorize(ctx, reference)
			}
			fullAuthorize := tc.AuthorizeTaskCreate
			require.NotNil(t, fullAuthorize)
			tc.AuthorizeTaskCreate = func(ctx context.Context, task *corev1alpha1.Task) *tools.ChatToolError {
				fullChecks++
				require.Equal(t, 1, referenceChecks)
				require.NotNil(t, task.Spec.AgentRuntime, "full authorization ran before materialization")
				require.Equal(t, []string{"Read"}, task.Spec.AgentRuntime.AllowedTools)
				return fullAuthorize(ctx, task)
			}
			result := executeCompatAgentReferenceTestTool(t, tc)
			var decoded tools.ChatToolResult
			require.NoError(t, json.Unmarshal([]byte(result), &decoded))
			require.True(t, decoded.Success, "%s", decoded.Error)
			require.Equal(t, 1, referenceChecks)
			require.Equal(t, 1, fullChecks)
			require.Equal(t, 1, backend.writes)
			require.Positive(t, reader.reads)
			var task corev1alpha1.Task
			require.NoError(t, fc.Get(context.Background(), client.ObjectKey{Namespace: agent.Namespace, Name: compatToolContextGeneratedName}, &task))
			require.NotNil(t, task.Spec.AgentRef)
			require.Equal(t, agent.Name, task.Spec.AgentRef.Name)
			require.NotNil(t, task.Spec.AgentRuntime)
			require.Equal(t, []string{"Read"}, task.Spec.AgentRuntime.AllowedTools)
		})
	}
}

func compatAgentReferenceTestContext(t *testing.T, endpoint string, cfg compatProxyToolContextConfig) *tools.ToolContext {
	t.Helper()
	switch endpoint {
	case "openai":
		cfg.Profile = openAICompatProxyToolContextProfile
		return newCompatProxyToolContext(cfg)
	case "anthropic":
		cfg.Profile = anthropicCompatProxyToolContextProfile
		return newCompatProxyToolContext(cfg)
	case "responses":
		handler := &OpenAICompatHandler{
			client: cfg.Client, apiReader: cfg.AuthorizationReader,
			contextTokenAuthorization: cfg.AuthorizationConfig,
		}
		var tc *tools.ToolContext
		app := fiber.New()
		app.Get("/context", func(c fiber.Ctx) error {
			c.Locals(UserInfoContextKey, cfg.UserInfo)
			tc = handler.responsesToolContext(c, cfg.Namespace, cfg.Provider)
			return c.SendStatus(http.StatusNoContent)
		})
		resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/context", nil))
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		require.Equal(t, http.StatusNoContent, resp.StatusCode)
		require.NotNil(t, tc)
		tc.GenerateTaskName = cfg.GenerateTaskName
		return tc
	default:
		t.Fatalf("unknown compatibility endpoint %q", endpoint)
		return nil
	}
}

func executeCompatAgentReferenceTestTool(t *testing.T, tc *tools.ToolContext) string {
	t.Helper()
	registry := tools.NewRegistry()
	registry.Register(&tools.CreateAgentTaskTool{})
	result, err := registry.Execute(tools.WithToolContext(context.Background(), tc), "create_agent_task", json.RawMessage(`{"agentRef":"agentkit","prompt":"work"}`))
	require.NoError(t, err)
	return result
}
