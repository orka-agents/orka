// Copyright (c) 2026. MIT License - see LICENSE file for details.

package fakeentra_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/agentidentity"
	"github.com/orka-agents/orka/internal/provideridentity"
	"github.com/orka-agents/orka/test/fixtures/fakeentra"
)

func TestFakeEntraModelEndpointRefusesAgentIdentityTokens(t *testing.T) {
	server, _ := newModelFixture(t)
	config := agentidentity.EntraConfig{TenantID: tenant, BlueprintClientID: blueprint, Authority: server.URL}
	issuer := &agentidentity.EntraIssuer{
		Config: config, HTTPClient: server.Client(),
		Credential: func() (string, error) { return assertion, nil },
	}
	token, err := issuer.AppToken(context.Background(), identity, modelResource)
	if err != nil {
		t.Fatalf("AppToken() error = %v", err)
	}
	status, _ := callModel(t, server, "/openai/deployments/gpt-x/chat/completions?api-version=1",
		`{"messages":[]}`, http.Header{"Authorization": {"Bearer " + token}})
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want an agent identity's token refused", status)
	}
}

const (
	workload      = "55555555-5555-4555-8555-555555555555"
	modelResource = "https://ai.azure.com"
)

func newModelFixture(t *testing.T) (*httptest.Server, func(credential string) *provideridentity.Source) {
	t.Helper()
	fake := fakeentra.New(fakeentra.Config{
		TenantID: tenant, BlueprintClientID: blueprint, Assertion: assertion,
		Identities: []string{identity}, Resource: resource, TokenLifetime: time.Hour,
		Workloads: []string{strings.ToUpper(workload)}, ModelResource: modelResource,
	})
	server := httptest.NewTLSServer(fake.Handler())
	t.Cleanup(server.Close)
	source := func(credential string) *provideridentity.Source {
		source, err := provideridentity.NewSource(provideridentity.Config{Identities: []provideridentity.Identity{{
			Name: "models", TenantID: tenant, ClientID: workload, Authority: server.URL,
			AllowedEndpoints: []string{server.URL},
		}}}, func() (string, error) { return credential, nil })
		if err != nil {
			t.Fatal(err)
		}
		source.HTTPClient = server.Client()
		return source
	}
	return server, source
}

func callModel(t *testing.T, server *httptest.Server, path, body string, header http.Header) (int, string) {
	t.Helper()
	request, _ := http.NewRequest(http.MethodPost, server.URL+path, strings.NewReader(body))
	request.Header = header
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	data, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(data)
}

func TestFakeEntraServesAzureOpenAIToItsWorkloadIdentityOnly(t *testing.T) {
	server, source := newModelFixture(t)
	token, err := source(assertion).Token(context.Background(), "models", server.URL)
	if err != nil {
		t.Fatalf("Token() error = %v", err)
	}
	bearer := http.Header{"Authorization": {"Bearer " + token}}
	const chat = "/openai/deployments/gpt-x/chat/completions?api-version=2025-03-01-preview"

	status, body := callModel(t, server, chat, `{"messages":[{"role":"user","content":"marker-1"}]}`, bearer)
	var completion struct {
		Choices []struct {
			Message struct{ Content string } `json:"message"`
		} `json:"choices"`
	}
	err = json.Unmarshal([]byte(body), &completion)
	if status != http.StatusOK || err != nil || len(completion.Choices) != 1 ||
		completion.Choices[0].Message.Content != fakeentra.ModelReplyPrefix+" marker-1" {
		t.Fatalf("chat completion = %d %s", status, body)
	}
	status, body = callModel(t, server, chat, `{"stream":true,"messages":[{"role":"user","content":"marker-2"}]}`, bearer)
	if status != http.StatusOK || !strings.Contains(body, fakeentra.ModelReplyPrefix+" marker-2") ||
		!strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Fatalf("streamed chat completion = %d %s", status, body)
	}
	status, _ = callModel(t, server, "/openai/responses?api-version=preview", `{}`, bearer)
	if status != http.StatusNotFound {
		t.Fatalf("responses = %d, want the 404 clients fall back on", status)
	}

	if _, err := source("another-credential").Token(context.Background(), "models", server.URL); err == nil {
		t.Fatal("a workload token was issued for the wrong federated credential")
	}
	for name, header := range map[string]http.Header{
		"no token":                   {},
		"unknown token":              {"Authorization": {"Bearer fx-entra-workload-forged"}},
		"token and api key":          {"Authorization": {"Bearer " + token}, "Api-Key": {"key"}},
		"token and a second api key": {"Authorization": {"Bearer " + token}, "Api-Key": {"", "key"}},
	} {
		if status, _ := callModel(t, server, chat, `{"messages":[]}`, header); status != http.StatusUnauthorized {
			t.Fatalf("%s: status = %d, want 401", name, status)
		}
	}

	got := state(t, server)
	if len(got.Models) != 7 || !got.Models[0].Authorized || got.Models[0].Identity != workload ||
		!got.Models[1].Stream {
		t.Fatalf("model calls = %+v", got.Models)
	}
	recorded, _ := json.Marshal(got.Models)
	for _, issued := range got.Issued {
		if strings.Contains(string(recorded), issued) {
			t.Fatal("a token reached the recorded model calls")
		}
	}
}
