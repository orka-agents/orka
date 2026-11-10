package main

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/yaml"
)

func renderControllerArgs(t *testing.T, args ...string) []string {
	t.Helper()
	rendered := requireHelmRender(t, append(args, "--show-only", "templates/deployment.yaml")...)
	var deployment appsv1.Deployment
	if err := yaml.Unmarshal([]byte(rendered), &deployment); err != nil {
		t.Fatalf("decode controller Deployment: %v", err)
	}
	return deployment.Spec.Template.Spec.Containers[0].Args
}

func TestStaticChartEvalsAreOffByDefault(t *testing.T) {
	for _, arg := range renderControllerArgs(t) {
		if strings.HasPrefix(arg, "--enable-evals") || strings.HasPrefix(arg, "--eval-") ||
			strings.HasPrefix(arg, "--protected-set") {
			t.Fatalf("default render passes %s", arg)
		}
	}
}

func TestStaticChartEvalFlags(t *testing.T) {
	args := renderControllerArgs(t,
		"--set", "controller.evals.enabled=true",
		"--set-string", "controller.evals.gateConfigMap=orka-system/orka-protected-set",
		"--set", "controller.evals.promoterGroups={orka-promoters,release-leads}",
		"--set-string", "controller.evals.proposerAgent=proposer",
		"--set-string", "controller.protectedSetConfigMap=orka-protected-set",
		"--set", "controller.evals.proposerModels={openai/gpt-4.1,openai/gpt-4o-mini}",
	)
	for _, want := range []string{
		"--enable-evals=true",
		"--eval-gate-configmap=orka-system/orka-protected-set",
		"--eval-promoter-groups=orka-promoters,release-leads",
		"--eval-proposer-agent=proposer",
		"--protected-set-configmap=orka-protected-set",
		"--eval-proposer-models=openai/gpt-4.1,openai/gpt-4o-mini",
	} {
		if !containsString(args, want) {
			t.Errorf("controller args are missing %s:\n%s", want, strings.Join(args, "\n"))
		}
	}
	// Enabled without a gate or promoters, plain EvalRuns run and no flag is
	// passed empty.
	for _, arg := range renderControllerArgs(t, "--set", "controller.evals.enabled=true") {
		if strings.HasPrefix(arg, "--eval-") {
			t.Fatalf("render without a gate passes %s", arg)
		}
	}
}
