// Copyright (c) 2026. MIT License - see LICENSE file for details.

package fakeentra_test

import (
	"context"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/llm"
	"github.com/orka-agents/orka/internal/llm/openai"
	"github.com/orka-agents/orka/test/fixtures/fakeentra"
)

func TestFakeEntraModelAnswersParseWithTheAzureSDK(t *testing.T) {
	server, source := newModelFixture(t)

	identities := source(assertion)
	provider, err := openai.NewProvider(llm.ProviderConfig{
		BearerToken:         func(ctx context.Context) (string, error) { return identities.Token(ctx, "models", server.URL) },
		BaseURL:             server.URL,
		ProviderType:        "azure-openai",
		AzureDeploymentName: "gpt-x",
		HTTPTransport:       server.Client().Transport,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := &llm.CompletionRequest{Model: "gpt-x", Messages: []llm.Message{{Role: "user", Content: "marker-3"}}}
	response, err := provider.Complete(context.Background(), request)
	if err != nil || response.Content != fakeentra.ModelReplyPrefix+" marker-3" {
		t.Fatalf("Complete() = %+v, %v", response, err)
	}
	stream, err := provider.Stream(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	var streamed strings.Builder
	for chunk := range stream {
		if chunk.Error != nil {
			t.Fatalf("stream error = %v", chunk.Error)
		}
		streamed.WriteString(chunk.Content)
	}
	if streamed.String() != fakeentra.ModelReplyPrefix+" marker-3" {
		t.Fatalf("streamed %q", streamed.String())
	}
	for _, call := range state(t, server).Models {
		if !call.Authorized {
			t.Fatalf("model call %+v was not authorized", call)
		}
	}
}
