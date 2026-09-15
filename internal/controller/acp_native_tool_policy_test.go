package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/tools"
)

func nativeToolPolicyFixture(provider corev1alpha1.AgentRuntimeType) (*corev1alpha1.Task, *corev1alpha1.Agent, ACPRuntimeImages) {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "native-tools", Namespace: "default", UID: "task-uid", Generation: 1},
		Spec: corev1alpha1.TaskSpec{
			Type:      corev1alpha1.TaskTypeAgent,
			Workspace: &corev1alpha1.WorkspaceConfig{Intent: corev1alpha1.WorkspaceIntentWrite},
		},
	}
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "runner", Namespace: "default", UID: "agent-uid", Generation: 1},
		Spec: corev1alpha1.AgentSpec{
			Model: &corev1alpha1.ModelConfig{Name: "test-model"},
			Runtime: &corev1alpha1.AgentCLIRuntime{
				Type: provider, ContractVersion: new(corev1alpha1.AgentRuntimeContractHarnessV2),
				ToolPolicy: corev1alpha1.AgentToolPolicyFull,
			},
		},
	}
	if provider == corev1alpha1.AgentRuntimeOpencode {
		agent.Spec.Model = testOpenCodeModelConfig()
	}
	image := "docker.io/example/runtime@sha256:" + strings.Repeat("a", 64)
	return task, agent, ACPRuntimeImages{Codex: image, Claude: image, Copilot: image, Opencode: image}
}

func TestFullNativeToolPolicyPreservesBrokeredAuthority(t *testing.T) {
	for _, provider := range []corev1alpha1.AgentRuntimeType{
		corev1alpha1.AgentRuntimeCodex, corev1alpha1.AgentRuntimeClaude, corev1alpha1.AgentRuntimeCopilot, corev1alpha1.AgentRuntimeOpencode,
	} {
		t.Run(string(provider), func(t *testing.T) {
			task, agent, images := nativeToolPolicyFixture(provider)
			agent.Spec.Runtime.DefaultAllowedTools = []string{"delegate_task", "wait_for_tasks"}
			registry := tools.NewRegistry()
			registry.Register(tools.NewDelegateTaskTool(nil))
			registry.Register(tools.NewWaitForTasksTool(nil))
			plan, err := PlanACPRuntime(task, agent, images)
			require.NoError(t, err)
			configuration, err := buildRuntimeSessionMCPConfigurationWithRegistry(t.Context(), nil, task, agent, plan.Profile, registry)
			require.NoError(t, err)
			require.NoError(t, configuration.ValidateProfile(plan.Profile))
			require.Equal(t, harnessv2.NativeToolPolicyFull, configuration.ToolPolicy.NativeToolPolicy)
			require.Equal(t, []string{"delegate_task", "wait_for_tasks"}, configuration.ToolPolicy.AllowedToolNames)
			require.True(t, configuration.ToolPolicy.AllowBash)
			for _, descriptor := range configuration.ToolPolicy.Tools {
				require.True(t, descriptor.Source.Brokered())
			}
			// A full native grant never turns a native name or an unlisted Orka
			// operation into a usable brokered tool.
			require.False(t, configuration.ToolPolicy.Allows("Bash"))
			require.False(t, configuration.ToolPolicy.Allows("cancel_task"))
			status := acpTaskToolPolicyStatus(string(provider), configuration)
			require.Equal(t, "full", status.Mode)
			require.Equal(t, configuration.ToolPolicyDigest, status.Digest)
			require.NotEmpty(t, status.RunnerVersion)
		})
	}
}

func TestFullNativeToolPolicyRejectsConflictingAuthority(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*corev1alpha1.Task, *corev1alpha1.Agent)
		want   string
	}{
		{"read intent", func(task *corev1alpha1.Task, _ *corev1alpha1.Agent) {
			task.Spec.Workspace.Intent = corev1alpha1.WorkspaceIntentRead
		}, "workspace.intent: read"},
		{"implicit read intent", func(task *corev1alpha1.Task, _ *corev1alpha1.Agent) { task.Spec.Workspace = nil }, "workspace.intent: read"},
		{"Bash disabled", func(task *corev1alpha1.Task, _ *corev1alpha1.Agent) {
			task.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{AllowBash: new(false)}
		}, "allowBash: false"},
		{"Task cannot override operator Bash denial", func(task *corev1alpha1.Task, agent *corev1alpha1.Agent) {
			agent.Spec.Runtime.DefaultAllowBash = new(false)
			task.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{AllowBash: new(true)}
		}, "defaultAllowBash: false"},
		{"native allowlist", func(_ *corev1alpha1.Task, agent *corev1alpha1.Agent) {
			agent.Spec.Runtime.DefaultAllowedTools = []string{"Read"}
		}, "native tool restriction"},
		{"native denylist", func(task *corev1alpha1.Task, _ *corev1alpha1.Agent) {
			task.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{DisallowedTools: []string{"WebFetch"}}
		}, "native tool restriction"},
		{"native tool outside shared list", func(task *corev1alpha1.Task, _ *corev1alpha1.Agent) {
			task.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{DisallowedTools: []string{"NotebookEdit"}}
		}, "native tool restriction"},
		{"empty Task allowlist", func(task *corev1alpha1.Task, _ *corev1alpha1.Agent) {
			task.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{AllowedTools: []string{}}
		}, "cannot narrow full"},
		{"reusable Session", func(task *corev1alpha1.Task, _ *corev1alpha1.Agent) {
			task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "session"}
		}, "Task-owned Session"},
		{"legacy protocol", func(_ *corev1alpha1.Task, agent *corev1alpha1.Agent) {
			agent.Spec.Runtime.ContractVersion = new(corev1alpha1.AgentRuntimeContractHarnessV1)
		}, "explicitly classified"},
	} {
		t.Run(test.name, func(t *testing.T) {
			task, agent, images := nativeToolPolicyFixture(corev1alpha1.AgentRuntimeClaude)
			test.change(task, agent)
			_, err := PlanACPRuntime(task, agent, images)
			require.ErrorContains(t, err, test.want)
		})
	}
}

func TestRestrictedNativeToolPolicyRejectsShellBypasses(t *testing.T) {
	for _, provider := range []corev1alpha1.AgentRuntimeType{corev1alpha1.AgentRuntimeClaude, corev1alpha1.AgentRuntimeCopilot, corev1alpha1.AgentRuntimeOpencode} {
		for _, denied := range []string{"Read", "Write", "Edit", "WebFetch", "WebSearch"} {
			t.Run(string(provider)+"/"+denied, func(t *testing.T) {
				task, agent, images := nativeToolPolicyFixture(provider)
				agent.Spec.Runtime.ToolPolicy = corev1alpha1.AgentToolPolicyRestricted
				agent.Spec.Runtime.DefaultAllowedTools = []string{"Read", "Write", "Edit", "Bash", "Glob", "Grep", "WebFetch", "WebSearch"}
				task.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{DisallowedTools: []string{denied}}
				_, err := PlanACPRuntime(task, agent, images)
				require.ErrorContains(t, err, "Bash can bypass")
			})
		}
	}
}

func TestRestrictedNativeToolPolicyCompatibility(t *testing.T) {
	for _, provider := range []corev1alpha1.AgentRuntimeType{corev1alpha1.AgentRuntimeCodex, corev1alpha1.AgentRuntimeClaude, corev1alpha1.AgentRuntimeCopilot, corev1alpha1.AgentRuntimeOpencode} {
		t.Run(string(provider), func(t *testing.T) {
			task, agent, images := nativeToolPolicyFixture(provider)
			agent.Spec.Runtime.ToolPolicy = corev1alpha1.AgentToolPolicyRestricted
			agent.Spec.Runtime.DefaultAllowedTools = []string{"Read", "Glob"}
			task.Spec.Workspace.Intent = corev1alpha1.WorkspaceIntentRead
			plan, err := PlanACPRuntime(task, agent, images)
			if provider == corev1alpha1.AgentRuntimeCodex {
				require.ErrorContains(t, err, "codex restricted tools are unsupported")
				return
			}
			require.NoError(t, err)
			configuration, err := buildRuntimeSessionMCPConfiguration(t.Context(), nil, task, agent, plan.Profile)
			require.NoError(t, err)
			require.Equal(t, harnessv2.NativeToolPolicyRestricted, configuration.ToolPolicy.NativeToolPolicy)
			for _, descriptor := range configuration.ToolPolicy.Tools {
				require.Contains(t, []string{"read", "glob"}, strings.ToLower(descriptor.Name))
			}
			// A missing policy does not inherit the OpenCode default in explicit mode.
			agent.Spec.Runtime.DefaultAllowedTools = nil
			_, err = PlanACPRuntime(task, agent, images)
			require.ErrorContains(t, err, "explicit defaultAllowedTools")
		})
	}
}

func TestRestrictedNativeToolPolicyCanonicalizesDenials(t *testing.T) {
	for _, provider := range []corev1alpha1.AgentRuntimeType{corev1alpha1.AgentRuntimeClaude, corev1alpha1.AgentRuntimeCopilot, corev1alpha1.AgentRuntimeOpencode} {
		t.Run(string(provider), func(t *testing.T) {
			task, agent, images := nativeToolPolicyFixture(provider)
			agent.Spec.Runtime.ToolPolicy = corev1alpha1.AgentToolPolicyRestricted
			agent.Spec.Runtime.DefaultAllowedTools = []string{"Read", "gLoB"}
			task.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{DisallowedTools: []string{"rEaD"}}
			plan, err := PlanACPRuntime(task, agent, images)
			require.NoError(t, err)
			configuration, err := buildRuntimeSessionMCPConfiguration(t.Context(), nil, task, agent, plan.Profile)
			require.NoError(t, err)
			for _, name := range []string{"Read", "read", "rEaD"} {
				_, allowed := configuration.ToolPolicy.Descriptor(name)
				require.False(t, allowed, name)
			}
			require.Len(t, configuration.ToolPolicy.Tools, 1)
			require.Equal(t, "glob", strings.ToLower(configuration.ToolPolicy.Tools[0].Name))
		})
	}
}

func TestNativeToolPolicyChangeRotatesSessionAndPoolIdentity(t *testing.T) {
	task, agent, images := nativeToolPolicyFixture(corev1alpha1.AgentRuntimeClaude)
	agent.Spec.Runtime.ToolPolicy = ""
	legacy, err := PlanACPRuntime(task, agent, images)
	require.NoError(t, err)
	legacyConfiguration, err := buildRuntimeSessionMCPConfiguration(t.Context(), nil, task, agent, legacy.Profile)
	require.NoError(t, err)
	agent.Spec.Runtime.ToolPolicy = corev1alpha1.AgentToolPolicyFull
	full, err := PlanACPRuntime(task, agent, images)
	require.NoError(t, err)
	fullConfiguration, err := buildRuntimeSessionMCPConfiguration(t.Context(), nil, task, agent, full.Profile)
	require.NoError(t, err)
	require.NotEqual(t, legacy.PoolName, full.PoolName)
	require.NotEqual(t, legacy.Profile.ToolPolicyDigest, full.Profile.ToolPolicyDigest)
	require.Equal(t, legacy.Profile.AdapterDigests, full.Profile.AdapterDigests)
	require.Equal(t, legacy.Profile.ProxyCredentialScope, full.Profile.ProxyCredentialScope)
	require.Equal(t, legacy.Profile.ResourceClass, full.Profile.ResourceClass)
	require.Error(t, fullConfiguration.ValidateProfile(legacy.Profile))
	require.Error(t, legacyConfiguration.ValidateProfile(full.Profile))
	legacyLineage, err := acpSessionLineageConfigDigest(legacy)
	require.NoError(t, err)
	fullLineage, err := acpSessionLineageConfigDigest(full)
	require.NoError(t, err)
	require.NotEqual(t, legacyLineage, fullLineage)
	// Downgrading only the embedded policy cannot match its frozen digest.
	fullConfiguration.ToolPolicy.NativeToolPolicy = ""
	require.Error(t, fullConfiguration.ValidateProfile(full.Profile))
}

func TestFullNativeToolsDoNotReplaceDelegatedChildTools(t *testing.T) {
	task, agent, _ := nativeToolPolicyFixture(corev1alpha1.AgentRuntimeOpencode)
	task.Labels = map[string]string{labels.LabelParentTask: "parent"}
	policy, err := effectiveACPNativeToolPolicy(task, agent)
	require.NoError(t, err)
	require.Equal(t, []string{"check_messages", "send_message"}, policy.AllowedToolNames)
	require.Equal(t, harnessv2.NativeToolPolicyFull, policy.NativeToolPolicy)
}

func TestNativeToolPolicyDiagnosticsDoNotClaimWebSetup(t *testing.T) {
	task, agent, images := nativeToolPolicyFixture(corev1alpha1.AgentRuntimeOpencode)
	plan, err := PlanACPRuntime(task, agent, images)
	require.NoError(t, err)
	configuration, err := buildRuntimeSessionMCPConfiguration(context.Background(), nil, task, agent, plan.Profile)
	require.NoError(t, err)
	byName := map[string]corev1alpha1.TaskToolFeatureStatus{}
	for _, feature := range acpTaskToolPolicyStatus("opencode", configuration).Features {
		byName[feature.Name] = feature
	}
	require.Equal(t, "ready", byName["commands"].State)
	require.Equal(t, "unverified", byName["web_search"].State)
	require.Contains(t, byName["web_search"].Reason, "mcp.exa.ai")
	require.Equal(t, "unverified", byName["web_fetch"].State)
	require.Equal(t, "unsupported", byName["native_helpers"].State)
}

func TestNativeToolPoliciesKeepRuntimePoolExecutionBoundary(t *testing.T) {
	for _, mode := range []corev1alpha1.AgentToolPolicyMode{corev1alpha1.AgentToolPolicyFull, corev1alpha1.AgentToolPolicyRestricted} {
		t.Run(string(mode), func(t *testing.T) {
			task, agent, images := nativeToolPolicyFixture(corev1alpha1.AgentRuntimeClaude)
			agent.Spec.Runtime.ToolPolicy = mode
			if mode == corev1alpha1.AgentToolPolicyRestricted {
				agent.Spec.Runtime.DefaultAllowedTools = []string{"Read", "Glob"}
			}
			plan, err := PlanACPRuntime(task, agent, images)
			require.NoError(t, err)
			pool := runtimePoolTestObject(1)
			pool.Name = plan.PoolName
			pool.Spec.Runtime.Image = plan.Image
			pool.Spec.Runtime.Profile = RuntimePoolProfileFromPlan(plan)
			r := runtimePoolTestReconciler(t, runtimePoolTestScheme(t), nil, pool)
			r.AllowedImages = images
			r.EnablePDB = true
			runtimePoolReconcile(t, r, pool)
			base := runtimePoolResourceName(pool.Namespace, pool.Name)
			deployment := runtimePoolTestDeployment(t, r, pool.Namespace, base)
			assertRuntimePoolDeploymentHardening(t, r, pool, deployment)
			assertRuntimePoolDiscoveryAndNetwork(t, r, pool, base)
			pod := deployment.Spec.Template.Spec
			require.False(t, pod.HostNetwork)
			require.False(t, pod.HostPID)
			require.False(t, pod.HostIPC)
			require.NotEmpty(t, pod.Containers[0].Resources.Limits[corev1.ResourceCPU])
			require.NotEmpty(t, pod.Containers[0].Resources.Limits[corev1.ResourceMemory])
			for _, volume := range pod.Volumes {
				require.Nil(t, volume.HostPath, volume.Name)
			}
			var policies networkingv1.NetworkPolicyList
			require.NoError(t, r.List(t.Context(), &policies, client.InNamespace(pool.Namespace)))
			for _, policy := range policies.Items {
				for _, egress := range policy.Spec.Egress {
					require.NotEmpty(t, egress.To, "no unrestricted destination grant")
					for _, peer := range egress.To {
						require.Nil(t, peer.IPBlock, "no direct internet or Kubernetes API grant")
						require.NotNil(t, peer.PodSelector)
						require.NotEmpty(t, peer.PodSelector.MatchLabels)
					}
				}
			}
		})
	}
}

func TestNativeToolPolicyDiagnosticsReportWriteGrant(t *testing.T) {
	task, agent, images := nativeToolPolicyFixture(corev1alpha1.AgentRuntimeClaude)
	agent.Spec.Runtime.ToolPolicy = corev1alpha1.AgentToolPolicyRestricted
	agent.Spec.Runtime.DefaultAllowedTools = []string{"Write"}
	plan, err := PlanACPRuntime(task, agent, images)
	require.NoError(t, err)
	configuration, err := buildRuntimeSessionMCPConfiguration(t.Context(), nil, task, agent, plan.Profile)
	require.NoError(t, err)
	for _, feature := range acpTaskToolPolicyStatus("claude", configuration).Features {
		if feature.Name == "file_write" {
			require.Equal(t, "ready", feature.State)
			return
		}
	}
	t.Fatal("missing file_write diagnostic")
}

func TestRestrictedNativeToolPolicyDoesNotRequireDeniedSearchSupport(t *testing.T) {
	task, agent, images := nativeToolPolicyFixture(corev1alpha1.AgentRuntimeCopilot)
	agent.Spec.Runtime.ToolPolicy = corev1alpha1.AgentToolPolicyRestricted
	agent.Spec.Runtime.DefaultAllowedTools = []string{"Read", "WebSearch"}
	task.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{DisallowedTools: []string{"websearch"}}
	plan, err := PlanACPRuntime(task, agent, images)
	require.NoError(t, err)
	configuration, err := buildRuntimeSessionMCPConfiguration(t.Context(), nil, task, agent, plan.Profile)
	require.NoError(t, err)
	require.Len(t, configuration.ToolPolicy.Tools, 1)
	require.Equal(t, "Read", configuration.ToolPolicy.Tools[0].Name)
}
