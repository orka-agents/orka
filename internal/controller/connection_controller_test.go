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
	"github.com/orka-agents/orka/internal/connectors"
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

// scopedConnectorProvider is an accepted provider whose read and write scopes
// differ, so mode changes have observable scope requirements.
func scopedConnectorProvider() *corev1alpha1.ConnectorProvider {
	provider := acceptedConnectorProvider()
	provider.Spec.OAuth.Scopes = corev1alpha1.ConnectorScopes{Read: []string{"read:user"}, Write: []string{"repo"}}
	return provider
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
		name        string
		objects     []runtime.Object
		existing    *corev1alpha1.ConnectionStatus
		mode        string
		generation  int64
		wantStatus  metav1.ConditionStatus
		wantReason  string
		wantState   string
		wantReady   metav1.ConditionStatus
		wantGranted metav1.ConditionStatus
	}{
		{
			name:        "provider missing",
			wantStatus:  metav1.ConditionFalse,
			wantReason:  corev1alpha1.ConnectionReasonProviderMissing,
			wantState:   corev1alpha1.ConnectionStateError,
			wantGranted: metav1.ConditionUnknown,
		},
		{
			// A provider that stops being accepted never leaves an earlier
			// ScopesGranted=True beside ProviderResolved=False.
			name:    "provider not accepted clears a granted verdict",
			objects: []runtime.Object{testConnectorProvider("tenant", "github")},
			existing: &corev1alpha1.ConnectionStatus{
				Conditions: []metav1.Condition{
					{Type: corev1alpha1.ConnectionConditionReady, Status: metav1.ConditionTrue, Reason: corev1alpha1.ConnectionReasonLinked},
					{Type: corev1alpha1.ConnectionConditionScopesGranted, Status: metav1.ConditionTrue, Reason: corev1alpha1.ConnectionReasonScopesGranted},
				},
			},
			wantStatus:  metav1.ConditionFalse,
			wantReason:  corev1alpha1.ConnectionReasonProviderInvalid,
			wantState:   corev1alpha1.ConnectionStateError,
			wantGranted: metav1.ConditionUnknown,
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
				Consent:    connectors.ConsentFor(scopedConnectorProvider()),
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
				Conditions: []metav1.Condition{{Type: corev1alpha1.ConnectionConditionReady, Status: metav1.ConditionFalse, Reason: corev1alpha1.ConnectionReasonRevoked}},
			},
			wantStatus: metav1.ConditionTrue,
			wantState:  corev1alpha1.ConnectionStateRevoked,
		},
		{
			name:    "expired survives a provider outage",
			objects: []runtime.Object{acceptedConnectorProvider()},
			existing: &corev1alpha1.ConnectionStatus{
				// A previous reconcile with a missing provider projected Error
				// over the stored state; the Ready reason still says Expired.
				State:      corev1alpha1.ConnectionStateError,
				Conditions: []metav1.Condition{{Type: corev1alpha1.ConnectionConditionReady, Status: metav1.ConditionFalse, Reason: corev1alpha1.ConnectionReasonExpired}},
			},
			wantStatus: metav1.ConditionTrue,
			wantState:  corev1alpha1.ConnectionStateExpired,
		},
		{
			name:    "ready condition true without recorded state",
			objects: []runtime.Object{acceptedConnectorProvider()},
			existing: &corev1alpha1.ConnectionStatus{
				Consent:    connectors.ConsentFor(scopedConnectorProvider()),
				Conditions: []metav1.Condition{{Type: corev1alpha1.ConnectionConditionReady, Status: metav1.ConditionTrue, Reason: "Linked"}},
			},
			wantStatus: metav1.ConditionTrue,
			wantState:  corev1alpha1.ConnectionStateReady,
		},
		{
			name:    "widened to readWrite after consent projects pending without touching ready",
			objects: []runtime.Object{scopedConnectorProvider()},
			existing: &corev1alpha1.ConnectionStatus{
				State:         corev1alpha1.ConnectionStateReady,
				GrantedScopes: []string{"read:user"},
				Consent:       connectors.ConsentFor(scopedConnectorProvider()),
				Conditions:    []metav1.Condition{{Type: corev1alpha1.ConnectionConditionReady, Status: metav1.ConditionTrue, Reason: corev1alpha1.ConnectionReasonLinked, ObservedGeneration: 1}},
			},
			mode:        corev1alpha1.ConnectionModeReadWrite,
			generation:  2,
			wantStatus:  metav1.ConditionTrue,
			wantState:   corev1alpha1.ConnectionStatePending,
			wantReady:   metav1.ConditionTrue,
			wantGranted: metav1.ConditionFalse,
		},
		{
			name:    "narrowed back to readOnly restores readiness",
			objects: []runtime.Object{scopedConnectorProvider()},
			existing: &corev1alpha1.ConnectionStatus{
				Consent:       connectors.ConsentFor(scopedConnectorProvider()),
				State:         corev1alpha1.ConnectionStatePending,
				GrantedScopes: []string{"read:user"},
				Conditions: []metav1.Condition{
					{Type: corev1alpha1.ConnectionConditionReady, Status: metav1.ConditionTrue, Reason: corev1alpha1.ConnectionReasonLinked, ObservedGeneration: 1},
					{Type: corev1alpha1.ConnectionConditionScopesGranted, Status: metav1.ConditionFalse, Reason: corev1alpha1.ConnectionReasonConsentRequired, ObservedGeneration: 2},
				},
			},
			mode:        corev1alpha1.ConnectionModeReadOnly,
			generation:  3,
			wantStatus:  metav1.ConditionTrue,
			wantState:   corev1alpha1.ConnectionStateReady,
			wantReady:   metav1.ConditionTrue,
			wantGranted: metav1.ConditionTrue,
		},
		{
			name: "provider that starts requiring more scopes asks for consent again",
			objects: func() []runtime.Object {
				provider := scopedConnectorProvider()
				provider.Spec.OAuth.Scopes.Read = []string{"read:user", "read:org"}
				return []runtime.Object{provider}
			}(),
			existing: &corev1alpha1.ConnectionStatus{
				State:         corev1alpha1.ConnectionStateReady,
				GrantedScopes: []string{"read:user"},
				Consent:       connectors.ConsentFor(scopedConnectorProvider()),
				Conditions:    []metav1.Condition{{Type: corev1alpha1.ConnectionConditionReady, Status: metav1.ConditionTrue, Reason: corev1alpha1.ConnectionReasonLinked, ObservedGeneration: 1}},
			},
			wantStatus:  metav1.ConditionTrue,
			wantState:   corev1alpha1.ConnectionStatePending,
			wantReady:   metav1.ConditionTrue,
			wantGranted: metav1.ConditionFalse,
		},
		{
			name: "provider whose OAuth client changed asks for consent again",
			objects: func() []runtime.Object {
				provider := scopedConnectorProvider()
				provider.Spec.OAuth.ClientID = "rotated-client"
				return []runtime.Object{provider}
			}(),
			existing: &corev1alpha1.ConnectionStatus{
				State:         corev1alpha1.ConnectionStateReady,
				GrantedScopes: []string{"read:user"},
				Consent:       connectors.ConsentFor(scopedConnectorProvider()),
				Conditions:    []metav1.Condition{{Type: corev1alpha1.ConnectionConditionReady, Status: metav1.ConditionTrue, Reason: corev1alpha1.ConnectionReasonLinked, ObservedGeneration: 1}},
			},
			wantStatus:  metav1.ConditionTrue,
			wantState:   corev1alpha1.ConnectionStatePending,
			wantReady:   metav1.ConditionTrue,
			wantGranted: metav1.ConditionFalse,
		},
		{
			name:    "consent without a provider record asks for consent again",
			objects: []runtime.Object{scopedConnectorProvider()},
			existing: &corev1alpha1.ConnectionStatus{
				State:         corev1alpha1.ConnectionStateReady,
				GrantedScopes: []string{"read:user"},
				Conditions:    []metav1.Condition{{Type: corev1alpha1.ConnectionConditionReady, Status: metav1.ConditionTrue, Reason: corev1alpha1.ConnectionReasonLinked, ObservedGeneration: 1}},
			},
			wantStatus:  metav1.ConditionTrue,
			wantState:   corev1alpha1.ConnectionStatePending,
			wantReady:   metav1.ConditionTrue,
			wantGranted: metav1.ConditionFalse,
		},
		{
			name:    "readWrite consent covers a readWrite mode",
			objects: []runtime.Object{scopedConnectorProvider()},
			existing: &corev1alpha1.ConnectionStatus{
				GrantedScopes: []string{"read:user", "repo"},
				Consent:       connectors.ConsentFor(scopedConnectorProvider()),
				Conditions:    []metav1.Condition{{Type: corev1alpha1.ConnectionConditionReady, Status: metav1.ConditionTrue, Reason: corev1alpha1.ConnectionReasonLinked, ObservedGeneration: 1}},
			},
			mode:        corev1alpha1.ConnectionModeReadWrite,
			wantStatus:  metav1.ConditionTrue,
			wantState:   corev1alpha1.ConnectionStateReady,
			wantReady:   metav1.ConditionTrue,
			wantGranted: metav1.ConditionTrue,
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
			if tt.mode != "" {
				connection.Spec.Mode = tt.mode
			}
			if tt.generation != 0 {
				connection.Generation = tt.generation
			}
			objects := append([]runtime.Object{connection}, tt.objects...)
			c := ctrlfake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objects...).WithStatusSubresource(&corev1alpha1.Connection{}).Build()
			reconciler := &ConnectionReconciler{Client: c, Scheme: scheme}
			key := types.NamespacedName{Namespace: "tenant", Name: "github-alice"}
			result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
			if err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			if result.RequeueAfter != 0 {
				t.Fatalf("RequeueAfter = %v, want watch-driven reconciles only", result.RequeueAfter)
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
				ready := meta.FindStatusCondition(updated.Status.Conditions, corev1alpha1.ConnectionConditionReady)
				if ready == nil {
					t.Fatal("reconciler must preserve the Ready condition owned by the consent path")
				}
				if tt.wantReady != "" && ready.Status != tt.wantReady {
					t.Fatalf("Ready = %#v, want %s", ready, tt.wantReady)
				}
			}
			if tt.wantGranted != "" {
				granted := meta.FindStatusCondition(updated.Status.Conditions, corev1alpha1.ConnectionConditionScopesGranted)
				if granted == nil || granted.Status != tt.wantGranted {
					t.Fatalf("ScopesGranted = %#v, want %s", granted, tt.wantGranted)
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
	if resolved == nil || resolved.Status != metav1.ConditionUnknown || resolved.Reason != corev1alpha1.ConnectionReasonProviderReadFailed {
		t.Fatalf("ProviderResolved = %#v, want Unknown with ProviderReadFailed", resolved)
	}
	if granted := meta.FindStatusCondition(updated.Status.Conditions, corev1alpha1.ConnectionConditionScopesGranted); granted == nil || granted.Status != metav1.ConditionUnknown {
		t.Fatalf("ScopesGranted = %#v, want Unknown while the provider cannot be read", granted)
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

// TestConnectionReconcilerPersistsScopesGrantedOnlyChange covers a pass whose
// only change is ScopesGranted: the consent owner already wrote Ready=True and
// state Ready, so nothing else moves, and the write must still happen.
func TestConnectionReconcilerPersistsScopesGrantedOnlyChange(t *testing.T) {
	scheme := connectorTestScheme(t)
	connection := testConnection("tenant", "github-alice", "github")
	connection.Status = corev1alpha1.ConnectionStatus{
		ObservedGeneration: connection.Generation,
		State:              corev1alpha1.ConnectionStateReady,
		GrantedScopes:      []string{"read:user"},
		Conditions: []metav1.Condition{
			{Type: corev1alpha1.ConnectionConditionReady, Status: metav1.ConditionTrue, Reason: corev1alpha1.ConnectionReasonLinked, ObservedGeneration: connection.Generation},
			{Type: corev1alpha1.ConnectionConditionScopesGranted, Status: metav1.ConditionFalse, Reason: corev1alpha1.ConnectionReasonPendingConsent,
				Message: "Consent has not completed", ObservedGeneration: connection.Generation},
			{Type: corev1alpha1.ConnectionConditionProviderResolved, Status: metav1.ConditionTrue, Reason: corev1alpha1.ConnectionReasonProviderResolved,
				Message: "ConnectorProvider is accepted", ObservedGeneration: connection.Generation},
		},
	}
	provider := scopedConnectorProvider()
	connection.Status.Consent = &corev1alpha1.ConnectionConsent{ProviderUID: string(provider.UID), AuthorityDigest: connectors.ProviderAuthorityDigest(provider)}
	c := ctrlfake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(connection, provider).WithStatusSubresource(&corev1alpha1.Connection{}).Build()
	reconciler := &ConnectionReconciler{Client: c, Scheme: scheme}
	key := types.NamespacedName{Namespace: "tenant", Name: "github-alice"}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	stored := &corev1alpha1.Connection{}
	if err := c.Get(context.Background(), key, stored); err != nil {
		t.Fatal(err)
	}
	granted := meta.FindStatusCondition(stored.Status.Conditions, corev1alpha1.ConnectionConditionScopesGranted)
	if granted == nil || granted.Status != metav1.ConditionTrue || stored.Status.State != corev1alpha1.ConnectionStateReady {
		t.Fatalf("ScopesGranted = %#v state = %q, want the scope change persisted", granted, stored.Status.State)
	}
}

// TestConnectionReconcilerRefusesInvalidProviderReference covers a provider
// reference no object could ever have: it is a stable invalid reference,
// not a read failure retried forever.
func TestConnectionReconcilerRefusesInvalidProviderReference(t *testing.T) {
	scheme := connectorTestScheme(t)
	connection := testConnection("tenant", "github-alice", "bad/name")
	c := ctrlfake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(connection).WithStatusSubresource(&corev1alpha1.Connection{}).Build()
	reconciler := &ConnectionReconciler{Client: c, Scheme: scheme}
	key := types.NamespacedName{Namespace: "tenant", Name: "github-alice"}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("an invalid reference must not surface as a retryable error: %v", err)
	}
	stored := &corev1alpha1.Connection{}
	if err := c.Get(context.Background(), key, stored); err != nil {
		t.Fatal(err)
	}
	resolved := meta.FindStatusCondition(stored.Status.Conditions, corev1alpha1.ConnectionConditionProviderResolved)
	if resolved == nil || resolved.Status != metav1.ConditionFalse || resolved.Reason != corev1alpha1.ConnectionReasonProviderMissing {
		t.Fatalf("ProviderResolved = %#v, want False/ProviderMissing", resolved)
	}
}
