/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package admission

import (
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	apiserveradmission "k8s.io/apiserver/pkg/admission"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

// acknowledgedGatewayTask returns oldTask with the storage recovery
// acknowledgement annotation set to holdID.
func acknowledgedGatewayTask(oldTask *unstructured.Unstructured, holdID string) *unstructured.Unstructured {
	newTask := oldTask.DeepCopy()
	annotations := newTask.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[corev1alpha1.StorageRecoveryAcknowledgedAnnotation] = holdID
	newTask.SetAnnotations(annotations)
	return newTask
}

// Any user Kubernetes RBAC lets update a Task can release a Task storage hold
// on a Gateway-owned Task, and only by changing that one annotation.
func TestGatewayTaskProtectionAllowsOnlyStorageRecoveryAcknowledgement(t *testing.T) {
	policy := compileGatewayTaskProtection(t)
	allows := func(newTask, oldTask *unstructured.Unstructured, subresource string) bool {
		return policy.allows(t, apiserveradmission.Update, untrustedUsername, subresource, newTask, oldTask)
	}

	t.Run("setting the annotation", func(t *testing.T) {
		oldTask := gatewayPolicyTask()
		require.True(t, allows(acknowledgedGatewayTask(oldTask, "record-0123456789abcdef"), oldTask, ""))
	})
	t.Run("changing the annotation", func(t *testing.T) {
		oldTask := acknowledgedGatewayTask(gatewayPolicyTask(), "record-0000000000000000")
		require.True(t, allows(acknowledgedGatewayTask(oldTask, "record-0123456789abcdef"), oldTask, ""))
	})
	t.Run("a Task without annotations", func(t *testing.T) {
		oldTask := gatewayPolicyTask()
		unstructured.RemoveNestedField(oldTask.Object, "metadata", "annotations")
		require.True(t, allows(acknowledgedGatewayTask(oldTask, "record-0123456789abcdef"), oldTask, ""))
	})
	t.Run("managed fields rewritten by the API server", func(t *testing.T) {
		oldTask := gatewayPolicyTask()
		newTask := acknowledgedGatewayTask(oldTask, "record-0123456789abcdef")
		unstructured.RemoveNestedField(newTask.Object, "metadata", "managedFields")
		require.True(t, allows(newTask, oldTask, ""))
	})

	for name, change := range map[string]func(*unstructured.Unstructured){
		"another annotation changed": func(task *unstructured.Unstructured) {
			annotations := task.GetAnnotations()
			annotations["example.org/preserved"] = "changed"
			task.SetAnnotations(annotations)
		},
		"another annotation added": func(task *unstructured.Unstructured) {
			annotations := task.GetAnnotations()
			annotations["example.org/added"] = "value"
			task.SetAnnotations(annotations)
		},
		"another annotation removed": func(task *unstructured.Unstructured) {
			annotations := task.GetAnnotations()
			delete(annotations, "example.org/preserved")
			task.SetAnnotations(annotations)
		},
		"a label changed": func(task *unstructured.Unstructured) {
			task.SetLabels(map[string]string{"gateway.orka.ai/gateway": "other"})
		},
		"a finalizer removed": func(task *unstructured.Unstructured) {
			task.SetFinalizers([]string{"orka.ai/cleanup"})
		},
		"the spec changed": func(task *unstructured.Unstructured) {
			require.NoError(t, unstructured.SetNestedField(task.Object, "other-image", "spec", "image"))
		},
		"the requester changed": func(task *unstructured.Unstructured) {
			require.NoError(t, unstructured.SetNestedField(task.Object, "someone-else", "spec", "requestedBy", "subject"))
		},
	} {
		t.Run("refused when "+name, func(t *testing.T) {
			oldTask := gatewayPolicyTask()
			newTask := acknowledgedGatewayTask(oldTask, "record-0123456789abcdef")
			change(newTask)
			require.False(t, allows(newTask, oldTask, ""))
		})
	}

	t.Run("refused when the annotation is removed", func(t *testing.T) {
		oldTask := acknowledgedGatewayTask(gatewayPolicyTask(), "record-0123456789abcdef")
		require.False(t, allows(gatewayPolicyTask(), oldTask, ""))
	})
	t.Run("refused through the status subresource", func(t *testing.T) {
		oldTask := gatewayPolicyTask()
		require.False(t, allows(acknowledgedGatewayTask(oldTask, "record-0123456789abcdef"), oldTask, "status"))
	})
	t.Run("does not open creation or deletion", func(t *testing.T) {
		task := acknowledgedGatewayTask(gatewayPolicyTask(), "record-0123456789abcdef")
		require.False(t, policy.allows(t, apiserveradmission.Create, untrustedUsername, "", task, nil))
		require.False(t, policy.allows(t, apiserveradmission.Delete, untrustedUsername, "", nil, task))
	})
}
