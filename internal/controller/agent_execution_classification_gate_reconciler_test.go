package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func TestPreSealedReconcilersRequeueBeforeAccessingMutableState(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	control := classifiedReadinessControl()
	control.Status.Classification.State = corev1alpha1.AgentExecutionClassificationOpen
	gate := &AgentExecutionClassificationGate{
		APIReader: fake.NewClientBuilder().WithScheme(scheme).WithObjects(control).Build(),
	}

	for _, test := range []struct {
		name      string
		reconcile func(client.Client) func(context.Context, ctrl.Request) (ctrl.Result, error)
	}{
		{
			name: "AgentExecutionAdjudication",
			reconcile: func(kubeClient client.Client) func(context.Context, ctrl.Request) (ctrl.Result, error) {
				return (&AgentExecutionAdjudicationReconciler{
					Client: kubeClient, AgentExecutionClassificationGate: gate,
				}).Reconcile
			},
		},
		{
			name: "Agent",
			reconcile: func(kubeClient client.Client) func(context.Context, ctrl.Request) (ctrl.Result, error) {
				return (&AgentReconciler{
					Client: kubeClient, AgentExecutionClassificationGate: gate,
				}).Reconcile
			},
		},
		{
			name: "AgentRuntime",
			reconcile: func(kubeClient client.Client) func(context.Context, ctrl.Request) (ctrl.Result, error) {
				return (&AgentRuntimeReconciler{
					Client: kubeClient, AgentExecutionClassificationGate: gate,
				}).Reconcile
			},
		},
		{
			name: "ExecutionWorkspace",
			reconcile: func(kubeClient client.Client) func(context.Context, ctrl.Request) (ctrl.Result, error) {
				return (&ExecutionWorkspaceReconciler{
					Client: kubeClient, AgentExecutionClassificationGate: gate,
				}).Reconcile
			},
		},
		{
			name: "RepositoryScan",
			reconcile: func(kubeClient client.Client) func(context.Context, ctrl.Request) (ctrl.Result, error) {
				return (&RepositoryScanReconciler{
					Client: kubeClient, AgentExecutionClassificationGate: gate,
				}).Reconcile
			},
		},
		{
			name: "RepositoryMonitor",
			reconcile: func(kubeClient client.Client) func(context.Context, ctrl.Request) (ctrl.Result, error) {
				return (&RepositoryMonitorReconciler{
					Client: kubeClient, AgentExecutionClassificationGate: gate,
				}).Reconcile
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			mutableStateAccessed := false
			kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
				Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
					mutableStateAccessed = true
					return errors.New("mutable client accessed while classification gate is open")
				},
			}).Build()

			result, err := test.reconcile(kubeClient)(context.Background(), ctrl.Request{})
			if err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			if mutableStateAccessed {
				t.Fatal("Reconcile() accessed mutable state before checking the classification gate")
			}
			if result.RequeueAfter != time.Second {
				t.Fatalf("Reconcile() RequeueAfter = %s, want %s", result.RequeueAfter, time.Second)
			}
		})
	}
}
