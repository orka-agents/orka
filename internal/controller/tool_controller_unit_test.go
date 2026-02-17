/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
)

func newToolScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(s)
	_ = corev1.AddToScheme(s)
	return s
}

// ---------- validateTool ----------

func TestValidateTool(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "auth-secret", Namespace: "default"},
		Data:       map[string][]byte{"token": []byte("t")},
	}

	tests := []struct {
		name      string
		tool      *corev1alpha1.Tool
		objects   []runtime.Object
		wantErr   bool
		errSubstr string
	}{
		{
			name: "valid tool without auth",
			tool: &corev1alpha1.Tool{
				ObjectMeta: metav1.ObjectMeta{Name: "t1", Namespace: "default"},
				Spec: corev1alpha1.ToolSpec{
					Description: "A tool",
					HTTP:        corev1alpha1.HTTPExecution{URL: "http://example.com/api"},
				},
			},
		},
		{
			name: "missing URL",
			tool: &corev1alpha1.Tool{
				ObjectMeta: metav1.ObjectMeta{Name: "t2", Namespace: "default"},
				Spec: corev1alpha1.ToolSpec{
					Description: "A tool",
					HTTP:        corev1alpha1.HTTPExecution{URL: ""},
				},
			},
			wantErr:   true,
			errSubstr: "http.url is required",
		},
		{
			name: "invalid URL",
			tool: &corev1alpha1.Tool{
				ObjectMeta: metav1.ObjectMeta{Name: "t3", Namespace: "default"},
				Spec: corev1alpha1.ToolSpec{
					Description: "A tool",
					HTTP:        corev1alpha1.HTTPExecution{URL: "not-a-url"},
				},
			},
			wantErr:   true,
			errSubstr: "invalid http.url",
		},
		{
			name: "missing description",
			tool: &corev1alpha1.Tool{
				ObjectMeta: metav1.ObjectMeta{Name: "t4", Namespace: "default"},
				Spec: corev1alpha1.ToolSpec{
					Description: "",
					HTTP:        corev1alpha1.HTTPExecution{URL: "http://example.com/api"},
				},
			},
			wantErr:   true,
			errSubstr: "description is required",
		},
		{
			name: "auth secret not found",
			tool: &corev1alpha1.Tool{
				ObjectMeta: metav1.ObjectMeta{Name: "t5", Namespace: "default"},
				Spec: corev1alpha1.ToolSpec{
					Description: "A tool",
					HTTP: corev1alpha1.HTTPExecution{
						URL:           "http://example.com/api",
						AuthSecretRef: &corev1alpha1.SecretKeySelector{Name: "missing", Key: "k"},
					},
				},
			},
			wantErr:   true,
			errSubstr: "referenced auth secret",
		},
		{
			name: "auth secret key not found",
			tool: &corev1alpha1.Tool{
				ObjectMeta: metav1.ObjectMeta{Name: "t6", Namespace: "default"},
				Spec: corev1alpha1.ToolSpec{
					Description: "A tool",
					HTTP: corev1alpha1.HTTPExecution{
						URL:           "http://example.com/api",
						AuthSecretRef: &corev1alpha1.SecretKeySelector{Name: "auth-secret", Key: "wrong-key"},
					},
				},
			},
			wantErr:   true,
			errSubstr: "key \"wrong-key\" not found",
		},
		{
			name: "valid tool with auth secret",
			tool: &corev1alpha1.Tool{
				ObjectMeta: metav1.ObjectMeta{Name: "t7", Namespace: "default"},
				Spec: corev1alpha1.ToolSpec{
					Description: "A tool",
					HTTP: corev1alpha1.HTTPExecution{
						URL:           "http://example.com/api",
						AuthSecretRef: &corev1alpha1.SecretKeySelector{Name: "auth-secret", Key: "token"},
					},
				},
			},
		},
		{
			name: "authInject body without authBodyKey",
			tool: &corev1alpha1.Tool{
				ObjectMeta: metav1.ObjectMeta{Name: "t8", Namespace: "default"},
				Spec: corev1alpha1.ToolSpec{
					Description: "A tool",
					HTTP: corev1alpha1.HTTPExecution{
						URL:        "http://example.com/api",
						AuthInject: "body",
					},
				},
			},
			wantErr:   true,
			errSubstr: "authBodyKey is required",
		},
		{
			name: "authInject body with authBodyKey is valid",
			tool: &corev1alpha1.Tool{
				ObjectMeta: metav1.ObjectMeta{Name: "t9", Namespace: "default"},
				Spec: corev1alpha1.ToolSpec{
					Description: "A tool",
					HTTP: corev1alpha1.HTTPExecution{
						URL:         "http://example.com/api",
						AuthInject:  "body",
						AuthBodyKey: "api_key",
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := newToolScheme()
			cb := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret.DeepCopy())
			r := &ToolReconciler{Client: cb.Build(), Scheme: scheme}

			err := r.validateTool(context.Background(), tt.tool)
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

// ---------- healthCheck ----------

func TestHealthCheck(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		wantErr bool
	}{
		{
			name:    "reachable 200",
			handler: func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) },
		},
		{
			name:    "reachable 500 still succeeds",
			handler: func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) },
		},
		{
			name:    "reachable 404 still succeeds",
			handler: func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(tt.handler)
			defer srv.Close()

			scheme := newToolScheme()
			r := &ToolReconciler{
				Client:     fake.NewClientBuilder().WithScheme(scheme).Build(),
				Scheme:     scheme,
				HTTPClient: srv.Client(),
			}

			tool := &corev1alpha1.Tool{
				Spec: corev1alpha1.ToolSpec{
					HTTP: corev1alpha1.HTTPExecution{URL: srv.URL},
				},
			}

			err := r.healthCheck(context.Background(), tool)
			if tt.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestHealthCheck_Unreachable(t *testing.T) {
	scheme := newToolScheme()
	r := &ToolReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).Build(),
		Scheme: scheme,
		HTTPClient: &http.Client{
			Transport: &http.Transport{},
		},
	}
	tool := &corev1alpha1.Tool{
		Spec: corev1alpha1.ToolSpec{
			HTTP: corev1alpha1.HTTPExecution{URL: "http://127.0.0.1:1/unreachable"},
		},
	}

	err := r.healthCheck(context.Background(), tool)
	if err == nil {
		t.Fatal("expected error for unreachable endpoint")
	}
	if !contains(err.Error(), "endpoint unreachable") {
		t.Errorf("error %q should contain 'endpoint unreachable'", err.Error())
	}
}

// ---------- getHTTPClient ----------

func TestGetHTTPClient(t *testing.T) {
	t.Run("returns injected client", func(t *testing.T) {
		custom := &http.Client{}
		r := &ToolReconciler{HTTPClient: custom}
		got := r.getHTTPClient()
		if got != custom {
			t.Error("expected injected client to be returned")
		}
	})

	t.Run("returns default client when nil", func(t *testing.T) {
		r := &ToolReconciler{}
		got := r.getHTTPClient()
		if got == nil {
			t.Fatal("expected non-nil default client")
		}
		if got.Timeout != toolHealthCheckTimeout {
			t.Errorf("timeout = %v, want %v", got.Timeout, toolHealthCheckTimeout)
		}
	})
}

// ---------- updateStatus ----------

func TestToolUpdateStatus(t *testing.T) {
	tests := []struct {
		name       string
		available  bool
		errMsg     string
		wantReason string
	}{
		{"available", true, "", "EndpointReachable"},
		{"unavailable", false, "endpoint unreachable", "EndpointUnreachable"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := newToolScheme()
			tool := &corev1alpha1.Tool{
				ObjectMeta: metav1.ObjectMeta{Name: "st", Namespace: "default"},
				Spec: corev1alpha1.ToolSpec{
					Description: "test",
					HTTP:        corev1alpha1.HTTPExecution{URL: "http://example.com"},
				},
			}
			cl := fake.NewClientBuilder().WithScheme(scheme).
				WithObjects(tool).
				WithStatusSubresource(&corev1alpha1.Tool{}).Build()
			r := &ToolReconciler{Client: cl, Scheme: scheme}

			result, err := r.updateStatus(context.Background(), tool, tt.available, tt.errMsg)
			if err != nil {
				t.Fatalf("updateStatus error: %v", err)
			}
			if result.RequeueAfter != toolHealthCheckInterval {
				t.Errorf("RequeueAfter = %v, want %v", result.RequeueAfter, toolHealthCheckInterval)
			}

			got := &corev1alpha1.Tool{}
			if err := cl.Get(context.Background(), types.NamespacedName{Name: "st", Namespace: "default"}, got); err != nil {
				t.Fatalf("get error: %v", err)
			}
			if got.Status.Available != tt.available {
				t.Errorf("Available = %v, want %v", got.Status.Available, tt.available)
			}
			if got.Status.Error != tt.errMsg {
				t.Errorf("Error = %q, want %q", got.Status.Error, tt.errMsg)
			}
			if got.Status.LastCheck == nil {
				t.Error("LastCheck should be set")
			}
			if len(got.Status.Conditions) == 0 {
				t.Fatal("expected at least one condition")
			}
			if got.Status.Conditions[0].Reason != tt.wantReason {
				t.Errorf("condition reason = %q, want %q", got.Status.Conditions[0].Reason, tt.wantReason)
			}
		})
	}
}
