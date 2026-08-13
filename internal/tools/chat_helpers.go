/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

// classifyChatK8sErr returns a ChatToolResult for common K8s API errors.
func classifyChatK8sErr(err error) (string, error) {
	if apierrors.IsNotFound(err) {
		return ChatToolErrorResult(errTypeNotFound, err.Error(), "Check the resource name and namespace")
	}
	if apierrors.IsAlreadyExists(err) {
		return ChatToolErrorResult("already_exists", err.Error(), "Use a different name or delete the existing resource first")
	}
	if apierrors.IsForbidden(err) {
		return ChatToolErrorResult("permission_denied", err.Error(), "Check RBAC permissions")
	}
	return ChatToolErrorResult(internalErrorType, err.Error(), "")
}

// checkChatNamespaceScope validates namespace access using ToolContext.
func checkChatNamespaceScope(tc *ToolContext, namespace string) (string, bool) {
	if tc.WatchNamespace != "" && namespace != tc.WatchNamespace {
		r, _ := ChatToolErrorResult("permission_denied",
			fmt.Sprintf("cannot create resources in namespace %q, restricted to %q", namespace, tc.WatchNamespace),
			"Use the allowed namespace")
		return r, false
	}
	if tc.EnforceNamespaceIsolation && namespace != tc.Namespace {
		r, _ := ChatToolErrorResult("permission_denied",
			fmt.Sprintf("cannot create resources in namespace %q, restricted to %q", namespace, tc.Namespace),
			"Use your namespace")
		return r, false
	}
	return "", true
}

// chatGetStringArg extracts a string argument from a map.
func chatGetStringArg(args map[string]any, key string) string {
	v, ok := args[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return fmt.Sprintf("%v", v)
	}
	return s
}

// chatGetStringArgDefault extracts a string argument with a default.
func chatGetStringArgDefault(args map[string]any, key, defaultVal string) string {
	v := chatGetStringArg(args, key)
	if v == "" {
		return defaultVal
	}
	return v
}

// chatGetIntArg extracts an integer argument with a default.
func chatGetIntArg(args map[string]any, key string, defaultVal int) int {
	v, ok := args[key]
	if !ok {
		return defaultVal
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	default:
		return defaultVal
	}
}

// chatParseBoolArg parses a bool tool argument that may arrive as a JSON
// boolean or a string boolean.
func chatParseBoolArg(value any) (bool, error) {
	switch v := value.(type) {
	case bool:
		return v, nil
	case string:
		return strconv.ParseBool(strings.TrimSpace(v))
	default:
		return false, fmt.Errorf("value is not a boolean")
	}
}

// chatParseIntArg parses an integer tool argument that may arrive as a JSON
// number or a numeric string.
func chatParseIntArg(value any) (int, error) {
	switch v := value.(type) {
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) || v != math.Trunc(v) {
			return 0, fmt.Errorf("value is not an integer")
		}
		return int(v), nil
	case int:
		return v, nil
	case int64:
		return int(v), nil
	case string:
		return strconv.Atoi(strings.TrimSpace(v))
	default:
		return 0, fmt.Errorf("value is not an integer")
	}
}

// chatGetStringSliceArg extracts a string slice argument.
func chatGetStringSliceArg(args map[string]any, key string) []string {
	v, ok := args[key]
	if !ok {
		return nil
	}
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	result := make([]string, 0, len(arr))
	for _, item := range arr {
		if s, ok := item.(string); ok {
			result = append(result, s)
		} else {
			result = append(result, fmt.Sprintf("%v", item))
		}
	}
	return result
}

// parseTimeoutArg parses a timeout duration string from args and returns an error result if invalid.
func parseTimeoutArg(args map[string]any) (time.Duration, string, bool) {
	const key = "timeout"
	s := chatGetStringArg(args, key)
	if s == "" {
		return 0, "", true
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		r, _ := ChatToolErrorResult("invalid_arguments",
			fmt.Sprintf("invalid %s: %v", key, err),
			"Use Go duration format (e.g., 30s, 5m)")
		return 0, r, false
	}
	return d, "", true
}

// maxTurns bounds mirror the Task CRD validation on spec.agentRuntime.maxTurns
// (+kubebuilder:validation:Minimum=1, +kubebuilder:validation:Maximum=1000).
const (
	minMaxTurns = 1
	maxMaxTurns = 1000
)

// parseMaxTurnsArg parses the optional maxTurns argument and returns an error
// result if invalid.
func parseMaxTurnsArg(args map[string]any) (*int32, string, bool) {
	raw, ok := args["maxTurns"]
	if !ok {
		return nil, "", true
	}
	turns, err := chatParseIntArg(raw)
	if err != nil {
		r, _ := ChatToolErrorResult("invalid_arguments",
			"maxTurns must be an integer",
			"Provide maxTurns as a positive integer or omit it")
		return nil, r, false
	}
	if turns < minMaxTurns || turns > maxMaxTurns {
		r, _ := ChatToolErrorResult("invalid_arguments",
			fmt.Sprintf("maxTurns must be between %d and %d", minMaxTurns, maxMaxTurns),
			"Provide maxTurns within the allowed range or omit it")
		return nil, r, false
	}
	value := int32(turns)
	return &value, "", true
}

// workspaceRequestsPublication reports whether any publication field upgrades
// the workspace to write intent.
func workspaceRequestsPublication(wsCfg *corev1alpha1.WorkspaceConfig) bool {
	return wsCfg.PublicationGitRepo != "" || wsCfg.PushBranch != "" || wsCfg.PRBaseBranch != "" || wsCfg.CreatePR
}

// parseCreatePRArg parses the optional workspace.createPR argument and returns
// an error result if invalid.
func parseCreatePRArg(wsMap map[string]any) (bool, string, bool) {
	raw, ok := wsMap["createPR"]
	if !ok {
		return false, "", true
	}
	createPR, err := chatParseBoolArg(raw)
	if err != nil {
		r, _ := ChatToolErrorResult("invalid_arguments",
			"workspace.createPR must be a boolean",
			"Set createPR to true or false")
		return false, r, false
	}
	return createPR, "", true
}

// taskCreatedMsg returns the appropriate message for a created task.
func taskCreatedMsg(schedule string) string {
	if schedule != "" {
		return fmt.Sprintf("Recurring task scheduled (schedule: %s)", schedule)
	}
	return "Task created"
}

// splitModelString splits a "provider/model" string.
func splitModelString(s string) (provider, name string) {
	parts := strings.SplitN(s, "/", 2)
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	return "", s
}
