/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

const (
	testCheckMessagesName = checkMessagesToolName
	queryValFalse         = "false"
)

func TestCheckMessagesTool_Name(t *testing.T) {
	tool := NewCheckMessagesTool()
	if got := tool.Name(); got != testCheckMessagesName {
		t.Errorf("Name() = %v, want %v", got, testCheckMessagesName)
	}
}

func TestCheckMessagesTool_Description(t *testing.T) {
	tool := NewCheckMessagesTool()
	if got := tool.Description(); got == "" {
		t.Error("Description() returned empty string")
	}
}

func TestCheckMessagesTool_Execute(t *testing.T) {
	tests := []struct {
		name       string
		args       *CheckMessagesArgs
		envVars    map[string]string
		serverCode int
		serverBody string
		wantErr    bool
		wantMsg    string
	}{
		{
			name: "no new messages",
			envVars: map[string]string{
				envOrkaTaskName:      testWorkerAName,
				envOrkaTaskNamespace: defaultNamespace,
				envOrkaParentTask:    testCoordinatorTaskName,
			},
			serverCode: http.StatusOK,
			serverBody: `[]`,
			wantMsg:    noNewMessagesText,
		},
		{
			name: "has messages",
			envVars: map[string]string{
				envOrkaTaskName:      testWorkerAName,
				envOrkaTaskNamespace: defaultNamespace,
				envOrkaParentTask:    testCoordinatorTaskName,
			},
			serverCode: http.StatusOK,
			serverBody: `[{"id":1,"fromTask":"worker-b","toTask":"worker-a","content":"found issue"}]`,
			wantMsg:    `[{"id":1,"fromTask":"worker-b","toTask":"worker-a","content":"found issue"}]`,
		},
		{
			name: "mark_read false",
			args: func() *CheckMessagesArgs {
				f := false
				return &CheckMessagesArgs{MarkRead: &f}
			}(),
			envVars: map[string]string{
				envOrkaTaskName:      testWorkerAName,
				envOrkaTaskNamespace: defaultNamespace,
				envOrkaParentTask:    testCoordinatorTaskName,
			},
			serverCode: http.StatusOK,
			serverBody: `[]`,
			wantMsg:    noNewMessagesText,
		},
		{
			name:    "missing env vars",
			envVars: map[string]string{},
			wantErr: true,
		},
		{
			name: serverErrorMessage,
			envVars: map[string]string{
				envOrkaTaskName:      testWorkerAName,
				envOrkaTaskNamespace: defaultNamespace,
				envOrkaParentTask:    testCoordinatorTaskName,
			},
			serverCode: http.StatusInternalServerError,
			serverBody: "internal error",
			wantErr:    true,
		},
		{
			name: "nil args (defaults)",
			envVars: map[string]string{
				envOrkaTaskName:      testWorkerAName,
				envOrkaTaskNamespace: defaultNamespace,
				envOrkaParentTask:    testCoordinatorTaskName,
			},
			serverCode: http.StatusOK,
			serverBody: `[]`,
			wantMsg:    noNewMessagesText,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Clear env vars first
			for _, k := range []string{envOrkaTaskName, envOrkaTaskNamespace, envOrkaParentTask, envOrkaControllerURL} {
				t.Setenv(k, "")
			}

			if tt.serverCode != 0 {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodGet {
						t.Errorf("expected GET, got %s", r.Method)
					}
					// Verify markRead query param
					if tt.args != nil && tt.args.MarkRead != nil && !*tt.args.MarkRead {
						if r.URL.Query().Get("markRead") != queryValFalse {
							t.Errorf("expected markRead=false, got %s", r.URL.Query().Get("markRead"))
						}
					}
					w.WriteHeader(tt.serverCode)
					w.Write([]byte(tt.serverBody)) //nolint:errcheck
				}))
				defer server.Close()
				tt.envVars[envOrkaControllerURL] = server.URL
			}

			for k, v := range tt.envVars {
				t.Setenv(k, v)
			}

			tool := NewCheckMessagesTool()
			var argsJSON json.RawMessage
			if tt.args != nil {
				argsJSON, _ = json.Marshal(tt.args)
			}
			result, err := tool.Execute(context.Background(), argsJSON)

			if tt.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if result != tt.wantMsg {
				t.Errorf("result = %q, want %q", result, tt.wantMsg)
			}
		})
	}
}

func TestCheckMessagesTool_Parameters(t *testing.T) {
	tool := NewCheckMessagesTool()
	params := tool.Parameters()
	if params == nil {
		t.Fatal("Parameters() returned nil")
	}

	var schema map[string]any
	if err := json.Unmarshal(params, &schema); err != nil {
		t.Fatalf("Parameters() returned invalid JSON: %v", err)
	}
	if schema[jsonSchemaTypeField] != typeObject {
		t.Error("schema type should be object")
	}
	props, ok := schema[jsonSchemaPropertiesField].(map[string]any)
	if !ok {
		t.Fatal("schema missing properties")
	}
	if _, ok := props["mark_read"]; !ok {
		t.Error("schema missing mark_read property")
	}
}
