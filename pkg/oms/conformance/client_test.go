package conformance

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

const testAuthorizationToken = "test-token"

func TestContractClientRequiresHTTPSByDefault(t *testing.T) {
	_, err := newContractClient(Target{
		BaseURL:            "http://127.0.0.1:8080",
		AuthorizationValue: testAuthorizationToken,
	})
	if err == nil {
		t.Fatal("newContractClient() accepted plaintext HTTP without an explicit loopback opt-in")
	}
}

func TestContractClientAllowsExplicitLiteralLoopbackHTTPWithoutProxy(t *testing.T) {
	client, err := newContractClient(Target{
		BaseURL:              "http://127.0.0.1:8080",
		AuthorizationValue:   testAuthorizationToken,
		InsecureLoopbackOnly: true,
	})
	if err != nil {
		t.Fatalf("newContractClient() error = %v", err)
	}
	transport, ok := client.http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T, want *http.Transport", client.http.Transport)
	}
	if transport.Proxy != nil {
		t.Fatal("insecure loopback client retained proxy support")
	}
}

func TestContractClientNeverAllowsBearerOverRemoteHTTP(t *testing.T) {
	for _, endpoint := range []string{
		"http://192.0.2.10:8080",
		"http://memory.example.com:8080",
		"http://localhost:8080",
	} {
		t.Run(endpoint, func(t *testing.T) {
			_, err := newContractClient(Target{
				BaseURL:              endpoint,
				AuthorizationValue:   testAuthorizationToken,
				InsecureLoopbackOnly: true,
			})
			if err == nil {
				t.Fatalf("newContractClient() accepted non-literal-loopback HTTP endpoint %q", endpoint)
			}
		})
	}
}

func TestBearerNormalizationSeparatesRawTokenAndAuthorizationValue(t *testing.T) {
	client, err := newContractClient(Target{
		BaseURL: "https://memory.example.com", AuthorizationValue: "Bearer conformance-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	if client.rawBearerToken != "conformance-secret" || client.authorizationValue != "Bearer conformance-secret" {
		t.Fatalf("credentials = raw %q authorization %q", client.rawBearerToken, client.authorizationValue)
	}
	body := []byte("provider echoed conformance-secret")
	if !containsCredential(body, client.authorizationValue) {
		t.Fatal("raw token leak was not detected from a full Authorization configuration")
	}
	if got := sanitizeOutputText(string(body), client.authorizationValue, 1024); got == string(body) {
		t.Fatal("raw token leak was not redacted")
	}
}

func TestContractClientPreservesAndValidatesClosedResponseHeaders(t *testing.T) {
	for _, tc := range []struct {
		name, contentType, cacheControl string
		wantErr                         bool
	}{
		{name: validTestCase, contentType: jsonMediaType, cacheControl: "private, no-store"},
		{name: "missing content type", cacheControl: "no-store", wantErr: true},
		{name: "wrong content type", contentType: "text/plain", cacheControl: "no-store", wantErr: true},
		{name: "missing no-store", contentType: jsonMediaType, cacheControl: "private", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				if tc.contentType != "" {
					writer.Header().Set("Content-Type", tc.contentType)
				}
				if tc.cacheControl != "" {
					writer.Header().Set("Cache-Control", tc.cacheControl)
				}
				_, _ = writer.Write([]byte(`{"protocolVersion":"orka.oms.v0alpha1","status":"ok"}`))
			}))
			t.Cleanup(server.Close)
			client, err := newContractClient(Target{
				BaseURL:              server.URL,
				AuthorizationValue:   testAuthorizationToken,
				HTTPClient:           server.Client(),
				InsecureLoopbackOnly: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			response, err := client.doResponse(
				context.Background(), http.MethodGet, "/v1/health", client.authorizationValue, nil, nil,
			)
			if (err != nil) != tc.wantErr {
				t.Fatalf("doResponse() error = %v, wantErr %v", err, tc.wantErr)
			}
			if !tc.wantErr && response.Header.Get("Cache-Control") != tc.cacheControl {
				t.Fatalf("preserved Cache-Control = %q, want %q", response.Header.Get("Cache-Control"), tc.cacheControl)
			}
		})
	}
}
