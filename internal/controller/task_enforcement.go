package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/enforcement"
	execevents "github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/workerenv"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const ConditionTypeEnforcementReady = "EnforcementReady"

func sameEnforcementAssociation(a, b corev1alpha1.ContainerEnforcementBinding) bool {
	a.Admitted, b.Admitted, a.Published, b.Published = false, false, false, false
	return reflect.DeepEqual(a, b)
}

func (r *TaskReconciler) validateTaskEnforcement(task *corev1alpha1.Task) error {
	if task.Spec.Enforcement == nil {
		return nil
	}
	if _, err := enforcement.TaskRequirement(task); err != nil {
		return &enforcement.RefusalError{Needs: []string{"complete-policy"}}
	}
	if r.Enforcement == nil {
		return &enforcement.RefusalError{Needs: []string{"operator-configuration"}}
	}
	if task.Spec.Type != corev1alpha1.TaskTypeAI && task.Spec.Type != corev1alpha1.TaskTypeAgent {
		return &enforcement.RefusalError{Needs: []string{"supported-runtime"}}
	}
	if task.Spec.Type == corev1alpha1.TaskTypeAgent && task.Spec.Execution != nil && task.Spec.Execution.Workspace != nil {
		return &enforcement.RefusalError{Needs: []string{"supported-runtime"}}
	}
	return nil
}

func (r *TaskReconciler) markEnforcement(ctx context.Context, task *corev1alpha1.Task, status metav1.ConditionStatus, reason, message string, needs ...string) error {
	expected := task.DeepCopy()
	var conflict error
	err := r.updateStatusWithRetry(ctx, task, func(current *corev1alpha1.Task) {
		conflict = nil
		if !sameEnforcementObservation(current, expected) {
			conflict = errors.New("Task attempt or enforcement observation changed before outcome publication")
			return
		}
		outcome := enforcement.Outcome(reason, needs...)
		outcome.ObservedGeneration = expected.Generation
		current.Status.EnforcementOutcome = outcome
		meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
			Type: ConditionTypeEnforcementReady, Status: status,
			Reason: reason, Message: sanitizeStatusMessage(message),
			ObservedGeneration: current.Generation, LastTransitionTime: metav1.Now(),
		})
	})
	return errors.Join(err, conflict)
}

func sameEnforcementObservation(current, expected *corev1alpha1.Task) bool {
	return current.UID == expected.UID && current.Generation == expected.Generation &&
		current.Status.Phase == expected.Status.Phase && current.Status.JobUID == expected.Status.JobUID &&
		current.Status.Attempts == expected.Status.Attempts &&
		reflect.DeepEqual(current.Status.EnforcementAdmission, expected.Status.EnforcementAdmission) &&
		reflect.DeepEqual(current.Status.EnforcementOutcome, expected.Status.EnforcementOutcome) &&
		reflect.DeepEqual(current.Status.AgentExecutionBinding, expected.Status.AgentExecutionBinding) &&
		reflect.DeepEqual(meta.FindStatusCondition(current.Status.Conditions, ConditionTypeEnforcementReady),
			meta.FindStatusCondition(expected.Status.Conditions, ConditionTypeEnforcementReady)) &&
		sameEnforcementExecutionAttempt(current.Status.Execution, expected.Status.Execution)
}

func sameEnforcementExecutionAttempt(left, right *corev1alpha1.TaskExecutionStatus) bool {
	if left == nil || right == nil {
		return left == right
	}
	return left.Attempt == right.Attempt && left.PromptID == right.PromptID &&
		left.RuntimePoolUID == right.RuntimePoolUID &&
		left.RuntimeInstanceID == right.RuntimeInstanceID &&
		left.RuntimeSessionUID == right.RuntimeSessionUID &&
		left.RuntimeSessionGeneration == right.RuntimeSessionGeneration &&
		left.RequestDigest == right.RequestDigest && left.ControllerEpoch == right.ControllerEpoch
}

func activeEnforcementAttempt(task *corev1alpha1.Task, uid types.UID, jobUID string) bool {
	_, cancelling := requestedTaskCancellation(task)
	if task.UID != uid || !task.DeletionTimestamp.IsZero() || cancelling ||
		task.Status.Phase != corev1alpha1.TaskPhaseRunning || task.Status.JobUID != jobUID {
		return false
	}
	return task.Spec.Timeout == nil || task.Status.StartTime == nil ||
		time.Now().Before(task.Status.StartTime.Add(task.Spec.Timeout.Duration))
}

func (r *TaskReconciler) persistEnforcementBinding(ctx context.Context, task *corev1alpha1.Task, binding corev1alpha1.ContainerEnforcementBinding, admitted bool) error {
	written := false
	uid := task.UID
	var conflict error
	err := r.updateStatusWithRetry(ctx, task, func(current *corev1alpha1.Task) {
		written, conflict = false, nil
		if !activeEnforcementAttempt(current, uid, binding.JobUID) {
			return
		}
		requirement, err := enforcement.TaskRequirement(current)
		if err != nil || requirement == nil {
			conflict = errors.New("required enforcement changed before admission")
			return
		}
		digest, err := enforcement.RequirementDigest(requirement)
		if err != nil || digest != binding.RequirementDigest {
			conflict = errors.New("required enforcement changed before admission")
			return
		}
		for index, prior := range current.Status.EnforcementBindings {
			if prior.BindingID != binding.BindingID {
				if prior.JobUID == binding.JobUID {
					conflict = errors.New("previous selected physical worker must stop and retire before replacement")
					return
				}
				continue
			}
			if !sameEnforcementAssociation(prior, binding) {
				conflict = errors.New("persisted physical enforcement association changed")
				return
			}
			if admitted {
				current.Status.EnforcementBindings[index].Admitted = true
			}
			written = true
			return
		}
		if admitted {
			conflict = errors.New("enforcement cleanup intent must precede provider activation")
			return
		}
		current.Status.EnforcementBindings = append(current.Status.EnforcementBindings, binding)
		written = true
	})
	if err != nil {
		return err
	}
	if conflict != nil {
		return conflict
	}
	if !written {
		return errors.New("Task attempt stopped before enforcement admission")
	}
	return nil
}

func (r *TaskReconciler) reconcileNativeEnforcement(ctx context.Context, task *corev1alpha1.Task, job *batchv1.Job) (ctrl.Result, bool, error) {
	if task.Spec.Enforcement == nil {
		return ctrl.Result{}, false, nil
	}
	if err := r.validateTaskEnforcement(task); err != nil {
		needs := enforcement.RefusalNeeds(err)
		message := enforcement.OutcomeMessage(enforcement.Outcome("EnforcementRefused", needs...))
		if statusErr := r.markEnforcement(ctx, task, metav1.ConditionFalse, "EnforcementRefused", message, needs...); statusErr != nil {
			return ctrl.Result{}, true, statusErr
		}
		result, failErr := r.failTask(ctx, task, err.Error())
		return result, true, failErr
	}
	if job.UID == "" || task.Status.JobUID != string(job.UID) || !metav1.IsControlledBy(job, task) {
		result, err := r.failTask(ctx, task, "enforcement requires the exact owned worker Job")
		return result, true, err
	}
	if job.Status.Succeeded > 0 || job.Status.Failed > 0 {
		published := false
		for _, binding := range slices.Clone(task.Status.EnforcementBindings) {
			if binding.JobUID != string(job.UID) {
				continue
			}
			observed, err := r.recoverNativeEnforcementPublication(ctx, task, &binding)
			if err != nil {
				return ctrl.Result{}, true, err
			}
			published = published || observed
		}
		if !published {
			message := "worker ended without a verified enforcement release publication"
			if job.Status.Failed > 0 {
				if task.Spec.Timeout != nil && jobFailedDueToActiveDeadline(job) {
					result, err := r.failTask(ctx, task, timedOutTaskMessage(task))
					return result, true, err
				}
				if r.shouldRetry(task) {
					result, err := r.retryTaskAfterFailure(ctx, task, r.diagnoseFailedJob(ctx, task))
					return result, true, err
				}
				message = r.diagnoseFailedJob(ctx, task)
			}
			result, err := r.failTask(ctx, task, message)
			return result, true, err
		}
		return ctrl.Result{}, false, nil
	}
	reader := r.taskMetadataReader()
	var pods corev1.PodList
	if err := reader.List(ctx, &pods, client.InNamespace(task.Namespace),
		client.MatchingLabels{labels.LabelTask: labels.SelectorValue(task.Name)}); err != nil {
		return ctrl.Result{}, true, err
	}
	var pod *corev1.Pod
	for index := range pods.Items {
		candidate := &pods.Items[index]
		if !metav1.IsControlledBy(candidate, job) || candidate.DeletionTimestamp != nil ||
			candidate.Status.Phase == corev1.PodSucceeded || candidate.Status.Phase == corev1.PodFailed {
			continue
		}
		if pod != nil {
			return ctrl.Result{RequeueAfter: time.Second}, true,
				r.markEnforcement(ctx, task, metav1.ConditionFalse, "EnforcementPending", "waiting for one current prepared worker Pod")
		}
		pod = candidate
	}
	running := false
	if pod != nil {
		for _, status := range pod.Status.ContainerStatuses {
			running = running || (status.Name == "worker" && status.ContainerID != "" && status.State.Running != nil)
		}
	}
	if !running || pod.Spec.NodeName == "" {
		// Pod termination can precede Job completion. Retain the exact completed
		// container's historical admission without releasing or reactivating it.
		for _, binding := range task.Status.EnforcementBindings {
			if binding.JobUID != string(job.UID) || !binding.Admitted || !binding.Published {
				continue
			}
			for _, candidate := range pods.Items {
				if string(candidate.UID) != binding.PodUID || candidate.Spec.NodeName != binding.NodeName ||
					!metav1.IsControlledBy(&candidate, job) || (pod != nil && pod.UID != candidate.UID) {
					continue
				}
				for _, status := range candidate.Status.ContainerStatuses {
					if status.Name == "worker" && status.ContainerID == binding.ContainerID && status.State.Terminated != nil {
						return ctrl.Result{}, false, r.finishNativeEnforcementAdmission(ctx, task, &binding, task.DeepCopy())
					}
				}
			}
		}
		return ctrl.Result{}, false,
			r.markEnforcement(ctx, task, metav1.ConditionFalse, "EnforcementPending", "waiting for the prepared worker container")
	}
	requirement, err := enforcement.TaskRequirement(task)
	if err != nil {
		return ctrl.Result{}, true, err
	}
	requirementDigest, err := enforcement.RequirementDigest(requirement)
	if err != nil {
		return ctrl.Result{}, true, err
	}
	if len(pod.Spec.Containers) != 1 || pod.Spec.Containers[0].Name != "worker" ||
		!slices.Equal(pod.Spec.Containers[0].Command, []string{"/worker-enforced"}) ||
		pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		result, err := r.failTask(ctx, task, "worker image/Pod does not provide qualified enforcement bootstrap")
		return result, true, err
	}
	digestInjected := false
	for _, env := range pod.Spec.Containers[0].Env {
		if env.Name == workerenv.EnforcementRequirementDigest {
			digestInjected = env.ValueFrom == nil && env.Value == requirementDigest
		}
	}
	if !digestInjected {
		result, err := r.failTask(ctx, task, "worker requirement does not match its frozen bootstrap")
		return result, true, err
	}
	node := &corev1.Node{}
	if err := reader.Get(ctx, client.ObjectKey{Name: pod.Spec.NodeName}, node); err != nil {
		return ctrl.Result{}, true, err
	}
	providerSetDigest, err := r.Enforcement.ProviderSetDigest(node.Name, string(node.UID))
	if err != nil {
		message := enforcement.OutcomeMessage(enforcement.Outcome("EnforcementRefused"))
		if statusErr := r.markEnforcement(ctx, task, metav1.ConditionFalse, "EnforcementRefused", message); statusErr != nil {
			return ctrl.Result{}, true, statusErr
		}
		result, failErr := r.failTask(ctx, task, message)
		return result, true, failErr
	}
	binding, err := enforcement.BindContainer(pod, node, "worker", requirementDigest, providerSetDigest)
	if err != nil {
		return ctrl.Result{}, true, err
	}
	binding.JobUID = string(job.UID)
	for _, prior := range slices.Clone(task.Status.EnforcementBindings) {
		if prior.JobUID == binding.JobUID && prior.BindingID != binding.BindingID {
			retired, err := r.retireNativeEnforcementBinding(ctx, task, prior)
			if err != nil {
				return ctrl.Result{}, true, err
			}
			if !retired {
				return ctrl.Result{RequeueAfter: time.Second}, true,
					r.markEnforcement(ctx, task, metav1.ConditionFalse, "EnforcementPending", "waiting for the previously selected physical worker to stop and retire", "stopped-previous-container")
			}
		}
	}
	approved, approvalErr := enforcement.PodApproved(pod, binding.PodUID, "worker", requirementDigest)
	for _, prior := range task.Status.EnforcementBindings {
		if prior.BindingID == binding.BindingID && prior.Admitted && approved && approvalErr == nil {
			if !sameEnforcementAssociation(prior, *binding) {
				return ctrl.Result{}, true, errors.New("admitted physical enforcement association changed")
			}
			_, err := r.observeNativeEnforcementPublication(ctx, task, &prior, pod, task.DeepCopy())
			return ctrl.Result{}, false, err
		}
	}
	if err := r.persistEnforcementBinding(ctx, task, *binding, false); err != nil {
		return ctrl.Result{}, true, err
	}
	// This bounds one RPC attempt only. It never expires a Task, binding or
	// policy; ordinary Task timeout and cancellation remain authoritative.
	callCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	observation := task.DeepCopy()
	proofs, outcome, err := r.Enforcement.Ensure(callCtx, binding, requirement)
	if err != nil {
		logf.FromContext(ctx).Error(err, "required container enforcement uncertain", "bindingID", binding.BindingID)
		message := enforcement.OutcomeMessage(enforcement.Outcome("EnforcementUncertain"))
		if statusErr := r.markEnforcement(ctx, task, metav1.ConditionUnknown, "EnforcementUncertain", message); statusErr != nil {
			return ctrl.Result{}, true, statusErr
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, true, nil
	}
	if outcome != nil {
		if refused := outcome.GetRefused(); refused != nil {
			message := enforcement.OutcomeMessage(enforcement.Outcome("EnforcementRefused", refused.Needs...))
			if err := r.markEnforcement(ctx, task, metav1.ConditionFalse, "EnforcementRefused", message, refused.Needs...); err != nil {
				return ctrl.Result{}, true, err
			}
			result, err := r.failTask(ctx, task, message)
			return result, true, err
		}
		if outcome.GetPending() != nil {
			return ctrl.Result{RequeueAfter: time.Second}, true,
				r.markEnforcement(ctx, task, metav1.ConditionFalse, "EnforcementPending",
					enforcement.OutcomeMessage(enforcement.Outcome("EnforcementPending", "provider-recovery")), "provider-recovery")
		}
	}
	if outcome != nil || len(proofs) == 0 {
		return ctrl.Result{}, true, errors.New("mandatory providers returned no complete enforcement authority")
	}
	if err := r.persistEnforcementBinding(ctx, task, *binding, true); err != nil {
		return ctrl.Result{}, true, err
	}
	binding.Admitted = true
	latest := &corev1alpha1.Task{}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(task), latest); err != nil {
		return ctrl.Result{}, true, err
	}
	if !activeEnforcementAttempt(latest, task.UID, binding.JobUID) {
		return ctrl.Result{}, true, errors.New("Task attempt stopped before worker release")
	}
	admission, err := json.Marshal(enforcement.PodAdmission{
		Version: enforcement.PodAdmissionVersion, BindingID: binding.BindingID,
		RequirementDigest: requirementDigest, PodUID: binding.PodUID, ContainerID: binding.ContainerID,
	})
	if err != nil {
		return ctrl.Result{}, true, err
	}
	before := pod.DeepCopy()
	if pod.Annotations == nil {
		pod.Annotations = make(map[string]string)
	}
	pod.Annotations[enforcement.PodAdmissionAnnotation] = string(admission)
	if err := r.Patch(ctx, pod, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		return ctrl.Result{}, true, fmt.Errorf("publish exact-incarnation worker admission: %w", err)
	}
	_, err = r.observeNativeEnforcementPublication(ctx, task, binding, pod, observation)
	return ctrl.Result{}, false, err
}

func (r *TaskReconciler) recoverNativeEnforcementPublication(ctx context.Context, task *corev1alpha1.Task, binding *corev1alpha1.ContainerEnforcementBinding) (bool, error) {
	observation := task.DeepCopy()
	if binding.Published {
		return true, r.finishNativeEnforcementAdmission(ctx, task, binding, observation)
	}
	pod := &corev1.Pod{}
	if err := r.taskMetadataReader().Get(ctx, client.ObjectKey{
		Namespace: binding.PodNamespace, Name: binding.PodName,
	}, pod); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return r.observeNativeEnforcementPublication(ctx, task, binding, pod, observation)
}

func (r *TaskReconciler) observeNativeEnforcementPublication(ctx context.Context, task *corev1alpha1.Task, binding *corev1alpha1.ContainerEnforcementBinding, pod *corev1.Pod, observation *corev1alpha1.Task) (bool, error) {
	if !binding.Admitted || string(pod.UID) != binding.PodUID {
		return false, nil
	}
	receipt, err := enforcement.PublishedPodAdmission(pod)
	if err != nil || receipt == nil {
		return false, err
	}
	expected := enforcement.PodAdmission{
		Version: enforcement.PodAdmissionVersion, BindingID: binding.BindingID,
		RequirementDigest: binding.RequirementDigest, PodUID: binding.PodUID, ContainerID: binding.ContainerID,
	}
	owner := metav1.GetControllerOf(pod)
	if *receipt != expected || owner == nil || string(owner.UID) != binding.JobUID ||
		owner.Kind != "Job" || owner.APIVersion != batchv1.SchemeGroupVersion.String() || pod.Spec.NodeName != binding.NodeName {
		return false, errors.New("published release does not match its persisted physical association")
	}
	if !binding.Published {
		written := false
		uid := task.UID
		err := r.updateStatusWithRetry(ctx, task, func(current *corev1alpha1.Task) {
			written = false
			if current.UID != uid {
				return
			}
			for index, prior := range current.Status.EnforcementBindings {
				if prior.BindingID == binding.BindingID && prior.Admitted && sameEnforcementAssociation(prior, *binding) {
					current.Status.EnforcementBindings[index].Published = true
					written = true
					return
				}
			}
		})
		if err != nil {
			return false, err
		}
		if !written {
			return false, errors.New("physical release receipt no longer has its persisted cleanup association")
		}
		binding.Published = true
	}
	return true, r.finishNativeEnforcementAdmission(ctx, task, binding, observation)
}

func (r *TaskReconciler) finishNativeEnforcementAdmission(ctx context.Context, task *corev1alpha1.Task, binding *corev1alpha1.ContainerEnforcementBinding, observation *corev1alpha1.Task) error {
	if r.ExecutionEventStore != nil {
		events, ok := r.ExecutionEventStore.(store.DeduplicatingExecutionEventStore)
		if !ok {
			return errors.New("enforcement admission requires an idempotent execution event store")
		}
		_, _, err := events.AppendExecutionEventIfAbsent(ctx, &store.ExecutionEvent{
			Namespace: task.Namespace, StreamType: store.ExecutionEventStreamTypeTask,
			StreamID: task.Name, TaskName: task.Name, SessionName: r.executionEventSessionName(ctx, task),
			Type: execevents.ExecutionEventTypeTaskStarted, Severity: execevents.ExecutionEventSeverityInfo,
			Summary: "Task worker admitted after complete container enforcement",
		}, "enforcement-admission:"+string(task.UID)+":"+binding.BindingID)
		if err != nil {
			return fmt.Errorf("record worker enforcement admission: %w", err)
		}
	}
	message := enforcementAdmissionMessage(binding)
	condition := meta.FindStatusCondition(task.Status.Conditions, ConditionTypeEnforcementReady)
	if condition != nil && condition.Status == metav1.ConditionTrue && condition.Reason == "EnforcementVerified" &&
		condition.ObservedGeneration == task.Generation && condition.Message == message &&
		task.Status.EnforcementAdmission != nil && task.Status.EnforcementOutcome == nil &&
		*task.Status.EnforcementAdmission == enforcementAdmission(task.Generation, binding) {
		return nil
	}
	return r.markEnforcementAdmission(ctx, task, binding, observation)
}

func enforcementAdmissionMessage(binding *corev1alpha1.ContainerEnforcementBinding) string {
	return fmt.Sprintf("container binding %s verified for %s; admission record, not a live provider check", binding.BindingID, binding.RequirementDigest)
}

func enforcementAdmission(generation int64, binding *corev1alpha1.ContainerEnforcementBinding) corev1alpha1.EnforcementAdmission {
	return corev1alpha1.EnforcementAdmission{
		BindingID: binding.BindingID, RequirementDigest: binding.RequirementDigest, ObservedGeneration: generation,
	}
}

func (r *TaskReconciler) markEnforcementAdmission(ctx context.Context, task *corev1alpha1.Task, binding *corev1alpha1.ContainerEnforcementBinding, observation *corev1alpha1.Task) error {
	expected := observation.DeepCopy()
	generation := expected.Generation
	var conflict error
	err := r.updateStatusWithRetry(ctx, task, func(current *corev1alpha1.Task) {
		conflict = nil
		requirement, err := enforcement.TaskRequirement(current)
		if err != nil || requirement == nil || !sameEnforcementObservation(current, expected) {
			conflict = errors.New("Task changed before enforcement admission observation")
			return
		}
		digest, err := enforcement.RequirementDigest(requirement)
		if err != nil || digest != binding.RequirementDigest {
			conflict = errors.New("Task requirement changed before enforcement admission observation")
			return
		}
		admission := enforcementAdmission(generation, binding)
		if binding.JobUID != "" && binding.JobUID != current.Status.JobUID {
			if current.Status.EnforcementAdmission == nil {
				current.Status.EnforcementAdmission = &admission
			}
			return
		}
		current.Status.EnforcementAdmission = &admission
		current.Status.EnforcementOutcome = nil
		meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
			Type: ConditionTypeEnforcementReady, Status: metav1.ConditionTrue,
			Reason: "EnforcementVerified", Message: enforcementAdmissionMessage(binding),
			ObservedGeneration: generation, LastTransitionTime: metav1.Now(),
		})
	})
	return errors.Join(err, conflict)
}

func (r *TaskReconciler) cleanupNativeEnforcement(ctx context.Context, task *corev1alpha1.Task, keepJobUID string) (bool, error) {
	for _, binding := range slices.Clone(task.Status.EnforcementBindings) {
		if keepJobUID != "" && binding.JobUID == keepJobUID {
			continue
		}
		retired, err := r.retireNativeEnforcementBinding(ctx, task, binding)
		if err != nil || !retired {
			return false, err
		}
	}
	return true, nil
}

func (r *TaskReconciler) retireNativeEnforcementBinding(ctx context.Context, task *corev1alpha1.Task, binding corev1alpha1.ContainerEnforcementBinding) (bool, error) {
	if r.Enforcement == nil {
		return false, errors.New("enforcement cleanup still requires its operator-trusted provider registry")
	}
	published, projectionErr := r.recoverNativeEnforcementPublication(ctx, task, &binding)
	if !published && binding.Admitted && projectionErr == nil {
		projectionErr = r.markEnforcement(ctx, task, metav1.ConditionUnknown, "EnforcementReleaseUnobserved",
			fmt.Sprintf("binding %s was authorized, but its release publication was not observed", binding.BindingID))
	}
	pod := &corev1.Pod{}
	if err := r.taskMetadataReader().Get(ctx, client.ObjectKey{
		Namespace: binding.PodNamespace, Name: binding.PodName,
	}, pod); err != nil && !apierrors.IsNotFound(err) {
		return false, errors.Join(projectionErr, err)
	}
	if string(pod.UID) == binding.PodUID && pod.UID != "" && pod.DeletionTimestamp == nil &&
		pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
		uid := pod.UID
		if err := r.Delete(ctx, pod, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}); err != nil && !apierrors.IsNotFound(err) {
			return false, errors.Join(projectionErr, fmt.Errorf("stop owned enforcement target: %w", err))
		}
	}
	callCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	retired, err := r.Enforcement.Retire(callCtx, &binding)
	cancel()
	if projectionErr != nil || err != nil || !retired {
		return false, errors.Join(projectionErr, err)
	}
	removed := false
	uid := task.UID
	if err := r.updateStatusWithRetry(ctx, task, func(current *corev1alpha1.Task) {
		removed = false
		if current.UID != uid {
			return
		}
		for _, item := range current.Status.EnforcementBindings {
			if item.BindingID == binding.BindingID && (!sameEnforcementAssociation(item, binding) ||
				item.Admitted != binding.Admitted || item.Published != binding.Published) {
				return
			}
		}
		current.Status.EnforcementBindings = slices.DeleteFunc(current.Status.EnforcementBindings,
			func(item corev1alpha1.ContainerEnforcementBinding) bool {
				return item.BindingID == binding.BindingID
			})
		removed = true
	}); err != nil {
		return false, err
	}
	if !removed {
		return false, errors.New("physical enforcement association or publication changed during cleanup")
	}
	return true, nil
}
