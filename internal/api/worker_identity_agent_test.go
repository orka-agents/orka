package api

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

// TestCreateTaskKeepsWorkersFromRootTasksForIdentityAgents covers a worker
// that posts a Task to the API instead of creating it directly: the API
// writes Tasks as the controller, which admission trusts, so it applies the
// same rule itself and refuses a parentless Task for an identity-bound Agent.
func TestCreateTaskKeepsWorkersFromRootTasksForIdentityAgents(t *testing.T) {
	identityAgent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "reviewer", Namespace: testDefaultNamespace},
		Spec: corev1alpha1.AgentSpec{Identity: &corev1alpha1.AgentIdentity{
			Entra: &corev1alpha1.EntraAgentIdentity{AgentIdentityID: "33333333-3333-3333-3333-33333333aaaa"},
		}},
	}
	plainAgent := &corev1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "helper", Namespace: testDefaultNamespace}}
	removedAgent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "between-edits", Namespace: testDefaultNamespace},
		Status: corev1alpha1.AgentStatus{IdentityClaim: &corev1alpha1.AgentIdentityClaim{
			Issuer: corev1alpha1.AgentIdentityIssuerEntra, ID: "33333333-3333-3333-3333-33333333aaaa",
		}},
	}
	post := func(username, agent string) (int, string) {
		handlers, _ := setupTestHandlersWithObjects(identityAgent, plainAgent, removedAgent)
		app := fiber.New()
		app.Use(func(c fiber.Ctx) error {
			c.Locals(UserInfoContextKey, &UserInfo{Username: username, AuthType: AuthTypeTokenReview})
			return c.Next()
		})
		app.Post("/tasks", handlers.CreateTask)
		resp := testJSONRequest(t, app, http.MethodPost, "/tasks", map[string]any{
			"name": "from-" + agent, "type": "agent", "agentRef": map[string]any{"name": agent}, "prompt": "hi",
		})
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	worker := "system:serviceaccount:" + testDefaultNamespace + ":orka-ai-worker"

	status, body := post(worker, "reviewer")
	require.Equal(t, http.StatusForbidden, status, body)
	require.Contains(t, body, "identity-bound Agent")

	status, body = post(worker, "helper")
	require.False(t, strings.Contains(body, "identity-bound Agent"), "a worker may name another Agent: %d %s", status, body)

	status, body = post(worker, "between-edits")
	require.Equal(t, http.StatusForbidden, status, body)
	require.Contains(t, body, "identity-bound Agent", "an Agent still holding an identity claim is identity-bound")

	status, body = post("system:serviceaccount:"+testDefaultNamespace+":editor", "reviewer")
	require.False(t, strings.Contains(body, "identity-bound Agent"), "a non-worker is judged by the Agent use check: %d %s", status, body)
}
