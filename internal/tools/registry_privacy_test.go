/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/orka-agents/orka/internal/tracing/genai"
	"github.com/orka-agents/orka/internal/tracing/testutil"
)

const privacyMarker = "SYNTHETIC_TOKEN_PRIVATE_BODY"

type privacyTestTool struct {
	tracingTestTool
	result string
	err    error
}

func (p privacyTestTool) Execute(context.Context, json.RawMessage) (string, error) {
	return p.result, p.err
}

func TestRegistryErrorPrivacy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result string
		err    error
		want   string
	}{
		{"go error", "", errors.New(privacyMarker), "*errors.errorString"},
		{"failed result", `{"success":false,"error":"` + privacyMarker + `","errorType":"invalid_arguments"}`, nil, "invalid_arguments"},
		{"untrusted error type", `{"success":false,"error":"` + privacyMarker + `","errorType":"` + privacyMarker + `"}`, nil, "tool_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := testutil.NewSpanHarness(t)
			metrics := testutil.NewMetricHarness(t)
			r := NewRegistry()
			r.Register(privacyTestTool{result: tc.result, err: tc.err})
			got, err := r.Execute(context.Background(), tracingToolName, json.RawMessage(`{}`))
			if got != tc.result || err != tc.err {
				t.Fatal("tool result changed")
			}
			span := findToolSpan(h.Recorder.Ended())
			testutil.AssertNoSpanText(t, span, privacyMarker)
			testutil.AssertSpanError(t, span, tc.want)
			assertToolMetricErrorTypes(t, metrics.Collect(t), tc.want)
		})
	}
}

func TestRejectedToolErrorPrivacy(t *testing.T) {
	for _, tc := range []struct {
		errType string
		want    string
	}{
		{"invalid_arguments", "invalid_arguments"},
		{"", "tool_rejected"},
		{privacyMarker, "tool_error"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			h := testutil.NewSpanHarness(t)
			metrics := testutil.NewMetricHarness(t)
			RecordRejectedToolCall(context.Background(), "tool", "call", tc.errType)
			span := testutil.SpanNamed(h.Recorder.Ended(), genai.OperationExecuteTool+" "+rejectedToolTelemetryName)
			testutil.AssertNoSpanText(t, span, privacyMarker)
			testutil.AssertSpanError(t, span, tc.want)
			assertToolMetricErrorTypes(t, metrics.Collect(t), tc.want)
		})
	}
}

func assertToolMetricErrorTypes(t *testing.T, rm metricdata.ResourceMetrics, want string) {
	t.Helper()
	found := 0
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			data, ok := m.Data.(metricdata.Histogram[float64])
			if m.Name != genai.MetricExecuteToolDuration || !ok {
				continue
			}
			for _, point := range data.DataPoints {
				found++
				if got, _ := point.Attributes.Value(genai.AttrErrorType); got.AsString() != want {
					t.Errorf("metric error.type = %q, want %q", got.AsString(), want)
				}
			}
		}
	}
	if found != 1 {
		t.Fatalf("%s data points = %d, want 1", genai.MetricExecuteToolDuration, found)
	}
}
