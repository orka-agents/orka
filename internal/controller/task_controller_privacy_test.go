/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/tracing/testutil"
)

func TestTaskReconcileErrorPrivacy(t *testing.T) {
	const marker = "SYNTHETIC_TOKEN_PRIVATE_BODY"
	for _, status := range []bool{false, true} {
		name := "finalizer"
		if status {
			name = "initial status"
		}
		t.Run(name, func(t *testing.T) {
			h := testutil.NewSpanHarness(t)
			task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "privacy-task", Namespace: "default"}}
			if status {
				task.Finalizers = []string{labels.TaskFinalizer}
			}
			r := newUnitReconciler(newTestScheme(), task)
			wantErr := errors.New(marker)
			r.Client = fake.NewClientBuilder().WithScheme(r.Scheme).WithObjects(task).WithStatusSubresource(&corev1alpha1.Task{}).WithInterceptorFuncs(interceptor.Funcs{
				Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
					return wantErr
				},
				SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
					return wantErr
				},
			}).Build()
			_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: task.Name, Namespace: task.Namespace}})
			if !errors.Is(err, wantErr) {
				t.Fatalf("Reconcile error = %v", err)
			}
			spans := h.Recorder.Ended()
			for _, span := range spans {
				testutil.AssertNoSpanText(t, span, marker)
			}
			testutil.AssertSpanError(t, testutil.SpanNamed(spans, "task.reconcile"), "*errors.errorString")
		})
	}
}
