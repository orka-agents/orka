/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/orka-agents/orka/internal/llm"
	"github.com/orka-agents/orka/internal/tracing/testutil"
)

func TestHandleChatErrorPrivacy(t *testing.T) {
	const marker = "SYNTHETIC_TOKEN_PRIVATE_BODY"
	const providerType = "trace-privacy-provider"
	h := testutil.NewSpanHarness(t)
	provider := &chatMockProvider{name: providerType, err: &llm.ProviderError{Provider: providerType, StatusCode: http.StatusUnauthorized, Message: marker}}
	llm.RegisterProvider(providerType, func(llm.ProviderConfig) (llm.Provider, error) { return provider, nil })
	objects := providerCRD(defaultNamespace, defaultNamespace, providerType, "test-model")
	kube := fake.NewClientBuilder().WithScheme(newTestScheme()).WithRuntimeObjects(objects...).Build()
	cfg := DefaultChatConfig()
	cfg.Provider = defaultNamespace
	handler := newTestChatHandler(t, kube, newTestSessionStore(t), newTestResultStore(t), cfg)
	app := fiber.New(fiber.Config{ErrorHandler: customErrorHandler})
	app.Post("/api/v1/chat", handler.HandleChat)
	body, err := json.Marshal(ChatRequest{Message: marker, SessionID: "privacy-session", Namespace: defaultNamespace})
	require.NoError(t, err)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/chat", bytes.NewReader(body))
	request = request.WithContext(context.Background())
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := app.Test(request)
	require.NoError(t, err)
	defer func() { require.NoError(t, response.Body.Close()) }()
	require.Equal(t, http.StatusInternalServerError, response.StatusCode)
	returned, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Contains(t, string(returned), marker, "HTTP error behavior must remain unchanged")
	spans := h.Recorder.Ended()
	for _, span := range spans {
		testutil.AssertNoSpanText(t, span, marker)
	}
	for name, class := range map[string]string{
		"chat.request":             "*llm.ProviderError",
		"chat.tool_loop.iteration": "*llm.ProviderError",
		"chat test-model":          "401",
	} {
		span := testutil.SpanNamed(spans, name)
		require.NotNil(t, span, "missing %s", name)
		testutil.AssertSpanError(t, span, class)
	}
}
