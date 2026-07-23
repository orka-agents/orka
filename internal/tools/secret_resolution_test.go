/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/workerenv"
)

const (
	testOpencodePrimaryCandidate  = "opencode-credentials"
	testOpencodeFallbackCandidate = "opencode-api-key"
	testOpencodeEndpoint          = "https://gateway.example.invalid/v1"
	testOpencodeAuthValue         = "test"
	testOpencodeCustomRef         = "custom-opencode-secret"
	testSecretResolutionNamespace = "default"
)

func TestRuntimeSecretCandidatesOpencode(t *testing.T) {
	candidates := RuntimeSecretCandidates(corev1alpha1.AgentRuntimeOpencode)
	for _, want := range []string{testOpencodePrimaryCandidate, testOpencodeFallbackCandidate} {
		if !slices.Contains(candidates, want) {
			t.Fatalf("RuntimeSecretCandidates(opencode) = %#v, want %q", candidates, want)
		}
	}
	if slices.Contains(candidates, "openai-api-key") {
		t.Fatalf("RuntimeSecretCandidates(opencode) = %#v, want no generic OpenAI fallback", candidates)
	}

	candidates[0] = "changed-candidate"
	if got := RuntimeSecretCandidates(corev1alpha1.AgentRuntimeOpencode)[0]; got == "changed-candidate" {
		t.Fatal("RuntimeSecretCandidates returned shared backing storage")
	}
}

func TestResolveRuntimeSecretRefOpencodeAutoDiscoveryRequiresGatewayCredentials(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: testOpencodePrimaryCandidate, Namespace: testSecretResolutionNamespace},
			Data: map[string][]byte{
				workerenv.OpenAIAPIKey: []byte("name-match-only"),
			},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: testOpencodeFallbackCandidate, Namespace: testSecretResolutionNamespace},
			Data: map[string][]byte{
				workerenv.OpenAIBaseURL: []byte(testOpencodeEndpoint),
				workerenv.OpenAIAPIKey:  []byte(testOpencodeAuthValue),
			},
		},
	).Build()

	ref, err := resolveRuntimeSecretRef(
		context.Background(),
		client,
		testSecretResolutionNamespace,
		corev1alpha1.AgentRuntimeOpencode,
		"",
	)
	if err != nil {
		t.Fatalf("resolveRuntimeSecretRef() error = %v", err)
	}
	if ref == nil || ref.Name != testOpencodeFallbackCandidate {
		t.Fatalf("resolveRuntimeSecretRef() = %#v, want usable opencode-api-key candidate", ref)
	}
}

func TestResolveRuntimeSecretRefOpencodeAutoDiscoveryRejectsIncompleteCredentials(t *testing.T) {
	tests := []struct {
		name string
		data map[string][]byte
	}{
		{
			name: "missing base URL",
			data: map[string][]byte{workerenv.OpenAIAPIKey: []byte(testOpencodeAuthValue)},
		},
		{
			name: "blank base URL",
			data: map[string][]byte{
				workerenv.OpenAIBaseURL: []byte(" \t\n"),
				workerenv.OpenAIAPIKey:  []byte(testOpencodeAuthValue),
			},
		},
		{
			name: "missing API key",
			data: map[string][]byte{workerenv.OpenAIBaseURL: []byte(testOpencodeEndpoint)},
		},
		{
			name: "blank API key",
			data: map[string][]byte{
				workerenv.OpenAIBaseURL: []byte(testOpencodeEndpoint),
				workerenv.OpenAIAPIKey:  []byte(" \t\n"),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatalf("add core scheme: %v", err)
			}
			client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: testOpencodePrimaryCandidate, Namespace: testSecretResolutionNamespace},
				Data:       tt.data,
			}).Build()

			ref, err := resolveRuntimeSecretRef(
				context.Background(),
				client,
				testSecretResolutionNamespace,
				corev1alpha1.AgentRuntimeOpencode,
				"",
			)
			if err == nil {
				t.Fatalf("resolveRuntimeSecretRef() = %#v, want incomplete credential error", ref)
			}
			for _, key := range []string{workerenv.OpenAIBaseURL, workerenv.OpenAIAPIKey} {
				if !strings.Contains(err.Error(), key) {
					t.Fatalf("resolveRuntimeSecretRef() error = %q, want %s requirement", err, key)
				}
			}
		})
	}
}

func TestResolveRuntimeSecretRefOpencodeExplicitCustomSecretPreservesExistingBehavior(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testOpencodeCustomRef, Namespace: testSecretResolutionNamespace},
	}).Build()

	ref, err := resolveRuntimeSecretRef(
		context.Background(),
		client,
		testSecretResolutionNamespace,
		corev1alpha1.AgentRuntimeOpencode,
		testOpencodeCustomRef,
	)
	if err != nil {
		t.Fatalf("resolveRuntimeSecretRef() error = %v", err)
	}
	if ref == nil || ref.Name != testOpencodeCustomRef {
		t.Fatalf("resolveRuntimeSecretRef() = %#v, want explicit custom secret", ref)
	}
}

func TestResolveRuntimeSecretRefOtherRuntimeAutoDiscoveryStillUsesNameMatch(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: claudeCredentialsSecretName, Namespace: testSecretResolutionNamespace},
	}).Build()

	ref, err := resolveRuntimeSecretRef(
		context.Background(),
		client,
		testSecretResolutionNamespace,
		corev1alpha1.AgentRuntimeClaude,
		"",
	)
	if err != nil {
		t.Fatalf("resolveRuntimeSecretRef() error = %v", err)
	}
	if ref == nil || ref.Name != claudeCredentialsSecretName {
		t.Fatalf("resolveRuntimeSecretRef() = %#v, want name-matched Claude secret", ref)
	}
}
