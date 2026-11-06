// Copyright (c) 2026. MIT License - see LICENSE file for details.

package agentidentity

import (
	"context"
	"encoding/json"
	"fmt"
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
	// disabled refuses only the identity's own requests, as Entra does for
	// a disabled agent identity while still issuing the blueprint's token.
	disabled string
}

func (f *fakeEntra) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		f.mu.Lock()
		f.requests = append(f.requests, r.PostForm)
		f.paths = append(f.paths, r.URL.Path)
		refuse := f.refuse || (f.disabled != "" && r.PostForm.Get("client_id") == f.disabled)
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
		Config:     EntraConfig{TenantID: testTenant, BlueprintClientID: testBlueprint, Authority: server.URL},
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

func TestSourceVerifyRequestsTheIdentitysOwnToken(t *testing.T) {
	fake := &fakeEntra{}
	now := time.Now()
	source := &Source{Config: configuredConfig(), Entra: newTestIssuer(t, fake, &now)}
	agent := identityAgent(now)
	agent.ObjectMeta = metav1.ObjectMeta{Name: "a", Namespace: "team-a"}
	if err := source.Verify(context.Background(), &agent); err != nil {
		t.Fatal(err)
	}
	if fake.count() != 2 || fake.requests[0].Get("fmi_path") != strings.ToLower(testIdentity) ||
		fake.requests[1].Get("client_id") != strings.ToLower(testIdentity) || fake.requests[1].Get("scope") != entraExchangeScope {
		t.Fatalf("verify requests = %v, want the exchange token and the identity's own token", fake.requests)
	}
	fake.mu.Lock()
	fake.refuse = true
	fake.mu.Unlock()
	now = now.Add(10 * time.Minute)
	if err := source.Verify(context.Background(), &agent); err == nil {
		t.Fatal("a refused identity must fail verification")
	}
}

func TestSourceVerifyFailsForADisabledIdentity(t *testing.T) {
	fake := &fakeEntra{disabled: strings.ToLower(testIdentity)}
	now := time.Now()
	source := &Source{Config: configuredConfig(), Entra: newTestIssuer(t, fake, &now)}
	agent := identityAgent(now)
	agent.ObjectMeta = metav1.ObjectMeta{Name: "a", Namespace: "team-a"}
	if err := source.Verify(context.Background(), &agent); err == nil {
		t.Fatal("an identity whose own token Entra refuses must fail verification")
	}
	if fake.count() != 2 || fake.requests[0].Get("client_id") != testBlueprint {
		t.Fatalf("requests = %v, want the blueprint's exchange to succeed first", fake.requests)
	}
}

func TestEntraTokenRequestsNeverFollowRedirects(t *testing.T) {
	var replayed sync.Mutex
	replays := 0
	elsewhere := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		replayed.Lock()
		replays++
		replayed.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "stolen", "expires_in": 3599})
	}))
	t.Cleanup(elsewhere.Close)
	redirecting := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/token", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirecting.Close)
	client := redirecting.Client()
	client.Transport.(*http.Transport).TLSClientConfig.RootCAs.AddCert(elsewhere.Certificate())
	issuer := &EntraIssuer{
		Config:     EntraConfig{TenantID: testTenant, BlueprintClientID: testBlueprint, Authority: redirecting.URL},
		Credential: func() (string, error) { return testControllerToken, nil },
		HTTPClient: client,
	}
	_, err := issuer.ExchangeToken(context.Background(), testIdentity)
	if err == nil || !strings.Contains(err.Error(), "HTTP 307") {
		t.Fatalf("error = %v, want the redirect refused", err)
	}
	replayed.Lock()
	defer replayed.Unlock()
	if replays != 0 {
		t.Fatal("the federated credential was replayed to the redirect target")
	}
	if client.CheckRedirect != nil {
		t.Fatal("the caller's HTTP client must not be modified")
	}
}

func TestEntraScopeKeepsTheResourceAsConfigured(t *testing.T) {
	fake := &fakeEntra{}
	now := time.Now()
	issuer := newTestIssuer(t, fake, &now)
	if _, err := issuer.AppToken(context.Background(), testIdentity, "https://database.windows.net/"); err != nil {
		t.Fatal(err)
	}
	if got := fake.requests[1].Get("scope"); got != "https://database.windows.net//.default" {
		t.Fatalf("scope = %q, want the trailing slash kept", got)
	}
}

func TestEntraTokenCacheIsBounded(t *testing.T) {
	now := time.Now()
	issuer := &EntraIssuer{Now: func() time.Time { return now }}
	fetch := func() (string, time.Duration, error) { return "t", time.Hour, nil }
	for i := range 10 {
		if _, err := issuer.cached(fmt.Sprint("expiring-", i), fetch); err != nil {
			t.Fatal(err)
		}
	}
	now = now.Add(maxCachedTokenLifetime)
	if _, err := issuer.cached("fresh", fetch); err != nil {
		t.Fatal(err)
	}
	if len(issuer.cache) != 1 {
		t.Fatalf("cache holds %d entries, want expired entries reclaimed", len(issuer.cache))
	}
	for i := range maxCachedTokens + 10 {
		now = now.Add(time.Millisecond)
		if _, err := issuer.cached(fmt.Sprint("key-", i), fetch); err != nil {
			t.Fatal(err)
		}
	}
	if len(issuer.cache) != maxCachedTokens {
		t.Fatalf("cache holds %d entries, want at most %d", len(issuer.cache), maxCachedTokens)
	}
}

func TestSourceCachesTokensPerPolicyUIDAndGeneration(t *testing.T) {
	fake := &fakeEntra{}
	now := time.Now()
	source := &Source{Config: configuredConfig(), Entra: newTestIssuer(t, fake, &now)}
	instance, _ := source.Config.Instance("entra")
	request := func(uid string, generation int64) outboundaccess.AgentIdentityTokenRequest {
		return outboundaccess.AgentIdentityTokenRequest{
			Identity: corev1alpha1.AgentExecutionIdentityRef{Issuer: "entra", ID: strings.ToLower(testIdentity), Instance: instance},
			Mode:     corev1alpha1.OutboundAgentIdentityModeSelf,
			Resource: "https://graph.microsoft.com",
			Policy:   outboundaccess.PolicyIdentity{UID: uid, Generation: generation},
		}
	}
	for _, step := range []struct {
		name         string
		request      outboundaccess.AgentIdentityTokenRequest
		wantRequests int
	}{
		{"first call fetches the exchange and app tokens", request("policy-a", 1), 2},
		{"the same policy reuses its token", request("policy-a", 1), 2},
		{"an edited policy gets a fresh token", request("policy-a", 2), 3},
		{"a recreated policy gets a fresh token", request("policy-b", 2), 4},
	} {
		if _, err := source.AgentIdentityToken(context.Background(), step.request); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		if fake.count() != step.wantRequests {
			t.Fatalf("%s: requests = %d, want %d", step.name, fake.count(), step.wantRequests)
		}
	}
}
