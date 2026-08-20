/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"go.opentelemetry.io/otel/codes"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	orkatracing "github.com/orka-agents/orka/internal/tracing"
	tracingtest "github.com/orka-agents/orka/internal/tracing/testutil"
)

func TestRecordACPPromptOutcomeIfSettled(t *testing.T) {
	if _, err := orkatracing.Init("acp-telemetry-test", false); err != nil {
		t.Fatalf("initialize tracing: %v", err)
	}
	harness := tracingtest.NewSpanHarness(t)
	ctx, promptTrace := startACPSpan(context.Background(), acpPromptSpanName)
	if err := recordACPPromptOutcomeIfSettled(ctx, acpPromptOutcomeUnknown, nil); err != nil {
		t.Fatalf("record settled prompt outcome: %v", err)
	}
	promptTrace.End(nil)

	promptSpan := tracingtest.SpanNamed(harness.Recorder.Ended(), acpPromptSpanName)
	if promptSpan == nil {
		t.Fatal("missing acp.prompt span")
	}
	if got := tracingtest.AttributeMap(promptSpan)[acpAttrPromptOutcome].AsString(); got != acpPromptOutcomeUnknown {
		t.Fatalf("acp.prompt outcome = %q, want %q", got, acpPromptOutcomeUnknown)
	}
	if got := promptSpan.Status().Code; got != codes.Error {
		t.Fatalf("acp.prompt status = %s, want %s", got, codes.Error)
	}
}

func TestRecordACPPromptOutcomeIfSettlementFails(t *testing.T) {
	if _, err := orkatracing.Init("acp-telemetry-test", false); err != nil {
		t.Fatalf("initialize tracing: %v", err)
	}
	harness := tracingtest.NewSpanHarness(t)
	ctx, promptTrace := startACPSpan(context.Background(), acpPromptSpanName)
	settlementErr := errors.New("settlement failed")
	if err := recordACPPromptOutcomeIfSettled(ctx, acpPromptOutcomeFailed, settlementErr); !errors.Is(err, settlementErr) {
		t.Fatalf("settlement error = %v, want %v", err, settlementErr)
	}
	promptTrace.End(settlementErr)

	promptSpan := tracingtest.SpanNamed(harness.Recorder.Ended(), acpPromptSpanName)
	if promptSpan == nil {
		t.Fatal("missing acp.prompt span")
	}
	if _, ok := tracingtest.AttributeMap(promptSpan)[acpAttrPromptOutcome]; ok {
		t.Fatal("failed settlement recorded a terminal prompt outcome")
	}
}

func TestACPSessionSpanRenamesToContinuation(t *testing.T) {
	if _, err := orkatracing.Init("acp-telemetry-test", false); err != nil {
		t.Fatalf("initialize tracing: %v", err)
	}
	harness := tracingtest.NewSpanHarness(t)
	_, sessionTrace := startACPSessionSpan(context.Background(), nil)
	sessionTrace.setSessionReused(true)
	sessionTrace.End(nil)

	spans := harness.Recorder.Ended()
	if tracingtest.SpanNamed(spans, acpSessionCreateSpanName) != nil {
		t.Fatal("reused RuntimeSession retained the create span name")
	}
	if tracingtest.SpanNamed(spans, acpSessionContinueSpanName) == nil {
		t.Fatal("missing acp.session.continue span")
	}
}

func TestACPSessionSpanRecordsCreationFailure(t *testing.T) {
	if _, err := orkatracing.Init("acp-telemetry-test", false); err != nil {
		t.Fatalf("initialize tracing: %v", err)
	}
	harness := tracingtest.NewSpanHarness(t)
	_, sessionTrace := startACPSessionSpan(context.Background(), nil)
	sessionTrace.End(&harnessv2.ClientError{
		StatusCode: http.StatusBadRequest,
		Code:       harnessv2.ErrorCodeInvalidRequest,
		Kind:       harnessv2.ClientErrorValidation,
	})

	sessionSpan := tracingtest.SpanNamed(harness.Recorder.Ended(), acpSessionCreateSpanName)
	if sessionSpan == nil {
		t.Fatal("missing acp.session.create span")
	}
	if got := sessionSpan.Status().Code; got != codes.Error {
		t.Fatalf("acp.session.create status = %s, want %s", got, codes.Error)
	}
	if got := tracingtest.AttributeMap(sessionSpan)["error.type"].AsString(); got == "" {
		t.Fatal("acp.session.create error.type is empty")
	}
}
