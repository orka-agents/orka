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
	opencodeEnableExa             = "OPENCODE_ENABLE_EXA"
	opencodeExperimentalLSPTool   = "OPENCODE_EXPERIMENTAL_LSP_TOOL"
	opencodeMetadataAdapter       = "adapter"
	opencodePermissionAllow       = "allow"
	opencodePermissionBash        = "bash"
	opencodePermissionDeny        = "deny"
	opencodePermissionEdit        = "edit"
	opencodePermissionExternalDir = "external_directory"
	opencodePermissionGlob        = "glob"
	opencodePermissionGrep        = "grep"
	opencodePermissionLSP         = "lsp"
	opencodePermissionQuestion    = "question"
	opencodePermissionWebSearch   = "websearch"
	opencodePermissionRead        = "read"
	opencodePermissionSkill       = "skill"
	opencodePermissionTask        = "task"
	opencodePermissionTodoWrite   = "todowrite"
	opencodePermissionWebFetch    = "webfetch"
	opencodeEnvTrue               = "true"
	opencodeFinishReasonError     = "error"
	opencodeFinishReasonUnknown   = "unknown"
	opencodeEscapedValueEnv       = "ORKA_OPENCODE_API_KEY_JSON_ESCAPED"
	opencodeReadOnlyConfigHome    = "/opt/orka-opencode-config"
)

type OpencodeAdapter struct {
	config OpencodeAdapterConfig
}

type opencodeConfig struct {
	Schema       string                      `json:"$schema"`
	Provider     map[string]opencodeProvider `json:"provider"`
	Agent        map[string]opencodeAgent    `json:"agent"`
	Permission   map[string]any              `json:"permission"`
	Instructions []string                    `json:"instructions,omitempty"`
	Share        string                      `json:"share"`
	AutoUpdate   bool                        `json:"autoupdate"`
	Snapshot     bool                        `json:"snapshot"`
}

type opencodeAgent struct {
	Steps int `json:"steps"`
}

type opencodeReadPermission struct {
	All        string `json:"*"`
	Env        string `json:"*.env"`
	EnvFiles   string `json:"*.env.*"`
	EnvExample string `json:"*.env.example"`
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
		Type   string `json:"type"`
		Text   string `json:"text"`
		Reason string `json:"reason"`
	} `json:"part"`
}

func NewOpencodeAdapter(config OpencodeAdapterConfig) *OpencodeAdapter {
	return &OpencodeAdapter{config: config}
}

func (a *OpencodeAdapter) Name() string { return RuntimeOpencode }

func (a *OpencodeAdapter) BuildCommand(_ context.Context, turn TurnContext) (*CommandSpec, error) {
	agentCfg := agentConfigFromTurn(turn)
	model := strings.TrimSpace(agentCfg.Model)
	if model == "" {
		return nil, fmt.Errorf("%s is required for opencode runtime", workerenv.Model)
	}
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
	configPath, scratchDir, err := writeOpencodeConfig(agentCfg, baseURL)
	if err != nil {
		return nil, err
	}

	xdgConfigHome := opencodeReadOnlyConfigHome
	xdgDataHome := filepath.Join(scratchDir, "data")
	xdgCacheHome := filepath.Join(scratchDir, "cache")
	xdgStateHome := filepath.Join(scratchDir, "state")
	featureEnv := opencodeFeatureEnv(agentCfg)
	env, unsetEnv := buildOpencodeEnv(
		turn.Env,
		scratchDir,
		xdgConfigHome,
		xdgDataHome,
		xdgCacheHome,
		xdgStateHome,
		configPath,
		featureEnv,
	)
	escapedValue := opencodeEscapedConfigValue(envEntryValue(turn.Env, workerenv.OpenAIAPIKey))
	env = setEnv(env, opencodeEscapedValueEnv, escapedValue)

	args := []string{"run", "--dir", dir, "--format", "json", "--model", opencodeProviderName + "/" + model}
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
	message, finishReason := opencodeFinalMessage(stdout)
	if finishReason == opencodeFinishReasonUnknown || finishReason == opencodeFinishReasonError {
		return TurnResult{Result: stdout, Metadata: map[string]string{opencodeMetadataAdapter: RuntimeOpencode}},
			fmt.Errorf("opencode output ended with %s finish reason", finishReason)
	}
	if message != "" {
		return TurnResult{Result: message, Metadata: map[string]string{opencodeMetadataAdapter: RuntimeOpencode}}, nil
	}
	return TurnResult{Result: stdout, Metadata: map[string]string{opencodeMetadataAdapter: RuntimeOpencode}},
		fmt.Errorf("opencode output did not contain a completed text event")
}

func buildOpencodeEnv(
	turnEnv []string,
	home string,
	xdgConfigHome string,
	xdgDataHome string,
	xdgCacheHome string,
	xdgStateHome string,
	configPath string,
	featureEnv []string,
) ([]string, []string) {
	enabledFeatureFlags := make(map[string]struct{}, len(featureEnv))
	for _, entry := range featureEnv {
		name, _, ok := strings.Cut(entry, "=")
		if ok {
			enabledFeatureFlags[name] = struct{}{}
		}
	}

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
			if _, ok := enabledFeatureFlags[name]; ok {
				continue
			}
			reserved[name] = struct{}{}
		}
	}
	unsetEnv := make([]string, 0, len(reserved))
	for name := range reserved {
		unsetEnv = append(unsetEnv, name)
	}
	sort.Strings(unsetEnv)

	env := make([]string, 0, len(turnEnv)+len(featureEnv)+6)
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
	env = setEnv(env, "XDG_CACHE_HOME", xdgCacheHome)
	env = setEnv(env, "XDG_STATE_HOME", xdgStateHome)
	env = setEnv(env, opencodeConfigPathEnv, configPath)
	env = setEnv(env, opencodeDisableProjectConfig, opencodeEnvTrue)
	env = setEnv(env, opencodeDisableAutoUpdate, opencodeEnvTrue)
	env = setEnv(env, opencodeDisableExternalSkills, opencodeEnvTrue)
	for _, entry := range featureEnv {
		name, value, ok := strings.Cut(entry, "=")
		if ok {
			env = setEnv(env, name, value)
		}
	}
	return env, unsetEnv
}

func opencodeFeatureEnv(cfg *agentEnvConfig) []string {
	if cfg == nil || !cfg.AllowedToolsSet {
		return nil
	}

	allowed := map[string]bool{}
	for _, tool := range cfg.AllowedTools {
		if permission := opencodePermissionForTool(tool); permission != "" {
			allowed[permission] = true
		}
	}
	for _, tool := range cfg.DisallowedTools {
		if permission := opencodePermissionForTool(tool); permission != "" {
			delete(allowed, permission)
		}
	}

	featureEnv := []string(nil)
	if allowed[opencodePermissionWebSearch] {
		featureEnv = append(featureEnv, opencodeEnableExa+"="+opencodeEnvTrue)
	}
	if allowed[opencodePermissionLSP] {
		featureEnv = append(featureEnv, opencodeExperimentalLSPTool+"="+opencodeEnvTrue)
	}
	return featureEnv
}

func writeOpencodeConfig(cfg *agentEnvConfig, baseURL string) (string, string, error) {
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
	for _, dir := range []string{"data", "cache", "state"} {
		if err := os.MkdirAll(filepath.Join(scratchDir, dir), 0o700); err != nil {
			cleanup()
			return "", "", fmt.Errorf("create opencode %s directory: %w", dir, err)
		}
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
	permissions, err := opencodePermissions(cfg)
	if err != nil {
		cleanup()
		return "", "", err
	}
	contents, err := json.MarshalIndent(opencodeConfig{
		Schema: "https://opencode.ai/config.json",
		Provider: map[string]opencodeProvider{
			opencodeProviderName: {
				NPM:  "@ai-sdk/openai-compatible",
				Name: opencodeProviderName,
				Options: opencodeProviderOptions{
					BaseURL: baseURL,
					APIKey:  "{env:" + opencodeEscapedValueEnv + "}",
				},
				Models: models,
			},
		},
		Agent: map[string]opencodeAgent{
			opencodeDefaultAgent: {Steps: maxTurns},
		},
		Permission:   permissions,
		Instructions: instructions,
		Share:        "disabled",
		AutoUpdate:   false,
		Snapshot:     false,
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

func opencodePermissions(cfg *agentEnvConfig) (map[string]any, error) {
	permissions := map[string]any{
		opencodePermissionEdit:  opencodePermissionAllow,
		opencodePermissionBash:  opencodePermissionDeny,
		opencodePermissionSkill: opencodePermissionDeny,
	}
	if cfg == nil {
		return permissions, nil
	}
	for _, tools := range [][]string{cfg.AllowedTools, cfg.DisallowedTools} {
		for _, tool := range tools {
			if _, _, scoped := strings.Cut(strings.TrimSpace(tool), "("); scoped {
				return nil, fmt.Errorf("opencode runtime does not support scoped tool policy entry %q", tool)
			}
		}
	}

	if cfg.AllowedToolsSet {
		for _, permission := range opencodeManagedPermissions() {
			permissions[permission] = opencodePermissionDeny
		}
		for _, tool := range cfg.AllowedTools {
			if permission := opencodePermissionForTool(tool); permission != "" {
				permissions[permission] = opencodeAllowedPermission(permission)
			}
		}
	} else if cfg.AllowBash {
		permissions[opencodePermissionBash] = opencodePermissionAllow
	}
	for _, tool := range cfg.DisallowedTools {
		if permission := opencodePermissionForTool(tool); permission != "" {
			permissions[permission] = opencodePermissionDeny
		}
	}
	if !cfg.AllowBash {
		permissions[opencodePermissionBash] = opencodePermissionDeny
	}
	permissions[opencodePermissionSkill] = opencodePermissionDeny
	return permissions, nil
}

func opencodeAllowedPermission(permission string) any {
	switch permission {
	case opencodePermissionRead:
		return opencodeReadPermission{
			All:        opencodePermissionAllow,
			Env:        opencodePermissionDeny,
			EnvFiles:   opencodePermissionDeny,
			EnvExample: opencodePermissionAllow,
		}
	default:
		return opencodePermissionAllow
	}
}

func opencodeManagedPermissions() []string {
	return []string{
		opencodePermissionRead,
		opencodePermissionExternalDir,
		opencodePermissionGlob,
		opencodePermissionGrep,
		opencodePermissionEdit,
		opencodePermissionBash,
		opencodePermissionWebFetch,
		opencodePermissionWebSearch,
		opencodePermissionTask,
		opencodePermissionTodoWrite,
		opencodePermissionQuestion,
		opencodePermissionLSP,
	}
}

func opencodePermissionForTool(tool string) string {
	name := normalizeToolName(tool)
	switch name {
	case "read", "fileread", "ls", "list":
		return opencodePermissionRead
	case opencodePermissionGlob:
		return opencodePermissionGlob
	case opencodePermissionGrep:
		return opencodePermissionGrep
	case "write", opencodePermissionEdit, "multiedit", "notebookedit", "patch", "filewrite":
		return opencodePermissionEdit
	case opencodePermissionBash, "shell", "codeexec":
		return opencodePermissionBash
	case opencodePermissionWebFetch:
		return opencodePermissionWebFetch
	case "websearch":
		return opencodePermissionWebSearch
	case opencodePermissionTask:
		return opencodePermissionTask
	case "todoread", opencodePermissionTodoWrite:
		return opencodePermissionTodoWrite
	case opencodePermissionQuestion:
		return opencodePermissionQuestion
	case "lsp":
		return opencodePermissionLSP
	default:
		return ""
	}
}

func opencodeEscapedConfigValue(value string) string {
	encoded, _ := json.Marshal(value)
	contents := string(encoded[1 : len(encoded)-1])
	contents = strings.ReplaceAll(contents, "{", `\u007b`)
	return strings.ReplaceAll(contents, "}", `\u007d`)
}

func opencodeBaseURL(value string) string {
	value = strings.TrimRight(strings.TrimSpace(value), "/")
	return strings.TrimSuffix(value, "/chat/completions")
}

func opencodeFinalMessage(stdout string) (string, string) {
	scanner := bufio.NewScanner(strings.NewReader(stdout))
	scanner.Buffer(make([]byte, 64*1024), maxStoredResultBytes)
	last := ""
	finishReason := ""
	for scanner.Scan() {
		var event opencodeOutputEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			continue
		}
		switch {
		case event.Type == "text" && event.Part.Type == "text" && strings.TrimSpace(event.Part.Text) != "":
			last = event.Part.Text
		case event.Type == "step_finish" && event.Part.Type == "step-finish":
			finishReason = strings.TrimSpace(event.Part.Reason)
		}
	}
	return last, finishReason
}
