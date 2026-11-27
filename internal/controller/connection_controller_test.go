/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/connectors"
	"github.com/orka-agents/orka/internal/store"
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

func TestConnectionReconcilerRepairsIndexLabels(t *testing.T) {
	scheme := connectorTestScheme(t)
	connection := testConnection("tenant", "github-alice", "github")
	provider := acceptedConnectorProvider()
	connection.Labels = map[string]string{"orka.ai/connection-subject": "stale"}
	connection.Spec.ProviderRef.Name = strings.Repeat("p", 80)
	provider.Name = connection.Spec.ProviderRef.Name
	c := ctrlfake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(connection, provider).WithStatusSubresource(&corev1alpha1.Connection{}).Build()
	reconciler := &ConnectionReconciler{Client: c, Scheme: scheme}
	key := types.NamespacedName{Namespace: "tenant", Name: "github-alice"}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	repaired := &corev1alpha1.Connection{}
	if err := c.Get(context.Background(), key, repaired); err != nil {
		t.Fatal(err)
	}
	want := connectors.ConnectionLabels(repaired)
	for key, value := range want {
		if repaired.Labels[key] != value {
			t.Fatalf("label %s = %q, want %q (labels %v)", key, repaired.Labels[key], value, repaired.Labels)
		}
	}
	if value := repaired.Labels[connectors.ConnectionProviderLabel]; len(value) > 63 || !strings.HasPrefix(value, "sha256-") {
		t.Fatalf("a long provider name must be label-encoded, got %q", value)
	}
	owned := &corev1alpha1.ConnectionList{}
	if err := c.List(context.Background(), owned, ctrlclient.InNamespace("tenant"), ctrlclient.MatchingLabels{
		connectors.ConnectionSubjectLabel: connectors.ConnectionSubjectLabelValue(repaired.Spec.Subject.Issuer, repaired.Spec.Subject.Subject),
	}); err != nil || len(owned.Items) != 1 {
		t.Fatalf("owned = %d err = %v", len(owned.Items), err)
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

type fakeConnectorCredentialStore struct {
	credentials          map[string]store.ConnectorCredential
	retired              map[string][]store.ConnectorCredential
	consents             map[string]int
	parked               map[string][]store.ConnectorCompletion
	deleted              []string
	tombstoned           []string
	deletedCompletions   []string
	failCompletionDelete bool
	grants               map[string]int64
	getErr               error
}

func newFakeConnectorCredentialStore() *fakeConnectorCredentialStore {
	return &fakeConnectorCredentialStore{
		credentials: map[string]store.ConnectorCredential{}, consents: map[string]int{}, parked: map[string][]store.ConnectorCompletion{},
	}
}

func (f *fakeConnectorCredentialStore) PutConnectorCredential(_ context.Context, ref store.ConnectorCredentialRef, credential store.ConnectorCredential) error {
	if f.grants == nil {
		f.grants = map[string]int64{}
	}
	f.grants[ref.ConnectionUID]++
	credential.GrantSequence = f.grants[ref.ConnectionUID]
	f.credentials[ref.ConnectionUID] = credential
	return nil
}

func (f *fakeConnectorCredentialStore) GetConnectorCredential(_ context.Context, ref store.ConnectorCredentialRef) (store.ConnectorCredential, error) {
	if f.getErr != nil {
		return store.ConnectorCredential{}, f.getErr
	}
	credential, ok := f.credentials[ref.ConnectionUID]
	if !ok {
		return store.ConnectorCredential{}, store.ErrNotFound
	}
	return credential, nil
}

func (f *fakeConnectorCredentialStore) RetireConnectorCredential(_ context.Context, ref store.ConnectorCredentialRef, credential store.ConnectorCredential) error {
	if f.retired == nil {
		f.retired = map[string][]store.ConnectorCredential{}
	}
	f.retired[ref.ConnectionUID] = append(f.retired[ref.ConnectionUID], credential)
	return nil
}

func (f *fakeConnectorCredentialStore) ListRetiredConnectorCredentials(_ context.Context, ref store.ConnectorCredentialRef) ([]store.ConnectorCredential, error) {
	return append([]store.ConnectorCredential(nil), f.retired[ref.ConnectionUID]...), nil
}

func (f *fakeConnectorCredentialStore) ReplaceConnectorCredential(_ context.Context, ref store.ConnectorCredentialRef, credential store.ConnectorCredential, _ int64) error {
	f.credentials[ref.ConnectionUID] = credential
	return nil
}

func (f *fakeConnectorCredentialStore) ShredConnectorCredential(_ context.Context, connectionUID string, _ int64) error {
	delete(f.credentials, connectionUID)
	return nil
}

func (f *fakeConnectorCredentialStore) TombstoneConnectorCustody(_ context.Context, connectionUID string) error {
	f.tombstoned = append(f.tombstoned, connectionUID)
	return nil
}

func (f *fakeConnectorCredentialStore) DeleteConnectorCredential(_ context.Context, connectionUID string) error {
	delete(f.credentials, connectionUID)
	f.deleted = append(f.deleted, connectionUID)
	return nil
}

func (f *fakeConnectorCredentialStore) CreateConnectorConsent(_ context.Context, consent store.ConnectorConsent) error {
	f.consents[consent.ConnectionUID]++
	return nil
}

func (f *fakeConnectorCredentialStore) ConsumeConnectorConsent(context.Context, string) (store.ConnectorConsent, error) {
	return store.ConnectorConsent{}, store.ErrNotFound
}

func (f *fakeConnectorCredentialStore) CreateConnectorCompletion(context.Context, store.ConnectorCompletion) error {
	return nil
}

func (f *fakeConnectorCredentialStore) ConsumeConnectorCompletion(context.Context, string) (store.ConnectorCompletion, error) {
	return store.ConnectorCompletion{}, store.ErrNotFound
}

func (f *fakeConnectorCredentialStore) CommitConnectorCompletion(ctx context.Context, _ string, ref store.ConnectorCredentialRef, credential store.ConnectorCredential) (store.ConnectorCredential, error) {
	if err := f.PutConnectorCredential(ctx, ref, credential); err != nil {
		return store.ConnectorCredential{}, err
	}
	return f.credentials[ref.ConnectionUID], nil
}

func (f *fakeConnectorCredentialStore) PeekConnectorCompletion(context.Context, string) (store.ConnectorCompletion, error) {
	return store.ConnectorCompletion{}, store.ErrNotFound
}

func (f *fakeConnectorCredentialStore) DeleteConnectorCompletion(_ context.Context, nonce string) error {
	if f.failCompletionDelete {
		return errors.New("database is locked")
	}
	for uid, completions := range f.parked {
		kept := completions[:0]
		for _, completion := range completions {
			if completion.Nonce != nonce {
				kept = append(kept, completion)
			}
		}
		f.parked[uid] = kept
	}
	f.deletedCompletions = append(f.deletedCompletions, nonce)
	return nil
}

func (f *fakeConnectorCredentialStore) ListConnectorCompletionsForConnection(_ context.Context, connectionUID string) ([]store.ConnectorCompletion, error) {
	return append([]store.ConnectorCompletion(nil), f.parked[connectionUID]...), nil
}

func (f *fakeConnectorCredentialStore) DeleteConnectorConsentsForConnection(_ context.Context, connectionUID string) error {
	delete(f.consents, connectionUID)
	return nil
}

type fakeConnectorRevoker struct {
	tokens             []string
	err                error
	custody            *fakeConnectorCredentialStore
	tombstonedAtRevoke *bool
}

func (f *fakeConnectorRevoker) Revoke(_ context.Context, _ connectors.OAuthProviderConfig, token string) error {
	if f.custody != nil && f.tombstonedAtRevoke == nil {
		fenced := len(f.custody.tombstoned) > 0
		f.tombstonedAtRevoke = &fenced
	}
	f.tokens = append(f.tokens, token)
	return f.err
}

func TestConnectionReconcilerFinalizerAndDisconnect(t *testing.T) {
	scheme := connectorTestScheme(t)
	provider := acceptedConnectorProvider()
	provider.Spec.OAuth.RevocationURL = "https://github.com/revoke"
	connection := testConnection("tenant", "github-alice", "github")
	credentials := newFakeConnectorCredentialStore()
	revoker := &fakeConnectorRevoker{err: errTestProviderRead, custody: credentials}
	c := ctrlfake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(connection, provider, connectorClientSecret("tenant")).
		WithStatusSubresource(&corev1alpha1.Connection{}).Build()
	reconciler := &ConnectionReconciler{Client: c, APIReader: c, Scheme: scheme, Credentials: credentials, Consents: credentials, Revoker: revoker}
	key := types.NamespacedName{Namespace: "tenant", Name: "github-alice"}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter != connectionRequeueInterval {
		t.Fatalf("finalizer add must requeue quickly, got %v", result.RequeueAfter)
	}
	updated := &corev1alpha1.Connection{}
	if err := c.Get(context.Background(), key, updated); err != nil {
		t.Fatal(err)
	}
	if !controllerutil.ContainsFinalizer(updated, ConnectionCustodyFinalizer) {
		t.Fatal("custody finalizer must be added before any consent can complete")
	}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(context.Background(), key, updated); err != nil {
		t.Fatal(err)
	}
	if updated.Status.State != corev1alpha1.ConnectionStatePending {
		t.Fatalf("state = %q, want Pending", updated.Status.State)
	}

	updated.Status.Consent = connectors.ConsentFor(provider)
	if err := c.Status().Update(context.Background(), updated); err != nil {
		t.Fatal(err)
	}
	authority := connectors.ProviderIssuerDigest(provider)
	credentials.credentials[string(updated.UID)] = store.ConnectorCredential{AccessToken: "gho_access", RefreshToken: "ghr_refresh", AuthorityDigest: authority, RevocationDigest: connectors.ProviderRevocationDigest(provider)}
	credentials.retired = map[string][]store.ConnectorCredential{string(updated.UID): {
		{AccessToken: "gho_previous", RefreshToken: "ghr_previous", AuthorityDigest: authority, RevocationDigest: connectors.ProviderRevocationDigest(provider)},
	}}
	credentials.consents[string(updated.UID)] = 1
	credentials.parked[string(updated.UID)] = []store.ConnectorCompletion{{
		Credential: store.ConnectorCredential{AccessToken: "gho_parked", RefreshToken: "ghr_parked", AuthorityDigest: authority, RevocationDigest: connectors.ProviderRevocationDigest(provider)},
	}}
	if err := c.Delete(context.Background(), updated); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(context.Background(), key, updated); err != nil {
		t.Fatalf("finalizer must hold the object: %v", err)
	}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("disconnect reconcile: %v", err)
	}
	if strings.Join(revoker.tokens, ",") != "ghr_refresh,gho_access,ghr_previous,gho_previous" {
		t.Fatalf("revoked tokens = %v, want the committed credential and the ones it replaced, refresh before access", revoker.tokens)
	}
	if revoker.tombstonedAtRevoke == nil || !*revoker.tombstonedAtRevoke {
		t.Fatal("custody must be tombstoned before revocation starts")
	}
	if _, held := credentials.credentials[string(updated.UID)]; held || len(credentials.deleted) != 1 {
		t.Fatalf("custody must be deleted even when revocation fails: %+v", credentials)
	}
	if _, held := credentials.consents[string(updated.UID)]; held {
		t.Fatal("pending consents must be dropped on disconnect")
	}
	if err := c.Get(context.Background(), key, updated); err == nil || !apierrors.IsNotFound(err) {
		t.Fatalf("finalizer must be released after custody deletion, err = %v", err)
	}
}

func TestConnectionReconcilerDisconnectWithoutCredentialSkipsRevocation(t *testing.T) {
	scheme := connectorTestScheme(t)
	connection := testConnection("tenant", "github-alice", "github")
	connection.Finalizers = []string{ConnectionCustodyFinalizer}
	now := metav1.Now()
	connection.DeletionTimestamp = &now
	credentials := newFakeConnectorCredentialStore()
	revoker := &fakeConnectorRevoker{}
	c := ctrlfake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(connection).WithStatusSubresource(&corev1alpha1.Connection{}).Build()
	reconciler := &ConnectionReconciler{Client: c, Scheme: scheme, Credentials: credentials, Consents: credentials, Revoker: revoker}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "tenant", Name: "github-alice"}}); err != nil {
		t.Fatal(err)
	}
	if len(revoker.tokens) != 0 {
		t.Fatalf("nothing to revoke, got %v", revoker.tokens)
	}
	if len(credentials.deleted) != 1 {
		t.Fatal("custody delete must still run")
	}
}

func TestConnectionReconcilerReapsExpiredCompletions(t *testing.T) {
	scheme := connectorTestScheme(t)
	provider := acceptedConnectorProvider()
	provider.Spec.OAuth.RevocationURL = "https://github.com/revoke"
	connection := testConnection("tenant", "github-alice", "github")
	connection.Finalizers = []string{ConnectionCustodyFinalizer}
	credentials := newFakeConnectorCredentialStore()
	authority := connectors.ProviderIssuerDigest(provider)
	credentials.parked[string(connection.UID)] = []store.ConnectorCompletion{
		{Nonce: "live", ExpiresAt: time.Now().Add(5 * time.Minute), Credential: store.ConnectorCredential{AccessToken: "gho_live", AuthorityDigest: authority, RevocationDigest: connectors.ProviderRevocationDigest(provider)}},
		{Nonce: "stale", ExpiresAt: time.Now().Add(-time.Minute), Credential: store.ConnectorCredential{AccessToken: "gho_stale", RefreshToken: "ghr_stale", AuthorityDigest: authority, RevocationDigest: connectors.ProviderRevocationDigest(provider)}},
		{Nonce: "committed", ExpiresAt: time.Now().Add(-time.Minute), Credential: store.ConnectorCredential{AccessToken: "gho_committed", RefreshToken: "ghr_committed", AuthorityDigest: authority, RevocationDigest: connectors.ProviderRevocationDigest(provider)}},
		{Nonce: "foreign", ExpiresAt: time.Now().Add(-time.Minute), Credential: store.ConnectorCredential{AccessToken: "gho_foreign", RefreshToken: "ghr_foreign", AuthorityDigest: "stale-authority"}},
		{Nonce: "rotated", ExpiresAt: time.Now().Add(-time.Minute), Credential: store.ConnectorCredential{AccessToken: "gho_committed", RefreshToken: "ghr_rotated", AuthorityDigest: authority, RevocationDigest: connectors.ProviderRevocationDigest(provider)}},
	}
	credentials.credentials[string(connection.UID)] = store.ConnectorCredential{AccessToken: "gho_committed", RefreshToken: "ghr_committed", AuthorityDigest: authority, RevocationDigest: connectors.ProviderRevocationDigest(provider)}
	revoker := &fakeConnectorRevoker{}
	c := ctrlfake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(connection, provider, connectorClientSecret("tenant")).
		WithStatusSubresource(&corev1alpha1.Connection{}).Build()
	reconciler := &ConnectionReconciler{Client: c, APIReader: c, Scheme: scheme, Credentials: credentials, Consents: credentials, Revoker: revoker}
	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "tenant", Name: "github-alice"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter < 4*time.Minute || result.RequeueAfter > 5*time.Minute+2*time.Second {
		t.Fatalf("RequeueAfter = %v, want just past the live completion's expiry", result.RequeueAfter)
	}
	if len(revoker.tokens) != 0 {
		t.Fatalf("revoked = %v, want no revocation of tokens nobody committed", revoker.tokens)
	}
	if strings.Join(credentials.deletedCompletions, ",") != "stale,committed,foreign,rotated" {
		t.Fatalf("deleted completions = %v, want every expired row dropped", credentials.deletedCompletions)
	}
	if remaining := credentials.parked[string(connection.UID)]; len(remaining) != 1 || remaining[0].Nonce != "live" {
		t.Fatalf("remaining completions = %+v", remaining)
	}
}

func TestConnectionReconcilerRetriesFailedCompletionCleanup(t *testing.T) {
	scheme := connectorTestScheme(t)
	provider := acceptedConnectorProvider()
	connection := testConnection("tenant", "github-alice", "github")
	connection.Finalizers = []string{ConnectionCustodyFinalizer}
	credentials := newFakeConnectorCredentialStore()
	credentials.failCompletionDelete = true
	credentials.parked[string(connection.UID)] = []store.ConnectorCompletion{
		{Nonce: "stale", ExpiresAt: time.Now().Add(-time.Minute), Credential: store.ConnectorCredential{AccessToken: "gho_stale"}},
	}
	c := ctrlfake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(connection, provider, connectorClientSecret("tenant")).
		WithStatusSubresource(&corev1alpha1.Connection{}).Build()
	reconciler := &ConnectionReconciler{Client: c, APIReader: c, Scheme: scheme, Credentials: credentials, Consents: credentials}
	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "tenant", Name: "github-alice"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter <= 0 || result.RequeueAfter > connectionRevocationRetry+2*time.Second {
		t.Fatalf("RequeueAfter = %v, want a retry of the failed cleanup", result.RequeueAfter)
	}
}

func TestConnectionReconcilerDisconnectSkipsRevocationAgainstChangedAuthority(t *testing.T) {
	scheme := connectorTestScheme(t)
	provider := acceptedConnectorProvider()
	provider.Spec.OAuth.RevocationURL = "https://github.com/revoke"
	connection := testConnection("tenant", "github-alice", "github")
	connection.Finalizers = []string{ConnectionCustodyFinalizer}
	credentials := newFakeConnectorCredentialStore()
	credentials.credentials[string(connection.UID)] = store.ConnectorCredential{AccessToken: "gho_access", RefreshToken: "ghr_refresh", AuthorityDigest: "old-authority"}
	revoker := &fakeConnectorRevoker{}
	c := ctrlfake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(connection, provider, connectorClientSecret("tenant")).
		WithStatusSubresource(&corev1alpha1.Connection{}).Build()
	reconciler := &ConnectionReconciler{Client: c, APIReader: c, Scheme: scheme, Credentials: credentials, Consents: credentials, Revoker: revoker}
	key := types.NamespacedName{Namespace: "tenant", Name: "github-alice"}
	if err := c.Delete(context.Background(), connection); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if len(revoker.tokens) != 0 {
		t.Fatalf("tokens must not be sent to a different authority, revoked = %v", revoker.tokens)
	}
	if len(credentials.deleted) != 1 {
		t.Fatal("custody must still be deleted")
	}
	if err := c.Get(context.Background(), key, &corev1alpha1.Connection{}); err == nil || !apierrors.IsNotFound(err) {
		t.Fatalf("finalizer must be released, err = %v", err)
	}
}

func TestConnectionReconcilerRevokesOnlyCommittedTokens(t *testing.T) {
	scheme := connectorTestScheme(t)
	provider := acceptedConnectorProvider()
	provider.Spec.OAuth.RevocationURL = "https://github.com/revoke"
	connection := testConnection("tenant", "github-alice", "github")
	connection.Finalizers = []string{ConnectionCustodyFinalizer}
	authority := connectors.ProviderIssuerDigest(provider)
	credentials := newFakeConnectorCredentialStore()
	credentials.credentials[string(connection.UID)] = store.ConnectorCredential{AccessToken: "gho_access", RefreshToken: "ghr_refresh", AuthorityDigest: authority, RevocationDigest: connectors.ProviderRevocationDigest(provider)}
	credentials.parked[string(connection.UID)] = []store.ConnectorCompletion{
		{Nonce: "stale", ExpiresAt: time.Now().Add(-time.Minute), Credential: store.ConnectorCredential{AccessToken: "gho_stale", RefreshToken: "ghr_stale", AuthorityDigest: authority, RevocationDigest: connectors.ProviderRevocationDigest(provider)}},
	}
	revoker := &fakeConnectorRevoker{}
	c := ctrlfake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(connection, provider, connectorClientSecret("tenant")).
		WithStatusSubresource(&corev1alpha1.Connection{}).Build()
	reconciler := &ConnectionReconciler{Client: c, APIReader: c, Scheme: scheme, Credentials: credentials, Consents: credentials, Revoker: revoker}
	key := types.NamespacedName{Namespace: "tenant", Name: "github-alice"}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if len(revoker.tokens) != 0 || strings.Join(credentials.deletedCompletions, ",") != "stale" {
		t.Fatalf("reaper: revoked = %v deleted = %v", revoker.tokens, credentials.deletedCompletions)
	}
	credentials.parked[string(connection.UID)] = []store.ConnectorCompletion{
		{Nonce: "parked", ExpiresAt: time.Now().Add(time.Minute), Credential: store.ConnectorCredential{AccessToken: "gho_parked", RefreshToken: "ghr_parked", AuthorityDigest: authority, RevocationDigest: connectors.ProviderRevocationDigest(provider)}},
	}
	live := &corev1alpha1.Connection{}
	if err := c.Get(context.Background(), key, live); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(context.Background(), live); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(revoker.tokens, ",") != "ghr_refresh,gho_access" {
		t.Fatalf("disconnect revoked = %v, want only the committed credential", revoker.tokens)
	}
	if len(credentials.deleted) != 1 {
		t.Fatal("custody must be deleted")
	}
}

func TestConnectionReconcilerFinishesCommittedCompletion(t *testing.T) {
	scheme := connectorTestScheme(t)
	provider := acceptedConnectorProvider()
	connection := testConnection("tenant", "github-alice", "github")
	connection.Finalizers = []string{ConnectionCustodyFinalizer}
	connection.Spec.Mode = corev1alpha1.ConnectionModeReadOnly
	credentials := newFakeConnectorCredentialStore()
	authority := connectors.ProviderIssuerDigest(provider)
	material := store.ConnectorCredential{
		AccessToken: "gho_done", RefreshToken: "ghr_done", AuthorityDigest: authority, RevocationDigest: connectors.ProviderRevocationDigest(provider),
		Scopes: connectors.ScopesForMode(provider, corev1alpha1.ConnectionModeReadOnly), ExpiresAt: time.Now().Add(time.Hour),
	}
	credentials.credentials[string(connection.UID)] = material
	consented := connectors.ProviderAuthorityDigest(provider)
	credentials.parked[string(connection.UID)] = []store.ConnectorCompletion{
		{Nonce: "replaced", Committed: true, Mode: corev1alpha1.ConnectionModeReadOnly, ExpiresAt: time.Now().Add(-time.Minute), ConsentAuthorityDigest: consented,
			Credential: store.ConnectorCredential{AccessToken: "gho_old", RefreshToken: "ghr_old", AuthorityDigest: authority, RevocationDigest: connectors.ProviderRevocationDigest(provider)}},
		{Nonce: "done", Committed: true, Mode: corev1alpha1.ConnectionModeReadOnly, ExpiresAt: time.Now().Add(-time.Minute), ConsentAuthorityDigest: consented, Credential: material},
	}
	var statusFailure atomic.Bool
	c := ctrlfake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(connection, provider, connectorClientSecret("tenant")).
		WithStatusSubresource(&corev1alpha1.Connection{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, c ctrlclient.Client, subResource string, obj ctrlclient.Object, opts ...ctrlclient.SubResourceUpdateOption) error {
				if statusFailure.Load() {
					return errors.New("transient status failure")
				}
				return c.SubResource(subResource).Update(ctx, obj, opts...)
			},
		}).Build()
	reconciler := &ConnectionReconciler{Client: c, APIReader: c, Scheme: scheme, Credentials: credentials, Consents: credentials}
	statusFailure.Store(true)
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "tenant", Name: "github-alice"}}); err == nil {
		t.Fatal("a failed status write must surface")
	}
	if remaining := credentials.parked[string(connection.UID)]; len(remaining) != 1 || remaining[0].Nonce != "done" {
		t.Fatalf("remaining after failed status write = %+v, want the applied completion kept", remaining)
	}
	statusFailure.Store(false)
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "tenant", Name: "github-alice"}}); err != nil {
		t.Fatal(err)
	}
	updated := &corev1alpha1.Connection{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "tenant", Name: "github-alice"}, updated); err != nil {
		t.Fatal(err)
	}
	if !connectors.ConnectionLinked(updated) || strings.Join(updated.Status.GrantedScopes, " ") != strings.Join(material.Scopes, " ") || updated.Status.LinkedAt == nil {
		t.Fatalf("status after committed completion = %+v, want the link recorded from custody", updated.Status)
	}
	if strings.Join(credentials.deletedCompletions, ",") != "replaced,done" || len(credentials.parked[string(connection.UID)]) != 0 {
		t.Fatalf("deleted completions = %v remaining = %+v", credentials.deletedCompletions, credentials.parked[string(connection.UID)])
	}

	stale := testConnection("tenant", "github-carol", "github")
	stale.Finalizers = []string{ConnectionCustodyFinalizer}
	stale.Spec.Mode = corev1alpha1.ConnectionModeReadOnly
	credentials.credentials[string(stale.UID)] = material
	credentials.parked[string(stale.UID)] = []store.ConnectorCompletion{
		{Nonce: "retargeted", Committed: true, Mode: corev1alpha1.ConnectionModeReadOnly, ExpiresAt: time.Now().Add(-time.Minute), ConsentAuthorityDigest: "previous-authority", Credential: material},
	}
	c = ctrlfake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(stale, provider, connectorClientSecret("tenant")).
		WithStatusSubresource(&corev1alpha1.Connection{}).Build()
	reconciler = &ConnectionReconciler{Client: c, APIReader: c, Scheme: scheme, Credentials: credentials, Consents: credentials}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "tenant", Name: "github-carol"}}); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "tenant", Name: "github-carol"}, updated); err != nil {
		t.Fatal(err)
	}
	if connectors.ConnectionLinked(updated) || updated.Status.LinkedAt != nil {
		t.Fatalf("a completion consented under another authority must not link: %+v", updated.Status)
	}
	if len(credentials.parked[string(stale.UID)]) != 0 {
		t.Fatalf("stale committed completion must be dropped, remaining = %+v", credentials.parked[string(stale.UID)])
	}
}

func TestConnectionReconcilerExpiresRefreshlessLink(t *testing.T) {
	scheme := connectorTestScheme(t)
	provider := acceptedConnectorProvider()
	credentials := newFakeConnectorCredentialStore()
	authority := connectors.ProviderIssuerDigest(provider)
	for name, refreshToken := range map[string]string{"github-alice": "", "github-bob": "ghr_live"} {
		connection := testConnection("tenant", name, "github")
		connection.Finalizers = []string{ConnectionCustodyFinalizer}
		expired := metav1.NewTime(time.Now().Add(-time.Minute))
		connection.Status.State = corev1alpha1.ConnectionStateReady
		connection.Status.Consent = connectors.ConsentFor(provider)
		connection.Status.ExpiresAt = &expired
		connection.Status.GrantedScopes = connectors.ScopesForMode(provider, corev1alpha1.ConnectionModeReadOnly)
		connection.Status.Conditions = []metav1.Condition{
			{Type: corev1alpha1.ConnectionConditionReady, Status: metav1.ConditionTrue, Reason: corev1alpha1.ConnectionReasonLinked, ObservedGeneration: connection.Generation},
		}
		credentials.credentials[string(connection.UID)] = store.ConnectorCredential{AccessToken: "gho_" + name, RefreshToken: refreshToken, AuthorityDigest: authority, ExpiresAt: expired.Time}
		c := ctrlfake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(connection, provider, connectorClientSecret("tenant")).
			WithStatusSubresource(&corev1alpha1.Connection{}).Build()
		reconciler := &ConnectionReconciler{Client: c, APIReader: c, Scheme: scheme, Credentials: credentials, Consents: credentials}
		if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "tenant", Name: name}}); err != nil {
			t.Fatal(err)
		}
		updated := &corev1alpha1.Connection{}
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: "tenant", Name: name}, updated); err != nil {
			t.Fatal(err)
		}
		ready := meta.FindStatusCondition(updated.Status.Conditions, corev1alpha1.ConnectionConditionReady)
		if refreshToken == "" {
			if updated.Status.State != corev1alpha1.ConnectionStateExpired || ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != corev1alpha1.ConnectionReasonExpired {
				t.Fatalf("%s: status = %+v, want Expired", name, updated.Status)
			}
		} else if updated.Status.State != corev1alpha1.ConnectionStateReady || ready == nil || ready.Status != metav1.ConditionTrue {
			t.Fatalf("%s: status = %+v, want a refreshable link left alone", name, updated.Status)
		}
	}
}

func TestConnectionNextPassFollowsExpiry(t *testing.T) {
	now := time.Now()
	connection := testConnection("tenant", "github-alice", "github")
	if got := connectionNextPass(connection, now, time.Time{}); got != 0 {
		t.Fatalf("no deadline = %v, want watch-driven reconciles only", got)
	}
	soon := metav1.NewTime(now.Add(20 * time.Second))
	connection.Status.ExpiresAt = &soon
	if got := connectionNextPass(connection, now, time.Time{}); got <= 0 || got > 21*time.Second {
		t.Fatalf("near expiry = %v, want just past the expiry", got)
	}
	far := metav1.NewTime(now.Add(time.Hour))
	connection.Status.ExpiresAt = &far
	if got := connectionNextPass(connection, now, time.Time{}); got < time.Hour || got > time.Hour+2*time.Second {
		t.Fatalf("far expiry = %v, want just past the expiry", got)
	}
	if got := connectionNextPass(connection, now, now.Add(10*time.Minute)); got < 10*time.Minute || got > 10*time.Minute+2*time.Second {
		t.Fatalf("completion deadline = %v, want just past the completion's expiry", got)
	}
	past := metav1.NewTime(now.Add(-time.Minute))
	connection.Status.ExpiresAt = &past
	if got := connectionNextPass(connection, now, time.Time{}); got != 0 {
		t.Fatalf("past expiry = %v, want no requeue", got)
	}
	if got := connectionNextPass(connection, now, now.Add(-time.Millisecond)); got != time.Second {
		t.Fatalf("elapsed completion deadline = %v, want a prompt requeue", got)
	}
}

func TestConnectionReconcilerPersistsRecoveredStatusOnSettledConnection(t *testing.T) {
	scheme := connectorTestScheme(t)
	provider := acceptedConnectorProvider()
	connection := testConnection("tenant", "github-alice", "github")
	connection.Finalizers = []string{ConnectionCustodyFinalizer}
	connection.Spec.Mode = corev1alpha1.ConnectionModeReadOnly
	credentials := newFakeConnectorCredentialStore()
	c := ctrlfake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(connection, provider, connectorClientSecret("tenant")).
		WithStatusSubresource(&corev1alpha1.Connection{}).Build()
	reconciler := &ConnectionReconciler{Client: c, APIReader: c, Scheme: scheme, Credentials: credentials, Consents: credentials}
	key := types.NamespacedName{Namespace: "tenant", Name: "github-alice"}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	settled := &corev1alpha1.Connection{}
	if err := c.Get(context.Background(), key, settled); err != nil {
		t.Fatal(err)
	}
	if connectors.ConnectionLinked(settled) || settled.Status.ObservedGeneration != settled.Generation {
		t.Fatalf("settled status = %+v, want an observed, unlinked Connection", settled.Status)
	}
	authority := connectors.ProviderIssuerDigest(provider)
	material := store.ConnectorCredential{
		AccessToken: "gho_done", RefreshToken: "ghr_done", AuthorityDigest: authority, GrantSequence: 1,
		Scopes: connectors.ScopesForMode(provider, corev1alpha1.ConnectionModeReadOnly), ExpiresAt: time.Now().Add(time.Hour),
	}
	credentials.credentials[string(connection.UID)] = material
	credentials.parked[string(connection.UID)] = []store.ConnectorCompletion{{
		Nonce: "done", Committed: true, Mode: corev1alpha1.ConnectionModeReadOnly, ExpiresAt: time.Now().Add(-time.Minute),
		ConsentAuthorityDigest: connectors.ProviderAuthorityDigest(provider), Credential: material,
	}}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	updated := &corev1alpha1.Connection{}
	if err := c.Get(context.Background(), key, updated); err != nil {
		t.Fatal(err)
	}
	if !connectors.ConnectionLinked(updated) || updated.Status.GrantSequence != 1 {
		t.Fatalf("status after recovery = %+v, want the link persisted with its grant", updated.Status)
	}
	if strings.Join(credentials.deletedCompletions, ",") != "done" {
		t.Fatalf("deleted completions = %v, want the recovered one removed after the write", credentials.deletedCompletions)
	}
}

func TestConnectionReconcilerRevokesAfterTokenEndpointMove(t *testing.T) {
	for name, mutate := range map[string]struct {
		mutate func(*corev1alpha1.ConnectorProvider)
		want   int
	}{
		"token endpoint moved": {func(p *corev1alpha1.ConnectorProvider) {
			p.Spec.OAuth.TokenURL = "https://github.com/login/oauth/token2"
		}, 2},
		"client replaced":           {func(p *corev1alpha1.ConnectorProvider) { p.Spec.OAuth.ClientID = "other-client" }, 0},
		"revocation endpoint moved": {func(p *corev1alpha1.ConnectorProvider) { p.Spec.OAuth.RevocationURL = "https://github.com/revoke2" }, 0},
	} {
		t.Run(name, func(t *testing.T) {
			scheme := connectorTestScheme(t)
			provider := acceptedConnectorProvider()
			provider.Spec.OAuth.RevocationURL = "https://github.com/revoke"
			connection := testConnection("tenant", "github-alice", "github")
			connection.Finalizers = []string{ConnectionCustodyFinalizer}
			credentials := newFakeConnectorCredentialStore()
			credentials.credentials[string(connection.UID)] = store.ConnectorCredential{
				AccessToken: "gho_access", RefreshToken: "ghr_refresh",
				AuthorityDigest: connectors.ProviderIssuerDigest(provider), RevocationDigest: connectors.ProviderRevocationDigest(provider),
			}
			mutate.mutate(provider)
			revoker := &fakeConnectorRevoker{}
			c := ctrlfake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(connection, provider, connectorClientSecret("tenant")).
				WithStatusSubresource(&corev1alpha1.Connection{}).Build()
			reconciler := &ConnectionReconciler{Client: c, APIReader: c, Scheme: scheme, Credentials: credentials, Consents: credentials, Revoker: revoker}
			key := types.NamespacedName{Namespace: "tenant", Name: "github-alice"}
			if err := c.Delete(context.Background(), connection); err != nil {
				t.Fatal(err)
			}
			if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
				t.Fatal(err)
			}
			if len(revoker.tokens) != mutate.want {
				t.Fatalf("revoked = %v, want %d tokens", revoker.tokens, mutate.want)
			}
			if len(credentials.deleted) != 1 {
				t.Fatal("custody must be deleted once revocation was judged")
			}
		})
	}
}

func TestConnectionReconcilerRetainsCustodyWithoutRevocationMaterial(t *testing.T) {
	scheme := connectorTestScheme(t)
	provider := acceptedConnectorProvider()
	provider.Spec.OAuth.RevocationURL = "https://github.com/revoke"
	connection := testConnection("tenant", "github-alice", "github")
	connection.Finalizers = []string{ConnectionCustodyFinalizer}
	credentials := newFakeConnectorCredentialStore()
	credentials.credentials[string(connection.UID)] = store.ConnectorCredential{
		AccessToken: "gho_access", RefreshToken: "ghr_refresh",
		AuthorityDigest: connectors.ProviderIssuerDigest(provider), RevocationDigest: connectors.ProviderRevocationDigest(provider),
	}
	revoker := &fakeConnectorRevoker{}
	c := ctrlfake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(connection, provider).
		WithStatusSubresource(&corev1alpha1.Connection{}).Build()
	reconciler := &ConnectionReconciler{Client: c, APIReader: c, Scheme: scheme, Credentials: credentials, Consents: credentials, Revoker: revoker}
	key := types.NamespacedName{Namespace: "tenant", Name: "github-alice"}
	if err := c.Delete(context.Background(), connection); err != nil {
		t.Fatal(err)
	}
	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	if err != nil || result.RequeueAfter != connectionRevocationRetry {
		t.Fatalf("reconcile without the client secret = %+v err = %v, want a retry", result, err)
	}
	if len(revoker.tokens) != 0 || len(credentials.deleted) != 0 {
		t.Fatalf("revoked = %v deleted = %v, want custody kept unrevoked", revoker.tokens, credentials.deleted)
	}
	if err := c.Get(context.Background(), key, &corev1alpha1.Connection{}); err != nil {
		t.Fatalf("the finalizer must hold the Connection: %v", err)
	}
	if len(credentials.tombstoned) != 1 {
		t.Fatalf("tombstoned = %v, want the custody fence in place", credentials.tombstoned)
	}
	if err := c.Create(context.Background(), connectorClientSecret("tenant")); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(revoker.tokens, ",") != "ghr_refresh,gho_access" || len(credentials.deleted) != 1 {
		t.Fatalf("revoked = %v deleted = %v after the secret returned", revoker.tokens, credentials.deleted)
	}
	if err := c.Get(context.Background(), key, &corev1alpha1.Connection{}); err == nil || !apierrors.IsNotFound(err) {
		t.Fatalf("finalizer must be released, err = %v", err)
	}
}

func TestConnectionReconcilerFinishesDisconnectWhenProviderIsBeingDeleted(t *testing.T) {
	scheme := connectorTestScheme(t)
	provider := acceptedConnectorProvider()
	provider.Spec.OAuth.RevocationURL = "https://github.com/revoke"
	provider.Finalizers = []string{ConnectorProviderConnectionsFinalizer}
	connection := testConnection("tenant", "github-alice", "github")
	connection.Finalizers = []string{ConnectionCustodyFinalizer}
	credentials := newFakeConnectorCredentialStore()
	credentials.credentials[string(connection.UID)] = store.ConnectorCredential{
		AccessToken: "gho_access", RefreshToken: "ghr_refresh",
		AuthorityDigest: connectors.ProviderIssuerDigest(provider), RevocationDigest: connectors.ProviderRevocationDigest(provider),
	}
	revoker := &fakeConnectorRevoker{}
	now := metav1.Now()
	terminating := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant", DeletionTimestamp: &now, Finalizers: []string{"kubernetes"}}}
	c := ctrlfake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(connection, provider).WithObjects(terminating).
		WithStatusSubresource(&corev1alpha1.Connection{}).Build()
	reconciler := &ConnectionReconciler{Client: c, APIReader: c, Scheme: scheme, Credentials: credentials, Consents: credentials, Revoker: revoker}
	key := types.NamespacedName{Namespace: "tenant", Name: "github-alice"}
	if err := c.Delete(context.Background(), provider); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(context.Background(), connection); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if len(revoker.tokens) != 0 || len(credentials.deleted) != 1 {
		t.Fatalf("revoked = %v deleted = %v, want custody deleted unrevoked", revoker.tokens, credentials.deleted)
	}
	if err := c.Get(context.Background(), key, &corev1alpha1.Connection{}); err == nil || !apierrors.IsNotFound(err) {
		t.Fatalf("finalizer must be released, err = %v", err)
	}
}

func TestConnectionReconcilerKeepsCustodyWhenOnlyProviderIsDeleted(t *testing.T) {
	scheme := connectorTestScheme(t)
	provider := acceptedConnectorProvider()
	provider.Spec.OAuth.RevocationURL = "https://github.com/revoke"
	provider.Finalizers = []string{ConnectorProviderConnectionsFinalizer}
	connection := testConnection("tenant", "github-alice", "github")
	connection.Finalizers = []string{ConnectionCustodyFinalizer}
	credentials := newFakeConnectorCredentialStore()
	credentials.credentials[string(connection.UID)] = store.ConnectorCredential{
		AccessToken: "gho_access", RefreshToken: "ghr_refresh",
		AuthorityDigest: connectors.ProviderIssuerDigest(provider), RevocationDigest: connectors.ProviderRevocationDigest(provider),
	}
	revoker := &fakeConnectorRevoker{}
	active := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant"}}
	c := ctrlfake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(connection, provider).WithObjects(active).
		WithStatusSubresource(&corev1alpha1.Connection{}).Build()
	reconciler := &ConnectionReconciler{Client: c, APIReader: c, Scheme: scheme, Credentials: credentials, Consents: credentials, Revoker: revoker}
	key := types.NamespacedName{Namespace: "tenant", Name: "github-alice"}
	if err := c.Delete(context.Background(), provider); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(context.Background(), connection); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if len(credentials.deleted) != 0 {
		t.Fatalf("custody deleted = %v, want it kept until the Secret returns", credentials.deleted)
	}
	if err := c.Get(context.Background(), key, &corev1alpha1.Connection{}); err != nil {
		t.Fatalf("the finalizer must hold the Connection, err = %v", err)
	}
}

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

func TestConnectionReconcilerKeepsCommittedCompletionWhenCustodyUnreadable(t *testing.T) {
	scheme := connectorTestScheme(t)
	provider := acceptedConnectorProvider()
	connection := testConnection("tenant", "github-alice", "github")
	connection.Finalizers = []string{ConnectionCustodyFinalizer}
	credentials := newFakeConnectorCredentialStore()
	material := store.ConnectorCredential{
		AccessToken: "gho_done", RefreshToken: "ghr_done", AuthorityDigest: connectors.ProviderIssuerDigest(provider),
		RevocationDigest: connectors.ProviderRevocationDigest(provider), ExpiresAt: time.Now().Add(time.Hour),
	}
	credentials.credentials[string(connection.UID)] = material
	credentials.parked[string(connection.UID)] = []store.ConnectorCompletion{
		{Nonce: "done", Committed: true, Mode: corev1alpha1.ConnectionModeReadOnly, ExpiresAt: time.Now().Add(-time.Minute),
			ConsentAuthorityDigest: connectors.ProviderAuthorityDigest(provider), Credential: material},
	}
	credentials.getErr = errors.New("database is locked")
	c := ctrlfake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(connection, provider, connectorClientSecret("tenant")).
		WithStatusSubresource(&corev1alpha1.Connection{}).Build()
	reconciler := &ConnectionReconciler{Client: c, APIReader: c, Scheme: scheme, Credentials: credentials, Consents: credentials}
	key := types.NamespacedName{Namespace: "tenant", Name: "github-alice"}
	_, _ = reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	if remaining := credentials.parked[string(connection.UID)]; len(remaining) != 1 || len(credentials.deletedCompletions) != 0 {
		t.Fatalf("remaining = %+v deleted = %v, want the committed completion kept", remaining, credentials.deletedCompletions)
	}
	credentials.getErr = nil
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	updated := &corev1alpha1.Connection{}
	if err := c.Get(context.Background(), key, updated); err != nil {
		t.Fatal(err)
	}
	if !connectors.ConnectionLinked(updated) || len(credentials.parked[string(connection.UID)]) != 0 {
		t.Fatalf("status = %+v remaining = %+v, want the link recorded once custody reads", updated.Status, credentials.parked[string(connection.UID)])
	}
}

func TestConnectionReconcilerRevocationReadsProviderUncached(t *testing.T) {
	scheme := connectorTestScheme(t)
	cachedProvider := acceptedConnectorProvider()
	cachedProvider.Spec.OAuth.RevocationURL = "https://github.com/revoke"
	currentProvider := cachedProvider.DeepCopy()
	currentProvider.Spec.OAuth.RevocationURL = "https://github.com/revoke-v2"
	connection := testConnection("tenant", "github-alice", "github")
	connection.Finalizers = []string{ConnectionCustodyFinalizer}
	credentials := newFakeConnectorCredentialStore()
	credentials.credentials[string(connection.UID)] = store.ConnectorCredential{
		AccessToken: "gho_access", RefreshToken: "ghr_refresh", AuthorityDigest: connectors.ProviderIssuerDigest(cachedProvider),
		RevocationDigest: connectors.ProviderRevocationDigest(cachedProvider),
	}
	revoker := &fakeConnectorRevoker{}
	cached := ctrlfake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(connection, cachedProvider, connectorClientSecret("tenant")).
		WithStatusSubresource(&corev1alpha1.Connection{}).Build()
	api := ctrlfake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(currentProvider, connectorClientSecret("tenant")).Build()
	reconciler := &ConnectionReconciler{Client: cached, APIReader: api, Scheme: scheme, Credentials: credentials, Consents: credentials, Revoker: revoker}
	key := types.NamespacedName{Namespace: "tenant", Name: "github-alice"}
	if err := cached.Delete(context.Background(), connection); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if len(revoker.tokens) != 0 {
		t.Fatalf("tokens were sent to the stale cached endpoint: %v", revoker.tokens)
	}
	if len(credentials.deleted) != 1 {
		t.Fatal("custody must still be deleted")
	}
}

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
