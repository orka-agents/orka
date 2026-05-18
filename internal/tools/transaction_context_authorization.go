package tools

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
)

func validateChildTaskAgainstParentTransaction(parent, child *corev1alpha1.Task, agentName string) error {
	if parent == nil || parent.Spec.Transaction == nil || child == nil {
		return nil
	}
	txCtx := parent.Spec.Transaction.Context
	if len(txCtx) == 0 {
		return nil
	}
	if agentName == "" && child.Spec.AgentRef != nil {
		agentName = child.Spec.AgentRef.Name
	}
	agentNamespace := child.Namespace
	if child.Spec.AgentRef != nil && child.Spec.AgentRef.Namespace != "" {
		agentNamespace = child.Spec.AgentRef.Namespace
	}

	if want := strings.TrimSpace(txCtx["namespace"]); want != "" && child.Namespace != want {
		return fmt.Errorf("child task namespace %q does not match transaction context %q", child.Namespace, want)
	}
	if want := strings.TrimSpace(txCtx["taskType"]); want != "" && string(child.Spec.Type) != want {
		return fmt.Errorf("child task type %q does not match transaction context %q", child.Spec.Type, want)
	}
	if want := strings.TrimSpace(txCtx["agent"]); want != "" && !transactionAgentMatches(agentName, agentNamespace, want) {
		return fmt.Errorf("child task agent %q does not match transaction context %q", namespacedToolName(agentNamespace, agentName), want)
	}
	if allowed, ok := transactionContextStringList(txCtx["allowedAgents"]); ok && !transactionAgentAllowed(agentName, agentNamespace, allowed) {
		return fmt.Errorf("child task agent %q is not allowed by transaction context", namespacedToolName(agentNamespace, agentName))
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
	return nil
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
