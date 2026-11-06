/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/workspace"
)

func reconcileEgressCredentialTool(t *testing.T, provider string, secret *corev1.Secret) (*recordingToolWorkspaceExecutor, corev1alpha1.Tool, []workspace.SubstrateEgressRule) {
	t.Helper()
	tool := newEgressTestMCPTool(corev1alpha1.SubstrateActorEgressRule{
		Scheme: "https", Host: "api.github.com",
		Credential: &corev1alpha1.SubstrateActorEgressCredential{Prefix: "Bearer ", SecretRef: corev1alpha1.SecretKeySelector{Name: "github-token", Key: "token"}},
	})
	objects := []client.Object{tool, approvedMCPActorTemplateForTest()}
	if secret != nil {
		objects = append(objects, secret)
	}
	scheme := newToolScheme()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	t.Cleanup(srv.Close)
	executor := &recordingToolWorkspaceExecutor{claimCreateds: []bool{true}}
	var applied []workspace.SubstrateEgressRule
	r := &ToolReconciler{
		Client:                    fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.Tool{}).WithObjects(objects...).Build(),
		Scheme:                    scheme,
		SubstrateRouterHTTPClient: srv.Client(),
		SubstrateMCPToolsEnabled:  true,
		SubstrateConfig:           SubstrateConfig{RouterURL: srv.URL, ClaimTimeout: 1, EgressCredentialProvider: provider},
		SubstrateEgressPolicyApplier: func(_ context.Context, _ SubstrateConfig, _ string, rules []workspace.SubstrateEgressRule) error {
			applied = rules
			_, err := workspace.CompileSubstrateActorEgressPolicy("ate-demo", rules)
			return err
		},
		SubstrateExecutorFactory: func(SubstrateConfig) (workspace.WorkspaceExecutor, error) { return executor, nil },
	}
	r.SubstrateTemplateValidator = substrateFixtureTemplateValidator(r.Client)
	for range 3 {
		_, _ = r.Reconcile(context.Background(), mcpToolRequest())
	}
	var got corev1alpha1.Tool
	if err := r.Get(context.Background(), types.NamespacedName{Name: "mcp-tool", Namespace: defaultNS}, &got); err != nil {
		t.Fatal(err)
	}
	return executor, got, applied
}

func githubTokenSecret(value string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "github-token", Namespace: defaultNS}, Data: map[string][]byte{"token": []byte(value)}}
}

func TestToolReconcilerCompilesEgressCredentialReference(t *testing.T) {
	executor, got, applied := reconcileEgressCredentialTool(t, workspace.SubstrateKubernetesSecretsCredentialProvider, githubTokenSecret("ghp_secret_value"))
	if !executor.waitReadyCalled || !got.Status.Available {
		t.Fatalf("status = %+v, want an available actor", got.Status)
	}
	if len(applied) != 1 || applied[0].Credential == nil {
		t.Fatalf("applied rules = %+v, want one credential rule", applied)
	}
	want := workspace.SubstrateEgressCredential{Header: "Authorization", Prefix: "Bearer ", Provider: "k8s.io", Namespace: defaultNS, Name: "github-token", Key: "token"}
	if *applied[0].Credential != want {
		t.Fatalf("credential = %+v, want %+v", *applied[0].Credential, want)
	}
}

func TestToolReconcilerEgressCredentialsFailClosed(t *testing.T) {
	for name, test := range map[string]struct {
		provider string
		secret   *corev1.Secret
		want     string
	}{
		"credential provider not configured": {"", githubTokenSecret("ghp_secret_value"), "not configured"},
		"missing Secret":                     {"k8s.io", nil, "not found"},
		"empty Secret key":                   {"k8s.io", githubTokenSecret(""), "empty or missing"},
	} {
		t.Run(name, func(t *testing.T) {
			executor, got, applied := reconcileEgressCredentialTool(t, test.provider, test.secret)
			if executor.waitReadyCalled || got.Status.Available || applied != nil {
				t.Fatalf("actor resumed or policy applied: available=%t applied=%+v", got.Status.Available, applied)
			}
			if !strings.Contains(got.Status.Error, test.want) {
				t.Fatalf("status error = %q, want %q", got.Status.Error, test.want)
			}
			if strings.Contains(got.Status.Error, "ghp_secret_value") {
				t.Fatal("Tool status leaked the credential value")
			}
		})
	}
}

func TestSubstrateConfigRejectsUnsupportedEgressCredentialProvider(t *testing.T) {
	cfg, err := SubstrateConfigFromEnv(func(name string) string {
		if name == EnvSubstrateEgressCredentialProvider {
			return "vault.example.com"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.EgressCredentialProvider != "vault.example.com" {
		t.Fatal("egress credential provider was not loaded from the environment")
	}
	cfg.APICAFile, cfg.APICertFile, cfg.APIKeyFile = "/ca", "/cert", "/key"
	if err := cfg.ValidateMCPTools(); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("unsupported credential provider accepted: %v", err)
	}
	cfg.EgressCredentialProvider = "k8s.io"
	if err := cfg.ValidateMCPTools(); err != nil {
		t.Fatal(err)
	}
}
