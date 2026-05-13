/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

// Package workerenv defines the environment-variable contract shared by the
// controller job builder and worker binaries.
package workerenv

import (
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

const (
	// Base worker env vars used by all worker types.
	TaskName       = "ORKA_TASK_NAME"
	TaskNamespace  = "ORKA_TASK_NAMESPACE"
	ResultEndpoint = "ORKA_RESULT_ENDPOINT"
	ControllerURL  = "ORKA_CONTROLLER_URL"
	Command        = "ORKA_COMMAND"
	AgentName      = "ORKA_AGENT_NAME"

	// Task relationship env vars.
	PriorTask          = "ORKA_PRIOR_TASK"
	PriorTaskNamespace = "ORKA_PRIOR_TASK_NAMESPACE"
	ParentTask         = "ORKA_PARENT_TASK"

	// Transaction context env vars.
	TransactionID                     = "ORKA_TRANSACTION_ID"
	TransactionProfile                = "ORKA_TRANSACTION_PROFILE"
	TransactionIssuer                 = "ORKA_TRANSACTION_ISSUER"
	TransactionSubject                = "ORKA_TRANSACTION_SUBJECT"
	TransactionRequestingWorkload     = "ORKA_TRANSACTION_REQUESTING_WORKLOAD"
	TransactionScope                  = "ORKA_TRANSACTION_SCOPE"
	TransactionScopes                 = "ORKA_TRANSACTION_SCOPES"
	TransactionContextDigest          = "ORKA_TRANSACTION_CONTEXT_DIGEST"
	TransactionRequesterContextDigest = "ORKA_TRANSACTION_REQUESTER_CONTEXT_DIGEST"
	TransactionTokenFile              = "ORKA_TRANSACTION_TOKEN_FILE"
	ContextTokenTTSURL                = "ORKA_CONTEXT_TOKEN_TTS_URL"
	ContextTokenSubjectTokenFile      = "ORKA_CONTEXT_TOKEN_SUBJECT_TOKEN_FILE"
	ContextTokenSubjectTokenType      = "ORKA_CONTEXT_TOKEN_SUBJECT_TOKEN_TYPE"
	ContextTokenOutboundScope         = "ORKA_CONTEXT_TOKEN_OUTBOUND_SCOPE"
	ContextTokenChildScope            = "ORKA_CONTEXT_TOKEN_CHILD_SCOPE"

	// AI worker env vars.
	AIProvider        = "ORKA_AI_PROVIDER"
	AIModel           = "ORKA_AI_MODEL"
	AIPrompt          = "ORKA_AI_PROMPT"
	AISystemPrompt    = "ORKA_AI_SYSTEM_PROMPT"
	AIBaseURL         = "ORKA_AI_BASE_URL"
	AIAzureAPIVersion = "ORKA_AI_AZURE_API_VERSION"
	AITools           = "ORKA_AI_TOOLS"
	AIFallbackCount   = "ORKA_AI_FALLBACK_COUNT"

	// Coordination/autonomous env vars used by AI worker and coordination tools.
	CoordinationEnabled       = "ORKA_COORDINATION_ENABLED"
	CoordinationMaxDepth      = "ORKA_COORDINATION_MAX_DEPTH"
	CoordinationMaxChildren   = "ORKA_COORDINATION_MAX_CHILDREN"
	CoordinationAllowedAgents = "ORKA_COORDINATION_ALLOWED_AGENTS"
	CoordinationDepth         = "ORKA_COORDINATION_DEPTH"
	AutonomousMode            = "ORKA_AUTONOMOUS_MODE"
	AutonomousIteration       = "ORKA_AUTONOMOUS_ITERATION"
	AutonomousMaxIterations   = "ORKA_AUTONOMOUS_MAX_ITERATIONS"

	// Agent runtime env vars.
	Prompt                     = "ORKA_PROMPT"
	Model                      = "ORKA_MODEL"
	SystemPrompt               = "ORKA_SYSTEM_PROMPT"
	MaxTurns                   = "ORKA_MAX_TURNS"
	AllowedTools               = "ORKA_ALLOWED_TOOLS"
	DisallowedTools            = "ORKA_DISALLOWED_TOOLS"
	AllowBash                  = "ORKA_ALLOW_BASH"
	TimeoutSeconds             = "ORKA_TIMEOUT_SECONDS"
	SessionName                = "ORKA_SESSION_NAME"
	CodexSandboxMode           = "ORKA_CODEX_SANDBOX_MODE"
	CodexAutoCompactTokenLimit = "ORKA_CODEX_AUTO_COMPACT_TOKEN_LIMIT"
	CodexDisableSandbox        = "ORKA_CODEX_DISABLE_SANDBOX"
	AnthropicBaseURL           = "ANTHROPIC_BASE_URL"
	OpenAIBaseURL              = "OPENAI_BASE_URL"
	AnthropicAPIKey            = "ANTHROPIC_API_KEY"
	OpenAIAPIKey               = "OPENAI_API_KEY"
	CodexAPIKey                = "CODEX_API_KEY"
	GitHubToken                = "GITHUB_TOKEN"
	GitToken                   = "GIT_TOKEN"
	GitAskpass                 = "GIT_ASKPASS"
	GitUsername                = "GIT_USERNAME"
	ClaudeCLIPath              = "CLAUDE_CLI_PATH"
	CodexCLIPath               = "CODEX_CLI_PATH"
	CopilotCLIPath             = "COPILOT_CLI_PATH"

	// Tool integration env vars.
	SearchAPIKey = "SEARCH_API_KEY"
	SearchAPIURL = "SEARCH_API_URL"

	// Kubernetes namespace env vars used by workers/tools.
	PodNamespace = "POD_NAMESPACE"
	Namespace    = "NAMESPACE"

	// Code execution tool env vars.
	CodeExecBackend                 = "ORKA_CODE_EXEC_BACKEND"
	CodeExecLocalCPUSeconds         = "ORKA_CODE_EXEC_LOCAL_CPU_SECONDS"
	CodeExecLocalMemoryKB           = "ORKA_CODE_EXEC_LOCAL_MEMORY_KB"
	CodeExecLocalMaxProcesses       = "ORKA_CODE_EXEC_LOCAL_MAX_PROCESSES"
	CodeExecKubernetesImage         = "ORKA_CODE_EXEC_K8S_IMAGE"
	CodeExecKubernetesPythonImage   = "ORKA_CODE_EXEC_K8S_PYTHON_IMAGE"
	CodeExecKubernetesNodeImage     = "ORKA_CODE_EXEC_K8S_NODE_IMAGE"
	CodeExecKubernetesBashImage     = "ORKA_CODE_EXEC_K8S_BASH_IMAGE"
	CodeExecKubernetesCPURequest    = "ORKA_CODE_EXEC_K8S_CPU_REQUEST"
	CodeExecKubernetesCPULimit      = "ORKA_CODE_EXEC_K8S_CPU_LIMIT"
	CodeExecKubernetesMemoryRequest = "ORKA_CODE_EXEC_K8S_MEMORY_REQUEST"
	CodeExecKubernetesMemoryLimit   = "ORKA_CODE_EXEC_K8S_MEMORY_LIMIT"
	CodeExecKubernetesNetworkPolicy = "ORKA_CODE_EXEC_K8S_NETWORK_POLICY"
	CodeExecKubernetesRuntimeClass  = "ORKA_CODE_EXEC_K8S_RUNTIME_CLASS_NAME"
	CodeExecKubernetesAppArmor      = "ORKA_CODE_EXEC_K8S_APPARMOR_PROFILE"
	OrkaNamespace                   = "ORKA_NAMESPACE"

	// Workspace env vars used by general and agent-runtime workers.
	GitRepo           = "ORKA_GIT_REPO"
	GitBranch         = "ORKA_GIT_BRANCH"
	GitRef            = "ORKA_GIT_REF"
	WorkspaceSubpath  = "ORKA_WORKSPACE_SUBPATH"
	ForkRepo          = "ORKA_FORK_REPO"
	PRBaseBranch      = "ORKA_PR_BASE_BRANCH"
	PushBranch        = "ORKA_PUSH_BRANCH"
	RequirePushBranch = "ORKA_REQUIRE_PUSH_BRANCH"
	WorkDir           = "ORKA_WORK_DIR"
	SkillsDir         = "ORKA_SKILLS_DIR"

	// Git config env vars used to mark the prepared workspace as safe.
	GitConfigCount  = "GIT_CONFIG_COUNT"
	GitConfigKey0   = "GIT_CONFIG_KEY_0"
	GitConfigValue0 = "GIT_CONFIG_VALUE_0"

	// Memory/controller context env vars used by AI worker memory integration.
	MemoryContextEnabled  = "ORKA_MEMORY_CONTEXT_ENABLED"
	MemoryContextLimit    = "ORKA_MEMORY_CONTEXT_LIMIT"
	MemoryContextMaxChars = "ORKA_MEMORY_CONTEXT_MAX_CHARS"
	ServiceAccountToken   = "ORKA_SA_TOKEN"
)

const trueString = "true"

// Env returns a simple Kubernetes environment variable.
func Env(name, value string) corev1.EnvVar {
	return corev1.EnvVar{Name: name, Value: value}
}

// EnvIfSet returns an env var and true when value is non-empty.
func EnvIfSet(name, value string) (corev1.EnvVar, bool) {
	if value == "" {
		return corev1.EnvVar{}, false
	}
	return Env(name, value), true
}

// AppendIfSet appends name=value to envVars only when value is non-empty.
func AppendIfSet(envVars []corev1.EnvVar, name, value string) []corev1.EnvVar {
	if value == "" {
		return envVars
	}
	return append(envVars, Env(name, value))
}

// IsTrue returns true when value enables a boolean env flag.
func IsTrue(value string) bool {
	return strings.EqualFold(strings.TrimSpace(value), trueString)
}

// SplitCSV returns trimmed, non-empty items from a comma-separated env value.
func SplitCSV(value string) []string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// JoinCSV joins values using the comma-separated format used by worker env vars.
func JoinCSV(values []string) string {
	return strings.Join(values, ",")
}

// BaseEnv is the common env contract passed to all Orka worker containers.
type BaseEnv struct {
	TaskName           string
	TaskNamespace      string
	ResultEndpoint     string
	ControllerURL      string
	TransactionID      string
	TransactionProfile string
}

// EnvVars renders the base worker environment.
func (e BaseEnv) EnvVars() []corev1.EnvVar {
	envVars := []corev1.EnvVar{
		Env(TaskName, e.TaskName),
		Env(TaskNamespace, e.TaskNamespace),
		Env(ResultEndpoint, e.ResultEndpoint),
		Env(ControllerURL, e.ControllerURL),
	}
	envVars = AppendIfSet(envVars, TransactionID, e.TransactionID)
	envVars = AppendIfSet(envVars, TransactionProfile, e.TransactionProfile)
	return envVars
}

// ParseBaseEnv reads the common worker environment.
func ParseBaseEnv(getenv func(string) string) BaseEnv {
	return BaseEnv{
		TaskName:           getenv(TaskName),
		TaskNamespace:      getenv(TaskNamespace),
		ResultEndpoint:     getenv(ResultEndpoint),
		ControllerURL:      getenv(ControllerURL),
		TransactionID:      getenv(TransactionID),
		TransactionProfile: getenv(TransactionProfile),
	}
}

// TransactionLogFields returns safe key/value fragments for worker stdout logs.
func TransactionLogFields(transactionID, profile string) string {
	fields := []string{}
	if transactionID != "" {
		fields = append(fields, "transactionID="+transactionID)
	}
	if profile != "" {
		fields = append(fields, "contextTokenProfile="+profile)
	}
	if len(fields) == 0 {
		return ""
	}
	return " " + strings.Join(fields, " ")
}

// FallbackProviderEnv is one fallback provider entry from the AI worker env contract.
type FallbackProviderEnv struct {
	Provider        string
	APIKey          string
	Model           string
	BaseURL         string
	AzureAPIVersion string
}

// FallbackPrefix returns the prefix for fallback provider i.
func FallbackPrefix(i int) string {
	return fmt.Sprintf("ORKA_AI_FALLBACK_%d", i)
}

func FallbackProviderKey(i int) string        { return FallbackPrefix(i) + "_PROVIDER" }
func FallbackAPIKeyKey(i int) string          { return FallbackPrefix(i) + "_API_KEY" }
func FallbackModelKey(i int) string           { return FallbackPrefix(i) + "_MODEL" }
func FallbackBaseURLKey(i int) string         { return FallbackPrefix(i) + "_BASE_URL" }
func FallbackAzureAPIVersionKey(i int) string { return FallbackPrefix(i) + "_AZURE_API_VERSION" }

// EnvVars renders the non-secret fallback metadata env vars for fallback i.
// API keys should normally be rendered from SecretKeyRef by the caller.
func (e FallbackProviderEnv) EnvVars(i int) []corev1.EnvVar {
	envVars := []corev1.EnvVar{
		Env(FallbackProviderKey(i), e.Provider),
		Env(FallbackModelKey(i), e.Model),
	}
	envVars = AppendIfSet(envVars, FallbackBaseURLKey(i), e.BaseURL)
	envVars = AppendIfSet(envVars, FallbackAzureAPIVersionKey(i), e.AzureAPIVersion)
	if e.APIKey != "" {
		envVars = append(envVars, Env(FallbackAPIKeyKey(i), e.APIKey))
	}
	return envVars
}

// ParseFallbackProviderEnv reads fallback provider i.
func ParseFallbackProviderEnv(getenv func(string) string, i int) FallbackProviderEnv {
	return FallbackProviderEnv{
		Provider:        getenv(FallbackProviderKey(i)),
		APIKey:          getenv(FallbackAPIKeyKey(i)),
		Model:           getenv(FallbackModelKey(i)),
		BaseURL:         getenv(FallbackBaseURLKey(i)),
		AzureAPIVersion: getenv(FallbackAzureAPIVersionKey(i)),
	}
}

// ParseFallbacks reads all fallback provider entries from env. Invalid counts are
// treated as no fallbacks to preserve historical worker behavior.
func ParseFallbacks(getenv func(string) string) []FallbackProviderEnv {
	countStr := strings.TrimSpace(getenv(AIFallbackCount))
	if countStr == "" {
		return nil
	}
	count, err := strconv.Atoi(countStr)
	if err != nil || count <= 0 {
		return nil
	}
	fallbacks := make([]FallbackProviderEnv, 0, count)
	for i := range count {
		fallbacks = append(fallbacks, ParseFallbackProviderEnv(getenv, i))
	}
	return fallbacks
}

// AIWorkerEnv is the typed AI worker env contract shared by JobBuilder and the
// AI worker binary.
type AIWorkerEnv struct {
	BaseEnv
	Provider        string
	Model           string
	Prompt          string
	SystemPrompt    string
	BaseURL         string
	AzureAPIVersion string
	Tools           []string
	Fallbacks       []FallbackProviderEnv
}

// EnvVars renders AI worker env vars. Fallback API keys are included only when
// set on the struct; JobBuilder should usually add them separately via secrets.
func (e AIWorkerEnv) EnvVars() []corev1.EnvVar {
	envVars := make([]corev1.EnvVar, 0, 8+len(e.Fallbacks)*4)
	if e.BaseEnv != (BaseEnv{}) {
		envVars = append(envVars, e.BaseEnv.EnvVars()...)
	}
	envVars = append(envVars,
		Env(AIProvider, e.Provider),
		Env(AIModel, e.Model),
		Env(AIPrompt, e.Prompt),
		Env(AISystemPrompt, e.SystemPrompt),
	)
	envVars = AppendIfSet(envVars, AIBaseURL, e.BaseURL)
	envVars = AppendIfSet(envVars, AIAzureAPIVersion, e.AzureAPIVersion)
	if len(e.Tools) > 0 {
		envVars = append(envVars, Env(AITools, JoinCSV(e.Tools)))
	}
	if len(e.Fallbacks) > 0 {
		envVars = append(envVars, Env(AIFallbackCount, strconv.Itoa(len(e.Fallbacks))))
		for i, fallback := range e.Fallbacks {
			envVars = append(envVars, fallback.EnvVars(i)...)
		}
	}
	return envVars
}

// ParseAIWorkerEnv reads the AI worker environment.
func ParseAIWorkerEnv(getenv func(string) string) AIWorkerEnv {
	return AIWorkerEnv{
		BaseEnv:         ParseBaseEnv(getenv),
		Provider:        getenv(AIProvider),
		Model:           getenv(AIModel),
		Prompt:          getenv(AIPrompt),
		SystemPrompt:    getenv(AISystemPrompt),
		BaseURL:         getenv(AIBaseURL),
		AzureAPIVersion: getenv(AIAzureAPIVersion),
		Tools:           SplitCSV(getenv(AITools)),
		Fallbacks:       ParseFallbacks(getenv),
	}
}

// ValidateRequired returns an error when required AI worker fields are missing.
func (e AIWorkerEnv) ValidateRequired() error {
	if e.Provider == "" {
		return fmt.Errorf("%s is required", AIProvider)
	}
	if e.Model == "" {
		return fmt.Errorf("%s is required", AIModel)
	}
	if e.Prompt == "" {
		return fmt.Errorf("%s is required", AIPrompt)
	}
	return nil
}

// CoordinationEnv is the coordination/autonomous env contract used by AI tasks.
type CoordinationEnv struct {
	Enabled       bool
	MaxDepth      int
	MaxChildren   int
	AllowedAgents []string
	Depth         string

	AutonomousMode          bool
	AutonomousIteration     int
	AutonomousMaxIterations int
}

// EnvVars renders coordination/autonomous env vars.
func (e CoordinationEnv) EnvVars() []corev1.EnvVar {
	if !e.Enabled {
		return nil
	}
	envVars := []corev1.EnvVar{
		Env(CoordinationEnabled, trueString),
		Env(CoordinationMaxDepth, strconv.Itoa(e.MaxDepth)),
		Env(CoordinationMaxChildren, strconv.Itoa(e.MaxChildren)),
	}
	if len(e.AllowedAgents) > 0 {
		envVars = append(envVars, Env(CoordinationAllowedAgents, JoinCSV(e.AllowedAgents)))
	}
	if e.Depth != "" {
		envVars = append(envVars, Env(CoordinationDepth, e.Depth))
	}
	if e.AutonomousMode {
		envVars = append(envVars,
			Env(AutonomousMode, trueString),
			Env(AutonomousIteration, strconv.Itoa(e.AutonomousIteration)),
		)
		if e.AutonomousMaxIterations > 0 {
			envVars = append(envVars, Env(AutonomousMaxIterations, strconv.Itoa(e.AutonomousMaxIterations)))
		}
	}
	return envVars
}

// ParseCoordinationEnv reads coordination/autonomous settings.
func ParseCoordinationEnv(getenv func(string) string) CoordinationEnv {
	return CoordinationEnv{
		Enabled:                 IsTrue(getenv(CoordinationEnabled)),
		MaxDepth:                parsePositiveInt(getenv(CoordinationMaxDepth)),
		MaxChildren:             parsePositiveInt(getenv(CoordinationMaxChildren)),
		AllowedAgents:           SplitCSV(getenv(CoordinationAllowedAgents)),
		Depth:                   getenv(CoordinationDepth),
		AutonomousMode:          IsTrue(getenv(AutonomousMode)),
		AutonomousIteration:     parsePositiveInt(getenv(AutonomousIteration)),
		AutonomousMaxIterations: parsePositiveInt(getenv(AutonomousMaxIterations)),
	}
}

func parsePositiveInt(value string) int {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || parsed <= 0 {
		return 0
	}
	return parsed
}
