/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package acp

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseSandboxExecArguments(t *testing.T) {
	const pythonPrefix = "import os as _orka_os, sys as _orka_sys; _orka_os.chdir(\"/workspace\"); " +
		"_orka_sys.path.insert(0, \"/workspace\"); del _orka_os, _orka_sys\nexec(compile("
	cases := []struct {
		name        string
		args        string
		wantRuntime string
		wantPrefix  string
		wantTimeout time.Duration
	}{
		{"python by default", `{"code":"print(1)"}`, "python", pythonPrefix, SandboxExecDefaultTimeout},
		{"bash", `{"language":"bash","code":"ls"}`, "bash", "cd /workspace || exit 125\nls", SandboxExecDefaultTimeout},
		{"sh is the shell", `{"language":"sh","code":"ls"}`, "bash", "cd /workspace || exit 125\nls", SandboxExecDefaultTimeout},
		{"python", `{"language":"Python","code":"print(1)","timeout":5}`, "python", pythonPrefix, 5 * time.Second},
		{"javascript, timeout as a string", `{"language":"javascript","code":"1","timeout":"30"}`, "node", "(() => {\nconst Module = require('module');", 30 * time.Second},
		{"timeout clamped", `{"language":"bash","code":"true","timeout":3600}`, "bash", "cd /workspace", SandboxExecMaxTimeout},
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
		`{"code":"x","timeout":"Inf"}`,
		`{"code":"x","timeout":"+Inf"}`,
		`{"code":"x","timeout":"-Inf"}`,
		`{"code":"x","timeout":"Infinity"}`,
		`{"code":"x","timeout":"NaN"}`,
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
	// The default the model reads must be the one the parser applies.
	language := schema["properties"].(map[string]any)["language"].(map[string]any)
	if language["default"] != sandboxExecDefaultLanguage {
		t.Fatalf("schema language default = %v, parser default = %s", language["default"], sandboxExecDefaultLanguage)
	}
}

// Match the guest drivers: Python compiles a whole string, and Node uses
// indirect eval with CommonJS globals inherited from /tmp/hl_bootstrap.js.
func TestSandboxExecPythonPreservesSource(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed")
	}
	for _, tc := range []struct {
		name  string
		code  string
		want  string
		fails bool
	}{
		{"future import", "from __future__ import annotations\nprint(123)", "123\n", false},
		{"docstring", "\"module documentation\"\nprint(__doc__)", "module documentation\n", false},
		{"quoted source", "print('single \" double \\\\ 雪')", "single \" double \\ 雪\n", false},
		{"workspace", "import os; open('result.txt', 'w').write('saved'); print(os.path.basename(os.getcwd()))", "workspace\n", false},
		{"workspace module", "import workspace_module; print(workspace_module.VALUE)", "workspace-module\n", false},
		{"syntax error", "if:", "SyntaxError", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace := filepath.Join(t.TempDir(), "workspace")
			if err := os.Mkdir(workspace, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(workspace, "workspace_module.py"), []byte("VALUE = 'workspace-module'\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			args, err := json.Marshal(map[string]any{"language": "python", "code": tc.code})
			if err != nil {
				t.Fatal(err)
			}
			request, err := ParseSandboxExecArguments(args)
			if err != nil {
				t.Fatal(err)
			}
			script := strings.ReplaceAll(request.Script, SandboxExecWorkspacePath, workspace)
			// -I leaves the current directory off sys.path, as the guest's
			// embedded interpreter does.
			output, runErr := exec.CommandContext(t.Context(), python, "-I", "-c", script).CombinedOutput()
			if (runErr != nil) != tc.fails || !strings.Contains(string(output), tc.want) {
				t.Fatalf("output = %q, error = %v, want %q, fails=%t", output, runErr, tc.want, tc.fails)
			}
			if tc.name == "workspace" {
				data, err := os.ReadFile(filepath.Join(workspace, "result.txt"))
				if err != nil || string(data) != "saved" {
					t.Fatalf("workspace output = %q, error = %v", data, err)
				}
			}
		})
	}
}

func TestSandboxExecNodePreservesSourceAndWorkspaceModules(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	for _, tc := range []struct {
		name  string
		code  string
		want  string
		fails bool
	}{
		{"strict mode", `"use strict"; undeclaredSandboxExecVariable = 1;`, "ReferenceError", true},
		{"package file", `console.log(require('./package.json').name)`, "workspace-package\n", false},
		{"module require", `console.log(module.require('./package.json').name)`, "workspace-package\n", false},
		{"main entrypoint", `if (require.main === module) console.log('ran-main');`, "ran-main\n", false},
		{"main module identity", `console.log(require.main === process.mainModule, require.main.filename === __filename)`, "true true\n", false},
		{"dependency", `console.log(require('workspace-dependency'))`, "dependency-loaded\n", false},
		{"module dependency", `console.log(module.require('workspace-dependency'))`, "dependency-loaded\n", false},
		{"module paths", `console.log(__dirname === process.cwd(), __filename.startsWith(process.cwd() + '/'), require.resolve('./package.json').startsWith(process.cwd() + '/'))`, "true true true\n", false},
		{"workspace", `require('fs').writeFileSync('result.txt', 'saved'); console.log(require('path').basename(process.cwd()))`, "workspace\n", false},
		{"quoted source", `console.log('single " double \\ 雪')`, "single \" double \\ 雪\n", false},
		{"promise rejection", `Promise.reject(new Error('sandbox promise rejected'))`, "sandbox promise rejected", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace := filepath.Join(t.TempDir(), "workspace")
			dependency := filepath.Join(workspace, "node_modules", "workspace-dependency")
			if err := os.MkdirAll(dependency, 0o700); err != nil {
				t.Fatal(err)
			}
			for name, data := range map[string]string{
				"package.json": `{"name":"workspace-package"}`,
				"node_modules/workspace-dependency/index.js": `module.exports = 'dependency-loaded';`,
			} {
				if err := os.WriteFile(filepath.Join(workspace, name), []byte(data), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			args, err := json.Marshal(map[string]any{"language": "javascript", "code": tc.code})
			if err != nil {
				t.Fatal(err)
			}
			request, err := ParseSandboxExecArguments(args)
			if err != nil {
				t.Fatal(err)
			}
			script, err := json.Marshal(strings.ReplaceAll(request.Script, SandboxExecWorkspacePath, workspace))
			if err != nil {
				t.Fatal(err)
			}
			bootstrap := `globalThis.require = require('module').createRequire('/tmp/hl_bootstrap.js');
			globalThis.module = module; globalThis.__dirname = '/tmp'; globalThis.__filename = '/tmp/hl_bootstrap.js';
			const result = (0, eval)(` + string(script) + `);
			if (result && typeof result.then === 'function') result.catch(error => { console.error(error); process.exitCode = 1; });`
			bootstrapPath := filepath.Join(filepath.Dir(workspace), "hl_bootstrap.js")
			if err := os.WriteFile(bootstrapPath, []byte(bootstrap), 0o600); err != nil {
				t.Fatal(err)
			}
			output, runErr := exec.CommandContext(t.Context(), node, bootstrapPath).CombinedOutput()
			if (runErr != nil) != tc.fails || !strings.Contains(string(output), tc.want) {
				t.Fatalf("output = %q, error = %v, want %q, fails=%t", output, runErr, tc.want, tc.fails)
			}
			if tc.name == "workspace" {
				data, err := os.ReadFile(filepath.Join(workspace, "result.txt"))
				if err != nil || string(data) != "saved" {
					t.Fatalf("workspace output = %q, error = %v", data, err)
				}
			}
		})
	}
}

func TestSandboxExecDescriptionNamesUnavailableModules(t *testing.T) {
	for _, module := range []string{"ssl", "sqlite3", "subprocess", "child_process"} {
		if !strings.Contains(SandboxExecDescription, module) {
			t.Errorf("sandbox_exec description omitted unavailable module %s", module)
		}
	}
}
