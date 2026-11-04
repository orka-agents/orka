// Copyright (c) 2026. MIT License - see LICENSE file for details.

package controller

import (
	"context"
	"errors"
	"reflect"
	"testing"

	workspace "github.com/orka-agents/orka-workspace/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

//nolint:gocyclo // Keep the source-loss matrix and full provider-ownership preservation assertions together.
func TestWorkspaceCheckpointRoutingSourceLoss(t *testing.T) {
	for _, test := range []struct {
		name, sourceState, failure                 string
		cacheOnly, routed, artifact, legacyPending bool
		wantError                                  bool
	}{
		{name: "missing exact source is terminal", sourceState: "missing", failure: "SourceNotFound"},
		{name: "replaced source is terminal", sourceState: "replaced", failure: "SourceUIDMismatch"},
		{name: "deleting source remains retryable", sourceState: "deleting", wantError: true},
		{name: "source API outage remains retryable", sourceState: "outage", wantError: true},
		{name: "cache-only source absence is not terminal proof", sourceState: "missing", cacheOnly: true, wantError: true},
		{name: "cache-only source replacement is not terminal proof", sourceState: "replaced", cacheOnly: true, wantError: true},
		{name: "routed independent checkpoint survives source deletion", sourceState: "missing", routed: true, artifact: true},
		{name: "routed independent checkpoint ignores source replacement", sourceState: "replaced", routed: true, artifact: true},
		{name: "legacy retained artifact keeps its original ownership", sourceState: "missing", artifact: true},
		{name: "legacy pending export keeps its reference owner", sourceState: "missing", legacyPending: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newExternalRuntimePoolFixture(t)
			checkpoint := &workspace.ExecutionWorkspaceCheckpoint{ObjectMeta: metav1.ObjectMeta{Namespace: f.workspace.Namespace, Name: "export", UID: "checkpoint-uid", Generation: 1,
				Annotations: map[string]string{"example.invalid/operator-note": "preserve"}},
				Spec:   workspace.ExecutionWorkspaceCheckpointSpec{WorkspaceRef: workspace.ObjectIdentityReference{Name: f.workspace.Name, UID: f.workspace.UID}},
				Status: workspace.ExecutionWorkspaceCheckpointStatus{Phase: "Pending"}}
			if test.routed {
				checkpoint.Labels = map[string]string{workspaceCheckpointProviderNameLabel: f.provider.Name, workspace.ProviderControllerLabel: f.provider.Spec.ControllerName}
			}
			if test.artifact {
				checkpoint.Finalizers = []string{"example.workspace.orka.ai/checkpoint-reference"}
				checkpoint.Status.Phase = "Ready"
				checkpoint.Status.Digest = f.workspace.Spec.ClassBinding.ProfileHash
				checkpoint.Status.ClassBinding = f.workspace.Spec.ClassBinding.DeepCopy()
				checkpoint.Status.CreatedAt = &metav1.Time{Time: runtimePoolTestNow}
				checkpoint.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "DataRetained", ObservedGeneration: 1}}
			}
			if test.legacyPending {
				checkpoint.Finalizers = []string{"orka.ai/substrate-checkpoint-reference"}
			}
			if err := f.r.Create(t.Context(), checkpoint); err != nil {
				t.Fatal(err)
			}
			if test.sourceState == "deleting" {
				f.workspace.Finalizers = []string{"example.workspace.orka.ai/source-protection"}
				if err := f.r.Update(t.Context(), f.workspace); err != nil {
					t.Fatal(err)
				}
			}
			if test.sourceState != "outage" {
				if err := f.r.Delete(t.Context(), f.workspace); err != nil {
					t.Fatal(err)
				}
			}
			if test.sourceState == "replaced" {
				replacement := f.workspace.DeepCopy()
				replacement.UID, replacement.ResourceVersion = "replacement-source-uid", ""
				if err := f.r.Create(t.Context(), replacement); err != nil {
					t.Fatal(err)
				}
			}
			sourceReads := 0
			reader := interceptor.NewClient(f.r.Client.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
				if _, ok := object.(*workspace.ExecutionWorkspace); ok {
					sourceReads++
					if test.sourceState == "outage" {
						return apierrors.NewServiceUnavailable("source API unavailable")
					}
				}
				return c.Get(ctx, key, object, opts...)
			}})
			r := &WorkspaceCheckpointRoutingReconciler{Client: f.r.Client, APIReader: reader}
			if test.cacheOnly {
				r.APIReader = nil
			}
			before := &workspace.ExecutionWorkspaceCheckpoint{}
			if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(checkpoint), before); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				result, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(checkpoint)})
				if (err != nil) != test.wantError || result.RequeueAfter != 0 {
					t.Fatalf("routing result = %+v, error = %v", result, err)
				}
			}
			got := &workspace.ExecutionWorkspaceCheckpoint{}
			if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(checkpoint), got); err != nil {
				t.Fatal(err)
			}
			if got.Annotations["workspace.orka.ai/checkpoint-routing-failure"] != test.failure || got.Annotations["example.invalid/operator-note"] != "preserve" ||
				got.UID != before.UID || !reflect.DeepEqual(got.Spec, before.Spec) || !reflect.DeepEqual(got.Status, before.Status) ||
				!reflect.DeepEqual(got.Labels, before.Labels) || !reflect.DeepEqual(got.Finalizers, before.Finalizers) || !reflect.DeepEqual(got.OwnerReferences, before.OwnerReferences) {
				t.Fatal("source-loss routing changed provider ownership/status or failed to persist its exact terminal diagnostic")
			}
			if test.failure != "" && sourceReads != 1 || (test.routed || test.artifact || test.legacyPending) && sourceReads != 0 {
				t.Fatalf("terminal/retained checkpoint performed %d source reads", sourceReads)
			}
		})
	}
}

func TestWorkspaceCheckpointRoutingFailureCannotOverwriteConcurrentProviderRoute(t *testing.T) {
	f := newExternalRuntimePoolFixture(t)
	checkpoint := &workspace.ExecutionWorkspaceCheckpoint{ObjectMeta: metav1.ObjectMeta{Namespace: f.workspace.Namespace, Name: "export", UID: "checkpoint-uid"},
		Spec: workspace.ExecutionWorkspaceCheckpointSpec{WorkspaceRef: workspace.ObjectIdentityReference{Name: f.workspace.Name, UID: f.workspace.UID}}}
	if err := f.r.Create(t.Context(), checkpoint); err != nil {
		t.Fatal(err)
	}
	reader := interceptor.NewClient(f.r.Client.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
		if _, ok := object.(*workspace.ExecutionWorkspace); ok {
			current := &workspace.ExecutionWorkspaceCheckpoint{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(checkpoint), current); err != nil {
				return err
			}
			current.Labels = map[string]string{workspaceCheckpointProviderNameLabel: f.provider.Name, workspace.ProviderControllerLabel: f.provider.Spec.ControllerName}
			if err := c.Update(ctx, current); err != nil {
				return err
			}
			return apierrors.NewNotFound(workspace.GroupVersion.WithResource("executionworkspaces").GroupResource(), key.Name)
		}
		return c.Get(ctx, key, object, opts...)
	}})
	r := &WorkspaceCheckpointRoutingReconciler{Client: f.r.Client, APIReader: reader}
	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(checkpoint)}); !apierrors.IsConflict(err) {
		t.Fatalf("stale routing failure must conflict: %v", err)
	}
	got := &workspace.ExecutionWorkspaceCheckpoint{}
	if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(checkpoint), got); err != nil {
		t.Fatal(err)
	}
	if got.Annotations["workspace.orka.ai/checkpoint-routing-failure"] != "" || got.Labels[workspaceCheckpointProviderNameLabel] != f.provider.Name {
		t.Fatal("stale source-loss observation overwrote accepted provider routing")
	}
	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(checkpoint)}); err != nil {
		t.Fatalf("established provider routing was re-opened: %v", err)
	}
}

func TestWorkspaceCheckpointRoutingAcceptedDiagnosticSurvivesLostPatchResponse(t *testing.T) {
	f := newExternalRuntimePoolFixture(t)
	checkpoint := &workspace.ExecutionWorkspaceCheckpoint{ObjectMeta: metav1.ObjectMeta{Namespace: f.workspace.Namespace, Name: "export", UID: "checkpoint-uid"},
		Spec: workspace.ExecutionWorkspaceCheckpointSpec{WorkspaceRef: workspace.ObjectIdentityReference{Name: f.workspace.Name, UID: f.workspace.UID}}}
	if err := f.r.Create(t.Context(), checkpoint); err != nil {
		t.Fatal(err)
	}
	if err := f.r.Delete(t.Context(), f.workspace); err != nil {
		t.Fatal(err)
	}
	patches := 0
	lostResponse := errors.New("routing patch response lost")
	c := interceptor.NewClient(f.r.Client.(client.WithWatch), interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, opts ...client.PatchOption) error {
		patches++
		if err := c.Patch(ctx, object, patch, opts...); err != nil {
			return err
		}
		return lostResponse
	}})
	r := &WorkspaceCheckpointRoutingReconciler{Client: c, APIReader: f.r.Client}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(checkpoint)}
	if _, err := r.Reconcile(t.Context(), request); !errors.Is(err, lostResponse) {
		t.Fatalf("lost accepted response was concealed: %v", err)
	}
	if _, err := r.Reconcile(t.Context(), request); err != nil || patches != 1 {
		t.Fatalf("accepted terminal diagnostic was retried: patches=%d, error=%v", patches, err)
	}
}
