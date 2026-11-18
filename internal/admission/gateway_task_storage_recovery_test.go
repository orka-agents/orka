/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package admission

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	apiserveradmission "k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/authorization/authorizer"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

// rbacGrant allows one user one verb on one resource, in one namespace and,
// when name is set, on one named object only, as a Kubernetes RBAC rule does.
// The verb "*" allows every verb.
type rbacGrant struct {
	user, verb, group, resource, subresource, namespace, name string
}

// grantAuthorizer allows exactly the requests one of its grants matches.
type grantAuthorizer []rbacGrant

func (a grantAuthorizer) Authorize(_ context.Context, attributes authorizer.Attributes) (authorizer.Decision, string, error) {
	for _, grant := range a {
		if attributes.IsResourceRequest() && attributes.GetUser().GetName() == grant.user &&
			(grant.verb == "*" || attributes.GetVerb() == grant.verb) &&
			attributes.GetAPIGroup() == grant.group && attributes.GetResource() == grant.resource &&
			attributes.GetSubresource() == grant.subresource && attributes.GetNamespace() == grant.namespace &&
			(grant.name == "" || attributes.GetName() == grant.name) {
			return authorizer.DecisionAllow, "", nil
		}
	}
	return authorizer.DecisionNoOpinion, "", nil
}

const (
	// taskStatusUpdater may manage Tasks and update their status in the test
	// namespace, as an operator can.
	taskStatusUpdater = "operator@example.com"
	// taskEditor may manage Tasks in the test namespace and read their
	// status, as Orka's Task admin role allows, but not update it.
	taskEditor = "task-admin@example.com"
	// elsewhereStatusUpdater may update Task status only in another namespace.
	elsewhereStatusUpdater = "elsewhere@example.com"
	// thisTaskStatusUpdater may update the status of the test Task only.
	thisTaskStatusUpdater = "this-task@example.com"
	// otherTaskStatusUpdater may update the status of another Task only.
	otherTaskStatusUpdater = "other-task@example.com"
)

func storageRecoveryAuthorizer() grantAuthorizer {
	group := corev1alpha1.GroupVersion.Group
	var grants grantAuthorizer
	for _, user := range []string{taskStatusUpdater, taskEditor, elsewhereStatusUpdater, thisTaskStatusUpdater, otherTaskStatusUpdater} {
		grants = append(grants,
			rbacGrant{user: user, verb: "*", group: group, resource: "tasks", namespace: admissionTestNamespace},
			rbacGrant{user: user, verb: "get", group: group, resource: "tasks", subresource: "status", namespace: admissionTestNamespace})
	}
	statusUpdate := func(user, namespace, name string) rbacGrant {
		return rbacGrant{user: user, verb: "update", group: group, resource: "tasks", subresource: "status", namespace: namespace, name: name}
	}
	return append(grants,
		statusUpdate(taskStatusUpdater, admissionTestNamespace, ""),
		statusUpdate(elsewhereStatusUpdater, "other-namespace", ""),
		statusUpdate(thisTaskStatusUpdater, admissionTestNamespace, admissionTestTaskName),
		statusUpdate(otherTaskStatusUpdater, admissionTestNamespace, "other-task"),
	)
}

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

// A user who may update a Task's status can release a Task storage hold on a
// Gateway-owned Task, and only by changing that one annotation. Permission to
// update the Task alone is not enough.
func TestGatewayTaskProtectionAllowsOnlyStorageRecoveryAcknowledgement(t *testing.T) {
	policy := compileGatewayTaskProtection(t).withAuthorizer(storageRecoveryAuthorizer())
	allowsUser := func(username string, newTask, oldTask *unstructured.Unstructured, subresource string) bool {
		return policy.allows(t, apiserveradmission.Update, username, subresource, newTask, oldTask)
	}
	allows := func(newTask, oldTask *unstructured.Unstructured, subresource string) bool {
		return allowsUser(taskStatusUpdater, newTask, oldTask, subresource)
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

	t.Run("by who may update this Task's status", func(t *testing.T) {
		oldTask := gatewayPolicyTask()
		newTask := acknowledgedGatewayTask(oldTask, "record-0123456789abcdef")
		require.True(t, allowsUser(thisTaskStatusUpdater, newTask, oldTask, ""))
	})
	for name, username := range map[string]string{
		"who may update Tasks but not their status":            taskEditor,
		"who may update Task status only in another namespace": elsewhereStatusUpdater,
		"who may update only another Task's status":            otherTaskStatusUpdater,
		"with no Task permissions":                             untrustedUsername,
	} {
		t.Run("refused for a user "+name, func(t *testing.T) {
			oldTask := gatewayPolicyTask()
			newTask := acknowledgedGatewayTask(oldTask, "record-0123456789abcdef")
			require.False(t, allowsUser(username, newTask, oldTask, ""))
		})
	}

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
		require.False(t, policy.allows(t, apiserveradmission.Create, taskStatusUpdater, "", task, nil))
		require.False(t, policy.allows(t, apiserveradmission.Delete, taskStatusUpdater, "", nil, task))
	})
}

// failingAuthorizer cannot reach a decision.
type failingAuthorizer struct{}

func (failingAuthorizer) Authorize(context.Context, authorizer.Attributes) (authorizer.Decision, string, error) {
	return authorizer.DecisionNoOpinion, "", errors.New("authorizer unavailable")
}

// When the status check cannot be answered, the acknowledgement is refused,
// and the controller and trusted workers keep their access.
func TestGatewayTaskStorageRecoveryAcknowledgementFailsClosed(t *testing.T) {
	policy := compileGatewayTaskProtection(t).withAuthorizer(failingAuthorizer{})
	oldTask := gatewayPolicyTask()
	newTask := acknowledgedGatewayTask(oldTask, "record-0123456789abcdef")

	require.False(t, policy.allows(t, apiserveradmission.Update, taskStatusUpdater, "", newTask, oldTask))
	for _, username := range []string{trustedControllerUser, trustedWorkerUser} {
		require.True(t, policy.allows(t, apiserveradmission.Update, username, "", newTask, oldTask), username)
	}
}
