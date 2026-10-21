/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func TestAPICreationRejectsRemovedRuntimeInputs(t *testing.T) {
	tests := []struct {
		name string
		path string
		body string
		want string
	}{
		{"flat workspace", "/tasks", `{"name":"rejected","type":"agent","agentRuntime":{"workspace":{}}}`, "use spec.workspace"},
		{"manifest workspace", "/tasks", `{"metadata":{"name":"rejected"},"spec":{"type":"agent","agentRuntime":{"workspace":null}}}`, "use spec.workspace"},
		{"task status", "/tasks", `{"name":"rejected","type":"agent","status":{"harnessRuntime":{}}}`, "status.harnessRuntime is no longer supported"},
		{"task binding", "/tasks", `{"name":"rejected","type":"agent","status":{"agentExecutionBinding":{"contractVersion":"orka.harness.v1"}}}`, "only orka.harness.v2"},
		{"Agent contract", "/agents", `{"name":"rejected","spec":{"runtime":{"type":"codex","contractVersion":"orka.harness.v1"}}}`, "only orka.harness.v2"},
		{"Agent credentials", "/agents", `{"name":"rejected","spec":{"runtime":{"type":"codex","secretRef":{"name":"old-auth"}}}}`, "spec.runtime.secretRef is no longer supported"},
		{"AgentRuntime contract", "/agent-runtimes", `{"metadata":{"name":"rejected"},"spec":{"contractVersion":"orka.harness.v1"}}`, "only orka.harness.v2"},
		{"AgentRuntime auth", "/agent-runtimes", `{"metadata":{"name":"rejected"},"spec":{"contractVersion":"orka.harness.v2","clientAuth":{"bearerTokenSecretRef":null}}}`, "bearerTokenSecretRef is no longer supported"},
		{"AgentRuntime capabilities", "/agent-runtimes", `{"metadata":{"name":"rejected"},"spec":{"contractVersion":"orka.harness.v2","capabilities":{"supportsCancel":true}}}`, "supportsCancel is no longer supported"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handlers, app := setupTestHandlers()
			app.Post("/tasks", handlers.CreateTask)
			app.Post("/agents", handlers.CreateAgent)
			app.Post("/agent-runtimes", handlers.CreateAgentRuntime)
			request := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(test.body))
			request.Header.Set("Content-Type", "application/json")
			response, err := app.Test(request)
			require.NoError(t, err)
			defer func() { require.NoError(t, response.Body.Close()) }()
			body, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			require.Equal(t, http.StatusBadRequest, response.StatusCode)
			require.Contains(t, string(body), test.want)
			var tasks corev1alpha1.TaskList
			require.NoError(t, handlers.client.List(t.Context(), &tasks))
			require.Empty(t, tasks.Items)
			var agents corev1alpha1.AgentList
			require.NoError(t, handlers.client.List(t.Context(), &agents))
			require.Empty(t, agents.Items)
			var runtimes corev1alpha1.AgentRuntimeList
			require.NoError(t, handlers.client.List(t.Context(), &runtimes))
			require.Empty(t, runtimes.Items)
		})
	}
}
