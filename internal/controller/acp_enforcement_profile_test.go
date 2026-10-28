package controller

import (
	"reflect"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

func TestACPRuntimeEnforcementProfileRoundTrip(t *testing.T) {
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{UID: "agent-uid", Generation: 1},
		Spec: corev1alpha1.AgentSpec{
			Model: &corev1alpha1.ModelConfig{Name: acpTestModel},
			Runtime: &corev1alpha1.AgentCLIRuntime{
				Type:            corev1alpha1.AgentRuntimeCodex,
				ContractVersion: new(corev1alpha1.AgentRuntimeContractHarnessV2),
			},
		},
	}
	task := &corev1alpha1.Task{Spec: corev1alpha1.TaskSpec{
		Type:      corev1alpha1.TaskTypeAgent,
		Workspace: &corev1alpha1.WorkspaceConfig{Intent: corev1alpha1.WorkspaceIntentRead},
	}}
	images := ACPRuntimeImages{Codex: "docker.io/example/codex@sha256:" + strings.Repeat("a", 64)}
	ordinary, err := PlanACPRuntime(task, agent, images)
	if err != nil {
		t.Fatal(err)
	}
	task.Spec.Enforcement = &corev1alpha1.EnforcementRequirement{
		Format: "agentsuite.policy/v1", Policy: []byte("opaque\x00policy\n"),
		RequiredCapabilities: []string{"filesystem.deny"}, Mode: "enforce",
	}
	protected, err := PlanACPRuntime(task, agent, images)
	if err != nil {
		t.Fatal(err)
	}
	if protected.Profile.EnforcementRequirementDigest == "" ||
		protected.Digest == ordinary.Digest || protected.PoolName == ordinary.PoolName {
		t.Fatal("complete requirement did not separate the protected pool")
	}
	if protected.Profile.AgentConfigurationDigest != ordinary.Profile.AgentConfigurationDigest {
		t.Fatal("Task enforcement changed Agent configuration")
	}
	pool := runtimePoolTestObject(1)
	pool.Spec.Runtime.Profile = RuntimePoolProfileFromPlan(protected)
	converted, err := runtimePoolHarnessProfile(pool.Spec.Runtime.Profile)
	if err != nil || !reflect.DeepEqual(converted, protected.Profile) {
		t.Fatalf("pool profile conversion lost requirement identity: %v", err)
	}
	if !reflect.DeepEqual(runtimeProfileFromPool(pool.Spec.Runtime.Profile), protected.Profile) {
		t.Fatal("dispatcher profile conversion lost requirement identity")
	}
	_, template := renderToolboxTemplate(t, pool, ACPToolboxPolicy{})
	_, deployed, err := runtimePoolValidationTargetFromTemplate(pool, template)
	if err != nil || !reflect.DeepEqual(deployed.profile, protected.Profile) {
		t.Fatalf("rendered pool reconstruction lost requirement identity: %v", err)
	}
	digest, err := harnessv2.CanonicalProfileDigest(deployed.profile)
	if err != nil || digest != protected.Digest {
		t.Fatalf("deployed digest differs from Task plan: %v", err)
	}
	task.Spec.Enforcement.Policy = append(task.Spec.Enforcement.Policy, '\n')
	changed, err := PlanACPRuntime(task, agent, images)
	if err != nil {
		t.Fatal(err)
	}
	if changed.Profile.EnforcementRequirementDigest == protected.Profile.EnforcementRequirementDigest ||
		changed.PoolName == protected.PoolName {
		t.Fatal("changed opaque policy bytes reused the same pool")
	}
	task.Spec.Enforcement = nil
	restored, err := PlanACPRuntime(task, agent, images)
	if err != nil || restored.Digest != ordinary.Digest || restored.PoolName != ordinary.PoolName {
		t.Fatalf("ordinary pool identity changed: %v", err)
	}
	pool.Spec.Runtime.Profile = RuntimePoolProfileFromPlan(ordinary)
	_, template = renderToolboxTemplate(t, pool, ACPToolboxPolicy{})
	for _, env := range template.Spec.Containers[0].Env {
		if env.Name == "ORKA_ACP_ENFORCEMENT_REQUIREMENT_DIGEST" {
			t.Fatal("ordinary pool acquired enforcement environment wiring")
		}
	}
}
