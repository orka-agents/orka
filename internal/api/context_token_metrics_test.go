/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/metrics"
)

func TestContextTokenAuthenticationRecordsMetrics(t *testing.T) {
	metrics.ContextTokenAuthTotal.Reset()

	provider := newTestOIDCProvider(t)
	ctxTokenConfig := testContextTokenConfig(t, provider, "")
	app := fiber.New()
	app.Use(NewAuthMiddleware(nil, AuthConfig{ContextTokens: ctxTokenConfig}))
	app.Get("/test", func(c fiber.Ctx) error { return c.SendString("OK") })

	validToken := issueTestContextToken(t, provider, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set(TransactionTokenHeaderName, validToken)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("valid context-token request failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid context-token status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	invalidToken := issueTestContextToken(t, provider, nil, map[string]any{"aud": "not-orka"})
	req = httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set(TransactionTokenHeaderName, invalidToken)
	resp, err = app.Test(req)
	if err != nil {
		t.Fatalf("invalid context-token request failed unexpectedly: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("invalid context-token status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}

	if count := testCounterValue(t, metrics.ContextTokenAuthTotal, ContextTokenProfileTransactionToken, "success"); count != 1 {
		t.Fatalf("context-token auth success metric = %v, want 1", count)
	}
	if count := testCounterValue(t, metrics.ContextTokenAuthTotal, ContextTokenProfileTransactionToken, "failure"); count != 1 {
		t.Fatalf("context-token auth failure metric = %v, want 1", count)
	}
}

func TestContextTokenAuthorizationRecordsMetrics(t *testing.T) {
	metrics.ContextTokenAuthorizationTotal.Reset()

	provider := newTestOIDCProvider(t)
	ctxTokenConfig := testContextTokenConfig(t, provider, "")
	app := setupTestHandlersWithAuthz(t, ctxTokenConfig, ContextTokenAuthorizationModeEnforce)

	allowedBody := CreateTaskRequest{
		Name:      "metrics-allowed-task",
		Namespace: "default",
		Type:      corev1alpha1.TaskTypeContainer,
		Image:     "busybox",
	}
	allowedBytes, _ := json.Marshal(allowedBody)
	allowedToken := issueTestContextToken(t, provider, nil, map[string]any{"scope": ContextTokenScopeTaskCreate})
	req := httptest.NewRequest(http.MethodPost, "/tasks", bytes.NewReader(allowedBytes))
	req.Header.Set(TransactionTokenHeaderName, allowedToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("allowed task request failed: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("allowed task status = %d, want %d", resp.StatusCode, http.StatusCreated)
	}

	deniedBody := CreateTaskRequest{
		Name:      "metrics-denied-task",
		Namespace: "default",
		Type:      corev1alpha1.TaskTypeContainer,
		Image:     "busybox",
	}
	deniedBytes, _ := json.Marshal(deniedBody)
	deniedToken := issueTestContextToken(t, provider, nil, map[string]any{"scope": ContextTokenScopeTaskGet})
	req = httptest.NewRequest(http.MethodPost, "/tasks", bytes.NewReader(deniedBytes))
	req.Header.Set(TransactionTokenHeaderName, deniedToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err = app.Test(req)
	if err != nil {
		t.Fatalf("denied task request failed unexpectedly: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("denied task status = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}

	if count := testCounterValue(t, metrics.ContextTokenAuthorizationTotal, "createTask", "allowed", "ok"); count != 1 {
		t.Fatalf("context-token authorization allowed metric = %v, want 1", count)
	}
	if count := testCounterValue(t, metrics.ContextTokenAuthorizationTotal, "createTask", "denied", "missing_scope"); count != 1 {
		t.Fatalf("context-token authorization denied metric = %v, want 1", count)
	}
}

func testCounterValue(t *testing.T, counter *prometheus.CounterVec, labels ...string) float64 {
	t.Helper()
	var m dto.Metric
	if err := counter.WithLabelValues(labels...).Write(&m); err != nil {
		t.Fatalf("failed to read counter metric: %v", err)
	}
	return m.GetCounter().GetValue()
}
