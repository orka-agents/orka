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

	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/llm"
	"github.com/orka-agents/orka/workers/common"
)

func TestAIWorkerFailureSummaryClassifiesUpstreamStatus(t *testing.T) {
	err := fmt.Errorf("completion failed: %w", &llm.ProviderError{
		Provider:   "openai",
		StatusCode: 429,
		Message:    `POST "https://relay.example/v1/responses": 429 Too Many Requests`,
	})

	got := aiWorkerFailureSummary(err)
	if !strings.HasPrefix(got, providerUpstreamErrorCode+": ") {
		t.Fatalf("aiWorkerFailureSummary() = %q, want the %s code", got, providerUpstreamErrorCode)
	}
	if !strings.Contains(got, "429 Too Many Requests") {
		t.Fatalf("aiWorkerFailureSummary() = %q, want the upstream detail preserved", got)
	}
}

func TestAIWorkerFailureSummaryClassifiesUnreachableProvider(t *testing.T) {
	// A refused dial never reaches an HTTP status but is still an upstream failure.
	err := fmt.Errorf("completion failed: %w", &llm.ProviderError{
		Provider: "openai",
		Message:  `Post "https://relay.example/v1/responses": dial tcp 10.96.0.7:443: connect: connection refused`,
	})

	got := aiWorkerFailureSummary(err)
	if !strings.HasPrefix(got, providerUpstreamErrorCode+": ") {
		t.Fatalf("aiWorkerFailureSummary() = %q, want the %s code", got, providerUpstreamErrorCode)
	}
	if !strings.Contains(got, "connection refused") {
		t.Fatalf("aiWorkerFailureSummary() = %q, want the transport detail preserved", got)
	}
}

func TestAIWorkerFailureSummaryLeavesWorkerFaultsUnclassified(t *testing.T) {
	for name, err := range map[string]error{
		"worker error":      fmt.Errorf("failed to create k8s client: no kubeconfig"),
		"unknown provider":  fmt.Errorf("failed to create LLM provider: %w", llm.ErrUnknownProvider),
		"missing api key":   fmt.Errorf("failed to create LLM provider: %w", llm.ErrAPIKeyRequired),
		"max iterations":    fmt.Errorf("max iterations reached without completion"),
		"empty final reply": fmt.Errorf("model returned an empty final response after one retry"),
	} {
		t.Run(name, func(t *testing.T) {
			if got := aiWorkerFailureSummary(err); strings.Contains(got, providerUpstreamErrorCode) {
				t.Fatalf("aiWorkerFailureSummary() = %q, want no upstream classification", got)
			}
		})
	}
}

func TestAIWorkerFailureSummaryPreservesWorkerErrorText(t *testing.T) {
	got := aiWorkerFailureSummary(fmt.Errorf("max iterations reached without completion"))
	if got != "max iterations reached without completion" {
		t.Fatalf("aiWorkerFailureSummary() = %q, want the error text unchanged", got)
	}
}

func TestAIWorkerFailureSummaryEmptyForNilError(t *testing.T) {
	if got := aiWorkerFailureSummary(nil); got != "" {
		t.Fatalf("aiWorkerFailureSummary(nil) = %q, want empty", got)
	}
}

func TestAIWorkerFailureSummaryCodeOnlyForBlankUpstreamDetail(t *testing.T) {
	got := aiWorkerFailureSummary(&llm.ProviderError{Provider: "openai", StatusCode: 503})
	if got != providerUpstreamErrorCode {
		t.Fatalf("aiWorkerFailureSummary() = %q, want %q", got, providerUpstreamErrorCode)
	}
}

func TestFinishAIWorkerRunReportsClassifiedFailure(t *testing.T) {
	recorder := common.NewFakeEventRecorder()
	err := fmt.Errorf("completion failed: %w", &llm.ProviderError{
		Provider: "openai", StatusCode: 429, Message: "429 Too Many Requests",
	})

	if got := finishAIWorkerRun(context.Background(), recorder, "rate-limited-task", err); !errors.Is(got, err) {
		t.Fatalf("finishAIWorkerRun() = %v, want the run error returned unchanged", got)
	}

	recorded := recorder.Events()
	if len(recorded) != 1 || recorded[0].Type != events.ExecutionEventTypeWorkerFailed {
		t.Fatalf("recorded events = %+v, want a single WorkerFailed event", recorded)
	}
	want := providerUpstreamErrorCode + ": completion failed: 429 Too Many Requests"
	if recorded[0].Summary != want {
		t.Fatalf("WorkerFailed summary = %q, want %q", recorded[0].Summary, want)
	}
}
