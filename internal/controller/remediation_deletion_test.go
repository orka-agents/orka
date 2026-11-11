package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation/contract"
	"github.com/orka-agents/orka/internal/remediation/service"
)

type remediationDeletionBackend struct{}

func (remediationDeletionBackend) ConfigurationDigest() string {
	return pv.Digest([]byte("deletion test configuration"))
}
func (remediationDeletionBackend) RequestDigest(contract.ExecutionRequest) (string, error) {
	return "", contract.ErrDisabled
}
func (remediationDeletionBackend) Start(context.Context, contract.ExecutionRequest, func(contract.ExecutionReceipt) error) (*pv.Record, error) {
	return nil, contract.ErrDisabled
}
func (remediationDeletionBackend) Recover(context.Context, contract.ExecutionRequest) (contract.ExecutionRecovery, error) {
	return contract.ExecutionRecovery{}, contract.ErrDisabled
}
func (remediationDeletionBackend) Resume(context.Context, contract.ExecutionRequest, contract.ExecutionReceipt) (*pv.Record, error) {
	return nil, contract.ErrDisabled
}
func (remediationDeletionBackend) Cancel(context.Context, contract.ExecutionReceipt) error {
	return contract.ErrDisabled
}

func TestRemediationOrphanDeletionReachesNormalFinalizer(t *testing.T) {
	now := metav1.Now()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: "remediation-orphan", Namespace: "default", UID: "orphan-task-uid",
			Labels: map[string]string{
				"remediation.orka.ai/run": "missing-remediation-state", "remediation.orka.ai/role": "select",
			},
			DeletionTimestamp: &now, Finalizers: []string{labels.TaskFinalizer},
		},
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
		Status: corev1alpha1.TaskStatus{
			Phase: corev1alpha1.TaskPhaseRunning, JobName: "remediation-orphan-job", JobUID: "orphan-job-uid",
		},
	}
	controller := true
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name: task.Status.JobName, Namespace: task.Namespace, UID: "orphan-job-uid",
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: corev1alpha1.GroupVersion.String(), Kind: "Task", Name: task.Name, UID: task.UID, Controller: &controller,
		}},
	}}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: task.Namespace, UID: "namespace-uid"}}
	reconciler := newUnitReconciler(newTestScheme(), namespace, task, job)
	reconciler.APIReader = reconciler.Client
	coordinator, err := service.New(service.Config{
		Client: reconciler.Client, Reader: reconciler.Client, Namespace: task.Namespace, AgentName: "native-agent",
		AIWorkerImage: "registry.example/ai-worker@sha256:" + strings.Repeat("a", 64),
		Results:       reconciler.ResultStore, Backend: remediationDeletionBackend{},
	})
	require.NoError(t, err)
	reconciler.RemediationTaskReconciler = coordinator
	require.NoError(t, reconciler.ResultStore.SaveResult(t.Context(), task.Namespace, task.Name, []byte("synthetic result")))
	_, err = reconciler.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(task)})
	require.NoError(t, err)
	require.True(t, apierrors.IsNotFound(reconciler.Get(t.Context(), client.ObjectKeyFromObject(task), new(corev1alpha1.Task))),
		"ordinary handleDeletion did not remove the Task finalizer")
	require.True(t, apierrors.IsNotFound(reconciler.Get(t.Context(), client.ObjectKeyFromObject(job), new(batchv1.Job))))
	_, err = reconciler.ResultStore.GetResult(t.Context(), task.Namespace, task.Name)
	require.Error(t, err, "ordinary handleDeletion did not remove the stored result")
}

func TestRemediationNonDeletingOrphanCannotDispatch(t *testing.T) {
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{
		Name: "remediation-live-orphan", Namespace: "default", UID: "live-orphan-uid",
		Labels: map[string]string{"remediation.orka.ai/run": "missing-state", "remediation.orka.ai/role": "select"},
	}, Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI}}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: task.Namespace, UID: "namespace-uid"}}
	reconciler := newUnitReconciler(newTestScheme(), namespace, task)
	coordinator, err := service.New(service.Config{
		Client: reconciler.Client, Reader: reconciler.Client, Namespace: task.Namespace, AgentName: "native-agent",
		AIWorkerImage: "registry.example/ai-worker@sha256:" + strings.Repeat("a", 64),
		Results:       reconciler.ResultStore, Backend: remediationDeletionBackend{},
	})
	require.NoError(t, err)
	reconciler.RemediationTaskReconciler = coordinator
	_, err = reconciler.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(task)})
	require.Error(t, err)
	var jobs batchv1.JobList
	require.NoError(t, reconciler.List(t.Context(), &jobs, client.InNamespace(task.Namespace)))
	require.Empty(t, jobs.Items)
}
