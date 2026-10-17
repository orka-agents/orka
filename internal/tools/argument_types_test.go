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
	"strings"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func TestNormalizeArgTypes(t *testing.T) {
	tool := &mockTool{
		name:       "typed",
		parameters: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"},"count":{"type":"integer"},"ratio":{"type":"number"},"flag":{"type":"boolean"},"items":{"type":"array"},"config":{"type":"object"}}}`),
	}
	tests := []struct {
		name    string
		args    string
		want    string
		wantErr string
	}{
		{name: "valid values pass", args: `{"text":"a","count":2,"ratio":1.5,"flag":true,"items":["x"],"config":{"k":"v"}}`, want: `{"text":"a","count":2,"ratio":1.5,"flag":true,"items":["x"],"config":{"k":"v"}}`},
		{name: "null counts as omitted", args: `{"text":null,"count":3}`, want: `{"count":3}`},
		{name: "numeric string becomes a number", args: `{"count":" 10 "}`, want: `{"count":10}`},
		{name: "precise numbers survive", args: `{"count":"9007199254740993","text":null}`, want: `{"count":9007199254740993}`},
		{name: "boolean string becomes a boolean", args: `{"flag":"TRUE"}`, want: `{"flag":true}`},
		{name: "number for a string stays", args: `{"text":42}`, want: `{"text":42}`},
		{name: "string for an object stays", args: `{"config":"openai/gpt-4.1"}`, want: `{"config":"openai/gpt-4.1"}`},
		{name: "undeclared field passes", args: `{"name":{"k":"v"}}`, want: `{"name":{"k":"v"}}`},
		{name: "non-object arguments pass to the tool", args: `[]`, want: `[]`},
		{name: "object for a string", args: `{"text":{"k":"v"}}`, wantErr: "text must be a string, got an object"},
		{name: "array for a string", args: `{"text":["a"]}`, wantErr: "text must be a string, got an array"},
		{name: "word for an integer", args: `{"count":"many"}`, wantErr: "count must be a whole number, got a non-numeric string"},
		{name: "fraction for an integer", args: `{"count":1.5}`, wantErr: "count must be a whole number, got a fraction"},
		{name: "NaN for a number", args: `{"ratio":"NaN"}`, wantErr: "ratio must be a number, got a non-numeric string"},
		{name: "boolean for a number", args: `{"ratio":true}`, wantErr: "ratio must be a number, got a boolean"},
		{name: "word for a boolean", args: `{"flag":"yes"}`, wantErr: "flag must be a boolean, got a non-boolean string"},
		{name: "number for a boolean", args: `{"flag":1}`, wantErr: "flag must be a boolean, got a number"},
		{name: "string for an array", args: `{"items":"echo hi"}`, wantErr: "items must be an array, got a string"},
		{name: "number for an object", args: `{"config":1}`, wantErr: "config must be an object, got a number"},
		{name: "trailing data is left for the tool", args: `{"count":"10"}garbage`, want: "raw"},
		{name: "duplicate keys", args: `{"text":"a","text":"b"}`, wantErr: "text must be sent once, got a duplicate"},
		{name: "duplicate key hiding a wrong type", args: `{"text":"safe","text":{"k":"v"}}`, wantErr: "text must be sent once, got a duplicate"},
		{name: "invalid unicode", args: `{"text":"\ud800"}`, wantErr: "text must be valid Unicode text, got invalid Unicode"},
		{name: "invalid unicode beside a null", args: `{"text":null,"other":"\ud800"}`, wantErr: "other must be valid Unicode text, got invalid Unicode"},
		{name: "invalid unicode in a field name", args: `{"te\ud800xt":"a"}`, wantErr: "every field name must be valid Unicode text, got invalid Unicode"},
		{name: "undeclared null is left for the tool", args: `{"text":"a","unused":null}`, want: "raw"},
		{name: "undeclared null survives re-encoding", args: `{"count":"5","unused":null}`, want: `{"count":5,"unused":null}`},
		{name: "integer too large for int64", args: `{"count":"9223372036854775808"}`, wantErr: "count must be a whole number, got a number out of range"},
		{name: "huge integral exponent", args: `{"count":1e400}`, wantErr: "count must be a whole number, got a number out of range"},
		{name: "most negative int64 exponent", args: `{"count":"1e-9223372036854775808"}`, wantErr: "count must be a whole number, got a fraction"},
		{name: "largest int64 exponent", args: `{"count":"1e9223372036854775807"}`, wantErr: "count must be a whole number, got a number out of range"},
		{name: "exponent beyond int64", args: `{"count":"1e-99999999999999999999"}`, wantErr: "count must be a whole number, got a fraction"},
		{name: "fraction float64 would round away", args: `{"count":"1.0000000000000001"}`, wantErr: "count must be a whole number, got a fraction"},
		{name: "half beyond float64 precision", args: `{"count":9007199254740992.5}`, wantErr: "count must be a whole number, got a fraction"},
		{name: "underflowing exponent", args: `{"count":"1e-400"}`, wantErr: "count must be a whole number, got a fraction"},
		{name: "overflowing number", args: `{"ratio":"1e400"}`, wantErr: "ratio must be a number, got a number out of range"},
		{name: "non-JSON number syntax", args: `{"ratio":"1/2"}`, wantErr: "ratio must be a number, got a non-numeric string"},
		{name: "leading zero is not a JSON number", args: `{"count":"01"}`, wantErr: "count must be a whole number, got a non-numeric string"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeArgTypes(tool, json.RawMessage(tt.args))
			if tt.wantErr != "" {
				var argErr *ToolArgumentError
				if !errors.As(err, &argErr) || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("normalizeArgTypes() error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeArgTypes() error = %v", err)
			}
			if tt.want == "raw" {
				if string(got) != tt.args {
					t.Fatalf("normalizeArgTypes() = %s, want the original %s", got, tt.args)
				}
				return
			}
			var gotValue, wantValue any
			if json.Unmarshal(got, &gotValue) != nil || json.Unmarshal([]byte(tt.want), &wantValue) != nil {
				t.Fatalf("normalizeArgTypes() = %s, want %s", got, tt.want)
			}
			if string(mustMarshalTest(t, gotValue)) != string(mustMarshalTest(t, wantValue)) || (tt.name == "precise numbers survive" && !strings.Contains(string(got), "9007199254740993")) {
				t.Fatalf("normalizeArgTypes() = %s, want %s", got, tt.want)
			}
		})
	}
}

// Integral values are rewritten to the plain integer spelling Go integer
// decoders accept.
func TestNormalizeArgTypesCanonicalizesIntegers(t *testing.T) {
	tool := &mockTool{name: "typed", parameters: json.RawMessage(`{"type":"object","properties":{"count":{"type":"integer"}}}`)}
	for args, want := range map[string]string{
		`{"count":"10.0"}`:                   `{"count":10}`,
		`{"count":10.0}`:                     `{"count":10}`,
		`{"count":"1e3"}`:                    `{"count":1000}`,
		`{"count":100e-2}`:                   `{"count":1}`,
		`{"count":"-0"}`:                     `{"count":0}`,
		`{"count":"-12.50e1"}`:               `{"count":-125}`,
		`{"count":"0e-9223372036854775808"}`: `{"count":0}`,
	} {
		got, err := normalizeArgTypes(tool, json.RawMessage(args))
		if err != nil || string(got) != want {
			t.Errorf("normalizeArgTypes(%s) = %s, %v; want %s", args, got, err, want)
		}
		var decoded struct{ Count int }
		if err := json.Unmarshal(got, &decoded); err != nil {
			t.Errorf("an int field cannot decode %s: %v", got, err)
		}
	}
}

func mustMarshalTest(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestRegistryExecuteRejectsWrongArgumentTypesBeforeTheTool(t *testing.T) {
	called := false
	r := NewRegistry()
	r.Register(&mockTool{
		name:       "typed",
		parameters: json.RawMessage(`{"type":"object","properties":{"count":{"type":"integer"}}}`),
		executeFunc: func(context.Context, json.RawMessage) (string, error) {
			called = true
			return "ok", nil
		},
	})
	_, err := r.Execute(context.Background(), "typed", json.RawMessage(`{"count":"many"}`))
	var argErr *ToolArgumentError
	if !errors.As(err, &argErr) || argErr.Field != "count" {
		t.Fatalf("Execute() error = %v, want a ToolArgumentError for count", err)
	}
	if called {
		t.Fatal("the tool ran with a wrong-typed argument")
	}

	_, err = r.Execute(context.Background(), "missing", json.RawMessage(`{}`))
	var notFound *ToolNotFoundError
	if !errors.As(err, &notFound) || err.Error() != `tool "missing" not found` {
		t.Fatalf("Execute() error = %v, want ToolNotFoundError", err)
	}
}

// A null name used to reach the tool as the literal string "<nil>" and create
// an Agent with that name.
func TestRegistryExecuteTreatsNullArgumentsAsOmitted(t *testing.T) {
	fc := newFakeClient()
	r := NewRegistry()
	RegisterChatTools(r)
	result, err := r.Execute(newCreateAgentTaskToolCtx(fc), "create_agent", json.RawMessage(`{"name":null}`))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !strings.Contains(result, "name is required") {
		t.Fatalf("Execute() = %s, want the missing-name error", result)
	}
	var agents corev1alpha1.AgentList
	if err := fc.List(context.Background(), &agents); err != nil {
		t.Fatal(err)
	}
	if len(agents.Items) != 0 {
		t.Fatalf("created %d Agents, want none", len(agents.Items))
	}
}

// Decoding stringified objects keeps only the last of duplicate keys, so the
// duplicate check must see the arguments as sent.
func TestRegistryExecuteRejectsDuplicateStringifiedObjects(t *testing.T) {
	called := false
	r := NewRegistry()
	r.Register(&mockTool{
		name:       "configured",
		parameters: json.RawMessage(`{"type":"object","properties":{"config":{"type":"object"}}}`),
		executeFunc: func(context.Context, json.RawMessage) (string, error) {
			called = true
			return "ok", nil
		},
	})
	_, err := r.Execute(context.Background(), "configured", json.RawMessage(`{"config":"{\"a\":1}","config":"{\"b\":2}"}`))
	var argErr *ToolArgumentError
	if !errors.As(err, &argErr) || argErr.Field != "config" {
		t.Fatalf("Execute() error = %v, want a duplicate-key ToolArgumentError for config", err)
	}
	if called {
		t.Fatal("the tool ran with a duplicate key")
	}
}

func TestNormalizeArgTypesRecursive(t *testing.T) {
	tool := &mockTool{name: "typed", parameters: json.RawMessage(`{
		"type":"object", "properties":{
			"config":{"type":"object", "additionalProperties":false, "properties":{
				"name":{"type":"string", "enum":["allowed"]},
				"count":{"type":"integer", "minimum":1, "maximum":10},
				"ratio":{"type":"number"},
				"enabled":{"type":"boolean"},
				"child":{"type":"object", "properties":{"count":{"type":"integer"}}},
				"items":{"type":"array", "items":{"type":"object", "properties":{
					"name":{"type":"string"}, "count":{"type":"integer"},
					"tags":{"type":"array", "items":{"type":"string"}}
				}}},
				"counts":{"type":"array", "items":{"type":"integer"}},
				"flags":{"type":"array", "items":{"type":"array", "items":{"type":"boolean"}}},
				"untyped":{"type":"array", "items":true},
				"structure":{"properties":{"counts":{"items":{"type":"integer"}}}}
			}}
		}
	}`)}
	for _, tt := range []struct {
		name, args, want, wantErr string
	}{
		{"valid unchanged", `{"config":{"name":"custom","count":0,"enabled":false,"items":[{"name":"coder","tags":["user"]}],"extra":{"count":"anything"}}}`, "raw", ""},
		{"nested coercions", `{"config":{"count":" 12 ","ratio":"1.5","enabled":"FALSE","child":{"count":1e2}}}`, `{"config":{"count":12,"ratio":1.5,"enabled":false,"child":{"count":100}}}`, ""},
		{"nested null fields omitted", `{"config":{"count":null,"child":{"count":null},"items":[{"name":null}],"extra":null}}`, `{"config":{"child":{},"items":[{}],"extra":null}}`, ""},
		{"array item coercions", `{"config":{"counts":["9007199254740993",10.0,"1e3"],"flags":[["TRUE","0",false]]}}`, `{"config":{"counts":[9007199254740993,10,1000],"flags":[[true,false,false]]}}`, ""},
		{"primitive string leniency", `{"config":{"name":false,"items":[{"name":42,"tags":[false,42,"user"]}]}}`, "raw", ""},
		{"untyped items unchanged", `{"config":{"untyped":[null,{"count":"anything"},[]]}}`, "raw", ""},
		{"stringified object", `{"config":"{\"count\":\"5\",\"enabled\":\"true\"}"}`, `{"config":{"count":5,"enabled":true}}`, ""},
		{"nested stringified object", `{"config":{"child":"{\"count\":\"5\"}"}}`, `{"config":{"child":{"count":5}}}`, ""},
		{"stringified array object item", `{"config":{"items":["{\"name\":\"coder\",\"count\":\"5\"}"]}}`, `{"config":{"items":[{"name":"coder","count":5}]}}`, ""},
		{"object shorthand unchanged", `{"config":{"child":"main","items":["coder"]}}`, "raw", ""},
		{"non-object string unchanged", `{"config":{"child":"[1,2]"}}`, "raw", ""},
		{"declared children without parent types", `{"config":{"structure":{"counts":["5"]}}}`, `{"config":{"structure":{"counts":[5]}}}`, ""},
		{"untyped parent not inferred", `{"config":{"structure":"main"}}`, "raw", ""},
		{"bad child without parent types", `{"config":{"structure":{"counts":["many"]}}}`, "", "config.structure.counts[0] must be a whole number, got a non-numeric string"},
		{"bad nested integer", `{"config":{"count":"many"}}`, "", "config.count must be a whole number, got a non-numeric string"},
		{"deep fractional integer", `{"config":{"child":{"count":1.5}}}`, "", "config.child.count must be a whole number, got a fraction"},
		{"bad nested number", `{"config":{"ratio":true}}`, "", "config.ratio must be a number, got a boolean"},
		{"bad nested boolean", `{"config":{"enabled":"yes"}}`, "", "config.enabled must be a boolean, got a non-boolean string"},
		{"bad nested object", `{"config":{"child":[]}}`, "", "config.child must be an object, got an array"},
		{"bad nested array", `{"config":{"items":{}}}`, "", "config.items must be an array, got an object"},
		{"bad object array item", `{"config":{"items":[false]}}`, "", "config.items[0] must be an object, got a boolean"},
		{"bad array item property", `{"config":{"items":[{"name":[]}]}}`, "", "config.items[0].name must be a string, got an array"},
		{"bad string array item", `{"config":{"items":[{"tags":[{}]}]}}`, "", "config.items[0].tags[0] must be a string, got an object"},
		{"bad integer array item", `{"config":{"counts":[1,"many"]}}`, "", "config.counts[1] must be a whole number, got a non-numeric string"},
		{"bad nested array item", `{"config":{"flags":[[true,"yes"]]}}`, "", "config.flags[0][1] must be a boolean, got a non-boolean string"},
		{"null object array item", `{"config":{"items":[null]}}`, "", "config.items[0] must be an object, got null"},
		{"null string array item", `{"config":{"items":[{"tags":[null]}]}}`, "", "config.items[0].tags[0] must be a string, got null"},
		{"bad stringified property", `{"config":"{\"items\":[{\"name\":[]}]}"}`, "", "config.items[0].name must be a string, got an array"},
		{"duplicate stringified keys", `{"config":"{\"count\":1,\"count\":\"many\"}"}`, "", "config.count must be sent once, got a duplicate"},
		{"duplicate nested keys inside stringified object", `{"config":"{\"child\":{\"count\":[],\"count\":1}}"}`, "", "config.child.count must be sent once, got a duplicate"},
		{"reverse duplicate nested keys inside stringified object", `{"config":"{\"child\":{\"count\":1,\"count\":[]}}"}`, "", "config.child.count must be sent once, got a duplicate"},
		{"duplicate array object keys inside stringified object", `{"config":"{\"items\":[{\"name\":[],\"name\":\"ok\"}]}"}`, "", "config.items[0].name must be sent once, got a duplicate"},
		{"invalid stringified unicode", `{"config":"{\"name\":\"\\ud800\"}"}`, "", "config.name must be valid Unicode text, got invalid Unicode"},
		{"bad nested stringified property", `{"config":{"child":"{\"count\":\"many\"}"}}`, "", "config.child.count must be a whole number, got a non-numeric string"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeArgTypes(tool, json.RawMessage(tt.args))
			if tt.wantErr != "" {
				var argErr *ToolArgumentError
				if !errors.As(err, &argErr) || err.Error() != "invalid arguments: "+tt.wantErr {
					t.Fatalf("normalizeArgTypes() error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeArgTypes() error = %v", err)
			}
			if tt.want == "raw" {
				if string(got) != tt.args {
					t.Fatalf("normalizeArgTypes() = %s, want original %s", got, tt.args)
				}
				return
			}
			assertNormalizedArgJSON(t, got, tt.want)
		})
	}
}

func TestNormalizeArgTypesAdditionalProperties(t *testing.T) {
	tool := &mockTool{name: "typed", parameters: json.RawMessage(`{
		"type":"object", "additionalProperties":{"type":"integer"}, "properties":{
			"labels":{"type":"object", "additionalProperties":{"type":"string"}},
			"counts":{"type":"object", "properties":{"enabled":{"type":"boolean"}}, "additionalProperties":{"type":"integer"}},
			"rules":{"type":"object", "additionalProperties":{"type":"object", "properties":{"enabled":{"type":"boolean"}}}},
			"names":{"type":"object", "additionalProperties":{"type":"array", "items":{"type":"string"}}},
			"anything":{"type":"object", "additionalProperties":true},
			"untyped":{"type":"object", "additionalProperties":{}}
		}
	}`)}
	for _, tt := range []struct {
		name, args, want, wantErr string
	}{
		{"typed root extras", `{"extra":"5"}`, `{"extra":5}`, ""},
		{"typed root null omitted", `{"extra":null}`, `{}`, ""},
		{"typed values normalized", `{"counts":{"first":"1e2","second":2.0,"enabled":"true"}}`, `{"counts":{"first":100,"second":2,"enabled":true}}`, ""},
		{"typed null values omitted", `{"labels":{"app":null},"counts":{"first":null}}`, `{"labels":{},"counts":{}}`, ""},
		{"string value leniency", `{"labels":{"app":false,"number":42}}`, "raw", ""},
		{"untyped extras unchanged", `{"anything":{"value":null,"other":{"x":[]}},"untyped":{"value":null}}`, "raw", ""},
		{"recursive typed values", `{"rules":{"first":{"enabled":"TRUE"},"second":"{\"enabled\":\"false\"}"}}`, `{"rules":{"first":{"enabled":true},"second":{"enabled":false}}}`, ""},
		{"typed arrays unchanged", `{"names":{"roles":["user","assistant"]}}`, "raw", ""},
		{"bad typed root extra", `{"extra":[]}`, "", "extra must be a whole number, got an array"},
		{"bad typed string value", `{"labels":{"app":{}}}`, "", "labels.app must be a string, got an object"},
		{"bad typed integer value", `{"counts":{"first":"many"}}`, "", "counts.first must be a whole number, got a non-numeric string"},
		{"bad recursive typed value", `{"rules":{"first":{"enabled":[]}}}`, "", "rules.first.enabled must be a boolean, got an array"},
		{"bad typed array value", `{"names":{"roles":[{}]}}`, "", "names.roles[0] must be a string, got an object"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeArgTypes(tool, json.RawMessage(tt.args))
			if tt.wantErr != "" {
				var argErr *ToolArgumentError
				if !errors.As(err, &argErr) || err.Error() != "invalid arguments: "+tt.wantErr {
					t.Fatalf("normalizeArgTypes() error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeArgTypes() error = %v", err)
			}
			if tt.want == "raw" {
				if string(got) != tt.args {
					t.Fatalf("normalizeArgTypes() = %s, want original %s", got, tt.args)
				}
				return
			}
			assertNormalizedArgJSON(t, got, tt.want)
		})
	}
}

func assertNormalizedArgJSON(t *testing.T, got json.RawMessage, want string) {
	t.Helper()
	gotValue, gotErr := decodeArgValue(got, "")
	wantValue, wantErr := decodeArgValue(json.RawMessage(want), "")
	if gotErr != nil || wantErr != nil || string(mustMarshalTest(t, gotValue)) != string(mustMarshalTest(t, wantValue)) {
		t.Fatalf("normalized arguments = %s, want %s (decode errors: %v, %v)", got, want, gotErr, wantErr)
	}
}

func TestRegistryExecuteRejectsNestedArgumentTypesBeforeTheTool(t *testing.T) {
	for _, tt := range []struct {
		name, args, field, control string
		tool                       Tool
	}{
		{"chat allowed agent name", `{"name":"lead","coordination":{"enabled":true,"allowedAgents":[{"name":[]}]}}`, "coordination.allowedAgents[0].name", `{"name":"lead","coordination":{"enabled":"true","maxDepth":"2","allowedAgents":[{"name":"coder"}]}}`, &ChatCreateAgentTool{}},
		{"chat tools entry", `{"name":"lead","tools":[{}]}`, "tools[0]", `{"name":"lead","tools":["web_search"]}`, &ChatCreateAgentTool{}},
		{"coordinator tools entry", `{"role":"coder","systemPrompt":"p","tools":[[]]}`, "tools[0]", `{"role":"coder","systemPrompt":"p","tools":["web_search"]}`, &CreateAgentTool{}},
		{"transcript role entry", `{"query":"prior","roles":[{}]}`, "roles[0]", `{"query":"prior","roles":["user","assistant"]}`, NewSearchTranscriptTool()},
		{"stringified coordination", `{"name":"lead","coordination":"{\"enabled\":true,\"allowedAgents\":[{\"name\":[]}]}"}`, "coordination.allowedAgents[0].name", `{"name":"lead","coordination":"{\"enabled\":true,\"allowedAgents\":[{\"name\":\"coder\"}]}"}`, &ChatCreateAgentTool{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			r := NewRegistry()
			r.Register(&mockTool{
				name: tt.tool.Name(), parameters: tt.tool.Parameters(),
				executeFunc: func(context.Context, json.RawMessage) (string, error) {
					called = true
					return "ok", nil
				},
			})
			_, err := r.Execute(context.Background(), tt.tool.Name(), json.RawMessage(tt.args))
			var argErr *ToolArgumentError
			if !errors.As(err, &argErr) || argErr.Field != tt.field {
				t.Fatalf("Execute() error = %v, want a ToolArgumentError for %s", err, tt.field)
			}
			if called {
				t.Fatal("the tool ran with a wrong-typed nested argument")
			}
			if _, err := r.Execute(context.Background(), tt.tool.Name(), json.RawMessage(tt.control)); err != nil || !called {
				t.Fatalf("valid control did not reach the tool: called = %v, error = %v", called, err)
			}
		})
	}
}

func TestRegistryExecuteNormalizesCoordinationBeforeAgentCreation(t *testing.T) {
	for _, tt := range []struct {
		name, coordination, wantField string
	}{
		{"bad allowed agent name", `{"enabled":true,"allowedAgents":[{"name":[]}]}`, "coordination.allowedAgents[0].name"},
		{"duplicate allowed agent name invalid first", `{"enabled":true,"allowedAgents":[{"name":[],"name":"coder"}]}`, "coordination.allowedAgents[0].name"},
		{"duplicate allowed agent name invalid last", `{"enabled":true,"allowedAgents":[{"name":"coder","name":[]}]}`, "coordination.allowedAgents[0].name"},
		{"duplicate allowed agent name inside stringified config", `"{\"enabled\":true,\"allowedAgents\":[{\"name\":[],\"name\":\"coder\"}]}"`, "coordination.allowedAgents[0].name"},
		{"bad allowed agent namespace", `{"enabled":true,"allowedAgents":[{"name":"coder","namespace":{}}]}`, "coordination.allowedAgents[0].namespace"},
		{"bad allowed agent item", `{"enabled":true,"allowedAgents":[false]}`, "coordination.allowedAgents[0]"},
		{"valid coercions", `{"enabled":"true","maxDepth":"2","maxConcurrentChildren":"3","allowedAgents":[{"name":"coder","namespace":null}]}`, ""},
		{"valid stringified config", `"{\"enabled\":\"true\",\"maxDepth\":\"2\",\"maxConcurrentChildren\":\"3\",\"allowedAgents\":[{\"name\":\"coder\"}]}"`, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fc := newFakeClient()
			r := NewRegistry()
			RegisterChatTools(r)
			result, err := r.Execute(newCreateAgentTaskToolCtx(fc), "create_agent", json.RawMessage(`{"name":"lead","systemPrompt":"p","coordination":`+tt.coordination+`}`))
			var agents corev1alpha1.AgentList
			if listErr := fc.List(context.Background(), &agents); listErr != nil {
				t.Fatal(listErr)
			}
			if tt.wantField != "" {
				var argErr *ToolArgumentError
				if !errors.As(err, &argErr) || argErr.Field != tt.wantField || len(agents.Items) != 0 {
					t.Fatalf("Execute() error = %v, agents = %d; want rejected %s and no Agents", err, len(agents.Items), tt.wantField)
				}
				return
			}
			var response ChatToolResult
			if err != nil || json.Unmarshal([]byte(result), &response) != nil || !response.Success || len(agents.Items) != 1 {
				t.Fatalf("valid Execute() result = %s, error = %v, agents = %d", result, err, len(agents.Items))
			}
			coord := agents.Items[0].Spec.Coordination
			if coord == nil || !coord.Enabled || coord.MaxDepth != 2 || coord.MaxConcurrentChildren != 3 || len(coord.AllowedAgents) != 1 || coord.AllowedAgents[0].Name != "coder" || coord.AllowedAgents[0].Namespace != "" {
				t.Fatalf("normalized coordination stored as %#v", coord)
			}
		})
	}
}

func TestToolArgumentsRejectRecursiveDuplicateKeysBeforeExecution(t *testing.T) {
	parameters := json.RawMessage(`{
		"type":"object", "properties":{
			"config":{"type":"object", "properties":{
				"name":{"type":"string"},
				"child":{"type":"object", "properties":{"name":{"type":"string"}}}
			}},
			"items":{"type":"array", "items":{"type":"object", "properties":{"name":{"type":"string"}}}},
			"matrix":{"type":"array", "items":{"type":"array", "items":{"type":"object", "properties":{"name":{"type":"string"}}}}},
			"labels":{"type":"object", "additionalProperties":{"type":"string"}},
			"rules":{"type":"object", "additionalProperties":{"type":"object", "properties":{"name":{"type":"string"}}}},
			"loose":{"type":"array"}
		}
	}`)
	for _, scope := range []struct {
		name, format, field string
	}{
		{"nested object", `{"config":%s}`, "config.name"},
		{"deep object", `{"config":{"child":%s}}`, "config.child.name"},
		{"array object", `{"items":[{"name":"sibling"},%s]}`, "items[1].name"},
		{"nested array object", `{"matrix":[[{"name":"sibling"},%s]]}`, "matrix[0][1].name"},
		{"typed map key", `{"labels":%s}`, "labels.name"},
		{"typed map object", `{"rules":{"entry":%s}}`, "rules.entry.name"},
		{"undeclared object", `{"unknown":%s}`, "unknown.name"},
		{"untyped array object", `{"loose":[%s]}`, "loose[0].name"},
	} {
		t.Run(scope.name, func(t *testing.T) {
			for _, order := range []struct {
				name, value string
			}{
				{"invalid first", `{"name":[],"name":"ok"}`},
				{"invalid last", `{"name":"ok","name":[]}`},
				{"valid types", `{"name":"first","name":"second"}`},
				{"null first", `{"name":null,"name":"ok"}`},
				{"escaped key", `{"name":[],"\u006eame":"ok"}`},
			} {
				t.Run(order.name, func(t *testing.T) {
					called := false
					var executed json.RawMessage
					tool := &mockTool{
						name: "typed", parameters: parameters,
						executeFunc: func(_ context.Context, args json.RawMessage) (string, error) {
							called, executed = true, args
							return "ok", nil
						},
					}
					args := json.RawMessage(fmt.Sprintf(scope.format, order.value))
					normalized, err := NormalizeToolArguments(tool, args)
					var argErr *ToolArgumentError
					if !errors.As(err, &argErr) || argErr.Field != scope.field || argErr.Want != "sent once" || argErr.Got != "a duplicate" || normalized != nil {
						t.Fatalf("NormalizeToolArguments() = %s, %v; want duplicate-key error for %s", normalized, err, scope.field)
					}
					r := NewRegistry()
					r.Register(tool)
					_, err = r.Execute(context.Background(), tool.Name(), args)
					if !errors.As(err, &argErr) || argErr.Field != scope.field || argErr.Want != "sent once" || argErr.Got != "a duplicate" || called {
						t.Fatalf("Execute() error = %v, called = %v; want duplicate-key error for %s and no side effect", err, called, scope.field)
					}
					control := json.RawMessage(fmt.Sprintf(scope.format, `{"name":"ok"}`))
					normalized, err = NormalizeToolArguments(tool, control)
					if err != nil {
						t.Fatalf("NormalizeToolArguments() rejected valid control: %v", err)
					}
					if _, err := r.Execute(context.Background(), tool.Name(), control); err != nil || !called || string(executed) != string(normalized) {
						t.Fatalf("valid control executed = %s, called = %v, error = %v; want normalized %s", executed, called, err, normalized)
					}
				})
			}
		})
	}
}

func TestNormalizeToolArgumentsPreservesRecursiveValues(t *testing.T) {
	tool := &mockTool{name: "typed", parameters: json.RawMessage(`{
		"type":"object", "properties":{
			"config":{"type":"object", "properties":{
				"name":{"type":"string"}, "count":{"type":"integer"},
				"ratio":{"type":"number"}, "enabled":{"type":"boolean"},
				"child":{"type":"object", "properties":{"count":{"type":"integer"}}}
			}},
			"items":{"type":"array", "items":{"type":"object", "properties":{"name":{"type":"string"}, "count":{"type":"integer"}}}},
			"labels":{"type":"object", "additionalProperties":{"type":"string"}},
			"ratios":{"type":"object", "additionalProperties":{"type":"number"}},
			"rules":{"type":"object", "additionalProperties":{"type":"object", "properties":{"name":{"type":"string"}, "count":{"type":"integer"}}}}
		}
	}`)}
	for _, tt := range []struct {
		name, args, want string
	}{
		{"unchanged numbers and sibling keys", `{"config":{"name":"ok","count":9007199254740993,"ratio":1.00e+03,"enabled":false},"items":[{"name":"ok"},{"name":"ok"}],"ratios":{"a":2.0000000000000001,"b":1e-400},"rules":{"a":{"name":"ok"},"b":{"name":"ok"}}}`, "raw"},
		{"coercions and nulls", `{"config":{"name":null,"count":"9007199254740993","ratio":"1.0000000000000001","enabled":"TRUE"},"items":[{"name":null,"count":"1e2"}],"labels":{"app":null,"number":42,"enabled":false},"ratios":{"exact":"2.0000000000000001"},"rules":{"entry":{"name":null,"count":"10.0"}},"unused":[9007199254740993,1.00e+03,1e400,null,{},[]]}`, `{"config":{"count":9007199254740993,"ratio":1.0000000000000001,"enabled":true},"items":[{"count":100}],"labels":{"number":42,"enabled":false},"ratios":{"exact":2.0000000000000001},"rules":{"entry":{"count":10}},"unused":[9007199254740993,1.00e+03,1e400,null,{},[]]}`},
		{"stringified object spelling", `{"config":"{\"ratio\":1.00e+03,\"count\":9007199254740993}"}`, `{"config":{"ratio":1.00e+03,"count":9007199254740993}}`},
		{"stringified nested continuations", `{"config":"{\"child\":\"{\\\"count\\\":\\\"10.0\\\"}\"}","items":["{\"name\":\"coder\",\"count\":\"9007199254740993\"}"],"rules":{"entry":"{\"name\":\"coder\",\"count\":\"1e2\"}"}}`, `{"config":{"child":{"count":10}},"items":[{"name":"coder","count":9007199254740993}],"rules":{"entry":{"name":"coder","count":100}}}`},
		{"object shorthand and opaque strings", `{"config":"profile/name","items":["coder"],"unused":"{\"name\":[],\"name\":\"ok\"}"}`, "raw"},
		{"valid Unicode", `{"config":{"name":"\ud83d\ude00"},"items":[{"name":"café"}],"labels":{"\u0061":"one","b":"two"}}`, "raw"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeToolArguments(tool, json.RawMessage(tt.args))
			if err != nil {
				t.Fatalf("NormalizeToolArguments() error = %v", err)
			}
			if tt.want == "raw" {
				if string(got) != tt.args {
					t.Fatalf("NormalizeToolArguments() = %s, want original %s", got, tt.args)
				}
				return
			}
			assertNormalizedArgJSON(t, got, tt.want)
		})
	}
	// Approval-bound callers must still distinguish numbers that float64
	// decoding would collapse to the same value.
	for _, tt := range []struct {
		field, first, second string
	}{
		{"count", "9007199254740992", "9007199254740993"},
		{"ratio", "1.0000000000000001", "1.0000000000000002"},
	} {
		t.Run("distinct "+tt.field, func(t *testing.T) {
			var normalized [2]json.RawMessage
			for i, value := range []string{tt.first, tt.second} {
				args := json.RawMessage(fmt.Sprintf(`{"config":{"%s":"%s"}}`, tt.field, value))
				got, err := NormalizeToolArguments(tool, args)
				want := fmt.Sprintf(`{"config":{"%s":%s}}`, tt.field, value)
				if err != nil || string(got) != want {
					t.Fatalf("NormalizeToolArguments(%s) = %s, %v; want %s", args, got, err, want)
				}
				normalized[i] = got
			}
			if string(normalized[0]) == string(normalized[1]) {
				t.Fatalf("distinct approval-bound arguments collapsed to %s", normalized[0])
			}
		})
	}
}

func TestNormalizeToolArgumentsRejectsInvalidNestedUnicode(t *testing.T) {
	tool := &mockTool{name: "typed", parameters: json.RawMessage(`{
		"type":"object", "properties":{
			"config":{"type":"object", "properties":{"name":{"type":"string"}}},
			"items":{"type":"array", "items":{"type":"object", "properties":{"name":{"type":"string"}}}}
		}
	}`)}
	for _, tt := range []struct {
		name, args, field string
	}{
		{"nested surrogate", `{"config":{"name":"\ud800"}}`, "config"},
		{"array surrogate", `{"items":[{"name":"\udc00"}]}`, "items"},
		{"nested field name", `{"config":{"bad\ud800key":"ok"}}`, "config"},
		{"nested UTF-8", `{"config":{"name":"` + string([]byte{0xff}) + `"}}`, "config"},
		{"stringified surrogate", `{"config":"{\"name\":\"\\ud800\"}"}`, "config.name"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NormalizeToolArguments(tool, json.RawMessage(tt.args))
			var argErr *ToolArgumentError
			if !errors.As(err, &argErr) || argErr.Field != tt.field || argErr.Want != "valid Unicode text" || argErr.Got != "invalid Unicode" {
				t.Fatalf("NormalizeToolArguments() error = %v, want invalid Unicode error for %s", err, tt.field)
			}
		})
	}
}
