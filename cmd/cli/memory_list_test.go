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
	memoryListContinueKey   = "continue"
	memoryListCursorKey     = "cursor"
	memoryListItemsKey      = "items"
	memoryListMetadataKey   = "metadata"
	memoryListIDKey         = "id"
	memoryListNamespaceKey  = "namespace"
	memoryListStateKey      = "state"
	memoryListServerFlag    = "--server"
	memoryListOutputFlag    = "--output"
	memoryListCursorFlag    = "--cursor"
	memoryListContinueFlag  = "--continue"
	memoryListCursorValue   = "opaque-memory-cursor"
	memoryListRootName      = "memory"
	memoryListSubcommand    = "list"
	memoryListQueryValue    = "durable context"
	memoryListTestID        = "mem-list-1"
	memoryListActiveState   = "active"
	memoryListContinueHint  = "Continue with --cursor "
	memoryListSamePageLimit = "25"
)

func TestMemoryListForwardsCursorAliases(t *testing.T) {
	tests := []struct {
		name  string
		flags []string
	}{
		{name: memoryListCursorKey, flags: []string{memoryListCursorFlag, "  " + memoryListCursorValue + "  "}},
		{
			name:  memoryListContinueKey,
			flags: []string{memoryListContinueFlag, "  " + memoryListCursorValue + "  "},
		},
		{name: "matching aliases", flags: []string{
			memoryListCursorFlag, memoryListCursorValue,
			memoryListContinueFlag, "  " + memoryListCursorValue + "  ",
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			var gotCursor, gotContinue, gotQuery, gotLimit, gotDisabled, gotDeleted string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != memoryAPIPath {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				gotCursor = r.URL.Query().Get(memoryListCursorKey)
				gotContinue = r.URL.Query().Get(memoryListContinueKey)
				gotQuery = r.URL.Query().Get("query")
				gotLimit = r.URL.Query().Get("limit")
				gotDisabled = r.URL.Query().Get("includeDisabled")
				gotDeleted = r.URL.Query().Get("includeDeleted")
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(memoryListResponseFixture("next-page"))
			}))
			defer server.Close()

			root := newRootCmd()
			var output bytes.Buffer
			root.SetOut(&output)
			root.SetErr(&output)
			args := []string{
				memoryListRootName, memoryListSubcommand, memoryListServerFlag, server.URL,
				"--query", "  " + memoryListQueryValue + "  ", "--limit", memoryListSamePageLimit,
				"--include-disabled", "--include-deleted", memoryListOutputFlag, outputJSON,
			}
			root.SetArgs(append(args, test.flags...))
			if err := root.Execute(); err != nil {
				t.Fatalf("Execute() error: %v\n%s", err, output.String())
			}
			if gotCursor != memoryListCursorValue || gotContinue != "" {
				t.Fatalf("cursor/continue query = %q/%q, want %q/empty", gotCursor, gotContinue, memoryListCursorValue)
			}
			if gotQuery != memoryListQueryValue || gotLimit != memoryListSamePageLimit ||
				gotDisabled != cliQueryTrue || gotDeleted != cliQueryTrue {
				t.Fatalf("filter query = query:%q limit:%q disabled:%q deleted:%q",
					gotQuery, gotLimit, gotDisabled, gotDeleted)
			}
			if strings.Contains(output.String(), memoryListContinueHint) {
				t.Fatalf("JSON output contained a table-only continuation hint: %q", output.String())
			}
		})
	}
}

func TestMemoryListRejectsConflictingCursorAliases(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := newRootCmd()
	root.SetArgs([]string{
		memoryListRootName, memoryListSubcommand,
		memoryListCursorFlag, "first-page", memoryListContinueFlag, "second-page",
	})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "--cursor and --continue must match") {
		t.Fatalf("Execute() error = %v, want conflicting alias rejection", err)
	}
}

func TestMemoryListTableShowsContinuationHint(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	server := newMemoryListTestServer(t, memoryListResponseFixture(memoryListCursorValue))
	defer server.Close()

	root := newRootCmd()
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs([]string{memoryListRootName, memoryListSubcommand, memoryListServerFlag, server.URL})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error: %v\n%s", err, output.String())
	}
	for _, expected := range []string{
		memoryListTestID, memoryListContinueHint + memoryListCursorValue,
		"reuse the same filters and --limit",
	} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("table output %q does not contain %q", output.String(), expected)
		}
	}
}

func TestMemoryListStructuredOutputsPreserveContinuation(t *testing.T) {
	for _, format := range []string{outputJSON, outputYAML} {
		t.Run(format, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			server := newMemoryListTestServer(t, memoryListResponseFixture(memoryListCursorValue))
			defer server.Close()

			root := newRootCmd()
			var output bytes.Buffer
			root.SetOut(&output)
			root.SetErr(&output)
			root.SetArgs([]string{
				memoryListRootName, memoryListSubcommand, memoryListServerFlag, server.URL, memoryListOutputFlag, format,
			})
			if err := root.Execute(); err != nil {
				t.Fatalf("Execute() error: %v\n%s", err, output.String())
			}
			if !strings.Contains(output.String(), memoryListCursorValue) {
				t.Fatalf("%s output hid continuation token: %q", format, output.String())
			}
			if strings.Contains(output.String(), memoryListContinueHint) {
				t.Fatalf("%s output contained a table-only continuation hint: %q", format, output.String())
			}
		})
	}
}

func newMemoryListTestServer(t *testing.T, response map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != memoryAPIPath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	}))
}

func memoryListResponseFixture(next string) map[string]any {
	return map[string]any{
		memoryListItemsKey: []any{map[string]any{
			memoryListIDKey:        memoryListTestID,
			memoryListNamespaceKey: defaultNamespace,
			memoryListStateKey:     memoryListActiveState,
		}},
		memoryListMetadataKey: map[string]any{memoryListContinueKey: next},
	}
}
