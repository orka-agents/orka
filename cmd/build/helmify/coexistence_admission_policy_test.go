package main

import (
	"strings"
	"testing"
)

func TestStaticChartClassificationAndResolutionAdmissionPolicies(t *testing.T) {
	digest := "sha256:" + strings.Repeat("3", 64)
	base := []string{
		"--set", "admission.enabled=true",
		"--set", "admission.webhooks.enabled=true",
		"--set-string", "controller.image.digest=" + digest,
		"--set-string", "admission.tls.existingSecret=orka-admission-tls",
		"--set-string", "admission.webhooks.caBundle=Y2E=",
	}

	classification := requireHelmRender(t, append(append([]string{}, base...),
		"--set-string", "admission.identity.classificationUsernames[0]=migration@example.test",
		"--show-only", "templates/agent-execution-classification-admission-policy.yaml",
	)...)
	for _, marker := range []string{
		"kind: ValidatingAdmissionPolicy",
		"kind: ValidatingAdmissionPolicyBinding",
		"resources: [agents, agentruntimes]",
		"contract-classification-introduced",
		"params.status.classification.state == 'Open'",
		"classification.controlUID) == string(params.metadata.uid)",
		"classification.controlGeneration == params.metadata.generation",
		"classification.inventoryDigest.matches('^sha256:[a-f0-9]{64}$')",
		"request.userInfo.username == \"migration@example.test\"",
		"parameterNotFoundAction: Deny",
	} {
		if !strings.Contains(classification, marker) {
			t.Fatalf("classification admission policy is missing %q:\n%s", marker, classification)
		}
	}

	resolution := requireHelmRender(t, append(append([]string{}, base...),
		"--show-only", "templates/agent-execution-resolution-admission-policy.yaml",
	)...)
	for _, marker := range []string{
		"kind: ValidatingAdmissionPolicy",
		"kind: ValidatingAdmissionPolicyBinding",
		"resources: [tasks, tasks/status, runtimesessioncontrols/status]",
		"resolution-reference-introduced",
		"params.status.observedGeneration == params.metadata.generation",
		"params.status.classification.state == 'Sealed'",
		"classification.controlUID) == string(params.metadata.uid)",
		"classification.controlGeneration == params.metadata.generation",
		"parameterNotFoundAction: Deny",
	} {
		if !strings.Contains(resolution, marker) {
			t.Fatalf("resolution admission policy is missing %q:\n%s", marker, resolution)
		}
	}
}
