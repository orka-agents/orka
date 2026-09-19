package tools

import (
	"context"
	"encoding/json"
	"maps"
	"reflect"
	"strings"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/agentcontext"
	"github.com/orka-agents/orka/internal/executionmode"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

type soulCreateInstructionCase struct {
	name       string
	role       string
	soul       *corev1alpha1.SoulSource
	invalidFor string
}

func (tc soulCreateInstructionCase) expectedError(mode executionmode.Mode, kind corev1alpha1.AgentRuntimeType) string {
	if tc.soul != nil && kind != "" && mode == executionmode.HarnessV1 {
		return "built-in harness v2"
	}
	if kind == corev1alpha1.AgentRuntimeCopilot && mode == executionmode.HarnessV2 {
		return tc.invalidFor
	}
	return ""
}

func TestSoulAgentCreationInlineCopilotValidation(t *testing.T) {
	t.Setenv(envOrkaTaskName, "parent")
	t.Setenv(envOrkaTaskNamespace, defaultNamespace)
	referencedSoul := &corev1alpha1.SoulSource{
		ConfigMapRef: &corev1alpha1.ConfigMapKeySelector{Name: "instructions", Key: "SOUL.md"},
		Digest:       agentcontext.Digest("Follow @soul.md"),
	}
	cases := []soulCreateInstructionCase{
		{name: "valid role only", role: "Review the change."},
		{name: "invalid role only", role: "Follow @role.md", invalidFor: "systemPrompt.inline"},
		{name: "valid soul only", soul: &corev1alpha1.SoulSource{Inline: "Be direct."}},
		{name: "invalid soul only", soul: &corev1alpha1.SoulSource{Inline: "Follow @soul.md"}, invalidFor: "soul.inline"},
		{name: "invalid soul with role", role: "Review the change.", soul: &corev1alpha1.SoulSource{Inline: "Follow @soul.md"}, invalidFor: "soul.inline"},
		{name: "invalid role with soul", role: "Follow @role.md", soul: &corev1alpha1.SoulSource{Inline: "Be direct."}, invalidFor: "systemPrompt.inline"},
		{name: "unresolved soul", role: "Review the change.", soul: referencedSoul},
		{name: "invalid role with referenced soul", role: "Follow @role.md", soul: referencedSoul, invalidFor: "systemPrompt.inline"},
	}
	for _, mode := range []executionmode.Mode{executionmode.HarnessV1, executionmode.HarnessV2} {
		for _, kind := range []corev1alpha1.AgentRuntimeType{
			"", corev1alpha1.AgentRuntimeCodex, corev1alpha1.AgentRuntimeClaude,
			corev1alpha1.AgentRuntimeCopilot, corev1alpha1.AgentRuntimeOpencode,
		} {
			for _, tc := range cases {
				for _, toolName := range []string{"native", "chat"} {
					t.Run(string(mode)+"/"+string(kind)+"/"+tc.name+"/"+toolName, func(t *testing.T) {
						checkSoulCreatedInlineInstructions(t, mode, kind, tc, toolName == "chat")
					})
				}
			}
		}
	}
}

func checkSoulCreatedInlineInstructions(t *testing.T, mode executionmode.Mode, kind corev1alpha1.AgentRuntimeType, tc soulCreateInstructionCase, chat bool) {
	t.Helper()
	ctx := context.Background()
	parent := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "parent", Namespace: defaultNamespace, UID: "parent-uid"}}
	creates, sourceReads := 0, 0
	c := newFakeClientWithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			creates++
			return assignFakeUIDOnCreate(ctx, c, obj, opts...)
		},
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*corev1.ConfigMap); ok {
				sourceReads++
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}, parent, copilotReferencedInstructions())
	model := map[string]any{"provider": "openai", "name": "review-model"}
	if kind == corev1alpha1.AgentRuntimeOpencode {
		model = map[string]any{"name": "openai/gpt-5.4", "contextWindow": 32768, "maxTokens": 4096}
	}
	args := map[string]any{
		"role": "reviewer", nameField: "reviewer", modelField: model, systemPromptField: tc.role,
	}
	if tc.soul != nil {
		args["soul"] = tc.soul
	}
	if kind != "" {
		args[runtimeField] = map[string]any{"type": kind}
	}
	var tool Tool = NewCreateAgentTool(c, mode)
	if chat {
		tool = &ChatCreateAgentTool{}
		args["initialPrompt"] = "Review now."
		ctx = WithToolContext(ctx, &ToolContext{
			Client: c, Namespace: defaultNamespace, ExecutionMode: mode,
			CheckTaskLimit:   func() *ChatToolError { return nil },
			GenerateTaskName: func() string { return "initial-task" },
			TaskLabels:       func() map[string]string { return nil },
			IncrementTasks:   func() {},
		})
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	result, callErr := tool.Execute(ctx, encoded)
	callErr = soulCreationCallError(t, chat, result, callErr)
	wantError := tc.expectedError(mode, kind)
	if wantError == "" && callErr != nil {
		t.Errorf("expected successful creation, got %v", callErr)
	} else if wantError != "" && (callErr == nil || !strings.Contains(callErr.Error(), wantError)) {
		t.Errorf("expected %q rejection, got %v", wantError, callErr)
	}
	if sourceReads != 0 {
		t.Errorf("inline validation read %d ConfigMaps", sourceReads)
	}
	agents := &corev1alpha1.AgentList{}
	if err := c.List(ctx, agents, client.InNamespace(defaultNamespace)); err != nil {
		t.Fatal(err)
	}
	tasks := &corev1alpha1.TaskList{}
	if err := c.List(ctx, tasks, client.InNamespace(defaultNamespace)); err != nil {
		t.Fatal(err)
	}
	if wantError != "" {
		if creates != 0 || len(agents.Items) != 0 || len(tasks.Items) != 1 {
			t.Fatalf("rejected creation performed writes: creates=%d, agents=%d, tasks=%d", creates, len(agents.Items), len(tasks.Items))
		}
		return
	}
	wantCreates, wantTasks := 1, 1
	if chat {
		wantCreates, wantTasks = 2, 2
	}
	if creates != wantCreates || len(agents.Items) != 1 || len(tasks.Items) != wantTasks {
		t.Fatalf("successful creation did not persist expected resources: creates=%d, agents=%d, tasks=%d", creates, len(agents.Items), len(tasks.Items))
	}
	created := &agents.Items[0]
	if !reflect.DeepEqual(created.Spec.Soul, tc.soul) {
		t.Fatal("creation changed the declared soul")
	}
	if tc.role != "" && (created.Spec.SystemPrompt == nil || created.Spec.SystemPrompt.Inline != tc.role) {
		t.Fatal("creation changed the declared role")
	}
}

type soulUpdateInstructionCase struct {
	name      string
	role      *corev1alpha1.PromptSource
	soul      *corev1alpha1.SoulSource
	args      map[string]any
	wantError string
}

func TestSoulAgentUpdateValidatesCompletedCopilotInstructions(t *testing.T) {
	role := &corev1alpha1.PromptSource{Inline: "Review the change."}
	badRole := &corev1alpha1.PromptSource{Inline: "Follow @role.md"}
	soul := &corev1alpha1.SoulSource{Inline: "Be direct."}
	badSoul := &corev1alpha1.SoulSource{Inline: "Follow @soul.md"}
	referencedSoul := &corev1alpha1.SoulSource{
		ConfigMapRef: &corev1alpha1.ConfigMapKeySelector{Name: "instructions", Key: "SOUL.md"},
		Digest:       agentcontext.Digest("Follow @soul.md"),
	}
	referencedRole := &corev1alpha1.PromptSource{ConfigMapRef: &corev1alpha1.ConfigMapKeySelector{Name: "instructions", Key: "ROLE.md"}}
	for _, tc := range []soulUpdateInstructionCase{
		{name: "valid role only", role: role, args: map[string]any{systemPromptField: "Updated role."}},
		{name: "invalid role only", role: role, args: map[string]any{systemPromptField: badRole.Inline}, wantError: "systemPrompt.inline"},
		{name: "invalid new soul", role: role, args: map[string]any{"soul": badSoul}, wantError: "soul.inline"},
		{name: "invalid soul only", args: map[string]any{"soul": badSoul}, wantError: "soul.inline"},
		{name: "retained invalid role", role: badRole, args: map[string]any{"soul": soul}, wantError: "systemPrompt.inline"},
		{name: "retained invalid soul", role: role, soul: badSoul, args: map[string]any{systemPromptField: "Updated role."}, wantError: "soul.inline"},
		{name: "retained invalid role without instruction changes", role: badRole, wantError: "systemPrompt.inline"},
		{name: "retained invalid soul without instruction changes", role: role, soul: badSoul, wantError: "soul.inline"},
		{name: "repair existing role", role: badRole, args: map[string]any{systemPromptField: "Updated role."}},
		{name: "repair existing soul", role: role, soul: badSoul, args: map[string]any{"soul": soul}},
		{name: "unresolved soul", role: role, args: map[string]any{"soul": referencedSoul}},
		{name: "invalid role with referenced soul", role: role, soul: referencedSoul, args: map[string]any{systemPromptField: badRole.Inline}, wantError: "systemPrompt.inline"},
		{name: "unresolved role", role: referencedRole, args: map[string]any{"soul": soul}},
		{name: "invalid soul with referenced role", role: referencedRole, args: map[string]any{"soul": badSoul}, wantError: "soul.inline"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checkSoulUpdatedInlineInstructions(t, &corev1alpha1.AgentCLIRuntime{
				Type: corev1alpha1.AgentRuntimeCopilot, ContractVersion: new(corev1alpha1.AgentRuntimeContractHarnessV2),
			}, tc)
		})
	}
}

func TestSoulAgentUpdateCopilotValidationPreservesOtherRoutes(t *testing.T) {
	for _, route := range []struct {
		name        string
		runtime     *corev1alpha1.AgentCLIRuntime
		supportSoul bool
	}{
		{name: "ai", supportSoul: true},
		{name: "codex v2", runtime: &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeCodex, ContractVersion: new(corev1alpha1.AgentRuntimeContractHarnessV2)}, supportSoul: true},
		{name: "claude v2", runtime: &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeClaude, ContractVersion: new(corev1alpha1.AgentRuntimeContractHarnessV2)}, supportSoul: true},
		{name: "opencode v2", runtime: &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeOpencode, ContractVersion: new(corev1alpha1.AgentRuntimeContractHarnessV2)}, supportSoul: true},
		{name: "copilot v1", runtime: &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeCopilot, ContractVersion: new(corev1alpha1.AgentRuntimeContractHarnessV1)}},
		{name: "copilot missing contract", runtime: &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeCopilot}},
		{name: "external copilot v2", runtime: &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeCopilot, RuntimeRef: &corev1alpha1.AgentRuntimeReference{Name: "external"}, ContractVersion: new(corev1alpha1.AgentRuntimeContractHarnessV2)}},
		{name: "external copilot v1", runtime: &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeCopilot, RuntimeRef: &corev1alpha1.AgentRuntimeReference{Name: "external"}, ContractVersion: new(corev1alpha1.AgentRuntimeContractHarnessV1)}},
	} {
		for _, withSoul := range []bool{false, true} {
			if withSoul && !route.supportSoul {
				continue
			}
			name := route.name + "/role only"
			args := map[string]any{systemPromptField: "Follow @role.md"}
			if withSoul {
				name = route.name + "/role and soul"
				args["soul"] = &corev1alpha1.SoulSource{Inline: "Follow @soul.md"}
			}
			t.Run(name, func(t *testing.T) {
				checkSoulUpdatedInlineInstructions(t, route.runtime, soulUpdateInstructionCase{
					role: &corev1alpha1.PromptSource{Inline: "Original role."}, args: args,
				})
			})
		}
	}
}

func checkSoulUpdatedInlineInstructions(t *testing.T, runtime *corev1alpha1.AgentCLIRuntime, tc soulUpdateInstructionCase) {
	t.Helper()
	ctx := context.Background()
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "reviewer", Namespace: defaultNamespace},
		Spec: corev1alpha1.AgentSpec{
			Runtime: runtime.DeepCopy(), SystemPrompt: tc.role.DeepCopy(), Soul: tc.soul.DeepCopy(),
			Model: &corev1alpha1.ModelConfig{Provider: "openai", Name: "review-model"},
		},
	}
	if runtime != nil && runtime.Type == corev1alpha1.AgentRuntimeOpencode {
		agent.Spec.Model = testOpenCodeModelConfig("openai/gpt-5.4")
	}
	updates, sourceReads := 0, 0
	c := newFakeClientWithInterceptorFuncs(interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			updates++
			return c.Update(ctx, obj, opts...)
		},
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*corev1.ConfigMap); ok {
				sourceReads++
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}, agent, copilotReferencedInstructions())
	before := &corev1alpha1.Agent{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(agent), before); err != nil {
		t.Fatal(err)
	}
	args := map[string]any{nameField: agent.Name}
	maps.Copy(args, tc.args)
	encoded, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	result, err := (&UpdateAgentTool{}).Execute(WithToolContext(ctx, &ToolContext{Client: c, Namespace: defaultNamespace}), encoded)
	if err != nil {
		t.Fatal(err)
	}
	var response ChatToolResult
	if err := json.Unmarshal([]byte(result), &response); err != nil {
		t.Fatal(err)
	}
	after := &corev1alpha1.Agent{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(agent), after); err != nil {
		t.Fatal(err)
	}
	if sourceReads != 0 {
		t.Errorf("inline validation read %d ConfigMaps", sourceReads)
	}
	if tc.wantError != "" {
		if response.Success || response.ErrorType != "invalid_arguments" || !strings.Contains(response.Error, tc.wantError) {
			t.Errorf("expected %q rejection, got %#v", tc.wantError, response)
		}
		if updates != 0 || !reflect.DeepEqual(before, after) {
			t.Errorf("rejected update persisted changes: updates=%d", updates)
		}
		return
	}
	if !response.Success || updates != 1 {
		t.Fatalf("expected one successful update, got %#v, updates=%d", response, updates)
	}
	want := before.DeepCopy()
	if role, ok := tc.args[systemPromptField].(string); ok {
		want.Spec.SystemPrompt = &corev1alpha1.PromptSource{Inline: role}
	}
	if soul, ok := tc.args["soul"].(*corev1alpha1.SoulSource); ok {
		want.Spec.Soul = soul.DeepCopy()
	}
	if !reflect.DeepEqual(want.Spec, after.Spec) {
		t.Fatal("successful update changed more than the requested fields")
	}
}

func copilotReferencedInstructions() *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "instructions", Namespace: defaultNamespace},
		Data:       map[string]string{"SOUL.md": "Follow @soul.md", "ROLE.md": "Follow @role.md"},
	}
}
