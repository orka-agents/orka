/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package acp

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestParseSandboxExecArguments(t *testing.T) {
	cases := []struct {
		name        string
		args        string
		wantRuntime string
		wantPrefix  string
		wantTimeout time.Duration
	}{
		{"bash by default", `{"code":"ls"}`, "bash", "cd /workspace || exit 125\nls", SandboxExecDefaultTimeout},
		{"python", `{"language":"Python","code":"print(1)","timeout":5}`, "python", "import os as _orka_os; _orka_os.chdir(\"/workspace\"); del _orka_os\nprint(1)", 5 * time.Second},
		{"javascript, timeout as a string", `{"language":"javascript","code":"1","timeout":"30"}`, "node", "process.chdir(\"/workspace\");\n1", 30 * time.Second},
		{"timeout clamped", `{"code":"true","timeout":3600}`, "bash", "cd /workspace", SandboxExecMaxTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request, err := ParseSandboxExecArguments(json.RawMessage(tc.args))
			if err != nil {
				t.Fatalf("ParseSandboxExecArguments(%s) error = %v", tc.args, err)
			}
			if request.Runtime != tc.wantRuntime || !strings.HasPrefix(request.Script, tc.wantPrefix) || request.Timeout != tc.wantTimeout {
				t.Fatalf("request = %+v, want runtime %s, script starting %q, timeout %s", request, tc.wantRuntime, tc.wantPrefix, tc.wantTimeout)
			}
		})
	}
}

func TestParseSandboxExecArgumentsRejects(t *testing.T) {
	for _, args := range []string{
		`[]`,
		`{}`,
		`{"code":"  "}`,
		`{"code":"x","language":"ruby"}`,
		`{"code":"x","language":1}`,
		`{"code":"x","timeout":0}`,
		`{"code":"x","timeout":1.5}`,
		`{"code":"x","timeout":"soon"}`,
		`{"code":"x","cwd":"/"}`,
	} {
		if _, err := ParseSandboxExecArguments(json.RawMessage(args)); err == nil {
			t.Errorf("ParseSandboxExecArguments(%s) accepted it", args)
		}
	}
}

func TestSandboxExecInputSchemaIsJSON(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal([]byte(SandboxExecInputSchema), &schema); err != nil || schema["type"] != "object" {
		t.Fatalf("schema = %v, err = %v", schema, err)
	}
}
