/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/workspace"
)

type egressPolicyApplication struct {
	actorID string
	rules   []workspace.SubstrateEgressRule
}

func reconcileEgressCredentialTool(t *testing.T, provider string, secret *corev1.Secret) (*recordingToolWorkspaceExecutor, corev1alpha1.Tool, []workspace.SubstrateEgressRule) {
	t.Helper()
	r, executor, applications := newEgressCredentialToolReconciler(t, provider, secret)
	for range 3 {
		_, _ = r.Reconcile(context.Background(), mcpToolRequest())
	}
	var applied []workspace.SubstrateEgressRule
	if len(*applications) > 0 {
		applied = (*applications)[len(*applications)-1].rules
	}
	return executor, getEgressCredentialTool(t, r), applied
}

func getEgressCredentialTool(t *testing.T, r *ToolReconciler) corev1alpha1.Tool {
	t.Helper()
	var got corev1alpha1.Tool
	if err := r.Get(context.Background(), types.NamespacedName{Name: "mcp-tool", Namespace: defaultNS}, &got); err != nil {
		t.Fatal(err)
	}
	return got
}

func newEgressCredentialToolReconciler(t *testing.T, provider string, secret *corev1.Secret) (*ToolReconciler, *recordingToolWorkspaceExecutor, *[]egressPolicyApplication) {
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
	applications := &[]egressPolicyApplication{}
	r := &ToolReconciler{
		Client:                    fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.Tool{}).WithObjects(objects...).Build(),
		Scheme:                    scheme,
		SubstrateRouterHTTPClient: srv.Client(),
		SubstrateMCPToolsEnabled:  true,
		SubstrateConfig:           SubstrateConfig{RouterURL: srv.URL, ClaimTimeout: 1, EgressCredentialProvider: provider},
		SubstrateEgressPolicyApplier: func(_ context.Context, _ SubstrateConfig, actorID string, rules []workspace.SubstrateEgressRule) error {
			*applications = append(*applications, egressPolicyApplication{actorID: actorID, rules: rules})
			_, err := workspace.CompileSubstrateActorEgressPolicy("ate-demo", rules)
			return err
		},
		SubstrateExecutorFactory: func(SubstrateConfig) (workspace.WorkspaceExecutor, error) { return executor, nil },
	}
	r.SubstrateTemplateValidator = substrateFixtureTemplateValidator(r.Client)
	return r, executor, applications
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

// A running credential-bearing actor whose Tool stops validating must lose
// all egress and be stopped, not keep the last installed policy and its
// injected credential while the gateway still caches it.
func TestToolReconcilerRevokesRunningActorEgressWhenCredentialBecomesInvalid(t *testing.T) {
	for name, invalidate := range map[string]func(t *testing.T, r *ToolReconciler){
		"credential provider cleared": func(_ *testing.T, r *ToolReconciler) { r.SubstrateConfig.EgressCredentialProvider = "" },
		"credential Secret deleted": func(t *testing.T, r *ToolReconciler) {
			if err := r.Delete(context.Background(), githubTokenSecret("")); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			r, executor, applications := newEgressCredentialToolReconciler(t, workspace.SubstrateKubernetesSecretsCredentialProvider, githubTokenSecret("ghp_secret_value"))
			for range 3 {
				_, _ = r.Reconcile(context.Background(), mcpToolRequest())
			}
			running := getEgressCredentialTool(t, r)
			if !running.Status.Available || running.Status.Actor == nil || len(*applications) == 0 {
				t.Fatalf("status = %+v, want an available actor with an installed policy", running.Status)
			}
			actorID := running.Status.Actor.ActorID

			invalidate(t, r)
			*applications = nil
			if _, err := r.Reconcile(context.Background(), mcpToolRequest()); err != nil {
				t.Fatal(err)
			}
			if len(*applications) != 1 || (*applications)[0].actorID != actorID || len((*applications)[0].rules) != 0 {
				t.Fatalf("applications = %+v, want one deny-all policy for actor %q", *applications, actorID)
			}
			got := getEgressCredentialTool(t, r)
			condition := meta.FindStatusCondition(got.Status.Conditions, toolConditionEgressPolicyReady)
			if got.Status.Available || condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != "EgressRevoked" {
				t.Fatalf("status = %+v, want an unavailable Tool with revoked egress", got.Status)
			}
			if stops := len(executor.releaseReqs) + len(executor.deleteReqs); stops != 1 {
				t.Fatalf("releases=%v deletes=%v, want the actor stopped once", executor.releaseReqs, executor.deleteReqs)
			}
			// The Tool stays invalid: reconciling again reports the same state.
			if _, err := r.Reconcile(context.Background(), mcpToolRequest()); err != nil {
				t.Fatal(err)
			}
			again := getEgressCredentialTool(t, r)
			condition = meta.FindStatusCondition(again.Status.Conditions, toolConditionEgressPolicyReady)
			if again.Status.Available || condition == nil || condition.Reason != "EgressRevoked" {
				t.Fatalf("status = %+v, want the Tool still unavailable with revoked egress", again.Status)
			}
		})
	}
}

// An actor whose egress cannot be denied would keep its last rules and the
// credential the gateway injects for them, so it is stopped instead.
func TestToolReconcilerReportsAndRetriesFailedEgressRevocation(t *testing.T) {
	r, executor, _ := newEgressCredentialToolReconciler(t, workspace.SubstrateKubernetesSecretsCredentialProvider, githubTokenSecret("ghp_secret_value"))
	for range 3 {
		_, _ = r.Reconcile(context.Background(), mcpToolRequest())
	}
	if !getEgressCredentialTool(t, r).Status.Available {
		t.Fatal("actor did not become available")
	}
	r.SubstrateConfig.EgressCredentialProvider = ""
	r.SubstrateEgressPolicyApplier = func(context.Context, SubstrateConfig, string, []workspace.SubstrateEgressRule) error {
		return errors.New("substrate unavailable")
	}
	if _, err := r.Reconcile(context.Background(), mcpToolRequest()); err == nil {
		t.Fatal("failed egress revocation was not retried")
	}
	got := getEgressCredentialTool(t, r)
	condition := meta.FindStatusCondition(got.Status.Conditions, toolConditionEgressPolicyReady)
	if got.Status.Available || condition == nil || condition.Reason != "EgressRevocationFailed" ||
		!strings.Contains(got.Status.Error, "actor egress was not revoked") {
		t.Fatalf("status = %+v, want a reported revocation failure", got.Status)
	}
	if len(executor.releaseReqs)+len(executor.deleteReqs) != 1 {
		t.Fatalf("releases=%v deletes=%v, want the actor stopped once", executor.releaseReqs, executor.deleteReqs)
	}
}

// Turning Substrate MCP Tools off must not leave a running actor egressing
// with its gateway-injected credential: the restarted controller revokes it.
func TestToolReconcilerRevokesCredentialedActorEgressAfterRestartWithMCPToolsDisabled(t *testing.T) {
	for name, applyErr := range map[string]error{"revoked": nil, "revocation fails": errors.New("substrate unavailable")} {
		t.Run(name, func(t *testing.T) {
			enabled, _, _ := newEgressCredentialToolReconciler(t, workspace.SubstrateKubernetesSecretsCredentialProvider, githubTokenSecret("ghp_secret_value"))
			for range 3 {
				_, _ = enabled.Reconcile(context.Background(), mcpToolRequest())
			}
			running := getEgressCredentialTool(t, enabled)
			if !running.Status.Available || running.Status.Actor == nil {
				t.Fatalf("status = %+v, want an available credentialed actor", running.Status)
			}

			var applications []egressPolicyApplication
			stopper := &recordingToolWorkspaceExecutor{}
			disabled := &ToolReconciler{
				Client:          enabled.Client,
				Scheme:          enabled.Scheme,
				SubstrateConfig: enabled.SubstrateConfig,
				SubstrateEgressPolicyApplier: func(_ context.Context, _ SubstrateConfig, actorID string, rules []workspace.SubstrateEgressRule) error {
					applications = append(applications, egressPolicyApplication{actorID: actorID, rules: rules})
					return applyErr
				},
				SubstrateExecutorFactory: func(SubstrateConfig) (workspace.WorkspaceExecutor, error) { return stopper, nil },
			}
			disabled.SubstrateTemplateValidator = substrateFixtureTemplateValidator(disabled.Client)
			_, err := disabled.Reconcile(context.Background(), mcpToolRequest())
			if stopper.claimName != "" || stopper.waitReadyCalled {
				t.Fatal("disabled controller must not claim or resume actors")
			}
			if stops := len(stopper.releaseReqs) + len(stopper.deleteReqs); stops != 1 {
				t.Fatalf("stops = %d, want the actor stopped once", stops)
			}
			if len(applications) != 1 || applications[0].actorID != running.Status.Actor.ActorID || len(applications[0].rules) != 0 {
				t.Fatalf("applications = %+v, want only a deny-all policy for actor %q", applications, running.Status.Actor.ActorID)
			}
			got := getEgressCredentialTool(t, disabled)
			condition := meta.FindStatusCondition(got.Status.Conditions, toolConditionEgressPolicyReady)
			if got.Status.Available || condition == nil || condition.Status != metav1.ConditionFalse {
				t.Fatalf("status = %+v, want an unavailable Tool without ready egress", got.Status)
			}
			if applyErr == nil {
				if err != nil || condition.Reason != "EgressRevoked" {
					t.Fatalf("Reconcile() error = %v, reason = %q, want a completed revocation", err, condition.Reason)
				}
				return
			}
			if err == nil || condition.Reason != "EgressRevocationFailed" || !strings.Contains(got.Status.Error, "actor egress was not revoked") {
				t.Fatalf("Reconcile() error = %v, status = %+v, want a reported and retried revocation failure", err, got.Status)
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
