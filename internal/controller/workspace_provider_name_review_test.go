// Copyright (c) 2026. MIT License - see LICENSE file for details.

package controller

import (
	"reflect"
	"strings"
	"testing"
	"time"

	workspace "github.com/orka-agents/orka-workspace/api/v1alpha1"
	workspaceprovider "github.com/orka-agents/orka-workspace/sdk"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metadata "k8s.io/apimachinery/pkg/api/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func unsupportedWorkspaceProviderName() string {
	return "provider-" + strings.Repeat("a", 55)
}

func TestWorkspaceProviderNameCompatibilityAtGenericAdmission(t *testing.T) {
	for _, name := range []string{"provider", strings.Repeat("a", 63), unsupportedWorkspaceProviderName()} {
		t.Run(name, func(t *testing.T) {
			class, provider, w := workspacePolicyReviewFixture(t)
			provider.Name, class.Spec.ProviderRef.Name, w.Spec.ProviderBinding.Name = name, name, name
			provider.Status.LastHeartbeat = &metav1.Time{Time: runtimePoolTestNow}
			refreshWorkspacePolicyReviewProfile(t, class, provider, w)
			mapper, profile := testParameterMapping(class.Namespace, class.Spec.ParametersRef)
			c := fake.NewClientBuilder().WithScheme(testWorkspaceScheme(t)).WithStatusSubresource(class, provider, w).
				WithObjects(class, provider, w, profile).Build()
			supported := name != unsupportedWorkspaceProviderName()
			checkProviderNameWorkspaceAdmission(t, c, w, mapper, supported)
			checkProviderNameClassAdmission(t, c, class, mapper, supported)
			checkProviderNameRegistrationAdmission(t, c, provider, supported)
		})
	}
}

func checkProviderNameWorkspaceAdmission(t *testing.T, c client.Client, w *workspace.ExecutionWorkspace, mapper apimeta.RESTMapper, supported bool) {
	t.Helper()
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(w)}
	r := &ExecutionWorkspaceReconciler{Client: c, APIReader: c, RESTMapper: mapper}
	// Already-published Ready conditions must not bypass live validation.
	for range 4 {
		if _, err := r.Reconcile(t.Context(), request); err != nil {
			t.Fatal(err)
		}
	}
	got := &workspace.ExecutionWorkspace{}
	if err := c.Get(t.Context(), request.NamespacedName, got); err != nil {
		t.Fatal(err)
	}
	if (got.Spec.CoreAdmission != nil) != supported || got.Spec.Workload != nil || got.Status.Allocation != nil {
		t.Fatal("registration name compatibility did not gate new workspace admission before workload effects")
	}
	if !supported {
		admitted := workspaceprovider.FindCondition(got.Status.Conditions, string(workspace.ConditionWorkspaceAdmitted))
		if admitted == nil || admitted.Status != metav1.ConditionFalse || admitted.Reason != "ProviderNameUnsupported" {
			t.Fatalf("new workspace denial = %#v", admitted)
		}
	}

}

func checkProviderNameClassAdmission(t *testing.T, c client.Client, class *workspace.ExecutionWorkspaceClass, mapper apimeta.RESTMapper, supported bool) {
	t.Helper()
	classReconciler := &ExecutionWorkspaceClassReconciler{Client: c, APIReader: c, RESTMapper: mapper}
	if _, err := classReconciler.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(class)}); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(class), class); err != nil {
		t.Fatal(err)
	}
	ready := workspaceprovider.FindCondition(class.Status.Conditions, string(workspace.ConditionClassReady))
	if ready == nil || (ready.Status == metav1.ConditionTrue) != supported || (!supported && ready.Reason != "ProviderNameUnsupported") {
		t.Fatalf("class readiness = %#v", ready)
	}

}

func checkProviderNameRegistrationAdmission(t *testing.T, c client.Client, provider *workspace.ExecutionWorkspaceProvider, supported bool) {
	t.Helper()
	providerReconciler := &ExecutionWorkspaceProviderReconciler{Client: c, APIReader: c, RESTMapper: testProviderParameterMapper(apimeta.RESTScopeRoot), Now: func() time.Time { return runtimePoolTestNow }}
	for range 2 {
		if _, err := providerReconciler.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(provider)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(provider), provider); err != nil {
		t.Fatal(err)
	}
	ready := workspaceprovider.FindCondition(provider.Status.Conditions, string(workspace.ConditionProviderReady))
	if ready == nil || (ready.Status == metav1.ConditionTrue) != supported || (!supported && ready.Reason != "ProviderNameUnsupported") {
		t.Fatalf("provider readiness = %#v", ready)
	}
}

func TestWorkspaceProviderNameLimitIsCoreCompatibilityNotKubernetesNameValidity(t *testing.T) {
	object := metav1.ObjectMeta{Name: unsupportedWorkspaceProviderName()}
	if errors := metadata.ValidateObjectMeta(&object, false, metadata.NameIsDNSSubdomain, field.NewPath("metadata")); len(errors) != 0 {
		t.Fatalf("long DNS-subdomain registration is not a valid Kubernetes object: %v", errors)
	}
	object.Labels = map[string]string{workspaceCheckpointProviderNameLabel: object.Name}
	if errors := metadata.ValidateObjectMeta(&object, false, metadata.NameIsDNSSubdomain, field.NewPath("metadata")); len(errors) == 0 {
		t.Fatal("exact long registration name unexpectedly fits a checkpoint routing label")
	}
}

func TestExternalACPProviderNameRejectedBeforeTaskWorkspaceCreation(t *testing.T) {
	r, class, provider, config := externalACPFixture(t, true)
	if err := r.Delete(t.Context(), provider); err != nil {
		t.Fatal(err)
	}
	provider.Name, provider.ResourceVersion = unsupportedWorkspaceProviderName(), ""
	if err := r.Create(t.Context(), provider); err != nil {
		t.Fatal(err)
	}
	class.Spec.ProviderRef.Name = provider.Name
	if err := r.Update(t.Context(), class); err != nil {
		t.Fatal(err)
	}
	profile := &unstructured.Unstructured{}
	profile.SetGroupVersionKind(config.GroupVersionKind().GroupVersion().WithKind("WorkspaceProfile"))
	if err := r.Get(t.Context(), client.ObjectKey{Namespace: class.Namespace, Name: class.Spec.ParametersRef.Name}, profile); err != nil {
		t.Fatal(err)
	}
	hash, err := externalACPClassProfileHash(class, provider, config, profile)
	if err != nil {
		t.Fatal(err)
	}
	class.Status.ProfileHash = hash
	class.Status.ProviderRef = &workspace.ClusterObjectReference{Name: provider.Name}
	if err := r.Status().Update(t.Context(), class); err != nil {
		t.Fatal(err)
	}
	if _, err := r.resolveACPWorkspaceClass(t.Context(), acpClassTestTask()); err == nil || !strings.Contains(err.Error(), "registration name") {
		t.Fatalf("Task class resolver accepted an unroutable registration: %v", err)
	}
	workspaces := &workspace.ExecutionWorkspaceList{}
	if err := r.List(t.Context(), workspaces); err != nil || len(workspaces.Items) != 0 {
		t.Fatalf("unsupported registration created workspaces: count=%d, error=%v", len(workspaces.Items), err)
	}
}

func TestUnsupportedWorkspaceProviderNameDoesNotBlockExistingCleanup(t *testing.T) {
	class, provider, w := workspacePolicyReviewFixture(t)
	provider.Name, class.Spec.ProviderRef.Name, w.Spec.ProviderBinding.Name = unsupportedWorkspaceProviderName(), unsupportedWorkspaceProviderName(), unsupportedWorkspaceProviderName()
	refreshWorkspacePolicyReviewProfile(t, class, provider, w)
	w.Spec.DesiredState = workspace.ExecutionWorkspaceDesiredDeleted
	markWorkspaceAdmittedForPolicyReview(w, w.Generation)
	w.Finalizers = []string{executionWorkspaceFinalizer}
	mapper, profile := testParameterMapping(class.Namespace, class.Spec.ParametersRef)
	c := fake.NewClientBuilder().WithScheme(testWorkspaceScheme(t)).WithStatusSubresource(w).WithObjects(class, provider, w, profile).Build()
	r := &ExecutionWorkspaceReconciler{Client: c, APIReader: c, RESTMapper: mapper}
	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(w)}); err != nil {
		t.Fatal(err)
	}
	got := &workspace.ExecutionWorkspace{}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(w), got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.DesiredState != workspace.ExecutionWorkspaceDesiredDeleted || got.Spec.CoreAdmission == nil {
		t.Fatal("name compatibility gate rewrote an existing cleanup intent")
	}
	if admitted := workspaceprovider.FindCondition(got.Status.Conditions, string(workspace.ConditionWorkspaceAdmitted)); admitted == nil || admitted.Status != metav1.ConditionTrue {
		t.Fatalf("existing exact cleanup binding was rejected: %#v", admitted)
	}
	// Registration finalization is likewise independent of new admission.
	provider.Finalizers = []string{executionWorkspaceProviderFinalizer}
	isolated := fake.NewClientBuilder().WithScheme(testWorkspaceScheme(t)).WithObjects(provider).Build()
	if err := isolated.Delete(t.Context(), provider); err != nil {
		t.Fatal(err)
	}
	if _, err := (&ExecutionWorkspaceProviderReconciler{Client: isolated, APIReader: isolated}).Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(provider)}); err != nil {
		t.Fatal(err)
	}
	if err := isolated.Get(t.Context(), client.ObjectKeyFromObject(provider), &workspace.ExecutionWorkspaceProvider{}); !apierrors.IsNotFound(err) {
		t.Fatalf("unsupported registration could not finalize after exact references retired: %v", err)
	}
}

func TestWorkspaceCheckpointUnsupportedProviderNameClosesOnlyNewRouting(t *testing.T) {
	for _, mode := range []string{"new request", "disabled", "cache-only", "routed", "retained artifact", "legacy pending"} {
		t.Run(mode, func(t *testing.T) {
			f, checkpoint := unsupportedProviderCheckpointFixture(t, mode)
			before := checkpoint.DeepCopy()
			r := &WorkspaceCheckpointRoutingReconciler{Client: f.r.Client, APIReader: f.r.Client}
			if mode == "cache-only" {
				r.APIReader = nil
			}
			for range 2 {
				result, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(checkpoint)})
				if (err != nil) != (mode == "cache-only") || result.RequeueAfter != 0 {
					t.Fatalf("routing result=%+v, error=%v", result, err)
				}
			}
			if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(checkpoint), checkpoint); err != nil {
				t.Fatal(err)
			}
			if mode == "new request" || mode == "disabled" {
				if checkpoint.Annotations[workspaceCheckpointRoutingFailureAnnotation] != "ProviderNameUnsupported" {
					t.Fatal("unroutable checkpoint did not receive a durable Core diagnostic")
				}
				delete(checkpoint.Annotations, workspaceCheckpointRoutingFailureAnnotation)
			}
			checkpoint.ResourceVersion = before.ResourceVersion
			if !reflect.DeepEqual(checkpoint, before) {
				t.Fatal("registration-name handling changed provider status, route, spec or retained-artifact ownership")
			}
		})
	}
}

func unsupportedProviderCheckpointFixture(t *testing.T, mode string) (*externalRuntimePoolFixture, *workspace.ExecutionWorkspaceCheckpoint) {
	t.Helper()
	f := newExternalRuntimePoolFixture(t)
	f.provider.Name = unsupportedWorkspaceProviderName()
	if mode == "disabled" {
		f.provider.Spec.LifecycleState = workspace.ExecutionWorkspaceProviderDisabled
	}
	f.provider.Status.SupportedFeatures = append(f.provider.Status.SupportedFeatures, workspace.WorkspaceFeatureCheckpoint)
	f.workspace.Spec.ProviderBinding.Name = f.provider.Name
	f.workspace.Spec.CoreAdmission.ProviderBinding.Name = f.provider.Name
	c := fake.NewClientBuilder().WithScheme(f.r.Scheme).WithStatusSubresource(f.workspace, f.provider, &workspace.ExecutionWorkspaceCheckpoint{}).
		WithObjects(f.workspace, f.provider).Build()
	f.r.Client, f.r.APIReader = c, c
	checkpoint := &workspace.ExecutionWorkspaceCheckpoint{ObjectMeta: metav1.ObjectMeta{Namespace: f.workspace.Namespace, Name: "export", UID: "checkpoint-uid", Generation: 1,
		Annotations: map[string]string{"example.invalid/note": "preserve"}}, Spec: workspace.ExecutionWorkspaceCheckpointSpec{WorkspaceRef: workspace.ObjectIdentityReference{Name: f.workspace.Name, UID: f.workspace.UID}},
		Status: workspace.ExecutionWorkspaceCheckpointStatus{Phase: "Pending"}}
	switch mode {
	case "routed":
		checkpoint.Labels = map[string]string{workspaceCheckpointProviderNameLabel: "previous-provider"}
	case "retained artifact":
		checkpoint.Status.Digest = "sha256:" + strings.Repeat("a", 64)
		checkpoint.Finalizers = []string{"example.workspace.orka.ai/checkpoint-reference"}
	case "legacy pending":
		checkpoint.Finalizers = []string{"orka.ai/substrate-checkpoint-reference"}
	}
	if err := c.Create(t.Context(), checkpoint); err != nil {
		t.Fatal(err)
	}
	return f, checkpoint
}
