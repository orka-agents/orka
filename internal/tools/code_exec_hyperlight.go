/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/orka-agents/orka/internal/hyperlight"
)

const codeExecBackendHyperlight = "hyperlight"

// HyperlightCodeExecutor runs each request in a fresh Hyperlight micro-VM in
// the worker pod: no network, no host files, and no fallback to a weaker
// backend when the pod cannot run one.
type HyperlightCodeExecutor struct {
	runner *hyperlight.Runner
}

var (
	defaultHyperlightRunnerOnce sync.Once
	defaultHyperlightRunner     *hyperlight.Runner
)

func (e *HyperlightCodeExecutor) hyperlightRunner() *hyperlight.Runner {
	if e.runner != nil {
		return e.runner
	}
	defaultHyperlightRunnerOnce.Do(func() {
		defaultHyperlightRunner = hyperlight.NewRunner(hyperlight.ConfigFromEnv())
	})
	return defaultHyperlightRunner
}

// hyperlightRuntimeForLanguage names the guest image for a code_exec language.
func hyperlightRuntimeForLanguage(language string) (string, bool) {
	switch language {
	case codeLanguagePython, python3BinaryName:
		return "python", true
	case codeLanguageJavaScript, codeLanguageNode:
		return "node", true
	case codeLanguageBash, codeLanguageShell:
		return "bash", true
	default:
		return "", false
	}
}

// Execute runs the request with the Hyperlight backend.
func (e *HyperlightCodeExecutor) Execute(ctx context.Context, req CodeExecutionRequest) CodeExecResult {
	start := time.Now()
	result := CodeExecResult{ExitCode: -1}

	if req.OutputLimitBytes <= 0 {
		req.OutputLimitBytes = defaultCodeExecOutputLimitBytes
	}
	if req.Timeout <= 0 {
		req.Timeout = defaultCodeExecTimeout
	}
	if req.Backend == "" {
		req.Backend = codeExecBackendHyperlight
	}
	if err := populateCodeExecRequestResourceAudit(&req); err != nil {
		return CodeExecResult{Error: fmt.Sprintf("failed to configure code execution resources: %v", err), ExitCode: -1}
	}

	ctx, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()
	defer func() {
		auditCodeExec(ctx, req, result, time.Since(start))
	}()

	runtime, ok := hyperlightRuntimeForLanguage(req.Language)
	if !ok {
		result.Error = fmt.Sprintf("unsupported language: %s", req.Language)
		return result
	}
	if runtime == "bash" {
		if msg := checkDenyPatterns(req.Code, req.DenyPatterns); msg != "" {
			result.Error = msg
			return result
		}
	}

	stdout := newCappedBuffer(req.OutputLimitBytes)
	stderr := newCappedBuffer(req.OutputLimitBytes)
	run, err := e.hyperlightRunner().Run(ctx, hyperlight.Request{
		Runtime: runtime,
		Script:  req.Code,
		Stdout:  &codeExecOutputWriter{ctx: ctx, dst: stdout},
		Stderr:  &codeExecOutputWriter{ctx: ctx, dst: stderr},
	})
	result = CodeExecResult{
		Output:          stdout.String(),
		ExitCode:        run.ExitCode,
		OutputTruncated: stdout.Truncated(),
		ErrorTruncated:  stderr.Truncated(),
	}
	if stderr.Len() > 0 || stderr.Truncated() {
		result.Error = stderr.String()
	}
	switch {
	case errors.Is(err, hyperlight.ErrUnavailable):
		result.Error = appendCodeExecError(result.Error, fmt.Sprintf("hyperlight backend unavailable: %v", err))
	case err != nil:
		result.Error = appendCodeExecError(result.Error, err.Error())
	case run.OutputExceeded:
		result.Error = appendCodeExecError(result.Error, fmt.Sprintf("execution stopped: output passed %d bytes", hyperlight.DefaultOutputBudget))
	case run.TimedOut:
		result.TimedOut = true
		result.Error = appendCodeExecError(result.Error, "execution timed out")
	}
	return result
}

func codeExecHyperlightResourceAuditForRequest(req CodeExecutionRequest) map[string]string {
	audit := map[string]string{"network": "none", "host_files": "none"}
	if runtime, ok := hyperlightRuntimeForLanguage(req.Language); ok {
		audit["runtime"] = runtime
		audit["scratch_mb"] = strconv.Itoa((&HyperlightCodeExecutor{}).hyperlightRunner().ScratchMB(runtime))
	}
	return audit
}

var _ CodeExecutor = (*HyperlightCodeExecutor)(nil)
