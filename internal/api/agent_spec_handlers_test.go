// Copyright (c) 2026. MIT License - see LICENSE file for details.

package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const conditionalAgentBody = `{"expectedUID":"original","expectedGeneration":3,"spec":{"model":{"provider":"openai","name":"desired"}}}`

func conditionalAgentFixture() *corev1alpha1.Agent {
	return &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "test-agent", Namespace: "default", UID: "original", Generation: 3, Labels: map[string]string{"keep": "label"}},
		Spec:       corev1alpha1.AgentSpec{Model: &corev1alpha1.ModelConfig{Provider: "openai", Name: "original"}, Tools: []corev1alpha1.ToolReference{{Name: "file_read"}}},
	}
}

func agentSpecRequest(t *testing.T, app *fiber.App, path, body string) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close() //nolint:errcheck
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, data
}

func TestAgentSpecConditionalUpdate(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"success", conditionalAgentBody, 200},
		{"empty complete spec", `{"expectedUID":"original","expectedGeneration":3,"spec":{}}`, 200},
		{"wrong UID", `{"expectedUID":"replacement","expectedGeneration":3,"spec":{}}`, 409},
		{"wrong generation", `{"expectedUID":"original","expectedGeneration":2,"spec":{}}`, 409},
		{"missing UID", `{"expectedGeneration":3,"spec":{}}`, 400},
		{"blank UID", `{"expectedUID":"  ","expectedGeneration":3,"spec":{}}`, 400},
		{"UID is not trimmed", `{"expectedUID":" original ","expectedGeneration":3,"spec":{}}`, 409},
		{"missing generation", `{"expectedUID":"original","spec":{}}`, 400},
		{"null generation", `{"expectedUID":"original","expectedGeneration":null,"spec":{}}`, 400},
		{"zero generation", `{"expectedUID":"original","expectedGeneration":0,"spec":{}}`, 400},
		{"negative generation", `{"expectedUID":"original","expectedGeneration":-1,"spec":{}}`, 400},
		{"fractional generation", `{"expectedUID":"original","expectedGeneration":1.5,"spec":{}}`, 400},
		{"overflow generation", `{"expectedUID":"original","expectedGeneration":9223372036854775808,"spec":{}}`, 400},
		{"missing spec", `{"expectedUID":"original","expectedGeneration":3}`, 400},
		{"null spec", `{"expectedUID":"original","expectedGeneration":3,"spec":null}`, 400},
		{"invalid spec", `{"expectedUID":"original","expectedGeneration":3,"spec":{"ttlAfterLastTask":"-1s"}}`, 400},
		{"malformed", `{`, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, app := setupTestHandlersWithObjects(conditionalAgentFixture())
			app.Put("/agents/:name/spec", h.UpdateAgentSpec)
			status, data := agentSpecRequest(t, app, "/agents/test-agent/spec", tc.body)
			require.Equal(t, tc.status, status, string(data))
			stored := &corev1alpha1.Agent{}
			require.NoError(t, h.client.Get(t.Context(), types.NamespacedName{Namespace: "default", Name: "test-agent"}, stored))
			if tc.status != http.StatusOK {
				require.Equal(t, "original", stored.Spec.Model.Name)
				require.Len(t, stored.Spec.Tools, 1)
				if tc.status == http.StatusConflict {
					require.Equal(t, "agent UID or generation does not match", string(data))
				}
				return
			}
			var result corev1alpha1.Agent
			require.NoError(t, json.Unmarshal(data, &result))
			require.Equal(t, types.UID("original"), result.UID)
			if tc.name == "empty complete spec" {
				require.Nil(t, stored.Spec.Model)
			} else {
				require.Equal(t, "desired", stored.Spec.Model.Name)
			}
			require.Empty(t, stored.Spec.Tools, "the route replaces the whole spec")
			require.Equal(t, "label", stored.Labels["keep"])
		})
	}
}

func TestAgentSpecNotFoundAndNamespaceBeforeBody(t *testing.T) {
	h, app := setupTestHandlers()
	app.Put("/agents/:name/spec", h.UpdateAgentSpec)
	status, _ := agentSpecRequest(t, app, "/agents/missing/spec", conditionalAgentBody)
	require.Equal(t, http.StatusNotFound, status)
	h.watchNamespace = "default"
	h.enforceNamespaceIsolation = true
	status, _ = agentSpecRequest(t, app, "/agents/test-agent/spec?namespace=other", `{`)
	require.Equal(t, http.StatusForbidden, status)
}

func TestAgentSpecLegacyRequestStillUnconditional(t *testing.T) {
	h, app := setupTestHandlersWithObjects(conditionalAgentFixture())
	app.Put("/agents/:name", h.UpdateAgent)
	status, data := agentSpecRequest(t, app, "/agents/test-agent", `{"expectedUID":"wrong","expectedGeneration":1,"spec":{"model":{"provider":"openai","name":"legacy"}}}`)
	require.Equal(t, http.StatusOK, status, string(data))
	var result corev1alpha1.Agent
	require.NoError(t, json.Unmarshal(data, &result))
	require.Equal(t, "legacy", result.Spec.Model.Name)
}

func TestAgentSpecContextTokenCannotChangeToDisallowedModel(t *testing.T) {
	provider := newTestOIDCProvider(t)
	cfg := testContextTokenConfig(t, provider, "")
	app := setupTestHandlersWithAuthz(t, cfg, ContextTokenAuthorizationModeEnforce, conditionalAgentFixture())
	token := issueTestContextToken(t, provider, nil, map[string]any{
		"scope": ContextTokenScopeAgentsWrite,
		"tctx":  map[string]any{"allowedAgents": []string{"test-agent"}, "allowedProviders": []string{"openai"}, "allowedModels": []string{"openai/gpt-4o"}},
	})
	req := httptest.NewRequest(http.MethodPut, "/agents/test-agent/spec", strings.NewReader(`{"expectedUID":"original","expectedGeneration":3,"spec":{"model":{"provider":"anthropic","name":"claude-3-5-sonnet"}}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(TransactionTokenHeaderName, token)
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close() //nolint:errcheck
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
}
