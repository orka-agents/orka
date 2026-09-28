package worker

import (
	"encoding/json"
	"strings"
	"testing"

	kubefake "k8s.io/client-go/kubernetes/fake"
)

func TestRemoteMCPArgumentNumericBudget(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		valid       bool
	}{
		{"positive exponent boundary", `1e65535`, true},
		{"negative exponent boundary", `-1E-65535`, true},
		{"signed decimal boundary", `-12.34e+65532`, true},
		{"exponent beyond boundary", `1e65536`, false},
		{"signed decimal beyond boundary", `-12.34e-65533`, false},
		{"coefficient boundary", strings.Repeat("1", 65536), true},
		{"coefficient beyond boundary", strings.Repeat("1", 65537), false},
		{"fraction boundary", `0.` + strings.Repeat("1", 65535), true},
		{"aggregate boundary", `[1e32767,{"n":-1e-32767}]`, true},
		{"aggregate beyond boundary", `[1e32767,{"n":-1e-32768}]`, false},
		{"exponent integer overflow", `1e999999999999999999999999999999999999`, false},
		{"leading exponent zeroes", `1e+00065535`, true},
		{"numeric-looking string", `"1e999999999999999999999999999999999999"`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &remoteProtocolFixture{call: func(params json.RawMessage) {
				var call struct {
					Arguments map[string]json.RawMessage `json:"arguments"`
				}
				if json.Unmarshal(params, &call) != nil || string(call.Arguments["value"]) != tc.value {
					t.Error("numeric budget guard changed an accepted value on the wire")
				}
			}}
			e := remoteTestExecutor(t, f.serve(t))
			_, err := e.Execute(t.Context(), remoteTestTool(t), json.RawMessage(`{"value":`+tc.value+`}`))
			if (err == nil) != tc.valid {
				t.Fatalf("budget acceptance = %t; want %t", err == nil, tc.valid)
			}
			if !tc.valid {
				if err == nil || !strings.Contains(err.Error(), "remote MCP numeric expansion exceeds limit") {
					t.Error("numeric budget failure lost its safe category")
				}
				if f.requests.Load() != 0 || len(e.k8sClient.(*kubefake.Clientset).Actions()) != 0 {
					t.Error("overbudget arguments accessed credentials or the remote endpoint")
				}
			} else if f.calls.Load() != 1 {
				t.Error("valid budget-boundary arguments did not reach the remote call")
			}
		})
	}
}

func TestRemoteMCPArgumentValidationHandlesUnrepresentableExponent(t *testing.T) {
	defer func() {
		if recover() != nil {
			t.Error("argument validation panicked on an unrepresentable numeric exponent")
		}
	}()
	f := &remoteProtocolFixture{}
	e := remoteTestExecutor(t, f.serve(t))
	tool := remoteTestTool(t)
	tool.Spec.Parameters.Raw = []byte(`{"type":"object","properties":{"value":{"maximum":1}}}`)
	if _, err := e.Execute(t.Context(), tool, json.RawMessage(`{"value":1e1000001}`)); err == nil {
		t.Error("unrepresentable argument was accepted")
	}
	if f.requests.Load() != 0 || len(e.k8sClient.(*kubefake.Clientset).Actions()) != 0 {
		t.Error("unrepresentable argument accessed credentials or the remote endpoint")
	}
}

func TestRemoteMCPArgumentsMatchExactReviewedSchema(t *testing.T) {
	for _, tc := range []struct {
		name, constraint, value string
		valid                   bool
	}{
		{"large minimum boundary", `{"minimum":9007199254740993}`, `9007199254740993`, true},
		{"below large minimum", `{"minimum":9007199254740993}`, `9007199254740992`, false},
		{"large maximum boundary", `{"maximum":9007199254740995}`, `9007199254740995`, true},
		{"above large maximum", `{"maximum":9007199254740995}`, `9007199254740996`, false},
		{"exclusive maximum", `{"exclusiveMaximum":9007199254740993}`, `9007199254740993`, false},
		{"exclusive minimum", `{"exclusiveMinimum":9007199254740993}`, `9007199254740993`, false},
		{"decimal bound", `{"maximum":0.1}`, `0.1`, true},
		{"above decimal bound", `{"maximum":0.1}`, `0.100000000000000001`, false},
		{"decimal multiple", `{"multipleOf":0.1}`, `0.3`, true},
		{"not decimal multiple", `{"multipleOf":0.1}`, `0.3000000000000000001`, false},
		{"large nonmultiple", `{"multipleOf":1}`, `9007199254740993.1`, false},
		{"exact enum", `{"enum":[9007199254740993]}`, `9007199254740993.0`, true},
		{"rounded enum", `{"enum":[9007199254740993]}`, `9007199254740992`, false},
		{"exact const", `{"const":{"n":0.123456789012345678901}}`, `{"n":0.123456789012345678901}`, true},
		{"different const", `{"const":{"n":0.123456789012345678901}}`, `{"n":0.123456789012345678902}`, false},
		{"local reference", `{"$ref":"#/$defs/choice"}`, `9007199254740993`, true},
		{"invalid local reference value", `{"$ref":"#/$defs/choice"}`, `9007199254740992`, false},
		{"missing required", `{"type":"string"}`, ``, false},
		{"string type", `{"type":"string"}`, `"fixture-sensitive-value"`, true},
		{"wrong type", `{"type":"string"}`, `false`, false},
		{"nested array", `{"type":"array","items":{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"],"additionalProperties":false}}`, `[{"n":9007199254740993}]`, true},
		{"nested violation", `{"type":"array","items":{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"],"additionalProperties":false}}`, `[{"n":1,"extra":"fixture-sensitive-value"}]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			schema := `{"type":"object","properties":{"value":` + tc.constraint + `},"required":["value"],"additionalProperties":false,"$defs":{"choice":{"enum":[9007199254740993]}}}`
			f := &remoteProtocolFixture{
				list: func(_ int, _ json.RawMessage) string {
					return `{"tools":[{"name":"service_health","inputSchema":` + schema + `}]}`
				},
				call: func(params json.RawMessage) {
					var call struct {
						Arguments map[string]json.RawMessage `json:"arguments"`
					}
					if json.Unmarshal(params, &call) != nil || string(call.Arguments["value"]) != tc.value {
						t.Error("validated value changed on the wire")
					}
				},
			}
			e := remoteTestExecutor(t, f.serve(t))
			tool := remoteTestTool(t)
			tool.Spec.Parameters.Raw = []byte(schema)
			// Discovery must not validate synthetic {} against required call arguments.
			if err := e.VerifyRemoteMCPTool(t.Context(), tool); err != nil {
				t.Fatal("reviewed schema could not be exposed")
			}
			before := f.requests.Load()
			kube := e.k8sClient.(*kubefake.Clientset)
			kube.ClearActions()
			args := `{}`
			if tc.value != "" {
				args = `{"value":` + tc.value + `}`
			}
			_, err := e.Execute(t.Context(), tool, json.RawMessage(args))
			if (err == nil) != tc.valid {
				t.Fatalf("argument acceptance = %t; want %t", err == nil, tc.valid)
			}
			if err != nil && strings.Contains(err.Error(), "fixture-sensitive-value") {
				t.Error("validation error exposed input data")
			}
			if !tc.valid && (f.requests.Load() != before || len(kube.Actions()) != 0) {
				t.Error("invalid arguments accessed credentials or the remote endpoint")
			}
		})
	}
}

func TestRemoteMCPRejectsArgumentsBeforeCredentialsAndNetwork(t *testing.T) {
	f := &remoteProtocolFixture{list: func(_ int, _ json.RawMessage) string {
		return `{"tools":[{"name":"service_health","inputSchema":{"type":"object","additionalProperties":false}}]}`
	}}
	e := remoteTestExecutor(t, f.serve(t))
	tool := remoteTestTool(t)
	tool.Spec.Parameters.Raw = []byte(`{"type":"object","additionalProperties":false}`)
	_, err := e.Execute(t.Context(), tool, json.RawMessage(`{"unreviewed":"fixture-sensitive-value"}`))
	if err == nil {
		t.Error("arguments outside the reviewed schema were accepted")
	} else if strings.Contains(err.Error(), "fixture-sensitive-value") {
		t.Error("argument validation error exposed the argument value")
	}
	if f.requests.Load() != 0 {
		t.Error("invalid arguments issued protocol traffic")
	}
	if len(e.k8sClient.(*kubefake.Clientset).Actions()) != 0 {
		t.Error("invalid arguments accessed credentials")
	}
}
