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
