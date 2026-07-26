package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
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

			provider := fakeProviderForReviewTest()
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

func TestFakeExecutionWorkspaceWaitsForCurrentCoreAdmission(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name             string
		generation       int64
		coreAdmission    bool
		admissionStatus  metav1.ConditionStatus
		admittedAt       int64
		desiredState     workspacev1alpha1.ExecutionWorkspaceDesiredState
		wantState        workspacev1alpha1.ExecutionWorkspaceState
		wantRequeueAfter time.Duration
	}{
		{
			name:             "missing admission",
			generation:       1,
			desiredState:     workspacev1alpha1.ExecutionWorkspaceDesiredReady,
			wantRequeueAfter: workspaceRequeueInterval,
		},
		{
			name:             "adapter forged current condition without core marker",
			generation:       1,
			admissionStatus:  metav1.ConditionTrue,
			admittedAt:       1,
			desiredState:     workspacev1alpha1.ExecutionWorkspaceDesiredReady,
			wantRequeueAfter: workspaceRequeueInterval,
		},
		{
			name:             "stale admission after spec generation change",
			coreAdmission:    true,
			generation:       2,
			admissionStatus:  metav1.ConditionTrue,
			admittedAt:       1,
			desiredState:     workspacev1alpha1.ExecutionWorkspaceDesiredReady,
			wantRequeueAfter: workspaceRequeueInterval,
		},
		{
			name:             "denied display condition pauses current progression",
			coreAdmission:    true,
			generation:       1,
			admissionStatus:  metav1.ConditionFalse,
			admittedAt:       1,
			desiredState:     workspacev1alpha1.ExecutionWorkspaceDesiredReady,
			wantRequeueAfter: workspaceRequeueInterval,
		},
		{
			name:            "current admission",
			coreAdmission:   true,
			generation:      1,
			admissionStatus: metav1.ConditionTrue,
			admittedAt:      1,
			desiredState:    workspacev1alpha1.ExecutionWorkspaceDesiredReady,
			wantState:       workspacev1alpha1.ExecutionWorkspaceStateReady,
		},
		{
			name:         "quarantine maintenance bypasses admission",
			generation:   1,
			desiredState: workspacev1alpha1.ExecutionWorkspaceDesiredQuarantined,
			wantState:    workspacev1alpha1.ExecutionWorkspaceStateQuarantined,
		},
		{
			name:         "delete maintenance bypasses admission",
			generation:   1,
			desiredState: workspacev1alpha1.ExecutionWorkspaceDesiredDeleted,
			wantState:    workspacev1alpha1.ExecutionWorkspaceStateDeleted,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			provider := fakeProviderForReviewTest()
			class := testGenericClass("default", "class", provider.Name)
			workspace := testBoundWorkspace(t, "default", "workspace", class.Name, provider.Name)
			workspace.Generation = tt.generation
			workspace.Spec.DesiredState = tt.desiredState
			if tt.coreAdmission {
				workspace.Spec.CoreAdmission = &workspacev1alpha1.ExecutionWorkspaceCoreAdmission{
					ClassBinding:       workspace.Spec.ClassBinding,
					ProviderBinding:    workspace.Spec.ProviderBinding,
					AdmittedGeneration: tt.admittedAt,
				}
			}
			if tt.admittedAt > 0 {
				workspace.Status.Conditions = []metav1.Condition{{
					Type:               string(workspacev1alpha1.ConditionWorkspaceAdmitted),
					Status:             tt.admissionStatus,
					Reason:             string(workspacev1alpha1.ReasonReady),
					ObservedGeneration: tt.admittedAt,
				}}
			}
			c := fake.NewClientBuilder().
				WithScheme(testWorkspaceScheme(t)).
				WithStatusSubresource(workspace).
				WithObjects(provider, class, workspace).
				Build()
			reconciler := &FakeExecutionWorkspaceReconciler{Client: c}
			request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}}
			result, err := reconciler.Reconcile(context.Background(), request)
			if err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if result.RequeueAfter != tt.wantRequeueAfter {
				t.Fatalf("RequeueAfter = %s, want %s", result.RequeueAfter, tt.wantRequeueAfter)
			}
			got := &workspacev1alpha1.ExecutionWorkspace{}
			if err := c.Get(context.Background(), request.NamespacedName, got); err != nil {
				t.Fatalf("get workspace: %v", err)
			}
			if got.Status.State != tt.wantState {
				t.Fatalf("state = %q, want %q; status=%#v", got.Status.State, tt.wantState, got.Status)
			}
			if tt.wantState == "" && (got.Status.ExternalID != "" || got.Status.ProviderBinding != nil) {
				t.Fatalf("adapter status was published before current core admission: %#v", got.Status)
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

			provider := fakeProviderForReviewTest()
			class := testGenericClass("default", "service-class", provider.Name)
			workspace := &workspacev1alpha1.ExecutionWorkspace{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "workspace",
					Namespace:  "default",
					UID:        types.UID("workspace-uid"),
					Generation: 1,
				},
				Spec: workspacev1alpha1.ExecutionWorkspaceSpec{
					Mode: workspacev1alpha1.ExecutionWorkspaceModeService,
					ClassBinding: workspacev1alpha1.ImmutableObjectBinding{
						Name: class.Name, UID: class.UID, Generation: class.Generation,
					},
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
			markWorkspaceAdmittedForPolicyReview(workspace, workspace.Generation)
			c := fake.NewClientBuilder().
				WithScheme(testWorkspaceScheme(t)).
				WithStatusSubresource(workspace).
				WithObjects(provider, class, workspace).
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

func fakeProviderForReviewTest() *workspacev1alpha1.ExecutionWorkspaceProvider {
	const name = "fake-provider"

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

func TestFakeExecutionWorkspacePoolCapacitySelectsOneDeterministicWinner(t *testing.T) {
	t.Parallel()
	provider := fakeProviderForReviewTest()
	pool := testGenericPool("default", "pool", provider.Name)
	pool.Spec.Capacity.MinReady = 0
	pool.Spec.Capacity.MaxSize = 1
	class := testGenericClass(pool.Namespace, "class", provider.Name)
	class.Spec.ProviderRef = nil
	class.Spec.ParametersRef = nil
	class.Spec.PoolRef = &corev1.LocalObjectReference{Name: pool.Name}

	first := testBoundWorkspace(t, pool.Namespace, "workspace-a", class.Name, provider.Name)
	second := testBoundWorkspace(t, pool.Namespace, "workspace-b", class.Name, provider.Name)
	mapper, parameters := preparePooledClassProfileForTest(t, provider, pool, class, first, second)
	markWorkspaceAdmittedForPolicyReview(first, first.Generation)
	first.Spec.CoreAdmission.PoolBinding = &workspacev1alpha1.ImmutableObjectBinding{
		Name: pool.Name, UID: pool.UID, Generation: pool.Generation,
	}
	markWorkspaceAdmittedForPolicyReview(second, second.Generation)
	second.Spec.CoreAdmission.PoolBinding = &workspacev1alpha1.ImmutableObjectBinding{
		Name: pool.Name, UID: pool.UID, Generation: pool.Generation,
	}

	c := fake.NewClientBuilder().
		WithScheme(testWorkspaceScheme(t)).
		WithStatusSubresource(first, second).
		WithObjects(provider, pool, class, first, second, parameters).
		Build()
	reconciler := &FakeExecutionWorkspaceReconciler{Client: c, APIReader: c, RESTMapper: mapper}

	secondRequest := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: second.Namespace, Name: second.Name}}
	if result, err := reconciler.Reconcile(context.Background(), secondRequest); err != nil {
		t.Fatalf("reconcile second candidate: %v", err)
	} else if result.RequeueAfter != workspaceRequeueInterval {
		t.Fatalf("second candidate RequeueAfter = %s, want %s", result.RequeueAfter, workspaceRequeueInterval)
	}
	firstRequest := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: first.Namespace, Name: first.Name}}
	if _, err := reconciler.Reconcile(context.Background(), firstRequest); err != nil {
		t.Fatalf("reconcile first candidate: %v", err)
	}
	if _, err := reconciler.Reconcile(context.Background(), secondRequest); err != nil {
		t.Fatalf("reconcile second candidate after allocation: %v", err)
	}

	gotFirst := &workspacev1alpha1.ExecutionWorkspace{}
	if err := c.Get(context.Background(), firstRequest.NamespacedName, gotFirst); err != nil {
		t.Fatalf("get first candidate: %v", err)
	}
	gotSecond := &workspacev1alpha1.ExecutionWorkspace{}
	if err := c.Get(context.Background(), secondRequest.NamespacedName, gotSecond); err != nil {
		t.Fatalf("get second candidate: %v", err)
	}
	if gotFirst.Status.State != workspacev1alpha1.ExecutionWorkspaceStateReady {
		t.Fatalf("first candidate state = %q, want Ready", gotFirst.Status.State)
	}
	if gotSecond.Status.State != "" || gotSecond.Status.ExternalID != "" {
		t.Fatalf("second candidate exceeded maxSize: %#v", gotSecond.Status)
	}
}

func TestFakeExecutionWorkspaceGrandfathersCoreAdmittedPoolBindingBeforeFirstStatus(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		replacePool bool
	}{
		{name: "deleted pool"},
		{name: "replaced pool", replacePool: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			provider := fakeProviderForReviewTest()
			boundPool := testGenericPool("default", "pool", provider.Name)
			class := testGenericClass(boundPool.Namespace, "class", provider.Name)
			class.Spec.PoolRef = &corev1.LocalObjectReference{Name: boundPool.Name}
			workspace := testBoundWorkspace(t, boundPool.Namespace, "workspace", class.Name, provider.Name)
			markWorkspaceAdmittedForPolicyReview(workspace, workspace.Generation)
			workspace.Spec.CoreAdmission.PoolBinding = &workspacev1alpha1.ImmutableObjectBinding{
				Name: boundPool.Name, UID: boundPool.UID, Generation: boundPool.Generation,
			}

			objects := []client.Object{provider, class, workspace}
			if tt.replacePool {
				replacement := boundPool.DeepCopy()
				replacement.UID = types.UID("replacement-pool-uid")
				replacement.Generation++
				objects = append(objects, replacement)
			}
			c := fake.NewClientBuilder().
				WithScheme(testWorkspaceScheme(t)).
				WithStatusSubresource(workspace).
				WithObjects(objects...).
				Build()
			reconciler := &FakeExecutionWorkspaceReconciler{Client: c, APIReader: c}
			request := ctrl.Request{NamespacedName: types.NamespacedName{
				Namespace: workspace.Namespace,
				Name:      workspace.Name,
			}}

			result, err := reconciler.Reconcile(context.Background(), request)
			if err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			if result.RequeueAfter != 0 {
				t.Fatalf("Reconcile() RequeueAfter = %s, want 0", result.RequeueAfter)
			}
			got := &workspacev1alpha1.ExecutionWorkspace{}
			if err := c.Get(context.Background(), request.NamespacedName, got); err != nil {
				t.Fatalf("get workspace: %v", err)
			}
			if got.Status.State != workspacev1alpha1.ExecutionWorkspaceStateReady || got.Status.ExternalID == "" {
				t.Fatalf("workspace remained stranded before first provider status: %#v", got.Status)
			}
		})
	}
}

func TestFakeExecutionWorkspacePoolGrandfatheringRequiresCurrentCoreAdmission(t *testing.T) {
	t.Parallel()
	provider := fakeProviderForReviewTest()
	boundPool := testGenericPool("default", "pool", provider.Name)
	class := testGenericClass(boundPool.Namespace, "class", provider.Name)
	class.Spec.PoolRef = &corev1.LocalObjectReference{Name: boundPool.Name}
	workspace := testBoundWorkspace(t, boundPool.Namespace, "workspace", class.Name, provider.Name)
	markWorkspaceAdmittedForPolicyReview(workspace, workspace.Generation)
	workspace.Spec.CoreAdmission.PoolBinding = &workspacev1alpha1.ImmutableObjectBinding{
		Name: boundPool.Name, UID: boundPool.UID, Generation: boundPool.Generation,
	}
	workspace.Status.ObservedGeneration = 0
	workspace.Status.Conditions = nil

	c := fake.NewClientBuilder().
		WithScheme(testWorkspaceScheme(t)).
		WithStatusSubresource(workspace).
		WithObjects(provider, class, workspace).
		Build()
	reconciler := &FakeExecutionWorkspaceReconciler{Client: c, APIReader: c}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}}

	result, err := reconciler.Reconcile(context.Background(), request)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter != workspaceRequeueInterval {
		t.Fatalf("Reconcile() RequeueAfter = %s, want %s", result.RequeueAfter, workspaceRequeueInterval)
	}
	got := &workspacev1alpha1.ExecutionWorkspace{}
	if err := c.Get(context.Background(), request.NamespacedName, got); err != nil {
		t.Fatalf("get workspace: %v", err)
	}
	if got.Status.State != "" || got.Status.ExternalID != "" || got.Status.ProviderBinding != nil {
		t.Fatalf("adapter status was published without current core admission: %#v", got.Status)
	}
}
