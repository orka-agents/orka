package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type transactionProviderInfo struct {
	Name      string
	Namespace string
	Type      string
}

type childTransactionContext struct {
	agentName      string
	agentNamespace string
	childType      corev1alpha1.TaskType
	agent          *corev1alpha1.Agent
	provider       *corev1alpha1.Provider
	providerInfo   transactionProviderInfo
	model          string
	fallbacks      []transactionProviderModel
	aiTools        []string
	runtimeTools   []string
	runtimeBash    bool
}

type transactionProviderModel struct {
	provider transactionProviderInfo
	model    string
}

func validateChildTaskAgainstParentTransaction(ctx context.Context, k8sClient client.Client, parent, child *corev1alpha1.Task, agentName string) error {
	if parent == nil || parent.Spec.Transaction == nil || child == nil {
		return nil
	}
	txCtx := parent.Spec.Transaction.Context
	if len(txCtx) == 0 {
		return nil
	}
	childCtx, err := resolveChildTransactionContext(ctx, k8sClient, child, agentName)
	if err != nil {
		return err
	}
	agentName = childCtx.agentName
	agentNamespace := childCtx.agentNamespace

	if agentName == "" && child.Spec.AgentRef != nil {
		agentName = child.Spec.AgentRef.Name
	}

	if want := strings.TrimSpace(txCtx["namespace"]); want != "" && child.Namespace != want {
		return fmt.Errorf("child task namespace %q does not match transaction context %q", child.Namespace, want)
	}
	if want := strings.TrimSpace(txCtx["taskType"]); want != "" && string(child.Spec.Type) != want {
		return fmt.Errorf("child task type %q does not match transaction context %q", child.Spec.Type, want)
	}
	if allowed, ok := transactionContextStringList(txCtx["allowedAgents"]); ok && !transactionAgentAllowed(agentName, agentNamespace, allowed) {
		return fmt.Errorf("child task agent %q is not allowed by transaction context", namespacedToolName(agentNamespace, agentName))
	} else if !ok {
		if want := strings.TrimSpace(txCtx["agent"]); want != "" && !transactionAgentMatches(agentName, agentNamespace, want) {
			return fmt.Errorf("child task agent %q does not match transaction context %q", namespacedToolName(agentNamespace, agentName), want)
		}
	}

	workspace := taskWorkspace(child)
	for _, constraint := range []struct {
		key string
		got string
	}{
		{key: "repo", got: workspaceGitRepo(workspace)},
		{key: "branch", got: workspaceBranch(workspace)},
		{key: "ref", got: workspaceRef(workspace)},
	} {
		if want := strings.TrimSpace(txCtx[constraint.key]); want != "" && constraint.got != want {
			return fmt.Errorf("child task workspace %s %q does not match transaction context %q", constraint.key, constraint.got, want)
		}
	}
	if err := validateChildProviderModelConstraints(txCtx, childCtx); err != nil {
		return err
	}
	if err := validateChildToolConstraints(txCtx, childCtx); err != nil {
		return err
	}
	return nil
}

func resolveChildTransactionContext(ctx context.Context, k8sClient client.Client, child *corev1alpha1.Task, agentName string) (childTransactionContext, error) {
	childCtx := childTransactionContext{
		agentName:      agentName,
		agentNamespace: child.Namespace,
		childType:      child.Spec.Type,
	}
	if child.Spec.AgentRef != nil {
		if childCtx.agentName == "" {
			childCtx.agentName = child.Spec.AgentRef.Name
		}
		if child.Spec.AgentRef.Namespace != "" {
			childCtx.agentNamespace = child.Spec.AgentRef.Namespace
		}
	}
	if k8sClient != nil && childCtx.agentName != "" {
		agent := &corev1alpha1.Agent{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: childCtx.agentName, Namespace: childCtx.agentNamespace}, agent); err != nil {
			if !apierrors.IsNotFound(err) {
				return childCtx, fmt.Errorf("resolve child agent %q in namespace %q: %w", childCtx.agentName, childCtx.agentNamespace, err)
			}
		} else {
			childCtx.agent = agent
		}
	}

	providerRef := childTransactionProviderRef(child, childCtx.agent)
	if providerRef != nil && strings.TrimSpace(providerRef.Name) != "" {
		providerNamespace := providerRef.Namespace
		if providerNamespace == "" {
			providerNamespace = child.Namespace
		}
		childCtx.providerInfo = transactionProviderInfo{Name: providerRef.Name, Namespace: providerNamespace}
		if k8sClient != nil {
			provider := &corev1alpha1.Provider{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: providerRef.Name, Namespace: providerNamespace}, provider); err != nil {
				if !apierrors.IsNotFound(err) {
					return childCtx, fmt.Errorf("resolve child provider %q in namespace %q: %w", providerRef.Name, providerNamespace, err)
				}
			} else {
				childCtx.provider = provider
			}
		}
	}
	childCtx.providerInfo, childCtx.model = childTransactionEffectiveProviderModel(child, childCtx.agent, childCtx.provider, childCtx.providerInfo)
	childCtx.fallbacks = childTransactionFallbackProviderModels(ctx, k8sClient, child.Namespace, childCtx.agent)
	childCtx.aiTools = childTransactionEffectiveAITools(child, childCtx.agent)
	childCtx.runtimeTools = childTransactionEffectiveRuntimeAllowedTools(child, childCtx.agent)
	childCtx.runtimeBash = childTransactionEffectiveRuntimeAllowBash(child, childCtx.agent)
	return childCtx, nil
}

func childTransactionProviderRef(child *corev1alpha1.Task, agent *corev1alpha1.Agent) *corev1alpha1.ProviderReference {
	if child.Spec.AI != nil && child.Spec.AI.ProviderRef != nil {
		return child.Spec.AI.ProviderRef
	}
	if agent != nil && agent.Spec.ProviderRef != nil {
		return agent.Spec.ProviderRef
	}
	return nil
}

func childTransactionEffectiveProviderModel(child *corev1alpha1.Task, agent *corev1alpha1.Agent, provider *corev1alpha1.Provider, providerInfo transactionProviderInfo) (transactionProviderInfo, string) {
	model := ""
	if provider != nil {
		providerInfo = transactionProviderInfo{
			Name:      provider.Name,
			Namespace: provider.Namespace,
			Type:      string(provider.Spec.Type),
		}
		model = provider.Spec.DefaultModel
	}
	if agent != nil && agent.Spec.Model != nil {
		if strings.TrimSpace(agent.Spec.Model.Provider) != "" {
			providerInfo = transactionProviderInfo{Type: agent.Spec.Model.Provider}
		}
		if strings.TrimSpace(agent.Spec.Model.Name) != "" {
			model = agent.Spec.Model.Name
		}
	}
	if child.Spec.AI != nil {
		if strings.TrimSpace(child.Spec.AI.Provider) != "" {
			providerInfo = transactionProviderInfo{Type: child.Spec.AI.Provider}
		}
		if strings.TrimSpace(child.Spec.AI.Model) != "" {
			model = child.Spec.AI.Model
		}
	}
	if provider != nil {
		providerInfo = transactionProviderInfo{
			Name:      provider.Name,
			Namespace: provider.Namespace,
			Type:      string(provider.Spec.Type),
		}
	}
	return providerInfo, model
}

func childTransactionFallbackProviderModels(ctx context.Context, k8sClient client.Client, namespace string, agent *corev1alpha1.Agent) []transactionProviderModel {
	if k8sClient == nil || agent == nil || agent.Spec.Model == nil || len(agent.Spec.Model.Fallbacks) == 0 {
		return nil
	}
	fallbacks := make([]transactionProviderModel, 0, len(agent.Spec.Model.Fallbacks))
	for _, fb := range agent.Spec.Model.Fallbacks {
		if strings.TrimSpace(fb.ProviderRef) == "" {
			continue
		}
		provider := &corev1alpha1.Provider{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: fb.ProviderRef, Namespace: namespace}, provider); err != nil {
			continue
		}
		model := strings.TrimSpace(fb.Model)
		if model == "" {
			model = provider.Spec.DefaultModel
		}
		fallbacks = append(fallbacks, transactionProviderModel{
			provider: transactionProviderInfo{
				Name:      provider.Name,
				Namespace: provider.Namespace,
				Type:      string(provider.Spec.Type),
			},
			model: model,
		})
	}
	return fallbacks
}

func validateChildProviderModelConstraints(txCtx map[string]string, childCtx childTransactionContext) error {
	if !childHasProviderModelConstraints(txCtx) {
		return nil
	}
	tokenNamespace, hasTokenNamespace := txCtx["namespace"], strings.TrimSpace(txCtx["namespace"]) != ""
	if err := validateChildProviderModel(txCtx, childCtx.providerInfo, childCtx.model, tokenNamespace, hasTokenNamespace, ""); err != nil {
		return err
	}
	for _, fb := range childCtx.fallbacks {
		if err := validateChildProviderModel(txCtx, fb.provider, fb.model, tokenNamespace, hasTokenNamespace, "fallback "); err != nil {
			return err
		}
	}
	return nil
}

func childHasProviderModelConstraints(txCtx map[string]string) bool {
	for _, key := range []string{"provider", "allowedProviders", "model", "allowedModels"} {
		if strings.TrimSpace(txCtx[key]) != "" {
			return true
		}
	}
	return false
}

func validateChildProviderModel(txCtx map[string]string, provider transactionProviderInfo, model, tokenNamespace string, hasTokenNamespace bool, prefix string) error {
	if want := strings.TrimSpace(txCtx["provider"]); want != "" && !transactionProviderMatches(provider, want, tokenNamespace, hasTokenNamespace) {
		return fmt.Errorf("child task %sprovider %q is not allowed by transaction context", prefix, transactionProviderDisplayName(provider))
	}
	if allowed, ok := transactionContextStringList(txCtx["allowedProviders"]); ok && !transactionProviderAllowed(provider, allowed, tokenNamespace, hasTokenNamespace) {
		return fmt.Errorf("child task %sprovider %q is not allowed by transaction context", prefix, transactionProviderDisplayName(provider))
	}
	if want := strings.TrimSpace(txCtx["model"]); want != "" && model != want {
		return fmt.Errorf("child task %smodel %q does not match transaction context %q", prefix, model, want)
	}
	if allowed, ok := transactionContextStringList(txCtx["allowedModels"]); ok && !transactionModelAllowed(provider, model, allowed, tokenNamespace, hasTokenNamespace) {
		return fmt.Errorf("child task %smodel %q is not allowed by transaction context", prefix, model)
	}
	return nil
}

func validateChildToolConstraints(txCtx map[string]string, childCtx childTransactionContext) error {
	allowed, ok := transactionContextStringList(txCtx["allowedTools"])
	if !ok {
		return nil
	}
	if childCtx.childType == corev1alpha1.TaskTypeAgent && !hasNonEmptyTransactionTools(childCtx.runtimeTools) {
		return fmt.Errorf("child task agent runtime tools are unrestricted by task or agent while transaction context restricts allowedTools")
	}
	runtimeTools := childTransactionRuntimeToolConstraints(childCtx)
	for _, tool := range append(append([]string{}, childCtx.aiTools...), runtimeTools...) {
		tool = strings.TrimSpace(tool)
		if tool == "" {
			continue
		}
		if !slices.Contains(allowed, tool) {
			return fmt.Errorf("child task tool %q is not allowed by transaction context", tool)
		}
	}
	return nil
}

func childTransactionRuntimeToolConstraints(childCtx childTransactionContext) []string {
	runtimeTools := append([]string{}, childCtx.runtimeTools...)
	if childCtx.childType == corev1alpha1.TaskTypeAgent && childCtx.runtimeBash {
		runtimeTools = append(runtimeTools, "Bash")
	}
	return runtimeTools
}

func hasNonEmptyTransactionTools(tools []string) bool {
	return slices.ContainsFunc(tools, func(tool string) bool {
		return strings.TrimSpace(tool) != ""
	})
}

func childTransactionEffectiveAITools(child *corev1alpha1.Task, agent *corev1alpha1.Agent) []string {
	tools := []string{}
	if agent != nil {
		for _, tool := range agent.Spec.Tools {
			if tool.Enabled != nil && !*tool.Enabled {
				continue
			}
			if strings.TrimSpace(tool.Name) != "" {
				tools = append(tools, tool.Name)
			}
		}
		if agent.Spec.Coordination != nil && agent.Spec.Coordination.Enabled {
			for _, tool := range transactionCoordinationToolNames() {
				if !slices.Contains(tools, tool) {
					tools = append(tools, tool)
				}
			}
		}
	}
	if child.Spec.AI != nil {
		for _, tool := range child.Spec.AI.Tools {
			if strings.TrimSpace(tool) != "" {
				tools = append(tools, tool)
			}
		}
	}
	return tools
}

func childTransactionEffectiveRuntimeAllowedTools(child *corev1alpha1.Task, agent *corev1alpha1.Agent) []string {
	if child.Spec.AgentRuntime != nil && len(child.Spec.AgentRuntime.AllowedTools) > 0 {
		return append([]string{}, child.Spec.AgentRuntime.AllowedTools...)
	}
	if agent != nil && agent.Spec.Runtime != nil && len(agent.Spec.Runtime.DefaultAllowedTools) > 0 {
		return append([]string{}, agent.Spec.Runtime.DefaultAllowedTools...)
	}
	return nil
}

func childTransactionEffectiveRuntimeAllowBash(child *corev1alpha1.Task, agent *corev1alpha1.Agent) bool {
	allowBash := true
	if agent != nil && agent.Spec.Runtime != nil && agent.Spec.Runtime.DefaultAllowBash != nil {
		allowBash = *agent.Spec.Runtime.DefaultAllowBash
	}
	if child.Spec.AgentRuntime != nil && child.Spec.AgentRuntime.AllowBash != nil {
		allowBash = *child.Spec.AgentRuntime.AllowBash
	}
	return allowBash
}

func transactionCoordinationToolNames() []string {
	return []string{
		"delegate_task",
		"wait_for_tasks",
		"create_container_task",
		"cancel_task",
		"send_message",
		"check_messages",
		"recall_memory",
		"remember",
		"propose_memory",
		"search_transcript",
		"create_pull_request",
		"check_pull_request_ci",
		"merge_pull_request",
		"auto_merge_pull_request",
		"review_pull_request",
		"post_review_comment",
		"create_agent",
		"delete_agent",
		"update_plan",
	}
}

func transactionContextStringList(value string) ([]string, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, false
	}
	var decoded []string
	if err := json.Unmarshal([]byte(value), &decoded); err == nil {
		return decoded, true
	}
	return splitCSV(value), true
}

func splitCSV(value string) []string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func transactionAgentAllowed(name, namespace string, allowed []string) bool {
	return slices.ContainsFunc(allowed, func(want string) bool {
		return transactionAgentMatches(name, namespace, want)
	})
}

func transactionAgentMatches(name, namespace, want string) bool {
	want = strings.TrimSpace(want)
	if want == "" || strings.TrimSpace(name) == "" {
		return false
	}
	return name == want || namespacedToolName(namespace, name) == want
}

func namespacedToolName(namespace, name string) string {
	if namespace == "" || name == "" {
		return name
	}
	return namespace + "/" + name
}

func transactionProviderAllowed(provider transactionProviderInfo, allowed []string, tokenNamespace string, hasTokenNamespace bool) bool {
	return slices.ContainsFunc(allowed, func(want string) bool {
		return transactionProviderMatches(provider, want, tokenNamespace, hasTokenNamespace)
	})
}

func transactionProviderMatches(provider transactionProviderInfo, want string, tokenNamespace string, hasTokenNamespace bool) bool {
	want = strings.TrimSpace(want)
	if want == "" {
		return false
	}
	if !transactionProviderNamespaceMatchesContext(provider, tokenNamespace, hasTokenNamespace) {
		return false
	}
	if provider.Name != "" && namespacedToolName(provider.Namespace, provider.Name) == want {
		return true
	}
	if provider.Name != "" && provider.Name == want {
		return true
	}
	return provider.Type != "" && provider.Type == want
}

func transactionModelAllowed(provider transactionProviderInfo, model string, allowed []string, tokenNamespace string, hasTokenNamespace bool) bool {
	if !transactionProviderNamespaceMatchesContext(provider, tokenNamespace, hasTokenNamespace) {
		return false
	}
	for _, want := range allowed {
		want = strings.TrimSpace(want)
		switch want {
		case "":
			continue
		case model:
			return true
		}
		if provider.Name != "" && want == provider.Name+"/"+model {
			return true
		}
		if provider.Name != "" && want == namespacedToolName(provider.Namespace, provider.Name)+"/"+model {
			return true
		}
		if provider.Type != "" && want == provider.Type+"/"+model {
			return true
		}
	}
	return false
}

func transactionProviderNamespaceMatchesContext(provider transactionProviderInfo, tokenNamespace string, hasTokenNamespace bool) bool {
	if !hasTokenNamespace {
		return true
	}
	providerNamespace := strings.TrimSpace(provider.Namespace)
	return providerNamespace == "" || providerNamespace == tokenNamespace
}

func transactionProviderDisplayName(provider transactionProviderInfo) string {
	if provider.Name != "" {
		return namespacedToolName(provider.Namespace, provider.Name)
	}
	return provider.Type
}

func workspaceGitRepo(workspace *corev1alpha1.WorkspaceConfig) string {
	if workspace == nil {
		return ""
	}
	return workspace.GitRepo
}

func workspaceBranch(workspace *corev1alpha1.WorkspaceConfig) string {
	if workspace == nil {
		return ""
	}
	return workspace.Branch
}

func workspaceRef(workspace *corev1alpha1.WorkspaceConfig) string {
	if workspace == nil {
		return ""
	}
	return workspace.Ref
}
