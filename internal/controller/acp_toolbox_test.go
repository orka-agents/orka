package controller

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/executionmode"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

const acpTestToolboxDigest = "sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func acpTestToolboxPolicy() ACPToolboxPolicy {
	return ACPToolboxPolicy{
		Enabled:           true,
		AllowedRegistries: []string{"registry.example.com/tools", "ghcr.io/orka-agents"},
		MountMethod:       ACPToolboxMountCopy,
	}
}

func acpTestToolboxAgent(toolboxes ...corev1alpha1.AgentToolbox) *corev1alpha1.Agent {
	maxTurns := int32(20)
	return &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{UID: types.UID("agent-uid"), Generation: 3},
		Spec: corev1alpha1.AgentSpec{
			Model: &corev1alpha1.ModelConfig{Name: acpTestModel},
			Runtime: &corev1alpha1.AgentCLIRuntime{
				Type: corev1alpha1.AgentRuntimeCodex, DefaultMaxTurns: &maxTurns,
				ContractVersion: new(corev1alpha1.AgentRuntimeContractHarnessV2),
				Toolboxes:       toolboxes,
			},
		},
	}
}

func acpTestToolbox(mountPath string, entries ...string) corev1alpha1.AgentToolbox {
	repository := strings.TrimPrefix(mountPath, "/opt/")
	if mountPath == harnessv2.RuntimeToolboxHomebrewMountPath {
		repository = "brew"
	}
	return corev1alpha1.AgentToolbox{
		Image:     "registry.example.com/tools/" + repository + "@" + acpTestToolboxDigest,
		MountPath: mountPath, PathEntries: entries,
	}
}

func TestToolboxImageAllowedMatchesWholeSegmentsOnly(t *testing.T) {
	allowed := []string{"registry.example.com", "ghcr.io/orka-agents/tools", " localhost:5001/ "}
	accept := []string{
		"registry.example.com/yq@" + acpTestToolboxDigest,
		"registry.example.com/a/b/c@" + acpTestToolboxDigest,
		"ghcr.io/orka-agents/tools/yq@" + acpTestToolboxDigest,
		"ghcr.io/orka-agents/tools@" + acpTestToolboxDigest,
		"localhost:5001/tools/yq@" + acpTestToolboxDigest,
	}
	for _, image := range accept {
		if !ToolboxImageAllowed(image, allowed) {
			t.Errorf("%s should be allowed", image)
		}
	}
	reject := []string{
		"registry.example.com.evil.io/yq@" + acpTestToolboxDigest,
		"registry.example.com:5000/yq@" + acpTestToolboxDigest,
		"evil.io/registry.example.com/yq@" + acpTestToolboxDigest,
		"ghcr.io/orka-agents/tools-evil/yq@" + acpTestToolboxDigest,
		"ghcr.io/orka-agents/yq@" + acpTestToolboxDigest,
		"ghcr.io/orka-agents@" + acpTestToolboxDigest,
		"localhost/tools/yq@" + acpTestToolboxDigest,
		"registry.example.com/yq",
		"",
	}
	for _, image := range reject {
		if ToolboxImageAllowed(image, allowed) {
			t.Errorf("%s should be rejected", image)
		}
	}
	if ToolboxImageAllowed("registry.example.com/yq@"+acpTestToolboxDigest, nil) {
		t.Error("an empty allowlist must allow nothing")
	}
	if ToolboxImageAllowed("registry.example.com/yq@"+acpTestToolboxDigest, []string{"", " "}) {
		t.Error("blank allowlist entries must allow nothing")
	}
}

func TestToolboxPolicyValidateAgentToolboxes(t *testing.T) {
	policy := acpTestToolboxPolicy()
	if err := policy.ValidateAgentToolboxes(acpTestToolboxAgent()); err != nil {
		t.Fatalf("agent without toolboxes: %v", err)
	}
	if err := policy.ValidateAgentToolboxes(acpTestToolboxAgent(acpTestToolbox("/opt/yq", "bin"), acpTestToolbox(harnessv2.RuntimeToolboxHomebrewMountPath, "bin"))); err != nil {
		t.Fatalf("valid toolboxes: %v", err)
	}
	cases := map[string]struct {
		policy ACPToolboxPolicy
		agent  *corev1alpha1.Agent
		want   string
	}{
		"disabled": {
			policy: ACPToolboxPolicy{}, agent: acpTestToolboxAgent(acpTestToolbox("/opt/yq", "bin")),
			want: "disabled on this controller",
		},
		"mounting unavailable": {
			policy: ACPToolboxPolicy{Enabled: true, AllowedRegistries: []string{"registry.example.com"}, UnavailableReason: "image volumes need Kubernetes 1.36 or newer"},
			agent:  acpTestToolboxAgent(acpTestToolbox("/opt/yq", "bin")),
			want:   "ToolboxUnavailable: image volumes need Kubernetes 1.36",
		},
		"registry not allowed": {
			policy: policy, agent: acpTestToolboxAgent(corev1alpha1.AgentToolbox{Image: "registry.example.com/tools-evil/yq@" + acpTestToolboxDigest, MountPath: "/opt/yq"}),
			want: "not from an allowed toolbox registry",
		},
		"not digest pinned": {
			policy: policy, agent: acpTestToolboxAgent(corev1alpha1.AgentToolbox{Image: "registry.example.com/tools/yq:1", MountPath: "/opt/yq"}),
			want: "pinned by digest",
		},
		"reserved mount path": {
			policy: policy, agent: acpTestToolboxAgent(corev1alpha1.AgentToolbox{Image: "registry.example.com/tools/yq@" + acpTestToolboxDigest, MountPath: "/opt/codex"}),
			want: "reserved",
		},
		"unsafe mount path": {
			policy: policy, agent: acpTestToolboxAgent(corev1alpha1.AgentToolbox{Image: "registry.example.com/tools/yq@" + acpTestToolboxDigest, MountPath: "/opt/../etc"}),
			want: "mountPath",
		},
		"overlap": {
			policy: policy, agent: acpTestToolboxAgent(acpTestToolbox("/opt/yq", "bin"), acpTestToolbox("/opt/yq", "sbin")),
			want: "overlaps",
		},
		"unsafe path entry": {
			policy: policy, agent: acpTestToolboxAgent(acpTestToolbox("/opt/yq", "../bin")),
			want: "pathEntry",
		},
		"colon in path entry": {
			policy: policy, agent: acpTestToolboxAgent(acpTestToolbox("/opt/yq", "bi:n")),
			want: "PATH",
		},
		"too many toolboxes": {
			policy: policy, agent: acpTestToolboxAgent(acpTestToolbox("/opt/a"), acpTestToolbox("/opt/b"), acpTestToolbox("/opt/c"), acpTestToolbox("/opt/d"), acpTestToolbox("/opt/e")),
			want: "at most 4",
		},
		"too many path entries": {
			policy: policy, agent: acpTestToolboxAgent(acpTestToolbox("/opt/yq", "a", "b", "c", "d", "e", "f", "g", "h", "i")),
			want: "at most 8",
		},
		"runtimeRef": {
			policy: policy, agent: func() *corev1alpha1.Agent {
				agent := acpTestToolboxAgent(acpTestToolbox("/opt/yq", "bin"))
				agent.Spec.Runtime.Type = ""
				agent.Spec.Runtime.ContractVersion = nil
				agent.Spec.Runtime.RuntimeRef = &corev1alpha1.AgentRuntimeReference{Name: "external"}
				return agent
			}(),
			want: "external runtimeRef",
		},
		"harness v1": {
			policy: policy, agent: func() *corev1alpha1.Agent {
				agent := acpTestToolboxAgent(acpTestToolbox("/opt/yq", "bin"))
				agent.Spec.Runtime.ContractVersion = new(corev1alpha1.AgentRuntimeContractHarnessV1)
				return agent
			}(),
			want: "harness v1",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := tc.policy.ValidateAgentToolboxes(tc.agent)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestAgentReconcilerValidateAgentRejectsToolboxesUnderPolicy(t *testing.T) {
	reconciler := &AgentReconciler{Mode: executionmode.HarnessV2}
	agent := acpTestToolboxAgent(acpTestToolbox("/opt/yq", "bin"))
	agent.Spec.Runtime.ContractVersion = nil
	if err := reconciler.validateAgent(t.Context(), agent); err == nil || !strings.Contains(err.Error(), "disabled on this controller") {
		t.Fatalf("admission with toolboxes disabled: %v", err)
	}
	reconciler.ToolboxPolicy = acpTestToolboxPolicy()
	if err := reconciler.validateAgent(t.Context(), agent); err != nil {
		t.Fatalf("admission with toolboxes enabled: %v", err)
	}
	v1 := &AgentReconciler{Mode: executionmode.HarnessV1, ToolboxPolicy: acpTestToolboxPolicy()}
	if err := v1.validateAgent(t.Context(), agent); err == nil || !strings.Contains(err.Error(), "harness v1") {
		t.Fatalf("harness v1 installation must reject toolboxes: %v", err)
	}
}

func TestPlanAgentExecutionRejectsToolboxCombinations(t *testing.T) {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "task", UID: types.UID("task-uid"), Generation: 1},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
	}
	reconciler := &TaskReconciler{Mode: executionmode.HarnessV2, ACPRuntimeEnabled: true, ACPToolboxPolicy: acpTestToolboxPolicy()}

	plan := reconciler.planAgentExecution(t.Context(), task, acpTestToolboxAgent(acpTestToolbox("/opt/yq", "bin")))
	if plan.path != agentExecutionPathACP {
		t.Fatalf("built-in v2 agent with toolboxes must plan the ACP path, got %#v", plan)
	}

	workspaceTask := task.DeepCopy()
	workspaceTask.Spec.Execution = &corev1alpha1.ExecutionSpec{Workspace: &corev1alpha1.ExecutionWorkspaceSpec{Enabled: true}}
	plan = reconciler.planAgentExecution(t.Context(), workspaceTask, acpTestToolboxAgent(acpTestToolbox("/opt/yq", "bin")))
	if plan.path != agentExecutionPathRejected || !strings.Contains(plan.rejectionReason, "execution.workspace") {
		t.Fatalf("execution workspace with toolboxes must be rejected, got %#v", plan)
	}

	v1Agent := acpTestToolboxAgent(acpTestToolbox("/opt/yq", "bin"))
	v1Agent.Spec.Runtime.ContractVersion = new(corev1alpha1.AgentRuntimeContractHarnessV1)
	v1Reconciler := &TaskReconciler{Mode: executionmode.HarnessV1, HarnessV1Enabled: true, ACPToolboxPolicy: acpTestToolboxPolicy()}
	plan = v1Reconciler.planAgentExecution(t.Context(), task, v1Agent)
	if plan.path != agentExecutionPathRejected || !strings.Contains(plan.rejectionReason, "harness v1") {
		t.Fatalf("harness v1 with toolboxes must be rejected, got %#v", plan)
	}

	externalAgent := acpTestToolboxAgent(acpTestToolbox("/opt/yq", "bin"))
	externalAgent.Spec.Runtime.Type = ""
	externalAgent.Spec.Runtime.ContractVersion = nil
	externalAgent.Spec.Runtime.RuntimeRef = &corev1alpha1.AgentRuntimeReference{Name: "external"}
	plan = reconciler.planAgentExecution(t.Context(), task, externalAgent)
	if plan.path != agentExecutionPathRejected || !strings.Contains(plan.rejectionReason, "external runtimeRef") {
		t.Fatalf("runtimeRef with toolboxes must be rejected before the AgentRuntime lookup, got %#v", plan)
	}

	disabled := &TaskReconciler{Mode: executionmode.HarnessV2, ACPRuntimeEnabled: true}
	plan = disabled.planAgentExecution(t.Context(), task, acpTestToolboxAgent(acpTestToolbox("/opt/yq", "bin")))
	if plan.path != agentExecutionPathRejected || !strings.Contains(plan.rejectionReason, "disabled on this controller") {
		t.Fatalf("toolboxes disabled must be rejected at planning, got %#v", plan)
	}
}

func TestPlanACPRuntimeCarriesToolboxesAndRotatesDigest(t *testing.T) {
	task := &corev1alpha1.Task{Spec: corev1alpha1.TaskSpec{
		Type: corev1alpha1.TaskTypeAgent, Workspace: &corev1alpha1.WorkspaceConfig{Intent: corev1alpha1.WorkspaceIntentRead},
	}}
	images := ACPRuntimeImages{Codex: "docker.io/example/codex@sha256:" + strings.Repeat("a", 64)}
	baseline, err := PlanACPRuntime(task, acpTestToolboxAgent(), images)
	if err != nil {
		t.Fatal(err)
	}
	if baseline.Profile.Toolboxes != nil {
		t.Fatalf("agent without toolboxes produced %#v", baseline.Profile.Toolboxes)
	}
	plan := func(toolboxes ...corev1alpha1.AgentToolbox) ACPRuntimePlan {
		t.Helper()
		planned, err := PlanACPRuntime(task, acpTestToolboxAgent(toolboxes...), images)
		if err != nil {
			t.Fatal(err)
		}
		return planned
	}
	first := plan(acpTestToolbox("/opt/yq", "bin", "sbin"), acpTestToolbox("/opt/jq", "bin"))
	if len(first.Profile.Toolboxes) != 2 || first.Profile.Toolboxes[0].MountPath != "/opt/yq" ||
		first.Profile.Toolboxes[0].PathEntries[1] != "sbin" || first.Profile.Toolboxes[1].MountPath != "/opt/jq" {
		t.Fatalf("plan lost toolbox order or content: %#v", first.Profile.Toolboxes)
	}
	if first.Digest == baseline.Digest || first.PoolName == baseline.PoolName {
		t.Fatal("toolboxes must rotate the profile digest and pool name")
	}
	if first.Profile.AgentConfigurationDigest != baseline.Profile.AgentConfigurationDigest {
		t.Fatal("toolboxes must not change the agent configuration digest")
	}
	// Hold the Agent, configuration, model, and image constant: only one
	// toolbox field changes per case.
	variants := map[string]ACPRuntimePlan{
		"image": plan(corev1alpha1.AgentToolbox{
			Image: "registry.example.com/tools/yq@sha256:" + strings.Repeat("f", 64), MountPath: "/opt/yq", PathEntries: []string{"bin", "sbin"},
		}, acpTestToolbox("/opt/jq", "bin")),
		"mountPath":     plan(acpTestToolbox("/opt/yq2", "bin", "sbin"), acpTestToolbox("/opt/jq", "bin")),
		"pathEntries":   plan(acpTestToolbox("/opt/yq", "bin"), acpTestToolbox("/opt/jq", "bin")),
		"entry order":   plan(acpTestToolbox("/opt/yq", "sbin", "bin"), acpTestToolbox("/opt/jq", "bin")),
		"toolbox order": plan(acpTestToolbox("/opt/jq", "bin"), acpTestToolbox("/opt/yq", "bin", "sbin")),
	}
	seen := map[harnessv2.ProfileDigest]string{first.Digest: "base"}
	for name, variant := range variants {
		if previous, duplicate := seen[variant.Digest]; duplicate {
			t.Errorf("%s produced the same digest as %s", name, previous)
		}
		seen[variant.Digest] = name
	}
	repeat := plan(acpTestToolbox("/opt/yq", "bin", "sbin"), acpTestToolbox("/opt/jq", "bin"))
	if repeat.Digest != first.Digest || repeat.PoolName != first.PoolName {
		t.Fatal("planning with toolboxes is not deterministic")
	}
	if _, err := PlanACPRuntime(task, acpTestToolboxAgent(acpTestToolbox("/opt/codex", "bin")), images); err == nil {
		t.Fatal("planning must reject a reserved mount path even without the policy gate")
	}
}

func TestToolboxProfileRoundTripsThroughEveryConversion(t *testing.T) {
	task := &corev1alpha1.Task{Spec: corev1alpha1.TaskSpec{
		Type: corev1alpha1.TaskTypeAgent, Workspace: &corev1alpha1.WorkspaceConfig{Intent: corev1alpha1.WorkspaceIntentRead},
	}}
	images := ACPRuntimeImages{Codex: "docker.io/example/codex@sha256:" + strings.Repeat("a", 64)}
	plan, err := PlanACPRuntime(task, acpTestToolboxAgent(acpTestToolbox("/opt/yq", "bin", "sbin"), acpTestToolbox(harnessv2.RuntimeToolboxHomebrewMountPath, "bin")), images)
	if err != nil {
		t.Fatal(err)
	}
	spec := RuntimePoolProfileFromPlan(plan)
	if len(spec.Toolboxes) != 2 || spec.Toolboxes[0].PathEntries[1] != "sbin" {
		t.Fatalf("RuntimePool projection lost toolboxes: %#v", spec.Toolboxes)
	}
	fromPool, err := runtimePoolHarnessProfile(spec)
	if err != nil {
		t.Fatal(err)
	}
	fromDispatcher := runtimeProfileFromPool(spec)
	pool := runtimePoolTestObject(1)
	pool.Spec.Runtime.Profile = spec
	r := runtimePoolTestReconciler(t, runtimePoolTestScheme(t), nil, pool)
	r.ToolboxPolicy = acpTestToolboxPolicy()
	cfg, err := r.runtimePoolConfig(pool)
	if err != nil {
		t.Fatal(err)
	}
	selector := map[string]string{runtimePoolKeyLabel: cfg.labels[runtimePoolKeyLabel]}
	template := r.runtimePoolPodTemplate(pool, cfg, selector, "auth", "provider")
	_, deployedCfg, err := runtimePoolValidationTargetFromTemplate(pool, template)
	if err != nil {
		t.Fatal(err)
	}
	environment := runtimePoolLiteralEnvironment(template.Spec.Containers[0].Env)
	fromEnvironment, err := runtimePoolToolboxesFromEnvironment(environment)
	if err != nil {
		t.Fatal(err)
	}
	supervisorProfile := plan.Profile
	supervisorProfile.Toolboxes = fromEnvironment

	expected := plan.Digest
	for name, profile := range map[string]harnessv2.RuntimeProfile{
		"plan": plan.Profile, "pool": fromPool, "dispatcher": fromDispatcher, "template": deployedCfg.profile, "supervisor-env": supervisorProfile,
	} {
		digest, err := harnessv2.CanonicalProfileDigest(profile)
		if err != nil {
			t.Fatal(err)
		}
		if digest != expected {
			t.Errorf("%s profile digest %s != plan digest %s (%#v)", name, digest, expected, profile.Toolboxes)
		}
	}
	if environment[runtimePoolToolboxMountMethod] != string(ACPToolboxMountCopy) {
		t.Fatalf("mount method env = %q", environment[runtimePoolToolboxMountMethod])
	}
}

func TestACPToolboxPromptNote(t *testing.T) {
	toolboxes := []harnessv2.RuntimeToolbox{
		{Image: "registry.example.com/tools/yq@" + acpTestToolboxDigest, MountPath: "/opt/yq-jq", PathEntries: []string{"bin"}},
		{Image: "registry.example.com/tools/brew@" + acpTestToolboxDigest, MountPath: harnessv2.RuntimeToolboxHomebrewMountPath, PathEntries: []string{"bin"}},
		{Image: "registry.example.com/tools/none@" + acpTestToolboxDigest, MountPath: "/opt/none"},
	}
	want := "Extra command-line tools are installed for this task:\n- /opt/yq-jq/bin\n- /home/linuxbrew/.linuxbrew/bin\n- /opt/none (mounted, not on PATH)\n" +
		"These folders are on PATH. Run `ls <folder>` to see what is available. There is no internet access, so you can't install new packages."
	if got := acpToolboxPromptNote(toolboxes, true); got != want {
		t.Fatalf("note = %q, want %q", got, want)
	}
	if acpToolboxPromptNote(toolboxes, false) != "" {
		t.Fatal("a reused resident session must not repeat the note")
	}
	if acpToolboxPromptNote(nil, true) != "" {
		t.Fatal("no toolboxes must produce no note")
	}
	many := make([]harnessv2.RuntimeToolbox, 0, 4)
	for i := range 4 {
		entries := make([]string, 0, 8)
		for j := range 8 {
			entries = append(entries, strings.Repeat(string(rune('a'+j)), 250)+string(rune('0'+i)))
		}
		many = append(many, harnessv2.RuntimeToolbox{
			Image: "registry.example.com/tools/x@" + acpTestToolboxDigest, MountPath: "/opt/" + strings.Repeat("m", 120) + string(rune('0'+i)), PathEntries: entries,
		})
	}
	if note := acpToolboxPromptNote(many, true); len(note) > acpToolboxPromptNoteMaxBytes || !strings.HasSuffix(note, acpToolboxPromptNoteFooter) {
		t.Fatalf("oversized note (%d bytes) was not bounded", len(note))
	}
	content := acpPromptInputContentWithToolboxes("", acpToolboxPromptNote(toolboxes, true), "user prompt")
	if len(content) != 2 || content[0].Text != want || content[1].Text != "user prompt" {
		t.Fatalf("content = %#v", content)
	}
	content = acpPromptInputContentWithToolboxes("bootstrap", acpToolboxPromptNote(toolboxes, true), "user prompt")
	if len(content) != 3 || content[0].Text != "bootstrap" || content[1].Text != want {
		t.Fatalf("content with bootstrap = %#v", content)
	}
	if plain := acpPromptInputContent("", "user prompt"); len(plain) != 1 {
		t.Fatalf("plain content = %#v", plain)
	}
}

func TestRuntimePoolToolboxUnavailableMessage(t *testing.T) {
	pool := runtimePoolTestObject(1)
	if _, ok := runtimePoolToolboxUnavailableMessage(pool); ok {
		t.Fatal("pool without conditions must not report toolbox unavailability")
	}
	pool.Status.Conditions = []metav1.Condition{{
		Type: corev1alpha1.RuntimePoolConditionRolloutReady, Status: metav1.ConditionFalse,
		Reason: corev1alpha1.RuntimePoolReasonToolboxUnavailable, Message: "ToolboxUnavailable: TOOLBOX_ARCH_MISMATCH: toolbox-copy-0: wrong arch",
		ObservedGeneration: pool.Generation,
	}}
	message, ok := runtimePoolToolboxUnavailableMessage(pool)
	if !ok || !strings.Contains(message, "TOOLBOX_ARCH_MISMATCH") {
		t.Fatalf("message = %q ok=%v", message, ok)
	}
	pool.Status.Conditions[0].ObservedGeneration = pool.Generation + 1
	if _, ok := runtimePoolToolboxUnavailableMessage(pool); ok {
		t.Fatal("a stale-generation condition must not fail the Task")
	}
	pool.Status.Conditions[0].ObservedGeneration = pool.Generation
	pool.Status.Conditions[0].Reason = corev1alpha1.RuntimePoolReasonRolloutFailed
	if _, ok := runtimePoolToolboxUnavailableMessage(pool); ok {
		t.Fatal("other rollout failures must keep waiting")
	}
}
