// Copyright (c) 2026. MIT License - see LICENSE file for details.

package controller

import (
	"context"
	"reflect"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestWorkspaceCoreCleanupRequiresImmutablePoolReverseLink(t *testing.T) {
	for _, test := range []struct {
		name    string
		pointed bool
		change  func(*corev1alpha1.RuntimePool)
	}{
		{"copied labels with another workspace name", false, func(pool *corev1alpha1.RuntimePool) {
			pool.Spec.ExecutionWorkspace.WorkspaceRef.Name = "other-workspace"
		}},
		{"copied labels with another workspace UID", false, func(pool *corev1alpha1.RuntimePool) {
			pool.Spec.ExecutionWorkspace.WorkspaceRef.UID = "other-workspace-uid"
		}},
		{"copied labels without workspace reference", false, func(pool *corev1alpha1.RuntimePool) {
			pool.Spec.ExecutionWorkspace.WorkspaceRef = nil
		}},
		{"copied labels without external workload", false, func(pool *corev1alpha1.RuntimePool) {
			pool.Spec.ExecutionWorkspace.Workload = nil
		}},
		{"annotated pool with another workspace reference", true, func(pool *corev1alpha1.RuntimePool) {
			pool.Spec.ExecutionWorkspace.WorkspaceRef.Name = "other-workspace"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newExternalRuntimePoolFixture(t)
			ctx := context.Background()
			workspace := f.currentWorkspace(t)
			pool := runtimePoolTestGetPool(t, f.r, f.pool)
			pool.Finalizers = []string{runtimePoolFinalizer}
			if !test.pointed {
				pool.Name = "foreign-copied-metadata"
				pool.UID = types.UID("foreign-pool-uid")
				pool.ResourceVersion = ""
			}
			test.change(&pool)
			if test.pointed {
				if err := f.r.Update(ctx, &pool); err != nil {
					t.Fatal(err)
				}
			} else if err := f.r.Create(ctx, &pool); err != nil {
				t.Fatal(err)
			}
			before := pool.DeepCopy()
			cleanup := workspaceCoreCleanup{Client: f.r.Client, APIReader: f.r.Client}
			_, foreign, err := cleanup.ensureLinkedRuntimePoolDeleted(ctx, workspace)
			if err != nil || !foreign {
				t.Fatalf("cleanup must reject the foreign pool: foreign=%t error=%v", foreign, err)
			}
			var after corev1alpha1.RuntimePool
			if err := f.r.Get(ctx, client.ObjectKeyFromObject(&pool), &after); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, &after) {
				t.Fatal("copied public metadata authorized mutation or deletion of a foreign pool")
			}
		})
	}
}

func TestWorkspaceCoreCleanupDeletesExactReverseLinkWithoutMutableAnnotation(t *testing.T) {
	f := newExternalRuntimePoolFixture(t)
	workspace := f.currentWorkspace(t)
	delete(workspace.Annotations, acpExecutionWorkspacePoolAnnotation)
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	pool.Finalizers = []string{runtimePoolFinalizer}
	if err := f.r.Update(context.Background(), &pool); err != nil {
		t.Fatal(err)
	}
	cleanup := workspaceCoreCleanup{Client: f.r.Client, APIReader: f.r.Client}
	gone, foreign, err := cleanup.ensureLinkedRuntimePoolDeleted(context.Background(), workspace)
	if err != nil || gone || foreign {
		t.Fatalf("exact reverse link must be retired even after annotation loss: gone=%t foreign=%t error=%v", gone, foreign, err)
	}
	var after corev1alpha1.RuntimePool
	if err := f.r.Get(context.Background(), client.ObjectKeyFromObject(&pool), &after); err != nil {
		t.Fatal(err)
	}
	if after.DeletionTimestamp.IsZero() {
		t.Fatal("exact immutable reverse-linked pool was not deleted")
	}
}
