package controller

import (
	"context"
	"errors"
	"reflect"
	"testing"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func providerAuthorizationOutage(c client.WithWatch, evaluationFailure bool) client.WithWatch {
	return interceptor.NewClient(c, interceptor.Funcs{Create: func(ctx context.Context, next client.WithWatch, object client.Object, options ...client.CreateOption) error {
		if review, ok := object.(*authorizationv1.SubjectAccessReview); ok {
			if evaluationFailure {
				review.Status.EvaluationError = "authorization service unavailable"
				return nil
			}
			return context.DeadlineExceeded
		}
		return next.Create(ctx, object, options...)
	}})
}

func TestExternalACPProviderAuthorizationOutagesRetryTaskPlanning(t *testing.T) {
	for _, evaluationFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "request timeout", true: "evaluation failure"}[evaluationFailure], func(t *testing.T) {
			r, _, _, _ := externalACPFixture(t, true)
			r.Client = providerAuthorizationOutage(r.Client.(client.WithWatch), evaluationFailure)
			task := acpClassTestTask()
			plan, rejected := r.rejectUnsupportedACPWorkspacePlan(t.Context(), task)
			if !rejected || !isRetryableACPWorkspaceClassResolutionError(plan.transientError) || plan.rejectionReason != "" || plan.workspaceStatusError != nil {
				t.Fatalf("authorization outage became a permanent plan rejection: %#v", plan)
			}
			before := task.DeepCopy()
			if _, err := r.rejectPlannedAgentExecution(t.Context(), task, plan); !errors.Is(err, plan.transientError) {
				t.Fatalf("planning error = %v, want retryable authorization failure", err)
			}
			if !reflect.DeepEqual(task, before) {
				t.Fatal("retryable authorization failure changed Task status")
			}
		})
	}
}

func TestExternalACPProviderAuthorizationOutagesPreserveClassReadiness(t *testing.T) {
	for _, evaluationFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "request timeout", true: "evaluation failure"}[evaluationFailure], func(t *testing.T) {
			r, class, _, _ := externalACPFixture(t, true)
			broken := providerAuthorizationOutage(r.Client.(client.WithWatch), evaluationFailure)
			reconciler := &ExecutionWorkspaceClassReconciler{Client: broken, APIReader: r.APIReader, RESTMapper: r.RESTMapper()}
			if _, err := reconciler.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(class)}); !isRetryableACPWorkspaceClassResolutionError(err) {
				t.Fatalf("class authorization outage = %v, want retryable error", err)
			}
			current := &workspacev1alpha1.ExecutionWorkspaceClass{}
			if err := r.Get(t.Context(), client.ObjectKeyFromObject(class), current); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(current.Status, class.Status) {
				t.Fatal("authorization outage withdrew the class's established readiness")
			}
		})
	}
}

func TestExternalACPProviderAuthorizationOutagesDoNotQuarantineWorkspace(t *testing.T) {
	for _, evaluationFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "request timeout", true: "evaluation failure"}[evaluationFailure], func(t *testing.T) {
			r, class, provider, config := externalACPFixture(t, true)
			r.Scheme = r.Client.Scheme()
			task := acpClassTestTask()
			resolved, err := r.resolveACPWorkspaceClass(t.Context(), task)
			if err != nil {
				t.Fatal(err)
			}
			binding, err := resolveACPWorkspaceBindingWithClass(task, "", resolved)
			if err != nil {
				t.Fatal(err)
			}
			workspace, err := r.createACPClassWorkspace(t.Context(), task, binding, "pool", acpClassWorkspaceName(task, binding))
			if err != nil {
				t.Fatal(err)
			}
			workspace.UID = "authorization-workspace-uid"
			workspace.Generation = 1
			workspace.CreationTimestamp = metav1.Now()
			workspace.Finalizers = []string{executionWorkspaceFinalizer}
			profile := &unstructured.Unstructured{}
			profile.SetGroupVersionKind(schema.GroupVersionKind{Group: "example.workspace.orka.ai", Version: "v1alpha1", Kind: "WorkspaceProfile"})
			if err := r.Get(t.Context(), types.NamespacedName{Namespace: class.Namespace, Name: "profile"}, profile); err != nil {
				t.Fatal(err)
			}
			base := fake.NewClientBuilder().WithScheme(r.Client.Scheme()).WithRESTMapper(r.Client.RESTMapper()).
				WithStatusSubresource(&workspacev1alpha1.ExecutionWorkspace{}, class, provider).
				WithObjects(class, provider, config, profile, workspace).Build()
			broken := providerAuthorizationOutage(base, evaluationFailure)
			reconciler := &ExecutionWorkspaceReconciler{Client: broken, APIReader: base, RESTMapper: base.RESTMapper()}
			if _, err := reconciler.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(workspace)}); !isRetryableACPWorkspaceClassResolutionError(err) {
				t.Fatalf("workspace authorization outage = %v, want retryable error", err)
			}
			current := &workspacev1alpha1.ExecutionWorkspace{}
			if err := base.Get(t.Context(), client.ObjectKeyFromObject(workspace), current); err != nil {
				t.Fatal(err)
			}
			if current.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredReady || current.Labels[workspacev1alpha1.QuarantinedLabel] != "" || !reflect.DeepEqual(current.Status, workspace.Status) {
				t.Fatal("temporary authorization failure quarantined or denied the workspace")
			}
		})
	}
}

func TestExternalACPProviderAuthorizationDenialRemainsPermanent(t *testing.T) {
	r, class, _, _ := externalACPFixture(t, false)
	plan, rejected := r.rejectUnsupportedACPWorkspacePlan(t.Context(), acpClassTestTask())
	if !rejected || plan.transientError != nil || plan.rejectionReason == "" || plan.workspaceStatusError == nil {
		t.Fatalf("authorization denial was not permanent: %#v", plan)
	}
	classReconciler := &ExecutionWorkspaceClassReconciler{Client: r.Client, APIReader: r.APIReader, RESTMapper: r.RESTMapper()}
	_, reason, _, err := classReconciler.resolveClassProvider(t.Context(), class)
	if err != nil || reason != string(workspacev1alpha1.ReasonAuthorizationDenied) {
		t.Fatalf("class authorization denial = %q, %v", reason, err)
	}
	if plan.path != agentExecutionPathRejected {
		t.Fatal("denied provider remained eligible for execution")
	}
}
