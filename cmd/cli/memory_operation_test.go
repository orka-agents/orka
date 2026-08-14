package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	memoryOperationContinueKey  = "continue"
	memoryOperationIDKey        = "id"
	memoryOperationItemsKey     = "items"
	memoryOperationMetadataKey  = "metadata"
	memoryOperationNamespaceKey = "namespace"
	memoryOperationStateKey     = "state"
	memoryOperationServerFlag   = "--server"
	memoryOperationCursorFlag   = "--cursor"
	memoryOperationContinueFlag = "--continue"
	memoryOperationQueuedState  = "queued"
	memoryOperationCursorValue  = "opaque-page-token"
	memoryOperationListTestID   = "mop-1"
	memoryCommandName           = "memory"
	memoryOperationCommandName  = "operation"
	memoryOperationListName     = "list"
)

func TestMemoryOperationListForwardsCursorAliases(t *testing.T) {
	tests := []struct {
		name  string
		flags []string
	}{
		{name: "cursor", flags: []string{memoryOperationCursorFlag, "  " + memoryOperationCursorValue + "  "}},
		{
			name:  memoryOperationContinueKey,
			flags: []string{memoryOperationContinueFlag, "  " + memoryOperationCursorValue + "  "},
		},
		{name: "matching aliases", flags: []string{
			memoryOperationCursorFlag, memoryOperationCursorValue,
			memoryOperationContinueFlag, "  " + memoryOperationCursorValue + "  ",
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			var gotCursor, gotState, gotLimit, gotContinue string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/api/v1/memory-operations" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				gotCursor = r.URL.Query().Get("cursor")
				gotContinue = r.URL.Query().Get(memoryOperationContinueKey)
				gotState = r.URL.Query().Get("state")
				gotLimit = r.URL.Query().Get("limit")
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					memoryOperationItemsKey:    []any{},
					memoryOperationMetadataKey: map[string]any{memoryOperationContinueKey: "next-page"},
				})
			}))
			defer server.Close()

			root := newRootCmd()
			var output bytes.Buffer
			root.SetOut(&output)
			root.SetErr(&output)
			args := []string{
				memoryCommandName, memoryOperationCommandName, memoryOperationListName, memoryOperationServerFlag, server.URL,
				"--state", " " + memoryOperationQueuedState + " ", "--limit", "25", "--output", outputJSON,
			}
			root.SetArgs(append(args, test.flags...))
			if err := root.Execute(); err != nil {
				t.Fatalf("Execute() error: %v\n%s", err, output.String())
			}
			if gotCursor != memoryOperationCursorValue {
				t.Fatalf("cursor query = %q, want %s", gotCursor, memoryOperationCursorValue)
			}
			if gotContinue != "" {
				t.Fatalf("continue query = %q, want canonical cursor parameter only", gotContinue)
			}
			if gotState != memoryOperationQueuedState || gotLimit != "25" {
				t.Fatalf("state/limit query = %q/%q, want queued/25", gotState, gotLimit)
			}
			if strings.Contains(output.String(), "Continue with --cursor") {
				t.Fatalf("JSON output contained a table-only continuation hint: %q", output.String())
			}
		})
	}
}

func TestMemoryOperationListRejectsConflictingCursorAliases(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := newRootCmd()
	root.SetArgs([]string{
		memoryCommandName, memoryOperationCommandName, memoryOperationListName,
		memoryOperationCursorFlag, "first-page", memoryOperationContinueFlag, "second-page",
	})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "--cursor and --continue must match") {
		t.Fatalf("Execute() error = %v, want conflicting alias rejection", err)
	}
}

func TestMemoryOperationListTableShowsContinuationHint(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/memory-operations" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			memoryOperationItemsKey: []any{map[string]any{
				memoryOperationIDKey:        memoryOperationListTestID,
				memoryOperationNamespaceKey: defaultNamespace,
				memoryOperationStateKey:     memoryOperationQueuedState,
			}},
			memoryOperationMetadataKey: map[string]any{memoryOperationContinueKey: memoryOperationCursorValue},
		})
	}))
	defer server.Close()

	root := newRootCmd()
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs([]string{
		memoryCommandName, memoryOperationCommandName, memoryOperationListName,
		memoryOperationServerFlag, server.URL,
	})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error: %v\n%s", err, output.String())
	}
	for _, expected := range []string{
		memoryOperationListTestID, "Continue with --cursor " + memoryOperationCursorValue,
		"reuse the same filters and --limit",
	} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("table output %q does not contain %q", output.String(), expected)
		}
	}
}
