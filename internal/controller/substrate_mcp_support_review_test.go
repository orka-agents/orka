// Copyright (c) 2026. MIT License - see LICENSE file for details.

package controller

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func TestResolveSubstrateActorPoolReferenceTemplateBeforeFinalizer(t *testing.T) {
	patchError := errors.New("pool patch denied")
	tests := []struct {
		name              string
		template          corev1alpha1.WorkspaceTemplateReference
		readOnly          bool
		existingFinalizer bool
		patchError        error
		wantError         string
		wantPatch         bool
	}{
		{
			name:      "mismatched template name leaves pool unchanged",
			template:  corev1alpha1.WorkspaceTemplateReference{Name: "claude", Namespace: "default"},
			wantError: "uses template default/claude, want default/codex",
		},
		{
			name:      "mismatched template namespace leaves pool unchanged",
			template:  corev1alpha1.WorkspaceTemplateReference{Name: "codex", Namespace: "other"},
			wantError: "uses template other/codex, want default/codex",
		},
		{
			name:      "read-only mismatched template is rejected before persistence",
			template:  corev1alpha1.WorkspaceTemplateReference{Name: "claude", Namespace: "default"},
			readOnly:  true,
			wantError: "uses template default/claude, want default/codex",
		},
		{
			name:      "matching template persists finalizer",
			template:  corev1alpha1.WorkspaceTemplateReference{Name: "codex", Namespace: "default"},
			wantPatch: true,
		},
		{
			name:      "matching default template namespace persists finalizer",
			template:  corev1alpha1.WorkspaceTemplateReference{Name: "codex"},
			wantPatch: true,
		},
		{
			name:      "read-only matching template without finalizer fails closed",
			template:  corev1alpha1.WorkspaceTemplateReference{Name: "codex", Namespace: "default"},
			readOnly:  true,
			wantError: "cannot persist cleanup finalizer",
		},
		{
			name:              "read-only matching template with finalizer succeeds",
			template:          corev1alpha1.WorkspaceTemplateReference{Name: "codex", Namespace: "default"},
			readOnly:          true,
			existingFinalizer: true,
		},
		{
			name:       "matching template patch failure fails closed",
			template:   corev1alpha1.WorkspaceTemplateReference{Name: "codex", Namespace: "default"},
			patchError: patchError,
			wantError:  "failed to persist cleanup finalizer",
			wantPatch:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			scheme := runtime.NewScheme()
			if err := corev1alpha1.AddToScheme(scheme); err != nil {
				t.Fatalf("AddToScheme() error = %v", err)
			}
			pool := &corev1alpha1.SubstrateActorPool{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "codex-pool",
					Namespace:  "default",
					UID:        "original-pool",
					Labels:     map[string]string{"orka.ai/review": "preserve"},
					Finalizers: []string{"orka.ai/existing-cleanup"},
				},
				Spec: corev1alpha1.SubstrateActorPoolSpec{
					TemplateRef:  tt.template,
					TargetActors: 1,
				},
				Status: corev1alpha1.SubstrateActorPoolStatus{
					Phase:       corev1alpha1.SubstrateActorPoolPhaseReady,
					TemplateUID: "original-template",
				},
			}
			if tt.existingFinalizer {
				controllerutil.AddFinalizer(pool, substrateActorPoolFinalizer)
			}
			patchCalls := 0
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pool).
				WithInterceptorFuncs(interceptor.Funcs{
					Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
						patchCalls++
						if tt.patchError != nil {
							return tt.patchError
						}
						return c.Patch(ctx, obj, patch, opts...)
					},
				}).Build()
			key := types.NamespacedName{Namespace: pool.Namespace, Name: pool.Name}
			before := &corev1alpha1.SubstrateActorPool{}
			if err := c.Get(ctx, key, before); err != nil {
				t.Fatalf("Get pool before resolution: %v", err)
			}
			var reader client.Reader = c
			if tt.readOnly {
				reader = struct{ client.Reader }{Reader: c}
			}
			resolved, err := resolveSubstrateActorPoolReference(ctx, reader, key.Namespace, key.Name, "default", "codex")
			if tt.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Errorf("resolveSubstrateActorPoolReference() error = %v, want %q", err, tt.wantError)
				}
				if resolved != nil {
					t.Error("failed resolution returned a pool")
				}
			} else if err != nil {
				t.Errorf("resolveSubstrateActorPoolReference() error = %v", err)
			}
			if tt.patchError != nil && !errors.Is(err, tt.patchError) {
				t.Errorf("resolution error = %v, want wrapped patch error %v", err, tt.patchError)
			}
			wantPatchCalls := 0
			if tt.wantPatch {
				wantPatchCalls = 1
			}
			if patchCalls != wantPatchCalls {
				t.Errorf("Patch() calls = %d, want %d", patchCalls, wantPatchCalls)
			}
			after := &corev1alpha1.SubstrateActorPool{}
			if err := c.Get(ctx, key, after); err != nil {
				t.Fatalf("Get pool after resolution: %v", err)
			}
			want := before.DeepCopy()
			if tt.wantPatch && tt.wantError == "" {
				controllerutil.AddFinalizer(want, substrateActorPoolFinalizer)
				want.ResourceVersion = after.ResourceVersion
			}
			if !reflect.DeepEqual(after, want) {
				t.Errorf("stored pool changed unexpectedly: got %#v, want %#v", after, want)
			}
			if tt.wantError == "" && !reflect.DeepEqual(resolved, after) {
				t.Errorf("resolved pool = %#v, want persisted pool %#v", resolved, after)
			}
		})
	}
}
