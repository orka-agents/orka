package tools

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/agentcontext"
	"github.com/orka-agents/orka/internal/executionmode"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestSoulCreationRuntimeCompatibility(t *testing.T) {
	t.Setenv(envOrkaTaskName, "parent")
	t.Setenv(envOrkaTaskNamespace, defaultNamespace)
	for _, mode := range []executionmode.Mode{executionmode.HarnessV1, executionmode.HarnessV2} {
		for _, kind := range []corev1alpha1.AgentRuntimeType{
			"", corev1alpha1.AgentRuntimeCodex, corev1alpha1.AgentRuntimeClaude,
			corev1alpha1.AgentRuntimeCopilot, corev1alpha1.AgentRuntimeOpencode,
		} {
			runtimeName := string(kind)
			if runtimeName == "" {
				runtimeName = "ai"
			}
			for _, source := range []struct {
				name string
				soul *corev1alpha1.SoulSource
			}{
				{name: "no soul"},
				{name: "inline", soul: &corev1alpha1.SoulSource{Inline: "Be direct."}},
				{name: "configmap", soul: &corev1alpha1.SoulSource{
					ConfigMapRef: &corev1alpha1.ConfigMapKeySelector{Name: "persona", Key: "SOUL.md"},
					Digest:       agentcontext.Digest("Be direct."),
				}},
			} {
				for _, toolName := range []string{"native", "chat"} {
					t.Run(string(mode)+"/"+runtimeName+"/"+source.name+"/"+toolName, func(t *testing.T) {
						checkSoulCreationRuntime(t, mode, kind, source.soul, toolName == "chat")
					})
				}
			}
		}
	}
}

func checkSoulCreationRuntime(t *testing.T, mode executionmode.Mode, kind corev1alpha1.AgentRuntimeType, soul *corev1alpha1.SoulSource, chat bool) {
	t.Helper()
	ctx := context.Background()
	parent := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "parent", Namespace: defaultNamespace, UID: "parent-uid"}}
	creates := 0
	c := newFakeClientWithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			creates++
			return assignFakeUIDOnCreate(ctx, c, obj, opts...)
		},
	}, parent)
	model := map[string]any{"provider": "openai", "name": "review-model"}
	if kind == corev1alpha1.AgentRuntimeOpencode {
		model = map[string]any{"name": "openai/gpt-5.4", "contextWindow": 32768, "maxTokens": 4096}
	}
	args := map[string]any{
		"role": "reviewer", nameField: "reviewer", modelField: model,
		systemPromptField: "Review the supplied change.",
	}
	if soul != nil {
		args["soul"] = soul
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
	allowed := soul == nil || kind == "" || mode == executionmode.HarnessV2
	if (callErr == nil) != allowed {
		t.Errorf("allowed=%v, error=%v", allowed, callErr)
	}
	if !allowed && (callErr == nil || !strings.Contains(callErr.Error(), "built-in harness v2")) {
		t.Errorf("expected runtime compatibility rejection, got %v", callErr)
	}
	agents := &corev1alpha1.AgentList{}
	if err := c.List(ctx, agents, client.InNamespace(defaultNamespace)); err != nil {
		t.Fatal(err)
	}
	tasks := &corev1alpha1.TaskList{}
	if err := c.List(ctx, tasks, client.InNamespace(defaultNamespace)); err != nil {
		t.Fatal(err)
	}
	if !allowed {
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
		t.Fatalf("supported creation did not persist expected resources: creates=%d, agents=%d, tasks=%d", creates, len(agents.Items), len(tasks.Items))
	}
	created := &agents.Items[0]
	if !reflect.DeepEqual(created.Spec.Soul, soul) {
		t.Fatal("creation changed the declared soul")
	}
	if kind != "" && (created.Spec.Runtime == nil || created.Spec.Runtime.ContractVersion == nil || *created.Spec.Runtime.ContractVersion != mode.ContractVersion()) {
		t.Fatal("creation did not preserve the installation-defaulted runtime contract")
	}
}

func soulCreationCallError(t *testing.T, chat bool, result string, callErr error) error {
	t.Helper()
	if !chat {
		return callErr
	}
	if callErr != nil {
		t.Fatal(callErr)
	}
	var response ChatToolResult
	if err := json.Unmarshal([]byte(result), &response); err != nil {
		t.Fatal(err)
	}
	if response.Success {
		return nil
	}
	if response.ErrorType != "invalid_arguments" {
		t.Fatalf("unexpected rejection type: %s", response.ErrorType)
	}
	return errors.New(response.Error)
}
