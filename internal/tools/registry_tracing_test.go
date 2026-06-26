/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/sozercan/orka/internal/tracing/genai"
	"github.com/sozercan/orka/internal/tracing/testutil"
)

type tracingTestTool struct{}

func (tracingTestTool) Name() string                { return "test_tool" }
func (tracingTestTool) Description() string         { return "test description" }
func (tracingTestTool) Parameters() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (tracingTestTool) Execute(context.Context, json.RawMessage) (string, error) {
	return `{"success":true}`, nil
}

func TestRegistryExecuteEmitsGenAIToolSpanAndMetric(t *testing.T) {
	spans := testutil.NewSpanHarness(t)
	metrics := testutil.NewMetricHarness(t)
	registry := NewRegistry()
	registry.Register(tracingTestTool{})
	ctx := WithToolContext(context.Background(), &ToolContext{ToolCallID: "call-1"})
	if _, err := registry.Execute(ctx, "test_tool", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	span := findToolSpan(spans.Recorder.Ended())
	if span == nil {
		t.Fatal("missing execute_tool span")
	}
	attrs := map[string]attribute.Value{}
	for _, kv := range span.Attributes() {
		attrs[string(kv.Key)] = kv.Value
	}
	if got := attrs[genai.AttrOperationName].AsString(); got != genai.OperationExecuteTool {
		t.Fatalf("operation attr = %q", got)
	}
	if got := attrs[genai.AttrToolName].AsString(); got != "test_tool" {
		t.Fatalf("tool name attr = %q", got)
	}
	if got := attrs[genai.AttrToolCallID].AsString(); got != "call-1" {
		t.Fatalf("tool call id attr = %q", got)
	}

	rm := metrics.Collect(t)
	if countMetricDataPoints(rm, genai.MetricExecuteToolDuration) != 1 {
		t.Fatalf("missing %s datapoint", genai.MetricExecuteToolDuration)
	}
}

func findToolSpan(spans []sdktrace.ReadOnlySpan) sdktrace.ReadOnlySpan {
	return findSpanByName(spans, "execute_tool test_tool")
}

func findSpanByName(spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	for _, span := range spans {
		if span.Name() == name {
			return span
		}
	}
	return nil
}

type failingTracingTestTool struct{}

func (failingTracingTestTool) Name() string        { return "failing_tool" }
func (failingTracingTestTool) Description() string { return "fails" }
func (failingTracingTestTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object"}`)
}
func (failingTracingTestTool) Execute(context.Context, json.RawMessage) (string, error) {
	return `{"success":false,"error":"bad input","errorType":"invalid_arguments"}`, nil
}

func TestRegistryExecuteMarksStructuredToolFailure(t *testing.T) {
	spans := testutil.NewSpanHarness(t)
	registry := NewRegistry()
	registry.Register(failingTracingTestTool{})
	if _, err := registry.Execute(context.Background(), "failing_tool", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	for _, span := range spans.Recorder.Ended() {
		if span.Name() == "execute_tool failing_tool" {
			if span.Status().Code != codes.Error {
				t.Fatalf("span status = %v, want error", span.Status())
			}
			return
		}
	}
	t.Fatal("missing failing tool span")
}

type plainJSONTracingTestTool struct{}

func (plainJSONTracingTestTool) Name() string        { return "plain_json_tool" }
func (plainJSONTracingTestTool) Description() string { return "plain json" }
func (plainJSONTracingTestTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object"}`)
}
func (plainJSONTracingTestTool) Execute(context.Context, json.RawMessage) (string, error) {
	return `{"items":[]}`, nil
}

func TestRegistryExecuteDoesNotFailPlainJSONSuccess(t *testing.T) {
	spans := testutil.NewSpanHarness(t)
	registry := NewRegistry()
	registry.Register(plainJSONTracingTestTool{})
	if _, err := registry.Execute(context.Background(), "plain_json_tool", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	for _, span := range spans.Recorder.Ended() {
		if span.Name() == "execute_tool plain_json_tool" {
			if span.Status().Code == codes.Error {
				t.Fatalf("span status = %v, want non-error", span.Status())
			}
			return
		}
	}
	t.Fatal("missing plain JSON tool span")
}

func TestRegistryExecuteMissingToolEmitsFailedSpanAndMetric(t *testing.T) {
	spans := testutil.NewSpanHarness(t)
	metrics := testutil.NewMetricHarness(t)
	registry := NewRegistry()

	_, err := registry.Execute(context.Background(), "missing_tool", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected missing tool error")
	}

	span := findSpanByName(spans.Recorder.Ended(), genai.OperationExecuteTool+" "+unknownToolTelemetryName)
	if span == nil {
		t.Fatal("missing execute_tool span for missing tool")
	}
	if span.Status().Code != codes.Error {
		t.Fatalf("span status = %v, want error", span.Status())
	}
	attrs := map[string]attribute.Value{}
	for _, kv := range span.Attributes() {
		attrs[string(kv.Key)] = kv.Value
	}
	if got := attrs[genai.AttrToolName].AsString(); got != unknownToolTelemetryName {
		t.Fatalf("tool name attr = %q, want unknown_tool", got)
	}
	rm := metrics.Collect(t)
	if countMetricDataPoints(rm, genai.MetricExecuteToolDuration) != 1 {
		t.Fatalf("missing %s datapoint", genai.MetricExecuteToolDuration)
	}
}
