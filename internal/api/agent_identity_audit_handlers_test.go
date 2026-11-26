/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/outboundaccess"
)

func TestListAgentIdentityAuditFiltersAndScopesNamespace(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))

	taskA := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "task-a",
			Namespace: "team-a",
			UID:       types.UID("uid-task-a"),
		},
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(taskA).Build()

	buf := outboundaccess.NewAgentIdentityAuditBuffer(10)
	now := time.Date(2026, 11, 25, 4, 0, 0, 0, time.UTC)
	buf.Record(outboundaccess.AgentIdentityAuditRecord{
		Timestamp:       now,
		Event:           "agent_identity_use",
		Outcome:         "issued",
		Mode:            "Self",
		PolicyNamespace: "team-a",
		PolicyName:      "graph-policy",
		Tool:            "graph_me",
		ToolMethod:      "GET",
		ArgumentsDigest: "sha256:abc",
		TaskUID:         "uid-task-a",
		AgentNamespace:  "team-a",
		AgentName:       "entra-agent",
		IdentityIssuer:  "entra",
		IdentityID:      "33333333-3333-3333-3333-33333333aaaa",
	})
	buf.Record(outboundaccess.AgentIdentityAuditRecord{
		Timestamp:       now.Add(time.Minute),
		Event:           "agent_identity_use",
		Outcome:         "refused",
		Reason:          "delegation",
		Mode:            "Delegated",
		PolicyNamespace: "team-a",
		PolicyName:      "graph-policy",
		Tool:            "graph_mail",
		ToolMethod:      "GET",
		ArgumentsDigest: "sha256:def",
		TaskUID:         "uid-task-a",
		AgentNamespace:  "team-a",
		AgentName:       "entra-agent",
		IdentityIssuer:  "entra",
		IdentityID:      "33333333-3333-3333-3333-33333333aaaa",
		Requester:       "hmac-sha256:deadbeef",
	})
	buf.Record(outboundaccess.AgentIdentityAuditRecord{
		Timestamp:       now.Add(2 * time.Minute),
		Event:           "agent_identity_use",
		Outcome:         "issued",
		Mode:            "Self",
		PolicyNamespace: "team-b",
		PolicyName:      "other-policy",
		Tool:            "secret_tool",
		TaskUID:         "uid-task-b",
		AgentNamespace:  "team-b",
		AgentName:       "other-agent",
		IdentityIssuer:  "entra",
		IdentityID:      "44444444-4444-4444-4444-44444444bbbb",
	})

	handlers := NewHandlers(HandlersConfig{
		Client:                    kube,
		EnforceNamespaceIsolation: true,
		AgentIdentityAudit:        buf,
	})
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, &UserInfo{
			Username:  "alice",
			Namespace: "team-a",
			AuthType:  AuthTypeOIDC,
			Issuer:    "https://issuer.example",
			Subject:   "alice-sub",
		})
		return c.Next()
	})
	app.Get("/api/v1/audit/identity", handlers.ListAgentIdentityAudit)

	// Query by task name in caller's namespace returns both team-a records, newest first.
	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/v1/audit/identity?namespace=team-a&task=task-a", nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var body struct {
		Items []outboundaccess.AgentIdentityAuditRecord `json:"items"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	require.Len(t, body.Items, 2)
	require.Equal(t, "refused", body.Items[0].Outcome)
	require.Equal(t, "delegation", body.Items[0].Reason)
	require.Equal(t, "hmac-sha256:deadbeef", body.Items[0].Requester)
	require.Equal(t, "issued", body.Items[1].Outcome)

	// Filter by outcome=refused returns only the refused record.
	resp, err = app.Test(httptest.NewRequest(http.MethodGet, "/api/v1/audit/identity?namespace=team-a&outcome=refused", nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body.Items = nil
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	require.Len(t, body.Items, 1)
	require.Equal(t, "graph_mail", body.Items[0].Tool)

	// Cross-namespace request is rejected by namespace isolation.
	resp, err = app.Test(httptest.NewRequest(http.MethodGet, "/api/v1/audit/identity?namespace=team-b", nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)

	// Invalid outcome returns 400.
	resp, err = app.Test(httptest.NewRequest(http.MethodGet, "/api/v1/audit/identity?namespace=team-a&outcome=bogus", nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)

	// Records whose Task cannot be resolved in the namespace are omitted (fail closed).
	buf.Record(outboundaccess.AgentIdentityAuditRecord{
		Timestamp:       now.Add(3 * time.Minute),
		Event:           "agent_identity_use",
		Outcome:         "issued",
		Mode:            "Self",
		PolicyNamespace: "team-a",
		PolicyName:      "graph-policy",
		Tool:            "orphan_tool",
		TaskUID:         "uid-missing-task",
		AgentNamespace:  "team-a",
		AgentName:       "entra-agent",
	})
	resp, err = app.Test(httptest.NewRequest(http.MethodGet, "/api/v1/audit/identity?namespace=team-a&taskUID=uid-missing-task", nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body.Items = nil
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	require.Empty(t, body.Items)
}

func TestListAgentIdentityAuditEnforcesContextTokenTaskScope(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))

	taskAllowed := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "task-allowed", Namespace: "team-a", UID: types.UID("uid-allowed")},
	}
	taskOther := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "task-other", Namespace: "team-a", UID: types.UID("uid-other")},
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(taskAllowed, taskOther).Build()

	buf := outboundaccess.NewAgentIdentityAuditBuffer(10)
	now := time.Now().UTC()
	buf.Record(outboundaccess.AgentIdentityAuditRecord{
		Timestamp: now, Event: "agent_identity_use", Outcome: "issued", Mode: "Self",
		PolicyNamespace: "team-a", PolicyName: "p", Tool: "allowed_tool", TaskUID: "uid-allowed",
	})
	buf.Record(outboundaccess.AgentIdentityAuditRecord{
		Timestamp: now.Add(time.Second), Event: "agent_identity_use", Outcome: "issued", Mode: "Self",
		PolicyNamespace: "team-a", PolicyName: "p", Tool: "other_tool", TaskUID: "uid-other",
	})

	authzCfg := ContextTokenAuthorizationConfig{
		Mode:           ContextTokenAuthorizationModeEnforce,
		TaskListScopes: []string{ContextTokenScopeTaskList},
	}
	handlers := NewHandlers(HandlersConfig{
		Client:                    kube,
		ContextTokenAuthorization: authzCfg,
		AgentIdentityAudit:        buf,
	})
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, &UserInfo{
			Username:  "alice",
			Namespace: "team-a",
			AuthType:  AuthTypeContextToken,
			ContextToken: &ContextToken{
				Scopes: []string{ContextTokenScopeTaskList},
				TransactionContext: map[string]any{
					"namespace": "team-a",
					"task":      "task-allowed",
				},
			},
		})
		return c.Next()
	})
	app.Get("/api/v1/audit/identity", handlers.ListAgentIdentityAudit)

	// Unfiltered query only returns the record for task-allowed.
	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/v1/audit/identity?namespace=team-a", nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var body struct {
		Items []outboundaccess.AgentIdentityAuditRecord `json:"items"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	require.Len(t, body.Items, 1)
	require.Equal(t, "allowed_tool", body.Items[0].Tool)

	// Filtering directly by taskUID of another task returns empty.
	resp, err = app.Test(httptest.NewRequest(http.MethodGet, "/api/v1/audit/identity?namespace=team-a&taskUID=uid-other", nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body.Items = nil
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	require.Empty(t, body.Items)

	// Filtering by another task name returns 403.
	resp, err = app.Test(httptest.NewRequest(http.MethodGet, "/api/v1/audit/identity?namespace=team-a&task=task-other", nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestListAgentIdentityAuditReturns404ForOtherUsersPrivateAHPTask(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))

	privateTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ahp-turn-bob-1",
			Namespace: "team-a",
			UID:       types.UID("uid-bob-ahp"),
			Labels:    map[string]string{labels.LabelAHPTurn: labels.LabelAHPTurnValue},
		},
		Spec: corev1alpha1.TaskSpec{
			RequestedBy: &corev1alpha1.RequestedBy{
				Issuer:  "https://issuer.example",
				Subject: "bob-sub",
			},
		},
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(privateTask).Build()

	handlers := NewHandlers(HandlersConfig{
		Client:                    kube,
		EnforceNamespaceIsolation: true,
		AgentIdentityAudit:        outboundaccess.NewAgentIdentityAuditBuffer(10),
	})
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, &UserInfo{
			Username:  "alice",
			Namespace: "team-a",
			AuthType:  AuthTypeOIDC,
			Issuer:    "https://issuer.example",
			Subject:   "alice-sub",
		})
		return c.Next()
	})
	app.Get("/api/v1/audit/identity", handlers.ListAgentIdentityAudit)

	// Another person's private AHP Task must return 404 (matching missing tasks)
	// rather than 403 so existence is not leaked.
	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/v1/audit/identity?namespace=team-a&task=ahp-turn-bob-1", nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestAgentIdentityAuditBufferRedactionFromResolver(t *testing.T) {
	// Verify that records written to the AuditSink carry no raw tokens or raw error text.
	buf := outboundaccess.NewAgentIdentityAuditBuffer(10)
	buf.Record(outboundaccess.AgentIdentityAuditRecord{
		Timestamp:       time.Now().UTC(),
		Event:           "agent_identity_use",
		Outcome:         "refused",
		Reason:          "issuer",
		Mode:            "Delegated",
		PolicyNamespace: "default",
		PolicyName:      "p",
		Tool:            "t",
		ArgumentsDigest: "sha256:1234",
		Requester:       "hmac-sha256:5678",
	})
	raw, err := json.Marshal(buf.List(outboundaccess.AgentIdentityAuditFilter{Namespace: "default"}))
	require.NoError(t, err)
	require.False(t, strings.Contains(string(raw), "Bearer"))
	require.Contains(t, string(raw), `"reason":"issuer"`)
}
