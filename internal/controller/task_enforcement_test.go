package controller

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	enforcementv1 "github.com/orka-agents/orka/api/enforcement/v1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/enforcement"
	execevents "github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/workerenv"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

type recordingEnforcementCoordinator struct {
	mu             sync.Mutex
	providerDigest string
	outcome        *enforcementv1.EnsureResponse
	ensureErr      error
	beforeEnsure   func(*corev1alpha1.ContainerEnforcementBinding, *enforcementv1.Requirement)
	ensureCalls    int
	retireCalls    int
	retired        bool
	retireErr      error
	beforeRetire   func()
}

func TestTaskEnforcementSupportedRuntimeKinds(t *testing.T) {
	for _, kind := range []corev1alpha1.TaskType{corev1alpha1.TaskTypeAI, corev1alpha1.TaskTypeAgent, corev1alpha1.TaskTypeContainer} {
		t.Run(string(kind), func(t *testing.T) {
			r := TaskReconciler{Enforcement: &recordingEnforcementCoordinator{}}
			task := &corev1alpha1.Task{Spec: corev1alpha1.TaskSpec{
				Type: kind, Enforcement: &corev1alpha1.EnforcementRequirement{
					Format: "example.policy/v1", Policy: []byte("complete policy"),
					RequiredCapabilities: []string{"filesystem/deny-write/v1"}, Mode: "enforce",
				},
			}}
			if kind == corev1alpha1.TaskTypeContainer {
				require.Error(t, r.validateTaskEnforcement(task))
			} else {
				require.NoError(t, r.validateTaskEnforcement(task))
			}
			r.Enforcement = nil
			require.Error(t, r.validateTaskEnforcement(task))
		})
	}
}

func (c *recordingEnforcementCoordinator) ProviderSetDigest(string, string) (string, error) {
	return c.providerDigest, nil
}

func (c *recordingEnforcementCoordinator) Ensure(_ context.Context, binding *corev1alpha1.ContainerEnforcementBinding, requirement *enforcementv1.Requirement) ([]*enforcementv1.Enforced, *enforcementv1.EnsureResponse, error) {
	c.mu.Lock()
	c.ensureCalls++
	c.mu.Unlock()
	if c.beforeEnsure != nil {
		c.beforeEnsure(binding, requirement)
	}
	if c.outcome != nil || c.ensureErr != nil {
		return nil, c.outcome, c.ensureErr
	}
	return []*enforcementv1.Enforced{{BindingId: binding.BindingID}}, nil, nil
}

func (c *recordingEnforcementCoordinator) Retire(context.Context, *corev1alpha1.ContainerEnforcementBinding) (bool, error) {
	c.mu.Lock()
	c.retireCalls++
	c.mu.Unlock()
	if c.beforeRetire != nil {
		c.beforeRetire()
	}
	return c.retired, c.retireErr
}

func nativeEnforcementFixture(t *testing.T) (*TaskReconciler, *corev1alpha1.Task, *batchv1.Job, *corev1.Pod, *recordingEnforcementCoordinator) {
	t.Helper()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "protected-ai", Namespace: "default", UID: "task-uid", Generation: 1},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAI,
			AI:   &corev1alpha1.AISpec{Provider: "openai", Model: "test-model", Prompt: "hello"},
			Enforcement: &corev1alpha1.EnforcementRequirement{
				Format: "example.policy/v1", Policy: []byte(" complete policy \n"),
				RequiredCapabilities: []string{"filesystem/deny-write/v1"}, Mode: "enforce",
			},
		},
		Status: corev1alpha1.TaskStatus{
			Phase: corev1alpha1.TaskPhaseRunning, JobName: "protected-ai-job", JobUID: "job-uid",
		},
	}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name: task.Status.JobName, Namespace: task.Namespace, UID: "job-uid",
		OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(task, corev1alpha1.GroupVersion.WithKind("Task"))},
	}}
	requirement, err := enforcement.TaskRequirement(task)
	require.NoError(t, err)
	digest, err := enforcement.RequirementDigest(requirement)
	require.NoError(t, err)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "protected-ai-worker", Namespace: task.Namespace, UID: "pod-uid",
			Labels:          map[string]string{labels.LabelTask: labels.SelectorValue(task.Name)},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(job, batchv1.SchemeGroupVersion.WithKind("Job"))},
		},
		Spec: corev1.PodSpec{
			NodeName: "node", RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name: "worker", Command: []string{"/worker-enforced"}, Args: []string{"--mode=ai"},
				Env: []corev1.EnvVar{{Name: workerenv.EnforcementRequirementDigest, Value: digest}},
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
			Name: "worker", ContainerID: "containerd://" + strings.Repeat("a", 64),
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}}},
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node", UID: "node-uid"}}
	r := newUnitReconciler(newTestScheme(), task, job, pod, node)
	coordinator := &recordingEnforcementCoordinator{providerDigest: "sha256:" + strings.Repeat("b", 64)}
	r.Enforcement = coordinator
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(task), task))
	return r, task, job, pod, coordinator
}

func TestNativeEnforcementPersistsIntentBeforeRelease(t *testing.T) {
	r, task, job, pod, coordinator := nativeEnforcementFixture(t)
	task.Status.EnforcementOutcome = enforcement.Outcome("EnforcementPending")
	task.Status.EnforcementOutcome.ObservedGeneration = task.Generation
	require.NoError(t, r.Status().Update(t.Context(), task))
	coordinator.beforeEnsure = func(binding *corev1alpha1.ContainerEnforcementBinding, requirement *enforcementv1.Requirement) {
		current := &corev1alpha1.Task{}
		require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(task), current))
		require.Len(t, current.Status.EnforcementBindings, 1)
		require.Equal(t, binding.BindingID, current.Status.EnforcementBindings[0].BindingID)
		require.False(t, current.Status.EnforcementBindings[0].Admitted)
		require.Equal(t, task.Spec.Enforcement.Policy, requirement.Policy)
		currentPod := &corev1.Pod{}
		require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(pod), currentPod))
		require.Empty(t, currentPod.Annotations[enforcement.PodAdmissionAnnotation])
	}
	_, handled, err := r.reconcileNativeEnforcement(t.Context(), task, job)
	require.NoError(t, err)
	require.False(t, handled)
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(pod), pod))
	approved, err := enforcement.PodApproved(pod, string(pod.UID), "worker", pod.Spec.Containers[0].Env[0].Value)
	require.NoError(t, err)
	require.True(t, approved)
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(task), task))
	require.True(t, task.Status.EnforcementBindings[0].Admitted)
	require.True(t, meta.IsStatusConditionTrue(task.Status.Conditions, ConditionTypeEnforcementReady))
	require.Nil(t, task.Status.EnforcementOutcome)
}

func TestNativeEnforcementNeverReleasesIncompleteAuthority(t *testing.T) {
	for _, name := range []string{"pending", "refused", "uncertain"} {
		t.Run(name, func(t *testing.T) {
			r, task, job, pod, coordinator := nativeEnforcementFixture(t)
			switch name {
			case "pending":
				coordinator.outcome = &enforcementv1.EnsureResponse{Result: &enforcementv1.EnsureResponse_Pending{
					Pending: &enforcementv1.Pending{Reason: "private-provider-prose"},
				}}
			case "refused":
				coordinator.outcome = &enforcementv1.EnsureResponse{Result: &enforcementv1.EnsureResponse_Refused{
					Refused: &enforcementv1.Refused{Reason: "private-provider-prose", Needs: []string{"supported-format", "private-provider-need"}},
				}}
			case "uncertain":
				coordinator.ensureErr = errors.Join(context.DeadlineExceeded, errors.New("private-provider-prose"))
			}
			_, handled, err := r.reconcileNativeEnforcement(t.Context(), task, job)
			require.NoError(t, err)
			require.True(t, handled)
			require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(pod), pod))
			require.Empty(t, pod.Annotations[enforcement.PodAdmissionAnnotation])
			require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(task), task))
			require.Len(t, task.Status.EnforcementBindings, 1)
			require.False(t, task.Status.EnforcementBindings[0].Admitted)
			require.False(t, meta.IsStatusConditionTrue(task.Status.Conditions, ConditionTypeEnforcementReady))
			require.NotNil(t, task.Status.EnforcementOutcome)
			require.Equal(t, task.Generation, task.Status.EnforcementOutcome.ObservedGeneration)
			if name == "refused" {
				require.Contains(t, task.Status.EnforcementOutcome.Needs, corev1alpha1.EnforcementNeed("supported-format"))
			}
			status, marshalErr := json.Marshal(task.Status)
			require.NoError(t, marshalErr)
			require.NotContains(t, string(status), "private-provider")
		})
	}
}

func TestEnforcementOutcomeRejectsStaleAttemptAndPositiveObservation(t *testing.T) {
	for _, change := range []string{"generation", "job", "attempt", "session", "admitted"} {
		t.Run(change, func(t *testing.T) {
			r, task, _, _, _ := nativeEnforcementFixture(t)
			task.Status.Execution = &corev1alpha1.TaskExecutionStatus{Attempt: 1, RuntimeSessionUID: "session-one"}
			require.NoError(t, r.Status().Update(t.Context(), task))
			stale := task.DeepCopy()
			switch change {
			case "generation":
				task.Generation++
				require.NoError(t, r.Update(t.Context(), task))
			case "job":
				task.Status.JobUID = "replacement-job"
			case "attempt":
				task.Status.Execution.Attempt++
			case "session":
				task.Status.Execution.RuntimeSessionUID = "replacement-session"
			case "admitted":
				meta.SetStatusCondition(&task.Status.Conditions, metav1.Condition{
					Type: ConditionTypeEnforcementReady, Status: metav1.ConditionTrue,
					Reason: "Enforced", ObservedGeneration: task.Generation,
					LastTransitionTime: metav1.Now(),
				})
			}
			require.NoError(t, r.Status().Update(t.Context(), task))
			err := r.markEnforcement(t.Context(), stale, metav1.ConditionFalse, "EnforcementRefused", "obsolete result")
			require.Error(t, err)
			current := &corev1alpha1.Task{}
			require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(task), current))
			require.Nil(t, current.Status.EnforcementOutcome)
			if change == "admitted" {
				require.True(t, meta.IsStatusConditionTrue(current.Status.Conditions, ConditionTypeEnforcementReady))
			}
		})
	}
}

func TestEnforcementOutcomeRetryKeepsOriginalGeneration(t *testing.T) {
	r, task, _, _, _ := nativeEnforcementFixture(t)
	conflicted := false
	r.Client = fake.NewClientBuilder().WithScheme(newTestScheme()).
		WithStatusSubresource(&corev1alpha1.Task{}).WithObjects(task).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, base client.Client, subresource string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				if !conflicted {
					conflicted = true
					current := &corev1alpha1.Task{}
					require.NoError(t, base.Get(ctx, client.ObjectKeyFromObject(task), current))
					current.Generation++
					require.NoError(t, base.Update(ctx, current))
					return apierrors.NewConflict(corev1alpha1.GroupVersion.WithResource("tasks").GroupResource(), task.Name, errors.New("concurrent Task change"))
				}
				return base.SubResource(subresource).Update(ctx, obj, opts...)
			},
		}).Build()
	require.Error(t, r.markEnforcement(t.Context(), task, metav1.ConditionFalse, "EnforcementRefused", "obsolete result"))
	require.True(t, conflicted)
	current := &corev1alpha1.Task{}
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(task), current))
	require.Nil(t, current.Status.EnforcementOutcome)
	require.Nil(t, meta.FindStatusCondition(current.Status.Conditions, ConditionTypeEnforcementReady))
}

func TestEnforcementAdmissionRejectsStalePositiveResult(t *testing.T) {
	for _, change := range []string{"generation", "attempt", "session-generation", "newer-outcome"} {
		t.Run(change, func(t *testing.T) {
			r, task, _, _, _ := nativeEnforcementFixture(t)
			task.Status.Execution = &corev1alpha1.TaskExecutionStatus{Attempt: 1, RuntimeSessionGeneration: 1}
			require.NoError(t, r.Status().Update(t.Context(), task))
			observation := task.DeepCopy()
			requirement, err := enforcement.TaskRequirement(task)
			require.NoError(t, err)
			digest, err := enforcement.RequirementDigest(requirement)
			require.NoError(t, err)
			binding := &corev1alpha1.ContainerEnforcementBinding{
				BindingID: "container:" + strings.Repeat("a", 64), RequirementDigest: digest, JobUID: task.Status.JobUID,
			}
			switch change {
			case "generation":
				task.Generation++
				require.NoError(t, r.Update(t.Context(), task))
			case "attempt":
				task.Status.Execution.Attempt++
			case "session-generation":
				task.Status.Execution.RuntimeSessionGeneration++
			case "newer-outcome":
				task.Status.EnforcementOutcome = enforcement.Outcome("EnforcementRefused", "supported-format")
				task.Status.EnforcementOutcome.ObservedGeneration = task.Generation
			}
			require.NoError(t, r.Status().Update(t.Context(), task))
			require.Error(t, r.markEnforcementAdmission(t.Context(), task, binding, observation))
			current := &corev1alpha1.Task{}
			require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(task), current))
			require.Nil(t, current.Status.EnforcementAdmission)
			require.False(t, meta.IsStatusConditionTrue(current.Status.Conditions, ConditionTypeEnforcementReady))
			if change == "newer-outcome" {
				require.Equal(t, "EnforcementRefused", current.Status.EnforcementOutcome.Reason)
			}
		})
	}
}

func TestNativeAdmissionPreservesPreRPCObservationAcrossOwnBindingWrites(t *testing.T) {
	r, task, job, _, coordinator := nativeEnforcementFixture(t)
	coordinator.beforeEnsure = func(_ *corev1alpha1.ContainerEnforcementBinding, _ *enforcementv1.Requirement) {
		current := &corev1alpha1.Task{}
		require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(task), current))
		current.Status.EnforcementOutcome = enforcement.Outcome("EnforcementRefused", "supported-format")
		current.Status.EnforcementOutcome.ObservedGeneration = current.Generation
		require.NoError(t, r.Status().Update(t.Context(), current))
	}
	_, _, err := r.reconcileNativeEnforcement(t.Context(), task, job)
	require.Error(t, err)
	current := &corev1alpha1.Task{}
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(task), current))
	require.Nil(t, current.Status.EnforcementAdmission)
	require.Equal(t, "EnforcementRefused", current.Status.EnforcementOutcome.Reason)
	require.False(t, meta.IsStatusConditionTrue(current.Status.Conditions, ConditionTypeEnforcementReady))
}

func TestHistoricalAdmissionRepairDoesNotPromoteReplacementAttempt(t *testing.T) {
	r, task, job, _, _ := nativeEnforcementFixture(t)
	_, _, err := r.reconcileNativeEnforcement(t.Context(), task, job)
	require.NoError(t, err)
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(task), task))
	binding := task.Status.EnforcementBindings[0]
	task.Status.JobUID = "replacement-job"
	task.Status.EnforcementAdmission = nil
	task.Status.EnforcementOutcome = enforcement.Outcome("EnforcementPending", "prepared-container")
	task.Status.EnforcementOutcome.ObservedGeneration = task.Generation
	meta.SetStatusCondition(&task.Status.Conditions, metav1.Condition{
		Type: ConditionTypeEnforcementReady, Status: metav1.ConditionFalse,
		Reason: "EnforcementPending", ObservedGeneration: task.Generation, LastTransitionTime: metav1.Now(),
	})
	require.NoError(t, r.Status().Update(t.Context(), task))
	expected := task.Status.EnforcementOutcome.DeepCopy()
	require.NoError(t, r.markEnforcementAdmission(t.Context(), task, &binding, task.DeepCopy()))
	require.NotNil(t, task.Status.EnforcementAdmission)
	require.Equal(t, expected, task.Status.EnforcementOutcome)
	require.False(t, meta.IsStatusConditionTrue(task.Status.Conditions, ConditionTypeEnforcementReady))
}

func TestNativeEnforcementFencesAttemptChangesDuringEnsure(t *testing.T) {
	for _, name := range []string{"cancel", "timeout", "job-replaced", "container-replaced"} {
		t.Run(name, func(t *testing.T) {
			r, task, job, pod, coordinator := nativeEnforcementFixture(t)
			coordinator.beforeEnsure = func(_ *corev1alpha1.ContainerEnforcementBinding, _ *enforcementv1.Requirement) {
				current := &corev1alpha1.Task{}
				require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(task), current))
				switch name {
				case "cancel":
					current.Annotations = map[string]string{labels.AnnotationCancelRequested: "stop"}
					require.NoError(t, r.Update(t.Context(), current))
				case "timeout":
					current.Spec.Timeout = &metav1.Duration{Duration: time.Nanosecond}
					require.NoError(t, r.Update(t.Context(), current))
					start := metav1.NewTime(time.Now().Add(-time.Minute))
					current.Status.StartTime = &start
					require.NoError(t, r.Status().Update(t.Context(), current))
				case "job-replaced":
					current.Status.JobUID = "replacement-job-uid"
					require.NoError(t, r.Status().Update(t.Context(), current))
				case "container-replaced":
					currentPod := &corev1.Pod{}
					require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(pod), currentPod))
					currentPod.Status.ContainerStatuses[0].ContainerID = "containerd://" + strings.Repeat("c", 64)
					require.NoError(t, r.Status().Update(t.Context(), currentPod))
				}
			}
			_, _, err := r.reconcileNativeEnforcement(t.Context(), task, job)
			require.Error(t, err)
			if name == "container-replaced" {
				require.True(t, apierrors.IsConflict(err))
			}
			require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(pod), pod))
			require.Empty(t, pod.Annotations[enforcement.PodAdmissionAnnotation])
			require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(task), task))
			require.Len(t, task.Status.EnforcementBindings, 1, "failed release retains physical cleanup responsibility")
		})
	}
}

func TestNativeEnforcementRepairsAdmissionProjectionWithoutReactivation(t *testing.T) {
	r, task, job, _, coordinator := nativeEnforcementFixture(t)
	_, _, err := r.reconcileNativeEnforcement(t.Context(), task, job)
	require.NoError(t, err)
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(task), task))
	meta.RemoveStatusCondition(&task.Status.Conditions, ConditionTypeEnforcementReady)
	task.Status.EnforcementAdmission = nil
	require.NoError(t, r.Status().Update(t.Context(), task))
	_, _, err = r.reconcileNativeEnforcement(t.Context(), task, job)
	require.NoError(t, err)
	require.Equal(t, 1, coordinator.ensureCalls)
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(task), task))
	require.True(t, meta.IsStatusConditionTrue(task.Status.Conditions, ConditionTypeEnforcementReady))
	require.NotNil(t, task.Status.EnforcementAdmission)
	require.Equal(t, task.Status.EnforcementBindings[0].BindingID, task.Status.EnforcementAdmission.BindingID)
	events, err := r.ExecutionEventStore.ListExecutionEvents(t.Context(), store.ExecutionEventFilter{
		Namespace: task.Namespace, StreamType: store.ExecutionEventStreamTypeTask,
		StreamID: task.Name, EventTypes: []string{execevents.ExecutionEventTypeTaskStarted},
	})
	require.NoError(t, err)
	require.Len(t, events, 1)
	coordinator.providerDigest = "sha256:" + strings.Repeat("d", 64)
	_, _, err = r.reconcileNativeEnforcement(t.Context(), task, job)
	require.Error(t, err, "an admitted Pod cannot bypass changed provider trust")
	require.Equal(t, 1, coordinator.ensureCalls)
}

func TestNativeEnforcementKeepsAdmissionWhileJobCompletionLags(t *testing.T) {
	for _, name := range []string{"terminating", "succeeded", "failed", "foreign-container", "unpublished"} {
		t.Run(name, func(t *testing.T) {
			r, task, job, pod, coordinator := nativeEnforcementFixture(t)
			_, _, err := r.reconcileNativeEnforcement(t.Context(), task, job)
			require.NoError(t, err)
			require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(task), task))
			admission := *task.Status.EnforcementAdmission
			require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(pod), pod))
			pod.Status.ContainerStatuses[0].State = corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{ExitCode: 0},
			}
			switch name {
			case "succeeded":
				pod.Status.Phase = corev1.PodSucceeded
			case "failed":
				pod.Status.Phase = corev1.PodFailed
				pod.Status.ContainerStatuses[0].State.Terminated.ExitCode = 1
			case "foreign-container":
				pod.Status.ContainerStatuses[0].ContainerID = "containerd://" + strings.Repeat("c", 64)
			case "unpublished":
				task.Status.EnforcementBindings[0].Published = false
				require.NoError(t, r.Status().Update(t.Context(), task))
			}
			require.NoError(t, r.Status().Update(t.Context(), pod))
			_, handled, err := r.reconcileNativeEnforcement(t.Context(), task, job)
			require.NoError(t, err)
			require.False(t, handled)
			require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(task), task))
			require.Equal(t, admission, *task.Status.EnforcementAdmission)
			wantAdmitted := name != "foreign-container" && name != "unpublished"
			require.Equal(t, wantAdmitted, meta.IsStatusConditionTrue(task.Status.Conditions, ConditionTypeEnforcementReady))
			require.Equal(t, 1, coordinator.ensureCalls, "completed containers must not reactivate providers")
		})
	}
}

func TestNativeEnforcementRetainsCleanupAcrossRetryAndUncertainty(t *testing.T) {
	for _, name := range []string{"live-attempt", "pending", "uncertain", "registry-missing", "retired"} {
		t.Run(name, func(t *testing.T) {
			r, task, job, pod, coordinator := nativeEnforcementFixture(t)
			_, _, err := r.reconcileNativeEnforcement(t.Context(), task, job)
			require.NoError(t, err)
			require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(task), task))
			require.NotNil(t, task.Status.EnforcementAdmission)
			admission := *task.Status.EnforcementAdmission
			task.Status.JobName, task.Status.JobUID = "", ""
			require.NoError(t, r.Status().Update(t.Context(), task))
			keep := ""
			switch name {
			case "live-attempt":
				keep = string(job.UID)
			case "uncertain":
				coordinator.retireErr = errors.New("provider unavailable")
			case "registry-missing":
				r.Enforcement = nil
			case "retired":
				coordinator.retired = true
			}
			done, err := r.cleanupNativeEnforcement(t.Context(), task, keep)
			require.Equal(t, name == "uncertain" || name == "registry-missing", err != nil)
			require.Equal(t, name == "retired" || name == "live-attempt", done)
			require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(task), task))
			require.Equal(t, admission, *task.Status.EnforcementAdmission, "retirement must not erase historical admission")
			if name == "retired" {
				require.Empty(t, task.Status.EnforcementBindings)
			} else {
				require.Len(t, task.Status.EnforcementBindings, 1)
			}
			podErr := r.Get(t.Context(), client.ObjectKeyFromObject(pod), &corev1.Pod{})
			if name == "live-attempt" || name == "registry-missing" {
				require.NoError(t, podErr)
				require.Zero(t, coordinator.retireCalls)
			} else {
				require.True(t, apierrors.IsNotFound(podErr), "stop is requested before provider retirement")
				require.Equal(t, 1, coordinator.retireCalls)
			}
		})
	}
}

func TestNativeEnforcementJobBuilderKeepsUnenforcedWorkersUnchanged(t *testing.T) {
	_, task, _, _, _ := nativeEnforcementFixture(t)
	task.Spec.Env = []corev1.EnvVar{
		{Name: workerenv.EnforcementRequirementDigest, Value: "caller-spoof"},
		{Name: workerenv.WorkerPodName, Value: "caller-pod"},
		{Name: workerenv.WorkerPodUID, Value: "caller-uid"},
	}
	for _, enabled := range []bool{true, false} {
		current := task.DeepCopy()
		if !enabled {
			current.Spec.Enforcement = nil
		}
		job, err := setupJobBuilder().Build(t.Context(), current, nil, nil)
		require.NoError(t, err)
		container := job.Spec.Template.Spec.Containers[0]
		command := "/worker"
		if enabled {
			command = "/worker-enforced"
		}
		require.Equal(t, []string{command}, container.Command)
		for _, name := range []string{workerenv.EnforcementRequirementDigest, workerenv.WorkerPodName, workerenv.WorkerPodUID} {
			matches := 0
			for _, env := range container.Env {
				if env.Name == name {
					matches++
					if name == workerenv.EnforcementRequirementDigest {
						require.NotEqual(t, "caller-spoof", env.Value)
					} else {
						require.NotNil(t, env.ValueFrom)
						require.NotNil(t, env.ValueFrom.FieldRef)
					}
				}
			}
			if enabled {
				require.Equal(t, 1, matches)
			} else {
				require.Zero(t, matches)
			}
		}
	}
}
