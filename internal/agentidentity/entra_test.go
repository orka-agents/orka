// Copyright (c) 2026. MIT License - see LICENSE file for details.

package agentidentity

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/outboundaccess"
)

const testControllerToken = "controller-sa-token"

// fakeEntra records token requests and answers like the blueprint flow.
type fakeEntra struct {
	mu       sync.Mutex
	requests []url.Values
	paths    []string
	refuse   bool
}

func (f *fakeEntra) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		f.mu.Lock()
		f.requests = append(f.requests, r.PostForm)
		f.paths = append(f.paths, r.URL.Path)
		refuse := f.refuse
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if refuse {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": "invalid_client", "error_codes": []int{700016},
				"error_description": "secret detail that must not leak",
			})
			return
		}
		token := "exchange-token"
		if r.PostForm.Get("scope") != entraExchangeScope {
			token = "app-token-for-" + r.PostForm.Get("scope")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": token, "expires_in": 3599, "token_type": "Bearer"})
	}
}

func (f *fakeEntra) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func newTestIssuer(t *testing.T, fake *fakeEntra, now *time.Time) *EntraIssuer {
	t.Helper()
	server := httptest.NewTLSServer(fake.handler(t))
	t.Cleanup(server.Close)
	return &EntraIssuer{
		Config:     EntraConfig{TenantID: testTenant, BlueprintClientID: testBlueprint},
		Authority:  server.URL,
		Credential: func() (string, error) { return testControllerToken, nil },
		HTTPClient: server.Client(),
		Now:        func() time.Time { return *now },
	}
}

func TestEntraAppTokenUsesTheBlueprintFlow(t *testing.T) {
	fake := &fakeEntra{}
	now := time.Now()
	issuer := newTestIssuer(t, fake, &now)
	token, err := issuer.AppToken(context.Background(), testIdentity, "https://graph.microsoft.com")
	if err != nil {
		t.Fatal(err)
	}
	if token != "app-token-for-https://graph.microsoft.com/.default" {
		t.Fatalf("token = %q", token)
	}
	if fake.count() != 2 {
		t.Fatalf("requests = %d, want the exchange and the app request", fake.count())
	}
	exchange, app := fake.requests[0], fake.requests[1]
	for key, want := range map[string]string{
		"grant_type": "client_credentials", "client_id": testBlueprint, "scope": entraExchangeScope,
		"fmi_path": strings.ToLower(testIdentity), "client_assertion": testControllerToken,
		"client_assertion_type": jwtBearerAssertionType,
	} {
		if got := exchange.Get(key); got != want {
			t.Fatalf("exchange %s = %q, want %q", key, got, want)
		}
	}
	for key, want := range map[string]string{
		"grant_type": "client_credentials", "client_id": strings.ToLower(testIdentity),
		"scope": "https://graph.microsoft.com/.default", "client_assertion": "exchange-token",
		"client_assertion_type": jwtBearerAssertionType,
	} {
		if got := app.Get(key); got != want {
			t.Fatalf("app %s = %q, want %q", key, got, want)
		}
	}
	if app.Has("fmi_path") {
		t.Fatal("the identity's own request must not carry fmi_path")
	}
	if fake.paths[0] != "/"+testTenant+"/oauth2/v2.0/token" {
		t.Fatalf("token path = %q", fake.paths[0])
	}
}

func TestEntraTokensAreCachedAtMostFiveMinutes(t *testing.T) {
	fake := &fakeEntra{}
	now := time.Now()
	issuer := newTestIssuer(t, fake, &now)
	for range 3 {
		if _, err := issuer.AppToken(context.Background(), testIdentity, "https://graph.microsoft.com"); err != nil {
			t.Fatal(err)
		}
	}
	if fake.count() != 2 {
		t.Fatalf("requests = %d, want cached tokens reused", fake.count())
	}
	now = now.Add(5*time.Minute + time.Second)
	if _, err := issuer.AppToken(context.Background(), testIdentity, "https://graph.microsoft.com"); err != nil {
		t.Fatal(err)
	}
	if fake.count() != 4 {
		t.Fatalf("requests = %d, want both tokens fetched again after five minutes", fake.count())
	}
}

func TestEntraRefusalCarriesOnlyTheErrorCode(t *testing.T) {
	fake := &fakeEntra{refuse: true}
	now := time.Now()
	issuer := newTestIssuer(t, fake, &now)
	_, err := issuer.ExchangeToken(context.Background(), testIdentity)
	if err == nil || !strings.Contains(err.Error(), "invalid_client") || !strings.Contains(err.Error(), "AADSTS700016") {
		t.Fatalf("error = %v, want the Entra error code", err)
	}
	if strings.Contains(err.Error(), "secret detail") || strings.Contains(err.Error(), testControllerToken) {
		t.Fatalf("error leaks response or credential detail: %v", err)
	}
}

func TestSourceRefusesAChangedIssuerInstanceAndOtherModes(t *testing.T) {
	fake := &fakeEntra{}
	now := time.Now()
	source := &Source{Config: configuredConfig(), Entra: newTestIssuer(t, fake, &now)}
	instance, _ := source.Config.Instance("entra")
	request := outboundaccess.AgentIdentityTokenRequest{
		Identity: corev1alpha1.AgentExecutionIdentityRef{Issuer: "entra", ID: strings.ToLower(testIdentity), Instance: instance},
		Mode:     corev1alpha1.OutboundAgentIdentityModeSelf,
		Resource: "https://graph.microsoft.com",
	}
	if _, err := source.AgentIdentityToken(context.Background(), request); err != nil {
		t.Fatalf("frozen instance matches: %v", err)
	}
	stale := request
	stale.Identity.Instance = "entra:other/tenant"
	if _, err := source.AgentIdentityToken(context.Background(), stale); err == nil || !strings.Contains(err.Error(), "changed since the Task was bound") {
		t.Fatalf("stale instance: error = %v", err)
	}
	delegated := request
	delegated.Mode = "Delegated"
	if _, err := source.AgentIdentityToken(context.Background(), delegated); err == nil {
		t.Fatal("an unsupported mode must be refused")
	}
}

func TestSourceVerifyRequestsTheExchangeToken(t *testing.T) {
	fake := &fakeEntra{}
	now := time.Now()
	source := &Source{Config: configuredConfig(), Entra: newTestIssuer(t, fake, &now)}
	agent := identityAgent(now)
	agent.ObjectMeta = metav1.ObjectMeta{Name: "a", Namespace: "team-a"}
	if err := source.Verify(context.Background(), &agent); err != nil {
		t.Fatal(err)
	}
	if fake.count() != 1 || fake.requests[0].Get("fmi_path") != strings.ToLower(testIdentity) {
		t.Fatalf("verify requests = %v", fake.requests)
	}
	fake.mu.Lock()
	fake.refuse = true
	fake.mu.Unlock()
	now = now.Add(10 * time.Minute)
	if err := source.Verify(context.Background(), &agent); err == nil {
		t.Fatal("a refused identity must fail verification")
	}
}
