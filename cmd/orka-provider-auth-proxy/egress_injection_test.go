package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

// An Agent Substrate egress gateway replaces the runtime's placeholder
// Authorization value with the configured prefix ("Bearer ") plus the Secret
// value. The proxy must accept that header unchanged and must never accept
// the public placeholder a runtime sends when the gateway is bypassed.
func TestProviderAuthProxyAcceptsGatewayInjectedBearerOnly(t *testing.T) {
	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		if r.Header.Get(authorizationHeader) != "" {
			t.Fatal("upstream received the injected proxy credential")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(upstream.Close)
	proxy := newTestProxy(t, upstream.URL, testSharedProviderToken)
	for name, test := range map[string]struct {
		authorization string
		want          int
	}{
		"gateway-injected":    {"Bearer " + testSharedProviderToken, http.StatusNoContent},
		"runtime placeholder": {"Bearer " + harnessv2.ProviderTokenEgressInjectionPlaceholder, http.StatusUnauthorized},
		"bare placeholder":    {harnessv2.ProviderTokenEgressInjectionPlaceholder, http.StatusUnauthorized},
	} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "http://proxy/v1/responses", strings.NewReader(`{}`))
			request.Header.Set(authorizationHeader, test.authorization)
			response := httptest.NewRecorder()
			proxy.ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d", response.Code, test.want)
			}
		})
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstream calls = %d, want only the injected request", upstreamCalls)
	}
}
