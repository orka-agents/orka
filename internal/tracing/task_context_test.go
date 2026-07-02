/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tracing

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/baggage"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/tracing/testutil"
)

func TestTaskTraceContextRoundTrip(t *testing.T) {
	_, err := Init("test", false)
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	h := testutil.NewSpanHarness(t)
	bg, err := baggage.Parse("tenant=acme")
	if err != nil {
		t.Fatalf("parse baggage: %v", err)
	}
	ctx := baggage.ContextWithBaggage(context.Background(), bg)
	ctx, parent := Tracer("test").Start(ctx, "creator")
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: "default"}}
	StampTaskTraceContext(ctx, task)
	parent.End()
	if task.Annotations[labels.AnnotationTraceParent] == "" {
		t.Fatalf("missing %s annotation", labels.AnnotationTraceParent)
	}
	if got := task.Annotations[labels.AnnotationTraceBaggage]; got != "" {
		t.Fatalf("%s = %q, want omitted", labels.AnnotationTraceBaggage, got)
	}

	extracted := ExtractTaskTraceContext(context.Background(), task)
	if got := baggage.FromContext(extracted).String(); got != "" {
		t.Fatalf("extracted baggage = %q, want omitted", got)
	}
	_, child := Tracer("test").Start(extracted, "controller")
	child.End()
	spans := h.Recorder.Ended()
	if len(spans) != 2 {
		t.Fatalf("ended spans = %d, want 2", len(spans))
	}
	var creatorID string
	for _, span := range spans {
		if span.Name() == "creator" {
			creatorID = span.SpanContext().SpanID().String()
		}
	}
	for _, span := range spans {
		if span.Name() == "controller" {
			if got := span.Parent().SpanID().String(); got != creatorID {
				t.Fatalf("controller parent = %s, want creator %s", got, creatorID)
			}
			return
		}
	}
	t.Fatal("missing controller span")
}
