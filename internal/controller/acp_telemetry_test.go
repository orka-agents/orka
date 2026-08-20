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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	orkatracing "github.com/orka-agents/orka/internal/tracing"
	tracingtest "github.com/orka-agents/orka/internal/tracing/testutil"
)

func TestRecordACPPromptOutcomeIfSettledEndsBeforeLaterError(t *testing.T) {
	tests := []struct {
		name      string
		outcome   string
		wantError bool
	}{
		{name: "failed", outcome: acpPromptOutcomeFailed, wantError: true},
		{name: "cancelled", outcome: acpPromptOutcomeCancelled},
		{name: "unknown", outcome: acpPromptOutcomeUnknown, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := orkatracing.Init("acp-telemetry-test", false); err != nil {
				t.Fatalf("initialize tracing: %v", err)
			}
			harness := tracingtest.NewSpanHarness(t)
			ctx, promptTrace := startACPSpan(context.Background(), acpPromptSpanName)
			if err := recordACPPromptOutcomeIfSettled(ctx, promptTrace, test.outcome, nil); err != nil {
				t.Fatalf("record settled prompt outcome: %v", err)
			}
			promptTrace.End(errors.New("runtime session cleanup failed"))

			promptSpan := tracingtest.SpanNamed(harness.Recorder.Ended(), acpPromptSpanName)
			if promptSpan == nil {
				t.Fatal("missing acp.prompt span")
			}
			if got := tracingtest.AttributeMap(promptSpan)[acpAttrPromptOutcome].AsString(); got != test.outcome {
				t.Fatalf("acp.prompt outcome = %q, want %q", got, test.outcome)
			}
			if got := promptSpan.Status().Code == codes.Error; got != test.wantError {
				t.Fatalf("acp.prompt error status = %t, want %t", got, test.wantError)
			}
			if _, ok := tracingtest.AttributeMap(promptSpan)["error.type"]; ok {
				t.Fatal("settled acp.prompt span recorded a later cleanup error")
			}
		})
	}
}

func TestRecordACPPromptOutcomeIfSettlementFails(t *testing.T) {
	if _, err := orkatracing.Init("acp-telemetry-test", false); err != nil {
		t.Fatalf("initialize tracing: %v", err)
	}
	harness := tracingtest.NewSpanHarness(t)
	ctx, promptTrace := startACPSpan(context.Background(), acpPromptSpanName)
	settlementErr := errors.New("settlement failed")
	if err := recordACPPromptOutcomeIfSettled(ctx, promptTrace, acpPromptOutcomeFailed, settlementErr); !errors.Is(err, settlementErr) {
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

func TestACPPromptSpanSettlementIgnoresLaterDeliveryError(t *testing.T) {
	if _, err := orkatracing.Init("acp-telemetry-test", false); err != nil {
		t.Fatalf("initialize tracing: %v", err)
	}
	harness := tracingtest.NewSpanHarness(t)
	ctx, promptTrace := startACPSpan(context.Background(), acpPromptSpanName)
	recordACPPromptOutcome(ctx, acpPromptOutcomeSucceeded)
	promptTrace.End(nil)
	promptTrace.End(errors.New("delivery failed after prompt settlement"))

	promptSpan := tracingtest.SpanNamed(harness.Recorder.Ended(), acpPromptSpanName)
	if promptSpan == nil {
		t.Fatal("missing acp.prompt span")
	}
	if got := tracingtest.AttributeMap(promptSpan)[acpAttrPromptOutcome].AsString(); got != acpPromptOutcomeSucceeded {
		t.Fatalf("acp.prompt outcome = %q, want %q", got, acpPromptOutcomeSucceeded)
	}
	if got := promptSpan.Status().Code; got == codes.Error {
		t.Fatalf("acp.prompt status = %s, want non-error after successful settlement", got)
	}
	if _, ok := tracingtest.AttributeMap(promptSpan)["error.type"]; ok {
		t.Fatal("settled acp.prompt span recorded a later delivery error")
	}
}

func TestACPSessionSpanUsesFinalReuseDecision(t *testing.T) {
	tests := []struct {
		name      string
		decisions []bool
		wantName  string
	}{
		{name: "continued", decisions: []bool{true}, wantName: acpSessionContinueSpanName},
		{name: "recreated after continuation plan", decisions: []bool{true, false}, wantName: acpSessionCreateSpanName},
		{name: "continued after create plan", decisions: []bool{false, true}, wantName: acpSessionContinueSpanName},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := orkatracing.Init("acp-telemetry-test", false); err != nil {
				t.Fatalf("initialize tracing: %v", err)
			}
			harness := tracingtest.NewSpanHarness(t)
			_, sessionTrace := startACPSessionSpan(context.Background(), nil)
			for _, reused := range test.decisions {
				sessionTrace.setSessionReused(reused)
			}
			sessionTrace.End(nil)

			if tracingtest.SpanNamed(harness.Recorder.Ended(), test.wantName) == nil {
				t.Fatalf("missing final session span %q", test.wantName)
			}
		})
	}
}

func TestACPSessionOutcome(t *testing.T) {
	settlementErr := errors.New("session failed")
	tests := []struct {
		name      string
		reused    bool
		completed bool
		err       error
		want      string
	}{
		{name: "created", completed: true, want: acpSessionOutcomeCreated},
		{name: "continued", reused: true, completed: true, want: acpSessionOutcomeContinued},
		{name: "incomplete", reused: true, want: acpSessionOutcomeIncomplete},
		{name: "failed", completed: true, err: settlementErr, want: acpSessionOutcomeFailed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := acpSessionOutcome(test.reused, test.completed, test.err); got != test.want {
				t.Fatalf("acpSessionOutcome() = %q, want %q", got, test.want)
			}
		})
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

func TestACPPublicationRecoveryContinuesTaskTraceWithoutPromptSpan(t *testing.T) {
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "recovery-task"}}
	harness, parentSpanID := stampACPTaskTrace(t, task)
	_, publicationTrace := startACPPublicationRecoverySpan(context.Background(), task, "publication-recovery")
	publicationTrace.End(nil)

	spans := harness.Recorder.Ended()
	if tracingtest.SpanNamed(spans, acpPromptSpanName) != nil {
		t.Fatal("publication recovery emitted an acp.prompt span")
	}
	publicationSpan := tracingtest.SpanNamed(spans, acpPublicationSpanName)
	if publicationSpan == nil {
		t.Fatal("missing acp.publication.reconcile span")
	}
	if got := publicationSpan.Parent().SpanID().String(); got != parentSpanID {
		t.Fatalf("publication recovery parent = %s, want Task trace parent %s", got, parentSpanID)
	}
	if !tracingtest.AttributeMap(publicationSpan)[acpAttrPublicationRecovery].AsBool() {
		t.Fatal("publication recovery span is not marked as recovery")
	}
}
