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
	"github.com/orka-agents/orka/internal/labels"
)

func readyKeylessProvider(name string) *corev1alpha1.Provider {
	provider := keylessTestProvider()
	provider.Name = name
	provider.UID = ""
	provider.Status = corev1alpha1.ProviderStatus{Ready: true}
	return provider
}

func TestResolveProviderAcceptsAKeylessProviderOnlyForAIWorkers(t *testing.T) {
	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "t-keyless", Namespace: "default"},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAI,
			AI:   &corev1alpha1.AISpec{ProviderRef: &corev1alpha1.ProviderReference{Name: "azure"}},
		},
	}
	r := newUnitReconciler(scheme, readyKeylessProvider("azure"), task)
	provider, err := r.resolveProvider(context.Background(), task, nil)
	if err != nil || provider == nil || provider.Name != "azure" {
		t.Fatalf("resolveProvider(ai) = %v, %v; want the keyless Provider", provider, err)
	}

	container := task.DeepCopy()
	container.Spec.Type = corev1alpha1.TaskTypeContainer
	if _, err := r.resolveProvider(context.Background(), container, nil); err == nil || !strings.Contains(err.Error(), "spec.auth") {
		t.Fatalf("resolveProvider(container) = %v, want the keyless Provider refused", err)
	}

	readOnly := task.DeepCopy()
	readOnly.Annotations = map[string]string{labels.AnnotationAgentReadOnly: scheduledRunLabelValue}
	if _, err := r.resolveProvider(context.Background(), readOnly, nil); err == nil || !strings.Contains(err.Error(), "spec.auth") {
		t.Fatalf("resolveProvider(read-only) = %v, want the keyless Provider refused", err)
	}
}

func TestResolveProviderChecksKeylessFallbackProvidersByTaskType(t *testing.T) {
	scheme := newTestScheme()
	keyed := &corev1alpha1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: "primary", Namespace: "default"},
		Spec: corev1alpha1.ProviderSpec{
			Type:      corev1alpha1.ProviderTypeOpenAI,
			SecretRef: &corev1alpha1.ProviderSecretRef{Name: "sec1"},
		},
		Status: corev1alpha1.ProviderStatus{Ready: true},
	}
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "default"},
		Spec: corev1alpha1.AgentSpec{
			ProviderRef: &corev1alpha1.ProviderReference{Name: "primary"},
			Model: &corev1alpha1.ModelConfig{Fallbacks: []corev1alpha1.ModelFallback{
				{ProviderRef: "missing"},
				{ProviderRef: "azure"},
			}},
		},
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "t-fallback", Namespace: "default"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
	}
	r := newUnitReconciler(scheme, keyed, readyKeylessProvider("azure"), agent, task)
	if _, err := r.resolveProvider(context.Background(), task, agent); err != nil {
		t.Fatalf("resolveProvider(ai) = %v, want the keyless fallback relayed", err)
	}

	container := task.DeepCopy()
	container.Spec.Type = corev1alpha1.TaskTypeContainer
	_, err := r.resolveProvider(context.Background(), container, agent)
	if err == nil || !strings.Contains(err.Error(), "provider azure uses spec.auth") {
		t.Fatalf("resolveProvider(container) = %v, want the keyless fallback refused", err)
	}

	readOnly := task.DeepCopy()
	readOnly.Annotations = map[string]string{labels.AnnotationAgentReadOnly: scheduledRunLabelValue}
	if _, err := r.resolveProvider(context.Background(), readOnly, agent); err == nil || !strings.Contains(err.Error(), "provider azure uses spec.auth") {
		t.Fatalf("resolveProvider(read-only) = %v, want the keyless fallback refused", err)
	}

	agentTask := task.DeepCopy()
	agentTask.Spec.Type = corev1alpha1.TaskTypeAgent
	if _, err := r.resolveProvider(context.Background(), agentTask, agent); err != nil {
		t.Fatalf("resolveProvider(agent Task) = %v", err)
	}
}
