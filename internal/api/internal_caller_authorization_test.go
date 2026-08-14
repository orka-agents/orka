/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

func TestInternalCallerAuthorizerVerifyNamespace(t *testing.T) {
	tests := []struct {
		name       string
		userInfo   *UserInfo
		wantStatus int
	}{
		{name: "missing user", wantStatus: http.StatusUnauthorized},
		{name: "explicit namespace mismatch", userInfo: &UserInfo{Namespace: "other", Username: "system:serviceaccount:other:worker"}, wantStatus: http.StatusForbidden},
		{name: "service account username mismatch", userInfo: &UserInfo{Username: "system:serviceaccount:other:worker"}, wantStatus: http.StatusForbidden},
		{name: "service account namespace match", userInfo: &UserInfo{Username: "system:serviceaccount:default:worker"}, wantStatus: http.StatusNoContent},
		{name: "non service account without namespace remains allowed", userInfo: &UserInfo{Username: "admin"}, wantStatus: http.StatusNoContent},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := fiber.New()
			app.Use(func(c fiber.Ctx) error {
				if tt.userInfo != nil {
					c.Locals(UserInfoContextKey, tt.userInfo)
				}
				return c.Next()
			})
			app.Get("/check", func(c fiber.Ctx) error {
				if err := (internalCallerAuthorizer{}).verifyNamespace(c, "default"); err != nil {
					return err
				}
				return c.SendStatus(fiber.StatusNoContent)
			})
			resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/check", nil))
			require.NoError(t, err)
			require.Equal(t, tt.wantStatus, resp.StatusCode)
		})
	}
}

func TestInternalCallerAuthorizerVerifyTaskWorker(t *testing.T) {
	scheme := internalCallerAuthScheme(t)
	task := internalCallerAuthTask()
	job := internalCallerAuthJob(task, "job-a", "job-uid")
	pod := internalCallerAuthPod(task, "pod-a", "pod-uid", job)
	authorizer := internalCallerAuthorizer{k8sReader: fake.NewClientBuilder().WithScheme(scheme).WithObjects(task, job, pod).Build()}

	err := authorizer.verifyTaskWorker(context.Background(), &UserInfo{Username: "system:serviceaccount:default:worker"}, task)
	fiberErr := &fiber.Error{}
	if !errorsAsFiber(err, &fiberErr) || fiberErr.Code != http.StatusForbidden || fiberErr.Message != "caller pod token required" {
		t.Fatalf("non-tokenreview error = %v, want caller pod token required", err)
	}

	err = authorizer.verifyTaskWorker(context.Background(), &UserInfo{AuthType: AuthTypeTokenReview, Username: "system:serviceaccount:default:worker"}, task)
	if !errorsAsFiber(err, &fiberErr) || fiberErr.Code != http.StatusForbidden || fiberErr.Message != "caller pod identity required" {
		t.Fatalf("missing pod extras error = %v, want pod identity required", err)
	}

	err = authorizer.verifyTaskWorker(context.Background(), internalCallerAuthWorkerUser("pod-a", "pod-uid"), task)
	if err != nil {
		t.Fatalf("valid worker error = %v, want nil", err)
	}
	task.Status.JobName = ""
	err = authorizer.verifyTaskWorker(context.Background(), internalCallerAuthWorkerUser("pod-a", "pod-uid"), task)
	if !errorsAsFiber(err, &fiberErr) || fiberErr.Code != http.StatusForbidden || fiberErr.Message != "task has no active worker job" {
		t.Fatalf("worker before JobName status error = %v, want task has no active worker job", err)
	}
}

func TestInternalCallerAuthorizerRejectsUntrustedWorkerProvenance(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*corev1alpha1.Task, *batchv1.Job, *corev1.Pod)
	}{
		{
			name: "pod owner is not controller",
			mutate: func(_ *corev1alpha1.Task, _ *batchv1.Job, pod *corev1.Pod) {
				pod.OwnerReferences[0].Controller = nil
			},
		},
		{
			name: "pod owner api version is not exact",
			mutate: func(_ *corev1alpha1.Task, _ *batchv1.Job, pod *corev1.Pod) {
				pod.OwnerReferences[0].APIVersion = "batch/v1beta1"
			},
		},
		{
			name: "job owner is not controller",
			mutate: func(_ *corev1alpha1.Task, job *batchv1.Job, _ *corev1.Pod) {
				job.OwnerReferences[0].Controller = nil
			},
		},
		{
			name: "job task label is missing",
			mutate: func(_ *corev1alpha1.Task, job *batchv1.Job, _ *corev1.Pod) {
				job.Labels = nil
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := internalCallerAuthScheme(t)
			task := internalCallerAuthTask()
			task.Spec.Type = corev1alpha1.TaskTypeAI
			task.Status.Phase = corev1alpha1.TaskPhaseRunning
			job := internalCallerAuthJob(task, "job-a", "job-uid")
			pod := internalCallerAuthPod(task, "pod-a", "pod-uid", job)
			tt.mutate(task, job, pod)
			authorizer := internalCallerAuthorizer{
				k8sReader: fake.NewClientBuilder().WithScheme(scheme).WithObjects(task, job, pod).Build(),
			}

			_, err := authorizer.currentTaskWorker(context.Background(), internalCallerAuthWorkerUser("pod-a", "pod-uid"), task.Namespace)
			var fiberErr *fiber.Error
			if !errors.As(err, &fiberErr) || fiberErr.Code != http.StatusForbidden {
				t.Fatalf("currentTaskWorker() error = %v, want forbidden", err)
			}
		})
	}
}

func TestInternalCallerAuthorizerAcceptsLegacyProvenanceWhenPoliciesAreActive(t *testing.T) {
	scheme := internalCallerAuthScheme(t)
	task := internalCallerAuthTask()
	task.Spec.Type = corev1alpha1.TaskTypeAI
	task.Status.Phase = corev1alpha1.TaskPhaseRunning
	job := internalCallerAuthJob(task, "job-a", "job-uid")
	pod := internalCallerAuthPod(task, "pod-a", "pod-uid", job)
	delete(job.Labels, labels.LabelTaskUID)
	delete(pod.Labels, labels.LabelTaskUID)
	user := internalCallerAuthWorkerUser(pod.Name, string(pod.UID))
	prePolicyReader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task, job, pod).Build()
	if _, err := (internalCallerAuthorizer{k8sReader: prePolicyReader}).currentTaskWorker(context.Background(), user, task.Namespace); err == nil {
		t.Fatal("legacy provenance without admission attestation was accepted")
	}
	objects := []runtime.Object{task, job, pod}
	for _, object := range internalCallerAuthProvenanceObjects(job.CreationTimestamp.Add(time.Minute)) {
		objects = append(objects, object)
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objects...).Build()
	if _, err := (internalCallerAuthorizer{k8sReader: reader}).currentTaskWorker(context.Background(), user, task.Namespace); err != nil {
		t.Fatalf("pre-policy in-flight legacy provenance was rejected: %v", err)
	}
}

func internalCallerAuthProvenanceObjects(createdAt time.Time) []client.Object {
	objects := make([]client.Object, 0, 4)
	policyTime := metav1.NewTime(createdAt)
	for _, component := range []string{taskProvenancePolicyJobComponent, taskProvenancePolicyPodComponent} {
		name := "task-" + component + "-provenance"
		objects = append(objects,
			&admissionregistrationv1.ValidatingAdmissionPolicy{ObjectMeta: metav1.ObjectMeta{
				Name: name, CreationTimestamp: policyTime, Labels: map[string]string{taskProvenancePolicyLabel: component},
			}},
			&admissionregistrationv1.ValidatingAdmissionPolicyBinding{ObjectMeta: metav1.ObjectMeta{
				Name: name, CreationTimestamp: policyTime, Labels: map[string]string{taskProvenancePolicyLabel: component},
			}, Spec: admissionregistrationv1.ValidatingAdmissionPolicyBindingSpec{
				PolicyName: name, ValidationActions: []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny},
			}},
		)
	}
	return objects
}

func TestSubmitResultRemainsNamespaceOnlyWithKubernetesClient(t *testing.T) {
	scheme := internalCallerAuthScheme(t)
	task := internalCallerAuthTask()
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task).Build()
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	store := sqlite.NewStore(db, ":memory:")
	h := NewInternalHandlers(store, store, store, store, store, InternalHandlersConfig{Client: k8sClient})
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, &UserInfo{Username: "system:serviceaccount:default:worker"})
		return c.Next()
	})
	app.Post("/internal/v1/results/:namespace/:taskName", h.SubmitResult)

	resp, err := app.Test(httptest.NewRequest(http.MethodPost, "/internal/v1/results/default/task-a", bytes.NewReader([]byte("result"))))
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
}

func internalCallerAuthScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, batchv1.AddToScheme(scheme))
	require.NoError(t, admissionregistrationv1.AddToScheme(scheme))
	return scheme
}

func internalCallerAuthTask() *corev1alpha1.Task {
	return &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "task-a", Namespace: "default", UID: types.UID("task-uid")},
		Status:     corev1alpha1.TaskStatus{JobName: "job-a"},
	}
}

func internalCallerAuthJob(task *corev1alpha1.Task, name, uid string) *batchv1.Job {
	controller := true
	createdAt := metav1.NewTime(time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC))
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         task.Namespace,
			UID:               types.UID(uid),
			CreationTimestamp: createdAt,
			Labels: map[string]string{
				labels.LabelTask:    labels.SelectorValue(task.Name),
				labels.LabelTaskUID: string(task.UID),
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: corev1alpha1.GroupVersion.String(),
				Kind:       "Task",
				Name:       task.Name,
				UID:        task.UID,
				Controller: &controller,
			}},
		},
	}
}

func internalCallerAuthPod(task *corev1alpha1.Task, name, uid string, job *batchv1.Job) *corev1.Pod {
	controller := true
	createdAt := metav1.NewTime(job.CreationTimestamp.Add(time.Second))
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         task.Namespace,
			UID:               types.UID(uid),
			CreationTimestamp: createdAt,
			Labels: map[string]string{
				labels.LabelTask:    labels.SelectorValue(task.Name),
				labels.LabelTaskUID: string(task.UID),
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: batchv1.SchemeGroupVersion.String(),
				Kind:       "Job",
				Name:       job.Name,
				UID:        job.UID,
				Controller: &controller,
			}},
		},
		Spec: corev1.PodSpec{ServiceAccountName: "worker"},
	}
}

func internalCallerAuthWorkerUser(podName, podUID string) *UserInfo {
	return &UserInfo{
		Username: "system:serviceaccount:default:worker",
		AuthType: AuthTypeTokenReview,
		Extra: map[string]authenticationv1.ExtraValue{
			"authentication.kubernetes.io/pod-name": {podName},
			"authentication.kubernetes.io/pod-uid":  {podUID},
		},
	}
}

func errorsAsFiber(err error, target **fiber.Error) bool {
	return errors.As(err, target)
}
