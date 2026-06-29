/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package admission

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/labels"
)

const (
	admissionTestNamespace     = "tenant-a"
	admissionTestTaskName      = "admission-task"
	untrustedUsername          = "system:serviceaccount:tenant-a:tenant-user"
	trustedControllerUser      = "system:serviceaccount:orka-system:orka-controller-manager"
	trustedWorkerUser          = "system:serviceaccount:tenant-a:orka-ai-worker"
	admissionTestTransactionID = "txn-1"
)

func TestTaskProvenanceValidator_Create(t *testing.T) {
	validator := newTestTaskProvenanceValidator(t)

	tests := []struct {
		name     string
		user     string
		task     *corev1alpha1.Task
		allowed  bool
		contains string
	}{
		{
			name:    "untrusted create without provenance allowed",
			user:    untrustedUsername,
			task:    newAdmissionTestTask(),
			allowed: true,
		},
		{
			name:     "untrusted create with requestedBy denied",
			user:     untrustedUsername,
			task:     withRequestedBy(newAdmissionTestTask()),
			contains: fieldSpecRequestedBy,
		},
		{
			name:     "untrusted create with transaction denied",
			user:     untrustedUsername,
			task:     withTransaction(newAdmissionTestTask()),
			contains: fieldSpecTransaction,
		},
		{
			name:     "untrusted create with transaction metadata denied",
			user:     untrustedUsername,
			task:     withTransactionMetadata(newAdmissionTestTask()),
			contains: labels.LabelTransactionID,
		},
		{
			name:     "untrusted create with transaction token pending annotation denied",
			user:     untrustedUsername,
			task:     withTransactionTokenPending(newAdmissionTestTask()),
			contains: labels.AnnotationTransactionTokenPending,
		},
		{
			name:     "untrusted create with trace annotation denied",
			user:     untrustedUsername,
			task:     withTraceAnnotation(newAdmissionTestTask(), "00-"+strings.Repeat("1", 32)+"-"+strings.Repeat("2", 16)+"-01"),
			contains: labels.AnnotationTraceParent,
		},
		{
			name:    "trusted controller can create with provenance",
			user:    trustedControllerUser,
			task:    withTransaction(withRequestedBy(newAdmissionTestTask())),
			allowed: true,
		},
		{
			name:    "trusted namespace worker can create child with provenance",
			user:    trustedWorkerUser,
			task:    withTransactionMetadata(withTransaction(newAdmissionTestTask())),
			allowed: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := validator.Handle(context.Background(), admissionRequest(t, admissionv1.Create, tt.user, tt.task, nil, ""))
			require.Equal(t, tt.allowed, resp.Allowed)
			if tt.contains != "" {
				require.Contains(t, resp.Result.Message, tt.contains)
			}
		})
	}
}

func TestTaskProvenanceValidator_Update(t *testing.T) {
	validator := newTestTaskProvenanceValidator(t)

	oldTask := newAdmissionTestTask()
	oldWithProvenance := withTransactionMetadata(withTransaction(withRequestedBy(newAdmissionTestTask())))

	tests := []struct {
		name        string
		user        string
		oldTask     *corev1alpha1.Task
		newTask     *corev1alpha1.Task
		subresource string
		allowed     bool
		contains    string
	}{
		{
			name:    "untrusted update without provenance changes allowed",
			user:    untrustedUsername,
			oldTask: oldTask,
			newTask: withImage(oldTask.DeepCopy(), "alpine"),
			allowed: true,
		},
		{
			name:     "untrusted update adding requestedBy denied",
			user:     untrustedUsername,
			oldTask:  oldTask,
			newTask:  withRequestedBy(oldTask.DeepCopy()),
			contains: fieldSpecRequestedBy,
		},
		{
			name:     "untrusted update changing transaction denied",
			user:     untrustedUsername,
			oldTask:  oldWithProvenance,
			newTask:  withTransactionID(oldWithProvenance.DeepCopy(), "txn-2"),
			contains: fieldSpecTransaction,
		},
		{
			name:     "untrusted update changing transaction annotation denied",
			user:     untrustedUsername,
			oldTask:  oldWithProvenance,
			newTask:  withTransactionAnnotation(oldWithProvenance.DeepCopy(), "txn-2"),
			contains: labels.AnnotationTransactionID,
		},
		{
			name:     "untrusted update adding transaction token pending annotation denied",
			user:     untrustedUsername,
			oldTask:  oldTask,
			newTask:  withTransactionTokenPending(oldTask.DeepCopy()),
			contains: labels.AnnotationTransactionTokenPending,
		},
		{
			name:     "untrusted update adding trace annotation denied",
			user:     untrustedUsername,
			oldTask:  oldTask,
			newTask:  withTraceAnnotation(oldTask.DeepCopy(), "00-"+strings.Repeat("3", 32)+"-"+strings.Repeat("4", 16)+"-01"),
			contains: labels.AnnotationTraceParent,
		},
		{
			name:     "untrusted update adding harness runtime annotation denied",
			user:     untrustedUsername,
			oldTask:  oldTask,
			newTask:  withHarnessRuntimeAnnotation(oldTask.DeepCopy()),
			contains: labels.AnnotationHarnessRuntimeSession,
		},
		{
			name:    "trusted controller can update provenance",
			user:    trustedControllerUser,
			oldTask: oldTask,
			newTask: withTransaction(withRequestedBy(oldTask.DeepCopy())),
			allowed: true,
		},
		{
			name:        "status subresource update allowed",
			user:        untrustedUsername,
			oldTask:     oldTask,
			newTask:     withTransaction(oldTask.DeepCopy()),
			subresource: "status",
			allowed:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := validator.Handle(context.Background(), admissionRequest(t, admissionv1.Update, tt.user, tt.newTask, tt.oldTask, tt.subresource))
			require.Equal(t, tt.allowed, resp.Allowed)
			if tt.contains != "" {
				require.Contains(t, resp.Result.Message, tt.contains)
			}
		})
	}
}

func newTestTaskProvenanceValidator(t *testing.T) *TaskProvenanceValidator {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	return NewTaskProvenanceValidator(scheme, NewTaskProvenanceConfig(true, "", "", "orka-system"))
}

func admissionRequest(
	t *testing.T,
	operation admissionv1.Operation,
	username string,
	task *corev1alpha1.Task,
	oldTask *corev1alpha1.Task,
	subresource string,
) ctrladmission.Request {
	t.Helper()
	req := ctrladmission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation:   operation,
		Namespace:   admissionTestNamespace,
		SubResource: subresource,
		UserInfo: authenticationv1.UserInfo{
			Username: username,
		},
		Object: runtime.RawExtension{Raw: mustMarshalAdmissionTask(t, task)},
	}}
	if oldTask != nil {
		req.OldObject = runtime.RawExtension{Raw: mustMarshalAdmissionTask(t, oldTask)}
	}
	return req
}

func mustMarshalAdmissionTask(t *testing.T, task *corev1alpha1.Task) []byte {
	t.Helper()
	copy := task.DeepCopy()
	copy.TypeMeta = metav1.TypeMeta{
		APIVersion: corev1alpha1.GroupVersion.String(),
		Kind:       "Task",
	}
	data, err := json.Marshal(copy)
	require.NoError(t, err)
	return data
}

func newAdmissionTestTask() *corev1alpha1.Task {
	return &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      admissionTestTaskName,
			Namespace: admissionTestNamespace,
		},
		Spec: corev1alpha1.TaskSpec{
			Type:  corev1alpha1.TaskTypeContainer,
			Image: "busybox",
		},
	}
}

func withRequestedBy(task *corev1alpha1.Task) *corev1alpha1.Task {
	task.Spec.RequestedBy = &corev1alpha1.RequestedBy{Subject: "subject"}
	return task
}

func withTransaction(task *corev1alpha1.Task) *corev1alpha1.Task {
	task.Spec.Transaction = &corev1alpha1.TaskTransaction{ID: admissionTestTransactionID, Scope: "orka:tasks:create"}
	return task
}

func withTransactionID(task *corev1alpha1.Task, id string) *corev1alpha1.Task {
	task.Spec.Transaction.ID = id
	return task
}

func withTransactionMetadata(task *corev1alpha1.Task) *corev1alpha1.Task {
	if task.Labels == nil {
		task.Labels = map[string]string{}
	}
	if task.Annotations == nil {
		task.Annotations = map[string]string{}
	}
	task.Labels[labels.LabelTransactionID] = admissionTestTransactionID
	task.Annotations[labels.AnnotationTransactionID] = admissionTestTransactionID
	return task
}

func withTransactionAnnotation(task *corev1alpha1.Task, id string) *corev1alpha1.Task {
	if task.Annotations == nil {
		task.Annotations = map[string]string{}
	}
	task.Annotations[labels.AnnotationTransactionID] = id
	return task
}

func withTransactionTokenPending(task *corev1alpha1.Task) *corev1alpha1.Task {
	if task.Annotations == nil {
		task.Annotations = map[string]string{}
	}
	task.Annotations[labels.AnnotationTransactionTokenPending] = "true"
	task.Annotations[labels.AnnotationTransactionTokenPendingSince] = "2026-01-01T00:00:00Z"
	return task
}

func withTraceAnnotation(task *corev1alpha1.Task, traceparent string) *corev1alpha1.Task {
	if task.Annotations == nil {
		task.Annotations = map[string]string{}
	}
	task.Annotations[labels.AnnotationTraceParent] = traceparent
	task.Annotations[labels.AnnotationTraceBaggage] = "tenant=untrusted"
	return task
}

func withImage(task *corev1alpha1.Task, image string) *corev1alpha1.Task {
	task.Spec.Image = image
	return task
}

func withHarnessRuntimeAnnotation(task *corev1alpha1.Task) *corev1alpha1.Task {
	if task.Annotations == nil {
		task.Annotations = map[string]string{}
	}
	task.Annotations[labels.AnnotationHarnessRuntimeSession] = "runtime-a"
	return task
}

func TestNewTaskProvenanceConfigDefaults(t *testing.T) {
	cfg := NewTaskProvenanceConfig(true, "", "", "orka-system")
	require.True(t, cfg.Enabled)
	require.Contains(t, cfg.TrustedUsernames, trustedControllerUser)
	require.ElementsMatch(t, []string{"orka-ai-worker", "orka-vendor-worker"}, cfg.TrustedServiceAccountNames)
}
