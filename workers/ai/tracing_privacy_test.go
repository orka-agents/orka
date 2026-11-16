/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/llm"
	"github.com/orka-agents/orka/internal/tracing/testutil"
	"github.com/orka-agents/orka/workers/common"
)

func TestAIWorkerErrorType(t *testing.T) {
	providerErr := &llm.ProviderError{Provider: "mock", StatusCode: 429, Message: "SYNTHETIC_TOKEN"}
	statuslessErr := &llm.ProviderError{Message: "SYNTHETIC_TOKEN"}
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"provider status", providerErr, "llm_status_429"},
		{"wrapped provider status", fmt.Errorf("model call: %w", providerErr), "llm_status_429"},
		{"canceled", fmt.Errorf("SYNTHETIC_TOKEN: %w", context.Canceled), "context.canceled"},
		{"deadline", context.DeadlineExceeded, "context.deadline_exceeded"},
		{"wrapped provider without status", fmt.Errorf("model call: %w", statuslessErr), "*llm.ProviderError"},
		{"plain", errors.New("SYNTHETIC_TOKEN"), "*errors.errorString"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := aiWorkerErrorType(tc.err); got != tc.want {
				t.Fatalf("aiWorkerErrorType() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAgentLoopErrorPrivacy(t *testing.T) {
	const marker = "SYNTHETIC_TOKEN_PRIVATE_BODY"
	for _, tc := range []struct {
		name     string
		provider *mockProvider
	}{
		{"provider failure", &mockProvider{err: &llm.ProviderError{Provider: "mock", StatusCode: 401, Message: marker}}},
		{"failed response", &mockProvider{response: &llm.CompletionResponse{Content: marker, StopReason: marker}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := testutil.NewSpanHarness(t)
			provider := llm.NewTracingProvider(tc.provider)
			_, err := executeAgentLoopWithEvents(
				context.Background(), provider, []llm.Message{{Role: roleUser, Content: marker}}, "", "test-model",
				modelSettings{maxTokens: 4096}, nil, nil, nil, common.NoopEventRecorder{},
			)
			if err == nil || !strings.Contains(err.Error(), marker) {
				t.Fatalf("error behavior changed: %v", err)
			}
			spans := h.Recorder.Ended()
			for _, span := range spans {
				testutil.AssertNoSpanText(t, span, marker)
			}
			step := testutil.SpanNamed(spans, "agent.step")
			if step == nil {
				t.Fatal("missing step span")
			}
			class := testutil.AttributeMap(step)["error.type"].AsString()
			if class == "" {
				t.Fatal("missing error classification")
			}
			testutil.AssertSpanError(t, step, class)
		})
	}
}
