package controller

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/executionmode"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func soulOmittedContractFixture(t *testing.T, kind corev1alpha1.AgentRuntimeType) (*TaskReconciler, *corev1alpha1.Task, *corev1alpha1.Agent) {
	t.Helper()
	task := bindingTestTask()
	task.Spec.Workspace = nil
	agent := bindingTestAgent()
	agent.Spec.Runtime.Type = kind
	if kind == corev1alpha1.AgentRuntimeOpencode {
		agent.Spec.Model.Name = "openai/gpt-5-mini"
		agent.Spec.Model.ContextWindow = new(int32(128000))
		agent.Spec.Model.MaxTokens = new(int32(2048))
	}
	agent.Spec.Runtime.ContractVersion = nil
	agent.Spec.Soul = &corev1alpha1.SoulSource{Inline: "Use a cheerful, concise voice."}
	r, _ := newBindingTestReconciler(t, task, agent, bindingTestNamespace())
	r.Mode = executionmode.HarnessV2
	image := "docker.io/example/runtime@sha256:" + strings.Repeat("a", 64)
	r.ACPRuntimeImages = ACPRuntimeImages{Codex: image, Claude: image, Copilot: image, Opencode: image}
	return r, task, agent
}

func TestSoulCandidateDefaultsContractBeforeConfiguration(t *testing.T) {
	for _, kind := range []corev1alpha1.AgentRuntimeType{corev1alpha1.AgentRuntimeCodex, corev1alpha1.AgentRuntimeClaude, corev1alpha1.AgentRuntimeCopilot, corev1alpha1.AgentRuntimeOpencode} {
		t.Run(string(kind), func(t *testing.T) {
			r, task, agent := soulOmittedContractFixture(t, kind)
			candidate, err := r.resolveAgentExecutionCandidate(context.Background(), task, agent)
			if err != nil {
				t.Fatal(err)
			}
			explicit := agent.DeepCopy()
			explicit.Spec.Runtime.ContractVersion = new(corev1alpha1.AgentRuntimeContractHarnessV2)
			expected, err := r.resolveAgentExecutionCandidate(context.Background(), task, explicit)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(candidate.snapshotBody, expected.snapshotBody) {
				t.Fatal("effective default did not produce the same immutable configuration as explicit v2")
			}
			if agent.Spec.Runtime.ContractVersion != nil {
				t.Fatal("candidate mutated the caller's unstamped Agent")
			}
		})
	}
}

func TestPendingSoulAgentDoesNotRaceContractPersistence(t *testing.T) {
	for _, kind := range []corev1alpha1.AgentRuntimeType{corev1alpha1.AgentRuntimeCodex, corev1alpha1.AgentRuntimeClaude, corev1alpha1.AgentRuntimeCopilot, corev1alpha1.AgentRuntimeOpencode} {
		t.Run(string(kind), func(t *testing.T) {
			r, task, agent := soulOmittedContractFixture(t, kind)
			// Pause before admission, not before configuration validation/binding. No
			// Agent reconciliation is run, reproducing same-apply Agent/Task arrival.
			r.ACPAdmissionGate = NewACPAdmissionGate()
			r.ACPAdmissionGate.Close("fixture admission pause", time.Now())
			if _, err := r.handlePending(context.Background(), task); err != nil {
				t.Fatal(err)
			}
			stored := &corev1alpha1.Task{}
			if err := r.Get(context.Background(), client.ObjectKeyFromObject(task), stored); err != nil {
				t.Fatal(err)
			}
			if stored.Status.Phase == corev1alpha1.TaskPhaseFailed || stored.Status.AgentExecutionBinding == nil {
				t.Fatalf("unstamped Agent was rejected instead of bound: phase=%s message=%s", stored.Status.Phase, stored.Status.Message)
			}
			original := &corev1alpha1.Agent{}
			if err := r.Get(context.Background(), client.ObjectKeyFromObject(agent), original); err != nil {
				t.Fatal(err)
			}
			if original.Spec.Runtime.ContractVersion != nil {
				t.Fatal("Task path persisted a default onto the shared Agent")
			}
		})
	}
}

func TestSoulDefaultContractDoesNotEnableWrongMode(t *testing.T) {
	for _, mode := range []executionmode.Mode{"", executionmode.HarnessV1} {
		t.Run(string(mode), func(t *testing.T) {
			r, task, agent := soulOmittedContractFixture(t, corev1alpha1.AgentRuntimeCodex)
			r.Mode = mode
			if _, err := r.resolveAgentExecutionCandidate(context.Background(), task, agent); err == nil {
				t.Fatal("SOUL was admitted outside effective built-in v2 mode")
			}
			if agent.Spec.Runtime.ContractVersion != nil {
				t.Fatal("failed planning mutated the original Agent")
			}
		})
	}
	r, task, agent := soulOmittedContractFixture(t, corev1alpha1.AgentRuntimeCodex)
	agent.Spec.Runtime.ContractVersion = new(corev1alpha1.AgentRuntimeContractHarnessV1)
	if _, err := r.resolveAgentExecutionCandidate(context.Background(), task, agent); err == nil {
		t.Fatal("explicit v1 contract was upgraded to allow SOUL")
	}
	if *agent.Spec.Runtime.ContractVersion != corev1alpha1.AgentRuntimeContractHarnessV1 {
		t.Fatal("explicit contract changed")
	}
}
