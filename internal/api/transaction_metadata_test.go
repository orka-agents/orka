/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDigestMapNormalizesSetValuedContextClaimOrder(t *testing.T) {
	first := map[string]any{
		"allowedProviders": []any{"openai", "anthropic"},
		"allowedTools":     []string{"web_search", "file_read"},
		"nested": map[string]any{
			"allowedModels": []any{"gpt-5.4", "claude-sonnet-4"},
		},
	}
	second := map[string]any{
		"nested": map[string]any{
			"allowedModels": []any{"claude-sonnet-4", "gpt-5.4"},
		},
		"allowedTools":     []string{"file_read", "web_search"},
		"allowedProviders": []any{"anthropic", "openai"},
	}

	firstDigest := digestMap(first)
	secondDigest := digestMap(second)
	if firstDigest == "" {
		t.Fatalf("digestMap returned empty digest")
	}
	if firstDigest != secondDigest {
		t.Fatalf("digestMap produced different digests for reordered set-valued claims: %q != %q", firstDigest, secondDigest)
	}
}

func TestDigestMapPreservesOrderForNonSetLists(t *testing.T) {
	first := digestMap(map[string]any{"sequence": []any{"first", "second"}})
	second := digestMap(map[string]any{"sequence": []any{"second", "first"}})
	if first == second {
		t.Fatalf("digestMap treated non-set list order as interchangeable: %q", first)
	}
}

func TestSafeTransactionContextOmitsLongAllowlistedValues(t *testing.T) {
	ctx := map[string]any{
		"purpose":  strings.Repeat("x", maxSafeTransactionContextValueLength+1),
		"trace_id": "trace-123",
	}

	safe := safeTransactionContext(ctx)
	if _, ok := safe["purpose"]; ok {
		t.Fatalf("safeTransactionContext included overlong purpose value")
	}
	if got := safe["trace_id"]; got != "trace-123" {
		t.Fatalf("safeTransactionContext[trace_id] = %q, want trace-123", got)
	}
}

func TestSafeTransactionContextPreservesLongRenderedAuthorizationLists(t *testing.T) {
	longTool := strings.Repeat("x", maxSafeTransactionContextValueLength)
	ctx := map[string]any{
		"allowedTools": []string{longTool},
	}

	wantBytes, err := json.Marshal([]string{longTool})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	safe := safeTransactionContext(ctx)
	if got := safe["allowedTools"]; got != string(wantBytes) {
		t.Fatalf("safeTransactionContext[allowedTools] = %q, want preserved signed constraint", got)
	}
}

func TestSafeTransactionContextPreservesAllowedAgentsAndEmptyAllowLists(t *testing.T) {
	ctx := map[string]any{
		"allowedAgents":    []any{"coordinator", "worker"},
		"allowedTools":     []string{},
		"provider":         "openai",
		"allowedProviders": []any{},
		"model":            "gpt-5.4",
		"allowedModels":    []string{},
	}

	safe := safeTransactionContext(ctx)
	if got := safe["allowedAgents"]; got != `["coordinator","worker"]` {
		t.Fatalf("safeTransactionContext[allowedAgents] = %q", got)
	}
	if got := safe["provider"]; got != "openai" {
		t.Fatalf("safeTransactionContext[provider] = %q", got)
	}
	if got := safe["model"]; got != "gpt-5.4" {
		t.Fatalf("safeTransactionContext[model] = %q", got)
	}
	for _, key := range []string{"allowedTools", "allowedProviders", "allowedModels"} {
		if got := safe[key]; got != "[]" {
			t.Fatalf("safeTransactionContext[%s] = %q, want []", key, got)
		}
	}
}

func TestTaskTransactionPreservesCredentialSecretConstraint(t *testing.T) {
	token := &ContextToken{TransactionContext: map[string]any{"secret": "resource-credential"}}
	tx := taskTransactionFromContextToken(token)
	if tx.Context["secret"] != "resource-credential" {
		t.Fatalf("context = %#v", tx.Context)
	}
}
