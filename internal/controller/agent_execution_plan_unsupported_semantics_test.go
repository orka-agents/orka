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

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func TestPlanAgentExecutionRejectsUnsupportedBuiltInTaskSemantics(t *testing.T) {
	runtimeTypes := []corev1alpha1.AgentRuntimeType{
		corev1alpha1.AgentRuntimeCodex,
		corev1alpha1.AgentRuntimeClaude,
		corev1alpha1.AgentRuntimeCopilot,
	}

	tests := []struct {
		name        string
		mutateTask  func(*corev1alpha1.Task)
		mutateAgent func(*corev1alpha1.Agent)
		wantPath    agentExecutionPath
		wantReason  string
	}{
		{
			name:     "ordinary task remains supported",
			wantPath: agentExecutionPathACP,
		},
		{
			name: "disabled autonomous coordination remains supported",
			mutateAgent: func(agent *corev1alpha1.Agent) {
				agent.Spec.Coordination = &corev1alpha1.CoordinationConfig{Enabled: true}
			},
			wantPath: agentExecutionPathACP,
		},
		{
			name: "defaulted no-op retry policy remains supported",
			mutateTask: func(task *corev1alpha1.Task) {
				task.Spec.RetryPolicy = &corev1alpha1.RetryPolicy{BackoffMultiplier: 2}
			},
			wantPath: agentExecutionPathACP,
		},
		{
			name: "autonomous coordination fails closed",
			mutateAgent: func(agent *corev1alpha1.Agent) {
				agent.Spec.Coordination = &corev1alpha1.CoordinationConfig{
					Enabled:    true,
					Autonomous: true,
				}
			},
			wantPath:   agentExecutionPathRejected,
			wantReason: "Agent.spec.coordination.autonomous",
		},
		{
			name: "effective retry policy fails closed",
			mutateTask: func(task *corev1alpha1.Task) {
				task.Spec.RetryPolicy = &corev1alpha1.RetryPolicy{
					MaxRetries:        1,
					BackoffMultiplier: 2,
				}
			},
			wantPath:   agentExecutionPathRejected,
			wantReason: "Task.spec.retryPolicy",
		},
	}

	for _, runtimeType := range runtimeTypes {
		for _, tt := range tests {
			t.Run(string(runtimeType)+"/"+tt.name, func(t *testing.T) {
				task := validPlannerTask()
				agent := validPlannerAgent()
				agent.Spec.Runtime.Type = runtimeType
				if tt.mutateTask != nil {
					tt.mutateTask(task)
				}
				if tt.mutateAgent != nil {
					tt.mutateAgent(agent)
				}

				r := newUnitReconciler(newTestScheme())
				r.ACPRuntimeEnabled = true
				plan := r.planAgentExecution(context.Background(), task, agent)

				if plan.path != tt.wantPath {
					t.Fatalf("plan path = %q, want %q (plan=%#v)", plan.path, tt.wantPath, plan)
				}
				if tt.wantReason == "" {
					if plan.rejectionReason != "" {
						t.Fatalf("rejection reason = %q, want empty", plan.rejectionReason)
					}
					return
				}
				if !strings.Contains(plan.rejectionReason, tt.wantReason) {
					t.Fatalf("rejection reason = %q, want substring %q", plan.rejectionReason, tt.wantReason)
				}
			})
		}
	}
}

func TestPlanAgentExecutionAllowsHarnessV1RetryAdmission(t *testing.T) {
	task := validPlannerTask()
	task.Spec.RetryPolicy = &corev1alpha1.RetryPolicy{
		MaxRetries:        1,
		BackoffMultiplier: 2,
	}
	agent := validPlannerAgent()
	contract := corev1alpha1.AgentRuntimeContractHarnessV1
	agent.Spec.Runtime.ContractVersion = &contract

	r := newUnitReconciler(newTestScheme())
	r.HarnessV1Enabled = true
	plan := r.planAgentExecution(context.Background(), task, agent)

	if plan.path != agentExecutionPathHarnessV1 {
		t.Fatalf("plan path = %q, want %q (plan=%#v)", plan.path, agentExecutionPathHarnessV1, plan)
	}
}

func TestPlanAgentExecutionRejectsHarnessV1InheritedAgentAuthority(t *testing.T) {
	tests := []struct {
		name        string
		mutateAgent func(*corev1alpha1.Agent)
		wantReason  string
	}{
		{
			name: "resources",
			mutateAgent: func(agent *corev1alpha1.Agent) {
				agent.Spec.Resources.Requests = corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("1"),
				}
			},
			wantReason: "Agent.spec.resources",
		},
		{
			name: "execution placement",
			mutateAgent: func(agent *corev1alpha1.Agent) {
				agent.Spec.Execution = &corev1alpha1.ExecutionSpec{RuntimeClassName: "kata"}
			},
			wantReason: "Agent.spec.execution",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			task := validPlannerTask()
			agent := validPlannerAgent()
			contract := corev1alpha1.AgentRuntimeContractHarnessV1
			agent.Spec.Runtime.ContractVersion = &contract
			tt.mutateAgent(agent)

			r := newUnitReconciler(newTestScheme())
			r.HarnessV1Enabled = true
			plan := r.planAgentExecution(context.Background(), task, agent)

			if plan.path != agentExecutionPathRejected {
				t.Fatalf("plan path = %q, want %q (plan=%#v)", plan.path, agentExecutionPathRejected, plan)
			}
			if !strings.Contains(plan.rejectionReason, tt.wantReason) {
				t.Fatalf("rejection reason = %q, want substring %q", plan.rejectionReason, tt.wantReason)
			}
		})
	}
}
