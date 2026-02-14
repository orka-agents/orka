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
	apitypes "k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
)

const (
	parentTaskName = "parent-task"
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
			Name:      parentTaskName,
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
	if schema["type"] != typeObject {
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
				"ORKA_TASK_NAME":                   parentTaskName,
				"ORKA_TASK_NAMESPACE":              "default",
				"ORKA_COORDINATION_DEPTH":          "0",
				"ORKA_COORDINATION_ALLOWED_AGENTS": "researcher,coder",
				"ORKA_COORDINATION_MAX_DEPTH":      "3",
			},
			args:        json.RawMessage(`{"agent": "researcher", "prompt": "Research the topic"}`),
			wantErr:     false,
			checkResult: true,
			wantStatus:  "created",
		},
		{
			name: "agent not allowed",
			envVars: map[string]string{
				"ORKA_TASK_NAME":                   parentTaskName,
				"ORKA_TASK_NAMESPACE":              "default",
				"ORKA_COORDINATION_DEPTH":          "0",
				"ORKA_COORDINATION_ALLOWED_AGENTS": "researcher,coder",
				"ORKA_COORDINATION_MAX_DEPTH":      "3",
			},
			args:       json.RawMessage(`{"agent": "unauthorized-agent", "prompt": "Do something"}`),
			wantErr:    true,
			wantErrMsg: "not in the allowed agents list",
		},
		{
			name: "depth exceeded",
			envVars: map[string]string{
				"ORKA_TASK_NAME":                   parentTaskName,
				"ORKA_TASK_NAMESPACE":              "default",
				"ORKA_COORDINATION_DEPTH":          "3",
				"ORKA_COORDINATION_ALLOWED_AGENTS": "researcher",
				"ORKA_COORDINATION_MAX_DEPTH":      "3",
			},
			args:       json.RawMessage(`{"agent": "researcher", "prompt": "Research the topic"}`),
			wantErr:    true,
			wantErrMsg: "coordination depth exceeded",
		},
		{
			name: "missing agent arg",
			envVars: map[string]string{
				"ORKA_TASK_NAME":                   parentTaskName,
				"ORKA_TASK_NAMESPACE":              "default",
				"ORKA_COORDINATION_DEPTH":          "0",
				"ORKA_COORDINATION_ALLOWED_AGENTS": "researcher",
				"ORKA_COORDINATION_MAX_DEPTH":      "3",
			},
			args:       json.RawMessage(`{"prompt": "Research the topic"}`),
			wantErr:    true,
			wantErrMsg: "agent is required",
		},
		{
			name: "missing prompt arg",
			envVars: map[string]string{
				"ORKA_TASK_NAME":                   parentTaskName,
				"ORKA_TASK_NAMESPACE":              "default",
				"ORKA_COORDINATION_DEPTH":          "0",
				"ORKA_COORDINATION_ALLOWED_AGENTS": "researcher",
				"ORKA_COORDINATION_MAX_DEPTH":      "3",
			},
			args:       json.RawMessage(`{"agent": "researcher"}`),
			wantErr:    true,
			wantErrMsg: "prompt is required",
		},
		{
			name: "invalid JSON args",
			envVars: map[string]string{
				"ORKA_TASK_NAME":      parentTaskName,
				"ORKA_TASK_NAMESPACE": "default",
			},
			args:       json.RawMessage(`{invalid}`),
			wantErr:    true,
			wantErrMsg: "invalid arguments",
		},
		{
			name: "custom priority",
			envVars: map[string]string{
				"ORKA_TASK_NAME":                   parentTaskName,
				"ORKA_TASK_NAMESPACE":              "default",
				"ORKA_COORDINATION_DEPTH":          "0",
				"ORKA_COORDINATION_ALLOWED_AGENTS": "researcher",
				"ORKA_COORDINATION_MAX_DEPTH":      "3",
			},
			args:        json.RawMessage(`{"agent": "researcher", "prompt": "Research", "priority": 800}`),
			wantErr:     false,
			checkResult: true,
			wantStatus:  "created",
		},
		{
			name: "custom namespace",
			envVars: map[string]string{
				"ORKA_TASK_NAME":                   parentTaskName,
				"ORKA_TASK_NAMESPACE":              "default",
				"ORKA_COORDINATION_DEPTH":          "0",
				"ORKA_COORDINATION_ALLOWED_AGENTS": "researcher",
				"ORKA_COORDINATION_MAX_DEPTH":      "3",
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
	t.Setenv("ORKA_TASK_NAME", parentTaskName)
	t.Setenv("ORKA_TASK_NAMESPACE", "default")
	t.Setenv("ORKA_COORDINATION_DEPTH", "1")
	t.Setenv("ORKA_COORDINATION_ALLOWED_AGENTS", "researcher")
	t.Setenv("ORKA_COORDINATION_MAX_DEPTH", "5")

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
		if taskList.Items[i].Name != parentTaskName {
			childTask = &taskList.Items[i]
			break
		}
	}

	if childTask == nil {
		t.Fatal("child task not found")
	}

	// Verify labels
	if childTask.Labels["orka.ai/parent-task"] != parentTaskName {
		t.Errorf("label orka.ai/parent-task = %q, want %q", childTask.Labels["orka.ai/parent-task"], "parent-task")
	}
	if childTask.Labels["orka.ai/coordinator"] != trueStr {
		t.Errorf("label orka.ai/coordinator = %q, want %q", childTask.Labels["orka.ai/coordinator"], trueStr)
	}
	if childTask.Labels["orka.ai/delegated-agent"] != "researcher" {
		t.Errorf("label orka.ai/delegated-agent = %q, want %q", childTask.Labels["orka.ai/delegated-agent"], "researcher")
	}

	// Verify annotations
	if childTask.Annotations["orka.ai/coordination-depth"] != "2" {
		t.Errorf("annotation orka.ai/coordination-depth = %q, want %q", childTask.Annotations["orka.ai/coordination-depth"], "2")
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
	if ownerRef.Name != parentTaskName {
		t.Errorf("ownerRef.Name = %q, want %q", ownerRef.Name, parentTaskName)
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
	t.Setenv("ORKA_TASK_NAME", parentTaskName)
	t.Setenv("ORKA_TASK_NAMESPACE", "default")
	t.Setenv("ORKA_COORDINATION_DEPTH", "0")
	t.Setenv("ORKA_COORDINATION_ALLOWED_AGENTS", "claude-coder")
	t.Setenv("ORKA_COORDINATION_MAX_DEPTH", "3")

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
		if taskList.Items[i].Name != parentTaskName {
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
	t.Setenv("ORKA_TASK_NAME", parentTaskName)
	t.Setenv("ORKA_TASK_NAMESPACE", "default")
	t.Setenv("ORKA_COORDINATION_DEPTH", "0")
	t.Setenv("ORKA_COORDINATION_ALLOWED_AGENTS", "nonexistent-agent")
	t.Setenv("ORKA_COORDINATION_MAX_DEPTH", "3")

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
	t.Setenv("ORKA_TASK_NAME", parentTaskName)
	t.Setenv("ORKA_TASK_NAMESPACE", "default")
	t.Setenv("ORKA_COORDINATION_DEPTH", "0")
	t.Setenv("ORKA_COORDINATION_ALLOWED_AGENTS", "claude-coder")
	t.Setenv("ORKA_COORDINATION_MAX_DEPTH", "3")

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
		if taskList.Items[i].Name != parentTaskName {
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
	t.Setenv("ORKA_TASK_NAME", parentTaskName)
	t.Setenv("ORKA_TASK_NAMESPACE", "default")
	t.Setenv("ORKA_COORDINATION_DEPTH", "0")
	t.Setenv("ORKA_COORDINATION_ALLOWED_AGENTS", "researcher")
	t.Setenv("ORKA_COORDINATION_MAX_DEPTH", "3")

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
		if taskList.Items[i].Name != parentTaskName {
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

func TestDelegateTaskTool_Execute_PriorTask(t *testing.T) {
	// Create a prior task in the fake client
	prior := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "prior-task-1",
			Namespace: "default",
			UID:       "prior-uid",
			Labels: map[string]string{
				"orka.ai/iteration":       "1",
				"orka.ai/iteration-group": "group-abc",
			},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAgent,
			Prompt: "original prompt",
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{
				Workspace: &corev1alpha1.WorkspaceConfig{
					GitRepo: "https://github.com/example/repo",
					Branch:  "main",
				},
			},
		},
		Status: corev1alpha1.TaskStatus{
			Phase: corev1alpha1.TaskPhaseSucceeded,
		},
	}

	parent := parentTask()
	agent := researcherAgent()

	fakeClient := newFakeClient(parent, agent, prior)
	tool := NewDelegateTaskTool(fakeClient)

	t.Setenv("ORKA_TASK_NAME", parentTaskName)
	t.Setenv("ORKA_TASK_NAMESPACE", "default")
	t.Setenv("ORKA_COORDINATION_DEPTH", "0")
	t.Setenv("ORKA_COORDINATION_ALLOWED_AGENTS", "researcher")

	args, _ := json.Marshal(map[string]any{
		"agent":      "researcher",
		"prompt":     "fix the bug",
		"prior_task": "prior-task-1",
		"feedback":   "Add error handling",
	})

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	var delegateResult DelegateTaskResult
	if err := json.Unmarshal([]byte(result), &delegateResult); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if delegateResult.Status != "created" {
		t.Errorf("expected status 'created', got %q", delegateResult.Status)
	}

	// Verify the child task was created with PriorTaskRef
	childTask := &corev1alpha1.Task{}
	if err := fakeClient.Get(context.Background(), apitypes.NamespacedName{
		Name: delegateResult.TaskName, Namespace: "default",
	}, childTask); err != nil {
		t.Fatalf("get child task: %v", err)
	}

	if childTask.Spec.PriorTaskRef == nil {
		t.Fatal("expected PriorTaskRef to be set")
	}
	if childTask.Spec.PriorTaskRef.Name != "prior-task-1" {
		t.Errorf("expected PriorTaskRef.Name 'prior-task-1', got %q", childTask.Spec.PriorTaskRef.Name)
	}

	// Verify feedback was prepended to prompt
	if !strings.Contains(childTask.Spec.Prompt, "FEEDBACK FROM REVIEW") {
		t.Errorf("expected prompt to contain feedback, got %q", childTask.Spec.Prompt)
	}
	if !strings.Contains(childTask.Spec.Prompt, "Add error handling") {
		t.Errorf("expected prompt to contain feedback text")
	}

	// Verify iteration labels
	if childTask.Labels["orka.ai/iteration"] != "2" {
		t.Errorf("expected iteration=2, got %q", childTask.Labels["orka.ai/iteration"])
	}
	if childTask.Labels["orka.ai/iteration-group"] != "group-abc" {
		t.Errorf("expected iteration-group=group-abc, got %q", childTask.Labels["orka.ai/iteration-group"])
	}

	// Verify workspace was copied from prior task
	if childTask.Spec.AgentRuntime == nil || childTask.Spec.AgentRuntime.Workspace == nil {
		t.Fatal("expected workspace to be copied from prior task")
	}
	if childTask.Spec.AgentRuntime.Workspace.GitRepo != "https://github.com/example/repo" {
		t.Errorf("expected git repo from prior task, got %q", childTask.Spec.AgentRuntime.Workspace.GitRepo)
	}
}

func TestDelegateTaskTool_Execute_FeedbackOnly(t *testing.T) {
	parent := parentTask()
	agent := researcherAgent()

	fakeClient := newFakeClient(parent, agent)
	tool := NewDelegateTaskTool(fakeClient)

	t.Setenv("ORKA_TASK_NAME", parentTaskName)
	t.Setenv("ORKA_TASK_NAMESPACE", "default")
	t.Setenv("ORKA_COORDINATION_DEPTH", "0")
	t.Setenv("ORKA_COORDINATION_ALLOWED_AGENTS", "researcher")

	args, _ := json.Marshal(map[string]any{
		"agent":    "researcher",
		"prompt":   "implement feature",
		"feedback": "Use dependency injection",
	})

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	var delegateResult DelegateTaskResult
	_ = json.Unmarshal([]byte(result), &delegateResult)

	// Verify the child task was created with feedback in prompt
	childTask := &corev1alpha1.Task{}
	_ = fakeClient.Get(context.Background(), apitypes.NamespacedName{
		Name: delegateResult.TaskName, Namespace: "default",
	}, childTask)

	if !strings.Contains(childTask.Spec.Prompt, "FEEDBACK FROM REVIEW") {
		t.Errorf("expected feedback in prompt, got %q", childTask.Spec.Prompt)
	}
	// PriorTaskRef should NOT be set
	if childTask.Spec.PriorTaskRef != nil {
		t.Errorf("expected PriorTaskRef to be nil when prior_task not specified")
	}
}

func TestDelegateTaskTool_Execute_PushBranch(t *testing.T) {
	t.Setenv("ORKA_TASK_NAME", parentTaskName)
	t.Setenv("ORKA_TASK_NAMESPACE", "default")
	t.Setenv("ORKA_COORDINATION_DEPTH", "0")
	t.Setenv("ORKA_COORDINATION_ALLOWED_AGENTS", "claude-coder")
	t.Setenv("ORKA_COORDINATION_MAX_DEPTH", "3")

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

	args := json.RawMessage(`{
		"agent": "claude-coder",
		"prompt": "Implement feature",
		"workspace": {
			"gitRepo": "https://github.com/sozercan/ayna",
			"branch": "main",
			"gitSecretRef": "git-credentials",
			"pushBranch": "feature/edit-message"
		}
	}`)

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var delegateResult DelegateTaskResult
	_ = json.Unmarshal([]byte(result), &delegateResult)

	childTask := &corev1alpha1.Task{}
	if err := k8sClient.Get(context.Background(), apitypes.NamespacedName{
		Name: delegateResult.TaskName, Namespace: "default",
	}, childTask); err != nil {
		t.Fatalf("failed to get child task: %v", err)
	}

	if childTask.Spec.AgentRuntime == nil || childTask.Spec.AgentRuntime.Workspace == nil {
		t.Fatal("expected workspace to be set")
	}
	ws := childTask.Spec.AgentRuntime.Workspace
	if ws.PushBranch != "feature/edit-message" {
		t.Errorf("pushBranch = %q, want %q", ws.PushBranch, "feature/edit-message")
	}
	if ws.GitRepo != "https://github.com/sozercan/ayna" {
		t.Errorf("gitRepo = %q, want %q", ws.GitRepo, "https://github.com/sozercan/ayna")
	}
	if ws.GitSecretRef == nil || ws.GitSecretRef.Name != "git-credentials" {
		t.Errorf("gitSecretRef = %v, want git-credentials", ws.GitSecretRef)
	}
}

func TestDelegateTaskTool_Execute_AutoRetry(t *testing.T) {
	t.Setenv("ORKA_TASK_NAME", parentTaskName)
	t.Setenv("ORKA_TASK_NAMESPACE", "default")
	t.Setenv("ORKA_COORDINATION_DEPTH", "0")
	t.Setenv("ORKA_COORDINATION_ALLOWED_AGENTS", "researcher")
	t.Setenv("ORKA_COORDINATION_MAX_DEPTH", "3")

	k8sClient := newFakeClient(parentTask(), researcherAgent())
	tool := NewDelegateTaskTool(k8sClient)

	args := json.RawMessage(`{"agent": "researcher", "prompt": "Do the work", "auto_retry": true, "max_retries": 3}`)
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var delegateResult DelegateTaskResult
	if err := json.Unmarshal([]byte(result), &delegateResult); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	// Fetch child task and verify annotations
	childTask := &corev1alpha1.Task{}
	if err := k8sClient.Get(context.Background(), apitypes.NamespacedName{
		Name: delegateResult.TaskName, Namespace: "default",
	}, childTask); err != nil {
		t.Fatalf("get child task: %v", err)
	}

	if childTask.Annotations["orka.ai/auto-retry"] != trueStr {
		t.Errorf("expected auto-retry=true, got %q", childTask.Annotations["orka.ai/auto-retry"])
	}
	if childTask.Annotations["orka.ai/max-retries"] != "3" {
		t.Errorf("expected max-retries=3, got %q", childTask.Annotations["orka.ai/max-retries"])
	}
	if childTask.Annotations["orka.ai/retry-count"] != "0" {
		t.Errorf("expected retry-count=0, got %q", childTask.Annotations["orka.ai/retry-count"])
	}
	if childTask.Annotations["orka.ai/original-prompt"] != "Do the work" {
		t.Errorf("expected original-prompt stored, got %q", childTask.Annotations["orka.ai/original-prompt"])
	}
}

func TestDelegateTaskTool_Execute_AutoRetryDefault(t *testing.T) {
	t.Setenv("ORKA_TASK_NAME", parentTaskName)
	t.Setenv("ORKA_TASK_NAMESPACE", "default")
	t.Setenv("ORKA_COORDINATION_DEPTH", "0")
	t.Setenv("ORKA_COORDINATION_ALLOWED_AGENTS", "researcher")
	t.Setenv("ORKA_COORDINATION_MAX_DEPTH", "3")

	k8sClient := newFakeClient(parentTask(), researcherAgent())
	tool := NewDelegateTaskTool(k8sClient)

	// auto_retry without max_retries should default to 2
	args := json.RawMessage(`{"agent": "researcher", "prompt": "Do work", "auto_retry": true}`)
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var delegateResult DelegateTaskResult
	if err := json.Unmarshal([]byte(result), &delegateResult); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	childTask := &corev1alpha1.Task{}
	if err := k8sClient.Get(context.Background(), apitypes.NamespacedName{
		Name: delegateResult.TaskName, Namespace: "default",
	}, childTask); err != nil {
		t.Fatalf("get child task: %v", err)
	}

	if childTask.Annotations["orka.ai/max-retries"] != "2" {
		t.Errorf("expected default max-retries=2, got %q", childTask.Annotations["orka.ai/max-retries"])
	}
}

func TestDelegateTaskTool_Execute_NoAutoRetry(t *testing.T) {
	t.Setenv("ORKA_TASK_NAME", parentTaskName)
	t.Setenv("ORKA_TASK_NAMESPACE", "default")
	t.Setenv("ORKA_COORDINATION_DEPTH", "0")
	t.Setenv("ORKA_COORDINATION_ALLOWED_AGENTS", "researcher")
	t.Setenv("ORKA_COORDINATION_MAX_DEPTH", "3")

	k8sClient := newFakeClient(parentTask(), researcherAgent())
	tool := NewDelegateTaskTool(k8sClient)

	args := json.RawMessage(`{"agent": "researcher", "prompt": "Do work"}`)
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var delegateResult DelegateTaskResult
	if err := json.Unmarshal([]byte(result), &delegateResult); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	childTask := &corev1alpha1.Task{}
	if err := k8sClient.Get(context.Background(), apitypes.NamespacedName{
		Name: delegateResult.TaskName, Namespace: "default",
	}, childTask); err != nil {
		t.Fatalf("get child task: %v", err)
	}

	// When auto_retry is not set, no retry annotations should be present
	if _, ok := childTask.Annotations["orka.ai/auto-retry"]; ok {
		t.Error("expected no auto-retry annotation when auto_retry is false")
	}
}
