// Copyright (c) 2026. MIT License - see LICENSE file for details.

package agentidentity

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

const (
	testTenant    = "11111111-1111-1111-1111-111111111111"
	testBlueprint = "22222222-2222-2222-2222-222222222222"
	testIdentity  = "33333333-3333-3333-3333-33333333AAAA"
)

func configuredConfig() Config {
	return Config{Entra: &EntraConfig{TenantID: testTenant, BlueprintClientID: testBlueprint}, AgentUseAuthorization: true}
}

func identityAgent(name string, created time.Time) corev1alpha1.Agent {
	return corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team-a", UID: types.UID("uid-" + name), CreationTimestamp: metav1.NewTime(created)},
		Spec: corev1alpha1.AgentSpec{
			Runtime:  &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeClaude, Ceiling: &corev1alpha1.AgentRuntimeCeiling{AllowedTools: []string{"Read"}}},
			Identity: &corev1alpha1.AgentIdentity{Entra: &corev1alpha1.EntraAgentIdentity{AgentIdentityID: testIdentity}},
		},
	}
}

func TestConfigValidate(t *testing.T) {
	if err := (Config{}).Validate(); err != nil {
		t.Fatalf("no issuer: %v", err)
	}
	if err := configuredConfig().Validate(); err != nil {
		t.Fatalf("configured issuer: %v", err)
	}
	for _, entra := range []EntraConfig{{TenantID: testTenant}, {BlueprintClientID: testBlueprint}, {TenantID: "tenant", BlueprintClientID: testBlueprint}} {
		if err := (Config{Entra: &entra}).Validate(); err == nil {
			t.Fatalf("Validate(%+v) = nil, want error", entra)
		}
	}
}

func TestValidateAgentRequirements(t *testing.T) {
	agent := identityAgent("a", time.Now())
	if err := configuredConfig().ValidateAgent(&agent); err != nil {
		t.Fatalf("valid identity Agent: %v", err)
	}
	cases := map[string]struct {
		config Config
		mutate func(*corev1alpha1.Agent)
		want   string
	}{
		"issuer not configured":     {config: Config{AgentUseAuthorization: true}, want: "not configured"},
		"use authorization off":     {config: Config{Entra: configuredConfig().Entra}, want: "--agent-use-authorization-enabled"},
		"native agent":              {config: configuredConfig(), mutate: func(a *corev1alpha1.Agent) { a.Spec.Runtime = nil }, want: "runtime Agents"},
		"no tool ceiling":           {config: configuredConfig(), mutate: func(a *corev1alpha1.Agent) { a.Spec.Runtime.Ceiling = nil }, want: "ceiling.allowedTools"},
		"ceiling without tool list": {config: configuredConfig(), mutate: func(a *corev1alpha1.Agent) { a.Spec.Runtime.Ceiling.AllowedTools = nil }, want: "ceiling.allowedTools"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			candidate := identityAgent("a", time.Now())
			if tc.mutate != nil {
				tc.mutate(&candidate)
			}
			if err := tc.config.ValidateAgent(&candidate); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ValidateAgent() = %v, want %q", err, tc.want)
			}
		})
	}
	plain := corev1alpha1.Agent{}
	if err := (Config{}).ValidateAgent(&plain); err != nil {
		t.Fatalf("Agent without identity: %v", err)
	}
}

func TestBindingRefFreezesIssuerInstance(t *testing.T) {
	agent := identityAgent("a", time.Now())
	ref, err := configuredConfig().BindingRef(&agent)
	if err != nil {
		t.Fatal(err)
	}
	want := corev1alpha1.AgentExecutionIdentityRef{
		Issuer: "entra", ID: strings.ToLower(testIdentity), Instance: "entra:" + testTenant + "/" + testBlueprint,
	}
	if *ref != want {
		t.Fatalf("BindingRef() = %+v, want %+v", *ref, want)
	}
}

func TestClaimantIsTheOldestLiveAgent(t *testing.T) {
	now := time.Now()
	older, newer := identityAgent("older", now.Add(-time.Hour)), identityAgent("newer", now)
	newer.Spec.Identity.Entra.AgentIdentityID = strings.ToLower(testIdentity)
	if owner := Claimant([]corev1alpha1.Agent{newer, older}, "entra", strings.ToLower(testIdentity)); owner == nil || owner.Name != "older" {
		t.Fatalf("Claimant() = %v, want the older Agent; IDs compare case-insensitively", owner)
	}
	deleting := metav1.NewTime(now)
	older.DeletionTimestamp = &deleting
	if owner := Claimant([]corev1alpha1.Agent{newer, older}, "entra", strings.ToLower(testIdentity)); owner == nil || owner.Name != "newer" {
		t.Fatalf("Claimant() = %v, want the live Agent once the older one is deleting", owner)
	}
}
