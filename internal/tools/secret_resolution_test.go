/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func TestRuntimeSecretCandidatesOpencodeIsEmpty(t *testing.T) {
	if candidates := RuntimeSecretCandidates(corev1alpha1.AgentRuntimeOpencode); len(candidates) != 0 {
		t.Fatalf("RuntimeSecretCandidates(opencode) = %#v, want no Agent Secret candidates", candidates)
	}
}

func TestFirstUsableRuntimeSecretNameDoesNotDiscoverOpencodeSecrets(t *testing.T) {
	secrets := []corev1.Secret{
		{ObjectMeta: metav1.ObjectMeta{Name: "opencode-credentials", Namespace: defaultNamespace}},
		{ObjectMeta: metav1.ObjectMeta{Name: "opencode-api-key", Namespace: defaultNamespace}},
	}
	if got := FirstUsableRuntimeSecretName(secrets, corev1alpha1.AgentRuntimeOpencode); got != "" {
		t.Fatalf("FirstUsableRuntimeSecretName(opencode) = %q, want no auto-discovered Agent Secret", got)
	}
}
