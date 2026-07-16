package cliwrapper

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/orka-agents/orka/internal/workerenv"
)

const (
	defaultOpencodePath           = "opencode"
	opencodeProviderName          = "engine"
	opencodeDefaultAgent          = "build"
	opencodeModelContextLimit     = 128000
	opencodeModelOutputLimit      = 8192
	opencodeDisableProjectConfig  = "OPENCODE_DISABLE_PROJECT_CONFIG"
	opencodeDisableAutoUpdate     = "OPENCODE_DISABLE_AUTOUPDATE"
	opencodeDisableExternalSkills = "OPENCODE_DISABLE_EXTERNAL_SKILLS"
	opencodeConfigPathEnv         = "OPENCODE_CONFIG"
	opencodeEnvPrefix             = "OPENCODE_"
	opencodePermissionAllow       = "allow"
	opencodePermissionDeny        = "deny"
	opencodeEnvTrue               = "true"
)

type OpencodeAdapter struct {
	config OpencodeAdapterConfig
}

type opencodeConfig struct {
	Schema       string                      `json:"$schema"`
	Provider     map[string]opencodeProvider `json:"provider"`
	Agent        map[string]opencodeAgent    `json:"agent"`
	Permission   map[string]string           `json:"permission"`
	Instructions []string                    `json:"instructions,omitempty"`
	Share        string                      `json:"share"`
	AutoUpdate   bool                        `json:"autoupdate"`
}

type opencodeAgent struct {
	Steps int `json:"steps"`
}

type opencodeProvider struct {
	NPM     string                   `json:"npm"`
	Name    string                   `json:"name"`
	Options opencodeProviderOptions  `json:"options"`
	Models  map[string]opencodeModel `json:"models"`
}

type opencodeProviderOptions struct {
	BaseURL string `json:"baseURL"`
	APIKey  string `json:"apiKey"`
}

type opencodeModel struct {
	Limit opencodeModelLimit `json:"limit"`
}

type opencodeModelLimit struct {
	Context int `json:"context"`
	Output  int `json:"output"`
}

type opencodeOutputEvent struct {
	Type string `json:"type"`
	Part struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"part"`
}

func NewOpencodeAdapter(config OpencodeAdapterConfig) *OpencodeAdapter {
	return &OpencodeAdapter{config: config}
}

func (a *OpencodeAdapter) Name() string { return RuntimeOpencode }

func (a *OpencodeAdapter) BuildCommand(_ context.Context, turn TurnContext) (*CommandSpec, error) {
	agentCfg := agentConfigFromTurn(turn)
	dir := firstNonEmpty(turn.WorkDir, a.config.WorkDir, DefaultWrapperWorkDir)
	if stat, err := os.Stat(dir); err != nil || !stat.IsDir() {
		if err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("stat opencode workspace directory: %w", err)
		}
		if wd, wdErr := os.Getwd(); wdErr == nil {
			dir = wd
		}
	}

	baseURL := opencodeBaseURL(envEntryValue(turn.Env, workerenv.OpenAIBaseURL))
	if baseURL == "" {
		return nil, fmt.Errorf("%s is required for opencode runtime", workerenv.OpenAIBaseURL)
	}
	configPath, scratchDir, err := writeOpencodeConfig(agentCfg, baseURL, envEntryValue(turn.Env, workerenv.OpenAIAPIKey))
	if err != nil {
		return nil, err
	}

	xdgConfigHome := filepath.Join(scratchDir, "config")
	xdgDataHome := filepath.Join(scratchDir, "data")
	env, unsetEnv := buildOpencodeEnv(turn.Env, scratchDir, xdgConfigHome, xdgDataHome, configPath)

	args := []string{"run", "--dir", dir, "--format", "json"}
	if model := strings.TrimSpace(agentCfg.Model); model != "" {
		args = append(args, "--model", opencodeProviderName+"/"+model)
	}
	return &CommandSpec{
		Path:      firstNonEmpty(a.config.Path, defaultOpencodePath),
		Args:      args,
		Env:       env,
		UnsetEnv:  unsetEnv,
		Dir:       dir,
		Stdin:     []byte(turn.Prompt),
		TempFiles: []string{configPath, scratchDir},
	}, nil
}

func (a *OpencodeAdapter) ParseResult(_ context.Context, _ TurnContext, run CommandResult) (TurnResult, error) {
	stdout := run.ExactStdout()
	if message := opencodeFinalMessage(stdout); message != "" {
		return TurnResult{Result: message, Metadata: map[string]string{"adapter": RuntimeOpencode}}, nil
	}
	return TurnResult{Result: stdout, Metadata: map[string]string{"adapter": RuntimeOpencode}}, nil
}

func buildOpencodeEnv(turnEnv []string, home, xdgConfigHome, xdgDataHome, configPath string) ([]string, []string) {
	reserved := map[string]struct{}{}
	for _, entry := range append(os.Environ(), turnEnv...) {
		name, _, ok := strings.Cut(entry, "=")
		if !ok || !strings.HasPrefix(name, opencodeEnvPrefix) {
			continue
		}
		switch name {
		case opencodeConfigPathEnv, opencodeDisableProjectConfig, opencodeDisableAutoUpdate, opencodeDisableExternalSkills:
			continue
		default:
			reserved[name] = struct{}{}
		}
	}
	unsetEnv := make([]string, 0, len(reserved))
	for name := range reserved {
		unsetEnv = append(unsetEnv, name)
	}
	sort.Strings(unsetEnv)

	env := make([]string, 0, len(turnEnv)+6)
	for _, entry := range turnEnv {
		name, _, ok := strings.Cut(entry, "=")
		if ok && strings.HasPrefix(name, opencodeEnvPrefix) {
			continue
		}
		env = append(env, entry)
	}
	env = setEnv(env, "HOME", home)
	env = setEnv(env, "XDG_CONFIG_HOME", xdgConfigHome)
	env = setEnv(env, "XDG_DATA_HOME", xdgDataHome)
	env = setEnv(env, opencodeConfigPathEnv, configPath)
	env = setEnv(env, opencodeDisableProjectConfig, opencodeEnvTrue)
	env = setEnv(env, opencodeDisableAutoUpdate, opencodeEnvTrue)
	env = setEnv(env, opencodeDisableExternalSkills, opencodeEnvTrue)
	return env, unsetEnv
}

func writeOpencodeConfig(cfg *agentEnvConfig, baseURL, apiKey string) (string, string, error) {
	scratchDir, err := os.MkdirTemp("", "orka-opencode-*")
	if err != nil {
		return "", "", fmt.Errorf("create opencode scratch directory: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(scratchDir) }

	configDir := filepath.Join(scratchDir, "config", "opencode")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		cleanup()
		return "", "", fmt.Errorf("create opencode config directory: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(scratchDir, "data"), 0o700); err != nil {
		cleanup()
		return "", "", fmt.Errorf("create opencode data directory: %w", err)
	}

	models := map[string]opencodeModel{}
	if cfg != nil {
		if model := strings.TrimSpace(cfg.Model); model != "" {
			models[model] = opencodeModel{Limit: opencodeModelLimit{
				Context: opencodeModelContextLimit,
				Output:  opencodeModelOutputLimit,
			}}
		}
	}
	bashPermission := opencodePermissionDeny
	if cfg != nil && cfg.AllowBash {
		bashPermission = opencodePermissionAllow
	}
	maxTurns := 50
	instructions := []string(nil)
	if cfg != nil {
		if cfg.MaxTurns > 0 {
			maxTurns = cfg.MaxTurns
		}
		if systemPrompt := strings.TrimSpace(cfg.SystemPrompt); systemPrompt != "" {
			instructionsPath := filepath.Join(scratchDir, "instructions.md")
			if err := os.WriteFile(instructionsPath, []byte(systemPrompt+"\n"), 0o600); err != nil {
				cleanup()
				return "", "", fmt.Errorf("write opencode instructions: %w", err)
			}
			instructions = []string{instructionsPath}
		}
	}
	contents, err := json.MarshalIndent(opencodeConfig{
		Schema: "https://opencode.ai/config.json",
		Provider: map[string]opencodeProvider{
			opencodeProviderName: {
				NPM:  "@ai-sdk/openai-compatible",
				Name: opencodeProviderName,
				Options: opencodeProviderOptions{
					BaseURL: baseURL,
					APIKey:  apiKey,
				},
				Models: models,
			},
		},
		Agent: map[string]opencodeAgent{
			opencodeDefaultAgent: {Steps: maxTurns},
		},
		Permission: map[string]string{
			"edit":  opencodePermissionAllow,
			"bash":  bashPermission,
			"skill": opencodePermissionDeny,
		},
		Instructions: instructions,
		Share:        "disabled",
		AutoUpdate:   false,
	}, "", "  ")
	if err != nil {
		cleanup()
		return "", "", fmt.Errorf("marshal opencode config: %w", err)
	}

	configPath := filepath.Join(configDir, "opencode.json")
	if err := os.WriteFile(configPath, append(contents, '\n'), 0o600); err != nil {
		cleanup()
		return "", "", fmt.Errorf("write opencode config: %w", err)
	}
	if err := chownTreeForChild(scratchDir); err != nil {
		cleanup()
		return "", "", fmt.Errorf("chown opencode scratch directory: %w", err)
	}
	return configPath, scratchDir, nil
}

func opencodeBaseURL(value string) string {
	value = strings.TrimRight(strings.TrimSpace(value), "/")
	return strings.TrimSuffix(value, "/chat/completions")
}

func opencodeFinalMessage(stdout string) string {
	scanner := bufio.NewScanner(strings.NewReader(stdout))
	scanner.Buffer(make([]byte, 64*1024), maxStoredResultBytes)
	last := ""
	for scanner.Scan() {
		var event opencodeOutputEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			continue
		}
		if event.Type != "text" || event.Part.Type != "text" || strings.TrimSpace(event.Part.Text) == "" {
			continue
		}
		last = event.Part.Text
	}
	return last
}
