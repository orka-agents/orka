/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tracing

import (
	"context"
	"errors"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/orka-agents/orka/internal/tracing/genai"
)

// Standard-library wrapper types that add only message text.
var transparentWrapperTypes = map[string]struct{}{
	"*fmt.wrapError":    {},
	"*fmt.wrapErrors":   {},
	"*errors.joinError": {},
}

// RecordSpanError marks span as failed with ErrorType(err). It never reads
// err.Error(), so error text cannot reach the trace backend. Use it instead of
// span.RecordError, which exports the message as-is.
func RecordSpanError(span trace.Span, err error) {
	if err == nil || span == nil || !span.IsRecording() {
		return
	}
	RecordSpanErrorType(span, ErrorType(err))
}

// RecordSpanErrorType marks span as failed with a trusted, low-cardinality
// error class, such as an HTTP status code or a fixed outcome name. The class
// becomes the status description, the error.type attribute and the type of an
// exception event that has no message. Never pass text taken from an error
// message, a provider response or a tool result.
func RecordSpanErrorType(span trace.Span, errorType string) {
	if errorType == "" || span == nil || !span.IsRecording() {
		return
	}
	span.SetStatus(codes.Error, errorType)
	span.SetAttributes(attribute.String(genai.AttrErrorType, errorType))
	span.AddEvent(semconv.ExceptionEventName, trace.WithAttributes(semconv.ExceptionType(errorType)))
}

// ErrorType classifies err without reading its message. Context cancellation
// and deadlines get fixed names. Otherwise it returns the Go type of the error
// beneath standard-library wrappers that hold exactly one error, such as
// fmt.Errorf("...: %w", err) or errors.Join(err, nil), because a wrapper's type
// says nothing about the failure.
func ErrorType(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.Canceled):
		return "context.canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "context.deadline_exceeded"
	}
	for {
		errorType := fmt.Sprintf("%T", err)
		if _, ok := transparentWrapperTypes[errorType]; !ok {
			return errorType
		}
		var wrapped []error
		switch unwrapper := err.(type) {
		case interface{ Unwrap() []error }:
			wrapped = unwrapper.Unwrap()
		case interface{ Unwrap() error }:
			wrapped = []error{unwrapper.Unwrap()}
		}
		if len(wrapped) != 1 || wrapped[0] == nil {
			return errorType
		}
		err = wrapped[0]
	}
}
