package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

const (
	memoryGetRootName            = "memory"
	memoryGetSubcommand          = "get"
	memoryGetServerFlag          = "--server"
	memoryGetIncludeDisabledFlag = "--include-disabled"
	memoryGetQueryKey            = "includeDisabled"
	memoryGetTestID              = "mem-disabled"
	memoryGetIDKey               = "id"
	memoryGetNamespaceKey        = "namespace"
	memoryGetDisabledKey         = "disabled"
)

func TestMemoryGetIncludeDisabledForwarding(t *testing.T) {
	tests := []struct {
		name          string
		args          []string
		wantParameter bool
	}{
		{name: "default visibility"},
		{name: "include disabled", args: []string{memoryGetIncludeDisabledFlag}, wantParameter: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			var gotValue string
			var gotParameter bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != memoryAPIPath+"/"+memoryGetTestID {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				gotValue = r.URL.Query().Get(memoryGetQueryKey)
				gotParameter = r.URL.Query().Has(memoryGetQueryKey)
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					memoryGetIDKey: memoryGetTestID, memoryGetNamespaceKey: defaultNamespace, memoryGetDisabledKey: true,
				})
			}))
			defer server.Close()

			root := newRootCmd()
			var output bytes.Buffer
			root.SetOut(&output)
			root.SetErr(&output)
			args := []string{
				memoryGetRootName, memoryGetSubcommand, memoryGetTestID,
				memoryGetServerFlag, server.URL,
			}
			root.SetArgs(append(args, test.args...))
			if err := root.Execute(); err != nil {
				t.Fatalf("Execute() error: %v\n%s", err, output.String())
			}
			if gotParameter != test.wantParameter {
				t.Fatalf("includeDisabled parameter present = %t, want %t", gotParameter, test.wantParameter)
			}
			if test.wantParameter && gotValue != cliQueryTrue {
				t.Fatalf("includeDisabled query = %q, want %q", gotValue, cliQueryTrue)
			}
			if !test.wantParameter && gotValue != "" {
				t.Fatalf("default includeDisabled query = %q, want empty", gotValue)
			}
		})
	}
}
