/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// config/admission/ai_worker_identity_protection.yaml admits only AI worker
// Jobs and Secrets of this shape. A code_exec change that breaks any of these
// properties makes the Kubernetes backend fail admission in every install, so
// update the policy together with the builder.
func TestKubernetesCodeExecJobMeetsAIWorkerIdentityPolicy(t *testing.T) {
	executor := &KubernetesJobCodeExecutor{}
	for _, runID := range []string{"session/task/tool-call-01", strings.Repeat("long-run-id-", 12), "///"} {
		resources, err := executor.buildResources(CodeExecutionRequest{
			Backend:          codeExecBackendKubernetes,
			Language:         codeLanguagePython,
			Code:             "print('hi')",
			Timeout:          time.Second,
			OutputLimitBytes: defaultCodeExecOutputLimitBytes,
			RunID:            runID,
		})
		if err != nil {
			t.Fatalf("buildResources(%q) error = %v", runID, err)
		}
		if resources.secret.Type != corev1.SecretTypeOpaque {
			t.Errorf("code Secret type = %q, want Opaque", resources.secret.Type)
		}
		job := resources.job
		pod := job.Spec.Template.Spec
		if !strings.HasPrefix(job.Name, codeExecKubernetesJobPrefix) {
			t.Errorf("Job name %q lacks prefix %q", job.Name, codeExecKubernetesJobPrefix)
		}
		if pod.ServiceAccountName != job.Name || resources.serviceAccount.Name != job.Name {
			t.Errorf("Job %q runs as %q (created %q), want its own per-run ServiceAccount", job.Name, pod.ServiceAccountName, resources.serviceAccount.Name)
		}
		if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
			t.Error("Pod automountServiceAccountToken must be explicitly false")
		}
		if pod.HostNetwork || pod.HostPID || pod.HostIPC || len(pod.InitContainers) > 0 || len(pod.EphemeralContainers) > 0 {
			t.Error("Pod must not use host namespaces or init/ephemeral containers")
		}
		allowedLabels := map[string]string{
			"app.kubernetes.io/name":       "orka",
			codeExecKubernetesLabelTool:    codeExecToolName,
			codeExecKubernetesLabelJob:     job.Name,
			"batch.kubernetes.io/job-name": job.Name,
		}
		for key, value := range job.Spec.Template.Labels {
			if want, ok := allowedLabels[key]; !ok || value != want {
				t.Errorf("Pod label %s=%q is not one the policy admits", key, value)
			}
		}
		if pod.Subdomain != "" {
			t.Errorf("Pod subdomain %q is not admitted", pod.Subdomain)
		}
		if pod.SecurityContext != nil && pod.SecurityContext.SeccompProfile != nil &&
			pod.SecurityContext.SeccompProfile.Type == corev1.SeccompProfileTypeUnconfined {
			t.Error("Pod seccomp must not be Unconfined")
		}
		for _, volume := range pod.Volumes {
			if volume.EmptyDir == nil && volume.Secret == nil {
				t.Errorf("volume %q is neither emptyDir nor Secret", volume.Name)
			}
		}
		for _, container := range pod.Containers {
			sc := container.SecurityContext
			nonRoot := pod.SecurityContext != nil && pod.SecurityContext.RunAsNonRoot != nil && *pod.SecurityContext.RunAsNonRoot
			if sc != nil && sc.RunAsNonRoot != nil {
				nonRoot = *sc.RunAsNonRoot
			}
			switch {
			case sc == nil, sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation,
				sc.Privileged != nil && *sc.Privileged,
				sc.Capabilities == nil || len(sc.Capabilities.Add) > 0 || !slices.Contains(sc.Capabilities.Drop, "ALL"),
				sc.SeccompProfile != nil && sc.SeccompProfile.Type == corev1.SeccompProfileTypeUnconfined, !nonRoot:
				t.Errorf("container %q security context %+v does not meet the policy", container.Name, sc)
			}
			for _, port := range container.Ports {
				if port.HostPort != 0 {
					t.Errorf("container %q uses hostPort %d", container.Name, port.HostPort)
				}
			}
		}
	}
}
