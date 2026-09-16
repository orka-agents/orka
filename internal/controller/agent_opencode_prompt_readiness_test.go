package controller

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/acp"
)

func TestAgentReconcileValidatesOpenCodeConfigMapPromptBeforeReady(t *testing.T) {
	for _, test := range []struct {
		name, prompt string
		ready        bool
	}{
		{"literal", "Cite the source URLs.", true},
		{"empty", "", true},
		{"whitespace", " \t\n ", true},
		{"environment substitution", "{env:READINESS_TEST_PLACEHOLDER}", false},
		{"file substitution", "{file:/readiness-test-placeholder}", false},
		{"encoded limit", strings.Repeat("<", (acp.MaxOpenCodeSystemPromptEncodedBytes-2)/6+1), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "research-prompt", Namespace: testNS}, Data: map[string]string{"prompt": test.prompt}}
			agent := baseAgent("opencode-readiness")
			agent.Spec.Model = testOpenCodeModelConfig()
			agent.Spec.Runtime = &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeOpencode, ContractVersion: new(corev1alpha1.AgentRuntimeContractHarnessV2)}
			agent.Spec.SystemPrompt = &corev1alpha1.PromptSource{ConfigMapRef: &corev1alpha1.ConfigMapKeySelector{Name: cm.Name, Key: "prompt"}}
			scheme := runtime.NewScheme()
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			if err := corev1alpha1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(agent, cm).WithStatusSubresource(agent).Build()
			r := &AgentReconciler{Client: c, Scheme: scheme}
			key := client.ObjectKeyFromObject(agent)
			result, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key})
			if err != nil {
				t.Fatal(err)
			}
			if result.RequeueAfter <= 0 || result.RequeueAfter > 30*time.Second {
				t.Fatalf("ConfigMap readiness must be refreshed automatically within 30s, got %s", result.RequeueAfter)
			}
			var stored corev1alpha1.Agent
			if err := c.Get(t.Context(), key, &stored); err != nil {
				t.Fatal(err)
			}
			if stored.Status.Ready != test.ready {
				t.Fatalf("Agent Ready = %t, want %t", stored.Status.Ready, test.ready)
			}
			if !test.ready {
				// Reconciliation must also recover after the referenced prompt is
				// corrected, without changing the Agent's runtime or reference.
				if err := c.Get(t.Context(), client.ObjectKeyFromObject(cm), cm); err != nil {
					t.Fatal(err)
				}
				cm.Data["prompt"] = "Use public sources."
				if err := c.Update(t.Context(), cm); err != nil {
					t.Fatal(err)
				}
				result, err = r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key})
				if err != nil {
					t.Fatal(err)
				}
				if result.RequeueAfter <= 0 || result.RequeueAfter > 30*time.Second {
					t.Fatal("a recovered Agent must keep refreshing its ConfigMap prompt")
				}
				if err := c.Get(t.Context(), key, &stored); err != nil || !stored.Status.Ready {
					t.Fatalf("corrected prompt did not become ready: %v", err)
				}
			}
		})
	}
}

func TestConfigMapPromptValidationPreservesOtherRuntimeContracts(t *testing.T) {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "legacy-prompt", Namespace: testNS}, Data: map[string]string{"prompt": "{env:READINESS_TEST_PLACEHOLDER}"}}
	for _, contract := range []*corev1alpha1.AgentRuntimeContractVersion{nil, new(corev1alpha1.AgentRuntimeContractHarnessV1)} {
		agent := baseAgent("legacy")
		agent.Spec.Runtime = &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeOpencode, ContractVersion: contract}
		agent.Spec.SystemPrompt = &corev1alpha1.PromptSource{ConfigMapRef: &corev1alpha1.ConfigMapKeySelector{Name: cm.Name, Key: "prompt"}}
		if err := setupAgentReconciler(cm).validateSystemPromptConfigMap(t.Context(), agent); err != nil {
			t.Fatal("OpenCode v2 prompt rules must not change a legacy contract")
		}
	}
	agent := baseAgent("other-runtime")
	agent.Spec.Runtime = &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeCodex, ContractVersion: new(corev1alpha1.AgentRuntimeContractHarnessV2)}
	agent.Spec.SystemPrompt = &corev1alpha1.PromptSource{ConfigMapRef: &corev1alpha1.ConfigMapKeySelector{Name: cm.Name, Key: "prompt"}}
	if err := setupAgentReconciler(cm).validateSystemPromptConfigMap(t.Context(), agent); err != nil {
		t.Fatal("OpenCode prompt rules must not restrict other runtimes")
	}
}

func TestAgentReconcilePromptRefreshPreservesTTLandOtherContracts(t *testing.T) {
	for _, test := range []struct {
		name, runtimeType string
		contract          *corev1alpha1.AgentRuntimeContractVersion
		inline            bool
		ttl, maxDelay     time.Duration
	}{
		{"earlier TTL", "opencode", new(corev1alpha1.AgentRuntimeContractHarnessV2), false, 10 * time.Second, 10 * time.Second},
		{"later TTL", "opencode", new(corev1alpha1.AgentRuntimeContractHarnessV2), false, time.Hour, 30 * time.Second},
		{"other runtime", "codex", new(corev1alpha1.AgentRuntimeContractHarnessV2), false, 0, 0},
		{"legacy", "opencode", new(corev1alpha1.AgentRuntimeContractHarnessV1), false, 0, 0},
		{"unclassified", "opencode", nil, false, 0, 0},
		{"inline", "opencode", new(corev1alpha1.AgentRuntimeContractHarnessV2), true, 0, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "refresh-prompt", Namespace: testNS}, Data: map[string]string{"prompt": "Use public sources."}}
			agent := baseAgent("prompt-refresh")
			agent.CreationTimestamp = metav1.Now()
			agent.Spec.Model = testOpenCodeModelConfig()
			agent.Spec.Runtime = &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeType(test.runtimeType), ContractVersion: test.contract}
			agent.Spec.SystemPrompt = &corev1alpha1.PromptSource{ConfigMapRef: &corev1alpha1.ConfigMapKeySelector{Name: cm.Name, Key: "prompt"}}
			if test.inline {
				agent.Spec.SystemPrompt = &corev1alpha1.PromptSource{Inline: "Use public sources."}
			}
			if test.ttl > 0 {
				agent.Spec.TTLAfterLastTask = &metav1.Duration{Duration: test.ttl}
				lastUsed := metav1.Now()
				agent.Status.LastUsed = &lastUsed
			}
			scheme := runtime.NewScheme()
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			if err := corev1alpha1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(agent, cm).WithStatusSubresource(agent).Build()
			r := &AgentReconciler{Client: c, Scheme: scheme}
			result, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(agent)})
			if err != nil {
				t.Fatal(err)
			}
			if test.maxDelay == 0 {
				if result.RequeueAfter != 0 {
					t.Fatalf("unrelated Agent got a prompt refresh: %s", result.RequeueAfter)
				}
			} else if result.RequeueAfter <= 0 || result.RequeueAfter > test.maxDelay {
				t.Fatalf("requeue %s does not preserve the %s deadline", result.RequeueAfter, test.maxDelay)
			}
		})
	}
}
