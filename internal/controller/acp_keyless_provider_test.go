/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

// An agent Task reaches a keyless Provider only through the provider auth
// proxy's Entra mode, for the same identity and origin, on the ACP path.
func TestAgentTaskAdmitsAKeylessProviderOnlyOnTheProxyPath(t *testing.T) {
	scheme := newTestScheme()
	keyed := &corev1alpha1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: "keyed", Namespace: "default"},
		Spec: corev1alpha1.ProviderSpec{Type: corev1alpha1.ProviderTypeOpenAI,
			SecretRef: &corev1alpha1.ProviderSecretRef{Name: "key"}},
		Status: corev1alpha1.ProviderStatus{Ready: true},
	}
	r := newUnitReconciler(scheme, readyKeylessProvider("azure"), keyed)
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "review", Namespace: "default"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
	}
	agentFor := func(provider string) *corev1alpha1.Agent {
		agent := &corev1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "reviewer", Namespace: "default"}}
		if provider != "" {
			agent.Spec.ProviderRef = &corev1alpha1.ProviderReference{Name: provider}
		}
		return agent
	}
	matching := ACPProviderProxyEntra{Identity: "models", Endpoint: "https://EXAMPLE.openai.azure.com:443/"}

	for name, tc := range map[string]struct {
		proxy    ACPProviderProxyEntra
		provider string
		path     agentExecutionPath
		admit    bool
	}{
		"matching proxy on the ACP path": {proxy: matching, provider: "azure", path: agentExecutionPathACP, admit: true},
		"no proxy Entra mode":            {provider: "azure", path: agentExecutionPathACP},
		"other identity":                 {proxy: ACPProviderProxyEntra{Identity: "other", Endpoint: matching.Endpoint}, provider: "azure", path: agentExecutionPathACP},
		"other origin": {proxy: ACPProviderProxyEntra{Identity: "models", Endpoint: "https://other.openai.azure.com"},
			provider: "azure", path: agentExecutionPathACP},
		"external runtime": {proxy: matching, provider: "azure", path: agentExecutionPathExternal},
		"keyed Provider":   {provider: "keyed", path: agentExecutionPathACP, admit: true},
		"no Provider":      {path: agentExecutionPathACP, admit: true},
		"missing Provider": {provider: "absent", path: agentExecutionPathACP, admit: true},
	} {
		t.Run(name, func(t *testing.T) {
			r.ACPProviderProxyEntra = tc.proxy
			err := r.checkAgentTaskKeylessProvider(context.Background(), task, agentFor(tc.provider), tc.path)
			if tc.admit && err != nil {
				t.Fatalf("check = %v, want the Task admitted", err)
			}
			if !tc.admit && (err == nil || !strings.Contains(err.Error(), "spec.auth")) {
				t.Fatalf("check = %v, want the keyless Provider refused", err)
			}
		})
	}
}
