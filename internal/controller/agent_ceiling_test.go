package controller

import (
	"encoding/json"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
)

func ceilingTestAgent(runtimeType corev1alpha1.AgentRuntimeType, ceiling *corev1alpha1.AgentRuntimeCeiling) *corev1alpha1.Agent {
	return &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "reviewer", Namespace: "team-a"},
		Spec: corev1alpha1.AgentSpec{Runtime: &corev1alpha1.AgentCLIRuntime{
			Type:    runtimeType,
			Ceiling: ceiling,
		}},
	}
}

func ceilingTestTask(allowed []string, allowBash *bool) *corev1alpha1.Task {
	return &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: "team-a"},
		Spec: corev1alpha1.TaskSpec{
			Type:         corev1alpha1.TaskTypeAgent,
			Prompt:       "fix the flaky test",
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{AllowedTools: allowed, AllowBash: allowBash},
		},
	}
}

func TestValidateAgentRuntimeCeiling(t *testing.T) {
	yes, no := new(bool), new(bool)
	*yes = true
	turns := func(n int32) *int32 { return &n }
	tools := func(names ...string) []string { return append([]string{}, names...) }

	tests := []struct {
		name    string
		agent   *corev1alpha1.Agent
		task    *corev1alpha1.Task
		wantErr string
	}{
		{
			name:  "no ceiling keeps defaults replaceable",
			agent: ceilingTestAgent(corev1alpha1.AgentRuntimeClaude, nil),
			task:  ceilingTestTask(tools("Read", "Write", "Bash"), yes),
		},
		{
			name:  "task narrows within ceiling",
			agent: ceilingTestAgent(corev1alpha1.AgentRuntimeClaude, &corev1alpha1.AgentRuntimeCeiling{AllowedTools: tools("Read", "Grep", "Edit")}),
			task:  ceilingTestTask(tools("Read"), nil),
		},
		{
			name:    "task tool above ceiling",
			agent:   ceilingTestAgent(corev1alpha1.AgentRuntimeClaude, &corev1alpha1.AgentRuntimeCeiling{AllowedTools: tools("Read", "Grep", "Edit")}),
			task:    ceilingTestTask(tools("Read", "Write"), nil),
			wantErr: `does not allow tool "Write"`,
		},
		{
			name: "agent defaults above ceiling",
			agent: func() *corev1alpha1.Agent {
				agent := ceilingTestAgent(corev1alpha1.AgentRuntimeClaude, &corev1alpha1.AgentRuntimeCeiling{AllowedTools: tools("Read")})
				agent.Spec.Runtime.DefaultAllowedTools = tools("Read", "Edit")
				return agent
			}(),
			task:    ceilingTestTask(nil, nil),
			wantErr: `does not allow tool "Edit"`,
		},
		{
			name:    "implicit native grant exceeds a tool ceiling",
			agent:   ceilingTestAgent(corev1alpha1.AgentRuntimeClaude, &corev1alpha1.AgentRuntimeCeiling{AllowedTools: tools("Read")}),
			task:    ceilingTestTask(nil, nil),
			wantErr: "unrestricted native tool set",
		},
		{
			name:    "all-blank allowlist is judged unrestricted",
			agent:   ceilingTestAgent(corev1alpha1.AgentRuntimeClaude, &corev1alpha1.AgentRuntimeCeiling{AllowedTools: tools("Read")}),
			task:    ceilingTestTask(tools(" "), nil),
			wantErr: "unrestricted native tool set",
		},
		{
			name:  "deny list narrows a wider request",
			agent: ceilingTestAgent(corev1alpha1.AgentRuntimeClaude, &corev1alpha1.AgentRuntimeCeiling{AllowedTools: tools("Read", "Edit")}),
			task: func() *corev1alpha1.Task {
				task := ceilingTestTask(tools("Read", "Edit", "Write"), nil)
				task.Spec.AgentRuntime.DisallowedTools = tools("Write")
				return task
			}(),
		},
		{
			name:    "bash forbidden and granted",
			agent:   ceilingTestAgent(corev1alpha1.AgentRuntimeClaude, &corev1alpha1.AgentRuntimeCeiling{AllowBash: no}),
			task:    ceilingTestTask(tools("Read", "Bash"), yes),
			wantErr: "forbids Bash",
		},
		{
			// Dispatch matches deny entries exactly, so "bash" does not remove "Bash".
			name:  "differently cased deny entry does not narrow the request",
			agent: ceilingTestAgent(corev1alpha1.AgentRuntimeClaude, &corev1alpha1.AgentRuntimeCeiling{AllowedTools: tools(), AllowBash: no}),
			task: func() *corev1alpha1.Task {
				task := ceilingTestTask(tools("Bash"), yes)
				task.Spec.AgentRuntime.DisallowedTools = tools("bash")
				return task
			}(),
			wantErr: "forbids Bash",
		},
		{
			name:  "bash flag without a Bash tool grants nothing",
			agent: ceilingTestAgent(corev1alpha1.AgentRuntimeClaude, &corev1alpha1.AgentRuntimeCeiling{AllowBash: no}),
			task:  ceilingTestTask(tools("Read"), yes),
		},
		{
			name:    "bash defaults on when nothing narrows it",
			agent:   ceilingTestAgent(corev1alpha1.AgentRuntimeClaude, &corev1alpha1.AgentRuntimeCeiling{AllowBash: no}),
			task:    ceilingTestTask(nil, nil),
			wantErr: "forbids Bash",
		},
		{
			name:  "bash ceiling satisfied by allowBash false",
			agent: ceilingTestAgent(corev1alpha1.AgentRuntimeClaude, &corev1alpha1.AgentRuntimeCeiling{AllowBash: no}),
			task:  ceilingTestTask(nil, no),
		},
		{
			// Codex read sessions still run read commands through the shell.
			name:    "codex read-only session still has a shell",
			agent:   ceilingTestAgent(corev1alpha1.AgentRuntimeCodex, &corev1alpha1.AgentRuntimeCeiling{AllowBash: no}),
			task:    ceilingTestTask(tools("Glob", "Grep", "Read"), no),
			wantErr: "codex runtime runs shell commands",
		},
		{
			name:  "codex read-only session fits a tool-only ceiling",
			agent: ceilingTestAgent(corev1alpha1.AgentRuntimeCodex, &corev1alpha1.AgentRuntimeCeiling{AllowedTools: tools("Glob", "Grep", "Read")}),
			task:  ceilingTestTask(tools("Glob", "Grep", "Read"), no),
		},
		{
			name:  "bare ceiling entry covers a scoped request",
			agent: ceilingTestAgent(corev1alpha1.AgentRuntimeClaude, &corev1alpha1.AgentRuntimeCeiling{AllowedTools: tools("Read")}),
			task:  ceilingTestTask(tools("Read(/workspace/**)"), nil),
		},
		{
			name:    "scoped ceiling entry does not cover the bare tool",
			agent:   ceilingTestAgent(corev1alpha1.AgentRuntimeClaude, &corev1alpha1.AgentRuntimeCeiling{AllowedTools: tools("Read(/workspace/src/**)")}),
			task:    ceilingTestTask(tools("Read"), nil),
			wantErr: `does not allow tool "Read"`,
		},
		{
			name:  "tool names match without regard to case",
			agent: ceilingTestAgent(corev1alpha1.AgentRuntimeClaude, &corev1alpha1.AgentRuntimeCeiling{AllowedTools: tools("read")}),
			task:  ceilingTestTask(tools("Read(/workspace/**)"), nil),
		},
		{
			name: "scopes match exactly",
			agent: ceilingTestAgent(corev1alpha1.AgentRuntimeClaude, &corev1alpha1.AgentRuntimeCeiling{
				AllowedTools: tools("Bash(git branch -d topic)"),
			}),
			task:    ceilingTestTask(tools("Bash(git branch -D topic)"), yes),
			wantErr: `does not allow tool "Bash(git branch -D topic)"`,
		},
		{
			name: "identical scope is allowed",
			agent: ceilingTestAgent(corev1alpha1.AgentRuntimeClaude, &corev1alpha1.AgentRuntimeCeiling{
				AllowedTools: tools("bash(git branch -d topic)"),
			}),
			task: ceilingTestTask(tools("Bash(git branch -d topic)"), yes),
		},
		{
			name:    "explicitly empty ceiling permits no tools",
			agent:   ceilingTestAgent(corev1alpha1.AgentRuntimeClaude, &corev1alpha1.AgentRuntimeCeiling{AllowedTools: []string{}}),
			task:    ceilingTestTask(tools("Read"), nil),
			wantErr: `does not allow tool "Read"`,
		},
		{
			name:  "explicitly empty ceiling accepts a deny-all task",
			agent: ceilingTestAgent(corev1alpha1.AgentRuntimeClaude, &corev1alpha1.AgentRuntimeCeiling{AllowedTools: []string{}}),
			task:  ceilingTestTask([]string{}, nil),
		},
		{
			name:  "brokered tools count against the ceiling",
			agent: ceilingTestAgent(corev1alpha1.AgentRuntimeClaude, &corev1alpha1.AgentRuntimeCeiling{AllowedTools: tools("Read", "web_search")}),
			task:  ceilingTestTask(tools("Read", "web_search"), nil),
		},
		{
			name:    "brokered tool outside the ceiling",
			agent:   ceilingTestAgent(corev1alpha1.AgentRuntimeClaude, &corev1alpha1.AgentRuntimeCeiling{AllowedTools: tools("Read")}),
			task:    ceilingTestTask(tools("Read", "create_pull_request"), nil),
			wantErr: `does not allow tool "create_pull_request"`,
		},
		{
			name:  "injected coordination tools are not counted",
			agent: ceilingTestAgent(corev1alpha1.AgentRuntimeClaude, &corev1alpha1.AgentRuntimeCeiling{AllowedTools: tools("Read")}),
			task: func() *corev1alpha1.Task {
				task := ceilingTestTask(tools("Read"), nil)
				task.Labels = map[string]string{labels.LabelParentTask: "parent"}
				return task
			}(),
		},
		{
			name:  "requested coordination tools are counted",
			agent: ceilingTestAgent(corev1alpha1.AgentRuntimeClaude, &corev1alpha1.AgentRuntimeCeiling{AllowedTools: tools("Read")}),
			task: func() *corev1alpha1.Task {
				task := ceilingTestTask(tools("Read", "send_message"), nil)
				task.Labels = map[string]string{labels.LabelParentTask: "parent"}
				return task
			}(),
			wantErr: `does not allow tool "send_message"`,
		},
		{
			// Dispatch materializes the native list before injecting messaging
			// tools, so the exact-match deny entry "bash" leaves "Bash" granted.
			name:  "delegated child judged on the materialized native list",
			agent: ceilingTestAgent(corev1alpha1.AgentRuntimeClaude, &corev1alpha1.AgentRuntimeCeiling{AllowBash: no}),
			task: func() *corev1alpha1.Task {
				task := ceilingTestTask(nil, yes)
				task.Labels = map[string]string{labels.LabelParentTask: "parent"}
				task.Spec.AgentRuntime.DisallowedTools = tools("bash")
				return task
			}(),
			wantErr: "forbids Bash",
		},
		{
			name:  "padded names are trimmed before the deny list applies",
			agent: ceilingTestAgent(corev1alpha1.AgentRuntimeClaude, &corev1alpha1.AgentRuntimeCeiling{AllowedTools: tools("Read")}),
			task: func() *corev1alpha1.Task {
				task := ceilingTestTask(tools("Read", " Write "), nil)
				task.Spec.AgentRuntime.DisallowedTools = tools("Write")
				return task
			}(),
		},
		{
			name:  "padded Bash is removed by the Bash gate",
			agent: ceilingTestAgent(corev1alpha1.AgentRuntimeClaude, &corev1alpha1.AgentRuntimeCeiling{AllowedTools: tools("Read"), AllowBash: no}),
			task:  ceilingTestTask(tools("Read", " Bash "), no),
		},
		{
			name: "padded Bash in Agent defaults is removed by the Bash gate",
			agent: func() *corev1alpha1.Agent {
				agent := ceilingTestAgent(corev1alpha1.AgentRuntimeClaude, &corev1alpha1.AgentRuntimeCeiling{AllowedTools: tools("Read"), AllowBash: no})
				agent.Spec.Runtime.DefaultAllowedTools = tools("Read", " Bash ")
				agent.Spec.Runtime.DefaultAllowBash = no
				return agent
			}(),
			task: ceilingTestTask(nil, nil),
		},
		{
			name:  "read-only preset is judged after translation",
			agent: ceilingTestAgent(corev1alpha1.AgentRuntimeClaude, &corev1alpha1.AgentRuntimeCeiling{AllowedTools: tools("Glob", "Grep", "Read")}),
			task: func() *corev1alpha1.Task {
				task := ceilingTestTask(readOnlyAgentAllowedTools(), nil)
				task.Annotations = map[string]string{labels.AnnotationAgentReadOnly: scheduledRunLabelValue}
				return task
			}(),
		},
		{
			name: "opencode aliases are normalized on both sides",
			agent: func() *corev1alpha1.Agent {
				agent := ceilingTestAgent(corev1alpha1.AgentRuntimeOpencode, &corev1alpha1.AgentRuntimeCeiling{AllowedTools: tools("Read", "Edit")})
				agent.Spec.Runtime.DefaultAllowedTools = tools("Read", "Write")
				return agent
			}(),
			task: func() *corev1alpha1.Task {
				task := ceilingTestTask(nil, nil)
				task.Spec.Workspace = &corev1alpha1.WorkspaceConfig{Intent: corev1alpha1.WorkspaceIntentWrite}
				return task
			}(),
		},
		{
			name: "opencode tool above ceiling",
			agent: func() *corev1alpha1.Agent {
				agent := ceilingTestAgent(corev1alpha1.AgentRuntimeOpencode, &corev1alpha1.AgentRuntimeCeiling{AllowedTools: tools("Read")})
				agent.Spec.Runtime.DefaultAllowedTools = tools("Read", "Edit")
				return agent
			}(),
			task: func() *corev1alpha1.Task {
				task := ceilingTestTask(nil, nil)
				task.Spec.Workspace = &corev1alpha1.WorkspaceConfig{Intent: corev1alpha1.WorkspaceIntentWrite}
				return task
			}(),
			wantErr: "does not allow tool",
		},
		{
			name:  "task turns above ceiling",
			agent: ceilingTestAgent(corev1alpha1.AgentRuntimeClaude, &corev1alpha1.AgentRuntimeCeiling{MaxTurns: turns(20)}),
			task: func() *corev1alpha1.Task {
				task := ceilingTestTask(nil, nil)
				task.Spec.AgentRuntime.MaxTurns = turns(30)
				return task
			}(),
			wantErr: "at most 20 turns",
		},
		{
			name:  "task turns within ceiling",
			agent: ceilingTestAgent(corev1alpha1.AgentRuntimeClaude, &corev1alpha1.AgentRuntimeCeiling{MaxTurns: turns(20)}),
			task: func() *corev1alpha1.Task {
				task := ceilingTestTask(nil, nil)
				task.Spec.AgentRuntime.MaxTurns = turns(10)
				return task
			}(),
		},
		{
			name:    "built-in default turns above ceiling",
			agent:   ceilingTestAgent(corev1alpha1.AgentRuntimeClaude, &corev1alpha1.AgentRuntimeCeiling{MaxTurns: turns(20)}),
			task:    ceilingTestTask(nil, nil),
			wantErr: "resolves to 50",
		},
		{
			name: "external runtime allowlist above ceiling",
			agent: &corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{Name: "fibey", Namespace: "team-a"},
				Spec: corev1alpha1.AgentSpec{Runtime: &corev1alpha1.AgentCLIRuntime{
					RuntimeRef: &corev1alpha1.AgentRuntimeReference{Name: "fibey-runtime"},
					Ceiling:    &corev1alpha1.AgentRuntimeCeiling{AllowedTools: tools("read-inventory"), MaxTurns: turns(5)},
				}},
			},
			task:    ceilingTestTask(tools("create-work-order", "read-inventory"), nil),
			wantErr: `does not allow tool "create-work-order"`,
		},
		{
			name: "external runtime ignores the fixed turn budget",
			agent: &corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{Name: "fibey", Namespace: "team-a"},
				Spec: corev1alpha1.AgentSpec{Runtime: &corev1alpha1.AgentCLIRuntime{
					RuntimeRef: &corev1alpha1.AgentRuntimeReference{Name: "fibey-runtime"},
					Ceiling:    &corev1alpha1.AgentRuntimeCeiling{AllowedTools: tools("read-inventory"), MaxTurns: turns(5)},
				}},
			},
			task: ceilingTestTask(tools("read-inventory"), nil),
		},
		{
			name: "external runtime ignores the Bash ceiling",
			agent: &corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{Name: "fibey", Namespace: "team-a"},
				Spec: corev1alpha1.AgentSpec{Runtime: &corev1alpha1.AgentCLIRuntime{
					RuntimeRef: &corev1alpha1.AgentRuntimeReference{Name: "fibey-runtime"},
					Ceiling:    &corev1alpha1.AgentRuntimeCeiling{AllowedTools: tools("bash"), AllowBash: no},
				}},
			},
			task: ceilingTestTask(tools("bash"), nil),
		},
		{
			name: "external runtime Task without allowedTools is told to set them",
			agent: &corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{Name: "fibey", Namespace: "team-a"},
				Spec: corev1alpha1.AgentSpec{Runtime: &corev1alpha1.AgentCLIRuntime{
					RuntimeRef: &corev1alpha1.AgentRuntimeReference{Name: "fibey-runtime"},
					Ceiling:    &corev1alpha1.AgentRuntimeCeiling{AllowedTools: tools("read-inventory")},
				}},
			},
			task:    ceilingTestTask(nil, nil),
			wantErr: "the task does not set allowedTools; set allowedTools within the ceiling",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateAgentRuntimeCeiling(tt.task, tt.agent)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validateAgentRuntimeCeiling() error = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validateAgentRuntimeCeiling() error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidateAgentRuntimeCeilingAgentDefaultsOnly(t *testing.T) {
	no := new(bool)
	tight := &corev1alpha1.AgentRuntimeCeiling{AllowedTools: []string{"Read"}, AllowBash: no, MaxTurns: func() *int32 { n := int32(20); return &n }()}

	// Tasks may still supply their own policy, so an Agent without explicit
	// defaults is not judged against the ceiling on its own.
	if err := validateAgentRuntimeCeiling(nil, ceilingTestAgent(corev1alpha1.AgentRuntimeClaude, tight)); err != nil {
		t.Fatalf("agent without explicit defaults: error = %v, want nil", err)
	}

	withinTurns := int32(10)
	within := ceilingTestAgent(corev1alpha1.AgentRuntimeClaude, tight)
	within.Spec.Runtime.DefaultAllowedTools = []string{"Read"}
	within.Spec.Runtime.DefaultAllowBash = no
	within.Spec.Runtime.DefaultMaxTurns = &withinTurns
	if err := validateAgentRuntimeCeiling(nil, within); err != nil {
		t.Fatalf("agent defaults within ceiling: error = %v, want nil", err)
	}

	above := ceilingTestAgent(corev1alpha1.AgentRuntimeClaude, tight)
	above.Spec.Runtime.DefaultAllowedTools = []string{"Read", "Edit"}
	above.Spec.Runtime.DefaultAllowBash = no
	if err := validateAgentRuntimeCeiling(nil, above); err == nil || !strings.Contains(err.Error(), `"Edit"`) {
		t.Fatalf("agent defaults above ceiling: error = %v, want Edit rejection", err)
	}

	aboveTurns := int32(40)
	turnsAbove := ceilingTestAgent(corev1alpha1.AgentRuntimeClaude, tight)
	turnsAbove.Spec.Runtime.DefaultMaxTurns = &aboveTurns
	if err := validateAgentRuntimeCeiling(nil, turnsAbove); err == nil || !strings.Contains(err.Error(), "at most 20 turns") {
		t.Fatalf("agent default turns above ceiling: error = %v, want turn rejection", err)
	}

	external := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "fibey"},
		Spec: corev1alpha1.AgentSpec{Runtime: &corev1alpha1.AgentCLIRuntime{
			RuntimeRef: &corev1alpha1.AgentRuntimeReference{Name: "fibey-runtime"},
			Ceiling:    tight,
		}},
	}
	if err := validateAgentRuntimeCeiling(nil, external); err != nil {
		t.Fatalf("external runtime agent without defaults: error = %v, want nil", err)
	}
}

func TestValidateAgentRuntimeTaskCompatibilityEnforcesCeiling(t *testing.T) {
	agent := ceilingTestAgent(corev1alpha1.AgentRuntimeClaude, &corev1alpha1.AgentRuntimeCeiling{AllowedTools: []string{"Read"}})
	reconciler := &TaskReconciler{}
	if err := reconciler.validateAgentRuntimeTaskCompatibility(ceilingTestTask([]string{"Read"}, nil), agent); err != nil {
		t.Fatalf("task within ceiling: error = %v, want nil", err)
	}
	err := reconciler.validateAgentRuntimeTaskCompatibility(ceilingTestTask([]string{"Read", "Edit"}, nil), agent)
	if err == nil || !strings.Contains(err.Error(), `ceiling does not allow tool "Edit"`) {
		t.Fatalf("task above ceiling: error = %v, want ceiling rejection", err)
	}
}

func TestAgentRuntimeCeilingJSONPreservesExplicitEmptyTools(t *testing.T) {
	runtime := corev1alpha1.AgentCLIRuntime{
		Type:    corev1alpha1.AgentRuntimeClaude,
		Ceiling: &corev1alpha1.AgentRuntimeCeiling{AllowedTools: []string{}},
	}
	encoded, err := json.Marshal(runtime)
	if err != nil {
		t.Fatalf("marshal runtime: %v", err)
	}
	if !strings.Contains(string(encoded), `"ceiling":{"allowedTools":[]}`) {
		t.Fatalf("encoded runtime = %s, want explicit empty ceiling allowedTools", encoded)
	}
	var decoded corev1alpha1.AgentCLIRuntime
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal runtime: %v", err)
	}
	if decoded.Ceiling == nil || decoded.Ceiling.AllowedTools == nil || len(decoded.Ceiling.AllowedTools) != 0 {
		t.Fatalf("decoded ceiling = %#v, want explicit empty allowedTools", decoded.Ceiling)
	}

	unbounded, err := json.Marshal(corev1alpha1.AgentRuntimeCeiling{})
	if err != nil {
		t.Fatalf("marshal empty ceiling: %v", err)
	}
	if string(unbounded) != "{}" {
		t.Fatalf("encoded unbounded ceiling = %s, want {}", unbounded)
	}
}

func TestRuntimeRefCeilingJudgesRegisteredPolicy(t *testing.T) {
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "fibey", Namespace: "team-a"},
		Spec: corev1alpha1.AgentSpec{Runtime: &corev1alpha1.AgentCLIRuntime{
			RuntimeRef: &corev1alpha1.AgentRuntimeReference{Name: "fibey-runtime"},
			Ceiling:    &corev1alpha1.AgentRuntimeCeiling{AllowedTools: []string{"read-inventory"}},
		}},
	}
	registered := func(disallowed ...string) *corev1alpha1.AgentRuntimeMCPPolicySpec {
		return &corev1alpha1.AgentRuntimeMCPPolicySpec{
			AllowedTools:    []string{"read-inventory", "update-inventory"},
			DisallowedTools: append([]string{}, disallowed...),
		}
	}
	exact := ceilingTestTask([]string{"read-inventory", "update-inventory"}, nil)
	if err := validateAgentRuntimeCeilingWithRegisteredPolicy(exact, agent, registered("update-inventory")); err != nil {
		t.Fatalf("registration denies the tool outside the ceiling: error = %v, want nil", err)
	}
	// The registered deny list matches names exactly, as at dispatch.
	err := validateAgentRuntimeCeilingWithRegisteredPolicy(exact, agent, registered("Update-Inventory"))
	if err == nil || !strings.Contains(err.Error(), `ceiling does not allow tool "update-inventory"`) {
		t.Fatalf("differently cased registered deny: error = %v, want ceiling rejection", err)
	}
	// An allowlist that does not match the registration is judged as requested.
	mismatch := ceilingTestTask([]string{"update-inventory"}, nil)
	err = validateAgentRuntimeCeilingWithRegisteredPolicy(mismatch, agent, registered("update-inventory"))
	if err == nil || !strings.Contains(err.Error(), `ceiling does not allow tool "update-inventory"`) {
		t.Fatalf("allowlist not matching the registration: error = %v, want ceiling rejection", err)
	}
	if err := validateAgentRuntimeCeilingWithRegisteredPolicy(exact, agent, registered()); err == nil {
		t.Fatal("registration exposing a tool outside the ceiling: error = nil, want ceiling rejection")
	}
}

// TestCeilingCoversForwardingWriteTools covers a registered external runtime
// that lists a forwarding Tool's write tool by name: the ceiling entry for
// the Tool covers it, and no other entry does.
func TestCeilingCoversForwardingWriteTools(t *testing.T) {
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "fibey", Namespace: "team-a"},
		Spec: corev1alpha1.AgentSpec{Runtime: &corev1alpha1.AgentCLIRuntime{
			RuntimeRef: &corev1alpha1.AgentRuntimeReference{Name: "fibey-runtime"},
			Ceiling:    &corev1alpha1.AgentRuntimeCeiling{AllowedTools: []string{"azure-arm"}},
		}},
	}
	registered := &corev1alpha1.AgentRuntimeMCPPolicySpec{AllowedTools: []string{"azure-arm", "azure-arm:write"}}
	task := ceilingTestTask([]string{"azure-arm", "azure-arm:write"}, nil)
	if err := validateAgentRuntimeCeilingWithRegisteredPolicy(task, agent, registered); err != nil {
		t.Fatalf("a ceiling naming the Tool refused its write tool: %v", err)
	}
	agent.Spec.Runtime.Ceiling.AllowedTools = []string{"other"}
	if err := validateAgentRuntimeCeilingWithRegisteredPolicy(task, agent, registered); err == nil ||
		!strings.Contains(err.Error(), "ceiling does not allow tool") {
		t.Fatalf("a ceiling without the Tool: %v", err)
	}
	if !ceilingAllowsTool([]string{"Azure-Arm"}, "azure-arm:write") {
		t.Fatal("write tool coverage follows the Tool's ceiling entry")
	}
	for _, name := range []string{":write", "azure-arm:write:write", "Azure-Arm:write", "azure-arm(x):write"} {
		if ceilingAllowsTool([]string{"azure-arm", "Azure-Arm", "azure-arm(x)", "azure-arm:write"}, name) {
			t.Errorf("%q is no Tool's write tool, but the ceiling covered it", name)
		}
	}
}
