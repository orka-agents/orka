/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tokenexchange

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testJWTAlgorithm = "RS256"

//nolint:gocyclo // Table checks each grant/auth request shape in one conformance test.
func TestClientExchangeBuildsSupportedGrantsAndClientAuthentication(t *testing.T) {
	privateKey := testPrivateKeyPEM(t)
	tests := []struct {
		name  string
		grant string
		auth  ClientAuthentication
		check func(*testing.T, *http.Request, url.Values)
	}{
		{
			name:  "rfc8693 no client auth",
			grant: GrantTypeTokenExchange,
			auth:  ClientAuthentication{Method: ClientAuthNone, ClientID: "public-client"},
			check: func(t *testing.T, r *http.Request, form url.Values) {
				if got := form.Get("subject_token"); got != "subject-token" {
					t.Fatalf("subject_token = %q", got)
				}
				if got := form.Get("subject_token_type"); got != "urn:example:subject" {
					t.Fatalf("custom subject_token_type = %q", got)
				}
				if got := form.Get("actor_token_type"); got != "urn:example:actor" {
					t.Fatalf("custom actor_token_type = %q", got)
				}
				if got := form.Get("requested_token_type"); got != "urn:example:resource-token" {
					t.Fatalf("custom requested_token_type = %q", got)
				}
				if got := form["audience"]; len(got) != 2 || got[0] != "aud-a" || got[1] != "aud-b" {
					t.Fatalf("audience = %#v", got)
				}
				if got := form["resource"]; len(got) != 2 || got[0] != "res-a" || got[1] != "res-b" {
					t.Fatalf("resource = %#v", got)
				}
				if got := form.Get("scope"); got != "read write" {
					t.Fatalf("scope = %q", got)
				}
				if got := form.Get("custom"); got != "value" {
					t.Fatalf("custom = %q", got)
				}
				if got := form.Get("client_id"); got != "public-client" {
					t.Fatalf("client_id = %q", got)
				}
			},
		},
		{
			name:  "rfc7523 client secret basic",
			grant: GrantTypeJWTBearer,
			auth:  ClientAuthentication{Method: ClientAuthSecretBasic, ClientID: "client", ClientSecret: "secret"},
			check: func(t *testing.T, r *http.Request, form url.Values) {
				if got := form.Get("assertion"); got != "subject-token" {
					t.Fatalf("assertion = %q", got)
				}
				if form.Get("subject_token") != "" {
					t.Fatal("JWT bearer form unexpectedly contained subject_token")
				}
				clientID, secret, ok := r.BasicAuth()
				if !ok || clientID != url.QueryEscape("client") || secret != url.QueryEscape("secret") {
					t.Fatalf("basic auth = %q/%q/%v", clientID, secret, ok)
				}
			},
		},
		{
			name:  "client secret post",
			grant: GrantTypeTokenExchange,
			auth:  ClientAuthentication{Method: ClientAuthSecretPost, ClientID: "client", ClientSecret: "secret"},
			check: func(t *testing.T, _ *http.Request, form url.Values) {
				if form.Get("client_id") != "client" || form.Get("client_secret") != "secret" {
					t.Fatalf("client post form = %#v", form)
				}
			},
		},
		{
			name:  "private key jwt",
			grant: GrantTypeTokenExchange,
			auth: ClientAuthentication{
				Method:        ClientAuthPrivateKeyJWT,
				ClientID:      "private-client",
				PrivateKeyPEM: privateKey,
				KeyID:         "key-1",
				Audience:      "https://issuer.example.test/token",
			},
			check: func(t *testing.T, _ *http.Request, form url.Values) {
				if form.Get("client_assertion_type") != ClientAssertionTypeJWTBearer {
					t.Fatalf("client_assertion_type = %q", form.Get("client_assertion_type"))
				}
				parts := strings.Split(form.Get("client_assertion"), ".")
				if len(parts) != 3 {
					t.Fatalf("client assertion segments = %d", len(parts))
				}
				var header map[string]any
				decodeJWTPart(t, parts[0], &header)
				if header["alg"] != testJWTAlgorithm || header["kid"] != "key-1" {
					t.Fatalf("client assertion header = %#v", header)
				}
				var claims map[string]any
				decodeJWTPart(t, parts[1], &claims)
				if claims["iss"] != "private-client" || claims["sub"] != "private-client" || claims["aud"] != "https://issuer.example.test/token" {
					t.Fatalf("client assertion claims = %#v", claims)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := r.ParseForm(); err != nil {
					t.Fatalf("ParseForm() error = %v", err)
				}
				if got := r.Form.Get("grant_type"); got != tt.grant {
					t.Fatalf("grant_type = %q, want %q", got, tt.grant)
				}
				tt.check(t, r, r.Form)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"access_token":"resource-token","issued_token_type":"urn:example:resource-token","token_type":"Bearer","expires_in":60}`))
			}))
			defer server.Close()

			req := Request{
				Adapter:                 "direct",
				Endpoint:                server.URL,
				GrantType:               tt.grant,
				SubjectToken:            "subject-token",
				Audiences:               []string{"aud-a", "aud-b"},
				Scopes:                  []string{"read", "write"},
				Resources:               []string{"res-a", "res-b"},
				RequestedTokenType:      "urn:example:resource-token",
				AdditionalParameters:    map[string]string{"custom": "value"},
				ClientAuthentication:    tt.auth,
				ExpectedIssuedTokenType: "urn:example:resource-token",
				RequiredTokenType:       "Bearer",
			}
			if tt.grant == GrantTypeTokenExchange {
				req.SubjectTokenType = "urn:example:subject"
				req.ActorToken = "actor-token"
				req.ActorTokenType = "urn:example:actor"
			}
			result, err := NewClient(ClientOptions{HTTPClient: server.Client()}).Exchange(context.Background(), req)
			if err != nil {
				t.Fatalf("Exchange() error = %v", err)
			}
			if result.AccessToken != "resource-token" || result.TokenType != "Bearer" {
				t.Fatalf("result = %#v", result)
			}
		})
	}
}

func TestClientExchangeRejectsReservedParametersAndInvalidResponses(t *testing.T) {
	base := Request{
		Endpoint:                "https://issuer.example.test/token",
		GrantType:               GrantTypeTokenExchange,
		SubjectToken:            "subject",
		SubjectTokenType:        TokenTypeAccessToken,
		RequestedTokenType:      "urn:example:resource",
		ExpectedIssuedTokenType: "urn:example:resource",
		RequiredTokenType:       "Bearer",
	}
	for _, name := range []string{"grant_type", "SUBJECT_TOKEN", "client_secret", "requested_token_type"} {
		t.Run("reserved "+name, func(t *testing.T) {
			req := base
			req.AdditionalParameters = map[string]string{name: "override"}
			_, err := NewClient(ClientOptions{}).Exchange(context.Background(), req)
			if err == nil || !strings.Contains(err.Error(), "reserved") {
				t.Fatalf("Exchange() error = %v, want reserved parameter", err)
			}
		})
	}

	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "empty token", body: `{"access_token":"","issued_token_type":"urn:example:resource","token_type":"Bearer"}`, want: "access_token"},
		{name: "missing token type", body: `{"access_token":"token","issued_token_type":"urn:example:resource"}`, want: "token_type"},
		{name: "N A resource result", body: `{"access_token":"token","issued_token_type":"urn:example:resource","token_type":"N_A"}`, want: "token_type"},
		{name: "mismatched issued type", body: `{"access_token":"token","issued_token_type":"urn:other","token_type":"Bearer"}`, want: "issued_token_type"},
		{name: "malformed JSON", body: `{`, want: "decode"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()
			req := base
			req.Endpoint = server.URL
			_, err := NewClient(ClientOptions{HTTPClient: server.Client()}).Exchange(context.Background(), req)
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tt.want)) {
				t.Fatalf("Exchange() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestClientExchangeErrorAndResponseLimits(t *testing.T) {
	t.Run("oauth error is redacted", func(t *testing.T) {
		secret := "super-secret-token"
		subject := "opaque-subject-assertion-value"
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = fmt.Fprintf(w, `{"error":"invalid_client","error_description":"Authorization: Bearer %s; assertion %s"}`, secret, subject)
		}))
		defer server.Close()
		req := validResourceRequest(server.URL)
		req.SubjectToken = subject
		_, err := NewClient(ClientOptions{HTTPClient: server.Client()}).Exchange(context.Background(), req)
		if err == nil {
			t.Fatal("Exchange() error = nil")
		}
		if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), subject) {
			t.Fatalf("error leaked secret: %v", err)
		}
		var exchangeErr *ExchangeError
		if !errors.As(err, &exchangeErr) || exchangeErr.StatusCode != http.StatusUnauthorized {
			t.Fatalf("error = %#v", err)
		}
	})

	t.Run("oversized", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(strings.Repeat("x", 65)))
		}))
		defer server.Close()
		_, err := NewClient(ClientOptions{HTTPClient: server.Client(), MaxResponseBytes: 64}).Exchange(context.Background(), validResourceRequest(server.URL))
		if err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("Exchange() error = %v, want response limit", err)
		}
	})

	t.Run("timeout and cancellation", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(100 * time.Millisecond)
			w.WriteHeader(http.StatusGatewayTimeout)
		}))
		defer server.Close()
		req := validResourceRequest(server.URL)
		req.Timeout = 10 * time.Millisecond
		_, err := NewClient(ClientOptions{HTTPClient: server.Client()}).Exchange(context.Background(), req)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("timeout error = %v, want deadline exceeded", err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		req.Timeout = time.Second
		_, err = NewClient(ClientOptions{HTTPClient: server.Client()}).Exchange(ctx, req)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation error = %v", err)
		}
	})
}

func TestClientCacheUsesDigestsCapsExpiryAndCollapsesConcurrentMisses(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		once.Do(func() { close(started) })
		<-release
		_, _ = w.Write([]byte(`{"access_token":"cached-resource-token","issued_token_type":"urn:example:resource","token_type":"Bearer","expires_in":300}`))
	}))
	defer server.Close()

	now := time.Now().UTC()
	client := NewClient(ClientOptions{HTTPClient: server.Client(), MaxCacheEntries: 2, Now: func() time.Time { return now }})
	req := validResourceRequest(server.URL)
	req.SubjectToken = "raw-subject-token-must-not-be-a-key"
	req.CacheNamespace = "policy-generation-digest-a"
	req.SubjectExpiresAt = now.Add(30 * time.Second)

	const goroutines = 8
	errs := make(chan error, goroutines)
	for range goroutines {
		go func() {
			_, err := client.Exchange(context.Background(), req)
			errs <- err
		}()
	}
	<-started
	close(release)
	for range goroutines {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent Exchange() error = %v", err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("endpoint calls = %d, want 1", calls.Load())
	}
	client.mu.Lock()
	if len(client.cache) != 1 {
		t.Fatalf("cache entries = %d, want 1", len(client.cache))
	}
	for key, element := range client.cache {
		if strings.Contains(key, req.SubjectToken) || len(key) != sha256HexLength {
			t.Fatalf("unsafe cache key = %q", key)
		}
		entry := element.Value.(*cacheEntry)
		if !entry.expiresAt.Equal(req.SubjectExpiresAt) {
			t.Fatalf("cache expiry = %s, want subject expiry %s", entry.expiresAt, req.SubjectExpiresAt)
		}
	}
	client.mu.Unlock()

	if _, err := client.Exchange(context.Background(), req); err != nil {
		t.Fatalf("cached Exchange() error = %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("endpoint calls after cache hit = %d, want 1", calls.Load())
	}

	req.CacheNamespace = "policy-generation-digest-b"
	if _, err := client.Exchange(context.Background(), req); err != nil {
		t.Fatalf("new generation Exchange() error = %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("policy generations shared credentials; calls = %d, want 2", calls.Load())
	}
}

const sha256HexLength = 64

func validResourceRequest(endpoint string) Request {
	return Request{
		Adapter:                 "direct",
		Endpoint:                endpoint,
		GrantType:               GrantTypeTokenExchange,
		SubjectToken:            "subject-token",
		SubjectTokenType:        TokenTypeAccessToken,
		RequestedTokenType:      "urn:example:resource",
		ExpectedIssuedTokenType: "urn:example:resource",
		RequiredTokenType:       "Bearer",
	}
}

func testPrivateKeyPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey() error = %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func decodeJWTPart(t *testing.T, part string, destination any) {
	t.Helper()
	data, err := base64.RawURLEncoding.DecodeString(part)
	if err != nil {
		t.Fatalf("decode JWT part: %v", err)
	}
	if err := json.Unmarshal(data, destination); err != nil {
		t.Fatalf("decode JWT JSON: %v", err)
	}
}

func TestClientSingleflightWaiterHonorsCancellation(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case <-started:
		default:
			close(started)
		}
		<-release
		_, _ = w.Write([]byte(`{"access_token":"token","issued_token_type":"urn:example:resource","token_type":"Bearer","expires_in":60}`))
	}))
	defer server.Close()
	client := NewClient(ClientOptions{HTTPClient: server.Client()})
	req := validResourceRequest(server.URL)
	firstDone := make(chan error, 1)
	go func() {
		_, err := client.Exchange(context.Background(), req)
		firstDone <- err
	}()
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	_, err := client.Exchange(ctx, req)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("waiter error = %v", err)
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Fatalf("canceled waiter blocked for %s", time.Since(start))
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first exchange error = %v", err)
	}
}

func TestClientDoesNotFollowTokenEndpointRedirects(t *testing.T) {
	called := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer endpoint.Close()
	_, err := NewClient(ClientOptions{HTTPClient: endpoint.Client()}).Exchange(context.Background(), validResourceRequest(endpoint.URL))
	if err == nil {
		t.Fatal("Exchange() error = nil")
	}
	var exchangeErr *ExchangeError
	if !errors.As(err, &exchangeErr) || exchangeErr.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("Exchange() error = %#v", err)
	}
	if called {
		t.Fatal("redirect target received token endpoint request")
	}
}

func TestJWTBearerAcceptsStandardResponseWithoutIssuedTokenType(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("grant_type") != GrantTypeJWTBearer || r.Form.Get("assertion") != "signed-assertion" {
			t.Fatalf("JWT bearer form = %#v", r.Form)
		}
		_, _ = w.Write([]byte(`{"access_token":"resource-token","token_type":"Bearer","expires_in":60}`))
	}))
	defer server.Close()
	result, err := NewClient(ClientOptions{HTTPClient: server.Client()}).Exchange(context.Background(), Request{
		Adapter:           "direct",
		Endpoint:          server.URL,
		GrantType:         GrantTypeJWTBearer,
		SubjectToken:      "signed-assertion",
		RequiredTokenType: "Bearer",
	})
	if err != nil {
		t.Fatalf("Exchange() error = %v", err)
	}
	if result.AccessToken != "resource-token" || result.IssuedTokenType != "" {
		t.Fatalf("result = %#v", result)
	}
}

func TestResponseTypeMismatchDoesNotEchoEndpointControlledValue(t *testing.T) {
	reflected := "opaque-subject-assertion-value"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"access_token":"token","issued_token_type":%q,"token_type":%q}`, reflected, reflected)
	}))
	defer server.Close()
	req := validResourceRequest(server.URL)
	req.SubjectToken = reflected
	_, err := NewClient(ClientOptions{HTTPClient: server.Client()}).Exchange(context.Background(), req)
	if err == nil {
		t.Fatal("Exchange() error = nil")
	}
	if strings.Contains(err.Error(), reflected) {
		t.Fatalf("mismatch error leaked endpoint-controlled value: %v", err)
	}
}

func TestClientFlightContinuesWhenInitiatingCallerCancels(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-release:
			_, _ = w.Write([]byte(`{"access_token":"token","issued_token_type":"urn:example:resource","token_type":"Bearer","expires_in":60}`))
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	client := NewClient(ClientOptions{HTTPClient: server.Client()})
	req := validResourceRequest(server.URL)
	firstCtx, cancelFirst := context.WithCancel(context.Background())
	firstDone := make(chan error, 1)
	go func() {
		_, err := client.Exchange(firstCtx, req)
		firstDone <- err
	}()
	<-started
	secondDone := make(chan error, 1)
	go func() {
		_, err := client.Exchange(context.Background(), req)
		secondDone <- err
	}()
	// Give the second caller time to join the existing flight before canceling the initiator.
	time.Sleep(10 * time.Millisecond)
	cancelFirst()
	if err := <-firstDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("initiating caller error = %v", err)
	}
	close(release)
	if err := <-secondDone; err != nil {
		t.Fatalf("remaining waiter error = %v", err)
	}
}

func TestClientFlightCancelsExchangeAfterLastWaiterLeaves(t *testing.T) {
	requestStarted := make(chan struct{})
	requestCanceled := make(chan struct{})
	httpClient := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		close(requestStarted)
		<-req.Context().Done()
		close(requestCanceled)
		return nil, req.Context().Err()
	})}
	client := NewClient(ClientOptions{HTTPClient: httpClient})
	req := validResourceRequest("https://issuer.example.test/token")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := client.Exchange(ctx, req)
		done <- err
	}()
	<-requestStarted
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("caller error = %v", err)
	}
	select {
	case <-requestCanceled:
	case <-time.After(time.Second):
		t.Fatal("underlying exchange was not canceled after the last waiter left")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestIsPublicAddressRejectsSpecialUseRanges(t *testing.T) {
	for _, raw := range []string{"100.64.0.1", "192.0.2.1", "198.18.0.1", "192.88.99.2", "203.0.113.1", "64:ff9b:1::1", "100:0:0:1::1", "2001:db8::1", "3fff::1", "5f00::1", "fec0::1", "3ffe::1", "4000::1"} {
		if IsPublicAddress(net.ParseIP(raw)) {
			t.Fatalf("IsPublicAddress(%s) = true", raw)
		}
	}
	for _, raw := range []string{"8.8.8.8", "2001:4860:4860::8888"} {
		if !IsPublicAddress(net.ParseIP(raw)) {
			t.Fatalf("IsPublicAddress(%s) = false", raw)
		}
	}
}

func TestClientRejectsFragmentBearingEndpoint(t *testing.T) {
	req := validResourceRequest("https://issuer.example.test/token#fragment")
	_, err := NewClient(ClientOptions{}).Exchange(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "fragment") {
		t.Fatalf("Exchange() error = %v", err)
	}
}

func TestClientDoesNotStartFlightForAlreadyCanceledCaller(t *testing.T) {
	var calls atomic.Int32
	httpClient := &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("unexpected request")
	})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := NewClient(ClientOptions{HTTPClient: httpClient}).Exchange(ctx, validResourceRequest("https://issuer.example.test/token"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Exchange() error = %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("HTTP calls = %d, want 0", calls.Load())
	}
}

func TestClientSecretBasicFormEscapesOAuthCredentials(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clientID, secret, ok := r.BasicAuth()
		if !ok || clientID != url.QueryEscape("client:id+") || secret != url.QueryEscape("sec%ret:") {
			t.Fatalf("Basic auth = %q/%q/%v", clientID, secret, ok)
		}
		_, _ = w.Write([]byte(`{"access_token":"token","issued_token_type":"urn:example:resource","token_type":"Bearer"}`))
	}))
	defer server.Close()
	req := validResourceRequest(server.URL)
	req.ClientAuthentication = ClientAuthentication{
		Method:       ClientAuthSecretBasic,
		ClientID:     "client:id+",
		ClientSecret: "sec%ret:",
	}
	if _, err := NewClient(ClientOptions{HTTPClient: server.Client()}).Exchange(context.Background(), req); err != nil {
		t.Fatalf("Exchange() error = %v", err)
	}
}

func TestFlightKeyIncludesTimeoutButCacheKeyDoesNot(t *testing.T) {
	req := validResourceRequest("https://issuer.example.test/token")
	cacheKey, err := digestRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if digestFlightKey(cacheKey, 10*time.Millisecond) == digestFlightKey(cacheKey, time.Second) {
		t.Fatal("different timeout semantics shared a flight key")
	}
	req.Timeout = time.Second
	secondCacheKey, err := digestRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if cacheKey != secondCacheKey {
		t.Fatal("timeout changed the reusable credential cache key")
	}
}

func TestClientRejectsPlaintextPublicEndpoint(t *testing.T) {
	req := validResourceRequest("http://issuer.example.test/token")
	req.RequirePublicEndpoint = true
	_, err := NewClient(ClientOptions{}).Exchange(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "must use https") {
		t.Fatalf("Exchange() error = %v", err)
	}
}

func TestDigestIncludesPrivateKeyJWTAssertionSettings(t *testing.T) {
	req := validResourceRequest("https://issuer.example.test/token")
	req.ClientAuthentication = ClientAuthentication{
		Method:        ClientAuthPrivateKeyJWT,
		ClientID:      "client",
		PrivateKeyPEM: testPrivateKeyPEM(t),
		KeyID:         "key-a",
		Audience:      "aud-a",
		AssertionTTL:  time.Minute,
	}
	base, err := digestRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	mutations := []func(*Request){
		func(r *Request) { r.ClientAuthentication.KeyID = "key-b" },
		func(r *Request) { r.ClientAuthentication.Audience = "aud-b" },
		func(r *Request) { r.ClientAuthentication.AssertionTTL = 2 * time.Minute },
	}
	for _, mutate := range mutations {
		changed := req
		mutate(&changed)
		digest, err := digestRequest(changed)
		if err != nil {
			t.Fatal(err)
		}
		if digest == base {
			t.Fatal("private_key_jwt assertion setting did not affect exchange identity")
		}
	}
}

func TestCanceledCallerDoesNotReceiveCachedCredential(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"cached","issued_token_type":"urn:example:resource","token_type":"Bearer","expires_in":60}`))
	}))
	defer server.Close()
	client := NewClient(ClientOptions{HTTPClient: server.Client()})
	req := validResourceRequest(server.URL)
	if _, err := client.Exchange(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.Exchange(ctx, req); !errors.Is(err, context.Canceled) {
		t.Fatalf("cached Exchange() error = %v", err)
	}
}

func TestSubjectExpiryParticipatesInCacheIdentity(t *testing.T) {
	req := validResourceRequest("https://issuer.example.test/token")
	first, err := digestRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	req.SubjectExpiresAt = time.Now().Add(time.Minute).UTC()
	second, err := digestRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("subject expiration did not affect cache identity")
	}
}

func TestClientRejectsEndpointWhitespace(t *testing.T) {
	req := validResourceRequest(" https://issuer.example.test/token")
	_, err := NewClient(ClientOptions{}).Exchange(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "surrounding whitespace") {
		t.Fatalf("Exchange() error = %v", err)
	}
}

func TestPublicClientForcesTLSVerification(t *testing.T) {
	base := &http.Client{Transport: &http.Transport{
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: true, ServerName: "inherited.example"},
		ForceAttemptHTTP2: true,
		TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{
			"h2": func(string, *tls.Conn) http.RoundTripper { return http.DefaultTransport },
		},
	}} //nolint:gosec // verifies hardening overrides an unsafe injected base client.
	client := NewClient(ClientOptions{HTTPClient: base})
	hardened, err := client.clientFor(Request{RequirePublicEndpoint: true})
	if err != nil {
		t.Fatal(err)
	}
	transport := hardened.Transport.(*http.Transport)
	if transport.TLSClientConfig == nil || transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("public endpoint client retained InsecureSkipVerify")
	}
	if transport.TLSClientConfig.ServerName != "" {
		t.Fatalf("public endpoint client retained inherited ServerName %q", transport.TLSClientConfig.ServerName)
	}
	if !transport.DisableKeepAlives {
		t.Fatal("per-request hardened transport retained idle connections")
	}
	if transport.ForceAttemptHTTP2 || len(transport.TLSNextProto) != 0 {
		t.Fatal("per-request hardened transport retained HTTP/2 connection-pool hooks")
	}
	if transport.Protocols == nil || !transport.Protocols.HTTP1() || transport.Protocols.HTTP2() {
		t.Fatalf("hardened transport protocols = %v", transport.Protocols)
	}
	if got := transport.TLSClientConfig.NextProtos; len(got) != 1 || got[0] != "http/1.1" {
		t.Fatalf("hardened transport ALPN = %#v", got)
	}
}

func TestClientRejectsLossyEndpointAndAuthMethodNormalization(t *testing.T) {
	for _, endpoint := range []string{"https://issuer.example.test/token#", "HTTPS://issuer.example.test/token"} {
		req := validResourceRequest(endpoint)
		_, err := NewClient(ClientOptions{}).Exchange(context.Background(), req)
		if err == nil {
			t.Fatalf("Exchange(%q) error = nil", endpoint)
		}
	}
	req := validResourceRequest("https://issuer.example.test/token")
	req.ClientAuthentication = ClientAuthentication{Method: " " + ClientAuthSecretBasic + " ", ClientID: "client", ClientSecret: "secret"}
	_, err := NewClient(ClientOptions{}).Exchange(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "surrounding whitespace") {
		t.Fatalf("auth method error = %v", err)
	}
}

func TestCacheHitHonorsCancellationWhileWaitingForCacheLock(t *testing.T) {
	now := time.Now().UTC()
	client := NewClient(ClientOptions{Now: func() time.Time { return now }})
	req := validResourceRequest("https://issuer.example.test/token")
	key, err := digestRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	client.store(key, Result{AccessToken: "cached", ExpiresAt: now.Add(time.Minute)}, time.Time{})
	client.mu.Lock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := client.Exchange(ctx, req)
		done <- err
	}()
	time.Sleep(10 * time.Millisecond)
	cancel()
	client.mu.Unlock()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cached Exchange() error = %v", err)
	}
}

func TestCloseUnusedDialResultsClosesConnections(t *testing.T) {
	first, peer := net.Pipe()
	results := make(chan endpointDialResult, 1)
	results <- endpointDialResult{connection: first}
	closeUnusedDialResults(results, 1)
	_ = peer.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
	if _, err := peer.Write([]byte("x")); err == nil {
		t.Fatal("unused dial connection remained open")
	}
	_ = peer.Close()
}

func TestCloneRequestOwnsMutableInputs(t *testing.T) {
	req := validResourceRequest("https://issuer.example.test/token")
	req.Audiences = []string{"aud-a"}
	req.Scopes = []string{"read"}
	req.Resources = []string{"resource-a"}
	req.AdditionalParameters = map[string]string{"custom": "value-a"}
	req.TLS.CAPEM = []byte("ca-a")
	req.ClientAuthentication.PrivateKeyPEM = []byte("key-a")
	cloned := cloneRequest(req)
	req.Audiences[0] = "aud-mutated"
	req.Scopes[0] = "write"
	req.Resources[0] = "resource-b"
	req.AdditionalParameters["custom"] = "value-b"
	req.TLS.CAPEM[0] = 'X'
	req.ClientAuthentication.PrivateKeyPEM[0] = 'X'
	if cloned.Audiences[0] != "aud-a" || cloned.Scopes[0] != "read" || cloned.Resources[0] != "resource-a" ||
		cloned.AdditionalParameters["custom"] != "value-a" || string(cloned.TLS.CAPEM) != "ca-a" ||
		string(cloned.ClientAuthentication.PrivateKeyPEM) != "key-a" {
		t.Fatalf("clone retained caller-owned mutable data: %#v", cloned)
	}
}

func TestClientRejectsExpiresInOverflow(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"token","issued_token_type":"urn:example:resource","token_type":"Bearer","expires_in":9223372036854775807}`))
	}))
	defer server.Close()
	_, err := NewClient(ClientOptions{HTTPClient: server.Client()}).Exchange(context.Background(), validResourceRequest(server.URL))
	if err == nil || !strings.Contains(err.Error(), "representable") {
		t.Fatalf("Exchange() error = %v", err)
	}
}

func TestValidatePublicEndpointAddressesBoundsDNSFanout(t *testing.T) {
	addresses := make([]net.IPAddr, maxPublicEndpointAddresses+1)
	for i := range addresses {
		addresses[i] = net.IPAddr{IP: net.ParseIP(fmt.Sprintf("8.8.8.%d", i+1))}
	}
	if err := validatePublicEndpointAddresses(addresses); err == nil || !strings.Contains(err.Error(), "too many") {
		t.Fatalf("validatePublicEndpointAddresses() error = %v", err)
	}
}
