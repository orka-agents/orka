package controller

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/agentcontext"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

func defaultInstructionsAgent(provider corev1alpha1.AgentRuntimeType) *corev1alpha1.Agent {
	agent := copilotSoulInstructionsAgent("default-instructions")
	if provider == "" {
		agent.Spec.Runtime = nil
		agent.Spec.Model.Provider = "openai"
	} else {
		agent.Spec.Runtime.Type = provider
		if provider == corev1alpha1.AgentRuntimeOpencode {
			agent.Spec.Model.Name = "openai/model"
			agent.Spec.Model.ContextWindow = new(int32(32768))
			agent.Spec.Model.MaxTokens = new(int32(4096))
		}
	}
	return agent
}

func assertDefaultInstructionsMatchExecution(t *testing.T, agent *corev1alpha1.Agent, wantReady bool, message string, objects ...client.Object) {
	t.Helper()
	ctx := context.Background()
	r := newAgentSoulWatchReconciler(t, append(objects, agent)...)
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{UID: "default-task-uid", Generation: 1}, Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI}}
	var executionErr error
	if agent.Spec.Runtime == nil {
		if agent.Spec.Soul != nil {
			_, executionErr = resolveAISoul(ctx, r.Client, task, agent)
		} else {
			executionErr = NewJobBuilder(r.Client).validateContainerDeliveredPromptSize(ctx, task, agent)
		}
	} else {
		task.Spec.Type = corev1alpha1.TaskTypeAgent
		var configuration harnessv2.AgentSessionConfiguration
		configuration, executionErr = resolveACPAgentSessionConfiguration(ctx, r.Client, task, agent)
		if executionErr == nil {
			image := "docker.io/example/runtime@sha256:" + strings.Repeat("c", 64)
			_, executionErr = PlanACPRuntimeWithConfiguration(task, agent, ACPRuntimeImages{Codex: image, Claude: image, Copilot: image, Opencode: image}, configuration)
		}
	}
	if wantReady {
		require.NoError(t, executionErr)
	} else {
		require.Error(t, executionErr, "the actual execution path must reject this default")
	}
	_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(agent)})
	require.NoError(t, err)
	assertAgentInstructionsReady(t, r, agent, wantReady, message)
}

func TestAgentDefaultInstructionsAIBoundaries(t *testing.T) {
	soul := &agentcontext.ResolvedSoul{Text: agentSoulWatchText}
	overhead := len(agentcontext.Compose("r", soul)) - 1
	available := maxContainerDeliveredPromptBytes - overhead
	for _, test := range []struct {
		name     string
		role     string
		noSoul   bool
		wantSize int
		want     bool
	}{
		{name: "composed exact limit", role: strings.Repeat("r", available), wantSize: maxContainerDeliveredPromptBytes, want: true},
		{name: "composition crosses limit", role: strings.Repeat("r", available+1), wantSize: maxContainerDeliveredPromptBytes + 1},
		{name: "literal dollar exact limit", role: strings.Repeat("$", available/2) + strings.Repeat("r", available%2), wantSize: maxContainerDeliveredPromptBytes, want: true},
		{name: "literal dollar crosses limit", role: strings.Repeat("$", available/2) + strings.Repeat("r", available%2+1), wantSize: maxContainerDeliveredPromptBytes + 1},
		{name: "UTF-8 exact byte limit", role: strings.Repeat("界", available/3) + strings.Repeat("r", available%3), wantSize: maxContainerDeliveredPromptBytes, want: true},
		{name: "UTF-8 crosses byte limit", role: strings.Repeat("界", available/3+1), wantSize: overhead + 3*(available/3+1)},
		{name: "legacy raw dollars at limit", role: strings.Repeat("$", maxContainerDeliveredPromptBytes), noSoul: true, wantSize: maxContainerDeliveredPromptBytes, want: true},
		{name: "legacy raw role crosses limit", role: strings.Repeat("r", maxContainerDeliveredPromptBytes+1), noSoul: true, wantSize: maxContainerDeliveredPromptBytes + 1},
	} {
		for _, configMap := range []bool{false, true} {
			source := "inline"
			if configMap {
				source = "ConfigMap"
			}
			t.Run(test.name+"/"+source, func(t *testing.T) {
				agent := defaultInstructionsAgent("")
				agent.Spec.SystemPrompt.Inline = test.role
				delivered := literalKubernetesPrompt(agentcontext.Compose(test.role, soul))
				if test.noSoul {
					agent.Spec.Soul = nil
					delivered = test.role
				}
				require.Len(t, delivered, test.wantSize)
				var objects []client.Object
				if configMap {
					roleMap := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "role", Namespace: agent.Namespace}, Data: map[string]string{copilotInstructionsRoleMapKey: test.role}}
					agent.Spec.SystemPrompt = &corev1alpha1.PromptSource{ConfigMapRef: &corev1alpha1.ConfigMapKeySelector{Name: roleMap.Name, Key: copilotInstructionsRoleMapKey}}
					objects = append(objects, roleMap)
				}
				assertDefaultInstructionsMatchExecution(t, agent, test.want, "container-delivered prompts are limited", objects...)
			})
		}
	}
}

func TestAgentDefaultInstructionsACPConfigurationBoundaries(t *testing.T) {
	overhead := len(agentcontext.Compose("r", &agentcontext.ResolvedSoul{Text: agentSoulWatchText})) - 1
	available := harnessv2.MaxAgentSystemPromptBytes - overhead
	for _, provider := range []corev1alpha1.AgentRuntimeType{corev1alpha1.AgentRuntimeClaude, corev1alpha1.AgentRuntimeCopilot, corev1alpha1.AgentRuntimeOpencode} {
		for _, test := range []struct {
			name string
			role string
			want bool
		}{
			{name: "exact limit", role: strings.Repeat("r", available), want: true},
			{name: "composition crosses limit", role: strings.Repeat("r", available+1)},
			{name: "UTF-8 exact byte limit", role: strings.Repeat("界", available/3) + strings.Repeat("r", available%3), want: true},
			{name: "UTF-8 crosses byte limit", role: strings.Repeat("界", available/3+1)},
			{name: "JSON escapes do not impose Codex environment limit", role: strings.Repeat("<", available), want: true},
		} {
			t.Run(string(provider)+"/"+test.name, func(t *testing.T) {
				agent := defaultInstructionsAgent(provider)
				agent.Spec.SystemPrompt.Inline = test.role
				assertDefaultInstructionsMatchExecution(t, agent, test.want, "agent system prompt exceeds")
			})
		}
	}
}

func TestAgentDefaultInstructionsCodexNativeEnvelope(t *testing.T) {
	for _, model := range []string{"gpt-4", strings.Repeat("m", 256)} {
		agent := defaultInstructionsAgent(corev1alpha1.AgentRuntimeCodex)
		agent.Spec.Model.Name = model
		agent.Spec.Runtime.DefaultReasoningEffort = agentReasoningEffortHigh
		soul := &agentcontext.ResolvedSoul{Text: agentSoulWatchText}
		configuration := harnessv2.AgentSessionConfiguration{Model: model, ReasoningEffort: effectiveACPReasoningEffort(agent)}
		// Discover the actual native boundary through the planner's validator;
		// do not duplicate its byte cap or its JSON/native-envelope estimate.
		low, high := 0, harnessv2.MaxAgentSystemPromptBytes
		for low < high {
			mid := low + (high-low+1)/2
			configuration.SystemPrompt = agentcontext.Compose(strings.Repeat("r", mid), soul)
			if validateACPProviderSystemPrompt(string(corev1alpha1.AgentRuntimeCodex), configuration) == nil {
				low = mid
			} else {
				high = mid - 1
			}
		}
		require.Positive(t, low)
		require.Less(t, low, harnessv2.MaxAgentSystemPromptBytes/2, "the native environment bound must be exercised before the generic ACP bound")
		for _, test := range []struct {
			name string
			role string
			soul string
			want bool
		}{
			{name: "exact native envelope boundary", role: strings.Repeat("r", low), want: true},
			{name: "one byte past native boundary", role: strings.Repeat("r", low+1)},
			{name: "escaped quotes", role: strings.Repeat("\"", low)},
			{name: "escaped backslashes", role: strings.Repeat("\\", low)},
			{name: "escaped HTML", role: strings.Repeat("<", low)},
			{name: "escaped soul HTML", role: strings.Repeat("r", low), soul: strings.Repeat("<", len(agentSoulWatchText))},
			{name: "UTF-8 counted as bytes", role: strings.Repeat("界", low/3), want: true},
		} {
			t.Run(test.name+"/model-bytes-"+strconv.Itoa(len(model)), func(t *testing.T) {
				candidate := agent.DeepCopy()
				candidate.Spec.SystemPrompt.Inline = test.role
				if test.soul != "" {
					candidate.Spec.Soul.Inline = test.soul
				}
				assertDefaultInstructionsMatchExecution(t, candidate, test.want, "codex session configuration exceeds the safe environment limit")
			})
		}
	}
}

func TestAgentDefaultInstructionsRoleConfigMapRecovery(t *testing.T) {
	for _, provider := range []corev1alpha1.AgentRuntimeType{"", corev1alpha1.AgentRuntimeCodex, corev1alpha1.AgentRuntimeClaude, corev1alpha1.AgentRuntimeCopilot, corev1alpha1.AgentRuntimeOpencode} {
		for _, withSoul := range []bool{false, true} {
			name := string(provider) + "/role-only"
			if withSoul {
				name = string(provider) + "/role-and-soul"
			}
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				agent := defaultInstructionsAgent(provider)
				if !withSoul {
					agent.Spec.Soul = nil
				}
				roleMap := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "default-role", Namespace: agent.Namespace}, Data: map[string]string{copilotInstructionsRoleMapKey: copilotInstructionsRoleText}}
				agent.Spec.SystemPrompt = &corev1alpha1.PromptSource{ConfigMapRef: &corev1alpha1.ConfigMapKeySelector{Name: roleMap.Name, Key: copilotInstructionsRoleMapKey}}
				foreign := agent.DeepCopy()
				foreign.Namespace = "another-namespace"
				r := newAgentSoulWatchReconciler(t, agent, foreign)
				request := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(agent)}
				reconcileMap := func(want bool, message string) {
					t.Helper()
					require.Equal(t, []reconcile.Request{request}, r.agentsForSoulConfigMap(ctx, roleMap))
					_, err := r.Reconcile(ctx, request)
					require.NoError(t, err)
					assertAgentInstructionsReady(t, r, agent, want, message)
				}
				reconcileMap(false, "not found")
				require.NoError(t, r.Create(ctx, roleMap))
				reconcileMap(true, "")
				message := "agent system prompt exceeds"
				invalid := strings.Repeat("r", harnessv2.MaxAgentSystemPromptBytes+1)
				switch provider {
				case "":
					message = "container-delivered prompts are limited"
					invalid = strings.Repeat("r", maxContainerDeliveredPromptBytes+1)
					if withSoul {
						invalid = strings.Repeat("$", maxContainerDeliveredPromptBytes/2)
					}
				case corev1alpha1.AgentRuntimeCodex:
					message = "codex session configuration exceeds the safe environment limit"
					invalid = strings.Repeat("<", maxContainerDeliveredPromptBytes/2)
				}
				roleMap.Data[copilotInstructionsRoleMapKey] = invalid
				require.NoError(t, r.Update(ctx, roleMap))
				reconcileMap(false, message)
				roleMap.Data[copilotInstructionsRoleMapKey] = copilotInstructionsRoleText
				require.NoError(t, r.Update(ctx, roleMap))
				reconcileMap(true, "")
				require.NoError(t, r.Delete(ctx, roleMap))
				reconcileMap(false, "not found")
				roleMap.ResourceVersion = ""
				roleMap.UID = ""
				require.NoError(t, r.Create(ctx, roleMap))
				reconcileMap(true, "")
				require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(foreign), foreign))
				require.Empty(t, foreign.Status.Conditions, "role changes must not reconcile another namespace")
			})
		}
	}
}

func TestAgentDefaultInstructionsTaskRoleOverrideIsNotReadiness(t *testing.T) {
	ctx := context.Background()
	agent := defaultInstructionsAgent("")
	agent.Spec.SystemPrompt.Inline = strings.Repeat("r", maxContainerDeliveredPromptBytes)
	r := newAgentSoulWatchReconciler(t, agent)
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{UID: "task-uid", Generation: 1}, Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI, AI: &corev1alpha1.AISpec{SystemPrompt: "A short Task role."}}}
	prepared, err := resolveAISoul(ctx, r.Client, task, agent)
	require.NoError(t, err, "a Task may replace an oversized Agent default role")
	require.True(t, strings.HasPrefix(prepared.Prompt, task.Spec.AI.SystemPrompt))
	_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(agent)})
	require.NoError(t, err)
	assertAgentInstructionsReady(t, r, agent, false, "container-delivered prompts are limited")

	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(agent), agent))
	agent.Spec.SystemPrompt.Inline = copilotInstructionsRoleText
	require.NoError(t, r.Update(ctx, agent))
	task.Spec.AI.SystemPrompt = strings.Repeat("r", maxContainerDeliveredPromptBytes)
	_, err = resolveAISoul(ctx, r.Client, task, agent)
	require.ErrorContains(t, err, "safe environment limit", "readiness must not promise that arbitrary Task overrides fit")
	_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(agent)})
	require.NoError(t, err)
	assertAgentInstructionsReady(t, r, agent, true, "")
}

func TestAgentDefaultInstructionsAILiteralSoulEnvelope(t *testing.T) {
	soul := &agentcontext.ResolvedSoul{Text: strings.Repeat("$", agentcontext.MaxSoulBytes)}
	overhead := len(literalKubernetesPrompt(agentcontext.Compose("r", soul))) - 1
	for _, extra := range []int{0, 1} {
		t.Run(strconv.Itoa(extra)+"-bytes-over", func(t *testing.T) {
			agent := defaultInstructionsAgent("")
			agent.Spec.Soul.Inline = soul.Text
			agent.Spec.SystemPrompt.Inline = strings.Repeat("r", maxContainerDeliveredPromptBytes-overhead+extra)
			assertDefaultInstructionsMatchExecution(t, agent, extra == 0, "container-delivered prompts are limited")
		})
	}
}

func TestAgentDefaultInstructionsPreservesExternalAndLegacy(t *testing.T) {
	for _, provider := range []corev1alpha1.AgentRuntimeType{corev1alpha1.AgentRuntimeCodex, corev1alpha1.AgentRuntimeClaude, corev1alpha1.AgentRuntimeCopilot, corev1alpha1.AgentRuntimeOpencode} {
		for _, contract := range []*corev1alpha1.AgentRuntimeContractVersion{nil, new(corev1alpha1.AgentRuntimeContractHarnessV1)} {
			name := string(provider) + "/unclassified"
			if contract != nil {
				name = string(provider) + "/v1"
			}
			t.Run(name, func(t *testing.T) {
				agent := defaultInstructionsAgent(provider)
				agent.Spec.Soul = nil
				agent.Spec.Runtime.ContractVersion = contract
				agent.Spec.SystemPrompt.Inline = strings.Repeat("@", harnessv2.MaxAgentSystemPromptBytes+1)
				r := newAgentSoulWatchReconciler(t, agent)
				require.NoError(t, r.validateAgent(context.Background(), agent), "new native delivery validation must not run for a legacy contract")
			})
		}
	}
	t.Run("runtimeRef", func(t *testing.T) {
		agent := defaultInstructionsAgent(corev1alpha1.AgentRuntimeCopilot)
		agent.Spec.Soul = nil
		agent.Spec.Runtime = &corev1alpha1.AgentCLIRuntime{RuntimeRef: &corev1alpha1.AgentRuntimeReference{Name: "external"}}
		agent.Spec.Model = nil
		agent.Spec.SystemPrompt.Inline = strings.Repeat("@", harnessv2.MaxAgentSystemPromptBytes+1)
		r := newAgentSoulWatchReconciler(t, agent)
		require.NoError(t, r.validateAgent(context.Background(), agent), "external-profile policy remains owned by the existing runtimeRef path")
	})
}

func TestAgentDefaultInstructionsPreservesLegacyAISourcePrecedence(t *testing.T) {
	agent := defaultInstructionsAgent("")
	agent.Spec.Soul = nil
	roleMap := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "legacy-role", Namespace: agent.Namespace}, Data: map[string]string{copilotInstructionsRoleMapKey: strings.Repeat("r", maxContainerDeliveredPromptBytes+1)}}
	agent.Spec.SystemPrompt.ConfigMapRef = &corev1alpha1.ConfigMapKeySelector{Name: roleMap.Name, Key: copilotInstructionsRoleMapKey}
	assertDefaultInstructionsMatchExecution(t, agent, true, "", roleMap)
}
