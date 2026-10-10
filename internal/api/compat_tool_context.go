/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"fmt"

	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/connectors"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/tools"
)

const maxProxyCreatedTasks = 20

type compatProxyToolContextProfile struct {
	SourceLabel       string
	TaskCreateAction  string
	TaskDeleteAction  string
	AgentCreateAction string
	AgentUpdateAction string
	AgentDeleteAction string
	SecretReadAction  string
}

var openAICompatProxyToolContextProfile = compatProxyToolContextProfile{
	SourceLabel:       "openai-proxy",
	TaskCreateAction:  "openAIToolCreateTask",
	TaskDeleteAction:  "openAIToolDeleteTask",
	AgentCreateAction: "openAIToolCreateAgent",
	AgentUpdateAction: "openAIToolUpdateAgent",
	AgentDeleteAction: "openAIToolDeleteAgent",
	SecretReadAction:  "openAIToolReadSecret",
}

var anthropicCompatProxyToolContextProfile = compatProxyToolContextProfile{
	SourceLabel:       "anthropic-proxy",
	TaskCreateAction:  "anthropicToolCreateTask",
	AgentCreateAction: "anthropicToolCreateAgent",
	SecretReadAction:  "anthropicToolReadSecret",
}

type compatProxyToolContextConfig struct {
	Client                    client.Client
	AuthorizationReader       client.Reader
	KubeClient                kubernetes.Interface
	Namespace                 string
	Provider                  ProviderResolutionInfo
	WatchNamespace            string
	EnforceNamespaceIsolation bool
	ResultStore               store.ResultStore
	GatewayEventStore         store.GatewayEventStore
	GenerateTaskName          func() string
	Profile                   compatProxyToolContextProfile
	AuthContext               *ContextToken
	AuthorizationConfig       ContextTokenAuthorizationConfig
	UserInfo                  *UserInfo
	LinkedAccounts            LinkedAccountsFactory
	ConnectorsEnabled         bool
}

func newCompatProxyToolContext(cfg compatProxyToolContextConfig) *tools.ToolContext {
	tasksCreated := 0
	authorizationReader := uncachedReaderOr(cfg.AuthorizationReader, cfg.Client)
	requester := requesterFromUserInfo(cfg.UserInfo)
	toolCtx := &tools.ToolContext{
		Client:                    cfg.Client,
		PolicyReader:              authorizationReader,
		Requester:                 requester,
		LinkedAccounts:            scopedLinkedAccounts(cfg.LinkedAccounts, cfg.Namespace, requester, cfg.UserInfo, cfg.AuthorizationConfig),
		AuthorizeConnectorRead:    connectorReadToolAuthorizer(cfg.UserInfo, cfg.AuthorizationConfig, cfg.ConnectorsEnabled),
		CreatedTasks:              tools.NewCreatedTasks(),
		KubeClient:                cfg.KubeClient,
		Namespace:                 cfg.Namespace,
		Tenant:                    cfg.Namespace,
		Provider:                  cfg.Provider.Name,
		ProviderType:              cfg.Provider.Type,
		WatchNamespace:            cfg.WatchNamespace,
		EnforceNamespaceIsolation: cfg.EnforceNamespaceIsolation,
		ResultStore:               cfg.ResultStore,
		GenerateTaskName:          cfg.GenerateTaskName,
		TaskLabels:                func() map[string]string { return map[string]string{"orka.ai/source": cfg.Profile.SourceLabel} },
		CheckTaskLimit: func() *tools.ChatToolError {
			if tasksCreated >= maxProxyCreatedTasks {
				return &tools.ChatToolError{Type: "limit_reached", Message: fmt.Sprintf("task creation limit reached (max %d)", maxProxyCreatedTasks), Suggestion: "Wait for existing tasks to complete"}
			}
			return nil
		},
		IncrementTasks: func() { tasksCreated++ },
	}
	if cfg.Profile.TaskCreateAction != "" {
		toolCtx.AuthorizeTaskCreate = func(ctx context.Context, task *corev1alpha1.Task) *tools.ChatToolError {
			authorize := func(ctx context.Context, task *corev1alpha1.Task) error {
				return authorizeAndStampToolTaskCreate(ctx, authorizationReader, cfg.KubeClient, cfg.AuthContext, cfg.AuthorizationConfig, cfg.Profile.TaskCreateAction, cfg.UserInfo, task)
			}
			return chatToolAuthorizationError(authorize, ctx, task, "Use a task configuration authorized by the context token")
		}
		toolCtx.SealTaskCreate = requesterStampSealer
	}
	if cfg.Profile.TaskDeleteAction != "" {
		toolCtx.AuthorizeTaskDelete = func(ctx context.Context, task *corev1alpha1.Task) *tools.ChatToolError {
			authorize := func(ctx context.Context, task *corev1alpha1.Task) error {
				return authorizeContextTokenTaskDeleteObject(ctx, authorizationReader, cfg.AuthContext, cfg.AuthorizationConfig, cfg.Profile.TaskDeleteAction, task)
			}
			return chatToolAuthorizationError(authorize, ctx, task, "Use a task authorized by the context token")
		}
	}
	if cfg.Profile.AgentCreateAction != "" {
		toolCtx.AuthorizeAgentCreate = func(ctx context.Context, agent *corev1alpha1.Agent) *tools.ChatToolError {
			authorize := func(ctx context.Context, agent *corev1alpha1.Agent) error {
				return authorizeContextTokenToolAgentCreate(ctx, authorizationReader, cfg.AuthContext, cfg.AuthorizationConfig, cfg.Profile.AgentCreateAction, agent)
			}
			return chatToolAuthorizationError(authorize, ctx, agent, "Use an agent configuration authorized by the context token")
		}
	}
	if cfg.Profile.AgentUpdateAction != "" {
		toolCtx.AuthorizeAgentUpdate = func(ctx context.Context, agent *corev1alpha1.Agent) *tools.ChatToolError {
			authorize := func(ctx context.Context, agent *corev1alpha1.Agent) error {
				return authorizeContextTokenToolAgentUpdate(ctx, authorizationReader, cfg.AuthContext, cfg.AuthorizationConfig, cfg.Profile.AgentUpdateAction, agent)
			}
			return chatToolAuthorizationError(authorize, ctx, agent, "Use an agent update authorized by the context token")
		}
	}
	if cfg.Profile.AgentDeleteAction != "" {
		toolCtx.AuthorizeAgentDelete = func(ctx context.Context, agent *corev1alpha1.Agent) *tools.ChatToolError {
			authorize := func(ctx context.Context, agent *corev1alpha1.Agent) error {
				return authorizeContextTokenToolAgentDelete(cfg.AuthContext, cfg.AuthorizationConfig, cfg.Profile.AgentDeleteAction, agent)
			}
			return chatToolAuthorizationError(authorize, ctx, agent, "Use an agent authorized by the context token")
		}
	}
	if cfg.Profile.SecretReadAction != "" {
		toolCtx.AuthorizeSecretRead = func(ctx context.Context, namespace, secretName string) *tools.ChatToolError {
			if err := authorizeContextTokenSecretRead(cfg.AuthContext, cfg.AuthorizationConfig, cfg.Profile.SecretReadAction, namespace, secretName); err != nil {
				return &tools.ChatToolError{
					Type:       apiErrorAuthorizationFailed,
					Message:    err.Error(),
					Suggestion: "Use a context token authorized to read the git credential secret",
				}
			}
			return nil
		}
		toolCtx.RequireSecretReadAuthorization = true
	}
	authorizeExternalToolContext(toolCtx, cfg.UserInfo, cfg.GatewayEventStore)
	return toolCtx
}

// scopedLinkedAccounts builds the person's linked-account resolver under
// the connector-read boundary the connector routes and list_connections
// apply: under enforcement, a delegated token without that scope uses no
// linked account (a catalog built-in keeps its own credential path, as it
// does for a person with no link), and audit mode records the missing
// scope when a linked built-in is actually resolved.
func scopedLinkedAccounts(factory LinkedAccountsFactory, namespace string, requester *corev1alpha1.RequestedBy, ui *UserInfo, cfg ContextTokenAuthorizationConfig) tools.LinkedAccountCredentials {
	if factory == nil || requester == nil {
		return nil
	}
	inner := factory(namespace, requester)
	if inner == nil {
		return nil
	}
	authorize := connectorReadToolAuthorizer(ui, cfg, true)
	if authorize == nil {
		return inner
	}
	return scopeGatedLinkedAccounts{inner: inner, authorize: authorize}
}

// scopeGatedLinkedAccounts consults the connector-read boundary before it
// resolves a linked account, so a refused token never reaches custody.
type scopeGatedLinkedAccounts struct {
	inner     tools.LinkedAccountCredentials
	authorize func() *tools.ChatToolError
}

// BuiltinToolCredential implements tools.LinkedAccountCredentials.
func (s scopeGatedLinkedAccounts) BuiltinToolCredential(ctx context.Context, toolName string) (tools.LinkedAccountCredential, bool, error) {
	if _, linked := connectors.BuiltinConnectorToolClass(toolName); linked && s.authorize() != nil {
		return tools.LinkedAccountCredential{}, false, nil
	}
	return s.inner.BuiltinToolCredential(ctx, toolName)
}
