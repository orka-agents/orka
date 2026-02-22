/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
)

func TestNewSystemPromptBuilder(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	b := NewSystemPromptBuilder(c, "test-ns")
	if b == nil {
		t.Fatal("expected non-nil builder")
	}
	if b.namespace != "test-ns" {
		t.Errorf("namespace = %q, want %q", b.namespace, "test-ns")
	}
	if b.client == nil {
		t.Error("expected non-nil client")
	}
	if b.cachedPrompt != "" {
		t.Error("expected empty cachedPrompt")
	}
	if b.cachedHash != "" {
		t.Error("expected empty cachedHash")
	}
}

func TestBuildIdentitySection(t *testing.T) {
	s := buildIdentitySection()
	if !strings.Contains(s, "<identity>") {
		t.Error("missing <identity> tag")
	}
	if !strings.Contains(s, "</identity>") {
		t.Error("missing </identity> tag")
	}
	if !strings.Contains(s, "Orka orchestrator") {
		t.Error("missing orchestrator mention")
	}
}

func TestBuildCapabilitiesSection(t *testing.T) {
	s := buildCapabilitiesSection()
	if !strings.Contains(s, "<capabilities>") {
		t.Error("missing <capabilities> tag")
	}
	if !strings.Contains(s, "</capabilities>") {
		t.Error("missing </capabilities> tag")
	}
	if !strings.Contains(s, "three types of tasks") {
		t.Error("missing task types mention")
	}
}

func TestBuildBehaviorSection(t *testing.T) {
	s := buildBehaviorSection()
	if !strings.Contains(s, "<behavior>") {
		t.Error("missing <behavior> tag")
	}
	if !strings.Contains(s, "CRITICAL RULE") {
		t.Error("missing critical rule")
	}
}

func TestBuildToolCallStyleSection(t *testing.T) {
	s := buildToolCallStyleSection()
	if !strings.Contains(s, "<tool_call_style>") {
		t.Error("missing <tool_call_style> tag")
	}
	if !strings.Contains(s, "narrate") {
		t.Error("missing narrate guidance")
	}
}

func TestBuildTaskTypesSection(t *testing.T) {
	tests := []struct {
		name          string
		mode          PromptMode
		wantImages    bool
		wantContainer bool
	}{
		{
			name:          "full mode includes images",
			mode:          PromptModeFull,
			wantImages:    true,
			wantContainer: true,
		},
		{
			name:          "minimal mode omits images",
			mode:          PromptModeMinimal,
			wantImages:    false,
			wantContainer: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := buildTaskTypesSection(tt.mode)
			if !strings.Contains(s, "<task_types>") {
				t.Error("missing <task_types> tag")
			}
			if !strings.Contains(s, "</task_types>") {
				t.Error("missing </task_types> tag")
			}
			if tt.wantContainer && !strings.Contains(s, "container") {
				t.Error("missing container task type")
			}
			hasImages := strings.Contains(s, "cgr.dev/chainguard")
			if hasImages != tt.wantImages {
				t.Errorf("images present = %v, want %v", hasImages, tt.wantImages)
			}
		})
	}
}

func TestBuildCoordinationSection(t *testing.T) {
	tests := []struct {
		name     string
		mode     PromptMode
		wantLen  bool
		wantText string
	}{
		{
			name:     "full mode includes coordination",
			mode:     PromptModeFull,
			wantLen:  true,
			wantText: "<coordination>",
		},
		{
			name:    "minimal mode returns empty",
			mode:    PromptModeMinimal,
			wantLen: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := buildCoordinationSection(tt.mode)
			if tt.wantLen {
				if !strings.Contains(s, tt.wantText) {
					t.Errorf("missing %q", tt.wantText)
				}
				if !strings.Contains(s, "delegate_task") {
					t.Error("missing delegate_task mention")
				}
			} else {
				if s != "" {
					t.Errorf("expected empty string for minimal mode, got %d bytes", len(s))
				}
			}
		})
	}
}

func TestBuildSchedulingSection(t *testing.T) {
	tests := []struct {
		name    string
		mode    PromptMode
		wantLen bool
	}{
		{"full mode includes scheduling", PromptModeFull, true},
		{"minimal mode returns empty", PromptModeMinimal, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := buildSchedulingSection(tt.mode)
			if tt.wantLen {
				if !strings.Contains(s, "<scheduling>") {
					t.Error("missing <scheduling> tag")
				}
				if !strings.Contains(s, "cron") {
					t.Error("missing cron mention")
				}
			} else {
				if s != "" {
					t.Error("expected empty for minimal mode")
				}
			}
		})
	}
}

func TestBuildRulesSection(t *testing.T) {
	s := buildRulesSection()
	if !strings.Contains(s, "<rules>") {
		t.Error("missing <rules> tag")
	}
	if !strings.Contains(s, "</rules>") {
		t.Error("missing </rules> tag")
	}
	if !strings.Contains(s, "create_container_task") {
		t.Error("missing create_container_task mention")
	}
}

func TestBuildExamplesSection(t *testing.T) {
	tests := []struct {
		name    string
		mode    PromptMode
		wantLen bool
	}{
		{"full mode includes examples", PromptModeFull, true},
		{"minimal mode returns empty", PromptModeMinimal, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := buildExamplesSection(tt.mode)
			if tt.wantLen {
				if !strings.Contains(s, "<examples>") {
					t.Error("missing <examples> tag")
				}
				if !strings.Contains(s, "Example 1") {
					t.Error("missing Example 1")
				}
			} else {
				if s != "" {
					t.Error("expected empty for minimal mode")
				}
			}
		})
	}
}

func TestFormatAgent(t *testing.T) {
	boolTrue := true
	boolFalse := false

	tests := []struct {
		name    string
		agent   *corev1alpha1.Agent
		want    []string // substrings expected
		notWant []string // substrings not expected
	}{
		{
			name: "agent with no spec fields",
			agent: &corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{Name: "simple"},
			},
			want: []string{"simple"},
		},
		{
			name: "agent with model name only",
			agent: &corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{Name: "modeled"},
				Spec: corev1alpha1.AgentSpec{
					Model: &corev1alpha1.ModelConfig{Name: "gpt-4"},
				},
			},
			want:    []string{"modeled", "model: gpt-4"},
			notWant: []string{"provider:"},
		},
		{
			name: "agent with model and provider",
			agent: &corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{Name: "full-model"},
				Spec: corev1alpha1.AgentSpec{
					Model: &corev1alpha1.ModelConfig{
						Name:     "gpt-4",
						Provider: "openai",
					},
				},
			},
			want: []string{"full-model", "model: gpt-4", "provider: openai"},
		},
		{
			name: "agent with enabled tools",
			agent: &corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{Name: "tooled"},
				Spec: corev1alpha1.AgentSpec{
					Tools: []corev1alpha1.ToolReference{
						{Name: "web_search", Enabled: &boolTrue},
						{Name: "code_exec", Enabled: &boolTrue},
					},
				},
			},
			want: []string{"tooled", "tools: [web_search, code_exec]"},
		},
		{
			name: "agent with disabled tools excluded",
			agent: &corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{Name: "partial-tools"},
				Spec: corev1alpha1.AgentSpec{
					Tools: []corev1alpha1.ToolReference{
						{Name: "enabled_tool", Enabled: &boolTrue},
						{Name: "disabled_tool", Enabled: &boolFalse},
					},
				},
			},
			want:    []string{"enabled_tool"},
			notWant: []string{"disabled_tool"},
		},
		{
			name: "agent with tools nil enabled defaults to enabled",
			agent: &corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{Name: "default-tools"},
				Spec: corev1alpha1.AgentSpec{
					Tools: []corev1alpha1.ToolReference{
						{Name: "default_tool"},
					},
				},
			},
			want: []string{"default_tool"},
		},
		{
			name: "agent with runtime",
			agent: &corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{Name: "runner"},
				Spec: corev1alpha1.AgentSpec{
					Runtime: &corev1alpha1.AgentCLIRuntime{
						Type: corev1alpha1.AgentRuntimeCopilot,
					},
				},
			},
			want: []string{"runner", "runtime: copilot"},
		},
		{
			name: "agent with all fields",
			agent: &corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{Name: "complete"},
				Spec: corev1alpha1.AgentSpec{
					Model: &corev1alpha1.ModelConfig{
						Name:     "claude-3",
						Provider: "anthropic",
					},
					Tools: []corev1alpha1.ToolReference{
						{Name: "search", Enabled: &boolTrue},
					},
					Runtime: &corev1alpha1.AgentCLIRuntime{
						Type: corev1alpha1.AgentRuntimeClaude,
					},
				},
			},
			want: []string{"complete", "model: claude-3", "provider: anthropic", "tools: [search]", "runtime: claude"},
		},
		{
			name: "agent with all tools disabled shows no tools section",
			agent: &corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{Name: "no-tools"},
				Spec: corev1alpha1.AgentSpec{
					Tools: []corev1alpha1.ToolReference{
						{Name: "disabled1", Enabled: &boolFalse},
						{Name: "disabled2", Enabled: &boolFalse},
					},
				},
			},
			notWant: []string{"tools:"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatAgent(tt.agent)
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("formatAgent() = %q, missing %q", got, w)
				}
			}
			for _, nw := range tt.notWant {
				if strings.Contains(got, nw) {
					t.Errorf("formatAgent() = %q, should not contain %q", got, nw)
				}
			}
		})
	}
}

func TestComputeHash(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	b := NewSystemPromptBuilder(c, "default")

	t.Run("consistent for same inputs", func(t *testing.T) {
		h1 := b.computeHash("agents", "tools", "providers", "skills")
		h2 := b.computeHash("agents", "tools", "providers", "skills")
		if h1 != h2 {
			t.Errorf("hash mismatch: %q != %q", h1, h2)
		}
	})

	t.Run("different for different inputs", func(t *testing.T) {
		h1 := b.computeHash("agents1", "tools", "providers", "skills")
		h2 := b.computeHash("agents2", "tools", "providers", "skills")
		if h1 == h2 {
			t.Error("expected different hashes for different inputs")
		}
	})

	t.Run("returns 16-char hex string", func(t *testing.T) {
		h := b.computeHash("a", "b", "c", "d")
		if len(h) != 16 {
			t.Errorf("hash length = %d, want 16", len(h))
		}
		for _, c := range h {
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
				t.Errorf("hash %q contains non-hex char %q", h, string(c))
			}
		}
	})

	t.Run("empty inputs produce valid hash", func(t *testing.T) {
		h := b.computeHash("", "", "", "")
		if len(h) != 16 {
			t.Errorf("hash length = %d, want 16", len(h))
		}
	})
}

func TestBuildDynamicContext(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	t.Run("no resources returns defaults", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).Build()
		b := NewSystemPromptBuilder(c, "default")

		agents, tools, providers, skills, err := b.buildDynamicContext(context.Background())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(agents, "No agents configured") {
			t.Errorf("agents = %q, want 'No agents configured'", agents)
		}
		if !strings.Contains(tools, "web_search") {
			t.Error("missing built-in tool web_search")
		}
		if !strings.Contains(tools, "code_exec") {
			t.Error("missing built-in tool code_exec")
		}
		if !strings.Contains(tools, "file_read") {
			t.Error("missing built-in tool file_read")
		}
		if !strings.Contains(providers, "agents=0") {
			t.Errorf("providers = %q, expected agents=0", providers)
		}
		if !strings.Contains(providers, "tools=5") {
			t.Errorf("providers = %q, expected tools=5 (built-in only)", providers)
		}
		if skills != "" {
			t.Errorf("skills = %q, expected empty", skills)
		}
	})

	t.Run("with agents and tools", func(t *testing.T) {
		agent := &corev1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: "default"},
			Spec: corev1alpha1.AgentSpec{
				Model: &corev1alpha1.ModelConfig{Name: "gpt-4"},
			},
		}
		tool := &corev1alpha1.Tool{
			ObjectMeta: metav1.ObjectMeta{Name: "custom-tool", Namespace: "default"},
			Spec:       corev1alpha1.ToolSpec{Description: "A custom tool"},
		}
		provider := &corev1alpha1.Provider{
			ObjectMeta: metav1.ObjectMeta{Name: "openai", Namespace: "default"},
		}
		skill := &corev1alpha1.Skill{
			ObjectMeta: metav1.ObjectMeta{Name: "researcher-skill", Namespace: "default"},
			Spec: corev1alpha1.SkillSpec{
				DisplayName: "Research Skill",
				Description: "Research workflow guidance",
				Content:     corev1alpha1.SkillContent{Inline: "Use reliable sources."},
			},
		}

		c := fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(agent, tool, provider, skill).
			Build()
		b := NewSystemPromptBuilder(c, "default")

		agents, tools, providers, skills, err := b.buildDynamicContext(context.Background())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(agents, "coder") {
			t.Error("missing agent 'coder'")
		}
		if !strings.Contains(tools, "custom-tool - A custom tool") {
			t.Errorf("tools = %q, missing custom-tool", tools)
		}
		if !strings.Contains(providers, "openai") {
			t.Errorf("providers = %q, missing openai", providers)
		}
		if !strings.Contains(providers, "agents=1") {
			t.Errorf("providers = %q, expected agents=1", providers)
		}
		// 3 built-in + 1 custom = 4
		if !strings.Contains(providers, "tools=6") {
			t.Errorf("providers = %q, expected tools=6", providers)
		}
		if !strings.Contains(skills, "researcher-skill - Research Skill: Research workflow guidance") {
			t.Errorf("skills = %q, missing skill summary", skills)
		}
	})

	t.Run("copilot runtime detected from github-credentials secret", func(t *testing.T) {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "github-credentials", Namespace: "default"},
		}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
		b := NewSystemPromptBuilder(c, "default")

		_, _, providers, _, err := b.buildDynamicContext(context.Background())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(providers, "copilot") {
			t.Errorf("providers = %q, expected copilot runtime", providers)
		}
	})

	t.Run("claude runtime detected from anthropic-api-key secret", func(t *testing.T) {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "anthropic-api-key", Namespace: "default"},
		}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
		b := NewSystemPromptBuilder(c, "default")

		_, _, providers, _, err := b.buildDynamicContext(context.Background())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(providers, "claude") {
			t.Errorf("providers = %q, expected claude runtime", providers)
		}
	})

	t.Run("no runtime secrets means agent_runtimes=none", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).Build()
		b := NewSystemPromptBuilder(c, "default")

		_, _, providers, _, err := b.buildDynamicContext(context.Background())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(providers, "agent_runtimes=[none]") {
			t.Errorf("providers = %q, expected agent_runtimes=[none]", providers)
		}
	})

	t.Run("namespace filtering works", func(t *testing.T) {
		agent := &corev1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: "other-ns-agent", Namespace: "other"},
		}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(agent).Build()
		b := NewSystemPromptBuilder(c, "default")

		agents, _, _, _, err := b.buildDynamicContext(context.Background())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if strings.Contains(agents, "other-ns-agent") {
			t.Error("agent from other namespace should not appear")
		}
	})
}

func TestBuildSystemPrompt(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	t.Run("full mode includes all sections", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).Build()
		b := NewSystemPromptBuilder(c, "default")

		prompt, err := b.BuildSystemPrompt(context.Background(), "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		for _, section := range []string{
			"<identity>", "<capabilities>", "<behavior>",
			"<tool_call_style>", "<task_types>", "<coordination>",
			"<scheduling>", "<rules>", "<examples>",
			"<available_agents>", "<available_tools>",
		} {
			if !strings.Contains(prompt, section) {
				t.Errorf("missing section %q", section)
			}
		}
	})

	t.Run("minimal mode omits optional sections", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).Build()
		b := NewSystemPromptBuilder(c, "default")

		prompt, err := b.BuildSystemPrompt(context.Background(), "", PromptModeMinimal)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if strings.Contains(prompt, "<coordination>") {
			t.Error("minimal mode should not include <coordination>")
		}
		if strings.Contains(prompt, "<scheduling>") {
			t.Error("minimal mode should not include <scheduling>")
		}
		if strings.Contains(prompt, "<examples>") {
			t.Error("minimal mode should not include <examples>")
		}
		// Core sections should still be present
		if !strings.Contains(prompt, "<identity>") {
			t.Error("minimal mode should include <identity>")
		}
	})

	t.Run("user system prompt appended", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).Build()
		b := NewSystemPromptBuilder(c, "default")

		prompt, err := b.BuildSystemPrompt(context.Background(), "Be helpful and concise.")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(prompt, "<user_instructions>") {
			t.Error("missing <user_instructions> tag")
		}
		if !strings.Contains(prompt, "Be helpful and concise.") {
			t.Error("missing user prompt content")
		}
	})

	t.Run("empty user prompt omits user_instructions", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).Build()
		b := NewSystemPromptBuilder(c, "default")

		prompt, err := b.BuildSystemPrompt(context.Background(), "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if strings.Contains(prompt, "<user_instructions>") {
			t.Error("should not include <user_instructions> for empty user prompt")
		}
	})

	t.Run("caching returns same prompt for unchanged context", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).Build()
		b := NewSystemPromptBuilder(c, "default")

		p1, err := b.BuildSystemPrompt(context.Background(), "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		p2, err := b.BuildSystemPrompt(context.Background(), "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if p1 != p2 {
			t.Error("expected cached prompt to be identical")
		}
	})

	t.Run("skill changes invalidate cache", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).Build()
		b := NewSystemPromptBuilder(c, "default")

		p1, err := b.BuildSystemPrompt(context.Background(), "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		skill := &corev1alpha1.Skill{
			ObjectMeta: metav1.ObjectMeta{Name: "cache-skill", Namespace: "default"},
			Spec: corev1alpha1.SkillSpec{
				DisplayName: "Cache Skill",
				Description: "Cache invalidation test skill",
				Content:     corev1alpha1.SkillContent{Inline: "Use this skill."},
			},
		}
		if err := c.Create(context.Background(), skill); err != nil {
			t.Fatalf("create skill: %v", err)
		}

		p2, err := b.BuildSystemPrompt(context.Background(), "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if p1 == p2 {
			t.Error("prompt should change after adding a skill")
		}
		if !strings.Contains(p2, "<available_skills>") {
			t.Error("prompt should include <available_skills> section after adding a skill")
		}
	})

	t.Run("user prompt bypasses cache", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).Build()
		b := NewSystemPromptBuilder(c, "default")

		p1, err := b.BuildSystemPrompt(context.Background(), "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		p2, err := b.BuildSystemPrompt(context.Background(), "Custom instructions")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if p1 == p2 {
			t.Error("prompt with user instructions should differ from cached")
		}
		if !strings.Contains(p2, "Custom instructions") {
			t.Error("missing custom instructions in prompt")
		}
	})
}

func TestBuildDynamicContextSkillListError(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*corev1alpha1.SkillList); ok {
					return fmt.Errorf("simulated API server error")
				}
				return c.List(ctx, list, opts...)
			},
		}).
		Build()
	b := NewSystemPromptBuilder(c, "default")

	_, _, _, _, err := b.buildDynamicContext(context.Background())
	if err == nil {
		t.Fatal("expected error when skill listing fails")
	}
	if !strings.Contains(err.Error(), "listing skills") {
		t.Errorf("error = %q, expected 'listing skills' message", err.Error())
	}
}
