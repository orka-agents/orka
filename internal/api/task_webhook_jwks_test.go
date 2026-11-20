/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/taskwebhook"
)

type failingTaskWebhookKeys struct{}

func (failingTaskWebhookKeys) SigningKeys(context.Context) (taskwebhook.KeySet, error) {
	return taskwebhook.KeySet{}, errors.New("unavailable")
}

func (failingTaskWebhookKeys) PublishedKeys(context.Context) (taskwebhook.KeySet, error) {
	return taskwebhook.KeySet{}, errors.New("unavailable")
}

func newJWKSTestServer(t *testing.T, keys taskwebhook.KeySource) *Server {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	return NewServer(fake.NewClientBuilder().WithScheme(scheme).Build(), nil, ServerConfig{Port: 8080, TaskWebhookKeys: keys})
}

func newJWKSTestKey(t *testing.T) *taskwebhook.SigningKey {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := taskwebhook.NewSigningKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func getTaskWebhookJWKS(t *testing.T, server *Server) (*http.Response, taskwebhook.JWKS) {
	t.Helper()
	resp, err := server.app.Test(httptest.NewRequest(http.MethodGet, TaskWebhookJWKSPath, nil))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	var set taskwebhook.JWKS
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(body, &set); err != nil {
			t.Fatalf("decode JWKS %q: %v", body, err)
		}
	}
	return resp, set
}

func TestTaskWebhookJWKSIsPublicAndVerifiesRotatedDeliveries(t *testing.T) {
	t.Parallel()
	current, previous := newJWKSTestKey(t), newJWKSTestKey(t)
	server := newJWKSTestServer(t, taskwebhook.StaticKeySource{Keys: taskwebhook.KeySet{Current: current, Previous: previous}})

	resp, set := getTaskWebhookJWKS(t, server)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 without credentials", resp.StatusCode)
	}
	if resp.Header.Get("Content-Type") != "application/jwk-set+json" {
		t.Fatalf("Content-Type = %q", resp.Header.Get("Content-Type"))
	}
	if len(set.Keys) != 2 || set.Keys[0].KeyID != current.KeyID || set.Keys[1].KeyID != previous.KeyID {
		t.Fatalf("JWKS = %#v", set)
	}

	body := []byte(`{"phase":"Succeeded"}`)
	now := time.Now()
	for _, key := range []*taskwebhook.SigningKey{current, previous} {
		token, err := taskwebhook.Sign(key, taskwebhook.NewClaims("https://hooks.example.com/",
			taskwebhook.TaskClaim{UID: "uid", Namespace: "ns", Name: "task"}, "id", "task.succeeded", 1, body, now))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := taskwebhook.Verify(token, set, taskwebhook.VerifyOptions{Audience: "https://hooks.example.com/", Body: body, Now: now}); err != nil {
			t.Fatalf("Verify() with published JWKS error = %v", err)
		}
	}
}

func TestTaskWebhookJWKSWithoutKeys(t *testing.T) {
	t.Parallel()
	resp, set := getTaskWebhookJWKS(t, newJWKSTestServer(t, nil))
	if resp.StatusCode != http.StatusOK || len(set.Keys) != 0 {
		t.Fatalf("unconfigured JWKS = %d %#v, want empty set", resp.StatusCode, set)
	}

	resp, _ = getTaskWebhookJWKS(t, newJWKSTestServer(t, failingTaskWebhookKeys{}))
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("failing JWKS = %d cache=%q, want 503 no-store", resp.StatusCode, resp.Header.Get("Cache-Control"))
	}

	missing, err := newJWKSTestServer(t, nil).app.Test(httptest.NewRequest(http.MethodGet, "/.well-known/unknown", nil))
	if err != nil {
		t.Fatal(err)
	}
	if missing.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown well-known status = %d, want 404", missing.StatusCode)
	}
}
