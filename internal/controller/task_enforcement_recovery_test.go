package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	enforcementv1 "github.com/orka-agents/orka/api/enforcement/v1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/enforcement"
	execevents "github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type nativeAdmissionEventOutage struct {
	store.DeduplicatingExecutionEventStore
	fail       bool
	afterWrite bool
}

func (s *nativeAdmissionEventOutage) AppendExecutionEventIfAbsent(ctx context.Context, event *store.ExecutionEvent, key string) (*store.ExecutionEvent, bool, error) {
	if s.fail && !s.afterWrite {
		return nil, false, errors.New("admission event store unavailable")
	}
	persisted, appended, err := s.DeduplicatingExecutionEventStore.AppendExecutionEventIfAbsent(ctx, event, key)
	if s.fail && err == nil {
		return nil, false, errors.New("admission event reply lost")
	}
	return persisted, appended, err
}

func nativeAdmissionEvents(t *testing.T, r *TaskReconciler, task *corev1alpha1.Task) []store.ExecutionEvent {
	t.Helper()
	events, err := r.ExecutionEventStore.ListExecutionEvents(t.Context(), store.ExecutionEventFilter{
		Namespace: task.Namespace, StreamType: store.ExecutionEventStreamTypeTask,
		StreamID: task.Name, EventTypes: []string{execevents.ExecutionEventTypeTaskStarted},
	})
	require.NoError(t, err)
	return events
}

func TestNativeEnforcementReplacementWaitsForPhysicalRetirement(t *testing.T) {
	r, task, job, pod, coordinator := nativeEnforcementFixture(t)
	_, _, err := r.reconcileNativeEnforcement(t.Context(), task, job)
	require.NoError(t, err)
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(task), task))
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(pod), pod))
	pod.Finalizers = []string{"example.test/physical-termination"}
	require.NoError(t, r.Update(t.Context(), pod))
	replacement := pod.DeepCopy()
	replacement.Name, replacement.UID, replacement.ResourceVersion = "replacement", "replacement-pod-uid", ""
	replacement.Finalizers, replacement.Annotations = nil, nil
	replacement.Status.ContainerStatuses[0].ContainerID = "containerd://" + strings.Repeat("b", 64)
	require.NoError(t, r.Create(t.Context(), replacement))
	require.NoError(t, r.Delete(t.Context(), pod))

	_, handled, err := r.reconcileNativeEnforcement(t.Context(), task, job)
	require.NoError(t, err)
	require.True(t, handled)
	require.Equal(t, 1, coordinator.ensureCalls, "replacement must not activate while previous target may still execute")
	require.Equal(t, 1, coordinator.retireCalls)
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(replacement), replacement))
	require.Empty(t, replacement.Annotations[enforcement.PodAdmissionAnnotation])
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(task), task))
	require.Len(t, task.Status.EnforcementBindings, 1)
	require.Equal(t, string(pod.UID), task.Status.EnforcementBindings[0].PodUID)

	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(pod), pod))
	pod.Finalizers = nil
	require.NoError(t, r.Update(t.Context(), pod))
	coordinator.retired = true
	_, handled, err = r.reconcileNativeEnforcement(t.Context(), task, job)
	require.NoError(t, err)
	require.False(t, handled)
	require.Equal(t, 2, coordinator.ensureCalls)
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(task), task))
	require.Len(t, task.Status.EnforcementBindings, 1)
	require.Equal(t, string(replacement.UID), task.Status.EnforcementBindings[0].PodUID)
	require.True(t, task.Status.EnforcementBindings[0].Published)
	require.Len(t, nativeAdmissionEvents(t, r, task), 2)
	condition := meta.FindStatusCondition(task.Status.Conditions, ConditionTypeEnforcementReady)
	require.NotNil(t, condition)
	require.Contains(t, condition.Message, task.Status.EnforcementBindings[0].BindingID)
}

func TestNativeEnforcementSelectionCASRejectsSecondPhysicalWorker(t *testing.T) {
	r, task, job, _, _ := nativeEnforcementFixture(t)
	_, _, err := r.reconcileNativeEnforcement(t.Context(), task, job)
	require.NoError(t, err)
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(task), task))
	competing := task.Status.EnforcementBindings[0]
	competing.BindingID, competing.PodUID = "different-physical-binding", "different-pod-uid"
	competing.Admitted, competing.Published = false, false
	err = r.persistEnforcementBinding(t.Context(), task, competing, false)
	require.Error(t, err)
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(task), task))
	require.Len(t, task.Status.EnforcementBindings, 1)
}

func TestNativeEnforcementReplacementProjectsItsOwnAlreadyReadyBinding(t *testing.T) {
	r, task, job, pod, coordinator := nativeEnforcementFixture(t)
	_, _, err := r.reconcileNativeEnforcement(t.Context(), task, job)
	require.NoError(t, err)
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(task), task))
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(pod), pod))
	replacement := pod.DeepCopy()
	replacement.Name, replacement.UID, replacement.ResourceVersion = "replacement", "replacement-uid", ""
	replacement.Annotations = nil
	replacement.Status.ContainerStatuses[0].ContainerID = "containerd://" + strings.Repeat("b", 64)
	require.NoError(t, r.Delete(t.Context(), pod))
	require.NoError(t, r.Create(t.Context(), replacement))
	coordinator.retired = true
	_, handled, err := r.reconcileNativeEnforcement(t.Context(), task, job)
	require.NoError(t, err)
	require.False(t, handled)
	require.Len(t, nativeAdmissionEvents(t, r, task), 2)
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(task), task))
	require.Contains(t, meta.FindStatusCondition(task.Status.Conditions, ConditionTypeEnforcementReady).Message,
		task.Status.EnforcementBindings[0].BindingID)
}

func TestNativeEnforcementPublicationBookkeepingSurvivesCancellation(t *testing.T) {
	r, task, job, _, _ := nativeEnforcementFixture(t)
	_, _, err := r.reconcileNativeEnforcement(t.Context(), task, job)
	require.NoError(t, err)
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(task), task))
	task.Status.EnforcementBindings[0].Published = false
	task.Status.Phase = corev1alpha1.TaskPhaseCancelled
	require.NoError(t, r.Status().Update(t.Context(), task))
	published, err := r.recoverNativeEnforcementPublication(t.Context(), task, &task.Status.EnforcementBindings[0])
	require.NoError(t, err)
	require.True(t, published)
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(task), task))
	require.Equal(t, corev1alpha1.TaskPhaseCancelled, task.Status.Phase)
	require.True(t, task.Status.EnforcementBindings[0].Published)
	require.Len(t, nativeAdmissionEvents(t, r, task), 1)
}

func TestNativeEnforcementRepairsPublishedReceiptAfterPodRemoval(t *testing.T) {
	for _, lostReply := range []bool{false, true} {
		t.Run(map[bool]string{false: "store-unavailable", true: "reply-lost"}[lostReply], func(t *testing.T) {
			r, task, job, pod, coordinator := nativeEnforcementFixture(t)
			events := &nativeAdmissionEventOutage{
				DeduplicatingExecutionEventStore: r.ExecutionEventStore.(store.DeduplicatingExecutionEventStore),
				fail:                             true, afterWrite: lostReply,
			}
			r.ExecutionEventStore = events
			_, _, err := r.reconcileNativeEnforcement(t.Context(), task, job)
			require.Error(t, err)
			require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(task), task))
			require.True(t, task.Status.EnforcementBindings[0].Published)
			require.NoError(t, r.Delete(t.Context(), pod))
			job.Status.Succeeded = 1
			events.fail = false
			_, handled, err := r.reconcileNativeEnforcement(t.Context(), task, job)
			require.NoError(t, err)
			require.False(t, handled)
			require.Len(t, nativeAdmissionEvents(t, r, task), 1)
			coordinator.retired = true
			done, err := r.cleanupNativeEnforcement(t.Context(), task, "")
			require.NoError(t, err)
			require.True(t, done)
			require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(task), task))
			require.Empty(t, task.Status.EnforcementBindings)
		})
	}
}

func TestNativeEnforcementEventOutageDoesNotBlockStopOrDiscardReceipt(t *testing.T) {
	r, task, job, pod, coordinator := nativeEnforcementFixture(t)
	events := &nativeAdmissionEventOutage{
		DeduplicatingExecutionEventStore: r.ExecutionEventStore.(store.DeduplicatingExecutionEventStore), fail: true,
	}
	r.ExecutionEventStore = events
	_, _, err := r.reconcileNativeEnforcement(t.Context(), task, job)
	require.Error(t, err)
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(task), task))
	coordinator.retired = true
	done, err := r.cleanupNativeEnforcement(t.Context(), task, "")
	require.Error(t, err)
	require.False(t, done)
	require.Equal(t, 1, coordinator.retireCalls)
	require.Error(t, r.Get(t.Context(), client.ObjectKeyFromObject(pod), &corev1.Pod{}), "ordinary stop is still requested")
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(task), task))
	require.Len(t, task.Status.EnforcementBindings, 1)
	require.True(t, task.Status.EnforcementBindings[0].Published)
	events.fail = false
	done, err = r.cleanupNativeEnforcement(t.Context(), task, "")
	require.NoError(t, err)
	require.True(t, done)
	require.Len(t, nativeAdmissionEvents(t, r, task), 1)
}

func TestNativeEnforcementCleanupCASRetainsConcurrentPublication(t *testing.T) {
	r, task, job, _, coordinator := nativeEnforcementFixture(t)
	coordinator.outcome = &enforcementv1.EnsureResponse{Result: &enforcementv1.EnsureResponse_Pending{
		Pending: &enforcementv1.Pending{Reason: "still preparing"},
	}}
	_, _, err := r.reconcileNativeEnforcement(t.Context(), task, job)
	require.NoError(t, err)
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(task), task))
	coordinator.retired = true
	coordinator.beforeRetire = func() {
		current := &corev1alpha1.Task{}
		require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(task), current))
		current.Status.EnforcementBindings[0].Admitted = true
		current.Status.EnforcementBindings[0].Published = true
		require.NoError(t, r.Status().Update(t.Context(), current))
	}
	done, err := r.cleanupNativeEnforcement(t.Context(), task, "")
	require.Error(t, err)
	require.False(t, done)
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(task), task))
	require.Len(t, task.Status.EnforcementBindings, 1)
	require.True(t, task.Status.EnforcementBindings[0].Published)
	require.Empty(t, nativeAdmissionEvents(t, r, task))
	coordinator.beforeRetire = nil
	done, err = r.cleanupNativeEnforcement(t.Context(), task, "")
	require.NoError(t, err)
	require.True(t, done)
	require.Len(t, nativeAdmissionEvents(t, r, task), 1)
}

func TestNativeEnforcementDeletionErasesRepairedAdmissionEvents(t *testing.T) {
	r, task, job, _, coordinator := nativeEnforcementFixture(t)
	_, _, err := r.reconcileNativeEnforcement(t.Context(), task, job)
	require.NoError(t, err)
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(task), task))
	require.Len(t, nativeAdmissionEvents(t, r, task), 1)
	task.Finalizers = []string{labels.TaskFinalizer}
	require.NoError(t, r.Update(t.Context(), task))
	require.NoError(t, r.Delete(t.Context(), task))
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(task), task))
	coordinator.retired = true
	result, err := r.handleDeletion(t.Context(), task)
	require.NoError(t, err)
	if result.RequeueAfter != 0 {
		require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(task), task))
		result, err = r.handleDeletion(t.Context(), task)
		require.NoError(t, err)
	}
	require.Zero(t, result.RequeueAfter)
	require.Empty(t, nativeAdmissionEvents(t, r, task), "deleted names must not expose recreated history")
}

func TestNativeEnforcementRecoversLostReleaseReplyFromTerminalPod(t *testing.T) {
	r, task, job, pod, _ := nativeEnforcementFixture(t)
	_, _, err := r.reconcileNativeEnforcement(t.Context(), task, job)
	require.NoError(t, err)
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(task), task))
	task.Status.EnforcementBindings[0].Published = false
	meta.RemoveStatusCondition(&task.Status.Conditions, ConditionTypeEnforcementReady)
	require.NoError(t, r.Status().Update(t.Context(), task))
	require.NoError(t, r.ExecutionEventStore.DeleteExecutionEvents(t.Context(), task.Namespace, store.ExecutionEventStreamTypeTask, task.Name))
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(pod), pod))
	pod.Status.Phase = corev1.PodSucceeded
	pod.Status.ContainerStatuses[0].State = corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{ExitCode: 0},
	}
	require.NoError(t, r.Status().Update(t.Context(), pod))
	job.Status.Succeeded = 1
	_, handled, err := r.reconcileNativeEnforcement(t.Context(), task, job)
	require.NoError(t, err)
	require.False(t, handled)
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(task), task))
	require.True(t, task.Status.EnforcementBindings[0].Published)
	require.Len(t, nativeAdmissionEvents(t, r, task), 1)
	approved, err := enforcement.PodApproved(pod, string(pod.UID), "worker", task.Status.EnforcementBindings[0].RequirementDigest)
	require.Error(t, err)
	require.False(t, approved, "historical publication is not live release authority")
}

func TestNativeEnforcementPreAdmissionFailureUsesOrdinaryRetry(t *testing.T) {
	r, task, job, _, coordinator := nativeEnforcementFixture(t)
	task.Spec.RetryPolicy = &corev1alpha1.RetryPolicy{MaxRetries: 1}
	require.NoError(t, r.Update(t.Context(), task))
	task.Status.Attempts = 1
	require.NoError(t, r.Status().Update(t.Context(), task))
	job.Status.Failed = 1
	_, handled, err := r.reconcileNativeEnforcement(t.Context(), task, job)
	require.NoError(t, err)
	require.True(t, handled)
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(task), task))
	require.Equal(t, corev1alpha1.TaskPhasePending, task.Status.Phase)
	require.Empty(t, task.Status.JobUID)
	require.Zero(t, coordinator.ensureCalls)
	require.Len(t, nativeAdmissionEvents(t, r, task), 0)
}

func TestNativeEnforcementUnobservedPublicationNeverEstablishesSuccess(t *testing.T) {
	for _, removePod := range []bool{false, true} {
		t.Run(map[bool]string{false: "not-published", true: "unknown-after-disappearance"}[removePod], func(t *testing.T) {
			r, task, job, pod, coordinator := nativeEnforcementFixture(t)
			binding, err := enforcement.BindContainer(pod, &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: pod.Spec.NodeName, UID: "node-uid"},
			}, "worker", pod.Spec.Containers[0].Env[0].Value, coordinator.providerDigest)
			require.NoError(t, err)
			binding.JobUID = string(job.UID)
			require.NoError(t, r.persistEnforcementBinding(t.Context(), task, *binding, false))
			require.NoError(t, r.persistEnforcementBinding(t.Context(), task, *binding, true))
			if removePod {
				require.NoError(t, r.Delete(t.Context(), pod))
			}
			job.Status = batchv1.JobStatus{Succeeded: 1}
			_, handled, err := r.reconcileNativeEnforcement(t.Context(), task, job)
			require.NoError(t, err)
			require.True(t, handled)
			require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(task), task))
			require.Equal(t, corev1alpha1.TaskPhaseFailed, task.Status.Phase)
			require.False(t, task.Status.EnforcementBindings[0].Published)
			require.Empty(t, nativeAdmissionEvents(t, r, task))
		})
	}
}
