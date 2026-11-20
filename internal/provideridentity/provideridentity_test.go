// Copyright (c) 2026. MIT License - see LICENSE file for details.

package provideridentity

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testTenant     = "11111111-1111-1111-1111-111111111111"
	testClient     = "22222222-2222-2222-2222-222222222222"
	testCredential = "controller-sa-token"
	testEndpoint   = "https://example.openai.azure.com"
)

func testConfig(mutate func(*Identity)) Config {
	identity := Identity{Name: "models", TenantID: testTenant, ClientID: testClient, AllowedEndpoints: []string{testEndpoint}}
	if mutate != nil {
		mutate(&identity)
	}
	return Config{Identities: []Identity{identity}}
}

func TestParseRefusesUnknownFieldsAndAcceptsEmpty(t *testing.T) {
	if config, err := Parse("  "); err != nil || len(config.Identities) != 0 {
		t.Fatalf("Parse(empty) = %+v, %v", config, err)
	}
	if _, err := Parse(`{"identities":[{"name":"models","tenantID":"` + testTenant + `","clientID":"` + testClient + `","allowedEndpoint":["https://x"]}]}`); err == nil {
		t.Fatal("a misspelled field was accepted")
	}
	raw := `{"identities":[{"name":"models","tenantID":"` + testTenant + `","clientID":"` + testClient + `","allowedEndpoints":["` + testEndpoint + `"]}]}`
	if _, err := Parse(raw); err != nil {
		t.Fatalf("Parse(valid) = %v", err)
	}
	if _, err := Parse(raw + `{}`); err == nil {
		t.Fatal("trailing data was accepted")
	}
}

func TestValidateDerivesCloudDefaultsAndRequiresUndocumentedOnes(t *testing.T) {
	for name, tc := range map[string]struct {
		config  Config
		wantErr string
	}{
		"public defaults":           {config: testConfig(nil)},
		"bad name":                  {config: testConfig(func(i *Identity) { i.Name = "Models" }), wantErr: "DNS label"},
		"bad tenant":                {config: testConfig(func(i *Identity) { i.TenantID = " " + testTenant }), wantErr: "GUIDs"},
		"unknown cloud":             {config: testConfig(func(i *Identity) { i.Cloud = "Mars" }), wantErr: "cloud must be"},
		"no endpoints":              {config: testConfig(func(i *Identity) { i.AllowedEndpoints = nil }), wantErr: "at least one"},
		"endpoint with path":        {config: testConfig(func(i *Identity) { i.AllowedEndpoints = []string{testEndpoint + "/openai"} }), wantErr: "exact https origin"},
		"http endpoint":             {config: testConfig(func(i *Identity) { i.AllowedEndpoints = []string{"http://example.openai.azure.com"} }), wantErr: "exact https origin"},
		"wildcard endpoint":         {config: testConfig(func(i *Identity) { i.AllowedEndpoints = []string{"https://*.openai.azure.com"} }), wantErr: "exact https origin"},
		"scope without default":     {config: testConfig(func(i *Identity) { i.Scope = "https://ai.azure.com" }), wantErr: "/.default"},
		"http authority":            {config: testConfig(func(i *Identity) { i.Authority = "http://login.example" }), wantErr: "https URL"},
		"us gov needs scope":        {config: Config{TokenAudience: "api://AzureADTokenExchangeUSGov", Identities: testConfig(func(i *Identity) { i.Cloud = CloudAzureUSGovernment }).Identities}, wantErr: "set scope"},
		"us gov needs its audience": {config: testConfig(func(i *Identity) { i.Cloud = CloudAzureUSGovernment; i.Scope = "https://example.us/.default" }), wantErr: "does not match"},
		"us gov with scope":         {config: Config{TokenAudience: "api://AzureADTokenExchangeUSGov", Identities: testConfig(func(i *Identity) { i.Cloud = CloudAzureUSGovernment; i.Scope = "https://example.us/.default" }).Identities}},
		"china needs audience":      {config: testConfig(func(i *Identity) { i.Cloud = CloudAzureChina; i.Scope = "https://example.cn/.default" }), wantErr: "set exchangeAudience"},
		"china with explicit values": {config: Config{TokenAudience: "api://example-cn", Identities: testConfig(func(i *Identity) {
			i.Cloud = CloudAzureChina
			i.Scope = "https://example.cn/.default"
			i.ExchangeAudience = "api://example-cn"
		}).Identities}},
		"duplicate": {config: Config{Identities: append(testConfig(nil).Identities, testConfig(nil).Identities...)}, wantErr: "more than once"},
	} {
		t.Run(name, func(t *testing.T) {
			err := tc.config.Validate()
			if tc.wantErr == "" && err != nil {
				t.Fatalf("Validate() = %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("Validate() = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestCheckPinsTheExactOrigin(t *testing.T) {
	source, err := NewSource(testConfig(func(i *Identity) {
		i.AllowedEndpoints = []string{"https://Example.OpenAI.Azure.com:443/"}
	}), func() (string, error) { return testCredential, nil })
	if err != nil {
		t.Fatal(err)
	}
	for _, baseURL := range []string{testEndpoint, testEndpoint + "/", testEndpoint + "/openai/v1", "https://EXAMPLE.openai.azure.com:443"} {
		if err := source.Check("models", baseURL); err != nil {
			t.Fatalf("Check(%q) = %v", baseURL, err)
		}
	}
	for baseURL, want := range map[string]error{
		"https://other.openai.azure.com":        ErrEndpointNotAllowed,
		"https://example.openai.azure.com.evil": ErrEndpointNotAllowed,
		"https://example.openai.azure.com:8443": ErrEndpointNotAllowed,
	} {
		if err := source.Check("models", baseURL); !errors.Is(err, want) {
			t.Fatalf("Check(%q) = %v, want %v", baseURL, err, want)
		}
	}
	for _, baseURL := range []string{"http://example.openai.azure.com", "https://example.openai.azure.com@evil.io"} {
		if err := source.Check("models", baseURL); err == nil {
			t.Fatalf("Check(%q) was allowed", baseURL)
		}
	}
	if err := source.Check("other", testEndpoint); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("Check(unknown identity) = %v", err)
	}
	var nilSource *Source
	if err := nilSource.Check("models", testEndpoint); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("nil Source Check = %v", err)
	}
}

func TestNewSourceWithoutIdentitiesIsNil(t *testing.T) {
	source, err := NewSource(Config{}, nil)
	if err != nil || source != nil {
		t.Fatalf("NewSource(empty) = %v, %v; want nil, nil", source, err)
	}
}

type fakeEntra struct {
	mu       sync.Mutex
	requests []url.Values
	refuse   bool
}

func (f *fakeEntra) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/"+testTenant+"/oauth2/v2.0/token" {
			t.Errorf("token path = %q", r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		f.mu.Lock()
		f.requests = append(f.requests, r.PostForm)
		refuse := f.refuse
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if refuse {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid_client", "error_codes": []int{700016}, "error_description": "secret=" + testCredential})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "data-plane-token", "expires_in": 3599})
	}
}

func (f *fakeEntra) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func newTestSource(t *testing.T, fake *fakeEntra, now *time.Time) *Source {
	t.Helper()
	server := httptest.NewTLSServer(fake.handler(t))
	t.Cleanup(server.Close)
	source, err := NewSource(testConfig(func(i *Identity) { i.Authority = server.URL }), func() (string, error) { return testCredential, nil })
	if err != nil {
		t.Fatal(err)
	}
	source.HTTPClient = server.Client()
	source.Now = func() time.Time { return *now }
	return source
}

func TestTokenUsesTheWorkloadIdentityFlowAndCaches(t *testing.T) {
	fake := &fakeEntra{}
	now := time.Now()
	source := newTestSource(t, fake, &now)
	token, err := source.Token(context.Background(), "models", testEndpoint+"/openai")
	if err != nil || token != "data-plane-token" {
		t.Fatalf("Token() = %q, %v", token, err)
	}
	form := fake.requests[0]
	for key, want := range map[string]string{
		"grant_type":            "client_credentials",
		"client_id":             testClient,
		"scope":                 "https://ai.azure.com/.default",
		"client_assertion_type": "urn:ietf:params:oauth:client-assertion-type:jwt-bearer",
		"client_assertion":      testCredential,
	} {
		if got := form.Get(key); got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}
	if _, err := source.Token(context.Background(), "models", testEndpoint); err != nil {
		t.Fatal(err)
	}
	if fake.count() != 1 {
		t.Fatalf("Entra saw %d requests, want the second answered from the cache", fake.count())
	}
	now = now.Add(5 * time.Minute)
	if _, err := source.Token(context.Background(), "models", testEndpoint); err != nil {
		t.Fatal(err)
	}
	if fake.count() != 2 {
		t.Fatalf("Entra saw %d requests, want a fresh token after five minutes", fake.count())
	}
	// Verify always asks Entra.
	if err := source.Verify(context.Background(), "models", testEndpoint); err != nil {
		t.Fatal(err)
	}
	if fake.count() != 3 {
		t.Fatalf("Entra saw %d requests, want Verify to bypass the cache", fake.count())
	}
}

func TestTokenNeverAsksEntraForADisallowedEndpoint(t *testing.T) {
	fake := &fakeEntra{}
	now := time.Now()
	source := newTestSource(t, fake, &now)
	if _, err := source.Token(context.Background(), "models", "https://attacker.example"); !errors.Is(err, ErrEndpointNotAllowed) {
		t.Fatalf("Token() = %v, want ErrEndpointNotAllowed", err)
	}
	if fake.count() != 0 {
		t.Fatal("Entra was asked for a token that could not be used")
	}
}

func TestRefusalCarriesNoCredential(t *testing.T) {
	fake := &fakeEntra{refuse: true}
	now := time.Now()
	source := newTestSource(t, fake, &now)
	_, err := source.Token(context.Background(), "models", testEndpoint)
	if err == nil || errors.Is(err, ErrIssuerUnavailable) || !strings.Contains(err.Error(), "invalid_client") || strings.Contains(err.Error(), testCredential) {
		t.Fatalf("Token() = %v, want a refusal naming only the error code", err)
	}
	if err := source.Verify(context.Background(), "models", testEndpoint); err == nil {
		t.Fatal("Verify() accepted a refused identity")
	}
}

func TestMissingCredentialIsUnavailable(t *testing.T) {
	source, err := NewSource(testConfig(nil), func() (string, error) { return "", ErrIssuerUnavailable })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.Token(context.Background(), "models", testEndpoint); !errors.Is(err, ErrIssuerUnavailable) {
		t.Fatalf("Token() = %v, want ErrIssuerUnavailable", err)
	}
}
