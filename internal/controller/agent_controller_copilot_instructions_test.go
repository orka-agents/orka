package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/agentcontext"
)

const (
	copilotInstructionsRoleMapName = "copilot-role"
	copilotInstructionsRoleMapKey  = "ROLE.md"
	copilotInstructionsRoleText    = "Follow the task instructions."
)

func copilotSoulInstructionsAgent(name string) *corev1alpha1.Agent {
	agent := baseAgent(name)
	agent.UID = types.UID(name + "-uid")
	agent.Generation = 3
	agent.Spec.Model.Provider = ""
	agent.Spec.Runtime = &corev1alpha1.AgentCLIRuntime{
		Type: corev1alpha1.AgentRuntimeCopilot, ContractVersion: new(corev1alpha1.AgentRuntimeContractHarnessV2),
	}
	agent.Spec.SystemPrompt = &corev1alpha1.PromptSource{Inline: copilotInstructionsRoleText}
	agent.Spec.Soul = &corev1alpha1.SoulSource{Inline: agentSoulWatchText}
	return agent
}

func assertAgentInstructionsReady(t *testing.T, r *AgentReconciler, agent *corev1alpha1.Agent, want bool, message string) {
	t.Helper()
	stored := &corev1alpha1.Agent{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(agent), stored))
	require.Equal(t, want, stored.Status.Ready)
	require.Equal(t, agent.Generation, stored.Generation)
	condition := meta.FindStatusCondition(stored.Status.Conditions, "Ready")
	require.NotNil(t, condition)
	require.Equal(t, agent.Generation, condition.ObservedGeneration)
	if want {
		require.Equal(t, metav1.ConditionTrue, condition.Status)
		require.Equal(t, reasonValidationSucceeded, condition.Reason)
	} else {
		require.Equal(t, metav1.ConditionFalse, condition.Status)
		require.Equal(t, reasonValidationFailed, condition.Reason)
		require.Contains(t, condition.Message, message)
	}
}

func TestAgentCopilotSoulReadinessMatchesPlanning(t *testing.T) {
	for _, test := range []struct {
		name          string
		role          string
		soul          string
		roleConfigMap bool
		soulConfigMap bool
		wantReady     bool
	}{
		{name: "inline instructions", role: copilotInstructionsRoleText, soul: agentSoulWatchText, wantReady: true},
		{name: "no role", soul: agentSoulWatchText, wantReady: true},
		{name: "role only", role: copilotInstructionsRoleText, wantReady: true},
		{name: "role only ConfigMap", role: copilotInstructionsRoleText, roleConfigMap: true, wantReady: true},
		{name: "role only import", role: "Role: consult @role-notes.md"},
		{name: "role only ConfigMap import", role: "Role: consult @role-notes.md", roleConfigMap: true},
		{name: "inline soul import", role: copilotInstructionsRoleText, soul: "Persona: consult @soul-notes.md"},
		{name: "inline role import", role: "Role: consult @role-notes.md", soul: agentSoulWatchText},
		{name: "ConfigMap soul import", role: copilotInstructionsRoleText, soul: "Persona: consult @soul-notes.md", soulConfigMap: true},
		{name: "ConfigMap role import", role: "Role: consult @role-notes.md", soul: agentSoulWatchText, roleConfigMap: true},
		{name: "both ConfigMaps", role: "  Follow the task.\n", soul: agentSoulWatchText, roleConfigMap: true, soulConfigMap: true, wantReady: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			agent := copilotSoulInstructionsAgent("copilot-readiness")
			agent.Spec.SystemPrompt.Inline = test.role
			var expectedSoul *agentcontext.ResolvedSoul
			if test.soul == "" {
				agent.Spec.Soul = nil
			} else {
				agent.Spec.Soul.Inline = test.soul
				expectedSoul = &agentcontext.ResolvedSoul{Text: test.soul}
			}
			objects := []client.Object{agent}
			if test.roleConfigMap {
				roleMap := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: copilotInstructionsRoleMapName, Namespace: agent.Namespace}, Data: map[string]string{copilotInstructionsRoleMapKey: test.role}}
				objects = append(objects, roleMap)
				agent.Spec.SystemPrompt = &corev1alpha1.PromptSource{ConfigMapRef: &corev1alpha1.ConfigMapKeySelector{Name: roleMap.Name, Key: copilotInstructionsRoleMapKey}}
			}
			if test.soulConfigMap {
				soulMap := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: agentSoulWatchMapName, Namespace: agent.Namespace}, Data: map[string]string{agentSoulWatchMapKey: test.soul}}
				objects = append(objects, soulMap)
				agent.Spec.Soul = &corev1alpha1.SoulSource{ConfigMapRef: &corev1alpha1.ConfigMapKeySelector{Name: soulMap.Name, Key: agentSoulWatchMapKey}, Digest: agentcontext.Digest(test.soul)}
			}
			r := newAgentSoulWatchReconciler(t, objects...)
			task := &corev1alpha1.Task{Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent}}
			configuration, err := resolveACPAgentSessionConfiguration(ctx, r.Client, task, agent)
			require.NoError(t, err) // Generic soul resolution permits @; Copilot planning does not.
			require.Equal(t, agentcontext.Compose(test.role, expectedSoul), configuration.SystemPrompt)
			_, planErr := PlanACPRuntimeWithConfiguration(task, agent, ACPRuntimeImages{Copilot: "docker.io/example/copilot@sha256:" + strings.Repeat("c", 64)}, configuration)
			if test.wantReady {
				require.NoError(t, planErr)
			} else {
				require.ErrorContains(t, planErr, "must not contain @")
			}
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(agent)})
			require.NoError(t, err)
			assertAgentInstructionsReady(t, r, agent, test.wantReady, "must not contain @")
		})
	}
}

func TestAgentCopilotSoulReadinessPreservesOtherPaths(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*corev1alpha1.Agent)
	}{
		{name: "unclassified Copilot without soul", mutate: func(agent *corev1alpha1.Agent) {
			agent.Spec.Soul = nil
			agent.Spec.Runtime.ContractVersion = nil
		}},
		{name: "legacy Copilot without soul", mutate: func(agent *corev1alpha1.Agent) {
			agent.Spec.Soul = nil
			agent.Spec.Runtime.ContractVersion = new(corev1alpha1.AgentRuntimeContractHarnessV1)
		}},
		{name: "AI", mutate: func(agent *corev1alpha1.Agent) { agent.Spec.Runtime = nil; agent.Spec.Model.Provider = "openai" }},
		{name: "Codex", mutate: func(agent *corev1alpha1.Agent) { agent.Spec.Runtime.Type = corev1alpha1.AgentRuntimeCodex }},
		{name: "Claude", mutate: func(agent *corev1alpha1.Agent) { agent.Spec.Runtime.Type = corev1alpha1.AgentRuntimeClaude }},
		{name: "OpenCode", mutate: func(agent *corev1alpha1.Agent) {
			agent.Spec.Runtime.Type = corev1alpha1.AgentRuntimeOpencode
			agent.Spec.Model.Name = "openai/model"
			agent.Spec.Model.ContextWindow = new(int32(32768))
			agent.Spec.Model.MaxTokens = new(int32(4096))
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			agent := copilotSoulInstructionsAgent("unchanged-path")
			agent.Spec.SystemPrompt.Inline = "Role mentioning @notes.md"
			agent.Spec.Soul.Inline = "Persona mentioning @notes.md"
			test.mutate(agent)
			r := newAgentSoulWatchReconciler(t, agent)
			_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(agent)})
			require.NoError(t, err)
			assertAgentInstructionsReady(t, r, agent, true, "")
		})
	}
}

func TestAgentCopilotSoulRoleConfigMapReadinessTransitions(t *testing.T) {
	for _, withSoul := range []bool{true, false} {
		name := "role only"
		if withSoul {
			name = "role and soul"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			agent := copilotSoulInstructionsAgent("role-readiness")
			if !withSoul {
				agent.Spec.Soul = nil
			}
			agent.Spec.SystemPrompt = &corev1alpha1.PromptSource{ConfigMapRef: &corev1alpha1.ConfigMapKeySelector{Name: copilotInstructionsRoleMapName, Key: copilotInstructionsRoleMapKey}}
			otherNamespace := agent.DeepCopy()
			otherNamespace.Namespace = "another-namespace"
			r := newAgentSoulWatchReconciler(t, agent, otherNamespace)
			request := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(agent)}
			roleMap := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: copilotInstructionsRoleMapName, Namespace: agent.Namespace}, Data: map[string]string{copilotInstructionsRoleMapKey: copilotInstructionsRoleText}}
			reconcileMap := func(wantReady bool, message string) {
				t.Helper()
				requests := r.agentsForSoulConfigMap(ctx, roleMap)
				require.Equal(t, []reconcile.Request{request}, requests)
				_, err := r.Reconcile(ctx, requests[0])
				require.NoError(t, err)
				assertAgentInstructionsReady(t, r, agent, wantReady, message)
			}

			_, err := r.Reconcile(ctx, request)
			require.NoError(t, err)
			assertAgentInstructionsReady(t, r, agent, false, "not found")

			require.NoError(t, r.Create(ctx, roleMap))
			reconcileMap(true, "")
			roleMap.Data[copilotInstructionsRoleMapKey] = "Role: consult @role-notes.md"
			require.NoError(t, r.Update(ctx, roleMap))
			reconcileMap(false, "must not contain @")
			roleMap.Data[copilotInstructionsRoleMapKey] = copilotInstructionsRoleText
			require.NoError(t, r.Update(ctx, roleMap))
			reconcileMap(true, "")

			delete(roleMap.Data, copilotInstructionsRoleMapKey)
			require.NoError(t, r.Update(ctx, roleMap))
			reconcileMap(false, "not found")
			roleMap.Data[copilotInstructionsRoleMapKey] = copilotInstructionsRoleText
			require.NoError(t, r.Update(ctx, roleMap))
			reconcileMap(true, "")
			require.NoError(t, r.Delete(ctx, roleMap))
			reconcileMap(false, "not found")
			roleMap.ResourceVersion = ""
			roleMap.UID = ""
			require.NoError(t, r.Create(ctx, roleMap))
			reconcileMap(true, "")

			foreign := &corev1alpha1.Agent{}
			require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(otherNamespace), foreign))
			require.Empty(t, foreign.Status.Conditions, "same-name ConfigMap in another namespace must not reconcile this Agent")
		})
	}
}
