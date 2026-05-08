/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/sozercan/orka/internal/workerenv"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	codeExecToolName = "code_exec"

	codeExecBackendEnv        = workerenv.CodeExecBackend
	codeExecBackendInProcess  = "in-process"
	codeExecBackendKubernetes = "kubernetes"

	defaultCodeExecTimeout          = 30 * time.Second
	maxCodeExecTimeoutSeconds       = 60
	defaultCodeExecOutputLimitBytes = 64 * 1024

	codeExecLocalCPUSecondsEnv   = workerenv.CodeExecLocalCPUSeconds
	codeExecLocalMemoryKBEnv     = workerenv.CodeExecLocalMemoryKB
	codeExecLocalMaxProcessesEnv = workerenv.CodeExecLocalMaxProcesses

	defaultCodeExecLocalCPUSeconds   = int64(maxCodeExecTimeoutSeconds)
	defaultCodeExecLocalMemoryKB     = int64(4 * 1024 * 1024)
	defaultCodeExecLocalMaxProcesses = int64(1024)
)

// denyPattern pairs a compiled regex with a human-readable description
type denyPattern struct {
	re   *regexp.Regexp
	desc string
}

// defaultDenyPatterns blocks dangerous shell commands in bash/sh execution
var defaultDenyPatterns = []denyPattern{
	{regexp.MustCompile(`\brm\s+-[rf]{1,2}\b`), "destructive rm command"},
	{regexp.MustCompile(`\bdel\s+/[fq]\b`), "destructive del command"},
	{regexp.MustCompile(`\brmdir\s+/s\b`), "destructive rmdir command"},
	{regexp.MustCompile(`\b(format|mkfs|diskpart)\b`), "disk format command"},
	{regexp.MustCompile(`\bdd\s+if=`), "raw disk write (dd)"},
	{regexp.MustCompile(`>\s*/dev/sd[a-z]\b`), "write to block device"},
	{regexp.MustCompile(`\b(shutdown|reboot|poweroff)\b`), "system shutdown/reboot"},
	{regexp.MustCompile(`:\(\)\s*\{.*\};\s*:`), "fork bomb"},
}

// CodeExecutor is the execution backend used by CodeExecTool.
type CodeExecutor interface {
	Execute(ctx context.Context, req CodeExecutionRequest) CodeExecResult
}

// CodeExecutionRequest contains a validated code execution request for a backend.
type CodeExecutionRequest struct {
	Backend          string
	Language         string
	Code             string
	Timeout          time.Duration
	WorkDir          string
	DenyPatterns     []denyPattern
	OutputLimitBytes int64
	ResourceAudit    map[string]string
	Tenant           string
	Provider         string
	ProviderType     string
}

// CodeExecTool implements code execution functionality.
type CodeExecTool struct {
	workDir          string
	timeout          time.Duration
	allowedLangs     map[string]bool
	denyPatterns     []denyPattern
	executor         CodeExecutor
	backend          string
	outputLimitBytes int64
}

// CodeExecArgs are the arguments for the code execution tool.
type CodeExecArgs struct {
	Language string `json:"language"`
	Code     string `json:"code"`
	Timeout  int    `json:"timeout,omitempty"` // Timeout in seconds
}

// CodeExecResult represents the execution result.
type CodeExecResult struct {
	Output          string `json:"output"`
	Error           string `json:"error,omitempty"`
	ExitCode        int    `json:"exit_code"`
	TimedOut        bool   `json:"timed_out,omitempty"`
	OutputTruncated bool   `json:"output_truncated,omitempty"`
	ErrorTruncated  bool   `json:"error_truncated,omitempty"`
}

// InProcessCodeExecutor runs code on the current worker host with local process hardening.
type InProcessCodeExecutor struct{}

type unsupportedCodeExecutor struct {
	backend string
}

// NewCodeExecTool creates a new code execution tool.
func NewCodeExecTool() *CodeExecTool {
	workDir := os.Getenv(workerenv.WorkDir)
	if workDir == "" {
		workDir = "/tmp/orka-exec"
	}

	executor, backend := newCodeExecutorFromBackend(os.Getenv(codeExecBackendEnv))

	return &CodeExecTool{
		workDir:          workDir,
		timeout:          defaultCodeExecTimeout,
		allowedLangs:     defaultCodeExecAllowedLangs(),
		denyPatterns:     defaultDenyPatterns,
		executor:         executor,
		backend:          backend,
		outputLimitBytes: defaultCodeExecOutputLimitBytes,
	}
}

func defaultCodeExecAllowedLangs() map[string]bool {
	return map[string]bool{codeLanguagePython: true, python3BinaryName: true, codeLanguageJavaScript: true, codeLanguageNode: true, codeLanguageBash: true,
		codeLanguageShell: true,
	}
}

func newCodeExecutorFromBackend(backend string) (CodeExecutor, string) {
	switch normalizeCodeExecBackend(backend) {
	case codeExecBackendKubernetes:
		return &KubernetesJobCodeExecutor{}, codeExecBackendKubernetes
	case codeExecBackendInProcess:
		return &InProcessCodeExecutor{}, codeExecBackendInProcess
	default:
		return &unsupportedCodeExecutor{backend: backend}, backend
	}
}

func normalizeCodeExecBackend(backend string) string {
	switch strings.ToLower(strings.TrimSpace(backend)) {
	case "", "kubernetes", "k8s", jobField:
		return codeExecBackendKubernetes
	case "in-process", "in_process", "inprocess", "local":
		return codeExecBackendInProcess
	default:
		return strings.ToLower(strings.TrimSpace(backend))
	}
}

// Name returns the tool name.
func (t *CodeExecTool) Name() string {
	return codeExecToolName
}

// Description returns the tool description.
func (t *CodeExecTool) Description() string {
	return "Execute code in a sandboxed environment. Supports Python, JavaScript (Node.js), and Bash."
}

// Parameters returns the JSON Schema for parameters.
func (t *CodeExecTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"language": {
				"type": "string",
				"description": "Programming language (python, javascript, bash)",
				"enum": ["python", "python3", "javascript", "node", "bash", "sh"]
			},
			"code": {
				"type": "string",
				"description": "The code to execute"
			},
			"timeout": {
				"type": "integer",
				"description": "Execution timeout in seconds (default: 30, max: 60)",
				"default": 30
			}
		},
		"required": ["language", "code"]
	}`)
}

// Execute runs the code.
func (t *CodeExecTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var execArgs CodeExecArgs
	if err := json.Unmarshal(args, &execArgs); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	if execArgs.Code == "" {
		return "", fmt.Errorf("code is required")
	}

	lang := strings.ToLower(execArgs.Language)
	if !t.allowedLangs[lang] {
		return "", fmt.Errorf("unsupported language: %s", execArgs.Language)
	}

	timeout := t.timeout
	if timeout <= 0 {
		timeout = defaultCodeExecTimeout
	}
	if execArgs.Timeout > 0 && execArgs.Timeout <= maxCodeExecTimeoutSeconds {
		timeout = time.Duration(execArgs.Timeout) * time.Second
	}

	tenant, provider, providerType := codeExecScopeFromContext(ctx)
	backend := t.resolveCodeExecBackend(provider, providerType, tenant)
	executor := t.executor
	if executor == nil || normalizeCodeExecBackend(t.backend) != backend {
		var normalizedBackend string
		executor, normalizedBackend = newCodeExecutorFromBackend(backend)
		backend = normalizedBackend
	}

	if backend == codeExecBackendInProcess {
		if err := os.MkdirAll(t.workDir, 0755); err != nil {
			return "", fmt.Errorf("failed to create work directory: %w", err)
		}
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	result := executor.Execute(ctx, CodeExecutionRequest{
		Backend:          backend,
		Language:         lang,
		Code:             execArgs.Code,
		Timeout:          timeout,
		WorkDir:          t.workDir,
		DenyPatterns:     t.denyPatterns,
		OutputLimitBytes: t.codeExecOutputLimitBytes(),
		Tenant:           tenant,
		Provider:         provider,
		ProviderType:     providerType,
	})

	output, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return "", err
	}

	return string(output), nil
}

func (t *CodeExecTool) resolveCodeExecBackend(provider, providerType, tenant string) string {
	fallback := codeExecBackendKubernetes
	if t.backend != "" {
		fallback = t.backend
	}
	return normalizeCodeExecBackend(codeExecScopedEnv(codeExecBackendEnv, codeExecEffectiveProviderScope(provider, providerType), tenant, fallback))
}

func (t *CodeExecTool) codeExecOutputLimitBytes() int64 {
	if t.outputLimitBytes > 0 {
		return t.outputLimitBytes
	}
	return defaultCodeExecOutputLimitBytes
}

// Execute runs the request with the in-process backend.
func (e *InProcessCodeExecutor) Execute(ctx context.Context, req CodeExecutionRequest) CodeExecResult {
	start := time.Now()
	result := CodeExecResult{ExitCode: -1}

	if req.WorkDir == "" {
		req.WorkDir = "/tmp/orka-exec"
	}
	if req.OutputLimitBytes <= 0 {
		req.OutputLimitBytes = defaultCodeExecOutputLimitBytes
	}
	if req.Timeout <= 0 {
		req.Timeout = defaultCodeExecTimeout
	}
	if req.Backend == "" {
		req.Backend = codeExecBackendInProcess
	}
	if len(req.ResourceAudit) == 0 {
		req.ResourceAudit = codeExecLocalResourceAuditForRequest(req)
	}
	if err := os.MkdirAll(req.WorkDir, 0755); err != nil {
		return CodeExecResult{Error: fmt.Sprintf("failed to create work directory: %v", err), ExitCode: -1}
	}

	execCtx, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()
	ctx = execCtx

	defer func() {
		auditCodeExec(ctx, req, result, time.Since(start))
	}()

	switch req.Language {
	case codeLanguagePython, python3BinaryName:
		result = e.executePython(ctx, req)
	case codeLanguageJavaScript, codeLanguageNode:
		result = e.executeNode(ctx, req)
	case codeLanguageBash, codeLanguageShell:
		result = e.executeShell(ctx, req)
	default:
		result.Error = fmt.Sprintf("unsupported language: %s", req.Language)
	}

	return result
}

func (e *unsupportedCodeExecutor) Execute(ctx context.Context, req CodeExecutionRequest) CodeExecResult {
	start := time.Now()
	if req.Backend == "" {
		req.Backend = e.backend
	}
	result := CodeExecResult{
		Error:    fmt.Sprintf("unsupported code_exec backend: %s", e.backend),
		ExitCode: -1,
	}
	auditCodeExec(ctx, req, result, time.Since(start))
	return result
}

// executePython executes Python code.
func (e *InProcessCodeExecutor) executePython(ctx context.Context, req CodeExecutionRequest) CodeExecResult {
	tmpPath, result, ok := writeCodeExecTempFile(req.WorkDir, "script-*.py", req.Code, 0600)
	if !ok {
		return result
	}
	defer os.Remove(tmpPath) //nolint:errcheck

	cmd := newLimitedCodeExecCommand(ctx, req, python3BinaryName, tmpPath)
	return e.runCommand(ctx, cmd, req.WorkDir, req.OutputLimitBytes)
}

// executeNode executes JavaScript code.
func (e *InProcessCodeExecutor) executeNode(ctx context.Context, req CodeExecutionRequest) CodeExecResult {
	tmpPath, result, ok := writeCodeExecTempFile(req.WorkDir, "script-*.js", req.Code, 0600)
	if !ok {
		return result
	}
	defer os.Remove(tmpPath) //nolint:errcheck

	cmd := newLimitedCodeExecCommand(ctx, req, codeLanguageNode, tmpPath)
	return e.runCommand(ctx, cmd, req.WorkDir, req.OutputLimitBytes)
}

// executeShell executes Bash or POSIX shell code.
func (e *InProcessCodeExecutor) executeShell(ctx context.Context, req CodeExecutionRequest) CodeExecResult {
	if msg := checkDenyPatterns(req.Code, req.DenyPatterns); msg != "" {
		return CodeExecResult{Error: msg, ExitCode: -1}
	}

	tmpPath, result, ok := writeCodeExecTempFile(req.WorkDir, "script-*.sh", req.Code, 0700)
	if !ok {
		return result
	}
	defer os.Remove(tmpPath) //nolint:errcheck

	shell := codeLanguageBash
	if req.Language == codeLanguageShell {
		shell = codeLanguageShell
	}
	cmd := newLimitedCodeExecCommand(ctx, req, shell, tmpPath)
	return e.runCommand(ctx, cmd, req.WorkDir, req.OutputLimitBytes)
}

func writeCodeExecTempFile(workDir, pattern, code string, mode os.FileMode) (string, CodeExecResult, bool) {
	tmpFile, err := os.CreateTemp(workDir, pattern)
	if err != nil {
		return "", CodeExecResult{Error: fmt.Sprintf("failed to create temp script: %v", err), ExitCode: -1}, false
	}
	tmpPath := tmpFile.Name()
	if _, err := tmpFile.Write([]byte(code)); err != nil {
		tmpFile.Close()    //nolint:errcheck
		os.Remove(tmpPath) //nolint:errcheck
		return "", CodeExecResult{Error: fmt.Sprintf("failed to write script: %v", err), ExitCode: -1}, false
	}
	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpPath) //nolint:errcheck
		return "", CodeExecResult{Error: fmt.Sprintf("failed to close script: %v", err), ExitCode: -1}, false
	}
	if err := os.Chmod(tmpPath, mode); err != nil {
		os.Remove(tmpPath) //nolint:errcheck
		return "", CodeExecResult{Error: fmt.Sprintf("failed to chmod script: %v", err), ExitCode: -1}, false
	}
	return tmpPath, CodeExecResult{}, true
}

func checkDenyPatterns(code string, patterns []denyPattern) string {
	for _, p := range patterns {
		if p.re.MatchString(code) {
			return fmt.Sprintf("command blocked by safety guard: %s", p.desc)
		}
	}
	return ""
}

// runCommand executes a command and captures output.
func (t *CodeExecTool) runCommand(cmd *exec.Cmd) CodeExecResult {
	return (&InProcessCodeExecutor{}).runCommand(context.Background(), cmd, t.workDir, t.codeExecOutputLimitBytes())
}

func (e *InProcessCodeExecutor) runCommand(ctx context.Context, cmd *exec.Cmd, workDir string, outputLimitBytes int64) CodeExecResult {
	stdout := newCappedBuffer(outputLimitBytes)
	stderr := newCappedBuffer(outputLimitBytes)
	cmd.Stdout = &codeExecOutputWriter{ctx: ctx, dst: stdout}
	cmd.Stderr = &codeExecOutputWriter{ctx: ctx, dst: stderr}
	cmd.Stdin = bytes.NewReader(nil)
	cmd.Env = scrubCodeExecEnv(os.Environ())
	if workDir != "" {
		cmd.Dir = workDir
	}
	applyCodeExecPlatformHardening(cmd)

	err := cmd.Run()

	result := CodeExecResult{
		Output:          stdout.String(),
		ExitCode:        0,
		OutputTruncated: stdout.Truncated(),
		ErrorTruncated:  stderr.Truncated(),
	}

	if stderr.Len() > 0 || stderr.Truncated() {
		result.Error = stderr.String()
	}

	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			result.TimedOut = true
			result.ExitCode = -1
			result.Error = appendCodeExecError(result.Error, "execution timed out")
		} else if exitErr, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
		} else {
			result.ExitCode = -1
			if result.Error == "" {
				result.Error = err.Error()
			}
		}
	} else if ctx.Err() == context.DeadlineExceeded {
		result.TimedOut = true
		result.ExitCode = -1
		result.Error = appendCodeExecError(result.Error, "execution timed out")
	}

	return result
}

type codeExecOutputWriter struct {
	ctx context.Context
	dst *cappedBuffer
}

func (w *codeExecOutputWriter) Write(p []byte) (int, error) {
	if w.ctx.Err() != nil {
		return len(p), nil
	}
	return w.dst.Write(p)
}

type codeExecLocalLimits struct {
	CPUSeconds   int64
	MemoryKB     int64
	MaxProcesses int64
}

func codeExecLocalLimitsForRequest(req CodeExecutionRequest) codeExecLocalLimits {
	return codeExecLocalLimitsForScope(codeExecRequestProviderScope(req), req.Tenant)
}

func codeExecLocalLimitsForScope(provider, tenant string) codeExecLocalLimits {
	return codeExecLocalLimits{
		CPUSeconds:   codeExecScopedEnvInt64(codeExecLocalCPUSecondsEnv, provider, tenant, defaultCodeExecLocalCPUSeconds),
		MemoryKB:     codeExecScopedEnvInt64(codeExecLocalMemoryKBEnv, provider, tenant, defaultCodeExecLocalMemoryKB),
		MaxProcesses: codeExecScopedEnvInt64(codeExecLocalMaxProcessesEnv, provider, tenant, defaultCodeExecLocalMaxProcesses),
	}
}

func codeExecLocalResourceAuditForRequest(req CodeExecutionRequest) map[string]string {
	return codeExecLocalResourceAuditForScope(codeExecRequestProviderScope(req), req.Tenant)
}

func codeExecLocalResourceAuditForScope(provider, tenant string) map[string]string {
	limits := codeExecLocalLimitsForScope(provider, tenant)
	return map[string]string{
		"cpu_seconds_limit": fmt.Sprintf("%d", limits.CPUSeconds),
		"memory_kb_limit":   fmt.Sprintf("%d", limits.MemoryKB),
		"max_processes":     fmt.Sprintf("%d", limits.MaxProcesses),
	}
}

func codeExecScopedEnvInt64(name, provider, tenant string, fallback int64) int64 {
	value := codeExecScopedEnv(name, provider, tenant, "")
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func codeExecScopedEnv(name, provider, tenant, fallback string) string {
	value, _ := codeExecScopedEnvValue(name, provider, tenant)
	if value != "" {
		return value
	}
	return fallback
}

func codeExecScopedEnvValue(name, provider, tenant string) (string, string) {
	for _, envName := range codeExecScopedEnvNames(name, provider, tenant) {
		if value := strings.TrimSpace(os.Getenv(envName)); value != "" {
			return value, envName
		}
	}
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value, name
	}
	return "", ""
}

func codeExecScopedEnvNames(name, provider, tenant string) []string {
	provider = codeExecScopeSuffix(provider)
	tenant = codeExecScopeSuffix(tenant)
	names := make([]string, 0, 3)
	if provider != "" && tenant != "" {
		names = append(names, name+"_PROVIDER_"+provider+"_TENANT_"+tenant)
	}
	if tenant != "" {
		names = append(names, name+"_TENANT_"+tenant)
	}
	if provider != "" {
		names = append(names, name+"_PROVIDER_"+provider)
	}
	return names
}

func codeExecScopeSuffix(value string) string {
	var b strings.Builder
	lastUnderscore := false
	for _, r := range strings.TrimSpace(value) {
		var out rune
		switch {
		case r >= 'a' && r <= 'z':
			out = r - 'a' + 'A'
		case (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
			out = r
		default:
			out = '_'
		}
		if out == '_' {
			if b.Len() == 0 || lastUnderscore {
				continue
			}
			lastUnderscore = true
			b.WriteByte('_')
			continue
		}
		lastUnderscore = false
		b.WriteRune(out)
	}
	return strings.TrimRight(b.String(), "_")
}

func codeExecScopeFromContext(ctx context.Context) (tenant, provider, providerType string) {
	if tc := GetToolContext(ctx); tc != nil {
		tenant = strings.TrimSpace(tc.Tenant)
		if tenant == "" {
			tenant = strings.TrimSpace(tc.Namespace)
		}
		provider = strings.TrimSpace(tc.Provider)
		providerType = strings.TrimSpace(tc.ProviderType)
	}
	return tenant, provider, providerType
}

func codeExecRequestProviderScope(req CodeExecutionRequest) string {
	return codeExecEffectiveProviderScope(req.Provider, req.ProviderType)
}

func codeExecEffectiveProviderScope(provider, providerType string) string {
	if provider = strings.TrimSpace(provider); provider != "" {
		return provider
	}
	return strings.TrimSpace(providerType)
}

func appendCodeExecError(current, msg string) string {
	if current == "" {
		return msg
	}
	if strings.HasSuffix(current, "\n") {
		return current + msg
	}
	return current + "\n" + msg
}

type cappedBuffer struct {
	buf       bytes.Buffer
	limit     int64
	total     int64
	truncated bool
}

func newCappedBuffer(limit int64) *cappedBuffer {
	if limit <= 0 {
		limit = defaultCodeExecOutputLimitBytes
	}
	return &cappedBuffer{limit: limit}
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	b.total += int64(len(p))
	remaining := b.limit - int64(b.buf.Len())
	if remaining > 0 {
		toWrite := min(int64(len(p)), remaining)
		_, _ = b.buf.Write(p[:toWrite])
	}
	if int64(b.buf.Len()) < b.total {
		b.truncated = true
	}
	return len(p), nil
}

func (b *cappedBuffer) String() string {
	if !b.truncated {
		return b.buf.String()
	}
	return b.buf.String() + fmt.Sprintf("\n[truncated after %d bytes; %d bytes omitted]", b.limit, b.total-int64(b.buf.Len()))
}

func (b *cappedBuffer) Len() int {
	return b.buf.Len()
}

func (b *cappedBuffer) Truncated() bool {
	return b.truncated
}

func (b *cappedBuffer) Total() int64 {
	return b.total
}

const safeCodeExecPath = "/usr/local/bin:/usr/bin:/bin:/opt/homebrew/bin"

func scrubCodeExecEnv(environ []string) []string {
	scrubbed := []string{
		"PATH=" + safeCodeExecPath,
		"HOME=/tmp",
		"TMPDIR=/tmp",
		"TMP=/tmp",
		"TEMP=/tmp",
		"LANG=C.UTF-8",
		"LC_ALL=C.UTF-8",
		"LC_CTYPE=C.UTF-8",
		"TERM=dumb",
	}

	for _, name := range []string{"SystemRoot", "SYSTEMROOT", "WINDIR", "windir", "COMSPEC", "PATHEXT"} {
		if value, ok := lookupEnvValue(environ, name); ok {
			scrubbed = append(scrubbed, name+"="+value)
		}
	}

	return scrubbed
}

func lookupEnvValue(environ []string, name string) (string, bool) {
	for _, kv := range environ {
		key, value, ok := strings.Cut(kv, "=")
		if ok && key == name {
			return value, true
		}
	}
	return "", false
}

func auditCodeExec(ctx context.Context, req CodeExecutionRequest, result CodeExecResult, duration time.Duration) {
	codeHash := sha256.Sum256([]byte(req.Code))
	keysAndValues := []any{
		"backend", req.Backend,
		"language", req.Language,
		"code_sha256", fmt.Sprintf("%x", codeHash),
		"code_bytes", len(req.Code),
		"stdout_bytes", len(result.Output),
		"stderr_bytes", len(result.Error),
		"stdout_truncated", result.OutputTruncated,
		"stderr_truncated", result.ErrorTruncated,
		"duration_ms", duration.Milliseconds(),
		"exit_code", result.ExitCode,
		"timeout_ms", req.Timeout.Milliseconds(),
		"timed_out", result.TimedOut,
	}

	tenant := strings.TrimSpace(req.Tenant)
	provider := strings.TrimSpace(req.Provider)
	providerType := strings.TrimSpace(req.ProviderType)
	if tc := GetToolContext(ctx); tc != nil {
		if tenant == "" {
			tenant = strings.TrimSpace(tc.Tenant)
		}
		if tenant == "" {
			tenant = strings.TrimSpace(tc.Namespace)
		}
		if provider == "" {
			provider = strings.TrimSpace(tc.Provider)
		}
		if providerType == "" {
			providerType = strings.TrimSpace(tc.ProviderType)
		}
		keysAndValues = append(keysAndValues,
			"session_id", tc.SessionID,
			"task_id", tc.TaskID,
			"tool_call_id", tc.ToolCallID,
		)
	}
	if tenant != "" {
		keysAndValues = append(keysAndValues, "tenant", tenant)
	}
	if provider != "" {
		keysAndValues = append(keysAndValues, "provider", provider)
	}
	if providerType != "" {
		keysAndValues = append(keysAndValues, "provider_type", providerType)
	}

	if len(req.ResourceAudit) > 0 {
		keys := make([]string, 0, len(req.ResourceAudit))
		for key := range req.ResourceAudit {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			keysAndValues = append(keysAndValues, key, req.ResourceAudit[key])
		}
	}

	logger := log.FromContext(ctx)
	logger.Info("code_exec audit", keysAndValues...)
}

// Ensure CodeExecTool implements Tool.
var _ Tool = (*CodeExecTool)(nil)

// Ensure executor implementations satisfy CodeExecutor.
var _ CodeExecutor = (*InProcessCodeExecutor)(nil)
var _ CodeExecutor = (*unsupportedCodeExecutor)(nil)
