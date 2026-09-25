package admission

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
)

func TestTaskProvenanceValidatorChildCreationRequiresCallerBinding(t *testing.T) {
	validator := newTestTaskProvenanceValidator(t)
	child := newAdmissionTestTask()
	child.Labels = map[string]string{labels.LabelParentTask: "unrelated-root"}
	child.Annotations = map[string]string{labels.AnnotationParentTaskName: "unrelated-root"}
	controller := true
	child.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: corev1alpha1.GroupVersion.String(), Kind: "Task",
		Name: "unrelated-root", UID: "unrelated-root-uid", Controller: &controller,
	}}
	resp := validator.Handle(t.Context(), admissionRequest(t, admissionv1.Create, trustedWorkerUser, child, nil, ""))
	require.False(t, resp.Allowed, "a worker ServiceAccount name must not authorize forged coordination ancestry")
}

type coordinationCreateFixture struct {
	parent *corev1alpha1.Task
	other  *corev1alpha1.Task
	child  *corev1alpha1.Task
	job    *batchv1.Job
	pod    *corev1.Pod
	user   authenticationv1.UserInfo
}

func newCoordinationCreateFixture() coordinationCreateFixture {
	controller := true
	parent := newAdmissionTestTask()
	parent.Name, parent.UID = "parent", "parent-uid"
	parent.Status.JobName = "parent-job"
	parent.Status.JobUID = "job-uid"
	parent.Status.Phase = corev1alpha1.TaskPhaseRunning
	other := parent.DeepCopy()
	other.Name, other.UID = "unrelated-root", "unrelated-root-uid"
	other.Status.JobName = "unrelated-job"
	other.Status.JobUID = "unrelated-job-uid"
	parentOwner := metav1.OwnerReference{
		APIVersion: corev1alpha1.GroupVersion.String(), Kind: "Task",
		Name: parent.Name, UID: parent.UID, Controller: &controller,
	}
	child := newAdmissionTestTask()
	child.Name = "child"
	child.Labels = map[string]string{labels.LabelParentTask: parent.Name}
	child.Annotations = map[string]string{labels.AnnotationParentTaskName: parent.Name}
	child.OwnerReferences = []metav1.OwnerReference{parentOwner}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Namespace: admissionTestNamespace, Name: parent.Status.JobName, UID: "job-uid",
		OwnerReferences: []metav1.OwnerReference{parentOwner},
	}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: admissionTestNamespace, Name: "worker-pod", UID: "pod-uid",
			Labels: map[string]string{labels.LabelTask: parent.Name},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: batchv1.SchemeGroupVersion.String(), Kind: "Job",
				Name: job.Name, UID: job.UID, Controller: &controller,
			}},
		},
		Spec: corev1.PodSpec{ServiceAccountName: "orka-ai-worker"},
	}
	return coordinationCreateFixture{
		parent: parent, other: other, child: child, job: job, pod: pod,
		user: authenticationv1.UserInfo{Username: trustedWorkerUser, Extra: map[string]authenticationv1.ExtraValue{
			"authentication.kubernetes.io/pod-name": {pod.Name},
			"authentication.kubernetes.io/pod-uid":  {string(pod.UID)},
		}},
	}
}

func TestTaskProvenanceValidatorChildCreationUsesLiveParentIdentity(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, batchv1.AddToScheme(scheme))
	for _, test := range []struct {
		name        string
		change      func(*coordinationCreateFixture)
		allowed     bool
		readerError bool
	}{
		{name: "own active parent", allowed: true},
		{name: "inherited session reference", allowed: true, change: func(f *coordinationCreateFixture) {
			f.parent.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "parent-session", ThroughMessageID: "cutoff"}
			f.child.Spec.SessionRef = f.parent.Spec.SessionRef.DeepCopy()
		}},
		{name: "unrelated session reference", change: func(f *coordinationCreateFixture) {
			f.parent.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "parent-session"}
			f.other.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "other-session"}
			f.child.Spec.SessionRef = f.other.Spec.SessionRef.DeepCopy()
		}},
		{name: "session reference without parent session", change: func(f *coordinationCreateFixture) {
			f.child.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "other-session"}
		}},
		{name: "broadened session history", change: func(f *coordinationCreateFixture) {
			f.parent.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "parent-session", ThroughMessageID: "cutoff"}
			f.child.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "parent-session"}
		}},
		{name: "worker session reference without ownership", change: func(f *coordinationCreateFixture) {
			f.child.OwnerReferences = nil
			f.child.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "other-session"}
		}},
		{name: "controller authorizes session reference", allowed: true, change: func(f *coordinationCreateFixture) {
			f.user.Username = trustedControllerUser
			f.child.OwnerReferences = nil
			f.child.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "authorized-session"}
		}},
		{name: "vendor worker", allowed: true, change: func(f *coordinationCreateFixture) {
			f.pod.Spec.ServiceAccountName = "orka-vendor-worker"
			f.user.Username = serviceAccountUsername(admissionTestNamespace, f.pod.Spec.ServiceAccountName)
		}},
		{name: "unrelated root", change: func(f *coordinationCreateFixture) {
			f.child.Labels[labels.LabelParentTask] = f.other.Name
			f.child.Annotations[labels.AnnotationParentTaskName] = f.other.Name
			f.child.OwnerReferences[0].Name, f.child.OwnerReferences[0].UID = f.other.Name, f.other.UID
		}},
		{name: "unrelated root with forged Pod label", change: func(f *coordinationCreateFixture) {
			f.child.Labels[labels.LabelParentTask] = f.other.Name
			f.child.Annotations[labels.AnnotationParentTaskName] = f.other.Name
			f.child.OwnerReferences[0].Name, f.child.OwnerReferences[0].UID = f.other.Name, f.other.UID
			f.pod.Labels[labels.LabelTask] = f.other.Name
		}},
		{name: "recreated Pod", change: func(f *coordinationCreateFixture) { f.pod.UID = "new-pod-uid" }},
		{name: "recreated Job", change: func(f *coordinationCreateFixture) { f.job.UID = "new-job-uid" }},
		{name: "replacement Job and Pod", change: func(f *coordinationCreateFixture) {
			f.job.UID = "replacement-job-uid"
			f.pod.OwnerReferences[0].UID = f.job.UID
		}},
		{name: "recreated parent", change: func(f *coordinationCreateFixture) { f.parent.UID = "new-parent-uid" }},
		{name: "previous Job", change: func(f *coordinationCreateFixture) { f.parent.Status.JobName = "new-job" }},
		{name: "missing Job UID binding", change: func(f *coordinationCreateFixture) { f.parent.Status.JobUID = "" }},
		{name: "unbound token", change: func(f *coordinationCreateFixture) { f.user.Extra = nil }},
		{name: "other ServiceAccount", change: func(f *coordinationCreateFixture) { f.pod.Spec.ServiceAccountName = "other" }},
		{name: "other namespace", change: func(f *coordinationCreateFixture) {
			f.user.Username = serviceAccountUsername("other", f.pod.Spec.ServiceAccountName)
		}},
		{name: "non-controller Job owner", change: func(f *coordinationCreateFixture) { f.pod.OwnerReferences[0].Controller = nil }},
		{name: "terminal Pod", change: func(f *coordinationCreateFixture) { f.pod.Status.Phase = corev1.PodSucceeded }},
		{name: "terminal parent", change: func(f *coordinationCreateFixture) { f.parent.Status.Phase = corev1alpha1.TaskPhaseSucceeded }},
		{name: "recorded parent outcome", change: func(f *coordinationCreateFixture) {
			f.parent.Status.ExecutionOutcome = &corev1alpha1.TaskWorkloadExecutionOutcome{Phase: corev1alpha1.TaskPhaseSucceeded}
		}},
		{name: "deleting parent", change: func(f *coordinationCreateFixture) {
			now := metav1.Now()
			f.parent.DeletionTimestamp, f.parent.Finalizers = &now, []string{"orka.ai/cleanup"}
		}},
		{name: "informational lineage without ownership", allowed: true, change: func(f *coordinationCreateFixture) {
			f.child.OwnerReferences = nil
			f.user.Extra = nil
		}},
		{name: "lookup failure", readerError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newCoordinationCreateFixture()
			if test.change != nil {
				test.change(&fixture)
			}
			reader := fake.NewClientBuilder().WithScheme(scheme).
				WithObjects(fixture.parent, fixture.other, fixture.job, fixture.pod).Build()
			validator := newTestTaskProvenanceValidator(t)
			validator.reader = reader
			if test.readerError {
				validator.reader = interceptor.NewClient(reader, interceptor.Funcs{
					Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
						return errors.New("API temporarily unavailable")
					},
				})
			}
			req := admissionRequest(t, admissionv1.Create, fixture.user.Username, fixture.child, nil, "")
			req.UserInfo = fixture.user
			resp := validator.Handle(t.Context(), req)
			require.Equal(t, test.allowed, resp.Allowed)
			if test.readerError {
				require.EqualValues(t, http.StatusInternalServerError, resp.Result.Code)
			}
		})
	}
}
