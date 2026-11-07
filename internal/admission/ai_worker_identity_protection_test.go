/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package admission

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	apiserveradmission "k8s.io/apiserver/pkg/admission"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const aiWorkerIdentityTestWorker = "system:serviceaccount:" + admissionTestNamespace + ":orka-ai-worker"

var (
	secretsResource = schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
	secretKind      = schema.GroupVersionKind{Version: "v1", Kind: "Secret"}
	jobsResource    = schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}
	jobKind         = schema.GroupVersionKind{Group: "batch", Version: "v1", Kind: "Job"}
)

func compileAIWorkerIdentityProtection(t *testing.T) admissionPolicyEvaluator {
	t.Helper()
	return compileAdmissionPolicy(t, "ai_worker_identity_protection.yaml", strings.NewReplacer("AI_WORKER_SA", "orka-ai-worker"))
}

func aiWorkerSecret(secretType string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]any{
			"name":        "probe",
			"namespace":   admissionTestNamespace,
			"annotations": map[string]any{"kubernetes.io/service-account.name": "orka-controller-manager"},
		},
		"type": secretType,
	}}
}

// codeExecJob mirrors the Job the Kubernetes code_exec backend builds
// (internal/tools/code_exec_kubernetes.go); TestKubernetesCodeExecJobMeetsAIWorkerIdentityPolicy
// keeps the two in step.
func codeExecJob() *unstructured.Unstructured {
	name := "orka-code-exec-run-0123456789abcdef"
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"metadata":   map[string]any{"name": name, "namespace": admissionTestNamespace},
		"spec": map[string]any{
			"template": map[string]any{
				"spec": map[string]any{
					"restartPolicy":                "Never",
					"serviceAccountName":           name,
					"automountServiceAccountToken": false,
					"securityContext": map[string]any{
						"runAsNonRoot":   true,
						"runAsUser":      int64(65532),
						"seccompProfile": map[string]any{"type": "RuntimeDefault"},
					},
					"containers": []any{map[string]any{
						"name":  "code",
						"image": "python:3",
						"securityContext": map[string]any{
							"runAsNonRoot":             true,
							"allowPrivilegeEscalation": false,
							"readOnlyRootFilesystem":   true,
							"capabilities":             map[string]any{"drop": []any{"ALL"}},
						},
					}},
					"volumes": []any{
						map[string]any{"name": "tmp", "emptyDir": map[string]any{"sizeLimit": "16Mi"}},
						map[string]any{"name": "code", "secret": map[string]any{"secretName": name}},
					},
				},
			},
		},
	}}
}

func setJobField(t *testing.T, job *unstructured.Unstructured, value any, path ...string) *unstructured.Unstructured {
	t.Helper()
	require.NoError(t, unstructured.SetNestedField(job.Object, value, append([]string{"spec", "template", "spec"}, path...)...))
	return job
}

func setCodeContainerField(t *testing.T, job *unstructured.Unstructured, value any, path ...string) *unstructured.Unstructured {
	t.Helper()
	containers, _, err := unstructured.NestedSlice(job.Object, "spec", "template", "spec", "containers")
	require.NoError(t, err)
	container := containers[0].(map[string]any)
	require.NoError(t, unstructured.SetNestedField(container, value, path...))
	return setJobField(t, job, containers, "containers")
}

func TestAIWorkerIdentityProtectionSecrets(t *testing.T) {
	policy := compileAIWorkerIdentityProtection(t)
	admits := func(operation apiserveradmission.Operation, username, secretType string) bool {
		secret := aiWorkerSecret(secretType)
		var old *unstructured.Unstructured
		if operation == apiserveradmission.Update {
			old = secret.DeepCopy()
		}
		return policy.admits(t, secretKind, secretsResource, "probe", operation, username, "", secret, old)
	}
	for _, operation := range []apiserveradmission.Operation{apiserveradmission.Create, apiserveradmission.Update} {
		require.True(t, admits(operation, aiWorkerIdentityTestWorker, "Opaque"), "the worker's own Opaque Secrets")
		require.False(t, admits(operation, aiWorkerIdentityTestWorker, "kubernetes.io/service-account-token"),
			"a token Secret naming the controller would hand the worker the controller's token")
		require.False(t, admits(operation, aiWorkerIdentityTestWorker, "kubernetes.io/dockerconfigjson"))
	}
	for _, username := range []string{
		trustedControllerUser,
		"system:serviceaccount:another-namespace:orka-ai-worker",
		"system:serviceaccount:" + admissionTestNamespace + ":orka-vendor-worker",
		untrustedUsername,
	} {
		require.True(t, admits(apiserveradmission.Create, username, "kubernetes.io/service-account-token"),
			"%s is outside the policy", username)
	}
}

func TestAIWorkerIdentityProtectionJobs(t *testing.T) {
	policy := compileAIWorkerIdentityProtection(t)
	admits := func(username string, job *unstructured.Unstructured) bool {
		return policy.admits(t, jobKind, jobsResource, job.GetName(), apiserveradmission.Create, username, "", job, nil)
	}
	require.True(t, admits(aiWorkerIdentityTestWorker, codeExecJob()), "the code_exec Job")

	for name, job := range map[string]*unstructured.Unstructured{
		"runs as the controller": setJobField(t, codeExecJob(), "orka-controller-manager", "serviceAccountName"),
		"default ServiceAccount": func() *unstructured.Unstructured {
			job := codeExecJob()
			unstructured.RemoveNestedField(job.Object, "spec", "template", "spec", "serviceAccountName")
			return job
		}(),
		"mounts its token": setJobField(t, codeExecJob(), true, "automountServiceAccountToken"),
		"token by default": func() *unstructured.Unstructured {
			job := codeExecJob()
			unstructured.RemoveNestedField(job.Object, "spec", "template", "spec", "automountServiceAccountToken")
			return job
		}(),
		"not a code_exec name": func() *unstructured.Unstructured {
			job := setJobField(t, codeExecJob(), "orka-controller-manager", "serviceAccountName")
			job.SetName("orka-controller-manager")
			return job
		}(),
		"projected token volume": setJobField(t, codeExecJob(), []any{map[string]any{
			"name": "token", "projected": map[string]any{"sources": []any{map[string]any{
				"serviceAccountToken": map[string]any{"path": "token"}}}}}}, "volumes"),
		"hostPath volume": setJobField(t, codeExecJob(), []any{map[string]any{
			"name": "root", "hostPath": map[string]any{"path": "/"}}}, "volumes"),
		"PVC volume": setJobField(t, codeExecJob(), []any{map[string]any{
			"name": "store", "persistentVolumeClaim": map[string]any{"claimName": "orka-store"}}}, "volumes"),
		"hostNetwork":        setJobField(t, codeExecJob(), true, "hostNetwork"),
		"hostPID":            setJobField(t, codeExecJob(), true, "hostPID"),
		"hostIPC":            setJobField(t, codeExecJob(), true, "hostIPC"),
		"privileged":         setCodeContainerField(t, codeExecJob(), true, "securityContext", "privileged"),
		"privilege escalate": setCodeContainerField(t, codeExecJob(), true, "securityContext", "allowPrivilegeEscalation"),
		"escalation unset": func() *unstructured.Unstructured {
			job := codeExecJob()
			return setCodeContainerField(t, job, map[string]any{"runAsNonRoot": true}, "securityContext")
		}(),
		"added capability": setCodeContainerField(t, codeExecJob(), []any{"SYS_ADMIN"}, "securityContext", "capabilities", "add"),
		"root":             setCodeContainerField(t, codeExecJob(), false, "securityContext", "runAsNonRoot"),
		"hostPort": setCodeContainerField(t, codeExecJob(), []any{map[string]any{
			"containerPort": int64(80), "hostPort": int64(80)}}, "ports"),
		"init container": setJobField(t, codeExecJob(), []any{map[string]any{
			"name": "init", "image": "busybox", "securityContext": map[string]any{"privileged": true}}}, "initContainers"),
	} {
		t.Run(name, func(t *testing.T) {
			require.False(t, admits(aiWorkerIdentityTestWorker, job))
			require.True(t, admits(trustedControllerUser, job), "only the AI worker is constrained")
		})
	}
}
