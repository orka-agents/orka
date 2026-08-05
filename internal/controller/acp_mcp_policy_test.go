package controller

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/tools"
)

func TestBuildRuntimeSessionMCPConfigurationInjectsJournaledChildMessagingTools(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(tools.NewSendMessageTool())
	registry.Register(tools.NewCheckMessagesTool())
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: "child", Namespace: "default", UID: "child-uid",
			Labels: map[string]string{labels.LabelParentTask: "parent"},
		},
	}
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "default", UID: "agent-uid", Generation: 1},
		Spec: corev1alpha1.AgentSpec{
			Model: &corev1alpha1.ModelConfig{Name: "model"},
			Runtime: &corev1alpha1.AgentCLIRuntime{
				Type:                corev1alpha1.AgentRuntimeClaude,
				DefaultAllowedTools: []string{providerNativeToolRead},
			},
		},
	}
	plan, err := PlanACPRuntime(
		task, agent,
		ACPRuntimeImages{Claude: "docker.io/example/claude@sha256:" + strings.Repeat("a", 64)},
	)
	if err != nil {
		t.Fatal(err)
	}
	configuration, err := buildRuntimeSessionMCPConfigurationWithRegistry(
		context.Background(), nil, task, agent, plan.Profile, registry,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := configuration.ValidateProfile(plan.Profile); err != nil {
		t.Fatalf("ValidateProfile() error = %v", err)
	}
	if want := []string{providerNativeToolRead, "check_messages", "send_message"}; !slices.Equal(configuration.ToolPolicy.AllowedToolNames, want) {
		t.Fatalf("allowed tools = %v, want %v", configuration.ToolPolicy.AllowedToolNames, want)
	}
	byName := make(map[string]harnessv2.MCPToolDescriptor, len(configuration.ToolPolicy.Tools))
	for _, descriptor := range configuration.ToolPolicy.Tools {
		byName[descriptor.Name] = descriptor
	}
	for _, name := range []string{"check_messages", "send_message"} {
		descriptor, ok := byName[name]
		if !ok {
			t.Fatalf("missing delegated child descriptor %q", name)
		}
		if descriptor.Source != harnessv2.MCPToolSourceBrokeredBuiltin {
			t.Fatalf("descriptor %q source = %q, want brokered_builtin", name, descriptor.Source)
		}
		if descriptor.Effect != harnessv2.MCPToolEffectConsequential {
			t.Fatalf("descriptor %q effect = %q, want consequential", name, descriptor.Effect)
		}
	}
}

func TestBuildRuntimeSessionMCPConfigurationSynthesizesDenyOnlyProviderNativeTools(t *testing.T) {
	allowBashFalse := false
	tests := []struct {
		name           string
		provider       corev1alpha1.AgentRuntimeType
		images         ACPRuntimeImages
		disallowed     []string
		allowBash      *bool
		wantAllowed    []string
		wantDisallowed []string
		wantAllowBash  bool
	}{
		{
			name: "claude denylist", provider: corev1alpha1.AgentRuntimeClaude,
			images:         ACPRuntimeImages{Claude: "docker.io/example/claude@sha256:" + strings.Repeat("a", 64)},
			disallowed:     []string{"write"},
			wantAllowed:    []string{providerNativeToolBash, providerNativeToolEdit, providerNativeToolGlob, providerNativeToolGrep, providerNativeToolRead, providerNativeToolWebFetch, providerNativeToolWebSearch},
			wantDisallowed: []string{"write"},
			wantAllowBash:  true,
		},
		{
			name: "claude bash disabled", provider: corev1alpha1.AgentRuntimeClaude,
			images:        ACPRuntimeImages{Claude: "docker.io/example/claude@sha256:" + strings.Repeat("a", 64)},
			allowBash:     &allowBashFalse,
			wantAllowed:   []string{providerNativeToolEdit, providerNativeToolGlob, providerNativeToolGrep, providerNativeToolRead, providerNativeToolWebFetch, providerNativeToolWebSearch, providerNativeToolWrite},
			wantAllowBash: false,
		},
		{
			name: "copilot denylist", provider: corev1alpha1.AgentRuntimeCopilot,
			images:         ACPRuntimeImages{Copilot: "docker.io/example/copilot@sha256:" + strings.Repeat("b", 64)},
			disallowed:     []string{"write", providerNativeToolWebSearch},
			wantAllowed:    []string{providerNativeToolBash, providerNativeToolEdit, providerNativeToolGlob, providerNativeToolGrep, providerNativeToolRead, providerNativeToolWebFetch},
			wantDisallowed: []string{providerNativeToolWebSearch, "write"},
			wantAllowBash:  true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: "default", UID: "task-uid"},
				Spec: corev1alpha1.TaskSpec{AgentRuntime: &corev1alpha1.AgentRuntimeSpec{
					DisallowedTools: test.disallowed, AllowBash: test.allowBash,
				}},
			}
			agent := &corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "default", UID: "agent-uid", Generation: 1},
				Spec: corev1alpha1.AgentSpec{
					Model: &corev1alpha1.ModelConfig{Name: "model"},
					Runtime: &corev1alpha1.AgentCLIRuntime{
						Type: test.provider,
					},
				},
			}
			plan, err := PlanACPRuntime(task, agent, test.images)
			if err != nil {
				t.Fatal(err)
			}
			configuration, err := buildRuntimeSessionMCPConfiguration(
				context.Background(), nil, task, agent, plan.Profile,
			)
			if err != nil {
				t.Fatal(err)
			}
			if err := configuration.ValidateProfile(plan.Profile); err != nil {
				t.Fatalf("ValidateProfile() error = %v", err)
			}
			if !slices.Equal(configuration.ToolPolicy.AllowedToolNames, test.wantAllowed) {
				t.Fatalf("allowed tools = %v, want %v", configuration.ToolPolicy.AllowedToolNames, test.wantAllowed)
			}
			if !slices.Equal(configuration.ToolPolicy.DisallowedToolNames, test.wantDisallowed) {
				t.Fatalf("disallowed tools = %v, want %v", configuration.ToolPolicy.DisallowedToolNames, test.wantDisallowed)
			}
			if configuration.ToolPolicy.AllowBash != test.wantAllowBash {
				t.Fatalf("allowBash = %t, want %t", configuration.ToolPolicy.AllowBash, test.wantAllowBash)
			}
			gotDescriptors := make([]string, 0, len(configuration.ToolPolicy.Tools))
			for _, descriptor := range configuration.ToolPolicy.Tools {
				if descriptor.Source != harnessv2.MCPToolSourceProviderNative {
					t.Fatalf("descriptor %q source = %q, want provider_native", descriptor.Name, descriptor.Source)
				}
				gotDescriptors = append(gotDescriptors, descriptor.Name)
			}
			if !slices.Equal(gotDescriptors, test.wantAllowed) {
				t.Fatalf("provider-native descriptors = %v, want %v", gotDescriptors, test.wantAllowed)
			}
		})
	}
}

func TestBuildRuntimeSessionMCPConfigurationTranslatesReadOnlyOpenCodeTools(t *testing.T) {
	image := "docker.io/example/opencode@sha256:" + strings.Repeat("d", 64)
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: "review", Namespace: "default", UID: "task-uid",
			Annotations: map[string]string{labels.AnnotationAgentReadOnly: scheduledRunLabelValue},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:         corev1alpha1.TaskTypeAgent,
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{AllowedTools: readOnlyAgentAllowedTools()},
			Workspace:    &corev1alpha1.WorkspaceConfig{Intent: corev1alpha1.WorkspaceIntentRead},
		},
	}
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "reviewer", Namespace: "default", UID: "agent-uid", Generation: 1},
		Spec: corev1alpha1.AgentSpec{
			Model:   testOpenCodeModelConfig(),
			Runtime: &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeOpencode},
		},
	}
	plan, err := PlanACPRuntime(task, agent, ACPRuntimeImages{Opencode: image})
	if err != nil {
		t.Fatal(err)
	}
	configuration, err := buildRuntimeSessionMCPConfiguration(context.Background(), nil, task, agent, plan.Profile)
	if err != nil {
		t.Fatalf("buildRuntimeSessionMCPConfiguration() error = %v", err)
	}
	if want := []string{"glob", "read"}; !slices.Equal(configuration.ToolPolicy.AllowedToolNames, want) {
		t.Fatalf("read-only OpenCode tools = %#v, want %#v", configuration.ToolPolicy.AllowedToolNames, want)
	}
}

func TestEffectiveACPAllowedToolsOnlyTranslatesReadOnlyOpenCodePreset(t *testing.T) {
	agent := &corev1alpha1.Agent{Spec: corev1alpha1.AgentSpec{Runtime: &corev1alpha1.AgentCLIRuntime{
		Type: corev1alpha1.AgentRuntimeOpencode,
	}}}
	for _, tt := range []struct {
		name    string
		allowed []string
		want    []string
	}{
		{name: "repository monitor preset", allowed: readOnlyAgentAllowedTools(), want: []string{providerNativeToolGlob, providerNativeToolRead}},
		{name: "explicit deny all", allowed: []string{}, want: []string{}},
		{name: "all blank remains deny all", allowed: []string{" "}, want: []string{}},
		{name: "narrower glob only", allowed: []string{providerNativeToolGlob}, want: []string{providerNativeToolGlob}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{labels.AnnotationAgentReadOnly: scheduledRunLabelValue}},
				Spec:       corev1alpha1.TaskSpec{AgentRuntime: &corev1alpha1.AgentRuntimeSpec{AllowedTools: tt.allowed}},
			}
			got := effectiveACPAllowedTools(task, agent)
			if !slices.Equal(got, tt.want) || (tt.want != nil && got == nil) {
				t.Fatalf("effective tools = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestNormalizeACPProviderNativeToolPolicyPreservesExplicitAndUnrestrictedPolicies(t *testing.T) {
	explicitAllowed := []string{providerNativeToolRead, "web_search"}
	explicitDisallowed := []string{providerNativeToolWrite}
	allowed, disallowed, allowBash := normalizeACPProviderNativeToolPolicy(
		"claude", explicitAllowed, explicitDisallowed, false,
	)
	if !slices.Equal(allowed, explicitAllowed) || !slices.Equal(disallowed, explicitDisallowed) || allowBash {
		t.Fatalf("explicit policy normalized to allowed=%v disallowed=%v allowBash=%t", allowed, disallowed, allowBash)
	}

	allowed, disallowed, allowBash = normalizeACPProviderNativeToolPolicy("copilot", nil, nil, true)
	if allowed != nil || disallowed != nil || !allowBash {
		t.Fatalf("unrestricted policy normalized to allowed=%v disallowed=%v allowBash=%t", allowed, disallowed, allowBash)
	}

	allowed, disallowed, allowBash = normalizeACPProviderNativeToolPolicy("claude", nil, nil, false)
	wantBashDisabled := []string{providerNativeToolEdit, providerNativeToolGlob, providerNativeToolGrep, providerNativeToolRead, providerNativeToolWebFetch, providerNativeToolWebSearch, providerNativeToolWrite}
	if !slices.Equal(allowed, wantBashDisabled) || disallowed != nil || allowBash {
		t.Fatalf("bash-disabled policy normalized to allowed=%v disallowed=%v allowBash=%t", allowed, disallowed, allowBash)
	}
}

func TestBuildRuntimeSessionMCPConfigurationDeliversCanonicalToolDescriptors(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	custom := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "dispatch_work", Namespace: "default"},
		Spec: corev1alpha1.ToolSpec{
			Description: "Dispatch a work order", BrokeredToolClass: corev1alpha1.AgentRuntimeBrokeredToolClassWrite,
			Parameters: &apiextensionsv1.JSON{Raw: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}}}`)},
		},
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(custom).Build()
	allowBash := false
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: "default", UID: "task-uid"},
		Spec: corev1alpha1.TaskSpec{
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{
				AllowedTools: []string{"web_search", providerNativeToolRead, "dispatch_work"}, AllowBash: &allowBash,
			},
		},
	}
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "default", UID: "agent-uid", Generation: 1},
		Spec: corev1alpha1.AgentSpec{
			Model:        &corev1alpha1.ModelConfig{Name: "model"},
			Runtime:      &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeClaude},
			Coordination: &corev1alpha1.CoordinationConfig{ApprovalRequiredTools: []string{"dispatch_work"}},
		},
	}
	plan, err := PlanACPRuntime(task, agent, ACPRuntimeImages{Claude: "docker.io/example/claude@sha256:" + strings.Repeat("a", 64)})
	if err != nil {
		t.Fatal(err)
	}
	configuration, err := buildRuntimeSessionMCPConfiguration(context.Background(), reader, task, agent, plan.Profile)
	if err != nil {
		t.Fatal(err)
	}
	if err := configuration.ValidateProfile(plan.Profile); err != nil {
		t.Fatalf("ValidateProfile() error = %v", err)
	}
	if len(configuration.ToolPolicy.Tools) != 3 {
		t.Fatalf("descriptor count = %d, want 3", len(configuration.ToolPolicy.Tools))
	}
	byName := make(map[string]harnessv2.MCPToolDescriptor, len(configuration.ToolPolicy.Tools))
	for _, descriptor := range configuration.ToolPolicy.Tools {
		byName[descriptor.Name] = descriptor
	}
	if byName[providerNativeToolRead].Source != harnessv2.MCPToolSourceProviderNative || byName[providerNativeToolRead].Effect != harnessv2.MCPToolEffectReadOnly {
		t.Fatalf("provider-native descriptor = %#v", byName[providerNativeToolRead])
	}
	if byName["web_search"].Source != harnessv2.MCPToolSourceBrokeredBuiltin || byName["web_search"].Effect != harnessv2.MCPToolEffectReadOnly {
		t.Fatalf("built-in descriptor = %#v", byName["web_search"])
	}
	if byName["dispatch_work"].Source != harnessv2.MCPToolSourceBrokeredCustom || byName["dispatch_work"].Effect != harnessv2.MCPToolEffectConsequential {
		t.Fatalf("custom descriptor = %#v", byName["dispatch_work"])
	}
	if !configuration.ApprovalPolicy.Requires("dispatch_work") {
		t.Fatal("custom approval-required tool was not frozen into the policy")
	}
	if configuration.ToolPolicy.DescriptorDigest == "" {
		t.Fatal("descriptor digest is empty")
	}
	encoded, err := json.Marshal(configuration)
	if err != nil {
		t.Fatalf("marshal MCP configuration: %v", err)
	}
	var decoded harnessv2.MCPPolicyConfiguration
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal MCP configuration: %v", err)
	}
	if err := decoded.ValidateProfile(plan.Profile); err != nil {
		t.Fatalf("JSON round-trip ValidateProfile() error = %v", err)
	}
}

func TestBuildRuntimeSessionMCPConfigurationRejectsControllerLocalTools(t *testing.T) {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: "default", UID: "task-uid"},
		Spec:       corev1alpha1.TaskSpec{AgentRuntime: &corev1alpha1.AgentRuntimeSpec{AllowedTools: []string{"file_read"}}},
	}
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "default", UID: "agent-uid", Generation: 1},
		Spec: corev1alpha1.AgentSpec{
			Model:   &corev1alpha1.ModelConfig{Name: "model"},
			Runtime: &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeClaude},
		},
	}
	_, err := PlanACPRuntime(task, agent, ACPRuntimeImages{Claude: "docker.io/example/claude@sha256:" + strings.Repeat("a", 64)})
	if err == nil || !strings.Contains(err.Error(), "local-process-only") {
		t.Fatalf("controller-local tool error = %v", err)
	}
}

func TestBuildRuntimeSessionMCPConfigurationClassifiesOpenCodeNativeTools(t *testing.T) {
	requestedNativeTools := []string{"Read", "Write", "Edit", "Bash", "Glob", "Grep"}
	nativeTools := []string{"apply_patch", "bash", "edit", "glob", "grep", "read", "write"}
	brokeredTools := []string{"web_search", "web_fetch"}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: "default", UID: "task-uid"},
		Spec: corev1alpha1.TaskSpec{
			Type:         corev1alpha1.TaskTypeAgent,
			Workspace:    &corev1alpha1.WorkspaceConfig{Intent: corev1alpha1.WorkspaceIntentWrite},
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{AllowedTools: append(append([]string(nil), requestedNativeTools...), brokeredTools...)},
		},
	}
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "default", UID: "agent-uid", Generation: 1},
		Spec: corev1alpha1.AgentSpec{
			Model:   testOpenCodeModelConfig(),
			Runtime: &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeOpencode},
		},
	}
	plan, err := PlanACPRuntime(task, agent, ACPRuntimeImages{
		Opencode: "docker.io/example/opencode@sha256:" + strings.Repeat("a", 64),
	})
	if err != nil {
		t.Fatal(err)
	}

	configuration, err := buildRuntimeSessionMCPConfiguration(context.Background(), nil, task, agent, plan.Profile)
	if err != nil {
		t.Fatal(err)
	}
	if want := len(nativeTools) + len(brokeredTools); len(configuration.ToolPolicy.Tools) != want {
		t.Fatalf("descriptor count = %d, want %d", len(configuration.ToolPolicy.Tools), want)
	}
	byName := make(map[string]harnessv2.MCPToolDescriptor, len(configuration.ToolPolicy.Tools))
	for _, descriptor := range configuration.ToolPolicy.Tools {
		byName[descriptor.Name] = descriptor
	}
	for _, name := range nativeTools {
		if got := byName[name].Source; got != harnessv2.MCPToolSourceProviderNative {
			t.Fatalf("OpenCode tool %q source = %q, want provider-native", name, got)
		}
	}
	for _, name := range brokeredTools {
		if got := byName[name].Source; got != harnessv2.MCPToolSourceBrokeredBuiltin {
			t.Fatalf("OpenCode tool %q source = %q, want brokered built-in", name, got)
		}
	}
	for _, name := range []string{"read", "glob", "grep"} {
		if got := byName[name].Effect; got != harnessv2.MCPToolEffectReadOnly {
			t.Fatalf("OpenCode tool %q effect = %q, want read-only", name, got)
		}
	}
	for _, name := range []string{"bash", "apply_patch", "edit", "write"} {
		if got := byName[name].Effect; got != harnessv2.MCPToolEffectConsequential {
			t.Fatalf("OpenCode tool %q effect = %q, want consequential", name, got)
		}
	}
}

func TestBuildRuntimeSessionMCPConfigurationNormalizesOpenCodeMutationAliases(t *testing.T) {
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "default", UID: "agent-uid", Generation: 1},
		Spec: corev1alpha1.AgentSpec{
			Model:   testOpenCodeModelConfig(),
			Runtime: &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeOpencode},
		},
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: "default", UID: "task-uid"},
		Spec: corev1alpha1.TaskSpec{
			Type:         corev1alpha1.TaskTypeAgent,
			Workspace:    &corev1alpha1.WorkspaceConfig{Intent: corev1alpha1.WorkspaceIntentWrite},
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{AllowedTools: []string{"Edit"}},
		},
	}
	images := ACPRuntimeImages{Opencode: "docker.io/example/opencode@sha256:" + strings.Repeat("a", 64)}
	plan, err := PlanACPRuntime(task, agent, images)
	if err != nil {
		t.Fatal(err)
	}
	configuration, err := buildRuntimeSessionMCPConfiguration(context.Background(), nil, task, agent, plan.Profile)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"apply_patch", "edit", "write"}
	if !slices.Equal(configuration.ToolPolicy.AllowedToolNames, want) {
		t.Fatalf("allowed mutation aliases = %#v, want %#v", configuration.ToolPolicy.AllowedToolNames, want)
	}
	for _, name := range want {
		descriptor, ok := configuration.ToolPolicy.Descriptor(name)
		if !ok || descriptor.Source != harnessv2.MCPToolSourceProviderNative {
			t.Fatalf("mutation alias %q descriptor = %#v, found=%v", name, descriptor, ok)
		}
	}
}

func TestBuildRuntimeSessionMCPConfigurationDefaultsOpenCodeToolsOnlyWhenOmitted(t *testing.T) {
	tests := []struct {
		name         string
		allowedTools []string
		want         []string
	}{
		{
			name: "omitted",
			want: []string{"apply_patch", "bash", "edit", "glob", "grep", "read", "write"},
		},
		{
			name:         "explicit empty",
			allowedTools: []string{},
			want:         []string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agent := &corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "default", UID: "agent-uid", Generation: 1},
				Spec: corev1alpha1.AgentSpec{
					Model: testOpenCodeModelConfig(),
					Runtime: &corev1alpha1.AgentCLIRuntime{
						Type:                corev1alpha1.AgentRuntimeOpencode,
						DefaultAllowedTools: tt.allowedTools,
					},
				},
			}
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: "default", UID: "task-uid"},
				Spec: corev1alpha1.TaskSpec{
					Type:      corev1alpha1.TaskTypeAgent,
					Workspace: &corev1alpha1.WorkspaceConfig{Intent: corev1alpha1.WorkspaceIntentWrite},
				},
			}
			images := ACPRuntimeImages{Opencode: "docker.io/example/opencode@sha256:" + strings.Repeat("a", 64)}
			plan, err := PlanACPRuntime(task, agent, images)
			if err != nil {
				t.Fatal(err)
			}
			configuration, err := buildRuntimeSessionMCPConfiguration(context.Background(), nil, task, agent, plan.Profile)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(configuration.ToolPolicy.AllowedToolNames, tt.want) {
				t.Fatalf("allowed tools = %#v, want %#v", configuration.ToolPolicy.AllowedToolNames, tt.want)
			}
		})
	}
}

func TestBuildRuntimeSessionMCPConfigurationOpenCodeReadIntentClosesConsequentialTools(t *testing.T) {
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "default", UID: "agent-uid", Generation: 1},
		Spec: corev1alpha1.AgentSpec{
			Model: testOpenCodeModelConfig(),
			Runtime: &corev1alpha1.AgentCLIRuntime{
				Type:                corev1alpha1.AgentRuntimeOpencode,
				DefaultAllowedTools: []string{"Read", "Write", "Edit", "Bash", "Glob", "Grep"},
				DefaultAllowBash:    new(true),
			},
		},
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: "default", UID: "task-uid"},
		Spec: corev1alpha1.TaskSpec{
			Type:      corev1alpha1.TaskTypeAgent,
			Workspace: &corev1alpha1.WorkspaceConfig{Intent: corev1alpha1.WorkspaceIntentRead},
		},
	}
	images := ACPRuntimeImages{Opencode: "docker.io/example/opencode@sha256:" + strings.Repeat("a", 64)}
	plan, err := PlanACPRuntime(task, agent, images)
	if err != nil {
		t.Fatal(err)
	}
	configuration, err := buildRuntimeSessionMCPConfiguration(context.Background(), nil, task, agent, plan.Profile)
	if err != nil {
		t.Fatal(err)
	}
	if configuration.ToolPolicy.AllowBash {
		t.Fatal("read-intent OpenCode policy retained bash authority")
	}
	for _, name := range []string{"apply_patch", "bash", "edit", "grep", "write"} {
		if configuration.ToolPolicy.Allows(name) {
			t.Fatalf("read-intent OpenCode policy allows %q: %#v", name, configuration.ToolPolicy)
		}
	}
	for _, name := range []string{"glob", "read"} {
		if !configuration.ToolPolicy.Allows(name) {
			t.Fatalf("read-intent OpenCode policy removed %q: %#v", name, configuration.ToolPolicy)
		}
	}
}

func TestBuildRuntimeSessionMCPConfigurationOpenCodeDenyIsCaseInsensitive(t *testing.T) {
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "default", UID: "agent-uid", Generation: 1},
		Spec: corev1alpha1.AgentSpec{
			Model: testOpenCodeModelConfig(),
			Runtime: &corev1alpha1.AgentCLIRuntime{
				Type: corev1alpha1.AgentRuntimeOpencode, DefaultAllowedTools: []string{"Read", "Bash"},
			},
		},
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: "default", UID: "task-uid"},
		Spec: corev1alpha1.TaskSpec{
			Type:      corev1alpha1.TaskTypeAgent,
			Workspace: &corev1alpha1.WorkspaceConfig{Intent: corev1alpha1.WorkspaceIntentWrite},
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{
				DisallowedTools: []string{"bAsH"},
			},
		},
	}
	images := ACPRuntimeImages{Opencode: "docker.io/example/opencode@sha256:" + strings.Repeat("a", 64)}
	plan, err := PlanACPRuntime(task, agent, images)
	if err != nil {
		t.Fatal(err)
	}
	configuration, err := buildRuntimeSessionMCPConfiguration(context.Background(), nil, task, agent, plan.Profile)
	if err != nil {
		t.Fatal(err)
	}
	if configuration.ToolPolicy.Allows("bash") {
		t.Fatalf("case-variant bash deny did not close policy: %#v", configuration.ToolPolicy)
	}
	if !configuration.ToolPolicy.Allows("read") {
		t.Fatalf("read permission was unexpectedly removed: %#v", configuration.ToolPolicy)
	}
}

func TestBuildRuntimeSessionMCPConfigurationRejectsUngovernedOpenCodeNativeTools(t *testing.T) {
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "default", UID: "agent-uid", Generation: 1},
		Spec: corev1alpha1.AgentSpec{
			Model:   testOpenCodeModelConfig(),
			Runtime: &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeOpencode},
		},
	}
	for _, toolName := range []string{"task", "skill", "question", "todowrite", "WebSearch", "WebFetch"} {
		t.Run(toolName, func(t *testing.T) {
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: "default", UID: "task-uid"},
				Spec: corev1alpha1.TaskSpec{
					Type:         corev1alpha1.TaskTypeAgent,
					AgentRuntime: &corev1alpha1.AgentRuntimeSpec{AllowedTools: []string{toolName}},
				},
			}
			plan, err := PlanACPRuntime(task, agent, ACPRuntimeImages{
				Opencode: "docker.io/example/opencode@sha256:" + strings.Repeat("a", 64),
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := buildRuntimeSessionMCPConfiguration(context.Background(), nil, task, agent, plan.Profile); err == nil ||
				!strings.Contains(err.Error(), "not a known provider-native") {
				t.Fatalf("ungoverned OpenCode native tool error = %v, want rejection", err)
			}
		})
	}
}

func TestBuildRuntimeSessionMCPConfigurationRejectsUnknownOrUngovernedTools(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).Build()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: "default", UID: "task-uid"},
		Spec:       corev1alpha1.TaskSpec{AgentRuntime: &corev1alpha1.AgentRuntimeSpec{AllowedTools: []string{"unknown_tool"}}},
	}
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "default", UID: "agent-uid", Generation: 1},
		Spec: corev1alpha1.AgentSpec{
			Model:   &corev1alpha1.ModelConfig{Name: "model"},
			Runtime: &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeClaude},
		},
	}
	plan, err := PlanACPRuntime(task, agent, ACPRuntimeImages{Claude: "docker.io/example/claude@sha256:" + strings.Repeat("a", 64)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := buildRuntimeSessionMCPConfiguration(context.Background(), reader, task, agent, plan.Profile); err == nil ||
		!strings.Contains(err.Error(), "not a known") {
		t.Fatalf("unknown tool error = %v", err)
	}

	unclassified := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "unknown_tool", Namespace: "default"},
		Spec:       corev1alpha1.ToolSpec{Description: "not governed"},
	}
	reader = fake.NewClientBuilder().WithScheme(scheme).WithObjects(unclassified).Build()
	if _, err := buildRuntimeSessionMCPConfiguration(context.Background(), reader, task, agent, plan.Profile); err == nil ||
		!strings.Contains(err.Error(), "brokeredToolClass") {
		t.Fatalf("unclassified tool error = %v", err)
	}
}

func TestBuildRuntimeSessionMCPConfigurationHonorsExplicitEmptyTaskOpenCodeTools(t *testing.T) {
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "default", UID: "agent-uid", Generation: 1},
		Spec: corev1alpha1.AgentSpec{
			Model:   testOpenCodeModelConfig(),
			Runtime: &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeOpencode},
		},
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: "default", UID: "task-uid"},
		Spec: corev1alpha1.TaskSpec{
			Type:         corev1alpha1.TaskTypeAgent,
			Workspace:    &corev1alpha1.WorkspaceConfig{Intent: corev1alpha1.WorkspaceIntentWrite},
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{AllowedTools: []string{}},
		},
	}
	images := ACPRuntimeImages{Opencode: "docker.io/example/opencode@sha256:" + strings.Repeat("a", 64)}
	plan, err := PlanACPRuntime(task, agent, images)
	if err != nil {
		t.Fatal(err)
	}
	configuration, err := buildRuntimeSessionMCPConfiguration(context.Background(), nil, task, agent, plan.Profile)
	if err != nil {
		t.Fatal(err)
	}
	if configuration.ToolPolicy.AllowedToolNames == nil || len(configuration.ToolPolicy.AllowedToolNames) != 0 {
		t.Fatalf("allowed tools = %#v, want explicit deny-all", configuration.ToolPolicy.AllowedToolNames)
	}
}
