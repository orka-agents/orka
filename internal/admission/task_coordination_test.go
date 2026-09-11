package admission

import (
	"testing"

	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
)

func TestTaskProvenanceValidatorCoordinationAncestryImmutable(t *testing.T) {
	validator := newTestTaskProvenanceValidator(t)
	root := newAdmissionTestTask()
	child := root.DeepCopy()
	child.Labels = map[string]string{labels.LabelParentTask: "parent"}
	child.Annotations = map[string]string{labels.AnnotationParentTaskName: "parent"}
	controller := true
	child.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: corev1alpha1.GroupVersion.String(), Kind: "Task",
		Name: "parent", UID: "parent-uid", Controller: &controller,
	}}

	tests := []struct {
		name    string
		oldTask *corev1alpha1.Task
		change  func(*corev1alpha1.Task)
		allowed bool
	}{
		{name: "reparent child", oldTask: child, change: func(task *corev1alpha1.Task) {
			task.Labels[labels.LabelParentTask] = "unrelated"
			task.Annotations[labels.AnnotationParentTaskName] = "unrelated"
			task.OwnerReferences[0].Name = "unrelated"
			task.OwnerReferences[0].UID = "unrelated-uid"
		}},
		{name: "change parent label", oldTask: child, change: func(task *corev1alpha1.Task) {
			task.Labels[labels.LabelParentTask] = "unrelated"
		}},
		{name: "remove parent label", oldTask: child, change: func(task *corev1alpha1.Task) {
			delete(task.Labels, labels.LabelParentTask)
		}},
		{name: "change parent annotation", oldTask: child, change: func(task *corev1alpha1.Task) {
			task.Annotations[labels.AnnotationParentTaskName] = "unrelated"
		}},
		{name: "remove parent annotation", oldTask: child, change: func(task *corev1alpha1.Task) {
			delete(task.Annotations, labels.AnnotationParentTaskName)
		}},
		{name: "change parent UID", oldTask: child, change: func(task *corev1alpha1.Task) {
			task.OwnerReferences[0].UID = "recreated-parent-uid"
		}},
		{name: "change owner name", oldTask: child, change: func(task *corev1alpha1.Task) {
			task.OwnerReferences[0].Name = "unrelated"
		}},
		{name: "change owner kind", oldTask: child, change: func(task *corev1alpha1.Task) {
			task.OwnerReferences[0].Kind = "Agent"
		}},
		{name: "change owner version", oldTask: child, change: func(task *corev1alpha1.Task) {
			task.OwnerReferences[0].APIVersion = "other.example/v1"
		}},
		{name: "remove controller ownership", oldTask: child, change: func(task *corev1alpha1.Task) {
			task.OwnerReferences[0].Controller = nil
		}},
		{name: "remove parent ownership", oldTask: child, change: func(task *corev1alpha1.Task) {
			task.OwnerReferences = nil
		}},
		{name: "attach root to another tree", oldTask: root, change: func(task *corev1alpha1.Task) {
			task.Labels = child.Labels
			task.Annotations = child.Annotations
			task.OwnerReferences = child.OwnerReferences
		}},
		{name: "update other metadata and status", oldTask: child, allowed: true, change: func(task *corev1alpha1.Task) {
			task.Labels["example.com/label"] = "updated"
			task.Annotations["example.com/annotation"] = "updated"
			task.Finalizers = []string{"orka.ai/cleanup"}
			task.Status.Phase = corev1alpha1.TaskPhaseRunning
		}},
		{name: "garbage collector changes deletion blocking", oldTask: child, allowed: true, change: func(task *corev1alpha1.Task) {
			block := false
			task.OwnerReferences[0].BlockOwnerDeletion = &block
		}},
	}

	for _, user := range []string{
		trustedWorkerUser, "system:serviceaccount:" + admissionTestNamespace + ":orka-vendor-worker",
		untrustedUsername, trustedControllerUser,
	} {
		for _, subresource := range []string{"", statusSubresource} {
			for _, tt := range tests {
				t.Run(user+"/"+subresource+"/"+tt.name, func(t *testing.T) {
					updated := tt.oldTask.DeepCopy()
					tt.change(updated)
					resp := validator.Handle(t.Context(), admissionRequest(t, admissionv1.Update, user, updated, tt.oldTask, subresource))
					require.Equal(t, tt.allowed, resp.Allowed)
					if !tt.allowed {
						require.Contains(t, resp.Result.Message, "coordination ancestry is immutable")
					}
				})
			}
		}
	}

	// The controller may establish a delegated child's ancestry at creation.
	resp := validator.Handle(t.Context(), admissionRequest(t, admissionv1.Create, trustedControllerUser, child, nil, ""))
	require.True(t, resp.Allowed)

	for _, user := range []string{genericGarbageCollectorUsername, kubeControllerManagerUsername} {
		t.Run(user+"/orphan dependent", func(t *testing.T) {
			orphaned := child.DeepCopy()
			orphaned.OwnerReferences = nil
			resp := validator.Handle(t.Context(), admissionRequest(t, admissionv1.Update, user, orphaned, child, ""))
			require.True(t, resp.Allowed)
			resp = validator.Handle(t.Context(), admissionRequest(t, admissionv1.Update, user, child, orphaned, ""))
			require.False(t, resp.Allowed, "cleanup cannot establish new coordination ownership")
		})
	}
}
