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
	"k8s.io/apiserver/pkg/authentication/user"
)

const (
	protectedControllerSA         = "orka-controller-manager"
	protectedControllerDeployment = "orka-controller"
	aiWorkerUsername              = "system:serviceaccount:tenant-a:orka-ai-worker"
	replicaSetControllerUsername  = "system:serviceaccount:kube-system:replicaset-controller"
	deploymentControllerUsername  = "system:serviceaccount:kube-system:deployment-controller"
)

func compileControllerServiceAccountProtection(t *testing.T, namespace, serviceAccount string) admissionPolicyEvaluator {
	t.Helper()
	return compileAdmissionPolicy(t, "controller_serviceaccount_protection.yaml", strings.NewReplacer(
		"ORKA_NAMESPACE", namespace,
		"CONTROLLER_SA", serviceAccount,
		"CONTROLLER_DEPLOYMENT", protectedControllerDeployment,
	))
}

var controllerSAResources = map[string]schema.GroupVersionResource{
	"Pod":                   {Version: "v1", Resource: "pods"},
	"ReplicationController": {Version: "v1", Resource: "replicationcontrollers"},
	"Secret":                {Version: "v1", Resource: "secrets"},
	"Deployment":            {Group: "apps", Version: "v1", Resource: "deployments"},
	"ReplicaSet":            {Group: "apps", Version: "v1", Resource: "replicasets"},
	"StatefulSet":           {Group: "apps", Version: "v1", Resource: "statefulsets"},
	"DaemonSet":             {Group: "apps", Version: "v1", Resource: "daemonsets"},
	"Job":                   {Group: "batch", Version: "v1", Resource: "jobs"},
	"CronJob":               {Group: "batch", Version: "v1", Resource: "cronjobs"},
}

// controllerSAWorkload builds a workload of kind whose Pod spec sets field to
// serviceAccount; an empty field names no account.
func controllerSAWorkload(kind, name, field, serviceAccount string) *unstructured.Unstructured {
	podSpec := map[string]any{"containers": []any{map[string]any{"name": "c", "image": "busybox"}}}
	if field != "" {
		podSpec[field] = serviceAccount
	}
	gvr := controllerSAResources[kind]
	apiVersion := gvr.Version
	if gvr.Group != "" {
		apiVersion = gvr.Group + "/" + gvr.Version
	}
	object := map[string]any{
		"apiVersion": apiVersion, "kind": kind,
		"metadata": map[string]any{"name": name, "namespace": admissionTestNamespace},
	}
	template := map[string]any{"template": map[string]any{"spec": podSpec}}
	switch kind {
	case "Pod":
		object["spec"] = podSpec
	case "CronJob":
		object["spec"] = map[string]any{"schedule": "* * * * *", "jobTemplate": map[string]any{"spec": template}}
	default:
		object["spec"] = template
	}
	return &unstructured.Unstructured{Object: object}
}

func serviceAccountTokenSecret(serviceAccount, secretType string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Secret", "type": secretType,
		"metadata": map[string]any{
			"name": "s", "namespace": admissionTestNamespace,
			"annotations": map[string]any{"kubernetes.io/service-account.name": serviceAccount},
		},
	}}
}

func withReplicas(object *unstructured.Unstructured, replicas int64) *unstructured.Unstructured {
	object.Object["spec"].(map[string]any)["replicas"] = replicas
	return object
}

func withTemplateImage(object *unstructured.Unstructured, image string) *unstructured.Unstructured {
	spec := object.Object["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	spec["containers"] = []any{map[string]any{"name": "c", "image": image}}
	return object
}

func withReplicaSetOwner(object *unstructured.Unstructured) *unstructured.Unstructured {
	object.Object["metadata"].(map[string]any)["ownerReferences"] = []any{map[string]any{
		"apiVersion": "apps/v1", "kind": "ReplicaSet", "name": "orka-controller-abc", "uid": "rs-uid", "controller": true,
	}}
	return object
}

func TestControllerServiceAccountProtection(t *testing.T) {
	policy := compileControllerServiceAccountProtection(t, admissionTestNamespace, protectedControllerSA)
	const tokenType = "kubernetes.io/service-account-token"
	asController := func(kind, name string) *unstructured.Unstructured {
		return controllerSAWorkload(kind, name, "serviceAccountName", protectedControllerSA)
	}
	tests := []struct {
		name      string
		username  string
		object    *unstructured.Unstructured
		oldObject *unstructured.Unstructured
		allowed   bool
	}{
		// Pods: only the ReplicaSet controller, whatever owner reference a
		// creator writes.
		{name: "controller Pod from the ReplicaSet controller", username: replicaSetControllerUsername, object: asController("Pod", "p"), allowed: true},
		{name: "Pod with a forged ReplicaSet owner", username: aiWorkerUsername, object: withReplicaSetOwner(asController("Pod", "p"))},
		{name: "bare Pod as the controller", username: aiWorkerUsername, object: asController("Pod", "p")},
		{name: "Job controller Pod as the controller", username: "system:serviceaccount:kube-system:job-controller", object: asController("Pod", "p")},
		{name: "Pod via the deprecated field", username: aiWorkerUsername, object: controllerSAWorkload("Pod", "p", "serviceAccount", protectedControllerSA)},
		{name: "Pod with its own account", username: aiWorkerUsername, object: controllerSAWorkload("Pod", "p", "serviceAccountName", "other"), allowed: true},
		// ReplicaSets: only the Deployment controller creates them.
		{name: "controller ReplicaSet from the Deployment controller", username: deploymentControllerUsername, object: asController("ReplicaSet", "orka-controller-abc"), allowed: true},
		{name: "ReplicaSet created by someone else", username: aiWorkerUsername, object: asController("ReplicaSet", "rs")},
		{
			name: "update of a ReplicaSet already running as the controller", username: "system:serviceaccount:kube-system:generic-garbage-collector",
			object: asController("ReplicaSet", "orka-controller-abc"), oldObject: asController("ReplicaSet", "orka-controller-abc"), allowed: true,
		},
		{
			name: "controller ReplicaSet scaled without touching its template", username: "namespace-editor",
			object: withReplicas(asController("ReplicaSet", "orka-controller-abc"), 2), oldObject: asController("ReplicaSet", "orka-controller-abc"), allowed: true,
		},
		{
			name: "controller ReplicaSet template changed by someone else", username: "namespace-editor",
			object: withTemplateImage(asController("ReplicaSet", "orka-controller-abc"), "attacker"), oldObject: asController("ReplicaSet", "orka-controller-abc"),
		},
		{
			name: "controller ReplicaSet template changed by the Deployment controller", username: deploymentControllerUsername,
			object: withTemplateImage(asController("ReplicaSet", "orka-controller-abc"), "next"), oldObject: asController("ReplicaSet", "orka-controller-abc"), allowed: true,
		},
		{
			name: "ReplicaSet retargeted to the controller", username: aiWorkerUsername,
			object: asController("ReplicaSet", "rs"), oldObject: controllerSAWorkload("ReplicaSet", "rs", "serviceAccountName", "other"),
		},
		// Deployments: only the release's controller Deployment.
		{name: "controller Deployment", username: "helm-user", object: asController("Deployment", protectedControllerDeployment), allowed: true},
		{name: "another Deployment as the controller", username: "helm-user", object: asController("Deployment", "lookalike")},
		{name: "another Deployment with its own account", username: "helm-user", object: controllerSAWorkload("Deployment", "app", "serviceAccountName", "app"), allowed: true},
		// Every other workload kind.
		{name: "worker Job as the controller", username: aiWorkerUsername, object: asController("Job", "j")},
		{name: "CronJob as the controller", username: aiWorkerUsername, object: asController("CronJob", "c")},
		{name: "StatefulSet as the controller", username: aiWorkerUsername, object: asController("StatefulSet", "s")},
		{name: "DaemonSet as the controller", username: aiWorkerUsername, object: asController("DaemonSet", "d")},
		{name: "ReplicationController as the controller", username: aiWorkerUsername, object: asController("ReplicationController", "r")},
		{name: "code_exec Job with its own account", username: aiWorkerUsername, object: controllerSAWorkload("Job", "j", "serviceAccountName", "code-exec-run"), allowed: true},
		{name: "Job with the default account", username: aiWorkerUsername, object: controllerSAWorkload("Job", "j", "", ""), allowed: true},
		// ServiceAccount token Secrets.
		{name: "token Secret for the controller", username: aiWorkerUsername, object: serviceAccountTokenSecret(protectedControllerSA, tokenType)},
		{
			name: "token Secret retargeted to the controller", username: aiWorkerUsername,
			object: serviceAccountTokenSecret(protectedControllerSA, tokenType), oldObject: serviceAccountTokenSecret("code-exec-run", tokenType),
		},
		{
			name: "existing controller token Secret populated", username: "system:kube-controller-manager",
			object: serviceAccountTokenSecret(protectedControllerSA, tokenType), oldObject: serviceAccountTokenSecret(protectedControllerSA, tokenType), allowed: true,
		},
		{name: "token Secret for another account", username: aiWorkerUsername, object: serviceAccountTokenSecret("code-exec-run", tokenType), allowed: true},
		{name: "opaque Secret naming the controller", username: aiWorkerUsername, object: serviceAccountTokenSecret(protectedControllerSA, "Opaque"), allowed: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			operation := apiserveradmission.Create
			if test.oldObject != nil {
				operation = apiserveradmission.Update
			}
			kind := test.object.GroupVersionKind()
			require.Equal(t, test.allowed, policy.admits(t, kind, controllerSAResources[kind.Kind], test.object.GetName(), operation,
				test.username, "", test.object, test.oldObject))
		})
	}

	// Outside the controller's namespace the controller ServiceAccount does
	// not exist, and the policy leaves workloads alone.
	elsewhere := compileControllerServiceAccountProtection(t, "orka-system", protectedControllerSA)
	job := asController("Job", "j")
	require.True(t, elsewhere.admits(t, job.GroupVersionKind(), controllerSAResources["Job"], "j", apiserveradmission.Create,
		aiWorkerUsername, "", job, nil))
}

// A running controller Pod keeps its spec: an image swap or an ephemeral
// container would run other code with its projected token.
func TestControllerServiceAccountProtectionFreezesControllerPods(t *testing.T) {
	policy := compileControllerServiceAccountProtection(t, admissionTestNamespace, protectedControllerSA)
	pods := controllerSAResources["Pod"]
	running := func() *unstructured.Unstructured {
		return withReplicaSetOwner(controllerSAWorkload("Pod", "orka-controller-abc-xyz", "serviceAccountName", protectedControllerSA))
	}
	labelled := running()
	labelled.SetLabels(map[string]string{"debug": "true"})
	swapped := running()
	swapped.Object["spec"].(map[string]any)["containers"] = []any{map[string]any{"name": "c", "image": "attacker"}}
	debugged := running()
	debugged.Object["spec"].(map[string]any)["ephemeralContainers"] = []any{map[string]any{
		"name": "debugger", "image": "busybox", "targetContainerName": "c",
	}}
	other := controllerSAWorkload("Pod", "app", "serviceAccountName", "app")
	otherSwapped := controllerSAWorkload("Pod", "app", "serviceAccountName", "app")
	otherSwapped.Object["spec"].(map[string]any)["containers"] = []any{map[string]any{"name": "c", "image": "next"}}
	for name, tc := range map[string]struct {
		subresource string
		object, old *unstructured.Unstructured
		allowed     bool
	}{
		"metadata update":                       {object: labelled, old: running(), allowed: true},
		"image swap":                            {object: swapped, old: running()},
		"ephemeral container":                   {subresource: "ephemeralcontainers", object: debugged, old: running()},
		"image change on another account's Pod": {object: otherSwapped, old: other, allowed: true},
		"ephemeral container on another Pod":    {subresource: "ephemeralcontainers", object: otherSwapped, old: other, allowed: true},
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.allowed, policy.admits(t, tc.object.GroupVersionKind(), pods, tc.object.GetName(),
				apiserveradmission.Update, "namespace-editor", tc.subresource, tc.object, tc.old))
		})
	}
}

func TestControllerServiceAccountProtectionAdmitsOnlyTheKubeletTokenRequests(t *testing.T) {
	policy := compileControllerServiceAccountProtection(t, admissionTestNamespace, protectedControllerSA)
	kind := schema.GroupVersionKind{Group: "authentication.k8s.io", Version: "v1", Kind: "TokenRequest"}
	resource := schema.GroupVersionResource{Version: "v1", Resource: "serviceaccounts"}
	request := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "authentication.k8s.io/v1", "kind": "TokenRequest",
		"spec": map[string]any{"audiences": []any{"api://AzureADTokenExchange"}},
	}}
	kubelet := &user.DefaultInfo{Name: "system:node:node-a", Groups: []string{"system:nodes", "system:authenticated"}}
	worker := &user.DefaultInfo{Name: aiWorkerUsername, Groups: []string{"system:serviceaccounts", "system:authenticated"}}
	controller := &user.DefaultInfo{Name: "system:serviceaccount:" + admissionTestNamespace + ":" + protectedControllerSA}
	for name, tc := range map[string]struct {
		requester user.Info
		account   string
		allowed   bool
	}{
		"kubelet for the controller Pod":    {requester: kubelet, account: protectedControllerSA, allowed: true},
		"worker for the controller":         {requester: worker, account: protectedControllerSA},
		"controller for itself":             {requester: controller, account: protectedControllerSA},
		"worker for another account":        {requester: worker, account: "code-exec-run", allowed: true},
		"controller for a policy's account": {requester: controller, account: "workload", allowed: true},
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.allowed, policy.admitsAs(t, kind, resource, tc.account, apiserveradmission.Create,
				tc.requester, "token", request, nil))
		})
	}
}

func TestControllerServiceAccountProtectionTreatsDefaultAsAnAccount(t *testing.T) {
	policy := compileControllerServiceAccountProtection(t, admissionTestNamespace, "default")
	job := controllerSAWorkload("Job", "j", "", "")
	require.False(t, policy.admits(t, job.GroupVersionKind(), controllerSAResources["Job"],
		"j", apiserveradmission.Create, aiWorkerUsername, "", job, nil),
		"a controller running as default must not be impersonated by a Job that names no account")
}
