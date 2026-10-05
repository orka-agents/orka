/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"errors"
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
