/*
Copyright (c) 2026.
MIT License - see LICENSE file for details.
*/

package openai

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/orka-agents/orka/internal/llm"
)

// A token the issuer could not provide is a temporary failure: it keeps its
// 503 through the Azure request paths so the caller's retry budget applies.
func TestAzureTokenFailuresStayRetryable(t *testing.T) {
	newProvider := func(t *testing.T, mode apiMode) *Provider {
		t.Helper()
		provider, err := NewProvider(llm.ProviderConfig{
			BearerToken:         func(context.Context) (string, error) { return "", errors.New("issuer unavailable") },
			BaseURL:             "https://myresource.openai.azure.com",
			ProviderType:        "azure-openai",
			AzureDeploymentName: "gpt-x",
		})
		if err != nil {
			t.Fatal(err)
		}
		provider.mode.Store(int32(mode))
		return provider
	}
	retryable := func(t *testing.T, err error) {
		t.Helper()
		var providerErr *llm.ProviderError
		if !errors.As(err, &providerErr) || providerErr.StatusCode != http.StatusServiceUnavailable || !llm.ShouldRetry(err) {
			t.Fatalf("error = %#v, want a retryable 503 provider error", err)
		}
	}
	request := &llm.CompletionRequest{Model: "gpt-x", Messages: []llm.Message{{Role: "user", Content: "hi"}}}

	for name, mode := range map[string]apiMode{"chat completions": apiModeChatCompletions, "responses": apiModeResponses} {
		t.Run("complete "+name, func(t *testing.T) {
			_, err := newProvider(t, mode).Complete(context.Background(), request)
			retryable(t, err)
		})
		t.Run("stream "+name, func(t *testing.T) {
			stream, err := newProvider(t, mode).Stream(context.Background(), request)
			if err != nil {
				retryable(t, err)
				return
			}
			var streamErr error
			for chunk := range stream {
				if chunk.Error != nil {
					streamErr = chunk.Error
				}
			}
			retryable(t, streamErr)
		})
	}
}
