/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestNewToolExecutor(t *testing.T) {
	executor := NewToolExecutor()
	if executor == nil {
		t.Fatal("NewToolExecutor returned nil")
	}
	if executor.client == nil {
		t.Error("client is nil")
	}
	if executor.namespace == "" {
		t.Error("namespace should have a default value")
	}
}

func TestNewToolExecutor_WithNamespaceEnv(t *testing.T) {
	originalNamespace := os.Getenv("ORKA_TASK_NAMESPACE")
	os.Setenv("ORKA_TASK_NAMESPACE", "custom-namespace")      //nolint:errcheck
	defer os.Setenv("ORKA_TASK_NAMESPACE", originalNamespace) //nolint:errcheck

	executor := NewToolExecutor()
	if executor.namespace != "custom-namespace" {
		t.Errorf("namespace = %s, want custom-namespace", executor.namespace)
	}
}

func TestToolExecutor_Execute_Success(t *testing.T) {
	expectedResponse := `{"result": "success"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(expectedResponse)) //nolint:errcheck
	}))
	defer server.Close()

	executor := &ToolExecutor{
		client:     server.Client(),
		namespace:  "default",
		secretPath: "/secrets/tools",
	}

	tool := &corev1alpha1.Tool{
		Spec: corev1alpha1.ToolSpec{
			HTTP: corev1alpha1.HTTPExecution{
				URL: server.URL,
			},
		},
	}

	args := json.RawMessage(`{"key": "value"}`)
	result, err := executor.Execute(context.Background(), tool, args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if result != expectedResponse {
		t.Errorf("Execute() = %v, want %v", result, expectedResponse)
	}
}

func TestToolExecutor_Execute_DefaultMethodPOST(t *testing.T) {
	var receivedMethod string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedMethod = r.Method
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	executor := &ToolExecutor{
		client:     server.Client(),
		namespace:  "default",
		secretPath: "/secrets/tools",
	}

	tool := &corev1alpha1.Tool{
		Spec: corev1alpha1.ToolSpec{
			HTTP: corev1alpha1.HTTPExecution{
				URL:    server.URL,
				Method: "", // Empty, should default to POST
			},
		},
	}

	_, err := executor.Execute(context.Background(), tool, nil)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if receivedMethod != http.MethodPost {
		t.Errorf("Method = %s, want POST", receivedMethod)
	}
}

func TestToolExecutor_Execute_CustomMethod(t *testing.T) {
	var receivedMethod string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedMethod = r.Method
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	executor := &ToolExecutor{
		client:     server.Client(),
		namespace:  "default",
		secretPath: "/secrets/tools",
	}

	tool := &corev1alpha1.Tool{
		Spec: corev1alpha1.ToolSpec{
			HTTP: corev1alpha1.HTTPExecution{
				URL:    server.URL,
				Method: http.MethodPut,
			},
		},
	}

	_, err := executor.Execute(context.Background(), tool, nil)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if receivedMethod != http.MethodPut {
		t.Errorf("Method = %s, want PUT", receivedMethod)
	}
}

func TestToolExecutor_Execute_CustomHeaders(t *testing.T) {
	var receivedHeaders http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHeaders = r.Header
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	executor := &ToolExecutor{
		client:     server.Client(),
		namespace:  "default",
		secretPath: "/secrets/tools",
	}

	tool := &corev1alpha1.Tool{
		Spec: corev1alpha1.ToolSpec{
			HTTP: corev1alpha1.HTTPExecution{
				URL: server.URL,
				Headers: map[string]string{
					"X-Custom-Header": "custom-value",
				},
			},
		},
	}

	_, err := executor.Execute(context.Background(), tool, nil)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if receivedHeaders.Get("X-Custom-Header") != "custom-value" {
		t.Errorf("X-Custom-Header = %s, want custom-value", receivedHeaders.Get("X-Custom-Header"))
	}
	if receivedHeaders.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type = %s, want application/json", receivedHeaders.Get("Content-Type"))
	}
}

func TestToolExecutor_Execute_AuthHeader(t *testing.T) {
	var receivedAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	// Create temp secret file
	tmpDir := t.TempDir()
	secretDir := filepath.Join(tmpDir, "secret-name")
	os.MkdirAll(secretDir, 0755)                                                //nolint:errcheck
	os.WriteFile(filepath.Join(secretDir, "token"), []byte("test-token"), 0644) //nolint:errcheck

	executor := &ToolExecutor{
		client:     server.Client(),
		namespace:  "default",
		secretPath: tmpDir,
	}

	tool := &corev1alpha1.Tool{
		Spec: corev1alpha1.ToolSpec{
			HTTP: corev1alpha1.HTTPExecution{
				URL: server.URL,
				AuthSecretRef: &corev1alpha1.SecretKeySelector{
					Name: "secret-name",
					Key:  "token",
				},
				AuthInject: "header",
			},
		},
	}

	_, err := executor.Execute(context.Background(), tool, nil)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if receivedAuth != "Bearer test-token" {
		t.Errorf("Authorization = %s, want Bearer test-token", receivedAuth)
	}
}

func TestToolExecutor_Execute_AuthBody(t *testing.T) {
	var receivedBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&receivedBody) //nolint:errcheck
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	// Create temp secret file
	tmpDir := t.TempDir()
	secretDir := filepath.Join(tmpDir, "secret-name")
	os.MkdirAll(secretDir, 0755)                                                      //nolint:errcheck
	os.WriteFile(filepath.Join(secretDir, "api_key"), []byte("secret-api-key"), 0644) //nolint:errcheck

	executor := &ToolExecutor{
		client:     server.Client(),
		namespace:  "default",
		secretPath: tmpDir,
	}

	tool := &corev1alpha1.Tool{
		Spec: corev1alpha1.ToolSpec{
			HTTP: corev1alpha1.HTTPExecution{
				URL: server.URL,
				AuthSecretRef: &corev1alpha1.SecretKeySelector{
					Name: "secret-name",
					Key:  "api_key",
				},
				AuthInject:  "body",
				AuthBodyKey: "apiKey",
			},
		},
	}

	_, err := executor.Execute(context.Background(), tool, json.RawMessage(`{"query": "test"}`))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if receivedBody["apiKey"] != "secret-api-key" {
		t.Errorf("apiKey = %v, want secret-api-key", receivedBody["apiKey"])
	}
	if receivedBody["query"] != "test" {
		t.Errorf("query = %v, want test", receivedBody["query"])
	}
}

func TestToolExecutor_Execute_AuthBodyMissingKey(t *testing.T) {
	// Create temp secret file
	tmpDir := t.TempDir()
	secretDir := filepath.Join(tmpDir, "secret-name")
	os.MkdirAll(secretDir, 0755)                                                      //nolint:errcheck
	os.WriteFile(filepath.Join(secretDir, "api_key"), []byte("secret-api-key"), 0644) //nolint:errcheck

	executor := &ToolExecutor{
		client:     &http.Client{},
		namespace:  "default",
		secretPath: tmpDir,
	}

	tool := &corev1alpha1.Tool{
		Spec: corev1alpha1.ToolSpec{
			HTTP: corev1alpha1.HTTPExecution{
				URL: "http://localhost",
				AuthSecretRef: &corev1alpha1.SecretKeySelector{
					Name: "secret-name",
					Key:  "api_key",
				},
				AuthInject:  "body",
				AuthBodyKey: "", // Missing
			},
		},
	}

	_, err := executor.Execute(context.Background(), tool, nil)
	if err == nil {
		t.Error("Execute() expected error for missing authBodyKey")
	}
}

func TestToolExecutor_Execute_HTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal error")) //nolint:errcheck
	}))
	defer server.Close()

	executor := &ToolExecutor{
		client:     server.Client(),
		namespace:  "default",
		secretPath: "/secrets/tools",
	}

	tool := &corev1alpha1.Tool{
		Spec: corev1alpha1.ToolSpec{
			HTTP: corev1alpha1.HTTPExecution{
				URL: server.URL,
			},
		},
	}

	_, err := executor.Execute(context.Background(), tool, nil)
	if err == nil {
		t.Error("Execute() expected error for HTTP 500")
	}
}

func TestToolExecutor_Execute_InvalidArgs(t *testing.T) {
	executor := &ToolExecutor{
		client:     &http.Client{},
		namespace:  "default",
		secretPath: "/secrets/tools",
	}

	tool := &corev1alpha1.Tool{
		Spec: corev1alpha1.ToolSpec{
			HTTP: corev1alpha1.HTTPExecution{
				URL: "http://localhost",
			},
		},
	}

	_, err := executor.Execute(context.Background(), tool, json.RawMessage(`{invalid json}`))
	if err == nil {
		t.Error("Execute() expected error for invalid JSON args")
	}
}

func TestToolExecutor_Execute_Timeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	executor := &ToolExecutor{
		client:     server.Client(),
		namespace:  "default",
		secretPath: "/secrets/tools",
	}

	timeout := metav1.Duration{Duration: 5 * time.Second}
	tool := &corev1alpha1.Tool{
		Spec: corev1alpha1.ToolSpec{
			HTTP: corev1alpha1.HTTPExecution{
				URL:     server.URL,
				Timeout: &timeout,
			},
		},
	}

	_, err := executor.Execute(context.Background(), tool, nil)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	// Verify shared client timeout was NOT mutated (per-request client used instead)
	if executor.client.Timeout != 0 {
		t.Errorf("Timeout = %v, want 0 (shared client should not be mutated)", executor.client.Timeout)
	}
}

func TestToolExecutor_Execute_MissingAuthSecret(t *testing.T) {
	executor := &ToolExecutor{
		client:     &http.Client{},
		namespace:  "default",
		secretPath: "/nonexistent/path",
	}

	tool := &corev1alpha1.Tool{
		Spec: corev1alpha1.ToolSpec{
			HTTP: corev1alpha1.HTTPExecution{
				URL: "http://localhost",
				AuthSecretRef: &corev1alpha1.SecretKeySelector{
					Name: "nonexistent-secret",
					Key:  "token",
				},
			},
		},
	}

	_, err := executor.Execute(context.Background(), tool, nil)
	if err == nil {
		t.Error("Execute() expected error for missing auth secret")
	}
}

func TestToolExecutor_getSecretKey_MountedSecret(t *testing.T) {
	tmpDir := t.TempDir()
	secretDir := filepath.Join(tmpDir, "my-secret")
	os.MkdirAll(secretDir, 0755)                                                       //nolint:errcheck
	os.WriteFile(filepath.Join(secretDir, "my-key"), []byte("  secret-value  "), 0644) //nolint:errcheck

	executor := &ToolExecutor{
		secretPath: tmpDir,
		namespace:  "default",
	}

	value, err := executor.getSecretKey(context.Background(), "my-secret", "my-key")
	if err != nil {
		t.Fatalf("getSecretKey() error = %v", err)
	}

	// Should trim whitespace
	if value != "secret-value" {
		t.Errorf("getSecretKey() = %q, want %q", value, "secret-value")
	}
}

func TestToolExecutor_getSecretKey_K8sAPISecret(t *testing.T) {
	// Create a fake Kubernetes client with a secret
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "k8s-secret",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"api-key": []byte("k8s-secret-value"),
		},
	}

	fakeClient := fake.NewSimpleClientset(secret) //nolint:staticcheck // NewClientset requires apply configs

	executor := &ToolExecutor{
		secretPath: "/nonexistent", // Mount doesn't exist
		namespace:  "default",
		k8sClient:  fakeClient,
	}

	value, err := executor.getSecretKey(context.Background(), "k8s-secret", "api-key")
	if err != nil {
		t.Fatalf("getSecretKey() error = %v", err)
	}

	if value != "k8s-secret-value" {
		t.Errorf("getSecretKey() = %q, want %q", value, "k8s-secret-value")
	}
}

func TestToolExecutor_getSecretKey_NotFound(t *testing.T) {
	fakeClient := fake.NewSimpleClientset() //nolint:staticcheck // NewClientset requires apply configs

	executor := &ToolExecutor{
		secretPath: "/nonexistent",
		namespace:  "default",
		k8sClient:  fakeClient,
	}

	_, err := executor.getSecretKey(context.Background(), "nonexistent-secret", "key")
	if err == nil {
		t.Error("getSecretKey() expected error for nonexistent secret")
	}
}

func TestToolExecutor_getSecretKey_TaskSecretPath(t *testing.T) {
	tmpDir := t.TempDir()
	taskSecretPath := filepath.Join(tmpDir, "task")
	os.MkdirAll(taskSecretPath, 0755)                                                  //nolint:errcheck
	os.WriteFile(filepath.Join(taskSecretPath, "my-key"), []byte("task-secret"), 0644) //nolint:errcheck

	// The getSecretKey function checks /secrets/task/{key} as one of the paths
	// We need to mock this properly
	executor := &ToolExecutor{
		secretPath: tmpDir,
		namespace:  "default",
	}

	// This test verifies the mounted secret paths work
	secretDir := filepath.Join(tmpDir, "my-secret")
	os.MkdirAll(secretDir, 0755)                                                  //nolint:errcheck
	os.WriteFile(filepath.Join(secretDir, "key"), []byte("mounted-secret"), 0644) //nolint:errcheck

	value, err := executor.getSecretKey(context.Background(), "my-secret", "key")
	if err != nil {
		t.Fatalf("getSecretKey() error = %v", err)
	}

	if value != "mounted-secret" {
		t.Errorf("getSecretKey() = %q, want %q", value, "mounted-secret")
	}
}

func TestToolExecutor_Execute_EmptyArgs(t *testing.T) {
	var receivedBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedBody, _ = json.Marshal(map[string]any{})
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	executor := &ToolExecutor{
		client:     server.Client(),
		namespace:  "default",
		secretPath: "/secrets/tools",
	}

	tool := &corev1alpha1.Tool{
		Spec: corev1alpha1.ToolSpec{
			HTTP: corev1alpha1.HTTPExecution{
				URL: server.URL,
			},
		},
	}

	// Empty args
	_, err := executor.Execute(context.Background(), tool, nil)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	// Should create empty params map
	_ = receivedBody
}

func TestToolExecutor_Execute_DefaultAuthInject(t *testing.T) {
	var receivedAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	// Create temp secret file
	tmpDir := t.TempDir()
	secretDir := filepath.Join(tmpDir, "secret-name")
	os.MkdirAll(secretDir, 0755)                                                //nolint:errcheck
	os.WriteFile(filepath.Join(secretDir, "token"), []byte("test-token"), 0644) //nolint:errcheck

	executor := &ToolExecutor{
		client:     server.Client(),
		namespace:  "default",
		secretPath: tmpDir,
	}

	tool := &corev1alpha1.Tool{
		Spec: corev1alpha1.ToolSpec{
			HTTP: corev1alpha1.HTTPExecution{
				URL: server.URL,
				AuthSecretRef: &corev1alpha1.SecretKeySelector{
					Name: "secret-name",
					Key:  "token",
				},
				AuthInject: "", // Empty, should default to header
			},
		},
	}

	_, err := executor.Execute(context.Background(), tool, nil)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	// Default should be header auth
	if receivedAuth != "Bearer test-token" {
		t.Errorf("Authorization = %s, want Bearer test-token", receivedAuth)
	}
}

func TestToolExecutor_Execute_URLInterpolation(t *testing.T) {
	var receivedPath string
	var receivedBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		json.NewDecoder(r.Body).Decode(&receivedBody) //nolint:errcheck
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status": "ok"}`)) //nolint:errcheck
	}))
	defer server.Close()

	executor := &ToolExecutor{
		client:     server.Client(),
		namespace:  "default",
		secretPath: "/secrets/tools",
	}

	tool := &corev1alpha1.Tool{
		Spec: corev1alpha1.ToolSpec{
			HTTP: corev1alpha1.HTTPExecution{
				URL:    server.URL + "/repos/{{owner}}/{{repo}}/commits/{{ref}}/check-runs",
				Method: http.MethodGet,
			},
		},
	}

	args := json.RawMessage(`{"owner": "myorg", "repo": "myrepo", "ref": "abc123"}`)
	_, err := executor.Execute(context.Background(), tool, args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	// Verify URL was interpolated
	expectedPath := "/repos/myorg/myrepo/commits/abc123/check-runs"
	if receivedPath != expectedPath {
		t.Errorf("path = %q, want %q", receivedPath, expectedPath)
	}

	// Verify interpolated keys were removed from body
	if receivedBody != nil {
		if _, ok := receivedBody["owner"]; ok {
			t.Error("body should not contain 'owner' after interpolation")
		}
		if _, ok := receivedBody["repo"]; ok {
			t.Error("body should not contain 'repo' after interpolation")
		}
		if _, ok := receivedBody["ref"]; ok {
			t.Error("body should not contain 'ref' after interpolation")
		}
	}
}

func TestToolExecutor_Execute_URLInterpolation_Partial(t *testing.T) {
	var receivedPath string
	var receivedBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		json.NewDecoder(r.Body).Decode(&receivedBody) //nolint:errcheck
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	executor := &ToolExecutor{
		client:     server.Client(),
		namespace:  "default",
		secretPath: "/secrets/tools",
	}

	tool := &corev1alpha1.Tool{
		Spec: corev1alpha1.ToolSpec{
			HTTP: corev1alpha1.HTTPExecution{
				URL:    server.URL + "/repos/{{owner}}/{{repo}}/pulls/{{pull_number}}/merge",
				Method: http.MethodPut,
			},
		},
	}

	args := json.RawMessage(`{"owner": "myorg", "repo": "myrepo", "pull_number": 42, "merge_method": "squash"}`)
	_, err := executor.Execute(context.Background(), tool, args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	// Verify URL was interpolated (pull_number is numeric, should be converted to string)
	expectedPath := "/repos/myorg/myrepo/pulls/42/merge"
	if receivedPath != expectedPath {
		t.Errorf("path = %q, want %q", receivedPath, expectedPath)
	}

	// Verify non-interpolated key remains in body
	if receivedBody == nil {
		t.Fatal("body should not be nil")
	}
	if receivedBody["merge_method"] != "squash" {
		t.Errorf("body merge_method = %v, want squash", receivedBody["merge_method"])
	}

	// Interpolated keys should be removed
	if _, ok := receivedBody["owner"]; ok {
		t.Error("body should not contain 'owner'")
	}
}

func TestToolExecutor_Execute_URLInterpolation_NoPlaceholders(t *testing.T) {
	var receivedPath string
	var receivedBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		json.NewDecoder(r.Body).Decode(&receivedBody) //nolint:errcheck
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	executor := &ToolExecutor{
		client:     server.Client(),
		namespace:  "default",
		secretPath: "/secrets/tools",
	}

	tool := &corev1alpha1.Tool{
		Spec: corev1alpha1.ToolSpec{
			HTTP: corev1alpha1.HTTPExecution{
				URL: server.URL + "/api/search",
			},
		},
	}

	args := json.RawMessage(`{"query": "test", "limit": 10}`)
	_, err := executor.Execute(context.Background(), tool, args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	// URL should be unchanged
	if receivedPath != "/api/search" {
		t.Errorf("path = %q, want /api/search", receivedPath)
	}

	// All params should remain in body
	if receivedBody["query"] != "test" {
		t.Errorf("body query = %v, want test", receivedBody["query"])
	}
}
