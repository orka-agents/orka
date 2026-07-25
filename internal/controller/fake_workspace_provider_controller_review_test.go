package controller

import (
	"context"
	"testing"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fakeworkspacev1alpha1 "github.com/orka-agents/orka/api/fake.workspace/v1alpha1"
	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
	"github.com/orka-agents/orka/pkg/workspaceprovider"
)

func TestFakeExecutionWorkspaceProviderRequiresAvailableConfig(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	deletingAt := metav1.NewTime(now.Add(-time.Minute))
	deletingConfig := fakeProviderConfigForReviewTest("default")
	deletingConfig.SetDeletionTimestamp(&deletingAt)
	deletingConfig.SetFinalizers([]string{"test.orka.ai/protect"})

	tests := []struct {
		name          string
		config        client.Object
		wantPublished bool
	}{
		{name: "missing"},
		{name: "deleting", config: deletingConfig},
		{name: "valid", config: fakeProviderConfigForReviewTest("default"), wantPublished: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			provider := fakeProviderForReviewTest("fake-provider")
			if !tt.wantPublished {
				staleHeartbeat := metav1.NewTime(now.Add(-time.Second))
				provider.Status = workspacev1alpha1.ExecutionWorkspaceProviderStatus{
					ObservedGeneration: provider.Generation,
					Adapter:            &workspacev1alpha1.ExecutionWorkspaceAdapterStatus{Version: fakeWorkspaceAdapterVersion},
					Backend:            &workspacev1alpha1.ExecutionWorkspaceBackendStatus{Version: "in-memory"},
					SupportedContracts: []string{workspacev1alpha1.ContractVersionV1},
					SupportedFeatures:  []workspacev1alpha1.ExecutionWorkspaceFeature{workspacev1alpha1.WorkspaceFeatureExec},
					LastHeartbeat:      &staleHeartbeat,
					Conditions: []metav1.Condition{{
						Type:   string(workspacev1alpha1.ConditionProviderCompatible),
						Status: metav1.ConditionTrue,
						Reason: string(workspacev1alpha1.ReasonReady),
					}},
				}
			}
			objects := []client.Object{provider}
			if tt.config != nil {
				objects = append(objects, tt.config.DeepCopyObject().(client.Object))
			}
			c := fake.NewClientBuilder().
				WithScheme(testWorkspaceScheme(t)).
				WithStatusSubresource(provider).
				WithObjects(objects...).
				Build()
			reconciler := &FakeExecutionWorkspaceProviderReconciler{
				Client: c,
				Now:    func() time.Time { return now },
			}

			result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Name: provider.Name},
			})
			if err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			if result.RequeueAfter != fakeProviderHeartbeatPeriod {
				t.Fatalf("Reconcile() RequeueAfter = %s, want %s", result.RequeueAfter, fakeProviderHeartbeatPeriod)
			}

			got := &workspacev1alpha1.ExecutionWorkspaceProvider{}
			if err := c.Get(context.Background(), types.NamespacedName{Name: provider.Name}, got); err != nil {
				t.Fatalf("get provider: %v", err)
			}
			if !tt.wantPublished {
				if got.Status.ObservedGeneration != 0 {
					t.Fatalf("ObservedGeneration = %d, want 0", got.Status.ObservedGeneration)
				}
				if got.Status.Adapter != nil || got.Status.Backend != nil || len(got.Status.SupportedContracts) != 0 || len(got.Status.SupportedFeatures) != 0 {
					t.Fatalf("adapter advertisement = %#v, want cleared", got.Status)
				}
				if got.Status.LastHeartbeat != nil {
					t.Fatalf("LastHeartbeat = %v, want nil", got.Status.LastHeartbeat)
				}
				if workspaceprovider.FindCondition(got.Status.Conditions, string(workspacev1alpha1.ConditionProviderCompatible)) != nil {
					t.Fatalf("Compatible condition = %#v, want absent", got.Status.Conditions)
				}
				if workspaceprovider.FindCondition(got.Status.Conditions, string(workspacev1alpha1.ConditionProviderReady)) != nil {
					t.Fatalf("Ready condition = %#v, want absent", got.Status.Conditions)
				}
			} else {
				if got.Status.ObservedGeneration != provider.Generation {
					t.Fatalf("ObservedGeneration = %d, want %d", got.Status.ObservedGeneration, provider.Generation)
				}
				if got.Status.LastHeartbeat == nil || !got.Status.LastHeartbeat.Time.Equal(now) {
					t.Fatalf("LastHeartbeat = %v, want %s", got.Status.LastHeartbeat, now)
				}
				compatible := workspaceprovider.FindCondition(got.Status.Conditions, string(workspacev1alpha1.ConditionProviderCompatible))
				if compatible == nil || compatible.Status != metav1.ConditionTrue {
					t.Fatalf("Compatible condition = %#v, want True", compatible)
				}
			}

			coreReconciler := &ExecutionWorkspaceProviderReconciler{
				Client:     c,
				RESTMapper: testProviderParameterMapper(apimeta.RESTScopeRoot),
				Now:        func() time.Time { return now },
			}
			if _, err := coreReconciler.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Name: provider.Name},
			}); err != nil {
				t.Fatalf("core Reconcile() error = %v", err)
			}
			if _, err := coreReconciler.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Name: provider.Name},
			}); err != nil {
				t.Fatalf("second core Reconcile() error = %v", err)
			}
			if err := c.Get(context.Background(), types.NamespacedName{Name: provider.Name}, got); err != nil {
				t.Fatalf("get provider after core reconcile: %v", err)
			}
			ready := workspaceprovider.FindCondition(got.Status.Conditions, string(workspacev1alpha1.ConditionProviderReady))
			if ready == nil {
				t.Fatal("Ready condition is absent after core reconciliation")
			}
			wantReady := metav1.ConditionFalse
			if tt.wantPublished {
				wantReady = metav1.ConditionTrue
			}
			if ready.Status != wantReady {
				t.Fatalf("Ready status = %s, want %s: %#v", ready.Status, wantReady, ready)
			}
		})
	}
}

func TestFakeExecutionWorkspaceServiceEndpointSchemes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		protocol string
		port     int32
		wantURL  string
	}{
		{protocol: "HTTP", port: 8080, wantURL: "http://workspace.default.svc:8080"},
		{protocol: "HTTPS", port: 8443, wantURL: "https://workspace.default.svc:8443"},
		{protocol: "TCP", port: 9000, wantURL: "tcp://workspace.default.svc:9000"},
	}

	for _, tt := range tests {
		t.Run(tt.protocol, func(t *testing.T) {
			t.Parallel()

			provider := fakeProviderForReviewTest("fake-provider")
			workspace := &workspacev1alpha1.ExecutionWorkspace{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "workspace",
					Namespace:  "default",
					UID:        types.UID("workspace-uid"),
					Generation: 1,
				},
				Spec: workspacev1alpha1.ExecutionWorkspaceSpec{
					Mode: workspacev1alpha1.ExecutionWorkspaceModeService,
					ProviderBinding: workspacev1alpha1.ImmutableObjectBinding{
						Name:       provider.Name,
						UID:        provider.UID,
						Generation: provider.Generation,
					},
					DesiredState: workspacev1alpha1.ExecutionWorkspaceDesiredReady,
					Service: &workspacev1alpha1.ExecutionWorkspaceServiceSpec{Ports: []workspacev1alpha1.ExecutionWorkspaceServicePort{
						{Name: "service", Port: tt.port, Protocol: tt.protocol},
					}},
				},
			}
			c := fake.NewClientBuilder().
				WithScheme(testWorkspaceScheme(t)).
				WithStatusSubresource(workspace).
				WithObjects(provider, workspace).
				Build()
			reconciler := &FakeExecutionWorkspaceReconciler{Client: c}

			if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name},
			}); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}

			got := &workspacev1alpha1.ExecutionWorkspace{}
			if err := c.Get(context.Background(), types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}, got); err != nil {
				t.Fatalf("get workspace: %v", err)
			}
			if len(got.Status.Endpoints) != 1 {
				t.Fatalf("Endpoints = %#v, want one endpoint", got.Status.Endpoints)
			}
			endpoint := got.Status.Endpoints[0]
			if endpoint.URL != tt.wantURL || endpoint.Protocol != tt.protocol {
				t.Fatalf("endpoint = %#v, want URL %q and protocol %q", endpoint, tt.wantURL, tt.protocol)
			}
		})
	}
}

func fakeProviderForReviewTest(name string) *workspacev1alpha1.ExecutionWorkspaceProvider {
	return &workspacev1alpha1.ExecutionWorkspaceProvider{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			UID:        types.UID(name + "-uid"),
			Generation: 1,
		},
		Spec: workspacev1alpha1.ExecutionWorkspaceProviderSpec{
			ControllerName: FakeWorkspaceControllerName,
			ParametersRef: workspacev1alpha1.TypedObjectReference{
				Group: fakeworkspacev1alpha1.GroupVersion.Group,
				Kind:  fakeProviderConfigKind,
				Name:  "default",
			},
			LifecycleState:    workspacev1alpha1.ExecutionWorkspaceProviderActive,
			RequiredContracts: []string{workspacev1alpha1.ContractVersionV1},
		},
	}
}

func fakeProviderConfigForReviewTest(name string) *unstructured.Unstructured {
	config := &unstructured.Unstructured{}
	config.SetGroupVersionKind(fakeworkspacev1alpha1.GroupVersion.WithKind(fakeProviderConfigKind))
	config.SetName(name)
	config.SetUID(types.UID(name + "-uid"))
	return config
}
