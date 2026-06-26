/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tracing

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/labels"
	"github.com/sozercan/orka/internal/tracing/testutil"
)

func TestTaskTraceContextRoundTrip(t *testing.T) {
	_, err := Init("test", false)
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	h := testutil.NewSpanHarness(t)
	ctx, parent := Tracer("test").Start(context.Background(), "creator")
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: "default"}}
	StampTaskTraceContext(ctx, task)
	parent.End()
	if task.Annotations[labels.AnnotationTraceParent] == "" {
		t.Fatalf("missing %s annotation", labels.AnnotationTraceParent)
	}

	extracted := ExtractTaskTraceContext(context.Background(), task)
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
