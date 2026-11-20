/*
Copyright (c) 2026.
MIT License - see LICENSE file for details.
*/

package llm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBearerTransportSendsOnlyTheTokenToTheProviderOrigin(t *testing.T) {
	var seen http.Header
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	origin, err := httpsOrigin(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	transport := &bearerTransport{base: server.Client().Transport, origin: origin, source: func(context.Context) (string, error) {
		calls++
		return "entra-token", nil
	}}
	request, _ := http.NewRequest(http.MethodPost, server.URL+"/openai/deployments/x/chat/completions", strings.NewReader("{}"))
	request.Header.Set("Api-Key", "leaked-key")
	request.Header.Set("Authorization", "Bearer ambient-key")
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if got := seen.Get("Authorization"); got != "Bearer entra-token" {
		t.Fatalf("Authorization = %q", got)
	}
	if seen.Get("Api-Key") != "" {
		t.Fatal("an API key header reached the provider")
	}
	if request.Header.Get("Authorization") != "Bearer ambient-key" {
		t.Fatal("the caller's request was modified")
	}

	other, _ := http.NewRequest(http.MethodGet, "https://attacker.example/x", nil)
	if _, err := transport.RoundTrip(other); err == nil {
		t.Fatal("a request to another origin was sent")
	}
	if calls != 1 {
		t.Fatalf("token source called %d times, want only for the provider origin", calls)
	}
}

func TestBearerTransportFailsClosedWithoutAToken(t *testing.T) {
	transport := &bearerTransport{base: http.DefaultTransport, origin: "https://example.openai.azure.com", source: func(context.Context) (string, error) {
		return "", errors.New("issuer down")
	}}
	request, _ := http.NewRequest(http.MethodGet, "https://example.openai.azure.com/openai/v1/models", nil)
	_, err := transport.RoundTrip(request)
	var providerErr *ProviderError
	if !errors.As(err, &providerErr) || providerErr.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("error = %v, want a 503 ProviderError", err)
	}
}

func TestNewBearerHTTPClientRefusesRedirectsAndPlainHTTP(t *testing.T) {
	if _, err := NewBearerHTTPClient("http://example.openai.azure.com", func(context.Context) (string, error) { return "t", nil }); err == nil {
		t.Fatal("an http endpoint was accepted")
	}
	if _, err := NewBearerHTTPClient("https://example.openai.azure.com", nil); err == nil {
		t.Fatal("a nil token source was accepted")
	}
	client, err := NewBearerHTTPClient("https://Example.OpenAI.Azure.com:443/openai", func(context.Context) (string, error) { return "t", nil })
	if err != nil {
		t.Fatal(err)
	}
	if client.CheckRedirect == nil || !errors.Is(client.CheckRedirect(nil, nil), http.ErrUseLastResponse) {
		t.Fatal("the client follows redirects")
	}
	if transport, ok := client.Transport.(*bearerTransport); !ok || transport.origin != "https://example.openai.azure.com" {
		t.Fatalf("transport = %#v", client.Transport)
	}
}
