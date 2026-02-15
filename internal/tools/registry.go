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
	DefaultRegistry.Register(NewCreatePullRequestTool(k8sClient))
	DefaultRegistry.Register(NewMergePullRequestTool(k8sClient))
	DefaultRegistry.Register(NewAutoMergePullRequestTool(k8sClient))
	DefaultRegistry.Register(NewReviewPullRequestTool(k8sClient))
	DefaultRegistry.Register(NewPostReviewCommentTool(k8sClient))
	DefaultRegistry.Register(NewCreateAgentTool(k8sClient))
	DefaultRegistry.Register(NewDeleteAgentTool(k8sClient))
	DefaultRegistry.Register(NewUpdatePlanTool())
}

func init() {
	RegisterBuiltinTools()
}
