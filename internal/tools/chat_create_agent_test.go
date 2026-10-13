/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/executionmode"
)

const testProviderOpenAI = "openai"

func TestChatCreateAgentTool_ParametersDescribeOpenCodeACP(t *testing.T) {
	params := string((&ChatCreateAgentTool{}).Parameters())
	for _, want := range []string{"copilot, claude, codex, or opencode", "OpenCode defaults to Read, Write, Edit, Bash, Glob, and Grep", "OpenCode defaults to true", "Omit for OpenCode runtime Agents"} {
		if !strings.Contains(params, want) {
			t.Errorf("Parameters() missing %q", want)
		}
	}
}

func TestChatCreateAgentTool_ParametersRuntimeTurnBounds(t *testing.T) {
	var schema struct {
		Properties struct {
			Runtime struct {
				Properties struct {
					DefaultMaxTurns struct {
						Minimum int `json:"minimum"`
						Maximum int `json:"maximum"`
					} `json:"defaultMaxTurns"`
				} `json:"properties"`
			} `json:"runtime"`
		} `json:"properties"`
	}
	if err := json.Unmarshal((&ChatCreateAgentTool{}).Parameters(), &schema); err != nil {
		t.Fatal(err)
	}
	bounds := schema.Properties.Runtime.Properties.DefaultMaxTurns
	if bounds.Minimum != 1 || bounds.Maximum != 1000 {
		t.Fatalf("runtime.defaultMaxTurns bounds = %+v, want 1..1000", bounds)
	}
}

// Exercise direct execution and the registry's numeric-string normalization.
// The fake client does not enforce CRD validation, so rejected values must not
// reach creation (including the optional initial Task).
func TestChatCreateAgentTool_Execute_NumericBounds(t *testing.T) {
	for _, field := range []string{"runtime.defaultMaxTurns", "model.temperature"} {
		cases := []struct {
			name  string
			value string
			want  float64
			valid bool
		}{
			{name: "zero", value: "0", want: 0, valid: field == "model.temperature"},
			{name: "negative", value: "-1"},
			{name: "fractional", value: "1.5", want: 1.5, valid: field == "model.temperature"},
		}
		if field == "runtime.defaultMaxTurns" {
			cases = append(cases, []struct {
				name  string
				value string
				want  float64
				valid bool
			}{
				{name: "minimum", value: "1", want: 1, valid: true},
				{name: "maximum", value: "1000", want: 1000, valid: true},
				{name: "ordinary control", value: "15", want: 15, valid: true},
				{name: "integral decimal", value: "15.0", want: 15, valid: true},
				{name: "minimum exponent", value: "1e0", want: 1, valid: true},
				{name: "maximum exponent", value: "1e3", want: 1000, valid: true},
				{name: "maximum plus one", value: "1001"},
				{name: "int32 maximum", value: "2147483647"},
				{name: "int32 overflow", value: "2147483648"},
				{name: "uint32 plus one", value: "4294967297"},
				{name: "overflow exponent", value: "4.294967297e9"},
				{name: "negative overflow", value: "-4294967295"},
				{name: "fraction rounded to one", value: "1.0000000000000001"},
				{name: "fraction below minimum", value: "0.99999999999999999"},
			}...)
		} else {
			cases = append(cases, []struct {
				name  string
				value string
				want  float64
				valid bool
			}{
				{name: "maximum", value: "2", want: 2, valid: true},
				{name: "ordinary control", value: "0.7", want: 0.7, valid: true},
				{name: "maximum exponent", value: "2e0", want: 2, valid: true},
				{name: "scaled maximum exponent", value: "20E-1", want: 2, valid: true},
				{name: "negative zero", value: "-0", want: 0, valid: true},
				{name: "zero exponent", value: "0e50", want: 0, valid: true},
				{name: "underflow control", value: "1e-1000", want: 0, valid: true},
				{name: "huge negative exponent", value: "1e-999999999999999999999", want: 0, valid: true},
				{name: "zero huge exponent", value: "0e999999999999999999999", want: 0, valid: true},
				{name: "inside maximum", value: "1.9999999999999999", want: 2, valid: true},
				{name: "above maximum", value: "2.1"},
				{name: "rounded maximum", value: "2.0000000000000001"},
				{name: "rounded maximum exponent", value: "2.0000000000000001e0"},
				{name: "scaled rounded maximum exponent", value: "20000000000000001e-16"},
				{name: "negative underflow", value: "-1e-1000"},
				{name: "negative huge exponent", value: "-1e-999999999999999999999"},
			}...)
		}
		for _, mode := range []string{"direct", "registry"} {
			for _, quoted := range []bool{false, true} {
				for _, tc := range cases {
					t.Run(fmt.Sprintf("%s/%s/string=%t/%s", field, mode, quoted, tc.name), func(t *testing.T) {
						value := tc.value
						if quoted {
							value = fmt.Sprintf("%q", value)
						}
						model, runtime := `{"name":"test-model"}`, `{"type":"claude"}`
						if field == "model.temperature" {
							model = `{"name":"test-model","temperature":` + value + `}`
						} else {
							runtime = `{"type":"claude","defaultMaxTurns":` + value + `}`
						}
						initialPrompt := ""
						if !tc.valid {
							initialPrompt = `,"initialPrompt":"must not start"`
						}
						fc := newFakeClient()
						ctx := WithToolContext(context.Background(), &ToolContext{
							Client: fc, Namespace: defaultNamespace, ExecutionMode: executionmode.HarnessV2,
							CheckTaskLimit:   func() *ChatToolError { return nil },
							GenerateTaskName: func() string { return "initial-task" },
							TaskLabels:       func() map[string]string { return nil },
							IncrementTasks:   func() {},
						})
						tool := &ChatCreateAgentTool{}
						execute := tool.Execute
						if mode == "registry" {
							registry := NewRegistry()
							registry.Register(tool)
							execute = func(ctx context.Context, args json.RawMessage) (string, error) {
								result, err := registry.Execute(ctx, tool.Name(), args)
								if argErr, ok := errors.AsType[*ToolArgumentError](err); ok {
									return ChatToolErrorResult(errTypeInvalidArgs, argErr.Error(), "Use the declared argument types")
								}
								return result, err
							}
						}
						result, err := execute(ctx, json.RawMessage(`{"name":"bounded-agent","model":`+model+`,"runtime":`+runtime+initialPrompt+`}`))
						if err != nil {
							t.Fatalf("unexpected error: %v", err)
						}
						var response ChatToolResult
						if err := json.Unmarshal([]byte(result), &response); err != nil {
							t.Fatal(err)
						}
						var agents corev1alpha1.AgentList
						if err := fc.List(ctx, &agents); err != nil {
							t.Fatal(err)
						}
						if !tc.valid {
							if response.Success || response.ErrorType != errTypeInvalidArgs || !strings.Contains(response.Error, field) {
								t.Errorf("result = %#v, want rejection naming %s", response, field)
							}
							if len(agents.Items) != 0 {
								t.Errorf("rejected value created %d Agents", len(agents.Items))
							}
							var tasks corev1alpha1.TaskList
							if err := fc.List(ctx, &tasks); err != nil {
								t.Fatal(err)
							}
							if len(tasks.Items) != 0 {
								t.Errorf("rejected value created %d Tasks", len(tasks.Items))
							}
							return
						}
						if !response.Success || len(agents.Items) != 1 {
							t.Fatalf("result = %#v, agents = %d, want success", response, len(agents.Items))
						}
						agent := agents.Items[0]
						if field == "model.temperature" {
							if agent.Spec.Model == nil || agent.Spec.Model.Temperature == nil || *agent.Spec.Model.Temperature != tc.want {
								t.Fatalf("model = %#v, want temperature %v", agent.Spec.Model, tc.want)
							}
						} else if agent.Spec.Runtime == nil || agent.Spec.Runtime.DefaultMaxTurns == nil || float64(*agent.Spec.Runtime.DefaultMaxTurns) != tc.want {
							t.Fatalf("runtime = %#v, want defaultMaxTurns %v", agent.Spec.Runtime, tc.want)
						}
					})
				}
			}
		}
	}
}

func TestDecodeChatCreateAgentArgs_PreservesOtherParsing(t *testing.T) {
	for _, args := range []string{
		`null`,
		`{}`,
		`{"model":"openai/test","runtime":null}`,
		`{"model":null,"runtime":{"type":"claude","defaultMaxTurns":null}}`,
		`{"model":{"name":"test","temperature":null,"contextWindow":32768,"maxTokens":4096},"runtime":{"defaultMaxTurns":10,"defaultAllowedTools":["Read"],"defaultAllowBash":false},"coordination":{"enabled":true,"maxDepth":2,"allowedAgents":[{"name":"coder","namespace":null}]},"resources":{"requests":{"cpu":"100m"}}}`,
		`{"model":{"temperature":"0.7"},"runtime":{"defaultMaxTurns":"15"}}`,
		`{"model":{"temperature":0.5,"temperature":0.7},"runtime":{"defaultMaxTurns":1,"defaultMaxTurns":15}}`,
		`{"model":{"temperature":0.7},"model":"openai/test"}`,
		`{"unknown":{"number":1.5},"runtime":[1,2],"model":false}`,
		`{"model":{"temperature":0.7}} trailing`,
		`{"model":{"temperature":1e1000}}`,
	} {
		t.Run(args, func(t *testing.T) {
			var legacy map[string]any
			legacyErr := json.Unmarshal([]byte(args), &legacy)
			got, err := decodeChatCreateAgentArgs(json.RawMessage(args))
			if legacyErr != nil {
				if err == nil || err.Error() != legacyErr.Error() {
					t.Fatalf("error = %v, want original error %v", err, legacyErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, field := range [][2]string{{modelField, "temperature"}, {runtimeField, "defaultMaxTurns"}} {
				object, ok := got[field[0]].(map[string]any)
				if !ok {
					continue
				}
				if number, ok := object[field[1]].(json.Number); ok {
					object[field[1]], err = number.Float64()
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			if !reflect.DeepEqual(got, legacy) {
				t.Fatalf("arguments = %#v, want unchanged decoding %#v", got, legacy)
			}
		})
	}
}

func TestChatCreateAgentTool_Execute_OmittedNumericFields(t *testing.T) {
	for _, value := range []string{"", ",\"temperature\":null"} {
		fc := newFakeClient()
		ctx := WithToolContext(context.Background(), &ToolContext{Client: fc, Namespace: defaultNamespace, ExecutionMode: executionmode.HarnessV2})
		turns := ""
		if value != "" {
			turns = `,"defaultMaxTurns":null`
		}
		result, err := (&ChatCreateAgentTool{}).Execute(ctx, json.RawMessage(`{"name":"optional-numbers","model":{"name":"test-model"`+value+`},"runtime":{"type":"claude"`+turns+`}}`))
		if err != nil {
			t.Fatal(err)
		}
		var response ChatToolResult
		if err := json.Unmarshal([]byte(result), &response); err != nil || !response.Success {
			t.Fatalf("result = %s, error = %v, want success", result, err)
		}
		var agent corev1alpha1.Agent
		if err := fc.Get(ctx, client.ObjectKey{Name: "optional-numbers", Namespace: defaultNamespace}, &agent); err != nil {
			t.Fatal(err)
		}
		if agent.Spec.Model.Temperature != nil || agent.Spec.Runtime.DefaultMaxTurns != nil {
			t.Fatalf("omitted/null numbers must stay omitted: model=%#v, runtime=%#v", agent.Spec.Model, agent.Spec.Runtime)
		}
	}
}

func TestChatCreateAgentTool_ParametersRequireNonblankAllowedAgentNames(t *testing.T) {
	assertCreateAgentAllowedAgentNameSchema(t, (&ChatCreateAgentTool{}).Parameters())
}

func TestChatCreateAgentTool_Execute_RejectsMissingProviderRef(t *testing.T) {
	fc := newFakeClient(&corev1alpha1.Provider{ObjectMeta: metav1.ObjectMeta{Name: testProviderOpenAI, Namespace: defaultNamespace}})
	ctx := WithToolContext(context.Background(), &ToolContext{
		Client:        fc,
		Namespace:     defaultNamespace,
		ExecutionMode: executionmode.HarnessV2,
	})

	result, err := (&ChatCreateAgentTool{}).Execute(ctx, json.RawMessage(`{"name":"planner","providerRef":"copilot"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var r ChatToolResult
	if err := json.Unmarshal([]byte(result), &r); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if r.Success || r.ErrorType != errTypeInvalidArgs || !strings.Contains(r.Error, `"copilot"`) {
		t.Fatalf("result = %#v, want invalid arguments naming the missing Provider", r)
	}
	if strings.Contains(r.Error+r.Suggestion, testProviderOpenAI) {
		t.Fatalf("result = %#v, must not enumerate other Providers", r)
	}
	err = fc.Get(context.Background(), client.ObjectKey{Name: "planner", Namespace: defaultNamespace}, &corev1alpha1.Agent{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("Agent lookup error = %v, want NotFound", err)
	}
}

func TestChatCreateAgentTool_Execute_AuthorizesBeforeProviderLookup(t *testing.T) {
	ctx := WithToolContext(context.Background(), &ToolContext{
		Client:        newFakeClient(),
		Namespace:     defaultNamespace,
		ExecutionMode: executionmode.HarnessV2,
		AuthorizeAgentCreate: func(context.Context, *corev1alpha1.Agent) *ChatToolError {
			return &ChatToolError{Type: "permission_denied", Message: "provider not permitted"}
		},
	})

	result, err := (&ChatCreateAgentTool{}).Execute(ctx, json.RawMessage(`{"name":"planner","providerRef":"copilot"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var r ChatToolResult
	if err := json.Unmarshal([]byte(result), &r); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if r.Success || r.ErrorType != "permission_denied" {
		t.Fatalf("result = %#v, want the authorization error, not a Provider existence result", r)
	}
}

func TestChatCreateAgentTool_Execute_OmittedProviderRefLeavesNil(t *testing.T) {
	fc := newFakeClient()
	ctx := WithToolContext(context.Background(), &ToolContext{
		Client:        fc,
		Namespace:     defaultNamespace,
		ExecutionMode: executionmode.HarnessV2,
	})

	tool := &ChatCreateAgentTool{}
	result, err := tool.Execute(ctx, json.RawMessage(`{"name":"agent-no-provider","model":{"provider":"openai","name":"gpt-4.1-mini"}}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var r ChatToolResult
	if err := json.Unmarshal([]byte(result), &r); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if !r.Success {
		t.Fatalf("expected success, got error: %s", r.Error)
	}

	var created corev1alpha1.Agent
	if err := fc.Get(context.Background(), client.ObjectKey{
		Name:      "agent-no-provider",
		Namespace: defaultNamespace,
	}, &created); err != nil {
		t.Fatalf("failed to get created agent: %v", err)
	}

	if created.Spec.ProviderRef != nil {
		t.Fatalf("providerRef = %#v, want nil when providerRef argument is omitted", created.Spec.ProviderRef)
	}
	if created.Spec.Model == nil {
		t.Fatal("model is nil")
	}
	if created.Spec.Model.Provider != testProviderOpenAI {
		t.Fatalf("model.provider = %q, want openai when no providerRef is set", created.Spec.Model.Provider)
	}
}

func TestChatCreateAgentTool_Execute_RejectsOpenCodeSystemPrompt(t *testing.T) {
	fc := newFakeClient()
	ctx := WithToolContext(context.Background(), &ToolContext{Client: fc, Namespace: defaultNamespace, ExecutionMode: executionmode.HarnessV2})
	result, err := (&ChatCreateAgentTool{}).Execute(ctx, json.RawMessage(`{
		"name":"opencode-prompt-agent",
		"systemPrompt":"You write code",
		"model":{"name":"openai/gpt-5.4","contextWindow":32768,"maxTokens":4096},
		"runtime":{"type":"opencode"}
	}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var response ChatToolResult
	if err := json.Unmarshal([]byte(result), &response); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if response.Success || response.ErrorType != errTypeInvalidArgs || !strings.Contains(response.Error, "does not support systemPrompt") {
		t.Fatalf("response = %#v, want OpenCode systemPrompt rejection", response)
	}
	var created corev1alpha1.Agent
	if err := fc.Get(context.Background(), client.ObjectKey{Name: "opencode-prompt-agent", Namespace: defaultNamespace}, &created); !apierrors.IsNotFound(err) {
		t.Fatalf("invalid OpenCode Agent should not be created, get err=%v", err)
	}
}

func TestChatCreateAgentTool_Execute_RejectsUnsupportedOpenCodeTemperature(t *testing.T) {
	fc := newFakeClient()
	ctx := WithToolContext(context.Background(), &ToolContext{Client: fc, Namespace: defaultNamespace, ExecutionMode: executionmode.HarnessV2})
	result, err := (&ChatCreateAgentTool{}).Execute(ctx, json.RawMessage(`{
		"name":"opencode-temperature-agent",
		"model":{"name":"openai/gpt-5.4","temperature":1,"contextWindow":32768,"maxTokens":4096},
		"runtime":{"type":"opencode"}
	}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var response ChatToolResult
	if err := json.Unmarshal([]byte(result), &response); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if response.Success || response.ErrorType != errTypeInvalidArgs || !strings.Contains(response.Error, "temperature") {
		t.Fatalf("response = %#v, want unsupported OpenCode temperature rejection", response)
	}
	var created corev1alpha1.Agent
	if err := fc.Get(context.Background(), client.ObjectKey{Name: "opencode-temperature-agent", Namespace: defaultNamespace}, &created); !apierrors.IsNotFound(err) {
		t.Fatalf("invalid OpenCode Agent should not be created, get err=%v", err)
	}
}

func TestChatCreateAgentTool_Execute_AcceptsLegacyOpenCodeTemperature(t *testing.T) {
	fc := newFakeClient()
	ctx := WithToolContext(context.Background(), &ToolContext{Client: fc, Namespace: defaultNamespace, ExecutionMode: executionmode.HarnessV2})
	result, err := (&ChatCreateAgentTool{}).Execute(ctx, json.RawMessage(`{
		"name":"opencode-legacy-temperature-agent",
		"model":{"name":"openai/gpt-5.4","temperature":0.7,"contextWindow":32768,"maxTokens":4096},
		"runtime":{"type":"opencode"}
	}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var response ChatToolResult
	if err := json.Unmarshal([]byte(result), &response); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if !response.Success {
		t.Fatalf("expected success, got error: %s", response.Error)
	}
	var created corev1alpha1.Agent
	if err := fc.Get(context.Background(), client.ObjectKey{Name: "opencode-legacy-temperature-agent", Namespace: defaultNamespace}, &created); err != nil {
		t.Fatalf("failed to get created Agent: %v", err)
	}
	if created.Spec.Model == nil || created.Spec.Model.Temperature == nil || *created.Spec.Model.Temperature != 0.7 {
		t.Fatalf("model.temperature = %#v, want legacy value 0.7", created.Spec.Model)
	}
}

func TestChatCreateAgentTool_Execute_RejectsFractionalOpenCodeModelLimits(t *testing.T) {
	for _, tc := range []struct {
		name      string
		agentName string
		args      json.RawMessage
		wantField string
	}{
		{
			name:      "context window",
			agentName: "fractional-context-window",
			args: json.RawMessage(`{
				"name":"fractional-context-window",
				"model":{"name":"openai/gpt-5.4","contextWindow":32768.5,"maxTokens":4096},
				"runtime":{"type":"opencode"}
			}`),
			wantField: "model.contextWindow",
		},
		{
			name:      "max tokens",
			agentName: "fractional-max-tokens",
			args: json.RawMessage(`{
				"name":"fractional-max-tokens",
				"model":{"name":"openai/gpt-5.4","contextWindow":32768,"maxTokens":4096.5},
				"runtime":{"type":"opencode"}
			}`),
			wantField: "model.maxTokens",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fc := newFakeClient()
			ctx := WithToolContext(context.Background(), &ToolContext{Client: fc, Namespace: defaultNamespace, ExecutionMode: executionmode.HarnessV2})
			result, err := (&ChatCreateAgentTool{}).Execute(ctx, tc.args)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			var response ChatToolResult
			if err := json.Unmarshal([]byte(result), &response); err != nil {
				t.Fatalf("failed to parse result: %v", err)
			}
			if response.Success || response.ErrorType != errTypeInvalidArgs || !strings.Contains(response.Error, tc.wantField) {
				t.Fatalf("response = %#v, want %s integer rejection", response, tc.wantField)
			}
			var created corev1alpha1.Agent
			if err := fc.Get(context.Background(), client.ObjectKey{Name: tc.agentName, Namespace: defaultNamespace}, &created); !apierrors.IsNotFound(err) {
				t.Fatalf("invalid OpenCode Agent should not be created, get err=%v", err)
			}
		})
	}
}

func TestParsePositiveAgentModelInt32(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  any
		want int32
	}{
		{name: "decoded JSON integer", raw: float64(4096), want: 4096},
		{name: "max int32", raw: float64(1<<31 - 1), want: 1<<31 - 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parsePositiveAgentModelInt32("model.contextWindow", tc.raw)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %d, want %d", got, tc.want)
			}
		})
	}

	for _, tc := range []struct {
		name      string
		raw       any
		wantError string
	}{
		{name: "fractional decoded JSON number", raw: float64(4096.5), wantError: "must be an integer"},
		{name: "zero", raw: float64(0), wantError: "positive 32-bit integer"},
		{name: "negative", raw: float64(-1), wantError: "positive 32-bit integer"},
		{name: "overflow", raw: float64(1 << 31), wantError: "positive 32-bit integer"},
		{name: "wrong type", raw: "4096", wantError: "must be an integer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parsePositiveAgentModelInt32("model.contextWindow", tc.raw)
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("error = %v, want %q", err, tc.wantError)
			}
		})
	}
}

func TestChatCreateAgentTool_Execute_RejectsOpenCodeLegacySecret(t *testing.T) {
	fc := newFakeClient()
	ctx := WithToolContext(context.Background(), &ToolContext{Client: fc, Namespace: defaultNamespace, ExecutionMode: executionmode.HarnessV2})
	result, err := (&ChatCreateAgentTool{}).Execute(ctx, json.RawMessage(`{
		"name": "legacy-runtime-agent",
		"model": "openai/gpt-5.4",
		"runtime": {"type": "opencode", "secretRef": "legacy-runtime"}
	}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var response ChatToolResult
	if err := json.Unmarshal([]byte(result), &response); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if response.Success || response.ErrorType != errTypeInvalidArgs || !strings.Contains(response.Error, "does not accept runtime.secretRef") {
		t.Fatalf("response = %#v, want legacy secret rejection", response)
	}
	var created corev1alpha1.Agent
	if err := fc.Get(context.Background(), client.ObjectKey{Name: "legacy-runtime-agent", Namespace: defaultNamespace}, &created); !apierrors.IsNotFound(err) {
		t.Fatalf("invalid OpenCode Agent should not be created, get err=%v", err)
	}
}

func TestChatCreateAgentTool_Execute_UsesBuiltInOpenCode(t *testing.T) {
	fc := newFakeClient()
	ctx := WithToolContext(context.Background(), &ToolContext{Client: fc, Namespace: defaultNamespace, ExecutionMode: executionmode.HarnessV2})
	result, err := (&ChatCreateAgentTool{}).Execute(ctx, json.RawMessage(`{
		"name":"opencode-agent",
		"model":{"name":"moonshotai/Kimi-K2-Instruct-0905","contextWindow":32768,"maxTokens":4096},
		"runtime":{"type":"opencode"}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	var response ChatToolResult
	if err := json.Unmarshal([]byte(result), &response); err != nil {
		t.Fatal(err)
	}
	if !response.Success {
		t.Fatalf("response = %#v, want success", response)
	}
	var agent corev1alpha1.Agent
	if err := fc.Get(context.Background(), client.ObjectKey{Name: "opencode-agent", Namespace: defaultNamespace}, &agent); err != nil {
		t.Fatal(err)
	}
	wantTools := []string{"Read", "Write", "Edit", "Bash", "Glob", "Grep"}
	if agent.Spec.Runtime == nil || agent.Spec.Runtime.Type != corev1alpha1.AgentRuntimeOpencode || !slices.Equal(agent.Spec.Runtime.DefaultAllowedTools, wantTools) {
		t.Fatalf("runtime = %#v, want OpenCode defaults %#v", agent.Spec.Runtime, wantTools)
	}
	if got := agent.BuiltInContractVersion(); got != corev1alpha1.AgentRuntimeContractHarnessV2 {
		t.Fatalf("contractVersion = %q, want %q", got, corev1alpha1.AgentRuntimeContractHarnessV2)
	}
	if agent.Spec.Model == nil || agent.Spec.Model.Name != "moonshotai/Kimi-K2-Instruct-0905" || agent.Spec.Model.Provider != "" {
		t.Fatalf("model = %#v, want provider-qualified OpenCode model", agent.Spec.Model)
	}
	if agent.Spec.SystemPrompt != nil {
		t.Fatalf("systemPrompt = %#v, want nil for OpenCode runtime", agent.Spec.SystemPrompt)
	}
}

func TestChatCreateAgentTool_Execute_DefaultsHarnessV1Contract(t *testing.T) {
	fc := newFakeClient()
	ctx := WithToolContext(context.Background(), &ToolContext{
		Client:        fc,
		Namespace:     defaultNamespace,
		ExecutionMode: executionmode.HarnessV1,
	})
	result, err := (&ChatCreateAgentTool{}).Execute(ctx, json.RawMessage(`{
		"name":"v1-runtime-agent",
		"runtime":{"type":"codex"}
	}`))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	var response ChatToolResult
	if err := json.Unmarshal([]byte(result), &response); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if !response.Success {
		t.Fatalf("response = %#v, want success", response)
	}
	created := &corev1alpha1.Agent{}
	if err := fc.Get(context.Background(), client.ObjectKey{Name: "v1-runtime-agent", Namespace: defaultNamespace}, created); err != nil {
		t.Fatalf("failed to get Agent: %v", err)
	}
	if got := created.BuiltInContractVersion(); got != corev1alpha1.AgentRuntimeContractHarnessV1 {
		t.Fatalf("contractVersion = %q, want %q", got, corev1alpha1.AgentRuntimeContractHarnessV1)
	}
}

func TestChatCreateAgentTool_Execute_RollsBackAgentWhenInitialTaskAuthorizationFails(t *testing.T) {
	fc := newFakeClient()
	ctx := WithToolContext(context.Background(), &ToolContext{
		Client:        fc,
		Namespace:     defaultNamespace,
		ExecutionMode: executionmode.HarnessV2,
		TaskLabels:    func() map[string]string { return map[string]string{} },
		CheckTaskLimit: func() *ChatToolError {
			return nil
		},
		GenerateTaskName: func() string { return "blocked-task" },
		AuthorizeTaskCreate: func(context.Context, *corev1alpha1.Task) *ChatToolError {
			return &ChatToolError{Type: "permission_denied", Message: "task blocked by context token"}
		},
	})

	tool := &ChatCreateAgentTool{}
	result, err := tool.Execute(ctx, json.RawMessage(`{"name":"agent-rollback","initialPrompt":"run this"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var r ChatToolResult
	if err := json.Unmarshal([]byte(result), &r); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if r.Success {
		t.Fatalf("expected authorization failure, got success: %#v", r)
	}
	if r.ErrorType != "permission_denied" {
		t.Fatalf("errorType = %q, want permission_denied", r.ErrorType)
	}

	var created corev1alpha1.Agent
	err = fc.Get(context.Background(), client.ObjectKey{Name: "agent-rollback", Namespace: defaultNamespace}, &created)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("agent should have been rolled back, get err=%v", err)
	}
}

func TestChatCreateAgentTool_Execute_AuthorizesAgentBeforeCreate(t *testing.T) {
	fc := newFakeClient()
	ctx := WithToolContext(context.Background(), &ToolContext{
		Client:        fc,
		Namespace:     defaultNamespace,
		ExecutionMode: executionmode.HarnessV2,
		AuthorizeAgentCreate: func(context.Context, *corev1alpha1.Agent) *ChatToolError {
			return &ChatToolError{Type: "authorization_failed", Message: "agent blocked by context token"}
		},
	})

	tool := &ChatCreateAgentTool{}
	result, err := tool.Execute(ctx, json.RawMessage(`{"name":"agent-blocked"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var r ChatToolResult
	if err := json.Unmarshal([]byte(result), &r); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if r.Success {
		t.Fatalf("expected authorization failure, got success: %#v", r)
	}

	var created corev1alpha1.Agent
	err = fc.Get(context.Background(), client.ObjectKey{Name: "agent-blocked", Namespace: defaultNamespace}, &created)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("agent should not have been created, get err=%v", err)
	}
}
func TestParseRuntimeConfig_BuiltInRuntimesAreCredentialFree(t *testing.T) {
	for _, runtimeType := range []corev1alpha1.AgentRuntimeType{
		corev1alpha1.AgentRuntimeCopilot,
		corev1alpha1.AgentRuntimeClaude,
		corev1alpha1.AgentRuntimeCodex,
	} {
		t.Run(string(runtimeType), func(t *testing.T) {
			agent := &corev1alpha1.Agent{Spec: corev1alpha1.AgentSpec{
				Model:       &corev1alpha1.ModelConfig{Name: "test-model"},
				ProviderRef: &corev1alpha1.ProviderReference{Name: testProviderOpenAI},
				SecretRef:   &corev1.LocalObjectReference{Name: "legacy-runtime"},
			}}
			args := map[string]any{runtimeField: map[string]any{
				jsonSchemaTypeField: string(runtimeType),
				secretRefField:      "missing-legacy-runtime",
			}}
			if errResult, ok := parseRuntimeConfig(args, agent, executionmode.HarnessV2); !ok {
				t.Fatalf("parseRuntimeConfig returned error: %s", errResult)
			}
			if agent.Spec.Runtime == nil || agent.Spec.Runtime.Type != runtimeType {
				t.Fatalf("runtime = %#v, want %q", agent.Spec.Runtime, runtimeType)
			}
			if agent.Spec.ProviderRef != nil || agent.Spec.SecretRef != nil {
				t.Fatalf("providerRef=%#v secretRef=%#v, want credential-free runtime", agent.Spec.ProviderRef, agent.Spec.SecretRef)
			}
		})
	}
	t.Run("opencode", func(t *testing.T) {
		agent := &corev1alpha1.Agent{Spec: corev1alpha1.AgentSpec{
			Model:       testOpenCodeModelConfig("openai/gpt-5.4"),
			ProviderRef: &corev1alpha1.ProviderReference{Name: testProviderOpenAI},
			SecretRef:   &corev1.LocalObjectReference{Name: "legacy-runtime"},
		}}
		args := map[string]any{runtimeField: map[string]any{jsonSchemaTypeField: "opencode"}}
		if errResult, ok := parseRuntimeConfig(args, agent, executionmode.HarnessV2); !ok {
			t.Fatalf("parseRuntimeConfig returned error: %s", errResult)
		}
		wantTools := []string{"Read", "Write", "Edit", "Bash", "Glob", "Grep"}
		if agent.Spec.Runtime == nil || agent.Spec.Runtime.Type != corev1alpha1.AgentRuntimeOpencode || !slices.Equal(agent.Spec.Runtime.DefaultAllowedTools, wantTools) {
			t.Fatalf("runtime = %#v, want OpenCode defaults %#v", agent.Spec.Runtime, wantTools)
		}
		if agent.Spec.ProviderRef != nil || agent.Spec.SecretRef != nil || agent.Spec.Model.Provider != "" {
			t.Fatalf("providerRef=%#v secretRef=%#v model=%#v, want credential-free OpenCode runtime", agent.Spec.ProviderRef, agent.Spec.SecretRef, agent.Spec.Model)
		}
	})
	t.Run("normalizes runtime type", func(t *testing.T) {
		agent := &corev1alpha1.Agent{Spec: corev1alpha1.AgentSpec{Model: &corev1alpha1.ModelConfig{Name: "test-model"}}}
		args := map[string]any{runtimeField: map[string]any{jsonSchemaTypeField: "  claude  "}}
		if errResult, ok := parseRuntimeConfig(args, agent, executionmode.HarnessV2); !ok {
			t.Fatalf("parseRuntimeConfig returned error: %s", errResult)
		}
		if agent.Spec.Runtime == nil || agent.Spec.Runtime.Type != corev1alpha1.AgentRuntimeClaude {
			t.Fatalf("runtime = %#v, want normalized claude", agent.Spec.Runtime)
		}
	})
}

func TestParseRuntimeConfig_RejectsModelLimitsForNonOpenCodeBuiltIns(t *testing.T) {
	contextWindow := int32(32768)
	maxTokens := int32(4096)
	for _, tt := range []struct {
		name      string
		runtime   corev1alpha1.AgentRuntimeType
		model     *corev1alpha1.ModelConfig
		wantError string
	}{
		{name: "codex context window", runtime: corev1alpha1.AgentRuntimeCodex, model: &corev1alpha1.ModelConfig{ContextWindow: &contextWindow}, wantError: "contextWindow"},
		{name: "claude max tokens", runtime: corev1alpha1.AgentRuntimeClaude, model: &corev1alpha1.ModelConfig{MaxTokens: &maxTokens}, wantError: "maxTokens"},
		{name: "copilot context window", runtime: corev1alpha1.AgentRuntimeCopilot, model: &corev1alpha1.ModelConfig{ContextWindow: &contextWindow}, wantError: "contextWindow"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			agent := &corev1alpha1.Agent{Spec: corev1alpha1.AgentSpec{Model: tt.model}}
			result, ok := parseRuntimeConfig(map[string]any{runtimeField: map[string]any{jsonSchemaTypeField: string(tt.runtime)}}, agent, executionmode.HarnessV2)
			if ok {
				t.Fatal("parseRuntimeConfig accepted ignored built-in model limits")
			}
			var response ChatToolResult
			if err := json.Unmarshal([]byte(result), &response); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(response.Error, tt.wantError) {
				t.Fatalf("response = %#v, want %q rejection", response, tt.wantError)
			}
		})
	}
}

func TestParseRuntimeConfig_RejectsUnsupportedRuntime(t *testing.T) {
	for _, runtimeType := range []string{"unknown", "", "   "} {
		t.Run(runtimeType, func(t *testing.T) {
			agent := &corev1alpha1.Agent{}
			args := map[string]any{runtimeField: map[string]any{jsonSchemaTypeField: runtimeType}}
			errResult, ok := parseRuntimeConfig(args, agent, executionmode.HarnessV2)
			if ok {
				t.Fatal("parseRuntimeConfig accepted invalid runtime")
			}
			var response ChatToolResult
			if err := json.Unmarshal([]byte(errResult), &response); err != nil {
				t.Fatalf("unmarshal error result: %v", err)
			}
			if response.ErrorType != errTypeInvalidArgs || (!strings.Contains(response.Error, "unsupported runtime type") && !strings.Contains(response.Error, "runtime.type is required")) {
				t.Fatalf("response = %#v, want invalid runtime rejection", response)
			}
		})
	}
}

func TestParseRuntimeConfig_AppliesRuntimeDefaults(t *testing.T) {
	agent := &corev1alpha1.Agent{
		Spec: corev1alpha1.AgentSpec{
			Model:       &corev1alpha1.ModelConfig{Name: "test-model"},
			ProviderRef: &corev1alpha1.ProviderReference{Name: testProviderOpenAI},
		},
	}

	args := map[string]any{runtimeField: map[string]any{jsonSchemaTypeField: runtimeTypeClaude, "defaultMaxTurns": float64(15),
		"defaultAllowedTools": []any{"Read", "Write", "Bash"},
		"defaultAllowBash":    false,
	},
	}

	if errResult, ok := parseRuntimeConfig(args, agent, executionmode.HarnessV2); !ok {
		t.Fatalf("parseRuntimeConfig returned error: %s", errResult)
	}

	if agent.Spec.Runtime == nil {
		t.Fatal("agent.Spec.Runtime is nil")
	}
	if agent.Spec.Runtime.DefaultMaxTurns == nil || *agent.Spec.Runtime.DefaultMaxTurns != 15 {
		t.Fatalf("defaultMaxTurns = %v, want 15", agent.Spec.Runtime.DefaultMaxTurns)
	}
	if got := agent.Spec.Runtime.DefaultAllowedTools; len(got) != 3 || got[0] != "Read" || got[1] != "Write" || got[2] != "Bash" {
		t.Fatalf("defaultAllowedTools = %#v, want Read/Write/Bash", got)
	}
	if agent.Spec.Runtime.DefaultAllowBash == nil || *agent.Spec.Runtime.DefaultAllowBash {
		t.Fatalf("defaultAllowBash = %v, want false", agent.Spec.Runtime.DefaultAllowBash)
	}
}
func TestParseCoordinationConfig_EnabledClearsRuntimeAndSecretRef(t *testing.T) {
	agent := &corev1alpha1.Agent{
		Spec: corev1alpha1.AgentSpec{
			Runtime:   &corev1alpha1.AgentCLIRuntime{Type: runtimeTypeClaude},
			SecretRef: &corev1.LocalObjectReference{Name: testRuntimeCredsSecretName},
		},
	}

	args := map[string]any{
		"coordination": map[string]any{
			enabledString: true,
		},
	}

	if _, ok := parseCoordinationConfig(args, agent); !ok {
		t.Fatal("parseCoordinationConfig() rejected valid coordination")
	}

	if agent.Spec.Coordination == nil {
		t.Fatal("agent.Spec.Coordination is nil")
	}
	if !agent.Spec.Coordination.Enabled {
		t.Fatal("coordination.enabled = false, want true")
	}
	if agent.Spec.Runtime != nil {
		t.Errorf("runtime = %v, want nil", agent.Spec.Runtime)
	}
	if agent.Spec.SecretRef != nil {
		t.Errorf("secretRef = %v, want nil", agent.Spec.SecretRef)
	}
}

func TestParseRuntimeConfig_PreservesExplicitEmptyOpenCodeTools(t *testing.T) {
	allowBash := false
	agent := &corev1alpha1.Agent{Spec: corev1alpha1.AgentSpec{Model: testOpenCodeModelConfig("openai/gpt-5.4")}}
	args := map[string]any{runtimeField: map[string]any{
		jsonSchemaTypeField:   "opencode",
		"defaultAllowedTools": []any{},
		"defaultAllowBash":    allowBash,
	}}
	if errResult, ok := parseRuntimeConfig(args, agent, executionmode.HarnessV2); !ok {
		t.Fatalf("parseRuntimeConfig returned error: %s", errResult)
	}
	if agent.Spec.Runtime == nil || agent.Spec.Runtime.DefaultAllowedTools == nil || len(agent.Spec.Runtime.DefaultAllowedTools) != 0 {
		t.Fatalf("defaultAllowedTools = %#v, want explicit empty", agent.Spec.Runtime)
	}
	if agent.Spec.Runtime.DefaultAllowBash == nil || *agent.Spec.Runtime.DefaultAllowBash {
		t.Fatalf("defaultAllowBash = %v, want false", agent.Spec.Runtime.DefaultAllowBash)
	}
}

func TestParseRuntimeConfig_RejectsInvalidOpenCodeModel(t *testing.T) {
	contextWindow := int32(32768)
	maxTokens := int32(4096)
	inverted := int32(4096)
	for name, model := range map[string]*corev1alpha1.ModelConfig{
		"missing":         nil,
		"bare":            {Name: "gpt-5.4", ContextWindow: &contextWindow, MaxTokens: &maxTokens},
		"substitution":    {Name: "{env:PROVIDER}/gpt", ContextWindow: &contextWindow, MaxTokens: &maxTokens},
		"missing context": {Name: "openai/gpt-5.4", MaxTokens: &maxTokens},
		"missing output":  {Name: "openai/gpt-5.4", ContextWindow: &contextWindow},
		"inverted":        {Name: "openai/gpt-5.4", ContextWindow: &inverted, MaxTokens: &maxTokens},
	} {
		t.Run(name, func(t *testing.T) {
			agent := &corev1alpha1.Agent{Spec: corev1alpha1.AgentSpec{Model: model}}
			errResult, ok := parseRuntimeConfig(map[string]any{runtimeField: map[string]any{jsonSchemaTypeField: "opencode"}}, agent, executionmode.HarnessV2)
			if ok {
				t.Fatal("parseRuntimeConfig accepted invalid OpenCode model")
			}
			var response ChatToolResult
			if err := json.Unmarshal([]byte(errResult), &response); err != nil {
				t.Fatal(err)
			}
			if response.ErrorType != errTypeInvalidArgs {
				t.Fatalf("response = %#v, want invalid arguments", response)
			}
		})
	}
}

// A coordination restriction with the wrong JSON type used to be ignored, and an
// empty allowedAgents lets the Agent delegate to any agent.
func TestChatCreateAgentTool_Execute_RejectsWrongTypedCoordination(t *testing.T) {
	run := func(coordination string) (ChatToolResult, []corev1alpha1.Agent) {
		t.Helper()
		fc := newFakeClient()
		ctx := WithToolContext(context.Background(), &ToolContext{Client: fc, Namespace: defaultNamespace, ExecutionMode: executionmode.HarnessV2})
		result, err := (&ChatCreateAgentTool{}).Execute(ctx, json.RawMessage(`{"name":"lead","systemPrompt":"p","coordination":`+coordination+`}`))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var r ChatToolResult
		if err := json.Unmarshal([]byte(result), &r); err != nil {
			t.Fatalf("failed to parse result: %v", err)
		}
		var agents corev1alpha1.AgentList
		if err := fc.List(context.Background(), &agents); err != nil {
			t.Fatal(err)
		}
		return r, agents.Items
	}

	r, agents := run(`{"enabled":true,"maxDepth":2,"maxConcurrentChildren":3,"allowedAgents":[{"name":"coder"}]}`)
	if !r.Success || len(agents) != 1 {
		t.Fatalf("valid coordination: result = %#v, agents = %d", r, len(agents))
	}
	if coord := agents[0].Spec.Coordination; coord == nil || !coord.Enabled || coord.MaxDepth != 2 || coord.MaxConcurrentChildren != 3 || len(coord.AllowedAgents) != 1 || coord.AllowedAgents[0].Name != "coder" {
		t.Fatalf("valid coordination stored as %#v", agents[0].Spec.Coordination)
	}

	for _, tt := range []struct {
		coordination string
		field        string
	}{
		{`{"enabled":true,"allowedAgents":"coder"}`, "coordination.allowedAgents"},
		{`{"enabled":true,"allowedAgents":{"name":"coder"}}`, "coordination.allowedAgents"},
		{`{"enabled":true,"maxDepth":"2"}`, "coordination.maxDepth"},
		{`{"enabled":true,"maxConcurrentChildren":1.5}`, "coordination.maxConcurrentChildren"},
		{`{"enabled":true,"maxConcurrentChildren":4294967297}`, "coordination.maxConcurrentChildren"},
		{`{"enabled":true,"maxDepth":4294967298}`, "coordination.maxDepth"},
		{`{"enabled":"true"}`, "coordination.enabled"},
	} {
		r, agents := run(tt.coordination)
		if r.Success || r.ErrorType != errTypeInvalidArgs || !strings.Contains(r.Error, tt.field) {
			t.Errorf("coordination %s: result = %#v, want invalid arguments naming %s", tt.coordination, r, tt.field)
		}
		if len(agents) != 0 {
			t.Errorf("coordination %s created %d Agents", tt.coordination, len(agents))
		}
	}
}

func TestChatCreateAgentTool_Execute_AllowedAgentIdentities(t *testing.T) {
	for _, mode := range []string{"direct", "registry"} {
		t.Run(mode, func(t *testing.T) {
			for _, tt := range []struct {
				name          string
				allowedAgents string
				want          []corev1alpha1.AllowedAgent
				wantError     string
			}{
				{name: "numeric name", allowedAgents: `[{"name":123}]`, wantError: "coordination.allowedAgents[0].name must be a string"},
				{name: "boolean name", allowedAgents: `[{"name":true}]`, wantError: "coordination.allowedAgents[0].name must be a string"},
				{name: "null name", allowedAgents: `[{"name":null}]`, wantError: "coordination.allowedAgents[0].name must be a string"},
				{name: "object name", allowedAgents: `[{"name":{"value":"coder"}}]`, wantError: "coordination.allowedAgents[0].name must be a string"},
				{name: "array name", allowedAgents: `[{"name":["coder"]}]`, wantError: "coordination.allowedAgents[0].name must be a string"},
				{name: "numeric namespace", allowedAgents: `[{"name":"coder","namespace":123}]`, wantError: "coordination.allowedAgents[0].namespace must be a string"},
				{name: "boolean namespace", allowedAgents: `[{"name":"coder","namespace":false}]`, wantError: "coordination.allowedAgents[0].namespace must be a string"},
				{name: "null namespace", allowedAgents: `[{"name":"coder","namespace":null}]`, wantError: "coordination.allowedAgents[0].namespace must be a string"},
				{name: "object namespace", allowedAgents: `[{"name":"coder","namespace":{"value":"team"}}]`, wantError: "coordination.allowedAgents[0].namespace must be a string"},
				{name: "array namespace", allowedAgents: `[{"name":"coder","namespace":["team"]}]`, wantError: "coordination.allowedAgents[0].namespace must be a string"},
				{name: "invalid later name", allowedAgents: `[{"name":"coder"},{"name":123}]`, wantError: "coordination.allowedAgents[1].name must be a string"},
				{name: "invalid later namespace", allowedAgents: `[{"name":"coder"},{"name":"tester","namespace":null}]`, wantError: "coordination.allowedAgents[1].namespace must be a string"},
				{name: "missing name", allowedAgents: `[{}]`, wantError: "coordination.allowedAgents[0].name is required"},
				{name: "empty name", allowedAgents: `[{"name":""}]`, wantError: "coordination.allowedAgents[0].name is required"},
				{name: "blank name", allowedAgents: `[{"name":" \t\n "}]`, wantError: "coordination.allowedAgents[0].name is required"},
				{name: "omitted namespace", allowedAgents: `[{"name":"coder"}]`, want: []corev1alpha1.AllowedAgent{{Name: "coder"}}},
				{name: "empty namespace", allowedAgents: `[{"name":"coder","namespace":""}]`, want: []corev1alpha1.AllowedAgent{{Name: "coder"}}},
				{name: "explicit namespace", allowedAgents: `[{"name":"coder","namespace":"team"}]`, want: []corev1alpha1.AllowedAgent{{Name: "coder", Namespace: "team"}}},
				{name: "scalar strings", allowedAgents: `[{"name":"123","namespace":"true"}]`, want: []corev1alpha1.AllowedAgent{{Name: "123", Namespace: "true"}}},
				{name: "multiple agents", allowedAgents: `[{"name":"coder"},{"name":"tester","namespace":"team"}]`, want: []corev1alpha1.AllowedAgent{{Name: "coder"}, {Name: "tester", Namespace: "team"}}},
			} {
				t.Run(tt.name, func(t *testing.T) {
					wantError, want := tt.wantError, tt.want
					if mode == "registry" {
						// Declared null fields are omitted before Execute; a name is
						// still required, while an omitted namespace is valid.
						switch tt.name {
						case "null name":
							wantError = "coordination.allowedAgents[0].name is required"
						case "null namespace":
							wantError, want = "", []corev1alpha1.AllowedAgent{{Name: "coder"}}
						case "invalid later namespace":
							wantError, want = "", []corev1alpha1.AllowedAgent{{Name: "coder"}, {Name: "tester"}}
						}
					}
					fc := newFakeClient()
					ctx := WithToolContext(context.Background(), &ToolContext{Client: fc, Namespace: defaultNamespace, ExecutionMode: executionmode.HarnessV2})
					tool := &ChatCreateAgentTool{}
					execute := tool.Execute
					if mode == "registry" {
						registry := NewRegistry()
						registry.Register(tool)
						execute = func(ctx context.Context, args json.RawMessage) (string, error) {
							result, err := registry.Execute(ctx, tool.Name(), args)
							if argErr, ok := errors.AsType[*ToolArgumentError](err); ok {
								return ChatToolErrorResult(errTypeInvalidArgs, argErr.Error(), "Use the declared argument types")
							}
							return result, err
						}
					}
					result, err := execute(ctx, json.RawMessage(`{"name":"lead","systemPrompt":"p","coordination":{"enabled":true,"allowedAgents":`+tt.allowedAgents+`}}`))
					if err != nil {
						t.Fatalf("unexpected error: %v", err)
					}
					var response ChatToolResult
					if err := json.Unmarshal([]byte(result), &response); err != nil {
						t.Fatalf("failed to parse result: %v", err)
					}
					var agents corev1alpha1.AgentList
					if err := fc.List(ctx, &agents); err != nil {
						t.Fatal(err)
					}
					if wantError != "" {
						if response.Success || response.ErrorType != errTypeInvalidArgs || !strings.Contains(response.Error, wantError) {
							t.Errorf("result = %#v, want invalid arguments with error %q", response, wantError)
						}
						if len(agents.Items) != 0 {
							t.Errorf("rejected allowedAgents created %d Agents", len(agents.Items))
						}
						return
					}
					if !response.Success || len(agents.Items) != 1 {
						t.Fatalf("valid allowedAgents: result = %#v, agents = %d", response, len(agents.Items))
					}
					coord := agents.Items[0].Spec.Coordination
					if coord == nil || !coord.Enabled || !slices.Equal(coord.AllowedAgents, want) {
						t.Fatalf("coordination = %#v, want enabled with allowedAgents = %#v", coord, want)
					}
				})
			}
		})
	}
}
