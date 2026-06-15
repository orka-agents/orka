package cliwrapper

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	EnvListenAddr           = "ORKA_HARNESS_WRAPPER_LISTEN_ADDR"
	EnvRuntime              = "ORKA_HARNESS_WRAPPER_RUNTIME"
	EnvWorkDir              = "ORKA_HARNESS_WRAPPER_WORKDIR"
	EnvCommand              = "ORKA_HARNESS_WRAPPER_COMMAND"
	EnvCommandArgs          = "ORKA_HARNESS_WRAPPER_ARGS"
	EnvCommandEnv           = "ORKA_HARNESS_WRAPPER_ENV"
	EnvPromptMode           = "ORKA_HARNESS_WRAPPER_PROMPT_MODE"
	EnvPromptEnv            = "ORKA_HARNESS_WRAPPER_PROMPT_ENV"
	EnvPromptFile           = "ORKA_HARNESS_WRAPPER_PROMPT_FILE"
	EnvResultMode           = "ORKA_HARNESS_WRAPPER_RESULT_MODE"
	EnvResultFile           = "ORKA_HARNESS_WRAPPER_RESULT_FILE"
	EnvStdoutLimitBytes     = "ORKA_HARNESS_WRAPPER_STDOUT_LIMIT_BYTES"
	EnvStderrLimitBytes     = "ORKA_HARNESS_WRAPPER_STDERR_LIMIT_BYTES"
	EnvCancelGrace          = "ORKA_HARNESS_WRAPPER_CANCEL_GRACE_PERIOD"
	EnvAuthValue            = "ORKA_HARNESS_WRAPPER_BEARER_TOKEN"
	EnvAuthValueFile        = "ORKA_HARNESS_WRAPPER_BEARER_TOKEN_FILE"
	EnvAllowUnauthenticated = "ORKA_HARNESS_WRAPPER_ALLOW_UNAUTHENTICATED"
	EnvTurnRetention        = "ORKA_HARNESS_WRAPPER_TURN_RETENTION"
	EnvCopilotCLIPath       = "ORKA_HARNESS_WRAPPER_COPILOT_CLI_PATH"
	EnvCopilotHelperPath    = "ORKA_HARNESS_WRAPPER_COPILOT_HELPER_PATH"
)

const (
	PromptModeStdin = "stdin"
	PromptModeEnv   = "env"
	PromptModeFile  = "file"

	ResultModeStdout = "stdout"
	ResultModeFile   = "file"
)

const (
	DefaultListenAddr       = ":8080"
	DefaultOutputLimitBytes = 64 * 1024
	DefaultCancelGrace      = 2 * time.Second
	DefaultTurnRetention    = 5 * time.Minute
	DefaultPromptEnv        = "ORKA_TURN_PROMPT"
	DefaultWrapperWorkDir   = "/workspace"
)

type Config struct {
	ListenAddr           string
	Runtime              string
	WorkDir              string
	CommandEnv           []string
	StdoutLimitBytes     int64
	StderrLimitBytes     int64
	CancelGrace          time.Duration
	AuthValue            string
	AuthValueFile        string
	AllowUnauthenticated bool
	TurnRetention        time.Duration
	Generic              GenericAdapterConfig
	Codex                CodexAdapterConfig
	Claude               ClaudeAdapterConfig
	Copilot              CopilotAdapterConfig
}

type GenericAdapterConfig struct {
	Command    string
	Args       []string
	Env        []string
	WorkDir    string
	PromptMode string
	PromptEnv  string
	PromptFile string
	ResultMode string
	ResultFile string
}

type CodexAdapterConfig struct {
	Path        string
	WorkDir     string
	BypassProbe bool
}

type ClaudeAdapterConfig struct {
	Path    string
	WorkDir string
}

type CopilotAdapterConfig struct {
	Path       string
	HelperPath string
	WorkDir    string
}

func DefaultConfig() Config {
	return Config{
		ListenAddr:       DefaultListenAddr,
		Runtime:          RuntimeGeneric,
		StdoutLimitBytes: DefaultOutputLimitBytes,
		StderrLimitBytes: DefaultOutputLimitBytes,
		CancelGrace:      DefaultCancelGrace,
		TurnRetention:    DefaultTurnRetention,
		Generic: GenericAdapterConfig{
			PromptMode: PromptModeStdin,
			PromptEnv:  DefaultPromptEnv,
			ResultMode: ResultModeStdout,
		},
	}
}

func LoadConfigFromEnv() (Config, error) {
	cfg, err := LoadConfigFromEnvUnvalidated()
	if err != nil {
		return Config{}, err
	}
	return cfg, cfg.Validate()
}

//nolint:gocyclo // Centralized env parsing keeps wrapper configuration ownership in one module.
func LoadConfigFromEnvUnvalidated() (Config, error) {
	cfg := DefaultConfig()
	if v := strings.TrimSpace(os.Getenv(EnvListenAddr)); v != "" {
		cfg.ListenAddr = v
	}
	if v := strings.TrimSpace(os.Getenv(EnvRuntime)); v != "" {
		cfg.Runtime = v
	}
	if v := strings.TrimSpace(os.Getenv(EnvWorkDir)); v != "" {
		cfg.WorkDir = v
		cfg.Generic.WorkDir = v
		cfg.Codex.WorkDir = v
		cfg.Claude.WorkDir = v
	}
	if v := strings.TrimSpace(os.Getenv(EnvCommand)); v != "" {
		cfg.Generic.Command = v
	}
	if v := strings.TrimSpace(os.Getenv(EnvCommandArgs)); v != "" {
		args, err := parseStringListEnv(EnvCommandArgs, v)
		if err != nil {
			return Config{}, err
		}
		cfg.Generic.Args = args
	}
	if v := strings.TrimSpace(os.Getenv(EnvCommandEnv)); v != "" {
		env, err := parseStringListEnv(EnvCommandEnv, v)
		if err != nil {
			return Config{}, err
		}
		cfg.Generic.Env = env
		cfg.CommandEnv = env
	}
	if v := strings.TrimSpace(os.Getenv(EnvPromptMode)); v != "" {
		cfg.Generic.PromptMode = strings.ToLower(v)
	}
	if v := strings.TrimSpace(os.Getenv(EnvPromptEnv)); v != "" {
		cfg.Generic.PromptEnv = v
	}
	if v := strings.TrimSpace(os.Getenv(EnvPromptFile)); v != "" {
		cfg.Generic.PromptFile = v
	}
	if v := strings.TrimSpace(os.Getenv(EnvResultMode)); v != "" {
		cfg.Generic.ResultMode = strings.ToLower(v)
	}
	if v := strings.TrimSpace(os.Getenv(EnvResultFile)); v != "" {
		cfg.Generic.ResultFile = v
	}
	if v := strings.TrimSpace(os.Getenv(EnvStdoutLimitBytes)); v != "" {
		parsed, err := strconv.ParseInt(v, 10, 64)
		if err != nil || parsed < 0 {
			return Config{}, fmt.Errorf("invalid %s: %q", EnvStdoutLimitBytes, v)
		}
		cfg.StdoutLimitBytes = parsed
	}
	if v := strings.TrimSpace(os.Getenv(EnvStderrLimitBytes)); v != "" {
		parsed, err := strconv.ParseInt(v, 10, 64)
		if err != nil || parsed < 0 {
			return Config{}, fmt.Errorf("invalid %s: %q", EnvStderrLimitBytes, v)
		}
		cfg.StderrLimitBytes = parsed
	}
	if v := strings.TrimSpace(os.Getenv(EnvCancelGrace)); v != "" {
		parsed, err := time.ParseDuration(v)
		if err != nil || parsed < 0 {
			return Config{}, fmt.Errorf("invalid %s: %q", EnvCancelGrace, v)
		}
		cfg.CancelGrace = parsed
	}
	if v := strings.TrimSpace(os.Getenv(EnvAuthValue)); v != "" {
		cfg.AuthValue = v
	}
	if v := strings.TrimSpace(os.Getenv(EnvAuthValueFile)); v != "" {
		cfg.AuthValueFile = v
		data, err := os.ReadFile(v)
		if err != nil {
			return Config{}, fmt.Errorf("read %s: %w", EnvAuthValueFile, err)
		}
		cfg.AuthValue = strings.TrimSpace(string(data))
	}
	if v := strings.TrimSpace(os.Getenv(EnvAllowUnauthenticated)); v != "" {
		cfg.AllowUnauthenticated = strings.EqualFold(v, "true")
	}
	if v := strings.TrimSpace(os.Getenv(EnvTurnRetention)); v != "" {
		parsed, err := time.ParseDuration(v)
		if err != nil || parsed < 0 {
			return Config{}, fmt.Errorf("invalid %s: %q", EnvTurnRetention, v)
		}
		cfg.TurnRetention = parsed
	}
	if v := strings.TrimSpace(os.Getenv(EnvCopilotCLIPath)); v != "" {
		cfg.Copilot.Path = v
	}
	if v := strings.TrimSpace(os.Getenv(EnvCopilotHelperPath)); v != "" {
		cfg.Copilot.HelperPath = v
	}
	return cfg, nil
}

func (c Config) Validate() error {
	if strings.TrimSpace(c.ListenAddr) == "" {
		return fmt.Errorf("listen address is required")
	}
	if c.StdoutLimitBytes < 0 {
		return fmt.Errorf("stdout limit must be non-negative")
	}
	if c.StderrLimitBytes < 0 {
		return fmt.Errorf("stderr limit must be non-negative")
	}
	if c.CancelGrace < 0 {
		return fmt.Errorf("cancel grace period must be non-negative")
	}
	if c.TurnRetention < 0 {
		return fmt.Errorf("turn retention must be non-negative")
	}
	if !c.AllowUnauthenticated && strings.TrimSpace(c.AuthValue) == "" {
		return fmt.Errorf("auth token is required unless %s=true", EnvAllowUnauthenticated)
	}
	switch strings.ToLower(strings.TrimSpace(c.Runtime)) {
	case "", RuntimeGeneric, RuntimeCodex, RuntimeClaude, RuntimeCopilot, RuntimeMulti:
		return nil
	default:
		return fmt.Errorf("unsupported runtime adapter %q", c.Runtime)
	}
}

func parseStringListEnv(name, value string) ([]string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	if strings.HasPrefix(value, "[") {
		var out []string
		if err := json.Unmarshal([]byte(value), &out); err != nil {
			return nil, fmt.Errorf("invalid %s JSON array: %w", name, err)
		}
		return out, nil
	}
	return splitCSV(value), nil
}
