/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUpdatePlanTool_Name(t *testing.T) {
	tool := NewUpdatePlanTool()
	if got := tool.Name(); got != updatePlanToolName {
		t.Errorf("Name() = %q, want %q", got, updatePlanToolName)
	}
}

func TestUpdatePlanTool_Description(t *testing.T) {
	tool := NewUpdatePlanTool()
	if got := tool.Description(); got == "" {
		t.Error("Description() should not be empty")
	}
}

func TestUpdatePlanTool_Parameters(t *testing.T) {
	tool := NewUpdatePlanTool()
	params := tool.Parameters()
	if len(params) == 0 {
		t.Fatal("Parameters() should not be empty")
	}

	var schema map[string]any
	if err := json.Unmarshal(params, &schema); err != nil {
		t.Fatalf("Parameters() should be valid JSON: %v", err)
	}

	if schema[jsonSchemaTypeField] != jsonSchemaTypeObject {
		t.Errorf("schema type = %v, want object", schema[jsonSchemaTypeField])
	}

	props, ok := schema[jsonSchemaPropertiesField].(map[string]any)
	if !ok {
		t.Fatal("schema should have properties")
	}

	for _, required := range []string{"summary", "progress_pct", "goal_complete", "plan_document"} {
		if _, ok := props[required]; !ok {
			t.Errorf("schema missing property %q", required)
		}
	}
}

func TestUpdatePlanTool_Execute(t *testing.T) {
	tool := NewUpdatePlanTool()

	tests := []struct {
		name       string
		args       string
		envURL     string
		envTask    string
		envNS      string
		envToken   string
		serverCode int
		wantErr    string
		wantResult string
		skipServer bool
	}{
		{
			name:    invalidJSONArgsCaseName,
			args:    invalidJSONText,
			wantErr: invalidArgumentsMessage,
		},
		{
			name:    "empty summary",
			args:    `{"summary":"","plan_document":"# Plan"}`,
			wantErr: "summary is required",
		},
		{
			name:    "empty plan_document",
			args:    `{"summary":"test","plan_document":""}`,
			wantErr: "plan_document is required",
		},
		{
			name:       "missing all env vars",
			args:       testPlanJSON,
			envURL:     "",
			envTask:    "",
			envNS:      "",
			wantErr:    missingControllerTaskEnvMessage,
			skipServer: true,
		},
		{
			name:       "missing ORKA_TASK_NAME",
			args:       testPlanJSON,
			envURL:     localhostURL,
			envTask:    "",
			envNS:      defaultNamespace,
			wantErr:    missingControllerTaskEnvMessage,
			skipServer: true,
		},
		{
			name:       "missing ORKA_TASK_NAMESPACE",
			args:       testPlanJSON,
			envURL:     localhostURL,
			envTask:    testMyTaskName,
			envNS:      "",
			wantErr:    missingControllerTaskEnvMessage,
			skipServer: true,
		},
		{
			name:       "successful update with 204",
			args:       `{"summary":"phase 1 done","progress_pct":50,"goal_complete":false,"plan_document":"# Plan\n## Done"}`,
			envTask:    testMyTaskName,
			envNS:      defaultNamespace,
			serverCode: http.StatusNoContent,
			wantResult: "Plan updated: phase 1 done (progress: 50%)",
		},
		{
			name:       "successful update with 200",
			args:       `{"summary":"all done","progress_pct":100,"goal_complete":true,"plan_document":"# Complete"}`,
			envTask:    testMyTaskName,
			envNS:      defaultNamespace,
			serverCode: http.StatusOK,
			wantResult: "Plan updated: all done (progress: 100%, goal marked as COMPLETE)",
		},
		{
			name:       "server error 500",
			args:       `{"summary":"test","progress_pct":10,"plan_document":"# Plan"}`,
			envTask:    testMyTaskName,
			envNS:      defaultNamespace,
			serverCode: http.StatusInternalServerError,
			wantErr:    "failed to save plan: HTTP 500",
		},
		{
			name:       "server error 403",
			args:       `{"summary":"test","progress_pct":0,"plan_document":"# Plan"}`,
			envTask:    testMyTaskName,
			envNS:      defaultNamespace,
			serverCode: http.StatusForbidden,
			wantErr:    "failed to save plan: HTTP 403",
		},
		{
			name:       "with SA token from env",
			args:       `{"summary":"with token","progress_pct":25,"plan_document":"# Plan"}`,
			envTask:    "task1",
			envNS:      "ns1",
			envToken:   "my-sa-token",
			serverCode: http.StatusNoContent,
			wantResult: "Plan updated: with token (progress: 25%)",
		},
		{
			name:       "zero progress not complete",
			args:       `{"summary":"starting","progress_pct":0,"goal_complete":false,"plan_document":"# Initial"}`,
			envTask:    "t",
			envNS:      "n",
			serverCode: http.StatusNoContent,
			wantResult: "Plan updated: starting (progress: 0%)",
		},
		{
			name:       "goal complete at partial progress",
			args:       `{"summary":"blocked","progress_pct":60,"goal_complete":true,"plan_document":"# Blocked"}`,
			envTask:    "t",
			envNS:      "n",
			serverCode: http.StatusOK,
			wantResult: "Plan updated: blocked (progress: 60%, goal marked as COMPLETE)",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var serverURL string
			if !tc.skipServer && tc.wantErr != invalidArgumentsMessage && tc.wantErr != "summary is required" && tc.wantErr != "plan_document is required" {
				var receivedAuth string
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					receivedAuth = r.Header.Get("Authorization")
					if r.Method != http.MethodPost {
						t.Errorf("expected POST, got %s", r.Method)
					}
					expectedPath := fmt.Sprintf("/internal/v1/plans/%s/%s", tc.envNS, tc.envTask)
					if r.URL.Path != expectedPath {
						t.Errorf("path = %q, want %q", r.URL.Path, expectedPath)
					}
					if ct := r.Header.Get("Content-Type"); ct != "application/json" {
						t.Errorf("Content-Type = %q, want application/json", ct)
					}
					w.WriteHeader(tc.serverCode)
				}))
				defer srv.Close()
				serverURL = srv.URL

				// Verify auth header after execution
				if tc.envToken != "" {
					defer func() {
						wantAuth := "Bearer " + tc.envToken
						if receivedAuth != wantAuth {
							t.Errorf("Authorization = %q, want %q", receivedAuth, wantAuth)
						}
					}()
				}
			}

			if serverURL != "" {
				t.Setenv(envOrkaControllerURL, serverURL)
			} else if tc.envURL != "" {
				t.Setenv(envOrkaControllerURL, tc.envURL)
			} else {
				t.Setenv(envOrkaControllerURL, "")
			}
			t.Setenv(envOrkaTaskName, tc.envTask)
			t.Setenv(envOrkaTaskNamespace, tc.envNS)
			t.Setenv("ORKA_SA_TOKEN", tc.envToken)

			result, err := tool.Execute(t.Context(), json.RawMessage(tc.args))

			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error = %q, want containing %q", err.Error(), tc.wantErr)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result != tc.wantResult {
				t.Errorf("result = %q, want %q", result, tc.wantResult)
			}
		})
	}
}

func TestUpdatePlanTool_Execute_ConnectionRefused(t *testing.T) {
	tool := NewUpdatePlanTool()
	t.Setenv(envOrkaControllerURL, "http://127.0.0.1:1")
	t.Setenv(envOrkaTaskName, "task")
	t.Setenv(envOrkaTaskNamespace, "ns")
	t.Setenv("ORKA_SA_TOKEN", "")

	args := json.RawMessage(testPlanJSON)
	_, err := tool.Execute(t.Context(), args)
	if err == nil {
		t.Fatal("expected error for connection refused")
	}
	if !strings.Contains(err.Error(), "failed to save plan") {
		t.Errorf("error = %q, want containing 'failed to save plan'", err.Error())
	}
}

func TestUpdatePlanTool_Execute_NoAuthHeaderWhenNoToken(t *testing.T) {
	var receivedAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	tool := NewUpdatePlanTool()
	t.Setenv(envOrkaControllerURL, srv.URL)
	t.Setenv(envOrkaTaskName, "task")
	t.Setenv(envOrkaTaskNamespace, "ns")
	t.Setenv("ORKA_SA_TOKEN", "")

	args := json.RawMessage(testPlanJSON)
	_, err := tool.Execute(t.Context(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if receivedAuth != "" {
		t.Errorf("expected no Authorization header, got %q", receivedAuth)
	}
}

func TestUpdatePlanTool_Execute_RequestBodyValid(t *testing.T) {
	var received updatePlanArgs
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("failed to decode body: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	tool := NewUpdatePlanTool()
	t.Setenv(envOrkaControllerURL, srv.URL)
	t.Setenv(envOrkaTaskName, "task")
	t.Setenv(envOrkaTaskNamespace, "ns")
	t.Setenv("ORKA_SA_TOKEN", "")

	args := json.RawMessage(`{"summary":"my summary","progress_pct":75,"goal_complete":true,"plan_document":"# My Plan"}`)
	_, err := tool.Execute(t.Context(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if received.Summary != "my summary" {
		t.Errorf("body summary = %q, want %q", received.Summary, "my summary")
	}
	if received.ProgressPct != 75 {
		t.Errorf("body progress_pct = %d, want 75", received.ProgressPct)
	}
	if !received.GoalComplete {
		t.Error("body goal_complete = false, want true")
	}
	if received.PlanDocument != "# My Plan" {
		t.Errorf("body plan_document = %q, want %q", received.PlanDocument, "# My Plan")
	}
}
