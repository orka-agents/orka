/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tracing_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/orka-agents/orka/internal/tracing"
	"github.com/orka-agents/orka/internal/tracing/testutil"
)

type unreadableError struct{}

func (unreadableError) Error() string { panic("error messages must never be read") }

func TestRecordSpanErrorPrivacy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		record func(trace.Span)
		want   string
	}{
		{"message", func(span trace.Span) {
			tracing.RecordSpanError(span, errors.New("https://example.invalid?token=SYNTHETIC_TOKEN "+strings.Repeat("PRIVATE_BODY", 1000)))
		}, "*errors.errorString"},
		{"wrapped", func(span trace.Span) {
			tracing.RecordSpanError(span, errors.Join(errors.New("SYNTHETIC_TOKEN"), unreadableError{}))
		}, "*errors.joinError"},
		{"type only", func(span trace.Span) { tracing.RecordSpanError(span, unreadableError{}) }, "tracing_test.unreadableError"},
		{"classification", func(span trace.Span) { tracing.RecordSpanErrorType(span, "400") }, "400"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := testutil.NewSpanHarness(t)
			_, span := h.Provider.Tracer("test").Start(context.Background(), "operation")
			tc.record(span)
			span.End()
			got := h.Recorder.Ended()[0]
			testutil.AssertNoSpanText(t, got, "SYNTHETIC_TOKEN", "PRIVATE_BODY")
			testutil.AssertSpanError(t, got, tc.want)
		})
	}
}

type classifiedError struct{}

func (classifiedError) Error() string { return "SYNTHETIC_TOKEN" }

func TestErrorType(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"plain", errors.New("SYNTHETIC_TOKEN"), "*errors.errorString"},
		{"canceled", context.Canceled, "context.canceled"},
		{"wrapped canceled", fmt.Errorf("SYNTHETIC_TOKEN: %w", context.Canceled), "context.canceled"},
		{"deadline", fmt.Errorf("SYNTHETIC_TOKEN: %w", context.DeadlineExceeded), "context.deadline_exceeded"},
		{"wrapped typed", fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", classifiedError{})), "tracing_test.classifiedError"},
		{"wrap without cause", fmt.Errorf("SYNTHETIC_TOKEN %w", nil), "*fmt.wrapError"},
		{"join of one", fmt.Errorf("outer: %w", errors.Join(classifiedError{}, nil)), "tracing_test.classifiedError"},
		{"joined", errors.Join(errors.New("SYNTHETIC_TOKEN"), unreadableError{}), "*errors.joinError"},
		{"two wrapped", fmt.Errorf("%w: %w", classifiedError{}, errors.New("SYNTHETIC_TOKEN")), "*fmt.wrapErrors"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tracing.ErrorType(tc.err); got != tc.want {
				t.Fatalf("ErrorType() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRecordSpanErrorNoop(t *testing.T) {
	h := testutil.NewSpanHarness(t)
	_, span := h.Provider.Tracer("test").Start(context.Background(), "operation")
	tracing.RecordSpanError(span, nil)
	tracing.RecordSpanErrorType(span, "")
	tracing.RecordSpanError(nil, errors.New("ignored"))
	tracing.RecordSpanErrorType(nil, "ignored")
	_, notRecording := noop.NewTracerProvider().Tracer("test").Start(context.Background(), "operation")
	tracing.RecordSpanError(notRecording, unreadableError{})
	span.End()
	got := h.Recorder.Ended()[0]
	if len(got.Events()) != 0 || len(got.Attributes()) != 0 || got.Status().Description != "" {
		t.Fatal("recording no error changed the span")
	}
}
