// Copyright (c) 2026. MIT License - see LICENSE file for details.

package agentidentity

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/outboundaccess"
)

func userToken(t *testing.T, audience any) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"aud": audience, "oid": "user"})
	if err != nil {
		t.Fatal(err)
	}
	return "e30." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func TestRequireAudience(t *testing.T) {
	for name, tc := range map[string]struct {
		audience any
		ok       bool
	}{
		"blueprint client ID":   {audience: testBlueprint, ok: true},
		"blueprint URI":         {audience: "api://" + strings.ToUpper(testBlueprint), ok: true},
		"list with blueprint":   {audience: []string{"other", testBlueprint}, ok: true},
		"another client":        {audience: "00000003-0000-0000-c000-000000000000"},
		"another client in URI": {audience: "api://" + testTenant},
	} {
		t.Run(name, func(t *testing.T) {
			err := requireAudience(userToken(t, tc.audience), testBlueprint)
			if (err == nil) != tc.ok {
				t.Fatalf("requireAudience() = %v, want ok=%v", err, tc.ok)
			}
		})
	}
	if err := requireAudience("not-a-jwt", testBlueprint); err == nil {
		t.Fatal("a non-JWT must be refused")
	}
}

func TestEntraOnBehalfOfExchange(t *testing.T) {
	fake := &fakeEntra{}
	now := time.Now()
	source := &Source{Config: configuredConfig(), Entra: newTestIssuer(t, fake, &now)}
	instance, _ := source.Config.Instance("entra")
	user := userToken(t, testBlueprint)
	request := outboundaccess.AgentIdentityTokenRequest{
		Identity:      corev1alpha1.AgentExecutionIdentityRef{Issuer: "entra", ID: strings.ToLower(testIdentity), Instance: instance},
		Mode:          corev1alpha1.OutboundAgentIdentityModeDelegated,
		Resource:      "https://storage.azure.com",
		UserAssertion: user,
	}
	token, err := source.AgentIdentityToken(context.Background(), request)
	if err != nil || token != "app-token-for-https://storage.azure.com/.default" {
		t.Fatalf("token = %q, err = %v", token, err)
	}
	obo := fake.requests[1]
	for key, want := range map[string]string{
		"grant_type": jwtBearerGrantType, "client_id": strings.ToLower(testIdentity),
		"client_assertion": "exchange-token", "client_assertion_type": jwtBearerAssertionType,
		"assertion": user, "requested_token_use": "on_behalf_of", "scope": "https://storage.azure.com/.default",
	} {
		if got := obo.Get(key); got != want {
			t.Fatalf("on-behalf-of %s = %q, want %q", key, got, want)
		}
	}

	if _, err := source.AgentIdentityToken(context.Background(), request); err != nil || fake.count() != 2 {
		t.Fatalf("a repeated call for the same user reuses the cache: err=%v requests=%d", err, fake.count())
	}
	other := request
	other.UserAssertion = userToken(t, []string{testBlueprint, "x"})
	if _, err := source.AgentIdentityToken(context.Background(), other); err != nil || fake.count() != 3 {
		t.Fatalf("another user's token is exchanged on its own: err=%v requests=%d", err, fake.count())
	}

	wrongAudience := request
	wrongAudience.UserAssertion = userToken(t, "00000003-0000-0000-c000-000000000000")
	if _, err := source.AgentIdentityToken(context.Background(), wrongAudience); err == nil || !strings.Contains(err.Error(), "blueprint") {
		t.Fatalf("a token for another client: error = %v", err)
	}
}
