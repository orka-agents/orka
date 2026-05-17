/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestDefaultAgentSandboxConfig(t *testing.T) {
	cfg := DefaultAgentSandboxConfig()

	if cfg.RouterURL != "" {
		t.Fatalf("RouterURL = %q, want empty", cfg.RouterURL)
	}
	if cfg.DefaultTemplate != "" {
		t.Fatalf("DefaultTemplate = %q, want empty", cfg.DefaultTemplate)
	}
	if cfg.WarmPoolPolicy != AgentSandboxWarmPoolPolicyDisabled {
		t.Fatalf("WarmPoolPolicy = %q, want %q", cfg.WarmPoolPolicy, AgentSandboxWarmPoolPolicyDisabled)
	}
	if cfg.NamespaceStrategy != AgentSandboxNamespaceStrategyTask {
		t.Fatalf("NamespaceStrategy = %q, want %q", cfg.NamespaceStrategy, AgentSandboxNamespaceStrategyTask)
	}
	if cfg.ClaimTimeout != 2*time.Minute {
		t.Fatalf("ClaimTimeout = %s, want 2m", cfg.ClaimTimeout)
	}
	if cfg.CommandTimeout != 30*time.Minute {
		t.Fatalf("CommandTimeout = %s, want 30m", cfg.CommandTimeout)
	}
	if cfg.CleanupPolicy != corev1alpha1.WorkspaceCleanupPolicyDelete {
		t.Fatalf("CleanupPolicy = %q, want %q", cfg.CleanupPolicy, corev1alpha1.WorkspaceCleanupPolicyDelete)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestAgentSandboxConfigFromEnv(t *testing.T) {
	env := map[string]string{
		EnvAgentSandboxRouterURL:         "http://sandbox-router.orka-system.svc:8080",
		EnvAgentSandboxDefaultTemplate:   "coding-template",
		EnvAgentSandboxWarmPoolPolicy:    AgentSandboxWarmPoolPolicyTemplate,
		EnvAgentSandboxNamespaceStrategy: AgentSandboxNamespaceStrategyController,
		EnvAgentSandboxClaimTimeout:      "45s",
		EnvAgentSandboxCommandTimeout:    "10m",
		EnvAgentSandboxCleanupPolicy:     string(corev1alpha1.WorkspaceCleanupPolicyRetain),
	}

	cfg, err := AgentSandboxConfigFromEnv(func(key string) string { return env[key] })
	if err != nil {
		t.Fatalf("AgentSandboxConfigFromEnv() error = %v", err)
	}
	if cfg.RouterURL != env[EnvAgentSandboxRouterURL] {
		t.Fatalf("RouterURL = %q, want %q", cfg.RouterURL, env[EnvAgentSandboxRouterURL])
	}
	if cfg.DefaultTemplate != env[EnvAgentSandboxDefaultTemplate] {
		t.Fatalf("DefaultTemplate = %q, want %q", cfg.DefaultTemplate, env[EnvAgentSandboxDefaultTemplate])
	}
	if cfg.WarmPoolPolicy != AgentSandboxWarmPoolPolicyTemplate {
		t.Fatalf("WarmPoolPolicy = %q, want %q", cfg.WarmPoolPolicy, AgentSandboxWarmPoolPolicyTemplate)
	}
	if cfg.NamespaceStrategy != AgentSandboxNamespaceStrategyController {
		t.Fatalf("NamespaceStrategy = %q, want %q", cfg.NamespaceStrategy, AgentSandboxNamespaceStrategyController)
	}
	if cfg.ClaimTimeout != 45*time.Second {
		t.Fatalf("ClaimTimeout = %s, want 45s", cfg.ClaimTimeout)
	}
	if cfg.CommandTimeout != 10*time.Minute {
		t.Fatalf("CommandTimeout = %s, want 10m", cfg.CommandTimeout)
	}
	if cfg.CleanupPolicy != corev1alpha1.WorkspaceCleanupPolicyRetain {
		t.Fatalf("CleanupPolicy = %q, want %q", cfg.CleanupPolicy, corev1alpha1.WorkspaceCleanupPolicyRetain)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestAgentSandboxConfigFromEnv_InvalidDuration(t *testing.T) {
	_, err := AgentSandboxConfigFromEnv(func(key string) string {
		if key == EnvAgentSandboxClaimTimeout {
			return "not-a-duration"
		}
		return ""
	})
	if err == nil {
		t.Fatal("expected invalid duration error")
	}
	if !strings.Contains(err.Error(), EnvAgentSandboxClaimTimeout) {
		t.Fatalf("error = %q, want env var name", err.Error())
	}
}

func TestAgentSandboxConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*AgentSandboxConfig)
		wantErr string
	}{
		{
			name: "invalid warm pool policy",
			mutate: func(cfg *AgentSandboxConfig) {
				cfg.WarmPoolPolicy = "always"
			},
			wantErr: "warm pool policy",
		},
		{
			name: "invalid namespace strategy",
			mutate: func(cfg *AgentSandboxConfig) {
				cfg.NamespaceStrategy = "cluster"
			},
			wantErr: "namespace strategy",
		},
		{
			name: "invalid claim timeout",
			mutate: func(cfg *AgentSandboxConfig) {
				cfg.ClaimTimeout = -time.Second
			},
			wantErr: "claim timeout",
		},
		{
			name: "invalid command timeout",
			mutate: func(cfg *AgentSandboxConfig) {
				cfg.CommandTimeout = -time.Second
			},
			wantErr: "command timeout",
		},
		{
			name: "invalid cleanup policy",
			mutate: func(cfg *AgentSandboxConfig) {
				cfg.CleanupPolicy = corev1alpha1.WorkspaceCleanupPolicy("archive")
			},
			wantErr: "cleanup policy",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultAgentSandboxConfig()
			tt.mutate(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("expected error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %q, want substring %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestResolveExecutionWorkspaceRequest(t *testing.T) {
	workspace := func(mutators ...func(*corev1alpha1.ExecutionWorkspaceSpec)) *corev1alpha1.ExecutionWorkspaceSpec {
		ws := &corev1alpha1.ExecutionWorkspaceSpec{
			Enabled: true,
			TemplateRef: &corev1alpha1.WorkspaceTemplateReference{
				Name:      "task-template",
				Namespace: "sandbox-templates",
			},
		}
		for _, mutate := range mutators {
			mutate(ws)
		}
		return ws
	}

	t.Run("disabled workspace returns nil and ignores disabled feature gate", func(t *testing.T) {
		r := &TaskReconciler{}
		task := &corev1alpha1.Task{
			ObjectMeta: metav1.ObjectMeta{Namespace: defaultNS},
			Spec: corev1alpha1.TaskSpec{
				Type: corev1alpha1.TaskTypeAgent,
				Execution: &corev1alpha1.ExecutionSpec{
					Workspace: &corev1alpha1.ExecutionWorkspaceSpec{Enabled: false},
				},
			}}
		request, err := r.resolveExecutionWorkspaceRequest(context.Background(), task)
		if err != nil {
			t.Fatalf("resolveExecutionWorkspaceRequest() error = %v", err)
		}
		if request != nil {
			t.Fatalf("request = %#v, want nil", request)
		}
	})

	t.Run("applies controller defaults", func(t *testing.T) {
		r := &TaskReconciler{
			AgentSandboxEnabled: true,
			AgentSandboxConfig: AgentSandboxConfig{
				RouterURL:         "http://sandbox-router",
				DefaultTemplate:   "default-template",
				WarmPoolPolicy:    AgentSandboxWarmPoolPolicyTemplate,
				NamespaceStrategy: AgentSandboxNamespaceStrategyController,
				ClaimTimeout:      15 * time.Second,
				CommandTimeout:    5 * time.Minute,
				CleanupPolicy:     corev1alpha1.WorkspaceCleanupPolicyRetain,
			},
		}
		task := &corev1alpha1.Task{
			ObjectMeta: metav1.ObjectMeta{Namespace: defaultNS},
			Spec: corev1alpha1.TaskSpec{
				Type: corev1alpha1.TaskTypeAgent,
				Execution: &corev1alpha1.ExecutionSpec{
					Workspace: workspace(func(ws *corev1alpha1.ExecutionWorkspaceSpec) {
						ws.TemplateRef = nil
					}),
				},
			},
		}

		request, err := r.resolveExecutionWorkspaceRequest(context.Background(), task)
		if err != nil {
			t.Fatalf("resolveExecutionWorkspaceRequest() error = %v", err)
		}
		if request == nil {
			t.Fatal("request = nil, want resolved request")
		}
		if request.TemplateName != "default-template" {
			t.Fatalf("TemplateName = %q, want default-template", request.TemplateName)
		}
		if request.TemplateNamespace != defaultNS {
			t.Fatalf("TemplateNamespace = %q, want %s", request.TemplateNamespace, defaultNS)
		}
		if request.ReusePolicy != corev1alpha1.WorkspaceReusePolicyNone {
			t.Fatalf("ReusePolicy = %q, want %q", request.ReusePolicy, corev1alpha1.WorkspaceReusePolicyNone)
		}
		if request.CleanupPolicy != corev1alpha1.WorkspaceCleanupPolicyRetain {
			t.Fatalf("CleanupPolicy = %q, want %q", request.CleanupPolicy, corev1alpha1.WorkspaceCleanupPolicyRetain)
		}
		if request.RouterURL != "http://sandbox-router" || request.WarmPoolPolicy != AgentSandboxWarmPoolPolicyTemplate || request.NamespaceStrategy != AgentSandboxNamespaceStrategyController {
			t.Fatalf("request did not carry controller config: %#v", request)
		}
		if request.ClaimTimeout != 15*time.Second || request.CommandTimeout != 5*time.Minute {
			t.Fatalf("request timeouts = %s/%s, want 15s/5m", request.ClaimTimeout, request.CommandTimeout)
		}
	})

	t.Run("task fields override controller defaults", func(t *testing.T) {
		r := &TaskReconciler{
			AgentSandboxEnabled: true,
			AgentSandboxConfig: AgentSandboxConfig{
				DefaultTemplate: "default-template",
				CleanupPolicy:   corev1alpha1.WorkspaceCleanupPolicyDelete,
			},
		}
		task := &corev1alpha1.Task{Spec: corev1alpha1.TaskSpec{
			Type:       corev1alpha1.TaskTypeAgent,
			SessionRef: &corev1alpha1.SessionReference{Name: "session-1"},
			Execution: &corev1alpha1.ExecutionSpec{
				Workspace: workspace(func(ws *corev1alpha1.ExecutionWorkspaceSpec) {
					ws.ReusePolicy = corev1alpha1.WorkspaceReusePolicySession
					ws.CleanupPolicy = corev1alpha1.WorkspaceCleanupPolicyRetain
				}),
			},
		}}

		request, err := r.resolveExecutionWorkspaceRequest(context.Background(), task)
		if err != nil {
			t.Fatalf("resolveExecutionWorkspaceRequest() error = %v", err)
		}
		if request.TemplateName != "task-template" {
			t.Fatalf("TemplateName = %q, want task-template", request.TemplateName)
		}
		if request.TemplateNamespace != "sandbox-templates" {
			t.Fatalf("TemplateNamespace = %q, want sandbox-templates", request.TemplateNamespace)
		}
		if request.ReusePolicy != corev1alpha1.WorkspaceReusePolicySession {
			t.Fatalf("ReusePolicy = %q, want %q", request.ReusePolicy, corev1alpha1.WorkspaceReusePolicySession)
		}
		if request.ReuseKey != "session-1" {
			t.Fatalf("ReuseKey = %q, want session-1", request.ReuseKey)
		}
		if request.CleanupPolicy != corev1alpha1.WorkspaceCleanupPolicyRetain {
			t.Fatalf("CleanupPolicy = %q, want %q", request.CleanupPolicy, corev1alpha1.WorkspaceCleanupPolicyRetain)
		}
	})
}
