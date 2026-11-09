package controller

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	acpskill "github.com/orka-agents/orka/internal/acp/skill"
	"github.com/orka-agents/orka/internal/acp/toolbox"
	"github.com/orka-agents/orka/internal/executionmode"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

func acpTestSkillPolicy() ACPToolboxPolicy {
	policy := acpTestToolboxPolicy()
	policy.SkillsEnabled = true
	return policy
}

func acpTestSkill(name string) corev1alpha1.AgentRuntimeSkill {
	return corev1alpha1.AgentRuntimeSkill{Name: name, Image: "registry.example.com/tools/skills/" + name + "@" + acpTestToolboxDigest}
}

func acpTestSkillAgent(skills ...corev1alpha1.AgentRuntimeSkill) *corev1alpha1.Agent {
	agent := acpTestToolboxAgent()
	agent.Spec.Runtime.Skills = skills
	return agent
}

func TestToolboxPolicyValidateAgentSkills(t *testing.T) {
	policy := acpTestSkillPolicy()
	if err := policy.ValidateAgentSkills(acpTestSkillAgent()); err != nil {
		t.Fatalf("agent without skills: %v", err)
	}
	if err := (ACPToolboxPolicy{}).ValidateAgentSkills(acpTestSkillAgent()); err != nil {
		t.Fatalf("agent without skills must pass a disabled policy: %v", err)
	}
	for _, runtimeType := range []corev1alpha1.AgentRuntimeType{
		corev1alpha1.AgentRuntimeCodex, corev1alpha1.AgentRuntimeClaude, corev1alpha1.AgentRuntimeOpencode,
	} {
		agent := acpTestSkillAgent(acpTestSkill("code-review"), acpTestSkill("release-notes"))
		agent.Spec.Runtime.Type = runtimeType
		if err := policy.ValidateAgentSkills(agent); err != nil {
			t.Fatalf("%s skills: %v", runtimeType, err)
		}
	}
	withPath := acpTestSkill("code-review")
	withPath.Path = "/skills/bundle/code-review"
	if err := policy.ValidateAgentSkills(acpTestSkillAgent(withPath)); err != nil {
		t.Fatalf("explicit path: %v", err)
	}

	toolboxesOnly := acpTestToolboxPolicy()
	cases := map[string]struct {
		policy ACPToolboxPolicy
		agent  func() *corev1alpha1.Agent
		want   string
	}{
		"feature off":     {policy: ACPToolboxPolicy{}, agent: func() *corev1alpha1.Agent { return acpTestSkillAgent(acpTestSkill("a")) }, want: "disabled on this controller"},
		"skills gate off": {policy: toolboxesOnly, agent: func() *corev1alpha1.Agent { return acpTestSkillAgent(acpTestSkill("a")) }, want: "--acp-skills-enabled"},
		"toolboxes off":   {policy: ACPToolboxPolicy{SkillsEnabled: true, AllowedRegistries: []string{"registry.example.com"}}, agent: func() *corev1alpha1.Agent { return acpTestSkillAgent(acpTestSkill("a")) }, want: "disabled on this controller"},
		"delivery unavailable": {policy: func() ACPToolboxPolicy {
			p := acpTestSkillPolicy()
			p.UnavailableReason = "image volumes need Kubernetes 1.36"
			return p
		}(), agent: func() *corev1alpha1.Agent { return acpTestSkillAgent(acpTestSkill("a")) }, want: "1.36"},
		"registry not allowed": {policy: acpTestSkillPolicy(), agent: func() *corev1alpha1.Agent {
			return acpTestSkillAgent(corev1alpha1.AgentRuntimeSkill{Name: "a", Image: "evil.example.com/a@" + acpTestToolboxDigest})
		}, want: "not from an allowed toolbox registry"},
		"registry host suffix": {policy: acpTestSkillPolicy(), agent: func() *corev1alpha1.Agent {
			return acpTestSkillAgent(corev1alpha1.AgentRuntimeSkill{Name: "a", Image: "registry.example.com.evil.io/tools/a@" + acpTestToolboxDigest})
		}, want: "not from an allowed toolbox registry"},
		"tag instead of digest": {policy: acpTestSkillPolicy(), agent: func() *corev1alpha1.Agent {
			return acpTestSkillAgent(corev1alpha1.AgentRuntimeSkill{Name: "a", Image: "registry.example.com/tools/a:1"})
		}, want: "pinned by digest"},
		"path traversal": {policy: acpTestSkillPolicy(), agent: func() *corev1alpha1.Agent {
			s := acpTestSkill("a")
			s.Path = "/skills/../proc"
			return acpTestSkillAgent(s)
		}, want: "skill path"},
		"path outside /skills": {policy: acpTestSkillPolicy(), agent: func() *corev1alpha1.Agent {
			s := acpTestSkill("a")
			s.Path = "/var/run/secrets"
			return acpTestSkillAgent(s)
		}, want: "skill path"},
		"duplicate names": {policy: acpTestSkillPolicy(), agent: func() *corev1alpha1.Agent { return acpTestSkillAgent(acpTestSkill("a"), acpTestSkill("a")) }, want: "repeats the name"},
		"invalid name":    {policy: acpTestSkillPolicy(), agent: func() *corev1alpha1.Agent { return acpTestSkillAgent(acpTestSkill("Code_Review")) }, want: "skill name"},
		"too many": {policy: acpTestSkillPolicy(), agent: func() *corev1alpha1.Agent {
			skills := make([]corev1alpha1.AgentRuntimeSkill, 0, harnessv2.MaxRuntimeSkills+1)
			for i := range harnessv2.MaxRuntimeSkills + 1 {
				skills = append(skills, acpTestSkill("s"+string(rune('a'+i))))
			}
			return acpTestSkillAgent(skills...)
		}, want: "at most"},
		"external runtime": {policy: acpTestSkillPolicy(), agent: func() *corev1alpha1.Agent {
			agent := acpTestSkillAgent(acpTestSkill("a"))
			agent.Spec.Runtime.Type = ""
			agent.Spec.Runtime.ContractVersion = nil
			agent.Spec.Runtime.RuntimeRef = &corev1alpha1.AgentRuntimeReference{Name: "external"}
			return agent
		}, want: "external runtimeRef"},
		"copilot fails closed": {policy: acpTestSkillPolicy(), agent: func() *corev1alpha1.Agent {
			agent := acpTestSkillAgent(acpTestSkill("a"))
			agent.Spec.Runtime.Type = corev1alpha1.AgentRuntimeCopilot
			return agent
		}, want: "cannot limit skill discovery to Orka-provided skills"},
		"unsupported runtime": {policy: acpTestSkillPolicy(), agent: func() *corev1alpha1.Agent {
			agent := acpTestSkillAgent(acpTestSkill("a"))
			agent.Spec.Runtime.Type = corev1alpha1.AgentRuntimeType("aider")
			return agent
		}, want: "not supported by runtime \"aider\""},
		"v1 contract": {policy: acpTestSkillPolicy(), agent: func() *corev1alpha1.Agent {
			agent := acpTestSkillAgent(acpTestSkill("a"))
			agent.Spec.Runtime.ContractVersion = new(corev1alpha1.AgentRuntimeContractVersion("orka.harness.v1"))
			return agent
		}, want: "orka.harness.v2"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := tc.policy.ValidateAgentSkills(tc.agent())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestAgentReconcilerValidateAgentRejectsSkillsUnderPolicy(t *testing.T) {
	reconciler := &AgentReconciler{Mode: executionmode.HarnessV2}
	agent := acpTestSkillAgent(acpTestSkill("code-review"))
	agent.Spec.Runtime.ContractVersion = nil
	if err := reconciler.validateAgent(t.Context(), agent); err == nil || !strings.Contains(err.Error(), "runtime.skills are disabled") {
		t.Fatalf("admission with skills disabled: %v", err)
	}
	reconciler.ToolboxPolicy = acpTestSkillPolicy()
	if err := reconciler.validateAgent(t.Context(), agent); err != nil {
		t.Fatalf("admission with skills enabled: %v", err)
	}
}

func TestPlanAgentExecutionRejectsSkillCombinations(t *testing.T) {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "task", UID: types.UID("task-uid"), Generation: 1},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
	}
	reconciler := &TaskReconciler{Mode: executionmode.HarnessV2, ACPToolboxPolicy: acpTestSkillPolicy()}
	if plan := reconciler.planAgentExecution(t.Context(), task, acpTestSkillAgent(acpTestSkill("a"))); plan.path != agentExecutionPathACP {
		t.Fatalf("built-in v2 agent with skills must plan the ACP path, got %#v", plan)
	}
	workspaceTask := task.DeepCopy()
	workspaceTask.Spec.Execution = &corev1alpha1.ExecutionSpec{Workspace: &corev1alpha1.ExecutionWorkspaceSpec{ClassRef: &corev1alpha1.WorkspaceClassReference{Name: "acp-class"}}}
	if plan := reconciler.planAgentExecution(t.Context(), workspaceTask, acpTestSkillAgent(acpTestSkill("a"))); plan.path != agentExecutionPathRejected ||
		!strings.Contains(plan.rejectionReason, "execution.workspace") {
		t.Fatalf("execution workspace with skills must be rejected, got %#v", plan)
	}
	externalAgent := acpTestSkillAgent(acpTestSkill("a"))
	externalAgent.Spec.Runtime.Type = ""
	externalAgent.Spec.Runtime.ContractVersion = nil
	externalAgent.Spec.Runtime.RuntimeRef = &corev1alpha1.AgentRuntimeReference{Name: "external"}
	if plan := reconciler.planAgentExecution(t.Context(), task, externalAgent); plan.path != agentExecutionPathRejected ||
		!strings.Contains(plan.rejectionReason, "external runtimeRef") {
		t.Fatalf("runtimeRef with skills must be rejected before the AgentRuntime lookup, got %#v", plan)
	}
	// An Agent admitted earlier must still be rejected once the gate closes.
	disabled := &TaskReconciler{Mode: executionmode.HarnessV2, ACPToolboxPolicy: acpTestToolboxPolicy()}
	if plan := disabled.planAgentExecution(t.Context(), task, acpTestSkillAgent(acpTestSkill("a"))); plan.path != agentExecutionPathRejected ||
		!strings.Contains(plan.rejectionReason, "disabled on this controller") {
		t.Fatalf("skills disabled must be rejected at planning, got %#v", plan)
	}
}

func TestPlanACPRuntimeFreezesSkillsIntoTheProfile(t *testing.T) {
	task := &corev1alpha1.Task{Spec: corev1alpha1.TaskSpec{
		Type: corev1alpha1.TaskTypeAgent, Workspace: &corev1alpha1.WorkspaceConfig{Intent: corev1alpha1.WorkspaceIntentRead},
	}}
	images := ACPRuntimeImages{Codex: "docker.io/example/codex@sha256:" + strings.Repeat("a", 64)}
	plan := func(skills ...corev1alpha1.AgentRuntimeSkill) ACPRuntimePlan {
		t.Helper()
		planned, err := PlanACPRuntime(task, acpTestSkillAgent(skills...), images)
		if err != nil {
			t.Fatal(err)
		}
		return planned
	}
	baseline := plan()
	if baseline.Profile.Skills != nil {
		t.Fatalf("agent without skills produced %#v", baseline.Profile.Skills)
	}
	explicit := acpTestSkill("release-notes")
	explicit.Path = "/skills/bundle/notes"
	first := plan(acpTestSkill("code-review"), explicit)
	if len(first.Profile.Skills) != 2 || first.Profile.Skills[0].Path != "/skills/code-review" || first.Profile.Skills[1].Path != "/skills/bundle/notes" {
		t.Fatalf("plan must freeze the effective skill paths in order: %#v", first.Profile.Skills)
	}
	if first.Digest == baseline.Digest || first.PoolName == baseline.PoolName {
		t.Fatal("skills must rotate the profile digest and pool name")
	}
	if first.Profile.AgentConfigurationDigest != baseline.Profile.AgentConfigurationDigest {
		t.Fatal("skills must not change the agent configuration digest")
	}
	defaulted := acpTestSkill("code-review")
	defaulted.Path = "/skills/code-review"
	if same := plan(defaulted, explicit); same.Digest != first.Digest {
		t.Fatal("an explicit default path must freeze to the same profile as an omitted path")
	}
	otherImage := acpTestSkill("code-review")
	otherImage.Image = "registry.example.com/tools/skills/code-review@sha256:" + strings.Repeat("f", 64)
	seen := map[harnessv2.ProfileDigest]string{first.Digest: "base", baseline.Digest: "none"}
	for name, variant := range map[string]ACPRuntimePlan{
		"image": plan(otherImage, explicit),
		"order": plan(explicit, acpTestSkill("code-review")),
		"path": plan(acpTestSkill("code-review"), func() corev1alpha1.AgentRuntimeSkill {
			changed := explicit
			changed.Path = "/skills/bundle/notes-v2"
			return changed
		}()),
		"removed": plan(acpTestSkill("code-review")),
	} {
		if previous, duplicate := seen[variant.Digest]; duplicate {
			t.Errorf("%s produced the same digest as %s", name, previous)
		}
		seen[variant.Digest] = name
	}

	// A Task binds the RuntimePool profile frozen at planning time. Editing
	// the Agent afterwards produces a different profile and pool; the frozen
	// projection keeps the original skills.
	frozen := RuntimePoolProfileFromPlan(first)
	edited := plan(acpTestSkill("code-review"))
	if edited.PoolName == first.PoolName {
		t.Fatal("editing an Agent's skills must select a different RuntimePool")
	}
	if len(frozen.Skills) != 2 || frozen.Skills[1].Path != "/skills/bundle/notes" {
		t.Fatalf("frozen RuntimePool profile changed: %#v", frozen.Skills)
	}
}

func TestSkillProfileRoundTripsThroughEveryConversion(t *testing.T) {
	task := &corev1alpha1.Task{Spec: corev1alpha1.TaskSpec{
		Type: corev1alpha1.TaskTypeAgent, Workspace: &corev1alpha1.WorkspaceConfig{Intent: corev1alpha1.WorkspaceIntentRead},
	}}
	images := ACPRuntimeImages{Codex: "docker.io/example/codex@sha256:" + strings.Repeat("a", 64)}
	agent := acpTestSkillAgent(acpTestSkill("code-review"), acpTestSkill("release-notes"))
	agent.Spec.Runtime.Toolboxes = []corev1alpha1.AgentToolbox{acpTestToolbox("/opt/yq", "bin")}
	plan, err := PlanACPRuntime(task, agent, images)
	if err != nil {
		t.Fatal(err)
	}
	spec := RuntimePoolProfileFromPlan(plan)
	fromPool, err := runtimePoolHarnessProfile(spec)
	if err != nil {
		t.Fatal(err)
	}
	pool := runtimePoolTestObject(1)
	pool.Spec.Runtime.Profile = spec
	r := runtimePoolTestReconciler(t, runtimePoolTestScheme(t), nil, pool)
	r.ToolboxPolicy = acpTestSkillPolicy()
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
	fromEnvironment, err := runtimePoolSkillsFromEnvironment(environment)
	if err != nil {
		t.Fatal(err)
	}
	supervisorProfile := plan.Profile
	supervisorProfile.Skills = fromEnvironment
	for name, profile := range map[string]harnessv2.RuntimeProfile{
		"plan": plan.Profile, "pool": fromPool, "dispatcher": runtimeProfileFromPool(spec),
		"template": deployedCfg.profile, "supervisor-env": supervisorProfile,
	} {
		digest, err := harnessv2.CanonicalProfileDigest(profile)
		if err != nil {
			t.Fatal(err)
		}
		if digest != plan.Digest {
			t.Errorf("%s profile digest %s != plan digest %s (%#v)", name, digest, plan.Digest, profile.Skills)
		}
	}
	if environment[runtimePoolToolboxMountMethod] != string(ACPToolboxMountCopy) {
		t.Fatalf("mount method env = %q", environment[runtimePoolToolboxMountMethod])
	}
	count := 0
	for _, env := range template.Spec.Containers[0].Env {
		if env.Name == runtimePoolToolboxMountMethod {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("toolboxes and skills must share one mount method variable, got %d", count)
	}
}

func runtimePoolSkillTestObject(t *testing.T, skills ...harnessv2.RuntimeSkill) *corev1alpha1.RuntimePool {
	t.Helper()
	pool := runtimePoolTestObject(1)
	profile, err := runtimePoolHarnessProfile(pool.Spec.Runtime.Profile)
	if err != nil {
		t.Fatal(err)
	}
	profile.Skills = skills
	digest, err := harnessv2.CanonicalProfileDigest(profile)
	if err != nil {
		t.Fatal(err)
	}
	pool.Spec.Runtime.Profile.Digest = string(digest)
	pool.Spec.Runtime.Profile.Skills = runtimePoolSkillsFromProfile(skills)
	return pool
}

func runtimePoolTestSkills() []harnessv2.RuntimeSkill {
	return []harnessv2.RuntimeSkill{
		{Name: "code-review", Image: "registry.example.com/tools/skills/code-review@" + acpTestToolboxDigest, Path: "/skills/code-review"},
		{Name: "release-notes", Image: "registry.example.com/tools/skills/bundle@" + acpTestToolboxDigest, Path: "/skills/bundle/notes"},
	}
}

func TestRuntimePoolPodTemplateCopyModeWiresSkills(t *testing.T) {
	skills := runtimePoolTestSkills()
	pool := runtimePoolSkillTestObject(t, skills...)
	policy := acpTestSkillPolicy()
	policy.ImagePullSecrets = []string{"skill-pull"}
	r, template := renderToolboxTemplate(t, pool, policy)
	assertRuntimePoolEnvironment(t, r, pool, template.Spec.Containers[0].Env)
	if len(template.Spec.InitContainers) != 3 {
		t.Fatalf("init containers = %d, want handoff plus two skill copies", len(template.Spec.InitContainers))
	}
	assertToolboxHandoffContainer(t, pool, template.Spec.InitContainers[0])
	assertToolboxInitHardening(t, template)
	volumes := map[string]corev1.Volume{}
	for _, volume := range template.Spec.Volumes {
		volumes[volume.Name] = volume
	}
	for i, skill := range skills {
		copier := template.Spec.InitContainers[i+1]
		args := strings.Join(copier.Args, " ")
		if copier.Name != runtimePoolSkillCopyContainerName(i) || copier.Image != skill.Image ||
			copier.Command[0] != runtimePoolToolboxHandoffMountPath+"/"+toolbox.HandoffBinaryName ||
			!strings.HasPrefix(args, toolbox.CopySubcommand+" --src "+skill.Path+" --dst "+runtimePoolToolboxOutputMountPath) ||
			!strings.Contains(args, "--max-total-bytes 4194304") || !strings.Contains(args, "--max-entries 256") ||
			!strings.Contains(args, "--max-depth 8") || strings.Contains(args, "--path-entries") {
			t.Fatalf("skill copier %d = %#v", i, copier)
		}
		for _, mount := range copier.VolumeMounts {
			if mount.Name == runtimePoolToolboxHandoffVolume && !mount.ReadOnly {
				t.Fatal("the skill copier must mount the handoff read-only")
			}
		}
		volume := volumes[runtimePoolSkillVolumeName(i)]
		if volume.EmptyDir == nil || volume.EmptyDir.SizeLimit == nil || volume.EmptyDir.SizeLimit.Value() != runtimePoolSkillVolumeBytes {
			t.Fatalf("skill volume %d = %#v", i, volume)
		}
		assertSkillRuntimeMount(t, template, i, skill.MountPath(), toolbox.OutputRootName)
	}
	environment := runtimePoolLiteralEnvironment(template.Spec.Containers[0].Env)
	var projected []harnessv2.RuntimeSkill
	if err := json.Unmarshal([]byte(environment[runtimePoolSkillsEnv]), &projected); err != nil || len(projected) != 2 || projected[1].Path != "/skills/bundle/notes" {
		t.Fatalf("skill env = %q (%v)", environment[runtimePoolSkillsEnv], err)
	}
	if _, present := environment[runtimePoolToolboxesEnv]; present {
		t.Fatal("a skill-only pool must not project toolboxes")
	}
	if template.Spec.Containers[0].TerminationMessagePolicy != corev1.TerminationMessageFallbackToLogsOnError {
		t.Fatal("skill failures must reach the termination message")
	}
	if len(template.Spec.ImagePullSecrets) != 1 || template.Spec.ImagePullSecrets[0].Name != "skill-pull" {
		t.Fatalf("image pull secrets %#v", template.Spec.ImagePullSecrets)
	}
	assertRuntimeContainerProtections(t, template)
}

func assertSkillRuntimeMount(t *testing.T, template corev1.PodTemplateSpec, index int, mountPath, subPath string) {
	t.Helper()
	for _, mount := range template.Spec.Containers[0].VolumeMounts {
		if mount.Name != runtimePoolSkillVolumeName(index) {
			continue
		}
		if mount.MountPath != mountPath || mount.SubPath != subPath || !mount.ReadOnly {
			t.Fatalf("skill runtime mount %#v, want %s subPath %s read-only", mount, mountPath, subPath)
		}
		return
	}
	t.Fatalf("runtime container is missing the mount for skill %d", index)
}

func TestRuntimePoolPodTemplateSkillsShareToolboxWiring(t *testing.T) {
	pool := runtimePoolSkillTestObject(t, runtimePoolTestSkills()...)
	profile, err := runtimePoolHarnessProfile(pool.Spec.Runtime.Profile)
	if err != nil {
		t.Fatal(err)
	}
	profile.Toolboxes = runtimePoolTestToolboxes()
	digest, err := harnessv2.CanonicalProfileDigest(profile)
	if err != nil {
		t.Fatal(err)
	}
	pool.Spec.Runtime.Profile.Digest = string(digest)
	pool.Spec.Runtime.Profile.Toolboxes = runtimePoolToolboxesFromProfile(profile.Toolboxes)
	policy := acpTestSkillPolicy()
	policy.ImagePullSecrets = []string{"shared-pull"}
	_, template := renderToolboxTemplate(t, pool, policy)
	handoffs := 0
	names := make([]string, 0, len(template.Spec.InitContainers))
	for _, container := range template.Spec.InitContainers {
		names = append(names, container.Name)
		if container.Name == runtimePoolToolboxHandoffContainer {
			handoffs++
		}
	}
	if handoffs != 1 || strings.Join(names, ",") != "toolbox-handoff,toolbox-copy-0,toolbox-copy-1,skill-copy-0,skill-copy-1" {
		t.Fatalf("init containers %v", names)
	}
	if len(template.Spec.ImagePullSecrets) != 1 {
		t.Fatalf("pull secrets must be added once, got %#v", template.Spec.ImagePullSecrets)
	}
	if _, err := runtimePoolBootTopologySpec(template.Spec); err != nil {
		t.Fatalf("runtime recovery must accept controller-rendered skill init containers: %v", err)
	}
}

func TestRuntimePoolPodTemplateSkillImageVolumeMode(t *testing.T) {
	skills := runtimePoolTestSkills()
	pool := runtimePoolSkillTestObject(t, skills...)
	policy := acpTestSkillPolicy()
	policy.MountMethod = ACPToolboxMountImageVolume
	_, template := renderToolboxTemplate(t, pool, policy)
	if len(template.Spec.InitContainers) != 0 {
		t.Fatalf("image volume mode must not add init containers: %#v", template.Spec.InitContainers)
	}
	for i, skill := range skills {
		var volume *corev1.Volume
		for j := range template.Spec.Volumes {
			if template.Spec.Volumes[j].Name == runtimePoolSkillVolumeName(i) {
				volume = &template.Spec.Volumes[j]
			}
		}
		if volume == nil || volume.Image == nil || volume.Image.Reference != skill.Image {
			t.Fatalf("skill image volume %d = %#v", i, volume)
		}
		assertSkillRuntimeMount(t, template, i, skill.MountPath(), strings.TrimPrefix(skill.Path, "/"))
	}
	topology, err := runtimePoolBootTopologySpec(template.Spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, volume := range topology.Volumes {
		if volume.Image != nil {
			t.Fatalf("recovery topology kept skill image volume %s", volume.Name)
		}
	}
	assertRuntimeContainerProtections(t, template)
}

// TestRuntimePoolSkillImageVolumeStartupProbeOutlastsSkillCheck covers the
// supervisor's skill check, which reads image-volume skills before it serves
// and after any toolbox check. The startup probe must allow each check on top
// of the normal allowance, or kubelet restarts the container before the check
// reports a classified failure. Copy mode reads the images in init containers
// and keeps the default.
func TestRuntimePoolSkillImageVolumeStartupProbeOutlastsSkillCheck(t *testing.T) {
	startupBudget := func(template corev1.PodTemplateSpec) time.Duration {
		probe := template.Spec.Containers[0].StartupProbe
		return time.Duration(probe.FailureThreshold*probe.PeriodSeconds) * time.Second
	}
	imagePolicy := acpTestSkillPolicy()
	imagePolicy.MountMethod = ACPToolboxMountImageVolume

	skillsOnly := runtimePoolSkillTestObject(t, runtimePoolTestSkills()...)
	_, copyTemplate := renderToolboxTemplate(t, skillsOnly, acpTestSkillPolicy())
	_, imageTemplate := renderToolboxTemplate(t, skillsOnly, imagePolicy)
	_, plainTemplate := renderToolboxTemplate(t, runtimePoolTestObject(1), ACPToolboxPolicy{})
	base := startupBudget(plainTemplate)
	if got := startupBudget(copyTemplate); got != base {
		t.Fatalf("copy mode startup probe allows %s, want the default %s", got, base)
	}
	if got, want := startupBudget(imageTemplate), base+acpskill.StartupCheckTimeout; got < want {
		t.Fatalf("image volume skills without toolboxes: startup probe allows %s, want at least %s", got, want)
	}

	withToolboxes := runtimePoolSkillTestObject(t, runtimePoolTestSkills()...)
	profile, err := runtimePoolHarnessProfile(withToolboxes.Spec.Runtime.Profile)
	if err != nil {
		t.Fatal(err)
	}
	profile.Toolboxes = runtimePoolTestToolboxes()
	digest, err := harnessv2.CanonicalProfileDigest(profile)
	if err != nil {
		t.Fatal(err)
	}
	withToolboxes.Spec.Runtime.Profile.Digest = string(digest)
	withToolboxes.Spec.Runtime.Profile.Toolboxes = runtimePoolToolboxesFromProfile(profile.Toolboxes)
	_, bothTemplate := renderToolboxTemplate(t, withToolboxes, imagePolicy)
	if got, want := startupBudget(bothTemplate), base+toolbox.ImageVolumeCheckTimeout+acpskill.StartupCheckTimeout; got < want {
		t.Fatalf("image volume skills with toolboxes: startup probe allows %s, want at least %s", got, want)
	}
	plain, image := plainTemplate.Spec.Containers[0].StartupProbe, imageTemplate.Spec.Containers[0].StartupProbe
	if image.PeriodSeconds != plain.PeriodSeconds || image.TimeoutSeconds != plain.TimeoutSeconds {
		t.Fatalf("image volume startup probe changed more than its failure threshold: %#v", image)
	}
}

func TestRuntimePoolSkillAdmissionFailsClosed(t *testing.T) {
	pool := runtimePoolSkillTestObject(t, runtimePoolTestSkills()...)
	r := runtimePoolTestReconciler(t, runtimePoolTestScheme(t), nil, pool)
	cfg, err := r.runtimePoolConfig(pool)
	if err != nil {
		t.Fatal(err)
	}
	unsupported := cfg
	unsupported.profile.ProviderKind = string(corev1alpha1.AgentRuntimeCopilot)
	cases := map[string]struct {
		policy ACPToolboxPolicy
		cfg    runtimePoolConfig
		want   string
	}{
		"toolboxes disabled": {policy: ACPToolboxPolicy{SkillsEnabled: true}, cfg: cfg, want: "toolboxes are disabled"},
		"skills disabled":    {policy: acpTestToolboxPolicy(), cfg: cfg, want: "skills are disabled"},
		"registry removed": {policy: func() ACPToolboxPolicy {
			p := acpTestSkillPolicy()
			p.AllowedRegistries = []string{"ghcr.io/other"}
			return p
		}(), cfg: cfg, want: "skill image"},
		"unsupported runtime": {policy: acpTestSkillPolicy(), cfg: unsupported, want: "not supported by runtime \"copilot\""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r.ToolboxPolicy = tc.policy
			err := r.runtimePoolToolboxAdmission(pool, tc.cfg)
			if err == nil || !strings.HasPrefix(err.Error(), acpToolboxUnavailablePrefix) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want ToolboxUnavailable with %q", err, tc.want)
			}
		})
	}
	r.ToolboxPolicy = acpTestSkillPolicy()
	if err := r.runtimePoolToolboxAdmission(pool, cfg); err != nil {
		t.Fatalf("enabled policy: %v", err)
	}
}

func TestRuntimePoolSkillFailureClassification(t *testing.T) {
	images := runtimePoolBoundImages(nil, runtimePoolTestSkills())
	copyFailure := []corev1.Pod{{Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{{
		Name: runtimePoolSkillCopyContainerName(1),
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: 1, Message: "FAIL reason=TOOLBOX_UNSUPPORTED_FILE_TYPE msg=/skills/bundle/notes/pipe is a FIFO",
		}},
	}}}}}
	reason, message, ok := runtimePoolToolboxFailure(copyFailure, images, runtimePoolTestNow)
	if !ok || reason != corev1alpha1.RuntimePoolReasonToolboxUnavailable || !strings.Contains(message, "skill-copy-1: /skills/bundle/notes/pipe") {
		t.Fatalf("skill copier failure = %q %q %v", reason, message, ok)
	}
	supervisorFailure := []corev1.Pod{{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
		Name: runtimeField,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: 1, Message: "FAIL reason=SKILL_INVALID_FRONTMATTER msg=skill code-review SKILL.md: frontmatter key \"allowed-tools\" is not allowed",
		}},
	}}}}}
	if _, message, ok := runtimePoolToolboxFailure(supervisorFailure, images, runtimePoolTestNow); !ok || !strings.Contains(message, "SKILL_INVALID_FRONTMATTER") {
		t.Fatalf("supervisor skill failure = %q %v", message, ok)
	}
	pull := []corev1.Pod{{Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{{
		Name:  runtimePoolSkillCopyContainerName(0),
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: podWaitingReasonInvalidImageName, Message: "bad"}},
	}}}}}
	if _, message, ok := runtimePoolToolboxFailure(pull, images, runtimePoolTestNow); !ok || !strings.Contains(message, toolbox.ReasonImagePull) {
		t.Fatalf("skill image failure = %q %v", message, ok)
	}
	if _, _, ok := runtimePoolToolboxFailure(copyFailure, nil, runtimePoolTestNow); ok {
		t.Fatal("a pool without toolboxes or skills must not classify image failures")
	}
}

func TestRuntimePoolSkillsFromEnvironment(t *testing.T) {
	if skills, err := runtimePoolSkillsFromEnvironment(map[string]string{}); err != nil || skills != nil {
		t.Fatalf("absent = %#v, %v", skills, err)
	}
	if skills, err := runtimePoolSkillsFromEnvironment(map[string]string{runtimePoolSkillsEnv: "[]"}); err != nil || skills != nil {
		t.Fatalf("empty = %#v, %v", skills, err)
	}
	for _, raw := range []string{
		"{",
		`[{"name":"a","image":"registry.example.com/a:1","path":"/skills/a"}]`,
		`[{"name":"a","image":"registry.example.com/a@` + acpTestToolboxDigest + `","path":"/etc"}]`,
	} {
		if _, err := runtimePoolSkillsFromEnvironment(map[string]string{runtimePoolSkillsEnv: raw}); err == nil {
			t.Fatalf("%s must be rejected", raw)
		}
	}
}

func TestAgentRuntimeSkillsSurviveJSONRoundTrip(t *testing.T) {
	skill := acpTestSkill("code-review")
	skill.Path = "/skills/bundle/code-review"
	agent := acpTestSkillAgent(skill)
	data, err := json.Marshal(agent.Spec.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	var decoded corev1alpha1.AgentCLIRuntime
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Skills) != 1 || decoded.Skills[0] != skill {
		t.Fatalf("runtime skills lost in JSON round trip: %s", data)
	}
}
