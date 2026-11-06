/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package admission

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	apiserveradmission "k8s.io/apiserver/pkg/admission"
)

const (
	protectedControllerSA = "orka-controller-manager"
	aiWorkerUsername      = "system:serviceaccount:tenant-a:orka-ai-worker"
)

func compileControllerServiceAccountProtection(t *testing.T, namespace string) admissionPolicyEvaluator {
	t.Helper()
	return compileAdmissionPolicy(t, "controller_serviceaccount_protection.yaml", strings.NewReplacer(
		"ORKA_NAMESPACE", namespace,
		"CONTROLLER_SA", protectedControllerSA,
	))
}

func controllerSAPodSpec(field, serviceAccount string) map[string]any {
	spec := map[string]any{"containers": []any{map[string]any{"name": "c", "image": "busybox"}}}
	if field != "" {
		spec[field] = serviceAccount
	}
	return spec
}

func controllerSAWorkload(kind, field, serviceAccount string) *unstructured.Unstructured {
	podSpec := controllerSAPodSpec(field, serviceAccount)
	object := map[string]any{"metadata": map[string]any{"name": "w", "namespace": admissionTestNamespace}}
	switch kind {
	case "Pod":
		object["apiVersion"], object["kind"], object["spec"] = "v1", kind, podSpec
	case "Job":
		object["apiVersion"], object["kind"] = "batch/v1", kind
		object["spec"] = map[string]any{"template": map[string]any{"spec": podSpec}}
	case "CronJob":
		object["apiVersion"], object["kind"] = "batch/v1", kind
		object["spec"] = map[string]any{"schedule": "* * * * *", "jobTemplate": map[string]any{
			"spec": map[string]any{"template": map[string]any{"spec": podSpec}},
		}}
	}
	return &unstructured.Unstructured{Object: object}
}

func ownedBy(object *unstructured.Unstructured, apiVersion, kind string) *unstructured.Unstructured {
	object.Object["metadata"].(map[string]any)["ownerReferences"] = []any{map[string]any{
		"apiVersion": apiVersion, "kind": kind, "name": "owner", "uid": "owner-uid", "controller": true,
	}}
	return object
}

func TestControllerServiceAccountProtection(t *testing.T) {
	policy := compileControllerServiceAccountProtection(t, admissionTestNamespace)
	resources := map[string]schema.GroupVersionResource{
		"Pod":     {Version: "v1", Resource: "pods"},
		"Job":     {Group: "batch", Version: "v1", Resource: "jobs"},
		"CronJob": {Group: "batch", Version: "v1", Resource: "cronjobs"},
	}
	tests := []struct {
		name      string
		object    *unstructured.Unstructured
		operation apiserveradmission.Operation
		allowed   bool
	}{
		{name: "worker Job as the controller", object: controllerSAWorkload("Job", "serviceAccountName", protectedControllerSA)},
		{name: "worker Job via the deprecated field", object: controllerSAWorkload("Job", "serviceAccount", protectedControllerSA)},
		{name: "Job updated to the controller", object: controllerSAWorkload("Job", "serviceAccountName", protectedControllerSA), operation: apiserveradmission.Update},
		{name: "CronJob as the controller", object: controllerSAWorkload("CronJob", "serviceAccountName", protectedControllerSA)},
		{name: "Job-owned Pod as the controller", object: ownedBy(controllerSAWorkload("Pod", "serviceAccountName", protectedControllerSA), "batch/v1", "Job")},
		{name: "bare Pod as the controller", object: controllerSAWorkload("Pod", "serviceAccountName", protectedControllerSA)},
		{name: "controller Deployment Pod", object: ownedBy(controllerSAWorkload("Pod", "serviceAccountName", protectedControllerSA), "apps/v1", "ReplicaSet"), allowed: true},
		{name: "code_exec Job with its own account", object: controllerSAWorkload("Job", "serviceAccountName", "code-exec-run"), allowed: true},
		{name: "Job with the default account", object: controllerSAWorkload("Job", "", ""), allowed: true},
		{name: "Pod with its own account", object: controllerSAWorkload("Pod", "serviceAccountName", "other"), allowed: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			operation := test.operation
			if operation == "" {
				operation = apiserveradmission.Create
			}
			kind := test.object.GroupVersionKind()
			var old *unstructured.Unstructured
			if operation == apiserveradmission.Update {
				old = controllerSAWorkload(kind.Kind, "serviceAccountName", "code-exec-run")
			}
			require.Equal(t, test.allowed, policy.admits(t, kind, resources[kind.Kind], "w", operation,
				aiWorkerUsername, "", test.object, old))
		})
	}

	// Outside the controller's namespace the controller ServiceAccount does
	// not exist, and the policy leaves workloads alone.
	elsewhere := compileControllerServiceAccountProtection(t, "orka-system")
	job := controllerSAWorkload("Job", "serviceAccountName", protectedControllerSA)
	require.True(t, elsewhere.admits(t, job.GroupVersionKind(), resources["Job"], "w", apiserveradmission.Create,
		aiWorkerUsername, "", job, nil))
}

func TestControllerServiceAccountProtectionTreatsDefaultAsAnAccount(t *testing.T) {
	policy := compileAdmissionPolicy(t, "controller_serviceaccount_protection.yaml", strings.NewReplacer(
		"ORKA_NAMESPACE", admissionTestNamespace,
		"CONTROLLER_SA", "default",
	))
	job := controllerSAWorkload("Job", "", "")
	require.False(t, policy.admits(t, job.GroupVersionKind(), schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"},
		"w", apiserveradmission.Create, aiWorkerUsername, "", job, nil),
		"a controller running as default must not be impersonated by a Job that names no account")
}
