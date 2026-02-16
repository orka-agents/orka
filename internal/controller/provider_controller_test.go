/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
)

func newProviderScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(s)
	_ = corev1.AddToScheme(s)
	return s
}

// ---------- ValidationError.Error ----------

func TestValidationError_Error(t *testing.T) {
	e := &ValidationError{Message: "something broke"}
	if got := e.Error(); got != "something broke" {
		t.Errorf("Error() = %q, want %q", got, "something broke")
	}
}

// ---------- validateProvider ----------

func TestValidateProvider(t *testing.T) {
	tests := []struct {
		name      string
		provider  *corev1alpha1.Provider
		secrets   []*corev1.Secret
		wantErr   bool
		errSubstr string
	}{
		{
			name: "valid openai provider with default key",
			provider: &corev1alpha1.Provider{
				ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default"},
				Spec: corev1alpha1.ProviderSpec{
					Type:      corev1alpha1.ProviderTypeOpenAI,
					SecretRef: corev1alpha1.ProviderSecretRef{Name: "my-secret"},
				},
			},
			secrets: []*corev1.Secret{{
				ObjectMeta: metav1.ObjectMeta{Name: "my-secret", Namespace: "default"},
				Data:       map[string][]byte{"api-key": []byte("sk-123")},
			}},
		},
		{
			name: "valid openai provider with custom key",
			provider: &corev1alpha1.Provider{
				ObjectMeta: metav1.ObjectMeta{Name: "p2", Namespace: "default"},
				Spec: corev1alpha1.ProviderSpec{
					Type:      corev1alpha1.ProviderTypeOpenAI,
					SecretRef: corev1alpha1.ProviderSecretRef{Name: "my-secret", Key: "custom-key"},
				},
			},
			secrets: []*corev1.Secret{{
				ObjectMeta: metav1.ObjectMeta{Name: "my-secret", Namespace: "default"},
				Data:       map[string][]byte{"custom-key": []byte("sk-456")},
			}},
		},
		{
			name: "secret not found",
			provider: &corev1alpha1.Provider{
				ObjectMeta: metav1.ObjectMeta{Name: "p3", Namespace: "default"},
				Spec: corev1alpha1.ProviderSpec{
					Type:      corev1alpha1.ProviderTypeOpenAI,
					SecretRef: corev1alpha1.ProviderSecretRef{Name: "missing-secret"},
				},
			},
			wantErr:   true,
			errSubstr: "referenced secret not found",
		},
		{
			name: "key not found in secret (default api-key)",
			provider: &corev1alpha1.Provider{
				ObjectMeta: metav1.ObjectMeta{Name: "p4", Namespace: "default"},
				Spec: corev1alpha1.ProviderSpec{
					Type:      corev1alpha1.ProviderTypeOpenAI,
					SecretRef: corev1alpha1.ProviderSecretRef{Name: "my-secret"},
				},
			},
			secrets: []*corev1.Secret{{
				ObjectMeta: metav1.ObjectMeta{Name: "my-secret", Namespace: "default"},
				Data:       map[string][]byte{"wrong-key": []byte("val")},
			}},
			wantErr:   true,
			errSubstr: "key 'api-key' not found in secret",
		},
		{
			name: "key not found in secret (custom key)",
			provider: &corev1alpha1.Provider{
				ObjectMeta: metav1.ObjectMeta{Name: "p5", Namespace: "default"},
				Spec: corev1alpha1.ProviderSpec{
					Type:      corev1alpha1.ProviderTypeOpenAI,
					SecretRef: corev1alpha1.ProviderSecretRef{Name: "my-secret", Key: "token"},
				},
			},
			secrets: []*corev1.Secret{{
				ObjectMeta: metav1.ObjectMeta{Name: "my-secret", Namespace: "default"},
				Data:       map[string][]byte{"other": []byte("val")},
			}},
			wantErr:   true,
			errSubstr: "key 'token' not found in secret",
		},
		{
			name: "azure-openai missing deploymentName",
			provider: &corev1alpha1.Provider{
				ObjectMeta: metav1.ObjectMeta{Name: "p6", Namespace: "default"},
				Spec: corev1alpha1.ProviderSpec{
					Type:      corev1alpha1.ProviderTypeAzureOpenAI,
					SecretRef: corev1alpha1.ProviderSecretRef{Name: "my-secret"},
					BaseURL:   "https://my.azure.com",
				},
			},
			secrets: []*corev1.Secret{{
				ObjectMeta: metav1.ObjectMeta{Name: "my-secret", Namespace: "default"},
				Data:       map[string][]byte{"api-key": []byte("key")},
			}},
			wantErr:   true,
			errSubstr: "azure.deploymentName is required",
		},
		{
			name: "azure-openai missing baseURL",
			provider: &corev1alpha1.Provider{
				ObjectMeta: metav1.ObjectMeta{Name: "p7", Namespace: "default"},
				Spec: corev1alpha1.ProviderSpec{
					Type:      corev1alpha1.ProviderTypeAzureOpenAI,
					SecretRef: corev1alpha1.ProviderSecretRef{Name: "my-secret"},
					Azure:     &corev1alpha1.AzureConfig{DeploymentName: "gpt4"},
				},
			},
			secrets: []*corev1.Secret{{
				ObjectMeta: metav1.ObjectMeta{Name: "my-secret", Namespace: "default"},
				Data:       map[string][]byte{"api-key": []byte("key")},
			}},
			wantErr:   true,
			errSubstr: "baseURL is required for azure-openai",
		},
		{
			name: "azure-openai nil azure config",
			provider: &corev1alpha1.Provider{
				ObjectMeta: metav1.ObjectMeta{Name: "p8", Namespace: "default"},
				Spec: corev1alpha1.ProviderSpec{
					Type:      corev1alpha1.ProviderTypeAzureOpenAI,
					SecretRef: corev1alpha1.ProviderSecretRef{Name: "my-secret"},
					BaseURL:   "https://my.azure.com",
				},
			},
			secrets: []*corev1.Secret{{
				ObjectMeta: metav1.ObjectMeta{Name: "my-secret", Namespace: "default"},
				Data:       map[string][]byte{"api-key": []byte("key")},
			}},
			wantErr:   true,
			errSubstr: "azure.deploymentName is required",
		},
		{
			name: "valid azure-openai provider",
			provider: &corev1alpha1.Provider{
				ObjectMeta: metav1.ObjectMeta{Name: "p9", Namespace: "default"},
				Spec: corev1alpha1.ProviderSpec{
					Type:      corev1alpha1.ProviderTypeAzureOpenAI,
					SecretRef: corev1alpha1.ProviderSecretRef{Name: "my-secret"},
					BaseURL:   "https://my.azure.com",
					Azure:     &corev1alpha1.AzureConfig{DeploymentName: "gpt4"},
				},
			},
			secrets: []*corev1.Secret{{
				ObjectMeta: metav1.ObjectMeta{Name: "my-secret", Namespace: "default"},
				Data:       map[string][]byte{"api-key": []byte("key")},
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := newProviderScheme()
			cb := fake.NewClientBuilder().WithScheme(scheme)
			for _, s := range tt.secrets {
				cb = cb.WithObjects(s)
			}
			r := &ProviderReconciler{Client: cb.Build(), Scheme: scheme}

			err := r.validateProvider(context.Background(), tt.provider)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if tt.errSubstr != "" && !contains(err.Error(), tt.errSubstr) {
					t.Errorf("error %q should contain %q", err.Error(), tt.errSubstr)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// ---------- Reconcile ----------

func TestProviderReconcile(t *testing.T) {
	tests := []struct {
		name       string
		provider   *corev1alpha1.Provider
		secrets    []*corev1.Secret
		wantReady  bool
		wantMsgSub string
	}{
		{
			name: "valid provider becomes ready",
			provider: &corev1alpha1.Provider{
				ObjectMeta: metav1.ObjectMeta{Name: "prov-ok", Namespace: "default"},
				Spec: corev1alpha1.ProviderSpec{
					Type:      corev1alpha1.ProviderTypeOpenAI,
					SecretRef: corev1alpha1.ProviderSecretRef{Name: "sec-ok"},
				},
			},
			secrets: []*corev1.Secret{{
				ObjectMeta: metav1.ObjectMeta{Name: "sec-ok", Namespace: "default"},
				Data:       map[string][]byte{"api-key": []byte("k")},
			}},
			wantReady:  true,
			wantMsgSub: "valid",
		},
		{
			name: "missing secret marks not ready",
			provider: &corev1alpha1.Provider{
				ObjectMeta: metav1.ObjectMeta{Name: "prov-miss", Namespace: "default"},
				Spec: corev1alpha1.ProviderSpec{
					Type:      corev1alpha1.ProviderTypeOpenAI,
					SecretRef: corev1alpha1.ProviderSecretRef{Name: "no-secret"},
				},
			},
			wantReady:  false,
			wantMsgSub: "referenced secret not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := newProviderScheme()
			cb := fake.NewClientBuilder().WithScheme(scheme).
				WithStatusSubresource(&corev1alpha1.Provider{})
			if tt.provider != nil {
				cb = cb.WithObjects(tt.provider)
			}
			for _, s := range tt.secrets {
				cb = cb.WithObjects(s)
			}
			cl := cb.Build()
			r := &ProviderReconciler{Client: cl, Scheme: scheme}

			nn := types.NamespacedName{Name: tt.provider.Name, Namespace: tt.provider.Namespace}
			_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: nn})
			if err != nil {
				t.Fatalf("Reconcile returned error: %v", err)
			}

			// Re-fetch provider
			got := &corev1alpha1.Provider{}
			if err := cl.Get(context.Background(), nn, got); err != nil {
				t.Fatalf("failed to get provider: %v", err)
			}
			if got.Status.Ready != tt.wantReady {
				t.Errorf("Status.Ready = %v, want %v", got.Status.Ready, tt.wantReady)
			}
			if tt.wantMsgSub != "" && !contains(got.Status.Message, tt.wantMsgSub) {
				t.Errorf("Status.Message = %q, want substring %q", got.Status.Message, tt.wantMsgSub)
			}
			if tt.wantReady && got.Status.LastValidated == nil {
				t.Error("Status.LastValidated should be set for ready provider")
			}
			if !tt.wantReady && got.Status.LastValidated != nil {
				t.Error("Status.LastValidated should be nil for not-ready provider")
			}
		})
	}
}

func TestProviderReconcile_NotFound(t *testing.T) {
	scheme := newProviderScheme()
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &ProviderReconciler{Client: cl, Scheme: scheme}

	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "gone", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("expected no error for not-found provider, got: %v", err)
	}
}

// ---------- updateStatus ----------

func TestProviderUpdateStatus(t *testing.T) {
	tests := []struct {
		name    string
		ready   bool
		message string
	}{
		{"ready status", true, "Provider configuration is valid"},
		{"not ready status", false, "validation failed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := newProviderScheme()
			provider := &corev1alpha1.Provider{
				ObjectMeta: metav1.ObjectMeta{Name: "s-prov", Namespace: "default"},
				Spec: corev1alpha1.ProviderSpec{
					Type:      corev1alpha1.ProviderTypeOpenAI,
					SecretRef: corev1alpha1.ProviderSecretRef{Name: "s"},
				},
			}
			cl := fake.NewClientBuilder().WithScheme(scheme).
				WithObjects(provider).
				WithStatusSubresource(&corev1alpha1.Provider{}).Build()
			r := &ProviderReconciler{Client: cl, Scheme: scheme}

			_, err := r.updateStatus(context.Background(), provider, tt.ready, tt.message)
			if err != nil {
				t.Fatalf("updateStatus error: %v", err)
			}

			got := &corev1alpha1.Provider{}
			if err := cl.Get(context.Background(), types.NamespacedName{Name: "s-prov", Namespace: "default"}, got); err != nil {
				t.Fatalf("get error: %v", err)
			}
			if got.Status.Ready != tt.ready {
				t.Errorf("Ready = %v, want %v", got.Status.Ready, tt.ready)
			}
			if got.Status.Message != tt.message {
				t.Errorf("Message = %q, want %q", got.Status.Message, tt.message)
			}
			if len(got.Status.Conditions) == 0 {
				t.Fatal("expected at least one condition")
			}
			cond := got.Status.Conditions[0]
			if cond.Type != "Ready" {
				t.Errorf("condition type = %q, want Ready", cond.Type)
			}
			if tt.ready && cond.Reason != "ValidationSucceeded" {
				t.Errorf("condition reason = %q, want ValidationSucceeded", cond.Reason)
			}
			if !tt.ready && cond.Reason != "ValidationFailed" {
				t.Errorf("condition reason = %q, want ValidationFailed", cond.Reason)
			}
		})
	}
}

// ---------- SetupWithManager ----------

func TestProviderSetupWithManager_NilManager(t *testing.T) {
	r := &ProviderReconciler{}
	err := r.SetupWithManager(nil)
	if err == nil {
		t.Error("expected error when passing nil manager")
	}
}

// ---------- helpers ----------

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
