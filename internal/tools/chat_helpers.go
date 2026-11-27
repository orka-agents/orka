/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

const chatTaskCreateTimeout = 5 * time.Second

func createChatTask(ctx context.Context, c client.Client, task *corev1alpha1.Task) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	createCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), chatTaskCreateTimeout)
	defer cancel()
	return c.Create(createCtx, task)
}

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

func requireChatObjectArg(a map[string]any, key string) (string, bool) {
	value, ok := a[key]
	if !ok || value == nil {
		return "", true
	}
	if _, isObject := value.(map[string]any); isObject {
		return "", true
	}
	result, _ := ChatToolErrorResult(invalidArgumentsErrorType, key+" must be an object", "Pass "+key+" as a JSON object")
	return result, false
}

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

func chatGetStringArgDefault(args map[string]any, key, defaultVal string) string {
	v := chatGetStringArg(args, key)
	if v == "" {
		return defaultVal
	}
	return v
}

func chatGetIntArg(args map[string]any, key string, defaultVal int) int {
	v, ok := args[key]
	if !ok {
		return defaultVal
	}
	switch n := v.(type) {
	case float64:
		switch {
		case math.IsNaN(n):
			return defaultVal
		case n >= math.MaxInt:
			return math.MaxInt
		case n <= math.MinInt:
			return math.MinInt
		}
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	default:
		return defaultVal
	}
}

func chatPriorityArg(a map[string]any) (*int32, string, bool) {
	value := a[priorityField]
	if value == nil {
		return nil, "", true
	}
	number, ok := value.(float64)
	if !ok || number != math.Trunc(number) || number < 0 || number > 1000 {
		result, _ := ChatToolErrorResult(invalidArgumentsErrorType, "priority must be a whole number from 0 to 1000", "Use a priority from 0 to 1000; the default is 500")
		return nil, result, false
	}
	p := int32(number)
	return &p, "", true
}

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

const (
	minMaxTurns = 1
	maxMaxTurns = 1000
)

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

func workspaceRequestsPublication(wsCfg *corev1alpha1.WorkspaceConfig) bool {
	return wsCfg.PublicationGitRepo != "" || wsCfg.PushBranch != "" || wsCfg.PRBaseBranch != "" || wsCfg.CreatePR
}

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

func taskCreatedMsg(schedule string) string {
	if schedule != "" {
		return fmt.Sprintf("Recurring task scheduled (schedule: %s)", schedule)
	}
	return "Task created"
}

func splitModelString(s string) (provider, name string) {
	parts := strings.SplitN(s, "/", 2)
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	return "", s
}
