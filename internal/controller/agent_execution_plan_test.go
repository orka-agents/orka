/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	sandboxextv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
)

func TestPlanAgentExecutionMatrix(t *testing.T) {
	scheme := newTestScheme()
	baseAgent := validPlannerAgent()

	tests := []struct {
		name                   string
		mutateTask             func(*corev1alpha1.Task)
		mutateAgent            func(*corev1alpha1.Agent)
		objects                []client.Object
		agentSandboxEnabled    bool
		wantPath               agentExecutionPath
		wantReason             string
		wantWorkspaceStatusErr string
	}{
		{
			name:     "plain agent task runs as harness wrapper turn",
			wantPath: agentExecutionPathHarnessWrapper,
		},
		{
			name: "transaction token delegation is rejected before harness start",
			mutateTask: func(task *corev1alpha1.Task) {
				task.Spec.Transaction = &corev1alpha1.TaskTransaction{ID: "txn-1"}
			},
			wantPath:   agentExecutionPathRejected,
			wantReason: "transaction token delegation",
		},
		{
			name: "task resources are rejected before harness start",
			mutateTask: func(task *corev1alpha1.Task) {
				task.Spec.Resources.Requests = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}
			},
			wantPath:   agentExecutionPathRejected,
			wantReason: "custom Kubernetes resources",
		},
		{
			name: "agent resources are rejected before harness start",
			mutateAgent: func(agent *corev1alpha1.Agent) {
				agent.Spec.Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}
			},
			wantPath:   agentExecutionPathRejected,
			wantReason: "custom Kubernetes resources",
		},
		{
			name: "task execution placement is rejected before harness start",
			mutateTask: func(task *corev1alpha1.Task) {
				task.Spec.Execution = &corev1alpha1.ExecutionSpec{RuntimeClassName: "kata"}
			},
			wantPath:   agentExecutionPathRejected,
			wantReason: "execution placement",
		},
		{
			name: "agent execution placement is rejected before harness start",
			mutateAgent: func(agent *corev1alpha1.Agent) {
				agent.Spec.Execution = &corev1alpha1.ExecutionSpec{NodeSelector: map[string]string{"disk": "ssd"}}
			},
			wantPath:   agentExecutionPathRejected,
			wantReason: "execution placement",
		},
		{
			name: "valid execution workspace is rejected with workspace status error until harness supports it",
			mutateTask: func(task *corev1alpha1.Task) {
				task.Spec.Execution = &corev1alpha1.ExecutionSpec{Workspace: &corev1alpha1.ExecutionWorkspaceSpec{
					Enabled:     true,
					Provider:    corev1alpha1.WorkspaceProviderAgentSandbox,
					TemplateRef: &corev1alpha1.WorkspaceTemplateReference{Name: "sandbox-template"},
				}}
			},
			objects: []client.Object{&sandboxextv1alpha1.SandboxTemplate{ObjectMeta: metav1.ObjectMeta{
				Name:      "sandbox-template",
				Namespace: defaultNS,
			}}},
			agentSandboxEnabled:    true,
			wantPath:               agentExecutionPathRejected,
			wantReason:             "execution workspace is not supported by harness runtime yet",
			wantWorkspaceStatusErr: "execution workspace is not supported by harness runtime yet",
		},
		{
			name: "execution workspace resolution failure is surfaced for status update",
			mutateTask: func(task *corev1alpha1.Task) {
				task.Spec.Execution = &corev1alpha1.ExecutionSpec{Workspace: &corev1alpha1.ExecutionWorkspaceSpec{
					Enabled:     true,
					Provider:    corev1alpha1.WorkspaceProviderAgentSandbox,
					TemplateRef: &corev1alpha1.WorkspaceTemplateReference{Name: "missing-template"},
				}}
			},
			agentSandboxEnabled:    true,
			wantPath:               agentExecutionPathRejected,
			wantReason:             "failed to resolve execution workspace",
			wantWorkspaceStatusErr: "missing-template",
		},
		{
			name: "execution workspace resolution failure is surfaced before placement rejection",
			mutateTask: func(task *corev1alpha1.Task) {
				task.Spec.Execution = &corev1alpha1.ExecutionSpec{
					RuntimeClassName: "kata",
					Workspace: &corev1alpha1.ExecutionWorkspaceSpec{
						Enabled:     true,
						Provider:    corev1alpha1.WorkspaceProviderAgentSandbox,
						TemplateRef: &corev1alpha1.WorkspaceTemplateReference{Name: "missing-template"},
					},
				}
			},
			agentSandboxEnabled:    true,
			wantPath:               agentExecutionPathRejected,
			wantReason:             "failed to resolve execution workspace",
			wantWorkspaceStatusErr: "missing-template",
		},
		{
			name: "cross namespace prior task is rejected only when namespace isolation is enforced",
			mutateTask: func(task *corev1alpha1.Task) {
				task.Spec.PriorTaskRef = &corev1alpha1.PriorTaskReference{Name: "parent", Namespace: "other"}
			},
			wantPath:   agentExecutionPathRejected,
			wantReason: "cross-namespace priorTaskRef",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			task := validPlannerTask()
			agent := baseAgent.DeepCopy()
			if tt.mutateTask != nil {
				tt.mutateTask(task)
			}
			if tt.mutateAgent != nil {
				tt.mutateAgent(agent)
			}

			objs := make([]client.Object, 0, len(tt.objects))
			objs = append(objs, tt.objects...)
			r := newUnitReconciler(scheme, objs...)
			r.AgentSandboxEnabled = tt.agentSandboxEnabled
			r.EnforceNamespaceIsolation = true

			plan := r.planAgentExecution(context.Background(), task, agent)
			if plan.path != tt.wantPath {
				t.Fatalf("plan path = %q, want %q (plan=%#v)", plan.path, tt.wantPath, plan)
			}
			if tt.wantReason != "" && !strings.Contains(plan.rejectionReason, tt.wantReason) {
				t.Fatalf("rejection reason = %q, want substring %q", plan.rejectionReason, tt.wantReason)
			}
			if tt.wantWorkspaceStatusErr == "" {
				if plan.workspaceStatusError != nil {
					t.Fatalf("workspaceStatusError = %v, want nil", plan.workspaceStatusError)
				}
				return
			}
			if plan.workspaceStatusError == nil || !strings.Contains(plan.workspaceStatusError.Error(), tt.wantWorkspaceStatusErr) {
				t.Fatalf("workspaceStatusError = %v, want substring %q", plan.workspaceStatusError, tt.wantWorkspaceStatusErr)
			}
		})
	}
}

func TestPlanAgentExecutionAllowsCrossNamespacePriorTaskWhenIsolationDisabled(t *testing.T) {
	r := &TaskReconciler{EnforceNamespaceIsolation: false}
	task := validPlannerTask()
	task.Spec.PriorTaskRef = &corev1alpha1.PriorTaskReference{Name: "parent", Namespace: "other"}

	plan := r.planAgentExecution(context.Background(), task, validPlannerAgent())
	if plan.path != agentExecutionPathHarnessWrapper {
		t.Fatalf("plan path = %q, want %q", plan.path, agentExecutionPathHarnessWrapper)
	}
}

func validPlannerTask() *corev1alpha1.Task {
	return &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: defaultNS},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAgent,
			Prompt: "do work",
		},
	}
}

func validPlannerAgent() *corev1alpha1.Agent {
	return &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: defaultNS},
		Spec: corev1alpha1.AgentSpec{
			Runtime: &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeCodex},
		},
	}
}
