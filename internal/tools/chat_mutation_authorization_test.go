/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
)

func TestChatCancelTaskTool_Execute_AuthorizesBeforeDelete(t *testing.T) {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: testMyTaskName, Namespace: defaultNamespace},
	}
	fc := newFakeClient(task)
	ctx := WithToolContext(context.Background(), &ToolContext{
		Client:    fc,
		Namespace: defaultNamespace,
		AuthorizeTaskDelete: func(context.Context, *corev1alpha1.Task) *ChatToolError {
			return &ChatToolError{Type: "authorization_failed", Message: "task delete denied"}
		},
	})

	result, err := (&ChatCancelTaskTool{}).Execute(ctx, json.RawMessage(`{"name":"`+testMyTaskName+`"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertChatAuthorizationFailure(t, result)

	var remaining corev1alpha1.Task
	if err := fc.Get(context.Background(), client.ObjectKey{Name: testMyTaskName, Namespace: defaultNamespace}, &remaining); err != nil {
		t.Fatalf("task should not have been deleted: %v", err)
	}
}

func TestUpdateAgentTool_Execute_AuthorizesBeforeUpdate(t *testing.T) {
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: testMyAgentName, Namespace: defaultNamespace},
		Spec:       corev1alpha1.AgentSpec{},
	}
	fc := newFakeClient(agent)
	ctx := WithToolContext(context.Background(), &ToolContext{
		Client:    fc,
		Namespace: defaultNamespace,
		AuthorizeAgentUpdate: func(context.Context, *corev1alpha1.Agent) *ChatToolError {
			return &ChatToolError{Type: "authorization_failed", Message: "agent update denied"}
		},
	})

	args := map[string]any{nameField: testMyAgentName, systemPromptField: "blocked update"}
	argsJSON, _ := json.Marshal(args)
	result, err := (&UpdateAgentTool{}).Execute(ctx, argsJSON)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertChatAuthorizationFailure(t, result)

	var remaining corev1alpha1.Agent
	if err := fc.Get(context.Background(), client.ObjectKey{Name: testMyAgentName, Namespace: defaultNamespace}, &remaining); err != nil {
		t.Fatalf("agent should still exist: %v", err)
	}
	if remaining.Spec.SystemPrompt != nil {
		t.Fatalf("agent update should not have been persisted: %#v", remaining.Spec.SystemPrompt)
	}
}

func TestChatDeleteAgentTool_Execute_AuthorizesBeforeDelete(t *testing.T) {
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: testMyAgentName, Namespace: defaultNamespace},
		Spec:       corev1alpha1.AgentSpec{},
	}
	fc := newFakeClient(agent)
	ctx := WithToolContext(context.Background(), &ToolContext{
		Client:    fc,
		Namespace: defaultNamespace,
		AuthorizeAgentDelete: func(context.Context, *corev1alpha1.Agent) *ChatToolError {
			return &ChatToolError{Type: "authorization_failed", Message: "agent delete denied"}
		},
	})

	result, err := (&ChatDeleteAgentTool{}).Execute(ctx, json.RawMessage(`{"name":"`+testMyAgentName+`"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertChatAuthorizationFailure(t, result)

	var remaining corev1alpha1.Agent
	if err := fc.Get(context.Background(), client.ObjectKey{Name: testMyAgentName, Namespace: defaultNamespace}, &remaining); err != nil {
		t.Fatalf("agent should not have been deleted: %v", err)
	}
}

func assertChatAuthorizationFailure(t *testing.T, result string) {
	t.Helper()
	var r ChatToolResult
	if err := json.Unmarshal([]byte(result), &r); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if r.Success {
		t.Fatalf("expected authorization failure, got success: %#v", r)
	}
	if r.ErrorType != "authorization_failed" {
		t.Fatalf("errorType = %q, want authorization_failed", r.ErrorType)
	}
}
