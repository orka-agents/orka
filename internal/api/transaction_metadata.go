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
	"strconv"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
)

var safeTransactionContextKeys = []string{
	"purpose",
	"namespace",
	"taskType",
	"agent",
	"repo",
	"branch",
	"ref",
	"maxDepth",
	"allowedTools",
	"allowedProviders",
	"allowedModels",
	"e2e",
	"trace_id",
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
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:])
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
		if rendered, ok := renderSafeTransactionContextValue(raw); ok {
			out[key] = rendered
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func renderSafeTransactionContextValue(value any) (string, bool) {
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
		if len(v) == 0 {
			return "", false
		}
		encoded, err := json.Marshal(v)
		return string(encoded), err == nil
	case []any:
		if len(v) == 0 || !safeScalarList(v) {
			return "", false
		}
		encoded, err := json.Marshal(v)
		return string(encoded), err == nil
	default:
		return "", false
	}
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
