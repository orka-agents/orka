/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/approvals"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/outboundaccess"
	"github.com/orka-agents/orka/internal/tokenexchange"
)

func TestOutboundAccessPolicyReconcilerConditions(t *testing.T) {
	tests := []struct {
		name         string
		policy       *corev1alpha1.OutboundAccessPolicy
		objects      []runtime.Object
		wantAccepted metav1.ConditionStatus
		wantResolved metav1.ConditionStatus
	}{
		{
			name: "invalid adapters",
			policy: &corev1alpha1.OutboundAccessPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: "policy", Namespace: "tenant", Generation: 1},
			},
			wantAccepted: metav1.ConditionFalse,
			wantResolved: metav1.ConditionFalse,
		},
		{
			name:         "missing secret",
			policy:       controllerDirectPolicy("tenant", "policy", "subject"),
			wantAccepted: metav1.ConditionTrue,
			wantResolved: metav1.ConditionFalse,
		},
		{
			name:   "resolved",
			policy: controllerDirectPolicy("tenant", "policy", "subject"),
			objects: []runtime.Object{&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "subject", Namespace: "tenant"},
				Data:       map[string][]byte{"token": []byte("assertion")},
			}},
			wantAccepted: metav1.ConditionTrue,
			wantResolved: metav1.ConditionTrue,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			if err := corev1alpha1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			objects := []runtime.Object{tt.policy}
			objects = append(objects, tt.objects...)
			client := ctrlfake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objects...).WithStatusSubresource(&corev1alpha1.OutboundAccessPolicy{}).Build()
			reconciler := &OutboundAccessPolicyReconciler{Client: client, Scheme: scheme}
			if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "tenant", Name: "policy"}}); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			updated := &corev1alpha1.OutboundAccessPolicy{}
			if err := client.Get(context.Background(), types.NamespacedName{Namespace: "tenant", Name: "policy"}, updated); err != nil {
				t.Fatal(err)
			}
			if updated.Status.ObservedGeneration != updated.Generation {
				t.Fatalf("observedGeneration = %d, want %d", updated.Status.ObservedGeneration, updated.Generation)
			}
			accepted := meta.FindStatusCondition(updated.Status.Conditions, corev1alpha1.OutboundAccessPolicyConditionAccepted)
			resolved := meta.FindStatusCondition(updated.Status.Conditions, corev1alpha1.OutboundAccessPolicyConditionResolvedRefs)
			if accepted == nil || accepted.Status != tt.wantAccepted {
				t.Fatalf("Accepted = %#v, want %s", accepted, tt.wantAccepted)
			}
			if resolved == nil || resolved.Status != tt.wantResolved {
				t.Fatalf("ResolvedRefs = %#v, want %s", resolved, tt.wantResolved)
			}
			if len(updated.Status.Conditions) != 2 {
				t.Fatalf("conditions = %#v, want only Accepted and ResolvedRefs", updated.Status.Conditions)
			}
			statusJSON, err := json.Marshal(updated.Status)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(statusJSON), "assertion") || strings.Contains(string(statusJSON), "https://issuer.example.test") {
				t.Fatalf("status leaked credential or endpoint: %s", statusJSON)
			}
		})
	}
}

func controllerDirectPolicy(namespace, name, secretName string) *corev1alpha1.OutboundAccessPolicy {
	return &corev1alpha1.OutboundAccessPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Generation: 1},
		Spec: corev1alpha1.OutboundAccessPolicySpec{Direct: &corev1alpha1.DirectOutboundAccess{
			Grant:         corev1alpha1.OutboundGrantTokenExchange,
			TokenEndpoint: corev1alpha1.OutboundTokenEndpoint{URL: "https://issuer.example.test/token"},
			Subject: corev1alpha1.OutboundTokenSource{
				Source:    corev1alpha1.OutboundTokenSourceSecretRef,
				TokenType: tokenexchange.TokenTypeAccessToken,
				SecretRef: &corev1alpha1.NamespacedSecretKeySelector{Name: secretName, Key: "token"},
			},
			ExpectedIssuedTokenType: tokenexchange.TokenTypeAccessToken,
		}},
	}
}

func TestOutboundAccessPolicyReferenceMapping(t *testing.T) {
	policy := controllerDirectPolicy("tenant", "policy", "subject")
	if !outboundPolicyReferencesObject(policy, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "subject", Namespace: "tenant"}}) {
		t.Fatal("policy did not map its subject Secret")
	}
	if outboundPolicyReferencesObject(policy, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "subject", Namespace: "other"}}) {
		t.Fatal("policy mapped a cross-namespace Secret")
	}

	gateway := &corev1alpha1.OutboundAccessPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "gateway", Namespace: "tenant"},
		Spec: corev1alpha1.OutboundAccessPolicySpec{Gateway: &corev1alpha1.GatewayOutboundAccess{
			ServiceRef: corev1alpha1.OutboundServiceReference{Name: "agentgateway", Namespace: "infra", Port: 8080},
		}},
	}
	if !outboundPolicyReferencesObject(gateway, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "agentgateway", Namespace: "infra"}}) {
		t.Fatal("gateway policy did not map cross-namespace Service")
	}

	trusted, err := outboundaccess.ParseTrustedServiceReferences("infra/agentgateway:8080")
	if err != nil || !trusted.Allows(gateway.Spec.Gateway.ServiceRef, gateway.Namespace) {
		t.Fatalf("trusted gateway parse = %#v, %v", trusted, err)
	}
}

type outboundPolicyRoundTripFunc func(*http.Request) (*http.Response, error)

func (f outboundPolicyRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestToolReconcilerGatewayPolicySkipsDirectHealthCheck(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	gateway := readyControllerPolicy("tenant", "gateway", corev1alpha1.OutboundAccessPolicySpec{
		Gateway: &corev1alpha1.GatewayOutboundAccess{},
	})
	client := ctrlfake.NewClientBuilder().WithScheme(scheme).WithObjects(gateway).Build()
	called := false
	reconciler := &ToolReconciler{
		Client: client,
		HTTPClient: &http.Client{Transport: outboundPolicyRoundTripFunc(func(*http.Request) (*http.Response, error) {
			called = true
			return nil, context.Canceled
		})},
	}
	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "gateway-tool", Namespace: "tenant"},
		Spec: corev1alpha1.ToolSpec{HTTP: &corev1alpha1.HTTPExecution{
			URL:                     "https://downstream.example.test/resource",
			OutboundAccessPolicyRef: &corev1alpha1.LocalObjectReference{Name: gateway.Name},
		}},
	}
	if err := reconciler.healthCheck(context.Background(), tool); err != nil {
		t.Fatalf("healthCheck() error = %v", err)
	}
	if called {
		t.Fatal("gateway-backed Tool health check called the original downstream URL")
	}
}

func TestToolReconcilerOutboundAccessPolicyAuthCompatibility(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	direct := readyControllerPolicy("tenant", "direct", corev1alpha1.OutboundAccessPolicySpec{Direct: &corev1alpha1.DirectOutboundAccess{}})
	gateway := readyControllerPolicy("tenant", "gateway", corev1alpha1.OutboundAccessPolicySpec{Gateway: &corev1alpha1.GatewayOutboundAccess{}})
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "auth", Namespace: "tenant"}, Data: map[string][]byte{"token": []byte("credential")}}
	client := ctrlfake.NewClientBuilder().WithScheme(scheme).WithObjects(direct, gateway, secret).Build()
	reconciler := &ToolReconciler{Client: client, Scheme: scheme}

	tool := &corev1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{Name: "tool", Namespace: "tenant"}, Spec: corev1alpha1.ToolSpec{HTTP: &corev1alpha1.HTTPExecution{
		AuthSecretRef:           &corev1alpha1.SecretKeySelector{Name: "auth", Key: "token"},
		OutboundAccessPolicyRef: &corev1alpha1.LocalObjectReference{Name: "direct"},
	}}}
	if err := reconciler.validateToolHTTPAuth(context.Background(), tool); err == nil || !strings.Contains(strings.ToLower(err.Error()), "cannot coexist") {
		t.Fatalf("direct validateToolHTTPAuth() error = %v", err)
	}
	tool.Spec.HTTP.OutboundAccessPolicyRef.Name = "gateway"
	if err := reconciler.validateToolHTTPAuth(context.Background(), tool); err != nil {
		t.Fatalf("gateway validateToolHTTPAuth() error = %v", err)
	}
}

func readyControllerPolicy(namespace, name string, spec corev1alpha1.OutboundAccessPolicySpec) *corev1alpha1.OutboundAccessPolicy {
	generation := int64(2)
	return &corev1alpha1.OutboundAccessPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Generation: generation},
		Spec:       spec,
		Status: corev1alpha1.OutboundAccessPolicyStatus{
			ObservedGeneration: generation,
			Conditions: []metav1.Condition{
				{Type: corev1alpha1.OutboundAccessPolicyConditionAccepted, Status: metav1.ConditionTrue, ObservedGeneration: generation},
				{Type: corev1alpha1.OutboundAccessPolicyConditionResolvedRefs, Status: metav1.ConditionTrue, ObservedGeneration: generation},
			},
		},
	}
}

func TestHarnessBrokeredTransactionAuthorityReadsTaskOwnedSecret(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "remote-task",
			Namespace: "tenant",
			UID:       types.UID("task-uid"),
			Annotations: map[string]string{
				labels.AnnotationTransactionTokenSecret: "task-transaction",
			},
		},
		Spec: corev1alpha1.TaskSpec{Transaction: &corev1alpha1.TaskTransaction{
			Scope:  "api.read api.write",
			Scopes: []string{"api.read", "api.write"},
		}},
	}
	controller := true
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "task-transaction",
			Namespace: "tenant",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: corev1alpha1.GroupVersion.String(),
				Kind:       "Task",
				Name:       task.Name,
				UID:        task.UID,
				Controller: &controller,
			}},
		},
		Data: map[string][]byte{"token": []byte("task-scoped-token")},
	}
	client := ctrlfake.NewClientBuilder().WithScheme(scheme).WithObjects(task, secret).Build()
	reconciler := &TaskReconciler{Client: client, APIReader: client}
	token, scopes, err := reconciler.harnessBrokeredTransactionAuthority(context.Background(), task)
	if err != nil {
		t.Fatalf("harnessBrokeredTransactionAuthority() error = %v", err)
	}
	if token != "task-scoped-token" || len(scopes) != 2 {
		t.Fatalf("authority = token %q scopes %#v", token, scopes)
	}

	secret.OwnerReferences[0].UID = types.UID("other-task")
	if err := client.Update(context.Background(), secret); err != nil {
		t.Fatal(err)
	}
	if _, _, err := reconciler.harnessBrokeredTransactionAuthority(context.Background(), task); err == nil {
		t.Fatal("unowned transaction authority Secret was accepted")
	}
}

func TestHarnessBrokeredTransactionAuthorityIdentityChangesOnSecretRotation(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "remote-task",
			Namespace: "tenant",
			UID:       types.UID("task-uid"),
			Annotations: map[string]string{
				labels.AnnotationTransactionTokenSecret: "task-transaction",
			},
		},
		Spec: corev1alpha1.TaskSpec{Transaction: &corev1alpha1.TaskTransaction{
			ID:            "txn-1",
			Scope:         "api.read",
			Scopes:        []string{"api.read"},
			ContextDigest: "sha256:context",
		}},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "task-transaction",
			Namespace: "tenant",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: corev1alpha1.GroupVersion.String(), Kind: "Task", Name: task.Name, UID: task.UID,
			}},
		},
		Data: map[string][]byte{"token": []byte("token-a")},
	}
	client := ctrlfake.NewClientBuilder().WithScheme(scheme).WithObjects(task, secret).Build()
	reconciler := &TaskReconciler{Client: client, APIReader: client}
	first, err := reconciler.harnessBrokeredTransactionAuthorityIdentity(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	firstDigest, err := approvals.TargetSpecDigest(first)
	if err != nil {
		t.Fatal(err)
	}
	secret.Data["token"] = []byte("token-b")
	if err := client.Update(context.Background(), secret); err != nil {
		t.Fatal(err)
	}
	second, err := reconciler.harnessBrokeredTransactionAuthorityIdentity(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	secondDigest, err := approvals.TargetSpecDigest(second)
	if err != nil {
		t.Fatal(err)
	}
	if firstDigest == secondDigest {
		t.Fatal("transaction authority Secret rotation did not change brokered approval identity")
	}
}

func TestHarnessBrokeredOutboundPolicyIdentityChangesOnServiceAccountRecreation(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	policy := &corev1alpha1.OutboundAccessPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name: "resource-api", Namespace: "tenant", UID: types.UID("policy-uid"), ResourceVersion: "10", Generation: 2,
		},
		Spec: corev1alpha1.OutboundAccessPolicySpec{Direct: &corev1alpha1.DirectOutboundAccess{
			Subject: corev1alpha1.OutboundTokenSource{
				Source:            corev1alpha1.OutboundTokenSourceServiceAccount,
				ServiceAccountRef: &corev1alpha1.OutboundServiceAccountReference{Name: "workload"},
			},
		}},
	}
	serviceAccount := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: "workload", Namespace: "tenant", UID: types.UID("service-account-a"), ResourceVersion: "1"},
	}
	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "dispatch", Namespace: "tenant"},
		Spec: corev1alpha1.ToolSpec{HTTP: &corev1alpha1.HTTPExecution{
			OutboundAccessPolicyRef: &corev1alpha1.LocalObjectReference{Name: policy.Name},
		}},
	}
	client := ctrlfake.NewClientBuilder().WithScheme(scheme).WithObjects(policy, serviceAccount).Build()
	reconciler := &TaskReconciler{Client: client, APIReader: client}
	first, err := reconciler.harnessBrokeredOutboundPolicyIdentity(context.Background(), tool)
	if err != nil {
		t.Fatal(err)
	}
	firstDigest, err := approvals.TargetSpecDigest(first)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Delete(context.Background(), serviceAccount); err != nil {
		t.Fatal(err)
	}
	replacement := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: "workload", Namespace: "tenant", UID: types.UID("service-account-b")},
	}
	if err := client.Create(context.Background(), replacement); err != nil {
		t.Fatal(err)
	}
	second, err := reconciler.harnessBrokeredOutboundPolicyIdentity(context.Background(), tool)
	if err != nil {
		t.Fatal(err)
	}
	secondDigest, err := approvals.TargetSpecDigest(second)
	if err != nil {
		t.Fatal(err)
	}
	if firstDigest == secondDigest {
		t.Fatal("ServiceAccount recreation did not change brokered approval identity")
	}
}
