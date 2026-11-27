/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/orka-agents/orka/internal/workerenv"
)

func TestCodeExecKubernetesResourcesHardening(t *testing.T) {
	t.Setenv(workerenv.CodeExecKubernetesSuppStrict, "true")
	t.Setenv(workerenv.CodeExecKubernetesHostUsers, "false")

	exec := &KubernetesJobCodeExecutor{}
	res, err := exec.buildResourcesWithJobName("default", CodeExecutionRequest{
		Language: codeLanguagePython,
		Code:     "print('hi')",
		Timeout:  defaultCodeExecTimeout,
	}, "orka-code-exec-test")
	if err != nil {
		t.Fatalf("buildResourcesWithJobName() error = %v", err)
	}
	if res.secret.Immutable == nil || !*res.secret.Immutable {
		t.Fatalf("code_exec Secret Immutable = %v, want true", res.secret.Immutable)
	}
	if res.job.Spec.PodFailurePolicy == nil || len(res.job.Spec.PodFailurePolicy.Rules) != 2 ||
		res.job.Spec.PodFailurePolicy.Rules[0].Action != batchv1.PodFailurePolicyActionIgnore ||
		res.job.Spec.PodFailurePolicy.Rules[1].Action != batchv1.PodFailurePolicyActionFailJob {
		t.Fatalf("code_exec PodFailurePolicy = %#v", res.job.Spec.PodFailurePolicy)
	}
	if res.job.Spec.Template.Spec.SecurityContext.SupplementalGroupsPolicy == nil ||
		*res.job.Spec.Template.Spec.SecurityContext.SupplementalGroupsPolicy != corev1.SupplementalGroupsPolicyStrict {
		t.Fatalf("code_exec SupplementalGroupsPolicy = %v, want Strict", res.job.Spec.Template.Spec.SecurityContext.SupplementalGroupsPolicy)
	}
	if res.job.Spec.Template.Spec.HostUsers == nil || *res.job.Spec.Template.Spec.HostUsers {
		t.Fatalf("code_exec HostUsers = %v, want false", res.job.Spec.Template.Spec.HostUsers)
	}
}
