/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// classifyChatK8sErr returns a ChatToolResult for common K8s API errors.
func classifyChatK8sErr(err error) (string, error) {
	if apierrors.IsNotFound(err) {
		return ChatToolErrorResult("not_found", err.Error(), "Check the resource name and namespace")
	}
	if apierrors.IsAlreadyExists(err) {
		return ChatToolErrorResult("already_exists", err.Error(), "Use a different name or delete the existing resource first")
	}
	if apierrors.IsForbidden(err) {
		return ChatToolErrorResult("permission_denied", err.Error(), "Check RBAC permissions")
	}
	return ChatToolErrorResult("internal_error", err.Error(), "")
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

// parseDurationArg parses a duration string from args and returns an error result if invalid.
func parseDurationArg(args map[string]any, key string) (time.Duration, string, bool) {
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
