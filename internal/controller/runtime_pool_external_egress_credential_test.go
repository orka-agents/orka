// Copyright (c) 2026. MIT License - see LICENSE file for details.

package controller

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func findExternalProviderSecret(t *testing.T, f *externalRuntimePoolFixture) *corev1.Secret {
	t.Helper()
	secrets := &corev1.SecretList{}
	if err := f.r.List(t.Context(), secrets, client.MatchingLabels{runtimePoolProviderCredentialLabel: booleanTrueValue}); err != nil {
		t.Fatal(err)
	}
	if len(secrets.Items) != 1 {
		t.Fatalf("provider Secrets = %d, want 1", len(secrets.Items))
	}
	return &secrets.Items[0]
}

func findExternalAuthSecret(t *testing.T, f *externalRuntimePoolFixture) *corev1.Secret {
	t.Helper()
	secrets := &corev1.SecretList{}
	if err := f.r.List(t.Context(), secrets, client.MatchingLabels{runtimePoolAuthLabel: booleanTrueValue}); err != nil {
		t.Fatal(err)
	}
	if len(secrets.Items) != 1 {
		t.Fatalf("auth Secrets = %d, want 1", len(secrets.Items))
	}
	return &secrets.Items[0]
}

func newNativeSubstrateV1Fixture(t *testing.T) *externalRuntimePoolFixture {
	t.Helper()
	f := newExternalRuntimePoolFixture(t)
	f.advertiseNativeProcess(t)
	f.advertiseSubstrateV1Backend(t)
	return f
}

func TestExternalNativeWorkspaceKeepsProviderTokenOutOfTheSandbox(t *testing.T) {
	f := newNativeSubstrateV1Fixture(t)
	w := f.publish(t)
	encoded, err := json.Marshal(w.Spec.Workload)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, runtimePoolTestProviderToken) {
		t.Fatal("the published workload request carries the provider-proxy token")
	}
	provider, auth := findExternalProviderSecret(t, f), findExternalAuthSecret(t, f)
	bootstrap, err := externalNativeCredentialBootstrap(w.Spec.Workload, auth, provider)
	if err != nil {
		t.Fatal(err)
	}
	if bootstrap.ProviderToken != harnessv2.ProviderTokenEgressInjectionPlaceholder || strings.Contains(bootstrap.ProviderToken, string(runtimePoolTestProviderToken)) {
		t.Fatal("the native bootstrap delivers the provider-proxy token instead of the public placeholder")
	}
	if bootstrap.ControllerToken == "" || bootstrap.CapabilitySecret == "" {
		t.Fatal("the native bootstrap lost its runtime control credentials")
	}
	// A bootstrap for a different provider Secret than the admitted rule names
	// is stale and refused rather than seeding the placeholder.
	rotated := provider.DeepCopy()
	rotated.Name += "-rotated"
	if _, err := externalNativeCredentialBootstrap(w.Spec.Workload, auth, rotated); err == nil {
		t.Fatal("bootstrap accepted a provider Secret the admitted egress rule does not name")
	}
}

func TestExternalNativeWorkspaceRefusesWithoutProviderTokenInjection(t *testing.T) {
	f := newNativeSubstrateV1Fixture(t)
	f.withdrawEgressCredentialInjection(t)
	for range 6 {
		runtimePoolReconcile(t, f.r, f.pool)
	}
	if f.currentWorkspace(t).Spec.Workload != nil {
		t.Fatal("published a native request that would deliver the provider token into the sandbox")
	}
	status := runtimePoolTestGetPool(t, f.r, f.pool).Status
	if status.AdmissionState != "Closed" || !strings.Contains(status.Message, "provider proxy token injection is unavailable") || !strings.Contains(status.Message, "--acp-workspace-allow-provider-token-in-sandbox") {
		t.Fatalf("refusal is not reported clearly: %q", status.Message)
	}
}

func TestExternalNativeWorkspaceExplicitSandboxTokenFallback(t *testing.T) {
	f := newNativeSubstrateV1Fixture(t)
	f.withdrawEgressCredentialInjection(t)
	f.r.AllowProviderTokenInSandbox = true
	w := f.publish(t)
	runtime := w.Spec.Workload.Runtime
	if runtime.Egress == nil || runtime.Egress.HasCredentialInjection() || slices.Contains(runtime.RequiredFeatures, workspacev1alpha1.WorkspaceFeatureEgressCredentialInjection) {
		t.Fatal("explicit fallback still requested gateway credential injection")
	}
	bootstrap, err := externalNativeCredentialBootstrap(w.Spec.Workload, findExternalAuthSecret(t, f), findExternalProviderSecret(t, f))
	if err != nil {
		t.Fatal(err)
	}
	if bootstrap.ProviderToken != string(runtimePoolTestProviderToken) {
		t.Fatal("explicit fallback did not deliver the provider token through the sealed bootstrap")
	}
}

func TestExternalNativeWorkspaceRefusesInexpressibleInjection(t *testing.T) {
	f := newNativeSubstrateV1Fixture(t)
	f.r.ProviderProxy.BaseURL = "https://provider-auth-proxy.orka-system.svc:8443"
	for range 6 {
		runtimePoolReconcile(t, f.r, f.pool)
	}
	if f.currentWorkspace(t).Spec.Workload != nil {
		t.Fatal("published a TLS provider-proxy origin that the gateway cannot inject into")
	}
	if message := runtimePoolTestGetPool(t, f.r, f.pool).Status.Message; !strings.Contains(message, "requires an http provider proxy origin") {
		t.Fatalf("refusal does not explain the TLS origin: %q", message)
	}
}

func TestExternalRuntimeEgressProviderTokenInjectionShape(t *testing.T) {
	injection := &externalProviderTokenInjection{namespace: "orka-runtimes", name: "pool-provider-e1-gabc"}
	cfg := runtimePoolConfig{providerProxy: runtimePoolProviderProxyConfig{baseURL: "http://provider-auth-proxy.orka-system.svc:8080"}}
	r := &RuntimePoolReconciler{ControllerAPIURL: "https://orka-api.orka-system.svc"}
	policy, err := r.externalRuntimeEgress(cfg, injection)
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range policy.Rules {
		if injected := len(rule.ReplaceHeaders) != 0; injected != (rule.Host == "provider-auth-proxy.orka-system.svc") {
			t.Fatalf("credential replacement on the wrong origin: %#v", rule)
		}
	}
	request := &workspacev1alpha1.WorkloadRequest{Runtime: &workspacev1alpha1.RuntimeWorkload{Egress: policy}}
	if got, err := externalRuntimeProviderTokenInjection(request); err != nil || got == nil || *got != *injection {
		t.Fatalf("admitted injection = %#v, %v", got, err)
	}
	for name, controllerURL := range map[string]string{
		"shared host":      "http://provider-auth-proxy.orka-system.svc:9090",
		"shared origin":    "http://provider-auth-proxy.orka-system.svc:8080/api",
		"TLS proxy origin": "",
	} {
		r := &RuntimePoolReconciler{ControllerAPIURL: controllerURL}
		cfg := cfg
		if controllerURL == "" {
			r.ControllerAPIURL = "https://orka-api.orka-system.svc"
			cfg.providerProxy.baseURL = "https://provider-auth-proxy.orka-system.svc"
		}
		if _, err := r.externalRuntimeEgress(cfg, injection); err == nil {
			t.Errorf("%s: injection was admitted", name)
		}
	}
	for name, mutate := range map[string]func(*workspacev1alpha1.RuntimeEgressCredentialHeader){
		"other header": func(h *workspacev1alpha1.RuntimeEgressCredentialHeader) { h.Header = "x-api-key" },
		"other key":    func(h *workspacev1alpha1.RuntimeEgressCredentialHeader) { h.SecretKeyRef.Key = "other" },
		"no prefix":    func(h *workspacev1alpha1.RuntimeEgressCredentialHeader) { h.Prefix = "" },
		"workload ns":  func(h *workspacev1alpha1.RuntimeEgressCredentialHeader) { h.SecretKeyRef.Namespace = "" },
	} {
		tampered := request.DeepCopy()
		for i := range tampered.Runtime.Egress.Rules {
			for j := range tampered.Runtime.Egress.Rules[i].ReplaceHeaders {
				mutate(&tampered.Runtime.Egress.Rules[i].ReplaceHeaders[j])
			}
		}
		if _, err := externalRuntimeProviderTokenInjection(tampered); err == nil {
			t.Errorf("%s: unadmitted credential replacement was accepted", name)
		}
	}
}

func TestRuntimePoolProviderTokenRejectsInjectionPlaceholder(t *testing.T) {
	if err := validateRuntimePoolProviderToken([]byte(harnessv2.ProviderTokenEgressInjectionPlaceholder)); err == nil {
		t.Fatal("the public injection placeholder was accepted as the provider-proxy token")
	}
}
