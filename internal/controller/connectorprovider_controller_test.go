/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/connectors"
)

func connectorTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func testConnectorProvider(namespace, name string) *corev1alpha1.ConnectorProvider {
	return &corev1alpha1.ConnectorProvider{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Generation: 1, UID: types.UID(name + "-uid")},
		Spec: corev1alpha1.ConnectorProviderSpec{
			OAuth: corev1alpha1.ConnectorOAuthConfig{
				AuthorizeURL:    "https://github.com/login/oauth/authorize",
				TokenURL:        "https://github.com/login/oauth/access_token",
				ClientID:        "client",
				ClientSecretRef: corev1alpha1.SecretKeySelector{Name: "oauth", Key: "clientSecret"},
			},
			Tools: []corev1alpha1.ConnectorTool{{Name: "list_pull_requests", Class: corev1alpha1.ConnectorToolClassRead, Source: corev1alpha1.ConnectorToolSourceBuiltin}},
		},
	}
}

func connectorClientSecret(namespace string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "oauth", Namespace: namespace},
		Data:       map[string][]byte{"clientSecret": []byte("s3cret-value")},
	}
}

func TestConnectorProviderReconcilerConditions(t *testing.T) {
	tests := []struct {
		name         string
		provider     *corev1alpha1.ConnectorProvider
		objects      []runtime.Object
		knownBuiltin connectors.BuiltinToolCheck
		wantAccepted metav1.ConditionStatus
		wantResolved metav1.ConditionStatus
		wantReason   string
	}{
		{
			name: "invalid url",
			provider: func() *corev1alpha1.ConnectorProvider {
				p := testConnectorProvider("tenant", "github")
				p.Spec.OAuth.TokenURL = "http://github.com/token"
				return p
			}(),
			wantAccepted: metav1.ConditionFalse,
			wantResolved: metav1.ConditionFalse,
			wantReason:   connectors.ReasonInvalidProvider,
		},
		{
			name:         "unknown builtin",
			provider:     testConnectorProvider("tenant", "github"),
			objects:      []runtime.Object{connectorClientSecret("tenant")},
			knownBuiltin: func(string) bool { return false },
			wantAccepted: metav1.ConditionFalse,
			wantResolved: metav1.ConditionFalse,
		},
		{
			name:         "missing secret",
			provider:     testConnectorProvider("tenant", "github"),
			wantAccepted: metav1.ConditionTrue,
			wantResolved: metav1.ConditionFalse,
			wantReason:   connectors.ReasonReferenceNotFound,
		},
		{
			name:         "resolved",
			provider:     testConnectorProvider("tenant", "github"),
			objects:      []runtime.Object{connectorClientSecret("tenant")},
			knownBuiltin: func(name string) bool { return name == "list_pull_requests" },
			wantAccepted: metav1.ConditionTrue,
			wantResolved: metav1.ConditionTrue,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := connectorTestScheme(t)
			objects := append([]runtime.Object{tt.provider}, tt.objects...)
			c := ctrlfake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objects...).WithStatusSubresource(&corev1alpha1.ConnectorProvider{}).Build()
			reconciler := &ConnectorProviderReconciler{Client: c, APIReader: c, Scheme: scheme, KnownBuiltinTool: tt.knownBuiltin}
			key := types.NamespacedName{Namespace: "tenant", Name: "github"}
			result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
			if err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			if result.RequeueAfter != connectorProviderRefreshInterval {
				t.Fatalf("RequeueAfter = %v, want %v", result.RequeueAfter, connectorProviderRefreshInterval)
			}
			updated := &corev1alpha1.ConnectorProvider{}
			if err := c.Get(context.Background(), key, updated); err != nil {
				t.Fatal(err)
			}
			if updated.Status.ObservedGeneration != updated.Generation {
				t.Fatalf("observedGeneration = %d, want %d", updated.Status.ObservedGeneration, updated.Generation)
			}
			accepted := meta.FindStatusCondition(updated.Status.Conditions, corev1alpha1.ConnectorProviderConditionAccepted)
			resolved := meta.FindStatusCondition(updated.Status.Conditions, corev1alpha1.ConnectorProviderConditionResolvedRefs)
			if accepted == nil || accepted.Status != tt.wantAccepted {
				t.Fatalf("Accepted = %#v, want %s", accepted, tt.wantAccepted)
			}
			if resolved == nil || resolved.Status != tt.wantResolved {
				t.Fatalf("ResolvedRefs = %#v, want %s", resolved, tt.wantResolved)
			}
			if tt.wantReason != "" && accepted.Reason != tt.wantReason && resolved.Reason != tt.wantReason {
				t.Fatalf("reasons = %q/%q, want %q", accepted.Reason, resolved.Reason, tt.wantReason)
			}
			if len(updated.Status.Conditions) != 2 {
				t.Fatalf("conditions = %#v, want only Accepted and ResolvedRefs", updated.Status.Conditions)
			}
			statusJSON, err := json.Marshal(updated.Status)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(statusJSON), "s3cret-value") {
				t.Fatalf("status leaked client secret: %s", statusJSON)
			}
			// A second reconcile with no changes must not rewrite status.
			updates := 0
			counting := ctrlfake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(updated).WithStatusSubresource(&corev1alpha1.ConnectorProvider{}).
				WithRuntimeObjects(tt.objects...).
				WithInterceptorFuncs(interceptor.Funcs{SubResourceUpdate: func(ctx context.Context, cl ctrlclient.Client, sub string, obj ctrlclient.Object, opts ...ctrlclient.SubResourceUpdateOption) error {
					updates++
					return cl.SubResource(sub).Update(ctx, obj, opts...)
				}}).Build()
			reconciler = &ConnectorProviderReconciler{Client: counting, APIReader: counting, Scheme: scheme, KnownBuiltinTool: tt.knownBuiltin}
			if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
				t.Fatal(err)
			}
			if updates != 0 {
				t.Fatalf("steady-state reconcile wrote status %d times", updates)
			}
		})
	}
}

func TestConnectorProviderReconcilerMissingAndDeleting(t *testing.T) {
	scheme := connectorTestScheme(t)
	c := ctrlfake.NewClientBuilder().WithScheme(scheme).Build()
	reconciler := &ConnectorProviderReconciler{Client: c, Scheme: scheme}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "tenant", Name: "missing"}}); err != nil {
		t.Fatalf("missing provider must reconcile cleanly: %v", err)
	}
	deleting := testConnectorProvider("tenant", "github")
	now := metav1.Now()
	deleting.DeletionTimestamp = &now
	deleting.Finalizers = []string{"test.orka.ai/hold"}
	c = ctrlfake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(deleting).WithStatusSubresource(&corev1alpha1.ConnectorProvider{}).Build()
	reconciler = &ConnectorProviderReconciler{Client: c, Scheme: scheme}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "tenant", Name: "github"}}); err != nil {
		t.Fatal(err)
	}
	updated := &corev1alpha1.ConnectorProvider{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "tenant", Name: "github"}, updated); err != nil {
		t.Fatal(err)
	}
	if len(updated.Status.Conditions) != 0 {
		t.Fatalf("deleting provider must not gain conditions: %#v", updated.Status.Conditions)
	}
}

func TestConnectorProviderReconcilerReadError(t *testing.T) {
	scheme := connectorTestScheme(t)
	provider := testConnectorProvider("tenant", "github")
	c := ctrlfake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(provider).WithStatusSubresource(&corev1alpha1.ConnectorProvider{}).Build()
	failing := ctrlfake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, ctrlclient.WithWatch, ctrlclient.ObjectKey, ctrlclient.Object, ...ctrlclient.GetOption) error {
			return errors.New("apiserver unavailable")
		},
	}).Build()
	reconciler := &ConnectorProviderReconciler{Client: c, APIReader: failing, Scheme: scheme}
	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "tenant", Name: "github"}})
	if err == nil {
		t.Fatal("expected reconcile error")
	}
	updated := &corev1alpha1.ConnectorProvider{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "tenant", Name: "github"}, updated); err != nil {
		t.Fatal(err)
	}
	resolved := meta.FindStatusCondition(updated.Status.Conditions, corev1alpha1.ConnectorProviderConditionResolvedRefs)
	if resolved == nil || resolved.Status != metav1.ConditionUnknown || resolved.Reason != connectors.ReasonResolutionFailed {
		t.Fatalf("ResolvedRefs = %#v, want Unknown/ResolutionFailed", resolved)
	}
}

func TestConnectorProviderRequestsForSecret(t *testing.T) {
	scheme := connectorTestScheme(t)
	github := testConnectorProvider("tenant", "github")
	other := testConnectorProvider("tenant", "other")
	other.Spec.OAuth.ClientSecretRef.Name = "other-oauth"
	foreign := testConnectorProvider("elsewhere", "github")
	c := ctrlfake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(github, other, foreign).Build()
	reconciler := &ConnectorProviderReconciler{Client: c, Scheme: scheme}
	requests := reconciler.requestsForSecret(context.Background(), connectorClientSecret("tenant"))
	if len(requests) != 1 || requests[0].Name != "github" || requests[0].Namespace != "tenant" {
		t.Fatalf("requests = %#v, want only tenant/github", requests)
	}
}

func TestConnectorProviderDeletionWaitsForConnections(t *testing.T) {
	scheme := connectorTestScheme(t)
	provider := acceptedConnectorProvider()
	now := metav1.Now()
	provider.DeletionTimestamp = &now
	provider.Finalizers = []string{ConnectorProviderConnectionsFinalizer}
	connection := testConnection("tenant", "github-alice", "github")
	unrelated := testConnection("tenant", "other-bob", "other")
	c := ctrlfake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(provider, connection, unrelated, connectorClientSecret("tenant")).
		WithStatusSubresource(&corev1alpha1.ConnectorProvider{}).Build()
	reconciler := &ConnectorProviderReconciler{Client: c, Scheme: scheme}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "tenant", Name: "github"}}
	// A Connection still references the provider: deletion is held and the
	// condition says why, so the tokens it holds can still be revoked
	// against the issuing client at disconnect.
	result, err := reconciler.Reconcile(context.Background(), request)
	if err != nil || result.RequeueAfter == 0 {
		t.Fatalf("held deletion: result = %+v err = %v", result, err)
	}
	held := &corev1alpha1.ConnectorProvider{}
	if err := c.Get(context.Background(), request.NamespacedName, held); err != nil {
		t.Fatal(err)
	}
	accepted := meta.FindStatusCondition(held.Status.Conditions, corev1alpha1.ConnectorProviderConditionAccepted)
	if !controllerutil.ContainsFinalizer(held, ConnectorProviderConnectionsFinalizer) || accepted == nil || accepted.Reason != connectors.ReasonConnectionsRemain {
		t.Fatalf("held provider = finalizers %v condition %+v", held.Finalizers, accepted)
	}
	// Once the last referencing Connection is gone the provider is released;
	// a Connection to another provider does not hold it.
	if err := c.Delete(context.Background(), connection); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	released := &corev1alpha1.ConnectorProvider{}
	if err := c.Get(context.Background(), request.NamespacedName, released); err == nil && controllerutil.ContainsFinalizer(released, ConnectorProviderConnectionsFinalizer) {
		t.Fatalf("released provider still holds the finalizer: %v", released.Finalizers)
	}
}
