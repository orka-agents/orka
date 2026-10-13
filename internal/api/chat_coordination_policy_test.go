/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/orka-agents/orka/internal/llm"
)

type coordinationPromptCapture struct {
	chatMockProvider
	messages []llm.Message
}

func (p *coordinationPromptCapture) Complete(ctx context.Context, req *llm.CompletionRequest) (*llm.CompletionResponse, error) {
	p.messages = append([]llm.Message(nil), req.Messages...)
	return p.chatMockProvider.Complete(ctx, req)
}

func TestChatIssueWorkflowRequiresTwoStageBootstrap(t *testing.T) {
	const providerType = "coordination-policy-fixture"
	provider := &coordinationPromptCapture{}
	llm.RegisterProvider(providerType, func(llm.ProviderConfig) (llm.Provider, error) {
		return provider, nil
	})
	objects := providerCRD(defaultNamespace, defaultNamespace, providerType, "test-model")
	kube := fake.NewClientBuilder().WithScheme(newTestScheme()).WithRuntimeObjects(objects...).Build()
	cfg := DefaultChatConfig()
	cfg.Provider = defaultNamespace
	ch := newTestChatHandler(t, kube, newTestSessionStore(t), newTestResultStore(t), cfg)
	app := fiber.New(fiber.Config{ErrorHandler: customErrorHandler})
	app.Post("/api/v1/chat", ch.HandleChat)
	body, err := json.Marshal(ChatRequest{Message: "Fix issue #42 in the repository", Namespace: defaultNamespace})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := app.Test(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NotEmpty(t, provider.messages)
	message := provider.messages[len(provider.messages)-1]
	require.Equal(t, "user", message.Role)
	for _, want := range []string{
		"Empty or omitted allowedAgents denies delegate_task",
		"Do not delegate in the bootstrap Task",
		"NEW Task",
		"Keep the creation Task",
	} {
		assert.Contains(t, message.Content, want)
	}
	assert.NotContains(t, message.Content, "one-shot pattern")
}
