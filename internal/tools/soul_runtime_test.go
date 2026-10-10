package tools

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/agentcontext"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestUpdateAgentSoulRuntimeCompatibility(t *testing.T) {
	type testCase struct {
		name    string
		runtime *corev1alpha1.AgentCLIRuntime
		allowed bool
	}
	cases := make([]testCase, 0, 15)
	cases = append(cases,
		testCase{name: "ai", allowed: true},
		testCase{name: "external v2", runtime: &corev1alpha1.AgentCLIRuntime{
			RuntimeRef:      &corev1alpha1.AgentRuntimeReference{Name: "external"},
			ContractVersion: new(corev1alpha1.AgentRuntimeContractHarnessV2),
		}},
		testCase{name: "external v1", runtime: &corev1alpha1.AgentCLIRuntime{
			RuntimeRef:      &corev1alpha1.AgentRuntimeReference{Name: "external"},
			ContractVersion: new(corev1alpha1.AgentRuntimeContractHarnessV1),
		}},
	)
	for _, kind := range []corev1alpha1.AgentRuntimeType{
		corev1alpha1.AgentRuntimeCodex, corev1alpha1.AgentRuntimeClaude,
		corev1alpha1.AgentRuntimeCopilot, corev1alpha1.AgentRuntimeOpencode,
	} {
		for _, contract := range []*corev1alpha1.AgentRuntimeContractVersion{
			new(corev1alpha1.AgentRuntimeContractHarnessV2),
			new(corev1alpha1.AgentRuntimeContractHarnessV1), nil,
		} {
			version := "missing contract"
			allowed := false
			if contract != nil {
				version = string(*contract)
				allowed = *contract == corev1alpha1.AgentRuntimeContractHarnessV2
			}
			cases = append(cases, testCase{string(kind) + "/" + version, &corev1alpha1.AgentCLIRuntime{Type: kind, ContractVersion: contract}, allowed})
		}
	}
	for _, tc := range cases {
		for _, source := range []struct {
			name string
			soul *corev1alpha1.SoulSource
		}{
			{"inline", &corev1alpha1.SoulSource{Inline: "new persona"}},
			{"configmap", &corev1alpha1.SoulSource{
				ConfigMapRef: &corev1alpha1.ConfigMapKeySelector{Name: "persona", Key: "SOUL.md"},
				Digest:       agentcontext.Digest("new persona"),
			}},
		} {
			t.Run(tc.name+"/"+source.name, func(t *testing.T) {
				ctx := context.Background()
				agent := &corev1alpha1.Agent{
					ObjectMeta: metav1.ObjectMeta{Name: "reviewer", Namespace: defaultNamespace},
					Spec: corev1alpha1.AgentSpec{
						Runtime:      tc.runtime.DeepCopy(),
						Model:        &corev1alpha1.ModelConfig{Provider: "openai", Name: "review-model"},
						SystemPrompt: &corev1alpha1.PromptSource{Inline: "original role"},
					},
				}
				if tc.runtime != nil && tc.runtime.Type == corev1alpha1.AgentRuntimeOpencode {
					agent.Spec.Model = testOpenCodeModelConfig("openai/gpt-5.4")
				}
				if tc.runtime != nil && tc.runtime.RuntimeRef != nil {
					agent.Spec.Model = nil
					agent.Spec.SystemPrompt = nil
				}
				updates := 0
				c := newFakeClientWithInterceptorFuncs(interceptor.Funcs{
					Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
						updates++
						return c.Update(ctx, obj, opts...)
					},
				}, agent)
				before := &corev1alpha1.Agent{}
				if err := c.Get(ctx, client.ObjectKeyFromObject(agent), before); err != nil {
					t.Fatal(err)
				}
				args, err := json.Marshal(map[string]any{
					nameField: agent.Name, "soul": source.soul, systemPromptField: "updated role",
				})
				if err != nil {
					t.Fatal(err)
				}
				result, err := (&UpdateAgentTool{}).Execute(WithToolContext(ctx, &ToolContext{Client: c, Namespace: agent.Namespace}), args)
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
				if !tc.allowed {
					if response.Success || response.ErrorType != errTypeInvalidArgs || !strings.Contains(response.Error, "built-in harness v2") {
						t.Errorf("response = %#v, want runtime compatibility rejection", response)
					}
					if updates != 0 || !reflect.DeepEqual(before, after) {
						t.Errorf("rejected update reached persistence or changed stored Agent: updates=%d", updates)
					}
					return
				}
				if !response.Success || updates != 1 {
					t.Fatalf("response = %#v, updates=%d, want one successful update", response, updates)
				}
				if !reflect.DeepEqual(after.Spec.Soul, source.soul) || after.Spec.SystemPrompt == nil || after.Spec.SystemPrompt.Inline != "updated role" {
					t.Fatal("supported update did not persist the requested soul and role")
				}
			})
		}
	}
}

func TestSoulParameterSchemaDocumentsRuntimeCompatibility(t *testing.T) {
	var schema struct {
		Properties map[string]struct {
			Description string `json:"description"`
		} `json:"properties"`
	}
	if err := json.Unmarshal((&UpdateAgentTool{}).Parameters(), &schema); err != nil {
		t.Fatal(err)
	}
	description := schema.Properties["soul"].Description
	for _, constraint := range []string{
		"AI", "codex", "claude", "copilot", "opencode", "runtimeRef",
		string(corev1alpha1.AgentRuntimeContractHarnessV1), string(corev1alpha1.AgentRuntimeContractHarnessV2),
	} {
		if !strings.Contains(description, constraint) {
			t.Errorf("soul schema omits runtime compatibility constraint %q", constraint)
		}
	}
}
