/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/sozercan/orka/internal/llm"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ToolContext provides dependencies for tools that need K8s client access or other services.
type ToolContext struct {
	Client                    client.Client
	Namespace                 string
	WatchNamespace            string
	EnforceNamespaceIsolation bool
	// ResultStore for fetching task outputs (store.ResultStore)
	ResultStore interface {
		GetResult(ctx context.Context, namespace, taskName string) ([]byte, error)
	}
	// SessionDeleter for deleting sessions (controller.SessionManager)
	SessionDeleter interface {
		DeleteSession(ctx context.Context, namespace, sessionID string) error
	}
	// Task creation helpers provided by the chat executor
	GenerateTaskName func() string
	TaskLabels       func() map[string]string
	CheckTaskLimit   func() *ChatToolError
	IncrementTasks   func()
}

type toolContextKey struct{}

// WithToolContext adds a ToolContext to a context.
func WithToolContext(ctx context.Context, tc *ToolContext) context.Context {
	return context.WithValue(ctx, toolContextKey{}, tc)
}

// GetToolContext extracts a ToolContext from a context.
func GetToolContext(ctx context.Context) *ToolContext {
	tc, _ := ctx.Value(toolContextKey{}).(*ToolContext)
	return tc
}

// ChatToolError represents a structured error from a chat tool.
type ChatToolError struct {
	Type       string `json:"errorType"`
	Message    string `json:"error"`
	Suggestion string `json:"suggestion"`
}

func (e *ChatToolError) Error() string { return e.Message }

// ChatToolResult represents the result of a chat tool execution.
type ChatToolResult struct {
	Success    bool   `json:"success"`
	Data       any    `json:"data,omitempty"`
	Error      string `json:"error,omitempty"`
	ErrorType  string `json:"errorType,omitempty"`
	Suggestion string `json:"suggestion,omitempty"`
}

// MarshalChatResult marshals a ChatToolResult to a JSON string.
func MarshalChatResult(r ChatToolResult) (string, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return "", fmt.Errorf("failed to marshal chat tool result: %w", err)
	}
	return string(b), nil
}

// ChatToolSuccess returns a successful ChatToolResult JSON string.
func ChatToolSuccess(data any) (string, error) {
	return MarshalChatResult(ChatToolResult{Success: true, Data: data})
}

// ChatToolErrorResult returns a failed ChatToolResult JSON string.
func ChatToolErrorResult(errType, message, suggestion string) (string, error) {
	return MarshalChatResult(ChatToolResult{
		Error:      message,
		ErrorType:  errType,
		Suggestion: suggestion,
	})
}

const githubAPIBaseURL = "https://api.github.com"

const defaultNamespace = "default"

const trueStr = "true"

const defaultMergeMethod = "squash"

// Tool is the interface for built-in tools
type Tool interface {
	// Name returns the tool name
	Name() string

	// Description returns the tool description for the LLM
	Description() string

	// Parameters returns the JSON Schema for the tool parameters
	Parameters() json.RawMessage

	// Execute executes the tool with the given arguments
	Execute(ctx context.Context, args json.RawMessage) (string, error)
}

// Registry manages registered tools
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

// NewRegistry creates a new tool registry
func NewRegistry() *Registry {
	return &Registry{
		tools: make(map[string]Tool),
	}
}

// Register registers a tool
func (r *Registry) Register(tool Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[tool.Name()] = tool
}

// Get returns a tool by name
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	tool, ok := r.tools[name]
	return tool, ok
}

// List returns all registered tools
func (r *Registry) List() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	tools := make([]Tool, 0, len(r.tools))
	for _, tool := range r.tools {
		tools = append(tools, tool)
	}
	return tools
}

// Execute executes a tool by name
func (r *Registry) Execute(ctx context.Context, name string, args json.RawMessage) (string, error) {
	tool, ok := r.Get(name)
	if !ok {
		return "", fmt.Errorf("tool %q not found", name)
	}
	return tool.Execute(ctx, args)
}

// ToLLMTools converts the registry to LLM tool definitions
func (r *Registry) ToLLMTools(names []string) []llm.Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	tools := make([]llm.Tool, 0)
	for _, name := range names {
		if tool, ok := r.tools[name]; ok {
			tools = append(tools, llm.Tool{
				Name:        tool.Name(),
				Description: tool.Description(),
				Parameters:  tool.Parameters(),
			})
		}
	}
	return tools
}

// DefaultRegistry is the default tool registry with built-in tools
var DefaultRegistry = NewRegistry()

// RegisterBuiltinTools registers all built-in tools
func RegisterBuiltinTools() {
	DefaultRegistry.Register(NewWebSearchTool())
	DefaultRegistry.Register(NewCodeExecTool())
	DefaultRegistry.Register(NewFileReadTool())
	DefaultRegistry.Register(NewWebFetchTool())
	DefaultRegistry.Register(NewFileWriteTool())
}

// RegisterCoordinationTools registers coordination tools that require a K8s client
func RegisterCoordinationTools(k8sClient client.Client) {
	DefaultRegistry.Register(NewDelegateTaskTool(k8sClient))
	DefaultRegistry.Register(NewWaitForTasksTool(k8sClient))
	DefaultRegistry.Register(NewCancelTaskTool(k8sClient))
	DefaultRegistry.Register(NewSendMessageTool())
	DefaultRegistry.Register(NewCheckMessagesTool())
	DefaultRegistry.Register(NewCreatePullRequestTool(k8sClient))
	DefaultRegistry.Register(NewMergePullRequestTool(k8sClient))
	DefaultRegistry.Register(NewAutoMergePullRequestTool(k8sClient))
	DefaultRegistry.Register(NewReviewPullRequestTool(k8sClient))
	DefaultRegistry.Register(NewPostReviewCommentTool(k8sClient))
	DefaultRegistry.Register(NewListIssuesTool(k8sClient))
	DefaultRegistry.Register(NewListPullRequestsTool(k8sClient))
	DefaultRegistry.Register(NewGetIssueTool(k8sClient))
	DefaultRegistry.Register(NewCommentOnIssueTool(k8sClient))
	DefaultRegistry.Register(NewCreateAgentTool(k8sClient))
	DefaultRegistry.Register(NewDeleteAgentTool(k8sClient))
	DefaultRegistry.Register(NewUpdatePlanTool())
}

// RegisterChatTools registers the chat/management tools into the given registry.
func RegisterChatTools(r *Registry) {
	r.Register(&CreateAITaskTool{})
	r.Register(&CreateContainerTaskTool{})
	r.Register(&CreateAgentTaskTool{})
	r.Register(&CheckTaskProgressTool{})
	r.Register(&FetchTaskOutputTool{})
	r.Register(&WaitForTaskTool{})
	r.Register(&ChatCancelTaskTool{})
	r.Register(&ListAgentsTool{})
	r.Register(&ListToolsTool{})
	r.Register(&ListTasksTool{})
	r.Register(&ChatCreateAgentTool{})
	r.Register(&UpdateAgentTool{})
	r.Register(&ChatDeleteAgentTool{})
	r.Register(&CreateToolCRDTool{})
	r.Register(&DeleteToolTool{})
	r.Register(&DeleteSessionTool{})
}

// RegisterChatToolsDefault registers chat tools into DefaultRegistry for use by the proxy.
func RegisterChatToolsDefault() {
	RegisterChatTools(DefaultRegistry)
}

// ChatToolNames returns the names of all chat tools in registration order.
func ChatToolNames() []string {
	return []string{
		"create_ai_task",
		"create_container_task",
		"create_agent_task",
		"check_task_progress",
		"fetch_task_output",
		"wait_for_task",
		"cancel_task",
		"list_agents",
		"list_tools",
		"list_tasks",
		"create_agent",
		"update_agent",
		"delete_agent",
		"create_tool",
		"delete_tool",
		"delete_session",
	}
}

func init() {
	RegisterBuiltinTools()
	RegisterChatToolsDefault()
}
