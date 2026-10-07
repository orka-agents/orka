// Copyright (c) 2026. MIT License - see LICENSE file for details.

package controller

import (
	"context"
	"strings"
	"testing"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	workspaceprovider "github.com/orka-agents/orka-workspace/sdk"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
