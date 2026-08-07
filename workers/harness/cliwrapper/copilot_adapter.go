package cliwrapper

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	copilot "github.com/github/copilot-sdk/go"

	"github.com/orka-agents/orka/internal/workerenv"
	"github.com/orka-agents/orka/workers/common"
)

const (
	defaultCopilotMaxTurns              = 50
	defaultCopilotTimeout               = 20 * time.Minute
	copilotPermissionDeniedByToolPolicy = "denied-no-approval-rule-and-could-not-request-from-user"
)

type CopilotAdapter struct {
	config CopilotAdapterConfig
}

func NewCopilotAdapter(config CopilotAdapterConfig) *CopilotAdapter {
	return &CopilotAdapter{config: config}
}

func (a *CopilotAdapter) Name() string { return RuntimeCopilot }

func (a *CopilotAdapter) BuildCommand(_ context.Context, turn TurnContext) (*CommandSpec, error) {
	helperPath := strings.TrimSpace(a.config.HelperPath)
	if helperPath == "" {
		var err error
		helperPath, err = os.Executable()
		if err != nil {
			return nil, fmt.Errorf("resolve copilot helper executable: %w", err)
		}
	}
	dir := firstNonEmpty(turn.WorkDir, a.config.WorkDir)
	if dir == "" {
		dir = DefaultWrapperWorkDir
	}
	if stat, err := os.Stat(dir); err != nil || !stat.IsDir() {
		if err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("stat copilot workspace directory: %w", err)
		}
		if wd, wdErr := os.Getwd(); wdErr == nil {
			dir = wd
		}
	}
	env := append([]string(nil), turn.Env...)
	env = setEnv(env, "HOME", firstNonEmpty(envEntryValue(env, "HOME"), "/home/worker"))
	if cliPath := strings.TrimSpace(a.config.Path); cliPath != "" {
		env = setEnv(env, workerenv.CopilotCLIPath, cliPath)
	}
	return &CommandSpec{
		Path:  helperPath,
		Args:  []string{"copilot-turn"},
		Env:   env,
		Dir:   dir,
		Stdin: []byte(turn.Prompt),
	}, nil
}

func (a *CopilotAdapter) ParseResult(_ context.Context, _ TurnContext, run CommandResult) (TurnResult, error) {
	return TurnResult{Result: run.ExactStdout(), Metadata: map[string]string{"adapter": RuntimeCopilot}}, nil
}

func RunCopilotTurnCLI(ctx context.Context, stdin io.Reader, stdout io.Writer) error {
	cfg, err := common.LoadConfig(defaultCopilotMaxTurns)
	if err != nil {
		return err
	}
	if strings.TrimSpace(cfg.Prompt) == "" && stdin != nil {
		data, readErr := io.ReadAll(io.LimitReader(stdin, int64(maxTerminalResultBytes)+1))
		if readErr != nil {
			return fmt.Errorf("read copilot prompt stdin: %w", readErr)
		}
		if len(data) > maxTerminalResultBytes {
			return fmt.Errorf("copilot prompt exceeds harness terminal frame limit")
		}
		cfg.Prompt = string(data)
	}
	if strings.TrimSpace(cfg.Prompt) == "" {
		return fmt.Errorf("%s is required", workerenv.Prompt)
	}
	workDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("determine copilot workspace directory: %w", err)
	}
	result, err := executeCopilotTurn(ctx, cfg, workDir)
	if err != nil {
		if strings.TrimSpace(result) != "" && stdout != nil {
			_, _ = io.WriteString(stdout, result)
		}
		return err
	}
	if stdout != nil {
		_, _ = io.WriteString(stdout, result)
	}
	return nil
}

func executeCopilotTurn(ctx context.Context, cfg *common.AgentConfig, workDir string) (string, error) {
	execCtx := ctx
	var timeoutCancel context.CancelFunc
	if _, ok := execCtx.Deadline(); !ok {
		timeout := defaultCopilotTimeout
		if cfg.TimeoutSeconds > 0 {
			timeout = time.Duration(cfg.TimeoutSeconds) * time.Second
		}
		execCtx, timeoutCancel = context.WithTimeout(ctx, timeout)
	}
	if timeoutCancel != nil {
		defer timeoutCancel()
	}

	workDir = strings.TrimSpace(workDir)
	if workDir == "" {
		workDir = DefaultWrapperWorkDir
	}
	if cfg.SubPath != "" && filepath.Base(workDir) != filepath.Base(cfg.SubPath) {
		candidate := filepath.Join(workDir, cfg.SubPath)
		if stat, err := os.Stat(candidate); err == nil && stat.IsDir() {
			workDir = candidate
		}
	}

	opts := &copilot.ClientOptions{
		Cwd: workDir,
		Env: sanitizedProcessEnv(os.Environ()),
	}
	if p := strings.TrimSpace(os.Getenv(workerenv.CopilotCLIPath)); p != "" {
		opts.CLIPath = p
	}
	if token := strings.TrimSpace(os.Getenv(workerenv.GitHubToken)); token != "" {
		opts.GithubToken = token
	}
	client := copilot.NewClient(opts)
	if err := client.Start(execCtx); err != nil {
		return "", fmt.Errorf("start copilot client: %w", err)
	}
	defer func() { _ = client.Stop() }()

	session, err := client.CreateSession(execCtx, buildCopilotSessionConfig(cfg, workDir))
	if err != nil {
		return "", fmt.Errorf("create copilot session: %w", err)
	}
	response, err := session.SendAndWait(execCtx, copilot.MessageOptions{Prompt: cfg.Prompt})
	if err != nil {
		return "", fmt.Errorf("send copilot prompt: %w", err)
	}
	result := extractCopilotResult(response)
	return common.EnsureRequiredSecurityArtifacts(
		execCtx,
		cfg,
		result,
		func(followUpCtx context.Context, prompt string) (string, error) {
			followUpResponse, followUpErr := session.SendAndWait(followUpCtx, copilot.MessageOptions{Prompt: prompt})
			if followUpErr != nil {
				return "", followUpErr
			}
			return extractCopilotResult(followUpResponse), nil
		},
	)
}

func buildCopilotSessionConfig(cfg *common.AgentConfig, workDir string) *copilot.SessionConfig {
	if cfg == nil {
		cfg = &common.AgentConfig{MaxTurns: defaultCopilotMaxTurns}
	}
	allowedTools := trimmedTools(cfg.AllowedTools)
	allowedToolsSet := cfg.AllowedToolsSet || len(allowedTools) > 0
	denyAllTools := allowedToolsSet && len(allowedTools) == 0
	sessionCfg := &copilot.SessionConfig{
		Model:            strings.TrimSpace(cfg.Model),
		WorkingDirectory: strings.TrimSpace(workDir),
		OnPermissionRequest: func(
			_ copilot.PermissionRequest,
			_ copilot.PermissionInvocation,
		) (copilot.PermissionRequestResult, error) {
			if denyAllTools {
				return copilot.PermissionRequestResult{Kind: copilotPermissionDeniedByToolPolicy}, nil
			}
			return copilot.PermissionRequestResult{Kind: "approved"}, nil
		},
	}
	if systemPrompt := strings.TrimSpace(cfg.SystemPrompt); systemPrompt != "" {
		sessionCfg.SystemMessage = &copilot.SystemMessageConfig{Mode: "append", Content: systemPrompt}
	}
	if allowedToolsSet {
		sessionCfg.AvailableTools = make([]string, len(allowedTools))
		copy(sessionCfg.AvailableTools, allowedTools)
	}
	if tools := trimmedTools(cfg.DisallowedTools); len(tools) > 0 {
		sessionCfg.ExcludedTools = tools
	}
	if denyAllTools {
		// The SDK omits an empty availableTools array from the wire request.
		// Keep both enforcement points fail-closed so no permission request or
		// tool invocation can turn an explicit empty allowlist into allow-all.
		sessionCfg.Hooks = &copilot.SessionHooks{
			OnPreToolUse: func(
				_ copilot.PreToolUseHookInput,
				_ copilot.HookInvocation,
			) (*copilot.PreToolUseHookOutput, error) {
				return &copilot.PreToolUseHookOutput{
					PermissionDecision:       "deny",
					PermissionDecisionReason: "tool use denied by explicit empty allowlist",
				}, nil
			},
		}
	}
	return sessionCfg
}

func extractCopilotResult(event *copilot.SessionEvent) string {
	if event == nil {
		return ""
	}
	if event.Data.Content != nil && strings.TrimSpace(*event.Data.Content) != "" {
		return *event.Data.Content
	}
	if event.Data.SummaryContent != nil && strings.TrimSpace(*event.Data.SummaryContent) != "" {
		return *event.Data.SummaryContent
	}
	if event.Data.Result != nil {
		if event.Data.Result.DetailedContent != nil && strings.TrimSpace(*event.Data.Result.DetailedContent) != "" {
			return *event.Data.Result.DetailedContent
		}
		if strings.TrimSpace(event.Data.Result.Content) != "" {
			return event.Data.Result.Content
		}
	}
	return ""
}
