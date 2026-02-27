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

func TestSendMessageTool_Name(t *testing.T) {
	tool := NewSendMessageTool()
	if got := tool.Name(); got != sendMessageToolName {
		t.Errorf("Name() = %v, want %v", got, sendMessageToolName)
	}
}

func TestSendMessageTool_Description(t *testing.T) {
	tool := NewSendMessageTool()
	if got := tool.Description(); got == "" {
		t.Error("Description() returned empty string")
	}
}

func TestSendMessageTool_Parameters(t *testing.T) {
	tool := NewSendMessageTool()
	params := tool.Parameters()
	if params == nil {
		t.Fatal("Parameters() returned nil")
	}
	var schema map[string]any
	if err := json.Unmarshal(params, &schema); err != nil {
		t.Fatalf("Parameters() returned invalid JSON: %v", err)
	}
	if schema["type"] != typeObject {
		t.Error("Parameters schema should have type: object")
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatal("missing properties")
	}
	if _, ok := props["to_task"]; !ok {
		t.Error("missing to_task property")
	}
	if _, ok := props["content"]; !ok {
		t.Error("missing content property")
	}
}

func TestSendMessageTool_Execute(t *testing.T) {
	tests := []struct {
		name       string
		args       SendMessageArgs
		envVars    map[string]string
		serverCode int
		wantErr    bool
		wantMsg    string
	}{
		{
			name: "send to specific sibling",
			args: SendMessageArgs{
				ToTask:  "sibling-1",
				Content: "found a bug in auth module",
			},
			envVars: map[string]string{
				"ORKA_TASK_NAME":      "worker-a",
				"ORKA_TASK_NAMESPACE": "default",
				"ORKA_PARENT_TASK":    "coordinator",
			},
			serverCode: http.StatusNoContent,
			wantMsg:    "Message sent to sibling-1",
		},
		{
			name: "broadcast to all siblings",
			args: SendMessageArgs{
				ToTask:  "*",
				Content: "phase 1 complete",
			},
			envVars: map[string]string{
				"ORKA_TASK_NAME":      "worker-a",
				"ORKA_TASK_NAMESPACE": "default",
				"ORKA_PARENT_TASK":    "coordinator",
			},
			serverCode: http.StatusNoContent,
			wantMsg:    "Message sent to all siblings",
		},
		{
			name: "missing to_task",
			args: SendMessageArgs{
				Content: "hello",
			},
			envVars: map[string]string{
				"ORKA_TASK_NAME":      "worker-a",
				"ORKA_TASK_NAMESPACE": "default",
				"ORKA_PARENT_TASK":    "coordinator",
				"ORKA_CONTROLLER_URL": "http://localhost",
			},
			wantErr: true,
		},
		{
			name: "missing content",
			args: SendMessageArgs{
				ToTask: "sibling-1",
			},
			envVars: map[string]string{
				"ORKA_TASK_NAME":      "worker-a",
				"ORKA_TASK_NAMESPACE": "default",
				"ORKA_PARENT_TASK":    "coordinator",
				"ORKA_CONTROLLER_URL": "http://localhost",
			},
			wantErr: true,
		},
		{
			name: "missing env vars",
			args: SendMessageArgs{
				ToTask:  "sibling-1",
				Content: "hello",
			},
			envVars: map[string]string{},
			wantErr: true,
		},
		{
			name: "server error",
			args: SendMessageArgs{
				ToTask:  "sibling-1",
				Content: "hello",
			},
			envVars: map[string]string{
				"ORKA_TASK_NAME":      "worker-a",
				"ORKA_TASK_NAMESPACE": "default",
				"ORKA_PARENT_TASK":    "coordinator",
			},
			serverCode: http.StatusInternalServerError,
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Clear env vars first
			for _, k := range []string{"ORKA_TASK_NAME", "ORKA_TASK_NAMESPACE", "ORKA_PARENT_TASK", "ORKA_CONTROLLER_URL"} {
				t.Setenv(k, "")
			}

			var server *httptest.Server
			if tt.serverCode != 0 {
				server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodPost {
						t.Errorf("expected POST, got %s", r.Method)
					}
					w.WriteHeader(tt.serverCode)
				}))
				defer server.Close()
				tt.envVars["ORKA_CONTROLLER_URL"] = server.URL
			}

			for k, v := range tt.envVars {
				t.Setenv(k, v)
			}

			tool := NewSendMessageTool()
			argsJSON, _ := json.Marshal(tt.args)
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
