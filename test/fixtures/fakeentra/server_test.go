// Copyright (c) 2026. MIT License - see LICENSE file for details.

package fakeentra_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/agentidentity"
	"github.com/orka-agents/orka/test/fixtures/fakeentra"
)

const (
	tenant    = "11111111-1111-4111-8111-111111111111"
	blueprint = "22222222-2222-4222-8222-222222222222"
	identity  = "33333333-3333-4333-8333-333333333333"
	stranger  = "44444444-4444-4444-8444-444444444444"
	resource  = "api://orka-e2e-identity-probe"
	assertion = "fixture-controller-credential"
)

// The fake is driven here with the controller's own Entra client, so it
// answers exactly the requests the controller sends.
func newFixture(t *testing.T) (*httptest.Server, func(credential string) *agentidentity.Source) {
	t.Helper()
	fake := fakeentra.New(fakeentra.Config{
		TenantID: tenant, BlueprintClientID: blueprint, Assertion: assertion,
		Identities: []string{identity}, Resource: resource, TokenLifetime: time.Hour,
	})
	server := httptest.NewTLSServer(fake.Handler())
	t.Cleanup(server.Close)
	source := func(credential string) *agentidentity.Source {
		config := agentidentity.EntraConfig{TenantID: tenant, BlueprintClientID: blueprint, Authority: server.URL}
		return &agentidentity.Source{
			Config: agentidentity.Config{Entra: &config, AgentUseAuthorization: true},
			Entra: &agentidentity.EntraIssuer{
				Config: config, HTTPClient: server.Client(),
				Credential: func() (string, error) { return credential, nil },
			},
		}
	}
	return server, source
}

func agentWith(id string) *corev1alpha1.Agent {
	return &corev1alpha1.Agent{Spec: corev1alpha1.AgentSpec{Identity: &corev1alpha1.AgentIdentity{
		Entra: &corev1alpha1.EntraAgentIdentity{AgentIdentityID: id},
	}}}
}

func state(t *testing.T, server *httptest.Server) fakeentra.State {
	t.Helper()
	response, err := server.Client().Get(server.URL + "/fixture/state")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	var decoded fakeentra.State
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

func probe(t *testing.T, server *httptest.Server, token string) int {
	t.Helper()
	request, _ := http.NewRequest(http.MethodGet, server.URL+fakeentra.ProbePath, nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	return response.StatusCode
}

func TestTheControllerFlowGetsATokenTheProbeAccepts(t *testing.T) {
	server, source := newFixture(t)
	ctx := context.Background()
	if err := source(assertion).Verify(ctx, agentWith(identity)); err != nil {
		t.Fatalf("verify: %v", err)
	}
	token, err := source(assertion).Entra.AppToken(ctx, identity, resource)
	if err != nil {
		t.Fatalf("app token: %v", err)
	}
	if status := probe(t, server, token); status != http.StatusOK {
		t.Fatalf("probe status = %d", status)
	}
	verification, err := source(assertion).Entra.AppToken(ctx, identity, "api://AzureADTokenExchange")
	if err != nil {
		t.Fatal(err)
	}
	if status := probe(t, server, verification); status != http.StatusUnauthorized {
		t.Fatalf("a token for another resource must be refused, status = %d", status)
	}
	got := state(t, server)
	if len(got.Probes) != 2 || !got.Probes[0].Authorized || got.Probes[0].Identity != identity ||
		got.Probes[1].Authorized {
		t.Fatalf("probes = %+v", got.Probes)
	}
	var app []string
	for _, request := range got.Requests {
		if request.Error != "" {
			t.Fatalf("unexpected refusal %+v", request)
		}
		if request.Step == "app" {
			app = append(app, request.Scope)
		}
	}
	const verifyScope = "api://AzureADTokenExchange/.default"
	if strings.Join(app, ",") != verifyScope+","+resource+"/.default,"+verifyScope {
		t.Fatalf("app scopes = %v", app)
	}
	if len(got.Issued) == 0 || !strings.HasPrefix(got.Issued[0], "fx-entra-exchange-") {
		t.Fatalf("issued = %v", got.Issued)
	}
}

func TestTheFakeRefusesWhatEntraRefuses(t *testing.T) {
	server, source := newFixture(t)
	ctx := context.Background()
	if _, err := source("another-credential").Entra.AppToken(ctx, identity, resource); err == nil ||
		!strings.Contains(err.Error(), "AADSTS700213") {
		t.Fatalf("a wrong controller credential: %v", err)
	}
	err := source(assertion).Verify(ctx, agentWith(stranger))
	if err == nil || !strings.Contains(err.Error(), "AADSTS700016") {
		t.Fatalf("an identity outside the blueprint: %v", err)
	}
	if status := probe(t, server, "fx-entra-app-forged"); status != http.StatusUnauthorized {
		t.Fatalf("a forged token: status = %d", status)
	}
}

func TestADisabledIdentityFailsVerification(t *testing.T) {
	server, source := newFixture(t)
	ctx := context.Background()
	response, err := server.Client().Post(server.URL+"/fixture/identities/"+strings.ToUpper(identity)+"/disable", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("disable status = %d", response.StatusCode)
	}
	err = source(assertion).Verify(ctx, agentWith(identity))
	if err == nil || !strings.Contains(err.Error(), "AADSTS7000112") {
		t.Fatalf("a disabled identity: %v", err)
	}
	last := state(t, server).Requests
	if got := last[len(last)-1]; got.Step != "app" || got.ErrorCode != fakeentra.DisabledErrorCode {
		t.Fatalf("last request = %+v", got)
	}
}
