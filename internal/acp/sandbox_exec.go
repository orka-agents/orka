/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package acp

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// SandboxExecToolName is the runtime-local tool that runs a script in a fresh
// Hyperlight micro-VM in the runtime Pod, with the session workspace mounted.
// With the provider's own shell turned off (allowBash: false), it is the only
// way an agent runs a command.
const SandboxExecToolName = "sandbox_exec"

// SandboxExecWorkspacePath is where the session workspace is mounted in the
// micro-VM; scripts start there.
const SandboxExecWorkspacePath = "/workspace"

// sandboxExecShell is the default language and the shell's micro-VM image.
const sandboxExecShell = "bash"

const (
	SandboxExecDefaultTimeout   = 120 * time.Second
	SandboxExecMaxTimeout       = 600 * time.Second
	SandboxExecOutputLimitBytes = 64 << 10
)

// SandboxExecDescription is the tool description the agent sees.
const SandboxExecDescription = "Run a bash script, Python or JavaScript in a fresh, isolated micro-VM. " +
	"The session workspace is mounted read-write at /workspace, the working directory. " +
	"The VM has no network and nothing else from the host, and each call starts clean: " +
	"only files written under /workspace persist. The shell is BusyBox; Python is CPython " +
	"with its standard library; JavaScript is Node.js. Returns stdout, stderr and the exit code."

// SandboxExecInputSchema is the tool's JSON Schema. Execute enforces the same
// limits; the schema only guides the model.
const SandboxExecInputSchema = `{"type":"object","properties":{` +
	`"language":{"type":"string","enum":["bash","python","javascript"],"default":"bash","description":"Interpreter for the code"},` +
	`"code":{"type":"string","minLength":1,"description":"The script to run, starting in /workspace"},` +
	`"timeout":{"type":"integer","minimum":1,"maximum":600,"default":120,"description":"Timeout in seconds"}},` +
	`"required":["code"],"additionalProperties":false}`

// SandboxExecRequest is a validated sandbox_exec call.
type SandboxExecRequest struct {
	// Runtime is the micro-VM image: bash, python or node.
	Runtime string
	// Script is the code, prefixed to start in the workspace.
	Script  string
	Timeout time.Duration
}

// SandboxExecResult is the tool's result, returned to the agent as JSON.
type SandboxExecResult struct {
	Stdout          string `json:"stdout"`
	Stderr          string `json:"stderr,omitempty"`
	ExitCode        int    `json:"exit_code"`
	TimedOut        bool   `json:"timed_out,omitempty"`
	StdoutTruncated bool   `json:"stdout_truncated,omitempty"`
	StderrTruncated bool   `json:"stderr_truncated,omitempty"`
}

// ParseSandboxExecArguments validates the arguments of a sandbox_exec call.
// A model may send the timeout as a number or a numeric string.
func ParseSandboxExecArguments(raw json.RawMessage) (SandboxExecRequest, error) {
	var args map[string]any
	if err := json.Unmarshal(raw, &args); err != nil || args == nil {
		return SandboxExecRequest{}, fmt.Errorf("sandbox_exec arguments must be a JSON object")
	}
	for key := range args {
		switch key {
		case "language", "code", "timeout":
		default:
			return SandboxExecRequest{}, fmt.Errorf("sandbox_exec does not take %q", key)
		}
	}
	code, _ := args["code"].(string)
	if strings.TrimSpace(code) == "" {
		return SandboxExecRequest{}, fmt.Errorf("sandbox_exec requires code")
	}
	language := sandboxExecShell
	if value, present := args["language"]; present {
		text, ok := value.(string)
		if !ok {
			return SandboxExecRequest{}, fmt.Errorf("sandbox_exec language must be a string")
		}
		language = strings.ToLower(strings.TrimSpace(text))
	}
	timeout := SandboxExecDefaultTimeout
	if value, present := args["timeout"]; present {
		seconds, err := sandboxExecSeconds(value)
		if err != nil {
			return SandboxExecRequest{}, err
		}
		timeout = min(time.Duration(seconds)*time.Second, SandboxExecMaxTimeout)
	}

	request := SandboxExecRequest{Timeout: timeout}
	switch language {
	case sandboxExecShell, "sh":
		request.Runtime = sandboxExecShell
		request.Script = "cd " + SandboxExecWorkspacePath + " || exit 125\n" + code
	case "python", "python3":
		request.Runtime = "python"
		request.Script = "import os as _orka_os; _orka_os.chdir(" + strconv.Quote(SandboxExecWorkspacePath) + "); del _orka_os\n" + code
	case "javascript", "node", "js":
		request.Runtime = "node"
		request.Script = "process.chdir(" + strconv.Quote(SandboxExecWorkspacePath) + ");\n" + code
	default:
		return SandboxExecRequest{}, fmt.Errorf("sandbox_exec language must be bash, python or javascript, not %q", language)
	}
	return request, nil
}

func sandboxExecSeconds(value any) (int64, error) {
	var seconds float64
	switch typed := value.(type) {
	case float64:
		seconds = typed
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		if err != nil {
			return 0, fmt.Errorf("sandbox_exec timeout must be a number of seconds")
		}
		seconds = parsed
	default:
		return 0, fmt.Errorf("sandbox_exec timeout must be a number of seconds")
	}
	if math.IsNaN(seconds) || seconds < 1 || seconds != math.Trunc(seconds) {
		return 0, fmt.Errorf("sandbox_exec timeout must be a whole number of seconds, at least 1")
	}
	return int64(min(seconds, SandboxExecMaxTimeout.Seconds())), nil
}
