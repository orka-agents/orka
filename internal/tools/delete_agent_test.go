/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/sozercan/mercan/api/v1alpha1"
)

func testAgent() *corev1alpha1.Agent {
	return &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-agent",
			Namespace: "default",
		},
		Spec: corev1alpha1.AgentSpec{},
	}
}

func TestDeleteAgentTool_Name(t *testing.T) {
	tool := NewDeleteAgentTool(newFakeClient())
	if tool.Name() != "delete_agent" {
		t.Errorf("expected name 'delete_agent', got '%s'", tool.Name())
	}
}

func TestDeleteAgentTool_Execute(t *testing.T) {
	tests := []struct {
		name      string
		args      DeleteAgentArgs
		setup     func() *DeleteAgentTool
		envNS     string
		wantErr   bool
		errSubstr string
	}{
		{
			name: "success - agent exists and is deleted",
			args: DeleteAgentArgs{Name: "test-agent", Namespace: "default"},
			setup: func() *DeleteAgentTool {
				return NewDeleteAgentTool(newFakeClient(testAgent()))
			},
		},
		{
			name: "error - name is empty",
			args: DeleteAgentArgs{Name: ""},
			setup: func() *DeleteAgentTool {
				return NewDeleteAgentTool(newFakeClient())
			},
			wantErr:   true,
			errSubstr: "agent name is required",
		},
		{
			name: "error - agent not found",
			args: DeleteAgentArgs{Name: "nonexistent", Namespace: "default"},
			setup: func() *DeleteAgentTool {
				return NewDeleteAgentTool(newFakeClient())
			},
			wantErr:   true,
			errSubstr: "failed to get agent",
		},
		{
			name:  "success - namespace from env var",
			args:  DeleteAgentArgs{Name: "test-agent"},
			envNS: "default",
			setup: func() *DeleteAgentTool {
				return NewDeleteAgentTool(newFakeClient(testAgent()))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.envNS != "" {
				t.Setenv("MERCAN_TASK_NAMESPACE", tt.envNS)
			}

			tool := tt.setup()
			argsJSON, _ := json.Marshal(tt.args)
			result, err := tool.Execute(context.Background(), argsJSON)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.errSubstr)
				}
				if !strings.Contains(err.Error(), tt.errSubstr) {
					t.Errorf("expected error containing %q, got %q", tt.errSubstr, err.Error())
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			var res DeleteAgentResult
			if err := json.Unmarshal([]byte(result), &res); err != nil {
				t.Fatalf("failed to unmarshal result: %v", err)
			}
			if res.Name != tt.args.Name {
				t.Errorf("expected name %q, got %q", tt.args.Name, res.Name)
			}
			if res.Status != "deleted" {
				t.Errorf("expected status 'deleted', got %q", res.Status)
			}
		})
	}
}
