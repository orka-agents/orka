/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func testConnection(namespace, name, provider string) *corev1alpha1.Connection {
	return &corev1alpha1.Connection{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Generation: 1, UID: types.UID(name + "-uid")},
		Spec: corev1alpha1.ConnectionSpec{
			Subject:     corev1alpha1.ConnectionSubject{Issuer: "https://issuer.example.test", Subject: "alice"},
			ProviderRef: corev1alpha1.LocalObjectReference{Name: provider},
			Mode:        corev1alpha1.ConnectionModeReadOnly,
		},
	}
}

func acceptedConnectorProvider() *corev1alpha1.ConnectorProvider {
	provider := testConnectorProvider("tenant", "github")
	provider.Status.ObservedGeneration = provider.Generation
	provider.Status.Conditions = []metav1.Condition{
		{Type: corev1alpha1.ConnectorProviderConditionAccepted, Status: metav1.ConditionTrue, Reason: "Accepted", ObservedGeneration: provider.Generation},
		{Type: corev1alpha1.ConnectorProviderConditionResolvedRefs, Status: metav1.ConditionTrue, Reason: "ResolvedRefs", ObservedGeneration: provider.Generation},
	}
	return provider
}

func TestConnectionReconcilerProviderResolution(t *testing.T) {
	tests := []struct {
		name         string
		objects      []runtime.Object
		existing     *corev1alpha1.ConnectionStatus
		wantStatus   metav1.ConditionStatus
		wantReason   string
		wantState    string
		wantNoStatus bool
	}{
		{
			name:       "provider missing",
			wantStatus: metav1.ConditionFalse,
			wantReason: corev1alpha1.ConnectionReasonProviderMissing,
			wantState:  corev1alpha1.ConnectionStateError,
		},
		{
			name:       "provider not accepted",
			objects:    []runtime.Object{testConnectorProvider("tenant", "github")},
			wantStatus: metav1.ConditionFalse,
			wantReason: corev1alpha1.ConnectionReasonProviderInvalid,
			wantState:  corev1alpha1.ConnectionStateError,
		},
		{
			name: "provider stale generation",
			objects: func() []runtime.Object {
				p := acceptedConnectorProvider()
				p.Generation = 2
				return []runtime.Object{p}
			}(),
			wantStatus: metav1.ConditionFalse,
			wantReason: corev1alpha1.ConnectionReasonProviderInvalid,
			wantState:  corev1alpha1.ConnectionStateError,
		},
		{
			name:       "provider accepted pending consent",
			objects:    []runtime.Object{acceptedConnectorProvider()},
			wantStatus: metav1.ConditionTrue,
			wantReason: corev1alpha1.ConnectionReasonProviderResolved,
			wantState:  corev1alpha1.ConnectionStatePending,
		},
		{
			name:    "provider accepted keeps ready state",
			objects: []runtime.Object{acceptedConnectorProvider()},
			existing: &corev1alpha1.ConnectionStatus{
				State:      corev1alpha1.ConnectionStateReady,
				Conditions: []metav1.Condition{{Type: corev1alpha1.ConnectionConditionReady, Status: metav1.ConditionTrue, Reason: "Linked"}},
			},
			wantStatus: metav1.ConditionTrue,
			wantReason: corev1alpha1.ConnectionReasonProviderResolved,
			wantState:  corev1alpha1.ConnectionStateReady,
		},
		{
			name:    "provider accepted keeps revoked state",
			objects: []runtime.Object{acceptedConnectorProvider()},
			existing: &corev1alpha1.ConnectionStatus{
				State:      corev1alpha1.ConnectionStateRevoked,
				Conditions: []metav1.Condition{{Type: corev1alpha1.ConnectionConditionReady, Status: metav1.ConditionFalse, Reason: "Revoked"}},
			},
			wantStatus: metav1.ConditionTrue,
			wantState:  corev1alpha1.ConnectionStateRevoked,
		},
		{
			name:    "ready condition true without recorded state",
			objects: []runtime.Object{acceptedConnectorProvider()},
			existing: &corev1alpha1.ConnectionStatus{
				Conditions: []metav1.Condition{{Type: corev1alpha1.ConnectionConditionReady, Status: metav1.ConditionTrue, Reason: "Linked"}},
			},
			wantStatus: metav1.ConditionTrue,
			wantState:  corev1alpha1.ConnectionStateReady,
		},
		{
			name:    "ready condition false without recorded state",
			objects: []runtime.Object{acceptedConnectorProvider()},
			existing: &corev1alpha1.ConnectionStatus{
				Conditions: []metav1.Condition{{Type: corev1alpha1.ConnectionConditionReady, Status: metav1.ConditionFalse, Reason: "PendingConsent"}},
			},
			wantStatus: metav1.ConditionTrue,
			wantState:  corev1alpha1.ConnectionStatePending,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := connectorTestScheme(t)
			connection := testConnection("tenant", "github-alice", "github")
			if tt.existing != nil {
				connection.Status = *tt.existing
			}
			objects := append([]runtime.Object{connection}, tt.objects...)
			c := ctrlfake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objects...).WithStatusSubresource(&corev1alpha1.Connection{}).Build()
			reconciler := &ConnectionReconciler{Client: c, Scheme: scheme}
			key := types.NamespacedName{Namespace: "tenant", Name: "github-alice"}
			result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
			if err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			if result.RequeueAfter != connectionRefreshInterval {
				t.Fatalf("RequeueAfter = %v", result.RequeueAfter)
			}
			updated := &corev1alpha1.Connection{}
			if err := c.Get(context.Background(), key, updated); err != nil {
				t.Fatal(err)
			}
			if updated.Status.ObservedGeneration != updated.Generation {
				t.Fatalf("observedGeneration = %d", updated.Status.ObservedGeneration)
			}
			resolved := meta.FindStatusCondition(updated.Status.Conditions, corev1alpha1.ConnectionConditionProviderResolved)
			if resolved == nil || resolved.Status != tt.wantStatus {
				t.Fatalf("ProviderResolved = %#v, want %s", resolved, tt.wantStatus)
			}
			if tt.wantReason != "" && resolved.Reason != tt.wantReason {
				t.Fatalf("reason = %q, want %q", resolved.Reason, tt.wantReason)
			}
			if updated.Status.State != tt.wantState {
				t.Fatalf("state = %q, want %q", updated.Status.State, tt.wantState)
			}
			if tt.existing != nil {
				if ready := meta.FindStatusCondition(updated.Status.Conditions, corev1alpha1.ConnectionConditionReady); ready == nil {
					t.Fatal("reconciler must preserve the Ready condition owned by the consent path")
				}
			}
		})
	}
}

func TestConnectionReconcilerSteadyStateAndErrors(t *testing.T) {
	scheme := connectorTestScheme(t)
	connection := testConnection("tenant", "github-alice", "github")
	provider := acceptedConnectorProvider()
	c := ctrlfake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(connection, provider).WithStatusSubresource(&corev1alpha1.Connection{}).Build()
	reconciler := &ConnectionReconciler{Client: c, Scheme: scheme}
	key := types.NamespacedName{Namespace: "tenant", Name: "github-alice"}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	settled := &corev1alpha1.Connection{}
	if err := c.Get(context.Background(), key, settled); err != nil {
		t.Fatal(err)
	}
	updates := 0
	counting := ctrlfake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(settled, provider).WithStatusSubresource(&corev1alpha1.Connection{}).
		WithInterceptorFuncs(interceptor.Funcs{SubResourceUpdate: func(ctx context.Context, cl ctrlclient.Client, sub string, obj ctrlclient.Object, opts ...ctrlclient.SubResourceUpdateOption) error {
			updates++
			return cl.SubResource(sub).Update(ctx, obj, opts...)
		}}).Build()
	reconciler = &ConnectionReconciler{Client: counting, Scheme: scheme}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if updates != 0 {
		t.Fatalf("steady-state reconcile wrote status %d times", updates)
	}

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "tenant", Name: "missing"}}); err != nil {
		t.Fatalf("missing connection must reconcile cleanly: %v", err)
	}

	deleting := testConnection("tenant", "gone", "github")
	now := metav1.Now()
	deleting.DeletionTimestamp = &now
	deleting.Finalizers = []string{"test.orka.ai/hold"}
	c = ctrlfake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(deleting).WithStatusSubresource(&corev1alpha1.Connection{}).Build()
	reconciler = &ConnectionReconciler{Client: c, Scheme: scheme}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "tenant", Name: "gone"}}); err != nil {
		t.Fatal(err)
	}
	updated := &corev1alpha1.Connection{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "tenant", Name: "gone"}, updated); err != nil {
		t.Fatal(err)
	}
	if len(updated.Status.Conditions) != 0 {
		t.Fatalf("deleting connection must not gain conditions: %#v", updated.Status.Conditions)
	}

	failing := ctrlfake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(testConnection("tenant", "github-alice", "github")).WithStatusSubresource(&corev1alpha1.Connection{}).
		WithInterceptorFuncs(interceptor.Funcs{Get: func(ctx context.Context, cl ctrlclient.WithWatch, key ctrlclient.ObjectKey, obj ctrlclient.Object, opts ...ctrlclient.GetOption) error {
			if _, isProvider := obj.(*corev1alpha1.ConnectorProvider); isProvider {
				return errTestProviderRead
			}
			return cl.Get(ctx, key, obj, opts...)
		}}).Build()
	reconciler = &ConnectionReconciler{Client: failing, Scheme: scheme}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err == nil {
		t.Fatal("expected provider read error to surface")
	}
	if err := failing.Get(context.Background(), key, updated); err != nil {
		t.Fatal(err)
	}
	resolved := meta.FindStatusCondition(updated.Status.Conditions, corev1alpha1.ConnectionConditionProviderResolved)
	if resolved == nil || resolved.Status != metav1.ConditionUnknown {
		t.Fatalf("ProviderResolved = %#v, want Unknown", resolved)
	}
}

var errTestProviderRead = &testReadError{}

type testReadError struct{}

func (*testReadError) Error() string { return "provider read failed" }

func TestConnectionRequestsForProvider(t *testing.T) {
	scheme := connectorTestScheme(t)
	c := ctrlfake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(
		testConnection("tenant", "alice", "github"),
		testConnection("tenant", "bob", "github"),
		testConnection("tenant", "carol", "gmail"),
		testConnection("elsewhere", "dave", "github"),
	).Build()
	reconciler := &ConnectionReconciler{Client: c, Scheme: scheme}
	requests := reconciler.requestsForProvider(context.Background(), testConnectorProvider("tenant", "github"))
	if len(requests) != 2 {
		t.Fatalf("requests = %#v, want alice and bob", requests)
	}
	for _, request := range requests {
		if request.Namespace != "tenant" || (request.Name != "alice" && request.Name != "bob") {
			t.Fatalf("unexpected request %#v", request)
		}
	}
}
