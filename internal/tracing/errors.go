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

var transparentWrapperTypes = map[string]struct{}{
	"*fmt.wrapError":    {},
	"*fmt.wrapErrors":   {},
	"*errors.joinError": {},
}

func RecordSpanError(span trace.Span, err error) {
	if err == nil || span == nil || !span.IsRecording() {
		return
	}
	RecordSpanErrorType(span, ErrorType(err))
}

func RecordSpanErrorType(span trace.Span, errorType string) {
	if errorType == "" || span == nil || !span.IsRecording() {
		return
	}
	span.SetStatus(codes.Error, errorType)
	span.SetAttributes(attribute.String(genai.AttrErrorType, errorType))
	span.AddEvent(semconv.ExceptionEventName, trace.WithAttributes(semconv.ExceptionType(errorType)))
}

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
