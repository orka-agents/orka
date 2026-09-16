/*
Copyright (c) 2026.
MIT License - see LICENSE file for details.
*/
package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/executionmode"
)

func TestWithEffectiveBuiltInContract(t *testing.T) {
	v1 := corev1alpha1.AgentRuntimeContractHarnessV1
	omitted := &corev1alpha1.Agent{Spec: corev1alpha1.AgentSpec{
		Runtime: &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeCodex},
	}}
	resolved := withEffectiveBuiltInContract(omitted, executionmode.HarnessV2)
	if resolved.BuiltInContractVersion() != corev1alpha1.AgentRuntimeContractHarnessV2 {
		t.Fatalf("omitted contract must resolve to the controller mode, got %q", resolved.BuiltInContractVersion())
	}
	if omitted.Spec.Runtime.ContractVersion != nil {
		t.Fatal("the caller's Agent must not be mutated")
	}
	explicit := &corev1alpha1.Agent{Spec: corev1alpha1.AgentSpec{
		Runtime: &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeCodex, ContractVersion: &v1},
	}}
	if got := withEffectiveBuiltInContract(explicit, executionmode.HarnessV2); got != explicit {
		t.Fatal("an explicit contract must be returned untouched, even when it disagrees with the mode")
	}
	external := &corev1alpha1.Agent{Spec: corev1alpha1.AgentSpec{
		Runtime: &corev1alpha1.AgentCLIRuntime{RuntimeRef: &corev1alpha1.AgentRuntimeReference{Name: "ext"}},
	}}
	if got := withEffectiveBuiltInContract(external, executionmode.HarnessV2); got != external {
		t.Fatal("a runtimeRef Agent derives its contract from the AgentRuntime and must not be stamped")
	}
	if got := withEffectiveBuiltInContract(omitted, ""); got != omitted {
		t.Fatal("without a controller mode nothing can be resolved")
	}
}

func TestAgentReconcilerPersistsOmittedBuiltInContract(t *testing.T) {
	scheme := newTestScheme()
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "codex-agent", Namespace: defaultNS},
		Spec: corev1alpha1.AgentSpec{
			Model:   &corev1alpha1.ModelConfig{Name: "gpt-6-astra"},
			Runtime: &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeCodex},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(agent).WithStatusSubresource(agent).Build()
	r := &AgentReconciler{Client: c, Scheme: scheme, Mode: executionmode.HarnessV2}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: defaultNS, Name: "codex-agent"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	stored := &corev1alpha1.Agent{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: defaultNS, Name: "codex-agent"}, stored); err != nil {
		t.Fatal(err)
	}
	if stored.BuiltInContractVersion() != corev1alpha1.AgentRuntimeContractHarnessV2 {
		t.Fatalf("stored Agent must carry the namespace mode after reconcile, got %q", stored.BuiltInContractVersion())
	}
}
