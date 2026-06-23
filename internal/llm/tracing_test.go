/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package llm

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/sozercan/orka/internal/tracing"
	"github.com/sozercan/orka/internal/tracing/genai"
	"github.com/sozercan/orka/internal/tracing/testutil"
)

func TestTracingProviderName(t *testing.T) {
	tp := NewTracingProvider(&mockProvider{name: "test-provider"})
	if got := tp.Name(); got != "test-provider" {
		t.Errorf("Name() = %q, want %q", got, "test-provider")
	}
}

func TestTracingProviderComplete(t *testing.T) {
	tests := []struct {
		name    string
		inner   Provider
		wantErr bool
	}{
		{
			name:    "success delegates to inner",
			inner:   &mockProvider{name: "test"},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tp := NewTracingProvider(tt.inner)
			resp, err := tp.Complete(context.Background(), &CompletionRequest{Model: "gpt-4"})
			if (err != nil) != tt.wantErr {
				t.Fatalf("Complete() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && resp == nil {
				t.Fatal("Complete() returned nil response")
			}
			if !tt.wantErr && resp.Content != "mock response" {
				t.Errorf("Content = %q, want %q", resp.Content, "mock response")
			}
		})
	}
}

// errorProvider always returns an error from Complete.
type errorProvider struct{ mockProvider }

func (e *errorProvider) Complete(_ context.Context, _ *CompletionRequest) (*CompletionResponse, error) {
	return nil, errors.New("api error")
}

func TestTracingProviderCompleteError(t *testing.T) {
	tp := NewTracingProvider(&errorProvider{mockProvider: mockProvider{name: "err"}})
	_, err := tp.Complete(context.Background(), &CompletionRequest{Model: "gpt-4"})
	if err == nil {
		t.Fatal("Complete() expected error")
	}
	if err.Error() != "api error" {
		t.Errorf("error = %q, want %q", err.Error(), "api error")
	}
}

func TestTracingProviderStream(t *testing.T) {
	tp := NewTracingProvider(&mockProvider{name: "test"})
	ch, err := tp.Stream(context.Background(), &CompletionRequest{Model: "gpt-4"})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	chunk := <-ch
	if chunk.Content != "mock" {
		t.Errorf("chunk.Content = %q, want %q", chunk.Content, "mock")
	}
}

type telemetryProvider struct {
	name          string
	telemetryName string
	resp          *CompletionResponse
	err           error
}

func (p *telemetryProvider) Name() string                  { return p.name }
func (p *telemetryProvider) TelemetryProviderName() string { return p.telemetryName }
func (p *telemetryProvider) Complete(context.Context, *CompletionRequest) (*CompletionResponse, error) {
	if p.err != nil {
		return nil, p.err
	}
	return p.resp, nil
}
func (p *telemetryProvider) Stream(context.Context, *CompletionRequest) (<-chan StreamChunk, error) {
	ch := make(chan StreamChunk, 2)
	ch <- StreamChunk{Content: "hello"}
	ch <- StreamChunk{Done: true, StopReason: "stop"}
	close(ch)
	return ch, nil
}

func TestTracingProviderCompleteEmitsGenAISpan(t *testing.T) {
	h := testutil.NewSpanHarness(t)
	mh := testutil.NewMetricHarness(t)
	parentCtx, parent := tracing.GenAITracer("test").Start(context.Background(), "parent")

	tp := NewTracingProvider(&telemetryProvider{
		name:          "openai",
		telemetryName: "azure-openai",
		resp: &CompletionResponse{
			Content:      "ok",
			Provider:     "azure-openai",
			ID:           "resp-1",
			Model:        "gpt-4o",
			StopReason:   "stop",
			InputTokens:  11,
			OutputTokens: 7,
		},
	})
	resp, err := tp.Complete(parentCtx, &CompletionRequest{
		Model:          "gpt-4o",
		MaxTokens:      64,
		Temperature:    0.2,
		StopSequences:  []string{"END"},
		ResponseFormat: &ResponseFormat{Type: "json_object"},
	})
	parent.End()
	if err != nil || resp == nil {
		t.Fatalf("Complete() resp=%#v err=%v", resp, err)
	}

	spans := h.Recorder.Ended()
	var span sdktrace.ReadOnlySpan
	for _, candidate := range spans {
		if candidate.Name() == "chat gpt-4o" {
			span = candidate
			break
		}
	}
	if span == nil {
		t.Fatalf("missing chat span, got %d spans", len(spans))
	}
	if span.SpanKind() != trace.SpanKindClient {
		t.Fatalf("SpanKind = %v, want client", span.SpanKind())
	}
	if span.Parent().SpanID() != trace.SpanFromContext(parentCtx).SpanContext().SpanID() {
		t.Fatalf("parent span id = %s, want %s", span.Parent().SpanID(), trace.SpanFromContext(parentCtx).SpanContext().SpanID())
	}
	attrs := spanAttrs(span)
	assertStringAttr(t, attrs, genai.AttrOperationName, genai.OperationChat)
	assertStringAttr(t, attrs, genai.AttrProviderName, genai.ProviderAzureOpenAI)
	assertStringAttr(t, attrs, genai.AttrRequestModel, "gpt-4o")
	assertIntAttr(t, attrs, genai.AttrRequestMaxTokens, 64)
	assertFloatAttr(t, attrs, genai.AttrRequestTemperature, 0.2)
	assertStringAttr(t, attrs, genai.AttrOutputType, "json_object")
	assertIntAttr(t, attrs, genai.AttrUsageInputTokens, 11)
	assertIntAttr(t, attrs, genai.AttrUsageOutputTokens, 7)
	assertStringAttr(t, attrs, genai.AttrResponseID, "resp-1")
	assertStringAttr(t, attrs, genai.AttrResponseModel, "gpt-4o")
	assertStringAttr(t, attrs, "llm.provider", genai.ProviderAzureOpenAI)
	assertIntAttr(t, attrs, "llm.input_tokens", 11)

	rm := mh.Collect(t)
	if got := histogramPointCount(rm, genai.MetricClientOperationDuration); got != 1 {
		t.Fatalf("operation duration datapoints = %d, want 1", got)
	}
	if got := histogramPointCount(rm, genai.MetricClientTokenUsage); got != 2 {
		t.Fatalf("token usage datapoints = %d, want 2", got)
	}
}

func TestTracingProviderCompleteErrorSetsErrorType(t *testing.T) {
	h := testutil.NewSpanHarness(t)
	tp := NewTracingProvider(&telemetryProvider{
		name:          "anthropic",
		telemetryName: "anthropic",
		err:           &ProviderError{Provider: "anthropic", Message: "rate limited", StatusCode: 429},
	})
	_, err := tp.Complete(context.Background(), &CompletionRequest{Model: "claude"})
	if err == nil {
		t.Fatal("expected error")
	}
	spans := h.Recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(spans))
	}
	attrs := spanAttrs(spans[0])
	assertStringAttr(t, attrs, genai.AttrErrorType, "429")
	if spans[0].Status().Code != codes.Error {
		t.Fatalf("status = %v, want error", spans[0].Status())
	}
}

func TestTracingProviderStreamEmitsFirstChunk(t *testing.T) {
	h := testutil.NewSpanHarness(t)
	mh := testutil.NewMetricHarness(t)
	tp := NewTracingProvider(&telemetryProvider{name: "anthropic", telemetryName: "anthropic"})
	ch, err := tp.Stream(context.Background(), &CompletionRequest{Model: "claude"})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	for range ch {
	}
	spans := h.Recorder.Ended()
	if len(spans) != 1 || spans[0].Name() != "chat claude" {
		t.Fatalf("spans = %#v", spans)
	}
	attrs := spanAttrs(spans[0])
	assertBoolAttr(t, attrs, genai.AttrRequestStream, true)
	if _, ok := attrs[genai.AttrResponseTimeToFirstChunk]; !ok {
		t.Fatalf("missing %s attr", genai.AttrResponseTimeToFirstChunk)
	}
	rm := mh.Collect(t)
	if got := histogramPointCount(rm, genai.MetricClientTimeToFirstChunk); got != 1 {
		t.Fatalf("time-to-first-chunk datapoints = %d, want 1", got)
	}
}

func spanAttrs(span sdktrace.ReadOnlySpan) map[string]attribute.Value {
	out := map[string]attribute.Value{}
	for _, kv := range span.Attributes() {
		out[string(kv.Key)] = kv.Value
	}
	return out
}

func assertStringAttr(t *testing.T, attrs map[string]attribute.Value, key, want string) {
	t.Helper()
	v, ok := attrs[key]
	if !ok {
		t.Fatalf("missing attr %q", key)
	}
	if got := v.AsString(); got != want {
		t.Fatalf("attr %q = %q, want %q", key, got, want)
	}
}

func assertIntAttr(t *testing.T, attrs map[string]attribute.Value, key string, want int64) {
	t.Helper()
	v, ok := attrs[key]
	if !ok {
		t.Fatalf("missing attr %q", key)
	}
	if got := v.AsInt64(); got != want {
		t.Fatalf("attr %q = %d, want %d", key, got, want)
	}
}

func assertFloatAttr(t *testing.T, attrs map[string]attribute.Value, key string, want float64) {
	t.Helper()
	v, ok := attrs[key]
	if !ok {
		t.Fatalf("missing attr %q", key)
	}
	if got := v.AsFloat64(); got != want {
		t.Fatalf("attr %q = %f, want %f", key, got, want)
	}
}

func assertBoolAttr(t *testing.T, attrs map[string]attribute.Value, key string, want bool) {
	t.Helper()
	v, ok := attrs[key]
	if !ok {
		t.Fatalf("missing attr %q", key)
	}
	if got := v.AsBool(); got != want {
		t.Fatalf("attr %q = %v, want %v", key, got, want)
	}
}

func histogramPointCount(rm metricdata.ResourceMetrics, name string) int {
	count := 0
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != name {
				continue
			}
			switch data := m.Data.(type) {
			case metricdata.Histogram[int64]:
				count += len(data.DataPoints)
			case metricdata.Histogram[float64]:
				count += len(data.DataPoints)
			}
		}
	}
	return count
}

func TestTracingProviderStreamStopsOnContextCancelWithoutConsumer(t *testing.T) {
	h := testutil.NewSpanHarness(t)
	tp := NewTracingProvider(&bufferedStreamProvider{telemetryProvider: telemetryProvider{name: "anthropic", telemetryName: "anthropic"}})
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := tp.Stream(ctx, &CompletionRequest{Model: "claude"})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	cancel()
	_ = ch // intentionally do not read: cancellation must unblock the forwarding goroutine.
	deadline := time.After(time.Second)
	for {
		if len(h.Recorder.Ended()) == 1 {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("stream span did not end after context cancellation")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

type bufferedStreamProvider struct{ telemetryProvider }

func (p *bufferedStreamProvider) Stream(context.Context, *CompletionRequest) (<-chan StreamChunk, error) {
	ch := make(chan StreamChunk, 1)
	ch <- StreamChunk{Content: "hello"}
	close(ch)
	return ch, nil
}
