/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package tools

import (
	"context"
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apitypes "k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/sozercan/mercan/api/v1alpha1"
)

func researcherAgent() *corev1alpha1.Agent {
	return &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "researcher",
			Namespace: "default",
		},
		Spec: corev1alpha1.AgentSpec{},
	}
}

func parentTask() *corev1alpha1.Task {
	priority := int32(500)
	return &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "parent-task",
			Namespace: "default",
			UID:       apitypes.UID("parent-uid-1234"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeAI,
			Priority: &priority,
		},
	}
}

func TestDelegateTaskTool_Name(t *testing.T) {
	tool := NewDelegateTaskTool(newFakeClient())
	if got := tool.Name(); got != "delegate_task" {
		t.Errorf("Name() = %v, want %v", got, "delegate_task")
	}
}

func TestDelegateTaskTool_Description(t *testing.T) {
	tool := NewDelegateTaskTool(newFakeClient())
	if got := tool.Description(); got == "" {
		t.Error("Description() returned empty string")
	}
}

func TestDelegateTaskTool_Parameters(t *testing.T) {
	tool := NewDelegateTaskTool(newFakeClient())
	params := tool.Parameters()
	if params == nil {
		t.Fatal("Parameters() returned nil")
	}

	var schema map[string]any
	if err := json.Unmarshal(params, &schema); err != nil {
		t.Errorf("Parameters() returned invalid JSON: %v", err)
	}
	if schema["type"] != "object" {
		t.Error("Parameters schema should have type: object")
	}
}

func TestDelegateTaskTool_Execute(t *testing.T) {
	tests := []struct {
		name        string
		envVars     map[string]string
		args        json.RawMessage
		wantErr     bool
		wantErrMsg  string
		checkResult bool
		wantStatus  string
	}{
		{
			name: "successful delegation",
			envVars: map[string]string{
				"MERCAN_TASK_NAME":                   "parent-task",
				"MERCAN_TASK_NAMESPACE":              "default",
				"MERCAN_COORDINATION_DEPTH":          "0",
				"MERCAN_COORDINATION_ALLOWED_AGENTS": "researcher,coder",
				"MERCAN_COORDINATION_MAX_DEPTH":      "3",
			},
			args:        json.RawMessage(`{"agent": "researcher", "prompt": "Research the topic"}`),
			wantErr:     false,
			checkResult: true,
			wantStatus:  "created",
		},
		{
			name: "agent not allowed",
			envVars: map[string]string{
				"MERCAN_TASK_NAME":                   "parent-task",
				"MERCAN_TASK_NAMESPACE":              "default",
				"MERCAN_COORDINATION_DEPTH":          "0",
				"MERCAN_COORDINATION_ALLOWED_AGENTS": "researcher,coder",
				"MERCAN_COORDINATION_MAX_DEPTH":      "3",
			},
			args:       json.RawMessage(`{"agent": "unauthorized-agent", "prompt": "Do something"}`),
			wantErr:    true,
			wantErrMsg: "not in the allowed agents list",
		},
		{
			name: "depth exceeded",
			envVars: map[string]string{
				"MERCAN_TASK_NAME":                   "parent-task",
				"MERCAN_TASK_NAMESPACE":              "default",
				"MERCAN_COORDINATION_DEPTH":          "3",
				"MERCAN_COORDINATION_ALLOWED_AGENTS": "researcher",
				"MERCAN_COORDINATION_MAX_DEPTH":      "3",
			},
			args:       json.RawMessage(`{"agent": "researcher", "prompt": "Research the topic"}`),
			wantErr:    true,
			wantErrMsg: "coordination depth exceeded",
		},
		{
			name: "missing agent arg",
			envVars: map[string]string{
				"MERCAN_TASK_NAME":                   "parent-task",
				"MERCAN_TASK_NAMESPACE":              "default",
				"MERCAN_COORDINATION_DEPTH":          "0",
				"MERCAN_COORDINATION_ALLOWED_AGENTS": "researcher",
				"MERCAN_COORDINATION_MAX_DEPTH":      "3",
			},
			args:       json.RawMessage(`{"prompt": "Research the topic"}`),
			wantErr:    true,
			wantErrMsg: "agent is required",
		},
		{
			name: "missing prompt arg",
			envVars: map[string]string{
				"MERCAN_TASK_NAME":                   "parent-task",
				"MERCAN_TASK_NAMESPACE":              "default",
				"MERCAN_COORDINATION_DEPTH":          "0",
				"MERCAN_COORDINATION_ALLOWED_AGENTS": "researcher",
				"MERCAN_COORDINATION_MAX_DEPTH":      "3",
			},
			args:       json.RawMessage(`{"agent": "researcher"}`),
			wantErr:    true,
			wantErrMsg: "prompt is required",
		},
		{
			name: "invalid JSON args",
			envVars: map[string]string{
				"MERCAN_TASK_NAME":      "parent-task",
				"MERCAN_TASK_NAMESPACE": "default",
			},
			args:       json.RawMessage(`{invalid}`),
			wantErr:    true,
			wantErrMsg: "invalid arguments",
		},
		{
			name: "custom priority",
			envVars: map[string]string{
				"MERCAN_TASK_NAME":                   "parent-task",
				"MERCAN_TASK_NAMESPACE":              "default",
				"MERCAN_COORDINATION_DEPTH":          "0",
				"MERCAN_COORDINATION_ALLOWED_AGENTS": "researcher",
				"MERCAN_COORDINATION_MAX_DEPTH":      "3",
			},
			args:        json.RawMessage(`{"agent": "researcher", "prompt": "Research", "priority": 800}`),
			wantErr:     false,
			checkResult: true,
			wantStatus:  "created",
		},
		{
			name: "custom namespace",
			envVars: map[string]string{
				"MERCAN_TASK_NAME":                   "parent-task",
				"MERCAN_TASK_NAMESPACE":              "default",
				"MERCAN_COORDINATION_DEPTH":          "0",
				"MERCAN_COORDINATION_ALLOWED_AGENTS": "researcher",
				"MERCAN_COORDINATION_MAX_DEPTH":      "3",
			},
			args:    json.RawMessage(`{"agent": "researcher", "prompt": "Research", "namespace": "other-ns"}`),
			wantErr: true, // parent task not found in other-ns
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Set environment variables
			for k, v := range tt.envVars {
				t.Setenv(k, v)
			}

			// Create fake client with parent task and agent
			k8sClient := newFakeClient(parentTask(), researcherAgent())
			tool := NewDelegateTaskTool(k8sClient)

			result, err := tool.Execute(context.Background(), tt.args)

			if (err != nil) != tt.wantErr {
				t.Errorf("Execute() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.wantErr && tt.wantErrMsg != "" {
				if err == nil || !contains(err.Error(), tt.wantErrMsg) {
					t.Errorf("Execute() error = %v, want error containing %q", err, tt.wantErrMsg)
				}
				return
			}

			if tt.checkResult {
				var delegateResult DelegateTaskResult
				if err := json.Unmarshal([]byte(result), &delegateResult); err != nil {
					t.Fatalf("failed to unmarshal result: %v", err)
				}
				if delegateResult.Status != tt.wantStatus {
					t.Errorf("Execute() status = %q, want %q", delegateResult.Status, tt.wantStatus)
				}
				if delegateResult.TaskName == "" {
					t.Error("Execute() returned empty task name")
				}
			}
		})
	}
}

func TestDelegateTaskTool_Execute_ChildTaskFields(t *testing.T) {
	t.Setenv("MERCAN_TASK_NAME", "parent-task")
	t.Setenv("MERCAN_TASK_NAMESPACE", "default")
	t.Setenv("MERCAN_COORDINATION_DEPTH", "1")
	t.Setenv("MERCAN_COORDINATION_ALLOWED_AGENTS", "researcher")
	t.Setenv("MERCAN_COORDINATION_MAX_DEPTH", "5")

	k8sClient := newFakeClient(parentTask(), researcherAgent())
	tool := NewDelegateTaskTool(k8sClient)

	args := json.RawMessage(`{"agent": "researcher", "prompt": "Investigate this"}`)
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var delegateResult DelegateTaskResult
	if err := json.Unmarshal([]byte(result), &delegateResult); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}

	// Fetch the created child task to verify fields
	taskList := &corev1alpha1.TaskList{}
	if err := k8sClient.List(context.Background(), taskList); err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}

	// Find the child task (not the parent)
	var childTask *corev1alpha1.Task
	for i := range taskList.Items {
		if taskList.Items[i].Name != "parent-task" {
			childTask = &taskList.Items[i]
			break
		}
	}

	if childTask == nil {
		t.Fatal("child task not found")
	}

	// Verify labels
	if childTask.Labels["mercan.ai/parent-task"] != "parent-task" {
		t.Errorf("label mercan.ai/parent-task = %q, want %q", childTask.Labels["mercan.ai/parent-task"], "parent-task")
	}
	if childTask.Labels["mercan.ai/coordinator"] != "true" {
		t.Errorf("label mercan.ai/coordinator = %q, want %q", childTask.Labels["mercan.ai/coordinator"], "true")
	}
	if childTask.Labels["mercan.ai/delegated-agent"] != "researcher" {
		t.Errorf("label mercan.ai/delegated-agent = %q, want %q", childTask.Labels["mercan.ai/delegated-agent"], "researcher")
	}

	// Verify annotations
	if childTask.Annotations["mercan.ai/coordination-depth"] != "2" {
		t.Errorf("annotation mercan.ai/coordination-depth = %q, want %q", childTask.Annotations["mercan.ai/coordination-depth"], "2")
	}

	// Verify spec
	if childTask.Spec.Type != corev1alpha1.TaskTypeAI {
		t.Errorf("spec.type = %q, want %q", childTask.Spec.Type, corev1alpha1.TaskTypeAI)
	}
	if childTask.Spec.AgentRef == nil || childTask.Spec.AgentRef.Name != "researcher" {
		t.Errorf("spec.agentRef.name = %v, want %q", childTask.Spec.AgentRef, "researcher")
	}
	if childTask.Spec.Prompt != "Investigate this" {
		t.Errorf("spec.prompt = %q, want %q", childTask.Spec.Prompt, "Investigate this")
	}

	// Verify owner reference
	if len(childTask.OwnerReferences) != 1 {
		t.Fatalf("expected 1 owner reference, got %d", len(childTask.OwnerReferences))
	}
	ownerRef := childTask.OwnerReferences[0]
	if ownerRef.Name != "parent-task" {
		t.Errorf("ownerRef.Name = %q, want %q", ownerRef.Name, "parent-task")
	}
	if ownerRef.UID != apitypes.UID("parent-uid-1234") {
		t.Errorf("ownerRef.UID = %q, want %q", ownerRef.UID, "parent-uid-1234")
	}
	if ownerRef.Controller == nil || !*ownerRef.Controller {
		t.Error("ownerRef.Controller should be true")
	}
	if ownerRef.BlockOwnerDeletion == nil || !*ownerRef.BlockOwnerDeletion {
		t.Error("ownerRef.BlockOwnerDeletion should be true")
	}

	// Verify priority inherited from parent
	if childTask.Spec.Priority == nil || *childTask.Spec.Priority != 500 {
		t.Errorf("spec.priority = %v, want 500", childTask.Spec.Priority)
	}
}

func TestDelegateTaskTool_Execute_AgentType(t *testing.T) {
	t.Setenv("MERCAN_TASK_NAME", "parent-task")
	t.Setenv("MERCAN_TASK_NAMESPACE", "default")
	t.Setenv("MERCAN_COORDINATION_DEPTH", "0")
	t.Setenv("MERCAN_COORDINATION_ALLOWED_AGENTS", "claude-coder")
	t.Setenv("MERCAN_COORDINATION_MAX_DEPTH", "3")

	maxTurns := int32(100)
	agentTask := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "claude-coder",
			Namespace: "default",
		},
		Spec: corev1alpha1.AgentSpec{
			Runtime: &corev1alpha1.AgentCLIRuntime{
				Type:             "claude",
				DefaultMaxTurns:  &maxTurns,
				DefaultAllowBash: true,
			},
		},
	}

	k8sClient := newFakeClient(parentTask(), agentTask)
	tool := NewDelegateTaskTool(k8sClient)

	args := json.RawMessage(`{
		"agent": "claude-coder",
		"prompt": "Fix the auth module",
		"workspace": {
			"gitRepo": "https://github.com/myorg/myrepo.git",
			"branch": "main"
		},
		"maxTurns": 50,
		"allowBash": true
	}`)
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var delegateResult DelegateTaskResult
	if err := json.Unmarshal([]byte(result), &delegateResult); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}
	if delegateResult.Status != "created" {
		t.Errorf("status = %q, want %q", delegateResult.Status, "created")
	}

	// Fetch the child task
	taskList := &corev1alpha1.TaskList{}
	if err := k8sClient.List(context.Background(), taskList); err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}

	var childTask *corev1alpha1.Task
	for i := range taskList.Items {
		if taskList.Items[i].Name != "parent-task" {
			childTask = &taskList.Items[i]
			break
		}
	}
	if childTask == nil {
		t.Fatal("child task not found")
	}

	// Verify task type is agent
	if childTask.Spec.Type != corev1alpha1.TaskTypeAgent {
		t.Errorf("spec.type = %q, want %q", childTask.Spec.Type, corev1alpha1.TaskTypeAgent)
	}

	// Verify agent runtime config
	if childTask.Spec.AgentRuntime == nil {
		t.Fatal("spec.agentRuntime is nil")
	}
	if childTask.Spec.AgentRuntime.Workspace == nil {
		t.Fatal("spec.agentRuntime.workspace is nil")
	}
	if childTask.Spec.AgentRuntime.Workspace.GitRepo != "https://github.com/myorg/myrepo.git" {
		t.Errorf("workspace.gitRepo = %q, want %q", childTask.Spec.AgentRuntime.Workspace.GitRepo, "https://github.com/myorg/myrepo.git")
	}
	if childTask.Spec.AgentRuntime.Workspace.Branch != "main" {
		t.Errorf("workspace.branch = %q, want %q", childTask.Spec.AgentRuntime.Workspace.Branch, "main")
	}
	if childTask.Spec.AgentRuntime.MaxTurns == nil || *childTask.Spec.AgentRuntime.MaxTurns != 50 {
		t.Errorf("agentRuntime.maxTurns = %v, want 50", childTask.Spec.AgentRuntime.MaxTurns)
	}
	if childTask.Spec.AgentRuntime.AllowBash == nil || !*childTask.Spec.AgentRuntime.AllowBash {
		t.Error("agentRuntime.allowBash should be true")
	}
}

func TestDelegateTaskTool_Execute_AgentNotFound(t *testing.T) {
	t.Setenv("MERCAN_TASK_NAME", "parent-task")
	t.Setenv("MERCAN_TASK_NAMESPACE", "default")
	t.Setenv("MERCAN_COORDINATION_DEPTH", "0")
	t.Setenv("MERCAN_COORDINATION_ALLOWED_AGENTS", "nonexistent-agent")
	t.Setenv("MERCAN_COORDINATION_MAX_DEPTH", "3")

	// No agent registered in the fake client
	k8sClient := newFakeClient(parentTask())
	tool := NewDelegateTaskTool(k8sClient)

	args := json.RawMessage(`{"agent": "nonexistent-agent", "prompt": "Do something"}`)
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("Execute() expected error for nonexistent agent")
	}
	if !contains(err.Error(), "failed to get agent") {
		t.Errorf("Execute() error = %v, want error containing %q", err, "failed to get agent")
	}
}

func TestDelegateTaskTool_Execute_AgentTypeNoWorkspace(t *testing.T) {
	t.Setenv("MERCAN_TASK_NAME", "parent-task")
	t.Setenv("MERCAN_TASK_NAMESPACE", "default")
	t.Setenv("MERCAN_COORDINATION_DEPTH", "0")
	t.Setenv("MERCAN_COORDINATION_ALLOWED_AGENTS", "claude-coder")
	t.Setenv("MERCAN_COORDINATION_MAX_DEPTH", "3")

	agentWithRuntime := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "claude-coder",
			Namespace: "default",
		},
		Spec: corev1alpha1.AgentSpec{
			Runtime: &corev1alpha1.AgentCLIRuntime{
				Type: "claude",
			},
		},
	}

	k8sClient := newFakeClient(parentTask(), agentWithRuntime)
	tool := NewDelegateTaskTool(k8sClient)

	// No workspace, maxTurns, or allowBash args
	args := json.RawMessage(`{"agent": "claude-coder", "prompt": "Fix bugs"}`)
	_, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	taskList := &corev1alpha1.TaskList{}
	if err := k8sClient.List(context.Background(), taskList); err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}

	var childTask *corev1alpha1.Task
	for i := range taskList.Items {
		if taskList.Items[i].Name != "parent-task" {
			childTask = &taskList.Items[i]
			break
		}
	}
	if childTask == nil {
		t.Fatal("child task not found")
	}

	if childTask.Spec.Type != corev1alpha1.TaskTypeAgent {
		t.Errorf("spec.type = %q, want %q", childTask.Spec.Type, corev1alpha1.TaskTypeAgent)
	}
	if childTask.Spec.AgentRuntime == nil {
		t.Fatal("spec.agentRuntime should not be nil for agent-type tasks")
	}
	if childTask.Spec.AgentRuntime.Workspace != nil {
		t.Error("spec.agentRuntime.workspace should be nil when not provided")
	}
	if childTask.Spec.AgentRuntime.MaxTurns != nil {
		t.Error("spec.agentRuntime.maxTurns should be nil when not provided")
	}
	if childTask.Spec.AgentRuntime.AllowBash != nil {
		t.Error("spec.agentRuntime.allowBash should be nil when not provided")
	}
}

func TestDelegateTaskTool_Execute_AITypeNoRuntime(t *testing.T) {
	t.Setenv("MERCAN_TASK_NAME", "parent-task")
	t.Setenv("MERCAN_TASK_NAMESPACE", "default")
	t.Setenv("MERCAN_COORDINATION_DEPTH", "0")
	t.Setenv("MERCAN_COORDINATION_ALLOWED_AGENTS", "researcher")
	t.Setenv("MERCAN_COORDINATION_MAX_DEPTH", "3")

	k8sClient := newFakeClient(parentTask(), researcherAgent())
	tool := NewDelegateTaskTool(k8sClient)

	args := json.RawMessage(`{"agent": "researcher", "prompt": "Research the topic"}`)
	_, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	taskList := &corev1alpha1.TaskList{}
	if err := k8sClient.List(context.Background(), taskList); err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}

	var childTask *corev1alpha1.Task
	for i := range taskList.Items {
		if taskList.Items[i].Name != "parent-task" {
			childTask = &taskList.Items[i]
			break
		}
	}
	if childTask == nil {
		t.Fatal("child task not found")
	}

	if childTask.Spec.Type != corev1alpha1.TaskTypeAI {
		t.Errorf("spec.type = %q, want %q", childTask.Spec.Type, corev1alpha1.TaskTypeAI)
	}
	if childTask.Spec.AgentRuntime != nil {
		t.Error("spec.agentRuntime should be nil for AI-type tasks")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
