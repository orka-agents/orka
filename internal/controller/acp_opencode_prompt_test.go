package controller

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func TestOpenCodeSystemPromptConfigMapIsolationAndProfileRotation(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	prompt := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "research-prompt", Namespace: "default"}, Data: map[string]string{"text": "Cite source URLs and publication dates."}}
	other := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "research-prompt", Namespace: "other"}, Data: map[string]string{"text": "Different namespace instructions."}}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(prompt, other).Build()
	noBash := false
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "research", Namespace: "default", UID: types.UID("research-agent-uid"), Generation: 3},
		Spec: corev1alpha1.AgentSpec{
			Model:        testOpenCodeModelConfig(),
			Runtime:      &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeOpencode, ContractVersion: new(corev1alpha1.AgentRuntimeContractHarnessV2), DefaultAllowBash: &noBash, DefaultAllowedTools: []string{"web_search", "web_fetch"}},
			SystemPrompt: &corev1alpha1.PromptSource{ConfigMapRef: &corev1alpha1.ConfigMapKeySelector{Name: prompt.Name, Key: "text"}},
		},
	}
	task := &corev1alpha1.Task{Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent}}
	images := ACPRuntimeImages{Opencode: "docker.io/example/opencode@sha256:" + strings.Repeat("a", 64)}
	configuration, err := resolveACPAgentSessionConfiguration(context.Background(), reader, task, agent)
	if err != nil {
		t.Fatal(err)
	}
	if configuration.SystemPrompt != prompt.Data["text"] {
		t.Fatal("prompt did not resolve from the Agent namespace")
	}
	first, err := PlanACPRuntimeWithConfiguration(task, agent, images, configuration)
	if err != nil {
		t.Fatal(err)
	}
	updated := prompt.DeepCopy()
	updated.Data["text"] = "Use the source publication date, not the feed update date."
	if err := reader.Update(context.Background(), updated); err != nil {
		t.Fatal(err)
	}
	configuration, err = resolveACPAgentSessionConfiguration(context.Background(), reader, task, agent)
	if err != nil {
		t.Fatal(err)
	}
	second, err := PlanACPRuntimeWithConfiguration(task, agent, images, configuration)
	if err != nil {
		t.Fatal(err)
	}
	if first.Digest == second.Digest || first.Profile.AgentConfigurationDigest == second.Profile.AgentConfigurationDigest {
		t.Fatal("prompt update did not rotate immutable profile identity")
	}
	if second.Profile.ModelLimits == nil || second.Profile.ModelLimits.Context != int64(testOpenCodeContextWindow) || second.Profile.ModelLimits.Output != int64(testOpenCodeMaxTokens) {
		t.Fatal("system prompt changed the configured model limits")
	}
	configuration.SystemPrompt = "{file:/private/test-sentinel}"
	if _, err := PlanACPRuntimeWithConfiguration(task, agent, images, configuration); err == nil || !strings.Contains(err.Error(), "configuration substitutions") {
		t.Fatal("unsafe prompt did not fail before runtime admission")
	}
}
