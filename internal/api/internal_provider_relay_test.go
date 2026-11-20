/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/workerenv"
)

type relayTestIdentities struct {
	mu        sync.Mutex
	err       error
	calls     int
	identity  string
	targetURL string
}

func (s *relayTestIdentities) Token(_ context.Context, identity, baseURL string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.identity, s.targetURL = identity, baseURL
	if s.err != nil {
		return "", s.err
	}
	return "entra-token", nil
}

type relayUpstreamRequest struct {
	method, path, query string
	header              http.Header
	body                string
}

type providerRelayHarness struct {
	app        *fiber.App
	handlers   *InternalHandlers
	identities *relayTestIdentities
	upstream   *httptest.Server
	mu         sync.Mutex
	requests   []relayUpstreamRequest
}

func (h *providerRelayHarness) upstreamRequests() []relayUpstreamRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]relayUpstreamRequest(nil), h.requests...)
}

type providerRelayFixture struct {
	task     *corev1alpha1.Task
	provider *corev1alpha1.Provider
	relayEnv string
	// noIdentities builds the handlers without a Provider identity source.
	noIdentities bool
}

func newProviderRelayHarness(t *testing.T, mutate func(*providerRelayFixture)) *providerRelayHarness {
	t.Helper()
	h := &providerRelayHarness{identities: &relayTestIdentities{}}
	h.upstream = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.requests = append(h.requests, relayUpstreamRequest{
			method: r.Method, path: r.URL.EscapedPath(), query: r.URL.RawQuery, header: r.Header.Clone(), body: string(body),
		})
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Set-Cookie", "upstream=1")
		w.Header().Set("X-Request-Id", "req-1")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"chatcmpl-1"}`)
	}))
	t.Cleanup(h.upstream.Close)

	task := internalCallerAuthTask()
	task.Spec = corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI}
	provider := &corev1alpha1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: "azure", Namespace: "default", UID: "provider-uid"},
		Spec: corev1alpha1.ProviderSpec{
			Type:    corev1alpha1.ProviderTypeAzureOpenAI,
			BaseURL: h.upstream.URL,
			Auth:    &corev1alpha1.ProviderAuth{Entra: &corev1alpha1.ProviderEntraAuth{Identity: "models"}},
		},
		Status: corev1alpha1.ProviderStatus{Ready: true},
	}
	relayEnv, err := workerenv.EncodeProviderRelay(map[string]workerenv.ProviderRelayTarget{
		workerenv.ProviderRelayPrimary: {Namespace: "default", Name: "azure", UID: "provider-uid"},
	})
	require.NoError(t, err)
	fixture := &providerRelayFixture{task: task, provider: provider, relayEnv: relayEnv}
	if mutate != nil {
		mutate(fixture)
	}

	job := internalCallerAuthJob(fixture.task, "job-a", "job-uid")
	job.Spec.Template.Spec.Containers = []corev1.Container{{
		Name: "worker",
		Env:  []corev1.EnvVar{{Name: workerenv.AIProviderRelay, Value: fixture.relayEnv}},
	}}
	pod := internalCallerAuthPod(fixture.task, "pod-a", "pod-uid", job)
	other := internalCallerAuthTaskObject("task-b", "task-b-uid", "job-b", "", "")
	c := fake.NewClientBuilder().WithScheme(internalCallerAuthScheme(t)).
		WithObjects(fixture.task, job, pod, fixture.provider, other).Build()
	config := InternalHandlersConfig{Client: c, APIReader: c, ProviderRelayTransport: h.upstream.Client().Transport}
	if !fixture.noIdentities {
		config.ProviderIdentities = h.identities
	}
	handlers := NewInternalHandlers(nil, nil, nil, nil, nil, config)
	app := fiber.New()
	app.Use(func(ctx fiber.Ctx) error {
		ctx.Locals(UserInfoContextKey, internalCallerAuthWorkerUser("pod-a", "pod-uid"))
		return ctx.Next()
	})
	app.All("/internal/v1/tasks/:namespace/:taskName/providers/:slot/*", handlers.RelayProviderRequest)
	h.app, h.handlers = app, handlers
	return h
}

const relayChatPath = "/internal/v1/tasks/default/task-a/providers/primary/openai/deployments/gpt-x/chat/completions?api-version=2025-03-01-preview"

func (h *providerRelayHarness) do(t *testing.T, method, target string, header http.Header) (*http.Response, string) {
	t.Helper()
	request := httptest.NewRequest(method, target, strings.NewReader(`{"model":"gpt-x","messages":[]}`))
	request.Header.Set("Content-Type", "application/json")
	for name, values := range header {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	response, err := h.app.Test(request, fiber.TestConfig{Timeout: 10 * time.Second})
	require.NoError(t, err)
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	return response, string(body)
}

func TestProviderRelayForwardsTheWorkersRequestWithTheIdentityToken(t *testing.T) {
	h := newProviderRelayHarness(t, nil)
	response, body := h.do(t, http.MethodPost, relayChatPath, http.Header{
		"Authorization": {"Bearer worker-sa-token"},
		"Api-Key":       {"leaked-key"},
		"Cookie":        {"session=1"},
		"X-Orka-Task":   {"spoofed"},
	})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.JSONEq(t, `{"id":"chatcmpl-1"}`, body)
	require.Empty(t, response.Header.Get("Set-Cookie"), "the Provider's cookies must not reach the worker")
	require.Equal(t, "req-1", response.Header.Get("X-Request-Id"))

	requests := h.upstreamRequests()
	require.Len(t, requests, 1)
	got := requests[0]
	require.Equal(t, http.MethodPost, got.method)
	require.Equal(t, "/openai/deployments/gpt-x/chat/completions", got.path)
	require.Equal(t, "api-version=2025-03-01-preview", got.query)
	require.Equal(t, `{"model":"gpt-x","messages":[]}`, got.body)
	require.Equal(t, "Bearer entra-token", got.header.Get("Authorization"), "the worker's token must not reach the Provider")
	for _, name := range []string{"Api-Key", "Cookie", "X-Orka-Task"} {
		require.Empty(t, got.header.Get(name), name)
	}
	require.Equal(t, "models", h.identities.identity)
	require.Equal(t, h.upstream.URL, h.identities.targetURL)

	response, body = h.do(t, http.MethodPost, "/internal/v1/tasks/default/task-a/providers/primary/openai/responses?api-version=preview", nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.Equal(t, "/openai/responses", h.upstreamRequests()[1].path)
}

func TestProviderRelayRefusesRequestsOutsideTheAzurePaths(t *testing.T) {
	h := newProviderRelayHarness(t, nil)
	for _, tc := range []struct {
		method, target string
		status         int
	}{
		{http.MethodPost, "/internal/v1/tasks/default/task-a/providers/primary/openai/files?api-version=1", http.StatusNotFound},
		{http.MethodPost, "/internal/v1/tasks/default/task-a/providers/primary/openai/deployments/a/b/chat/completions", http.StatusNotFound},
		{http.MethodPost, "/internal/v1/tasks/default/task-a/providers/primary/openai/deployments/%2e%2e/chat/completions", http.StatusNotFound},
		{http.MethodPost, "/internal/v1/tasks/default/task-a/providers/primary/v1/chat/completions", http.StatusNotFound},
		{http.MethodPost, "/internal/v1/tasks/default/task-a/providers/primary/openai/responses?api-version=1&redirect=x", http.StatusBadRequest},
		{http.MethodPost, "/internal/v1/tasks/default/task-a/providers/primary/openai/responses?api-version=1&api-version=2", http.StatusBadRequest},
		{http.MethodGet, "/internal/v1/tasks/default/task-a/providers/primary/openai/responses", http.StatusMethodNotAllowed},
		{http.MethodPost, "/internal/v1/tasks/default/task-a/providers/secondary/openai/responses", http.StatusBadRequest},
		// The Job relays only the primary slot.
		{http.MethodPost, "/internal/v1/tasks/default/task-a/providers/fallback-0/openai/responses", http.StatusForbidden},
		// Another Task's relay is not this worker's.
		{http.MethodPost, "/internal/v1/tasks/default/task-b/providers/primary/openai/responses", http.StatusForbidden},
	} {
		response, body := h.do(t, tc.method, tc.target, nil)
		require.Equal(t, tc.status, response.StatusCode, "%s %s: %s", tc.method, tc.target, body)
	}
	require.Empty(t, h.upstreamRequests())
	require.Zero(t, h.identities.calls, "no token is issued for a refused request")
}

func TestProviderRelayChecksTheProviderTheJobWasBuiltFor(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*providerRelayFixture)
		status int
	}{
		"provider replaced":  {func(f *providerRelayFixture) { f.provider.UID = "other-uid" }, http.StatusConflict},
		"provider not ready": {func(f *providerRelayFixture) { f.provider.Status.Ready = false }, http.StatusServiceUnavailable},
		"provider has a key now": {func(f *providerRelayFixture) {
			f.provider.Spec.Auth = nil
			f.provider.Spec.SecretRef = &corev1alpha1.ProviderSecretRef{Name: "key"}
		}, http.StatusConflict},
		"provider deleted":      {func(f *providerRelayFixture) { f.provider.Name = "renamed" }, http.StatusConflict},
		"unreadable relay env":  {func(f *providerRelayFixture) { f.relayEnv = "{" }, http.StatusConflict},
		"no relay in the job":   {func(f *providerRelayFixture) { f.relayEnv = "" }, http.StatusForbidden},
		"container task":        {func(f *providerRelayFixture) { f.task.Spec.Type = corev1alpha1.TaskTypeContainer }, http.StatusForbidden},
		"no identities":         {func(f *providerRelayFixture) { f.noIdentities = true }, http.StatusNotImplemented},
		"task no longer active": {func(f *providerRelayFixture) { f.task.Status.Phase = corev1alpha1.TaskPhaseSucceeded }, http.StatusForbidden},
	} {
		t.Run(name, func(t *testing.T) {
			h := newProviderRelayHarness(t, tc.mutate)
			response, body := h.do(t, http.MethodPost, relayChatPath, nil)
			require.Equal(t, tc.status, response.StatusCode, body)
			require.Empty(t, h.upstreamRequests())
		})
	}
}

func TestProviderRelayFailsClosedWithoutAToken(t *testing.T) {
	h := newProviderRelayHarness(t, nil)
	h.identities.err = errors.New("AADSTS700213")
	response, body := h.do(t, http.MethodPost, relayChatPath, nil)
	require.Equal(t, http.StatusServiceUnavailable, response.StatusCode, body)
	require.NotContains(t, body, "AADSTS", "Entra details stay in the controller")
	require.Empty(t, h.upstreamRequests())
}

func TestProviderRelayBoundsConcurrentRequests(t *testing.T) {
	h := newProviderRelayHarness(t, nil)
	require.Equal(t, providerRelayMaxConcurrent, cap(h.handlers.providerRelaySlots))
	for range providerRelayMaxConcurrent {
		h.handlers.providerRelaySlots <- struct{}{}
	}
	response, body := h.do(t, http.MethodPost, relayChatPath, nil)
	require.Equal(t, http.StatusTooManyRequests, response.StatusCode, body)
	require.Empty(t, h.upstreamRequests())

	<-h.handlers.providerRelaySlots
	response, body = h.do(t, http.MethodPost, relayChatPath, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	// The finished request returned its slot.
	require.Len(t, h.handlers.providerRelaySlots, providerRelayMaxConcurrent-1)
}
