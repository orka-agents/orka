package controller

import (
	"context"
	"errors"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	orkatracing "github.com/orka-agents/orka/internal/tracing"
)

const (
	acpTracerName              = "orka.acp"
	acpPromptSpanName          = "acp.prompt"
	acpSessionCreateSpanName   = "acp.session.create"
	acpSessionContinueSpanName = "acp.session.continue"
	acpPublicationSpanName     = "acp.publication.reconcile"
	acpAttrPromptID            = "orka.acp.prompt.id"
	acpAttrTaskAttempt         = "orka.acp.task.attempt"
	acpAttrRuntimePoolName     = "orka.acp.runtime_pool.name"
	acpAttrRuntimeSessionUID   = "orka.acp.runtime_session.uid"
	acpAttrRuntimeSessionGen   = "orka.acp.runtime_session.generation"
	acpAttrPublicationID       = "orka.acp.publication.id"
	acpAttrPublicationRecovery = "orka.acp.publication.recovery"
	acpAttrPromptOutcome       = "orka.acp.prompt.outcome"
	acpPromptOutcomeSucceeded  = "succeeded"
	acpPromptOutcomeFailed     = "failed"
	acpPromptOutcomeCancelled  = "cancelled"
	acpPromptOutcomeUnknown    = "outcome_unknown"
)

// acpSpan makes span completion idempotent so a scoped span can be ended at the
// successful protocol boundary and still have a deferred error fallback.
type acpSpan struct {
	span  trace.Span
	ended bool
}

func startACPPromptSpan(ctx context.Context, task *corev1alpha1.Task) (context.Context, *acpSpan) {
	ctx = orkatracing.ExtractTaskTraceContext(ctx, task)
	return startACPSpan(ctx, acpPromptSpanName, acpTaskSpanAttributes(task)...)
}

func startACPSessionSpan(ctx context.Context, task *corev1alpha1.Task) (context.Context, *acpSpan) {
	ctx = orkatracing.ExtractTaskTraceContext(ctx, task)
	return startACPSpan(ctx, acpSessionCreateSpanName, acpTaskSpanAttributes(task)...)
}

func startACPPublicationSpan(
	ctx context.Context,
	task *corev1alpha1.Task,
	publicationID string,
	recovery bool,
) (context.Context, *acpSpan) {
	attrs := acpTaskSpanAttributes(task)
	if publicationID != "" {
		attrs = append(attrs, attribute.String(acpAttrPublicationID, publicationID))
	}
	attrs = append(attrs, attribute.Bool(acpAttrPublicationRecovery, recovery))
	return startACPSpan(ctx, acpPublicationSpanName, attrs...)
}

func startACPPublicationRecoverySpan(
	ctx context.Context,
	task *corev1alpha1.Task,
	publicationID string,
) (context.Context, *acpSpan) {
	ctx = orkatracing.ExtractTaskTraceContext(ctx, task)
	return startACPPublicationSpan(ctx, task, publicationID, true)
}

func startACPSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, *acpSpan) {
	ctx, span := orkatracing.Tracer(acpTracerName).Start(ctx, name, trace.WithAttributes(attrs...))
	return ctx, &acpSpan{span: span}
}

func acpTaskSpanAttributes(task *corev1alpha1.Task) []attribute.KeyValue {
	if task == nil {
		return nil
	}
	agentName := ""
	if task.Spec.AgentRef != nil {
		agentName = task.Spec.AgentRef.Name
	}
	attrs := orkatracing.TaskAttributes(task.Name, task.Namespace, task.Namespace, agentName, "")
	if task.Status.Execution == nil {
		return attrs
	}
	execution := task.Status.Execution
	if execution.Attempt > 0 {
		attrs = append(attrs, attribute.Int64(acpAttrTaskAttempt, int64(execution.Attempt)))
	}
	if execution.PromptID != "" {
		attrs = append(attrs, attribute.String(acpAttrPromptID, execution.PromptID))
	}
	if execution.RuntimePoolName != "" {
		attrs = append(attrs, attribute.String(acpAttrRuntimePoolName, execution.RuntimePoolName))
	}
	if execution.RuntimeSessionUID != "" {
		attrs = append(attrs, attribute.String(acpAttrRuntimeSessionUID, execution.RuntimeSessionUID))
	}
	if execution.RuntimeSessionGeneration > 0 {
		attrs = append(attrs, attribute.Int64(acpAttrRuntimeSessionGen, execution.RuntimeSessionGeneration))
	}
	return attrs
}

func (s *acpSpan) setRuntimeSession(uid string, generation uint64) {
	if s == nil || s.span == nil || s.ended {
		return
	}
	attrs := make([]attribute.KeyValue, 0, 2)
	if uid != "" {
		attrs = append(attrs, attribute.String(acpAttrRuntimeSessionUID, uid))
	}
	if generation > 0 {
		attrs = append(attrs, attribute.Int64(acpAttrRuntimeSessionGen, int64(generation)))
	}
	if len(attrs) > 0 {
		s.span.SetAttributes(attrs...)
	}
}

func recordACPPromptOutcome(ctx context.Context, outcome string) {
	span := trace.SpanFromContext(ctx)
	if !span.IsRecording() || outcome == "" {
		return
	}
	span.SetAttributes(attribute.String(acpAttrPromptOutcome, outcome))
	switch outcome {
	case acpPromptOutcomeFailed, acpPromptOutcomeUnknown:
		span.SetStatus(codes.Error, outcome)
	}
}

func recordACPPromptOutcomeIfSettled(ctx context.Context, outcome string, err error) error {
	if err == nil {
		recordACPPromptOutcome(ctx, outcome)
	}
	return err
}

func (s *acpSpan) setSessionReused(reused bool) {
	if s == nil || s.span == nil || s.ended {
		return
	}
	name := acpSessionCreateSpanName
	if reused {
		name = acpSessionContinueSpanName
	}
	s.span.SetName(name)
}

func (s *acpSpan) withContext(ctx context.Context) context.Context {
	if s == nil || s.span == nil || s.ended {
		return ctx
	}
	return trace.ContextWithSpan(ctx, s.span)
}

func (s *acpSpan) End(err error) {
	if s == nil || s.span == nil || s.ended {
		return
	}
	s.ended = true
	if err != nil {
		errType := acpSpanErrorType(err)
		s.span.SetAttributes(attribute.String("error.type", errType))
		s.span.SetStatus(codes.Error, errType)
	}
	s.span.End()
}

func acpSpanErrorType(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "context.canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "context.deadline_exceeded"
	default:
		return fmt.Sprintf("%T", err)
	}
}
