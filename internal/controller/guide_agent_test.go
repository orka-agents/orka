/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/executionmode"
)

const (
	guideTestNamespace  = "orka-system"
	guideTestDeployment = "orka-controller-manager"
	guideTestDocsImage  = "ghcr.io/orka-agents/orka/guide-docs@" + acpTestToolboxDigest
)

var guideTestImages = ACPRuntimeImages{
	Claude:  "docker.io/example/claude@sha256:" + strings.Repeat("a", 64),
	Codex:   "docker.io/example/codex@sha256:" + strings.Repeat("b", 64),
	Copilot: "docker.io/example/copilot@sha256:" + strings.Repeat("c", 64),
}

func guideTestConfig() GuideAgentConfig {
	return GuideAgentConfig{
		Enabled:         true,
		Namespace:       guideTestNamespace,
		Runtime:         corev1alpha1.AgentRuntimeClaude,
		Model:           "claude-test",
		MaxTurns:        DefaultGuideMaxTurns,
		DocsImage:       guideTestDocsImage,
		ContractVersion: executionmode.HarnessV2.ContractVersion(),
		OwnerDeployment: guideTestDeployment,
	}
}

func guideTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, corev1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	return scheme
}

func guideTestDeploymentObject(uid string) *appsv1.Deployment {
	return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name: guideTestDeployment, Namespace: guideTestNamespace, UID: types.UID(uid),
	}}
}

func guideTestReconciler(t *testing.T, config GuideAgentConfig, objects ...client.Object) (*GuideAgentReconciler, client.Client) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(guideTestScheme(t)).WithObjects(objects...).Build()
	return &GuideAgentReconciler{Client: c, APIReader: c, Config: config}, c
}

func reconcileGuide(t *testing.T, r *GuideAgentReconciler) error {
	t.Helper()
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: r.key()})
	return err
}

func getGuide(t *testing.T, c client.Client) (*corev1alpha1.Agent, bool) {
	t.Helper()
	agent := &corev1alpha1.Agent{}
	err := c.Get(context.Background(), types.NamespacedName{Namespace: guideTestNamespace, Name: GuideAgentName}, agent)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return agent, true
}

func TestGuideDesiredAgentIsReadOnly(t *testing.T) {
	agent := guideTestConfig().DesiredAgent()
	spec := agent.Spec
	if spec.Runtime == nil || spec.Runtime.Type != corev1alpha1.AgentRuntimeClaude {
		t.Fatalf("runtime = %#v, want built-in claude", spec.Runtime)
	}
	if got := spec.Runtime.DefaultAllowedTools; !slices.Equal(got, []string{"Glob", "Grep", "Read"}) {
		t.Fatalf("defaultAllowedTools = %v, want exactly Glob, Grep, Read", got)
	}
	if spec.Runtime.DefaultAllowBash == nil || *spec.Runtime.DefaultAllowBash {
		t.Fatal("defaultAllowBash must be explicitly false")
	}
	if spec.Runtime.RuntimeRef != nil || spec.ProviderRef != nil || spec.SecretRef != nil ||
		spec.Execution != nil || spec.Coordination != nil || len(spec.Tools) != 0 || len(spec.Skills) != 0 {
		t.Fatalf("guide Agent must carry no external runtime, credentials, workspace, tools or skills: %#v", spec)
	}
	if spec.Runtime.ContractVersion == nil || *spec.Runtime.ContractVersion != corev1alpha1.AgentRuntimeContractHarnessV2 {
		t.Fatalf("contractVersion = %v, want %s", spec.Runtime.ContractVersion, corev1alpha1.AgentRuntimeContractHarnessV2)
	}
	if spec.Runtime.DefaultMaxTurns == nil || *spec.Runtime.DefaultMaxTurns != DefaultGuideMaxTurns {
		t.Fatalf("defaultMaxTurns = %v, want %d", spec.Runtime.DefaultMaxTurns, DefaultGuideMaxTurns)
	}
	want := []corev1alpha1.AgentToolbox{{Image: guideTestDocsImage, MountPath: GuideDocsMountPath}}
	if !slices.EqualFunc(spec.Runtime.Toolboxes, want, func(a, b corev1alpha1.AgentToolbox) bool {
		return a.Image == b.Image && a.MountPath == b.MountPath && len(a.PathEntries) == 0
	}) {
		t.Fatalf("toolboxes = %#v, want %#v", spec.Runtime.Toolboxes, want)
	}
	if spec.SystemPrompt == nil || spec.SystemPrompt.ConfigMapRef != nil ||
		!strings.Contains(spec.SystemPrompt.Inline, "/opt/orka-docs/guide/SKILL.md") ||
		!strings.Contains(spec.SystemPrompt.Inline, "/opt/orka-docs/guide/references/in-cluster.md") {
		t.Fatalf("system prompt must point at the snapshot's skill: %#v", spec.SystemPrompt)
	}
	if agent.Name != GuideAgentName || agent.Namespace != guideTestNamespace || !isManagedGuideAgent(agent) {
		t.Fatalf("metadata = %#v, want the managed guide Agent", agent.ObjectMeta)
	}
}

// The guide must plan as a read-only session on every supported runtime: a
// guide Task has no workspace, so it runs with read intent.
func TestGuideAgentPlansReadOnlySessionOnSupportedRuntimes(t *testing.T) {
	images := guideTestImages
	reader := fake.NewClientBuilder().WithScheme(guideTestScheme(t)).Build()
	for _, runtimeType := range GuideRuntimeTypes {
		t.Run(string(runtimeType), func(t *testing.T) {
			config := guideTestConfig()
			config.Runtime = runtimeType
			if err := config.Validate(acpTestToolboxPolicy(), guideTestImages); err != nil {
				t.Fatalf("Validate() = %v", err)
			}
			agent := config.DesiredAgent()
			agent.UID, agent.Generation = types.UID("guide-uid"), 1
			agentReconciler := &AgentReconciler{Client: reader, Mode: executionmode.HarnessV2, ToolboxPolicy: acpTestToolboxPolicy()}
			if err := agentReconciler.validateAgent(context.Background(), agent); err != nil {
				t.Fatalf("Agent validation rejected the guide: %v", err)
			}
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{Name: "ask", Namespace: guideTestNamespace},
				Spec: corev1alpha1.TaskSpec{
					Type: corev1alpha1.TaskTypeAgent, Prompt: "Does Orka support toolboxes?",
					AgentRef: &corev1alpha1.AgentReference{Name: GuideAgentName},
				},
			}
			configuration, err := resolveACPAgentSessionConfiguration(context.Background(), reader, task, agent)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := PlanACPRuntimeWithConfiguration(task, agent, images, configuration)
			if err != nil {
				t.Fatalf("plan = %v", err)
			}
			if plan.Profile.WorkspaceIntent != "read" {
				t.Fatalf("workspace intent = %q, want read", plan.Profile.WorkspaceIntent)
			}
			if len(plan.Profile.Toolboxes) != 1 || plan.Profile.Toolboxes[0].MountPath != GuideDocsMountPath ||
				plan.Profile.Toolboxes[0].Image != guideTestDocsImage {
				t.Fatalf("toolboxes = %#v", plan.Profile.Toolboxes)
			}
			allowed, _, allowBash := normalizeACPRuntimeToolPolicy(string(runtimeType), effectiveACPWorkspaceIntent(task),
				effectiveACPAllowedTools(task, agent), nil, effectiveACPAllowBash(task, agent))
			if allowBash || slices.ContainsFunc(allowed, func(name string) bool {
				return !slices.Contains([]string{"Glob", "Grep", "Read"}, name)
			}) {
				t.Fatalf("tool policy = %v bash=%v, want only Glob, Grep, Read", allowed, allowBash)
			}
		})
	}
}

func TestGuideAgentConfigValidate(t *testing.T) {
	policy := acpTestToolboxPolicy()
	if err := (GuideAgentConfig{}).Validate(ACPToolboxPolicy{}, ACPRuntimeImages{}); err != nil {
		t.Fatalf("disabled guide must not be validated: %v", err)
	}
	unavailable := policy
	unavailable.UnavailableReason = "image volumes need Kubernetes 1.36"
	if err := guideTestConfig().Validate(unavailable, guideTestImages); err != nil {
		t.Fatalf("a mounting outage is reported on the Agent, not at startup: %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*GuideAgentConfig, *ACPToolboxPolicy, *ACPRuntimeImages)
		want   string
	}{
		{"opencode", func(c *GuideAgentConfig, _ *ACPToolboxPolicy, _ *ACPRuntimeImages) {
			c.Runtime = corev1alpha1.AgentRuntimeOpencode
		}, "--guide-runtime"},
		{"codex", func(c *GuideAgentConfig, _ *ACPToolboxPolicy, _ *ACPRuntimeImages) {
			c.Runtime = corev1alpha1.AgentRuntimeCodex
		}, "--guide-runtime"},
		{"unknown runtime", func(c *GuideAgentConfig, _ *ACPToolboxPolicy, _ *ACPRuntimeImages) { c.Runtime = "gemini" }, "--guide-runtime"},
		{"no model", func(c *GuideAgentConfig, _ *ACPToolboxPolicy, _ *ACPRuntimeImages) { c.Model = " " }, "--guide-model"},
		{"zero turns", func(c *GuideAgentConfig, _ *ACPToolboxPolicy, _ *ACPRuntimeImages) { c.MaxTurns = 0 }, "--guide-max-turns"},
		{"too many turns", func(c *GuideAgentConfig, _ *ACPToolboxPolicy, _ *ACPRuntimeImages) { c.MaxTurns = 1001 }, "--guide-max-turns"},
		{"no image", func(c *GuideAgentConfig, _ *ACPToolboxPolicy, _ *ACPRuntimeImages) { c.DocsImage = "" }, "--guide-docs-image is required"},
		{"tag instead of digest", func(c *GuideAgentConfig, _ *ACPToolboxPolicy, _ *ACPRuntimeImages) {
			c.DocsImage = "ghcr.io/orka-agents/orka/guide-docs:v1"
		}, "--guide-docs-image"},
		{"toolboxes disabled", func(_ *GuideAgentConfig, p *ACPToolboxPolicy, _ *ACPRuntimeImages) { p.Enabled = false }, "--acp-toolboxes-enabled"},
		{"registry not allowed", func(_ *GuideAgentConfig, p *ACPToolboxPolicy, _ *ACPRuntimeImages) {
			p.AllowedRegistries = []string{"registry.example.com"}
		}, "not from an allowed toolbox registry"},
		{"runtime image not configured", func(_ *GuideAgentConfig, _ *ACPToolboxPolicy, i *ACPRuntimeImages) { i.Claude = "" }, "--acp-claude-runtime-image"},
		{"no namespace", func(c *GuideAgentConfig, _ *ACPToolboxPolicy, _ *ACPRuntimeImages) { c.Namespace = "" }, "namespace"},
		{"no contract", func(c *GuideAgentConfig, _ *ACPToolboxPolicy, _ *ACPRuntimeImages) { c.ContractVersion = "" }, "contract"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config, toolboxPolicy, images := guideTestConfig(), acpTestToolboxPolicy(), guideTestImages
			tt.mutate(&config, &toolboxPolicy, &images)
			err := config.Validate(toolboxPolicy, images)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate() = %v, want error containing %q", err, tt.want)
			}
		})
	}
}

func TestGuideAgentReconcilerCreatesOwnedAgent(t *testing.T) {
	r, c := guideTestReconciler(t, guideTestConfig(), guideTestDeploymentObject("deploy-uid"))
	if err := reconcileGuide(t, r); err != nil {
		t.Fatal(err)
	}
	agent, ok := getGuide(t, c)
	if !ok {
		t.Fatal("guide Agent was not created")
	}
	want := guideTestConfig().DesiredAgent()
	if !slices.Equal(agent.Spec.Runtime.DefaultAllowedTools, want.Spec.Runtime.DefaultAllowedTools) ||
		agent.Spec.Runtime.Toolboxes[0].Image != guideTestDocsImage || !isManagedGuideAgent(agent) {
		t.Fatalf("created Agent = %#v", agent)
	}
	if len(agent.OwnerReferences) != 1 {
		t.Fatalf("ownerReferences = %#v, want the controller Deployment", agent.OwnerReferences)
	}
	owner := agent.OwnerReferences[0]
	if owner.APIVersion != "apps/v1" || owner.Kind != "Deployment" || owner.Name != guideTestDeployment ||
		owner.UID != "deploy-uid" || owner.Controller != nil || owner.BlockOwnerDeletion != nil {
		t.Fatalf("ownerReference = %#v", owner)
	}
	// A second pass is a no-op.
	resourceVersion := agent.ResourceVersion
	if err := reconcileGuide(t, r); err != nil {
		t.Fatal(err)
	}
	if again, _ := getGuide(t, c); again.ResourceVersion != resourceVersion {
		t.Fatal("reconciling an unchanged guide Agent wrote it again")
	}
}

func TestGuideAgentReconcilerWaitsForOwnerDeployment(t *testing.T) {
	r, c := guideTestReconciler(t, guideTestConfig())
	if err := reconcileGuide(t, r); err == nil {
		t.Fatal("a missing owner Deployment must be an error so the request is retried")
	}
	if _, ok := getGuide(t, c); ok {
		t.Fatal("guide Agent must not be created without its owner")
	}

	config := guideTestConfig()
	config.OwnerDeployment = ""
	r, c = guideTestReconciler(t, config)
	if err := reconcileGuide(t, r); err != nil {
		t.Fatal(err)
	}
	if agent, ok := getGuide(t, c); !ok || len(agent.OwnerReferences) != 0 {
		t.Fatalf("without an owner Deployment the Agent is created unowned: %#v", agent)
	}
}

func TestGuideAgentReconcilerRevertsDrift(t *testing.T) {
	drifted := guideTestConfig().DesiredAgent()
	drifted.Labels["team"] = "docs"
	delete(drifted.Labels, orkaManagedByLabelKey)
	drifted.Spec.Runtime.DefaultAllowedTools = []string{"Read", "Bash"}
	drifted.Spec.Runtime.DefaultAllowBash = new(true)
	drifted.Spec.SecretRef = &corev1.LocalObjectReference{Name: "stolen"}
	drifted.OwnerReferences = []metav1.OwnerReference{
		{APIVersion: "apps/v1", Kind: "Deployment", Name: guideTestDeployment, UID: "old-uid"},
		{APIVersion: "v1", Kind: "ConfigMap", Name: "keep", UID: "cm-uid"},
	}
	r, c := guideTestReconciler(t, guideTestConfig(), guideTestDeploymentObject("new-uid"), drifted)
	if err := reconcileGuide(t, r); err != nil {
		t.Fatal(err)
	}
	agent, _ := getGuide(t, c)
	want := guideTestConfig().DesiredAgent()
	if !slices.Equal(agent.Spec.Runtime.DefaultAllowedTools, want.Spec.Runtime.DefaultAllowedTools) ||
		*agent.Spec.Runtime.DefaultAllowBash || agent.Spec.SecretRef != nil {
		t.Fatalf("spec drift was not reverted: %#v", agent.Spec)
	}
	if agent.Labels[orkaManagedByLabelKey] != managedByLabelValue || agent.Labels["team"] != "docs" {
		t.Fatalf("labels = %v, want guide labels restored and others kept", agent.Labels)
	}
	if len(agent.OwnerReferences) != 2 || agent.OwnerReferences[0].Kind != "ConfigMap" ||
		agent.OwnerReferences[1].UID != "new-uid" {
		t.Fatalf("ownerReferences = %#v, want the ConfigMap kept and the Deployment reference replaced", agent.OwnerReferences)
	}
}

func TestGuideAgentReconcilerLeavesUnmanagedAgent(t *testing.T) {
	foreign := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: GuideAgentName, Namespace: guideTestNamespace, Labels: map[string]string{"owner": "me"}},
		Spec:       corev1alpha1.AgentSpec{Model: &corev1alpha1.ModelConfig{Name: "mine"}},
	}
	for _, enabled := range []bool{true, false} {
		config := guideTestConfig()
		config.Enabled = enabled
		r, c := guideTestReconciler(t, config, guideTestDeploymentObject("deploy-uid"), foreign.DeepCopy())
		if err := reconcileGuide(t, r); err != nil {
			t.Fatal(err)
		}
		agent, ok := getGuide(t, c)
		if !ok || agent.Spec.Model.Name != "mine" || agent.Spec.Runtime != nil || len(agent.OwnerReferences) != 0 {
			t.Fatalf("enabled=%v: an unmanaged Agent was changed: %#v", enabled, agent)
		}
	}
}

func TestGuideAgentReconcilerDeletesManagedAgentWhenDisabled(t *testing.T) {
	config := guideTestConfig()
	config.Enabled = false
	r, c := guideTestReconciler(t, config, guideTestConfig().DesiredAgent())
	if err := reconcileGuide(t, r); err != nil {
		t.Fatal(err)
	}
	if _, ok := getGuide(t, c); ok {
		t.Fatal("disabling the guide must delete the managed guide Agent")
	}
	if err := reconcileGuide(t, r); err != nil {
		t.Fatalf("a disabled guide with no Agent is a no-op: %v", err)
	}
}

func TestGuideAgentReconcilerIgnoresOtherAgents(t *testing.T) {
	r, c := guideTestReconciler(t, guideTestConfig(), guideTestDeploymentObject("deploy-uid"))
	for _, key := range []types.NamespacedName{
		{Namespace: guideTestNamespace, Name: "other"},
		{Namespace: "elsewhere", Name: GuideAgentName},
	} {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := getGuide(t, c); ok {
		t.Fatal("a request for another Agent must not create the guide")
	}
}
