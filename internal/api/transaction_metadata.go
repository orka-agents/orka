/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"sort"
	"strconv"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/taskmeta"
)

var safeTransactionContextKeys = []string{
	"purpose",
	"namespace",
	"taskType",
	"agent",
	"allowedAgents",
	"repo",
	"branch",
	"ref",
	"maxDepth",
	"allowedTools",
	"provider",
	"allowedProviders",
	"model",
	"allowedModels",
	"e2e",
	"trace_id",
	"secret",
}

const maxSafeTransactionContextValueLength = 1024

var setValuedContextDigestKeys = map[string]struct{}{
	"allowedAgents":    {},
	"allowedModels":    {},
	"allowedProviders": {},
	"allowedTools":     {},
}

var authorizationTransactionContextKeys = map[string]struct{}{
	"namespace":        {},
	"taskType":         {},
	"agent":            {},
	"allowedAgents":    {},
	"repo":             {},
	"branch":           {},
	"ref":              {},
	"maxDepth":         {},
	"allowedTools":     {},
	"provider":         {},
	"allowedProviders": {},
	"model":            {},
	"allowedModels":    {},
}

func stampTaskRequesterFromUserInfo(task *corev1alpha1.Task, ui *UserInfo) {
	if task == nil || ui == nil || (ui.AuthType != AuthTypeOIDC && ui.AuthType != AuthTypeContextToken) {
		return
	}

	task.Spec.RequestedBy = &corev1alpha1.RequestedBy{
		Subject:  ui.Subject,
		Issuer:   ui.Issuer,
		Username: ui.Username,
		Email:    ui.Email,
		Groups:   append([]string{}, ui.Groups...),
		Roles:    append([]string{}, ui.Roles...),
	}

	if ui.AuthType == AuthTypeContextToken {
		task.Spec.Transaction = taskTransactionFromContextToken(ui.ContextToken)
		taskmeta.ApplyTransactionMetadata(&task.ObjectMeta, task.Spec.Transaction)
	}
}

func taskTransactionFromContextToken(token *ContextToken) *corev1alpha1.TaskTransaction {
	if token == nil {
		return nil
	}

	return &corev1alpha1.TaskTransaction{
		Profile:                token.Profile,
		ID:                     token.TransactionID,
		Issuer:                 token.Issuer,
		Audience:               append([]string{}, token.Audience...),
		Subject:                token.Subject,
		RequestingWorkload:     token.RequestingWorkload,
		Scope:                  token.Scope,
		Scopes:                 append([]string{}, token.Scopes...),
		ContextDigest:          digestMap(token.TransactionContext),
		RequesterContextDigest: digestMap(token.RequesterContext),
		Context:                safeTransactionContext(token.TransactionContext),
	}
}

func digestMap(value map[string]any) string {
	if len(value) == 0 {
		return ""
	}
	encoded, err := json.Marshal(canonicalizeContextDigestValue("", value))
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func canonicalizeContextDigestValue(key string, value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for childKey, childValue := range v {
			out[childKey] = canonicalizeContextDigestValue(childKey, childValue)
		}
		return out
	case []any:
		return canonicalizeContextDigestList(key, v)
	case []string:
		out := make([]any, 0, len(v))
		for _, item := range v {
			out = append(out, item)
		}
		return canonicalizeContextDigestList(key, out)
	default:
		return value
	}
}

func canonicalizeContextDigestList(key string, values []any) []any {
	out := make([]any, 0, len(values))
	for _, value := range values {
		out = append(out, canonicalizeContextDigestValue(key, value))
	}
	if _, ok := setValuedContextDigestKeys[key]; !ok {
		return out
	}
	sort.SliceStable(out, func(i, j int) bool {
		return contextDigestSortKey(out[i]) < contextDigestSortKey(out[j])
	})
	return out
}

func contextDigestSortKey(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func safeTransactionContext(value map[string]any) map[string]string {
	if len(value) == 0 {
		return nil
	}

	out := map[string]string{}
	for _, key := range safeTransactionContextKeys {
		raw, ok := value[key]
		if !ok {
			continue
		}
		if rendered, ok := renderSafeTransactionContextValue(key, raw); ok && safeTransactionContextValueAllowed(key, rendered) {
			out[key] = rendered
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func renderSafeTransactionContextValue(key string, value any) (string, bool) {
	switch v := value.(type) {
	case string:
		if v == "" {
			return "", false
		}
		return v, true
	case bool:
		return strconv.FormatBool(v), true
	case json.Number:
		return v.String(), true
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), true
	case int:
		return strconv.Itoa(v), true
	case int64:
		return strconv.FormatInt(v, 10), true
	case []string:
		if len(v) == 0 && !safeTransactionContextPreservesEmptyList(key) {
			return "", false
		}
		encoded, err := json.Marshal(v)
		return string(encoded), err == nil
	case []any:
		if len(v) == 0 {
			if safeTransactionContextPreservesEmptyList(key) {
				return "[]", true
			}
			return "", false
		}
		if !safeScalarList(v) {
			return "", false
		}
		encoded, err := json.Marshal(v)
		return string(encoded), err == nil
	default:
		return "", false
	}
}

func safeTransactionContextPreservesEmptyList(key string) bool {
	_, ok := setValuedContextDigestKeys[key]
	return ok
}

func safeTransactionContextValueAllowed(key, rendered string) bool {
	if len(rendered) <= maxSafeTransactionContextValueLength {
		return true
	}
	_, ok := authorizationTransactionContextKeys[key]
	return ok
}

func safeScalarList(values []any) bool {
	return slices.IndexFunc(values, func(value any) bool {
		switch value.(type) {
		case string, bool, json.Number, float64, int, int64:
			return false
		default:
			return true
		}
	}) == -1
}
