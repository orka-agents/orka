// Copyright (c) 2026. MIT License - see LICENSE file for details.

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	workspaceprovider "github.com/orka-agents/orka-workspace/sdk"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestExternalWorkspaceClassRejectsReservedLegacyControllerRoute(t *testing.T) {
	for _, controllerName := range []string{acpWorkspaceControllerLabelValue, "adapter." + acpWorkspaceControllerLabelValue} {
		t.Run(controllerName, func(t *testing.T) {
			ctx := context.Background()
			r, class, provider, config := externalACPFixture(t, true)
			provider.Spec.ControllerName = controllerName
			if err := r.Update(ctx, provider); err != nil {
				t.Fatal(err)
			}
			// Re-pin the fixture's hash to its complete current inputs so the
			// only admission difference is the reserved routing identity.
			profile := &unstructured.Unstructured{}
			profile.SetGroupVersionKind(config.GroupVersionKind().GroupVersion().WithKind("WorkspaceProfile"))
			if err := r.Get(ctx, client.ObjectKey{Namespace: class.Namespace, Name: class.Spec.ParametersRef.Name}, profile); err != nil {
				t.Fatal(err)
			}
			hash, err := externalACPClassProfileHash(class, provider, config, profile)
			if err != nil {
				t.Fatal(err)
			}
			class.Status.ProfileHash = hash
			if err := r.Status().Update(ctx, class); err != nil {
				t.Fatal(err)
			}
			reserved := controllerName == acpWorkspaceControllerLabelValue
			_, err = r.resolveACPWorkspaceClass(ctx, acpClassTestTask())
			if reserved && (err == nil || !strings.Contains(err.Error(), "reserved")) {
				t.Fatalf("Task admitted reserved legacy route: %v", err)
			}
			if !reserved && err != nil {
				t.Fatalf("distinct external route was rejected: %v", err)
			}
			core := &ExecutionWorkspaceClassReconciler{Client: r.Client, APIReader: r.APIReader, RESTMapper: r.RESTMapper()}
			if _, err := core.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(class)}); err != nil {
				t.Fatal(err)
			}
			if err := r.Get(ctx, client.ObjectKeyFromObject(class), class); err != nil {
				t.Fatal(err)
			}
			ready := workspaceprovider.FindCondition(class.Status.Conditions, string(workspacev1alpha1.ConditionClassReady))
			if ready == nil || (ready.Status == metav1.ConditionTrue) == reserved {
				t.Fatalf("class readiness admitted reserved route or rejected distinct route: %#v", ready)
			}
			if reserved && !strings.Contains(ready.Message, "reserved") {
				t.Fatal("reserved route rejection did not explain the registration conflict")
			}
		})
	}
}

// A generic provider without the lifecycle contract must not claim the legacy
// ACP routing identity either: Core treats workspaces and checkpoints carrying
// it as legacy-owned and skips their generic routing.
func TestGenericWorkspaceProviderRejectsReservedLegacyControllerRoute(t *testing.T) {
	for _, controllerName := range []string{acpWorkspaceControllerLabelValue, "generic." + acpWorkspaceControllerLabelValue} {
		t.Run(controllerName, func(t *testing.T) {
			class, provider, w := workspacePolicyReviewFixture(t)
			provider.Spec.ControllerName = controllerName
			provider.Status.LastHeartbeat = &metav1.Time{Time: runtimePoolTestNow}
			refreshWorkspacePolicyReviewProfile(t, class, provider, w)
			mapper, profile := testParameterMapping(class.Namespace, class.Spec.ParametersRef)
			c := fake.NewClientBuilder().WithScheme(testWorkspaceScheme(t)).WithStatusSubresource(class, provider, w).
				WithObjects(class, provider, w, profile).Build()
			reserved := controllerName == acpWorkspaceControllerLabelValue

			workspaceReconciler := &ExecutionWorkspaceReconciler{Client: c, APIReader: c, RESTMapper: mapper}
			for range 4 {
				if _, err := workspaceReconciler.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(w)}); err != nil {
					t.Fatal(err)
				}
			}
			got := &workspacev1alpha1.ExecutionWorkspace{}
			if err := c.Get(t.Context(), client.ObjectKeyFromObject(w), got); err != nil {
				t.Fatal(err)
			}
			if (got.Spec.CoreAdmission != nil) == reserved {
				t.Fatalf("workspace admission with controllerName %q: admitted=%v", controllerName, got.Spec.CoreAdmission != nil)
			}
			if reserved {
				admitted := workspaceprovider.FindCondition(got.Status.Conditions, string(workspacev1alpha1.ConditionWorkspaceAdmitted))
				if admitted == nil || admitted.Status != metav1.ConditionFalse || admitted.Reason != reasonProviderControllerReserved {
					t.Fatalf("reserved route workspace denial = %#v", admitted)
				}
			}

			classReconciler := &ExecutionWorkspaceClassReconciler{Client: c, APIReader: c, RESTMapper: mapper}
			if _, err := classReconciler.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(class)}); err != nil {
				t.Fatal(err)
			}
			if err := c.Get(t.Context(), client.ObjectKeyFromObject(class), class); err != nil {
				t.Fatal(err)
			}
			classReady := workspaceprovider.FindCondition(class.Status.Conditions, string(workspacev1alpha1.ConditionClassReady))
			if classReady == nil || (classReady.Status == metav1.ConditionTrue) == reserved || (reserved && classReady.Reason != reasonProviderControllerReserved) {
				t.Fatalf("class readiness = %#v", classReady)
			}

			providerReconciler := &ExecutionWorkspaceProviderReconciler{Client: c, APIReader: c, RESTMapper: testProviderParameterMapper(apimeta.RESTScopeRoot), Now: func() time.Time { return runtimePoolTestNow }}
			for range 2 {
				if _, err := providerReconciler.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(provider)}); err != nil {
					t.Fatal(err)
				}
			}
			if err := c.Get(t.Context(), client.ObjectKeyFromObject(provider), provider); err != nil {
				t.Fatal(err)
			}
			providerReady := workspaceprovider.FindCondition(provider.Status.Conditions, string(workspacev1alpha1.ConditionProviderReady))
			if providerReady == nil || (providerReady.Status == metav1.ConditionTrue) == reserved || (reserved && providerReady.Reason != reasonProviderControllerReserved) {
				t.Fatalf("provider readiness = %#v", providerReady)
			}
		})
	}
}
