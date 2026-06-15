package cliwrapper

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type GenericAdapter struct {
	config GenericAdapterConfig
}

func NewGenericAdapter(config GenericAdapterConfig) *GenericAdapter {
	if strings.TrimSpace(config.PromptMode) == "" {
		config.PromptMode = PromptModeStdin
	}
	if strings.TrimSpace(config.PromptEnv) == "" {
		config.PromptEnv = DefaultPromptEnv
	}
	if strings.TrimSpace(config.ResultMode) == "" {
		config.ResultMode = ResultModeStdout
	}
	return &GenericAdapter{config: config}
}

func (a *GenericAdapter) Name() string { return RuntimeGeneric }

func (a *GenericAdapter) Validate() error {
	if a == nil {
		return fmt.Errorf("generic adapter is required")
	}
	if strings.TrimSpace(a.config.Command) == "" {
		return fmt.Errorf("generic runtime requires %s or --command", EnvCommand)
	}
	switch strings.ToLower(strings.TrimSpace(a.config.PromptMode)) {
	case PromptModeStdin, PromptModeEnv, PromptModeFile:
	default:
		return fmt.Errorf("unsupported generic prompt mode %q", a.config.PromptMode)
	}
	switch strings.ToLower(strings.TrimSpace(a.config.ResultMode)) {
	case ResultModeStdout, ResultModeFile:
	default:
		return fmt.Errorf("unsupported generic result mode %q", a.config.ResultMode)
	}
	if strings.ToLower(strings.TrimSpace(a.config.ResultMode)) == ResultModeFile &&
		strings.TrimSpace(a.config.ResultFile) == "" {
		return fmt.Errorf("generic result mode file requires %s or --result-file", EnvResultFile)
	}
	return nil
}

func (a *GenericAdapter) BuildCommand(_ context.Context, turn TurnContext) (*CommandSpec, error) {
	if err := a.Validate(); err != nil {
		return nil, err
	}
	cfg := a.config
	dir := firstNonEmpty(turn.WorkDir, cfg.WorkDir)
	spec := &CommandSpec{
		Path: cfg.Command,
		Args: append([]string(nil), cfg.Args...),
		Env:  append(append([]string(nil), turn.Env...), cfg.Env...),
		Dir:  dir,
	}

	switch strings.ToLower(strings.TrimSpace(cfg.PromptMode)) {
	case PromptModeStdin:
		spec.Stdin = []byte(turn.Prompt)
	case PromptModeEnv:
		spec.Env = setEnv(spec.Env, firstNonEmpty(cfg.PromptEnv, DefaultPromptEnv), turn.Prompt)
	case PromptModeFile:
		path := strings.TrimSpace(cfg.PromptFile)
		if path == "" {
			f, err := os.CreateTemp("", "orka-turn-prompt-*.txt")
			if err != nil {
				return nil, fmt.Errorf("create prompt temp file: %w", err)
			}
			path = f.Name()
			if err := f.Close(); err != nil {
				_ = os.Remove(path)
				return nil, fmt.Errorf("close prompt temp file: %w", err)
			}
			spec.TempFiles = append(spec.TempFiles, path)
		} else if !filepath.IsAbs(path) && dir != "" {
			path = filepath.Join(dir, path)
		}
		if err := os.WriteFile(path, []byte(turn.Prompt), 0o600); err != nil {
			return nil, fmt.Errorf("write prompt file: %w", err)
		}
		spec.Env = setEnv(spec.Env, firstNonEmpty(cfg.PromptEnv, DefaultPromptEnv), path)
	}

	if strings.ToLower(strings.TrimSpace(cfg.ResultMode)) == ResultModeFile {
		resultFile := cfg.ResultFile
		if !filepath.IsAbs(resultFile) && dir != "" {
			resultFile = filepath.Join(dir, resultFile)
		}
		_ = os.Remove(resultFile)
		spec.ResultFile = resultFile
	}
	return spec, nil
}

func (a *GenericAdapter) ParseResult(_ context.Context, _ TurnContext, run CommandResult) (TurnResult, error) {
	result := run.Stdout
	if strings.ToLower(strings.TrimSpace(a.config.ResultMode)) == ResultModeFile {
		path := strings.TrimSpace(run.ResultFile)
		if path == "" {
			path = strings.TrimSpace(a.config.ResultFile)
			if path != "" && !filepath.IsAbs(path) && strings.TrimSpace(a.config.WorkDir) != "" {
				path = filepath.Join(a.config.WorkDir, path)
			}
		}
		if path == "" {
			return TurnResult{}, fmt.Errorf("result file path is required")
		}
		data, err := readBoundedResultFile(path)
		if err != nil {
			return TurnResult{Result: result}, err
		}
		result = data
	}
	return TurnResult{Result: result}, nil
}

func readBoundedResultFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("read result file: %w", err)
	}
	defer file.Close() //nolint:errcheck
	data, err := io.ReadAll(io.LimitReader(file, int64(maxTerminalResultBytes)+1))
	if err != nil {
		return "", fmt.Errorf("read result file: %w", err)
	}
	if len(data) > maxTerminalResultBytes {
		return "", fmt.Errorf("result file exceeds harness terminal frame limit")
	}
	return string(data), nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func setEnv(env []string, key, value string) []string {
	key = strings.TrimSpace(key)
	if key == "" {
		return env
	}
	prefix := key + "="
	for i, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}
