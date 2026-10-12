/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/workerenv"
)

const testCancelTaskName = cancelTaskToolName

func TestCancelTaskTool_Name(t *testing.T) {
	tool := NewCancelTaskTool()
	if got := tool.Name(); got != testCancelTaskName {
		t.Errorf("Name() = %v, want %v", got, testCancelTaskName)
	}
}

func TestCancelTaskTool_Description(t *testing.T) {
	tool := NewCancelTaskTool()
	if got := tool.Description(); got == "" {
		t.Error("Description() returned empty string")
	}
}

func TestCancelTaskTool_Parameters(t *testing.T) {
	tool := NewCancelTaskTool()
	params := tool.Parameters()
	if params == nil {
		t.Fatal("Parameters() returned nil")
	}
	var schema map[string]any
	if err := json.Unmarshal(params, &schema); err != nil {
		t.Fatalf("Parameters() returned invalid JSON: %v", err)
	}
	if schema[jsonSchemaTypeField] != typeObject {
		t.Error("Parameters schema should have type: object")
	}
	props, ok := schema[jsonSchemaPropertiesField].(map[string]any)
	if !ok {
		t.Fatal("missing properties")
	}
	if _, ok := props[taskNameField]; !ok {
		t.Error("missing task_name property")
	}
	if _, ok := props[namespaceField]; !ok {
		t.Error("missing namespace property")
	}
	if _, ok := props["reason"]; !ok {
		t.Error("missing reason property")
	}
}

type cancelTaskRequest struct {
	method, path, auth, reason string
}

// startCancelTaskController serves the controller's cancel endpoint and
// records every request it receives.
func startCancelTaskController(t *testing.T, status int, body string) *[]cancelTaskRequest {
	t.Helper()
	var requests []cancelTaskRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		var payload struct {
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal(data, &payload)
		requests = append(requests, cancelTaskRequest{
			method: r.Method, path: r.URL.Path, auth: r.Header.Get("Authorization"), reason: payload.Reason,
		})
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	t.Setenv(workerenv.ControllerURL, server.URL)
	t.Setenv(workerenv.TaskName, parentTaskName)
	t.Setenv(workerenv.TaskNamespace, defaultNamespace)
	t.Setenv(workerenv.ServiceAccountToken, "worker-token")
	return &requests
}

func TestCancelTaskTool_ExecuteAsksControllerToCancelChild(t *testing.T) {
	const result = `{"taskName":"child-task-1","status":"cancellation requested"}`
	requests := startCancelTaskController(t, http.StatusOK, result)

	got, err := NewCancelTaskTool().Execute(context.Background(),
		json.RawMessage(`{"task_name":"child-task-1","reason":"wrong approach"}`))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got != result {
		t.Fatalf("Execute() = %q, want the controller result %q", got, result)
	}
	want := cancelTaskRequest{
		method: http.MethodPost,
		path:   "/internal/v1/tasks/" + defaultNamespace + "/" + parentTaskName + "/children/child-task-1/cancel",
		auth:   "Bearer worker-token",
		reason: "wrong approach",
	}
	if len(*requests) != 1 || (*requests)[0] != want {
		t.Fatalf("controller requests = %+v, want [%+v]", *requests, want)
	}
}

func TestCancelTaskTool_ExecuteReportsControllerRejection(t *testing.T) {
	startCancelTaskController(t, http.StatusForbidden, "task is outside caller coordination scope")

	_, err := NewCancelTaskTool().Execute(context.Background(), json.RawMessage(`{"task_name":"other-task"}`))
	if err == nil || !strings.Contains(err.Error(), "HTTP 403") ||
		!strings.Contains(err.Error(), "task is outside caller coordination scope") {
		t.Fatalf("Execute() error = %v, want the controller rejection", err)
	}
}

func TestCancelTaskTool_ExecuteRejectsInvalidRequestsLocally(t *testing.T) {
	for _, tc := range []struct {
		name, args, want string
	}{
		{name: "missing task name", args: `{}`, want: "task_name is required"},
		{name: "other namespace", args: `{"task_name":"child-task-1","namespace":"elsewhere"}`, want: "is not a child of the current task"},
		{name: "malformed", args: `{`, want: "failed to parse arguments"},
		{
			name: "long reason",
			args: `{"task_name":"child-task-1","reason":"` + strings.Repeat("x", MaxCancelReasonLength+1) + `"}`,
			want: "reason must be at most 512 characters",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := startCancelTaskController(t, http.StatusOK, `{}`)
			_, err := NewCancelTaskTool().Execute(context.Background(), json.RawMessage(tc.args))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Execute() error = %v, want %q", err, tc.want)
			}
			if len(*requests) != 0 {
				t.Fatalf("controller received %d requests, want none", len(*requests))
			}
		})
	}
}

func TestCancelTaskTool_ExecuteRequiresControllerContext(t *testing.T) {
	t.Setenv(workerenv.ControllerURL, "")
	t.Setenv(workerenv.TaskName, parentTaskName)
	t.Setenv(workerenv.TaskNamespace, defaultNamespace)

	_, err := NewCancelTaskTool().Execute(context.Background(), json.RawMessage(`{"task_name":"child-task-1"}`))
	if err == nil || !strings.Contains(err.Error(), workerenv.ControllerURL) {
		t.Fatalf("Execute() error = %v, want missing %s", err, workerenv.ControllerURL)
	}
}
