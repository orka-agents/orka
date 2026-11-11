package kube

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	pv "github.com/orka-agents/orka/internal/patchverification"
)

func (s *Service) reconcileSubmission(ctx context.Context, submission *pv.KubernetesSubmission) error {
	if submission.State == pv.SubmissionCancelling {
		if err := s.recoverCancelledPreparation(ctx, submission); err != nil {
			return err
		}
		if submission.Binding != nil {
			if _, err := s.store.CancelPatchVerificationRun(ctx, *submission.Binding); err != nil {
				return err
			}
		}
		done, err := s.cleanupSubmission(ctx, submission, "validation was cancelled")
		if err != nil || !done {
			return err
		}
		return s.store.CompleteKubernetesValidationSubmission(ctx, submission.Namespace, submission.RequestID, "validation was cancelled")
	}
	if err := s.validatePreparationOwner(ctx, submission); err != nil {
		if !errors.Is(err, pv.ErrBinding) {
			return err
		}
		return s.failSubmission(ctx, submission, "validation preparation or parent identity is unavailable")
	}
	if time.Since(submission.CreatedAt) > s.config.RunTimeout {
		return s.failSubmission(ctx, submission, "validation exceeded its whole-run deadline")
	}
	if submission.State == pv.SubmissionPreparing {
		return s.prepareSubmission(ctx, submission)
	}
	if submission.State != pv.SubmissionRunning || submission.Binding == nil {
		return fmt.Errorf("validation submission has no execution binding")
	}
	record, err := s.store.GetPatchVerificationRun(ctx, submission.RunID)
	if err != nil && !errors.Is(err, pv.ErrIntegrity) {
		return err
	}
	if record == nil {
		return fmt.Errorf("validation evidence record is unavailable")
	}
	if record.State != pv.RunRunning {
		done, err := s.cleanupSubmission(ctx, submission, "")
		if err != nil || !done {
			return err
		}
		return s.store.CompleteKubernetesValidationSubmission(ctx, submission.Namespace, submission.RequestID, "")
	}
	if len(pv.MissingRequirements(submission.Manifest.Environment)) != 0 {
		_, err := s.store.FinalizePatchVerificationRun(ctx, *submission.Binding)
		return err
	}
	return s.advanceChecks(ctx, submission, record)
}

func (s *Service) recoverCancelledPreparation(ctx context.Context, submission *pv.KubernetesSubmission) error {
	if submission.Binding != nil || submission.OriginalTaskUID == "" ||
		(submission.Manifest.Action == pv.VerifyPatch && submission.PatchedTaskUID == "") {
		return nil
	}
	binding, err := pv.NewRunBinding(submission.Manifest, submission.AttemptID, submission.OriginalTaskUID, submission.PatchedTaskUID)
	if err != nil {
		return err
	}
	if _, err := s.store.GetPatchVerificationRun(ctx, binding.RunID); err != nil {
		if errors.Is(err, pv.ErrRunNotFound) {
			return nil
		}
		return err
	}
	if _, err := s.store.CancelPatchVerificationRun(ctx, binding); err != nil {
		return err
	}
	if err := s.store.AttachCancelledKubernetesValidationRun(ctx, submission.Namespace, submission.RequestID, binding); err != nil {
		return err
	}
	submission.Binding, submission.RunID = &binding, binding.RunID
	return nil
}

func (s *Service) advanceChecks(ctx context.Context, submission *pv.KubernetesSubmission, record *pv.Record) error {
	for _, side := range pv.ActionSides(submission.Manifest.Action) {
		if side == pv.Patched {
			observations := make([]pv.Observation, 0, len(record.Evidence))
			for _, evidence := range record.Evidence {
				observations = append(observations, evidence.Observation)
			}
			if !pv.OriginalReady(submission.Manifest, *submission.Binding, observations) {
				_, err := s.store.FinalizePatchVerificationRun(ctx, *submission.Binding)
				return err
			}
		}
		name := submission.OriginalTaskName
		if side == pv.Patched {
			name = submission.PatchedTaskName
		}
		task := &corev1alpha1.Task{}
		if err := s.reader.Get(ctx, client.ObjectKey{Namespace: submission.Namespace, Name: name}, task); err != nil {
			if apierrors.IsNotFound(err) {
				return s.failSubmission(ctx, submission, "bound validation Task disappeared")
			}
			return err
		}
		if err := s.validateTask(submission, task); err != nil {
			return s.failSubmission(ctx, submission, "bound validation Task identity changed")
		}
		if !task.DeletionTimestamp.IsZero() || task.Status.Phase == corev1alpha1.TaskPhaseCancelled {
			return s.store.RequestKubernetesValidationCancellation(ctx, submission.Namespace, submission.RequestID)
		}
		for _, check := range submission.Manifest.Checks {
			dispatch, err := s.store.GetKubernetesValidationDispatch(ctx, submission.RunID, side, check.ID)
			if errors.Is(err, pv.ErrRunNotFound) {
				return s.createCheckJob(ctx, submission, task, side, check)
			}
			if err != nil {
				return err
			}
			switch dispatch.State {
			case pv.DispatchPlanned:
				return s.resumePlannedJob(ctx, submission, task, check, dispatch)
			case pv.DispatchCreated, pv.DispatchObserved:
				_, err := s.observeCheckJob(ctx, submission, task, check, dispatch)
				return err
			case pv.DispatchRecorded:
				continue
			case pv.DispatchFailed:
				return s.failSubmission(ctx, submission, dispatch.Failure)
			default:
				return s.failSubmission(ctx, submission, "validation dispatch has an unsupported state")
			}
		}
	}
	_, err := s.store.FinalizePatchVerificationRun(ctx, *submission.Binding)
	return err
}

func (s *Service) createCheckJob(ctx context.Context, submission *pv.KubernetesSubmission, task *corev1alpha1.Task, side string, check pv.Check) error {
	if s.renderJob == nil {
		return fmt.Errorf("validation Job renderer is unavailable")
	}
	job, err := s.renderJob(ctx, task)
	if err != nil {
		return err
	}
	expected, err := s.buildCheckJob(submission, task, check)
	if err != nil {
		return err
	}
	if job == nil || job.Name != expected.Name || job.Namespace != expected.Namespace ||
		!metav1.IsControlledBy(job, task) || !jobExecutionMatches(expected, job) {
		return fmt.Errorf("validation Job renderer changed the bound workload")
	}
	spec, err := json.Marshal(job.Spec)
	if err != nil {
		return err
	}
	dispatch := pv.KubernetesDispatch{RunID: submission.RunID, Side: side, CheckID: check.ID,
		JobName: job.Name, SpecDigest: pv.Digest(spec), State: pv.DispatchPlanned}
	if err := s.store.CreateKubernetesValidationDispatch(ctx, dispatch); err != nil {
		return err
	}
	return s.resumePlannedJob(ctx, submission, task, check, &dispatch)
}

func (s *Service) resumePlannedJob(ctx context.Context, submission *pv.KubernetesSubmission, task *corev1alpha1.Task, check pv.Check, dispatch *pv.KubernetesDispatch) error {
	job, err := s.buildCheckJob(submission, task, check)
	if err != nil {
		return err
	}
	spec, err := json.Marshal(job.Spec)
	if err != nil || pv.Digest(spec) != dispatch.SpecDigest {
		return s.store.MarkKubernetesValidationDispatchFailed(ctx, dispatch.RunID, dispatch.Side, check.ID, "frozen Job specification changed")
	}
	existing := &batchv1.Job{}
	err = s.reader.Get(ctx, client.ObjectKeyFromObject(job), existing)
	if err == nil {
		if existing.UID == "" || !metav1.IsControlledBy(existing, task) || !jobExecutionMatches(job, existing) {
			return s.store.MarkKubernetesValidationDispatchFailed(ctx, dispatch.RunID, dispatch.Side, check.ID, "existing Job identity or execution specification changed")
		}
		return s.store.MarkKubernetesValidationDispatchCreated(ctx, dispatch.RunID, dispatch.Side, check.ID, string(existing.UID))
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	if submission.Manifest.Environment.Profile == pv.LocalServices {
		if err := s.checkCanary(ctx); err != nil {
			return s.store.MarkKubernetesValidationDispatchFailed(ctx, dispatch.RunID, dispatch.Side, check.ID, "trusted network canary is not reachable from the controller")
		}
	}
	bundle, err := s.validationInputBundle(submission, dispatch.Side, check.ID)
	if err != nil {
		return s.store.MarkKubernetesValidationDispatchFailed(ctx, dispatch.RunID, dispatch.Side, check.ID, "input bundle creation failed")
	}
	input := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: job.Name + "-input", Namespace: job.Namespace},
		Immutable: new(true), BinaryData: map[string][]byte{"bundle.gz": bundle}}
	if err := s.OwnObject(task, input); err != nil {
		return err
	}
	if err := s.client.Create(ctx, input); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return err
		}
		if preparedSubmission(submission) {
			return s.store.MarkKubernetesValidationDispatchFailed(ctx, dispatch.RunID, dispatch.Side, check.ID,
				"accepted validation Job admission cannot be replayed")
		}
		stored := &corev1.ConfigMap{}
		if err := s.reader.Get(ctx, client.ObjectKeyFromObject(input), stored); err != nil {
			return err
		}
		if !metav1.IsControlledBy(stored, task) || !ptr.Deref(stored.Immutable, false) ||
			!reflect.DeepEqual(stored.BinaryData, input.BinaryData) || len(stored.Data) != 0 {
			return s.store.MarkKubernetesValidationDispatchFailed(ctx, dispatch.RunID, dispatch.Side, check.ID, "input ConfigMap identity changed")
		}
	}
	if err := s.ensureNetworkPolicy(ctx, task, job); err != nil {
		if permanentAPIError(err) {
			return s.store.MarkKubernetesValidationDispatchFailed(ctx, dispatch.RunID, dispatch.Side, check.ID, "Kubernetes rejected network isolation policy")
		}
		return err
	}
	if err := s.client.Create(ctx, job); err != nil {
		if permanentAPIError(err) {
			return s.store.MarkKubernetesValidationDispatchFailed(ctx, dispatch.RunID, dispatch.Side, check.ID, "Kubernetes rejected validation Job admission")
		}
		return err
	}

	if job.UID == "" {
		return s.store.MarkKubernetesValidationDispatchFailed(ctx, dispatch.RunID, dispatch.Side, check.ID, "Kubernetes API returned Job without UID")
	}
	return s.store.MarkKubernetesValidationDispatchCreated(ctx, dispatch.RunID, dispatch.Side, check.ID, string(job.UID))
}

func (s *Service) observeCheckJob(ctx context.Context, submission *pv.KubernetesSubmission, task *corev1alpha1.Task, check pv.Check, dispatch *pv.KubernetesDispatch) (ctrl.Result, error) {
	job := &batchv1.Job{}
	if err := s.reader.Get(ctx, client.ObjectKey{Namespace: task.Namespace, Name: dispatch.JobName}, job); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, s.store.MarkKubernetesValidationDispatchFailed(ctx, dispatch.RunID, dispatch.Side, dispatch.CheckID, "created Job disappeared")
		}
		return ctrl.Result{}, err
	}
	if string(job.UID) != dispatch.JobUID || !metav1.IsControlledBy(job, task) {
		return ctrl.Result{}, s.store.MarkKubernetesValidationDispatchFailed(ctx, dispatch.RunID, dispatch.Side, dispatch.CheckID, "Job identity changed")
	}
	expected, err := s.buildCheckJob(submission, task, check)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !jobExecutionMatches(expected, job) {
		return ctrl.Result{}, s.store.MarkKubernetesValidationDispatchFailed(ctx, dispatch.RunID, dispatch.Side, check.ID, "Job execution specification changed")
	}
	if err := s.ensureNetworkPolicy(ctx, task, expected); err != nil {
		return ctrl.Result{}, err
	}
	pods := &corev1.PodList{}
	if err := s.reader.List(ctx, pods, client.InNamespace(task.Namespace), client.MatchingLabels{"job-name": job.Name}); err != nil {
		return ctrl.Result{}, err
	}
	if len(pods.Items) == 0 {
		if job.Status.Failed > 0 || time.Since(job.CreationTimestamp.Time) > time.Duration(check.TimeoutSeconds+150)*time.Second {
			return ctrl.Result{}, s.store.MarkKubernetesValidationDispatchFailed(ctx, dispatch.RunID, dispatch.Side, check.ID, "Job failed without a usable Pod")
		}
		return ctrl.Result{RequeueAfter: s.config.ReconcileInterval}, nil
	}
	if len(pods.Items) != 1 {
		return ctrl.Result{}, s.store.MarkKubernetesValidationDispatchFailed(ctx, dispatch.RunID, dispatch.Side, dispatch.CheckID, "Job created multiple Pods")
	}
	pod := &pods.Items[0]
	if !metav1.IsControlledBy(pod, job) || !labelsMatch(expected.Spec.Template.Labels, pod.Labels) ||
		!podExecutionMatches(expected.Spec.Template.Spec, pod.Spec) {
		return ctrl.Result{}, s.store.MarkKubernetesValidationDispatchFailed(ctx, dispatch.RunID, dispatch.Side, dispatch.CheckID, "Pod identity or security spec changed")
	}
	status, terminated := validationContainerStatus(pod)
	if !terminated {
		if pod.Status.Phase == corev1.PodFailed || time.Since(job.CreationTimestamp.Time) > time.Duration(check.TimeoutSeconds+150)*time.Second {
			return ctrl.Result{}, s.store.MarkKubernetesValidationDispatchFailed(ctx, dispatch.RunID, dispatch.Side, check.ID, "Pod setup or execution deadline failed")
		}
		return ctrl.Result{RequeueAfter: s.config.ReconcileInterval}, nil
	}
	if !runtimeIdentityMatches(submission, status, pod) {
		return ctrl.Result{}, s.store.MarkKubernetesValidationDispatchFailed(ctx, dispatch.RunID, dispatch.Side, dispatch.CheckID, "runtime image identity mismatch")
	}
	if dispatch.State == pv.DispatchObserved && (dispatch.PodUID != string(pod.UID) || dispatch.ContainerID != status.ContainerID) {
		return ctrl.Result{}, s.store.MarkKubernetesValidationDispatchFailed(ctx, dispatch.RunID, dispatch.Side, check.ID, "observed Pod identity changed")
	}
	if dispatch.State == pv.DispatchCreated {
		if err := s.store.MarkKubernetesValidationDispatchObserved(ctx, dispatch.RunID, dispatch.Side, dispatch.CheckID, dispatch.JobUID, string(pod.UID), status.ContainerID); err != nil {
			return ctrl.Result{}, err
		}
		dispatch.PodUID, dispatch.ContainerID, dispatch.State = string(pod.UID), status.ContainerID, pv.DispatchObserved
	}
	return s.recordPodEvidence(ctx, submission, task, check, dispatch, pod, status)
}

func (s *Service) recordPodEvidence(
	ctx context.Context, submission *pv.KubernetesSubmission, task *corev1alpha1.Task, check pv.Check,
	dispatch *pv.KubernetesDispatch, pod *corev1.Pod, status corev1.ContainerStatus,
) (ctrl.Result, error) {
	report, err := s.readPodReport(ctx, pod.Namespace, pod.Name)
	if err != nil {
		if ctx.Err() != nil || IsTransientError(err) {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, s.store.MarkKubernetesValidationDispatchFailed(ctx, dispatch.RunID, dispatch.Side, dispatch.CheckID, "terminal Pod report unavailable")
	}
	if report.Version != pv.PodProtocolVersion || report.TaskUID != string(task.UID) || report.PodUID != string(pod.UID) ||
		report.Evidence.Observation.RunID != submission.RunID || report.Evidence.Observation.CheckID != check.ID ||
		report.Evidence.Observation.Side != dispatch.Side || report.Evidence.Observation.ImageID != submission.Manifest.Environment.ImageID {
		return ctrl.Result{}, s.store.MarkKubernetesValidationDispatchFailed(ctx, dispatch.RunID, dispatch.Side, dispatch.CheckID, "terminal Pod report identity mismatch")
	}
	report.Evidence.Observation.JobUID = dispatch.JobUID
	report.Evidence.Observation.PodUID = string(pod.UID)
	report.Evidence.Observation.ContainerID = status.ContainerID
	if err := s.store.RecordPatchVerificationEvidence(ctx, *submission.Binding, report.Evidence); err != nil {
		return ctrl.Result{}, err
	}
	if err := s.store.MarkKubernetesValidationDispatchRecorded(ctx, dispatch.RunID, dispatch.Side, dispatch.CheckID, dispatch.JobUID, string(pod.UID)); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: s.config.ReconcileInterval}, nil
}

func runtimeIdentityMatches(submission *pv.KubernetesSubmission, status corev1.ContainerStatus, pod *corev1.Pod) bool {
	helperID := imageDigest(submission.Manifest.Environment.Dependencies[helperImageIDKey])
	if len(pod.Status.InitContainerStatuses) != 1 || helperID == "" {
		return false
	}
	helper := pod.Status.InitContainerStatuses[0]
	return imageDigest(status.ImageID) == imageDigest(submission.Manifest.Environment.ImageID) &&
		status.ContainerID != "" && status.RestartCount == 0 && status.State.Terminated != nil &&
		status.State.Terminated.ExitCode == 0 && helper.Name == "install-runner" && helper.ContainerID != "" &&
		helper.RestartCount == 0 && helper.State.Terminated != nil && helper.State.Terminated.ExitCode == 0 &&
		imageDigest(helper.ImageID) == helperID
}

func jobExecutionMatches(expected, actual *batchv1.Job) bool {
	return ptr.Deref(actual.Spec.BackoffLimit, int32(-1)) == 0 &&
		ptr.Deref(actual.Spec.ActiveDeadlineSeconds, int64(-1)) == ptr.Deref(expected.Spec.ActiveDeadlineSeconds, int64(-2)) &&
		!ptr.Deref(actual.Spec.Suspend, false) && ptr.Deref(actual.Spec.Parallelism, int32(1)) == 1 &&
		(expected.Spec.PodReplacementPolicy == nil || reflect.DeepEqual(expected.Spec.PodReplacementPolicy, actual.Spec.PodReplacementPolicy)) &&
		reflect.DeepEqual(expected.Spec.PodFailurePolicy, actual.Spec.PodFailurePolicy) &&
		labelsMatch(expected.Spec.Template.Labels, actual.Spec.Template.Labels) &&
		actual.Spec.BackoffLimitPerIndex == nil && actual.Spec.MaxFailedIndexes == nil && actual.Spec.ManagedBy == nil &&
		ptr.Deref(actual.Spec.CompletionMode, batchv1.NonIndexedCompletion) == batchv1.NonIndexedCompletion &&
		ptr.Deref(actual.Spec.Completions, int32(1)) == 1 && podExecutionMatches(expected.Spec.Template.Spec, actual.Spec.Template.Spec)
}

func labelsMatch(expected, actual map[string]string) bool {
	for key, value := range expected {
		if observed, found := actual[key]; !found || observed != value {
			return false
		}
	}
	return true
}

func podExecutionMatches(expected, actual corev1.PodSpec) bool {
	if actual.HostNetwork || actual.HostPID || actual.HostIPC || ptr.Deref(actual.ShareProcessNamespace, false) ||
		ptr.Deref(actual.EnableServiceLinks, true) ||
		ptr.Deref(actual.AutomountServiceAccountToken, true) || len(actual.EphemeralContainers) != 0 ||
		actual.RuntimeClassName != nil || actual.RestartPolicy != corev1.RestartPolicyNever ||
		!reflect.DeepEqual(expected.SecurityContext, actual.SecurityContext) ||
		!reflect.DeepEqual(expected.NodeSelector, actual.NodeSelector) ||
		len(expected.Containers) != len(actual.Containers) || len(expected.InitContainers) != len(actual.InitContainers) ||
		len(expected.Volumes) != len(actual.Volumes) {
		return false
	}

	for index := range expected.Volumes {
		if !reflect.DeepEqual(expected.Volumes[index], actual.Volumes[index]) {
			return false
		}
	}
	containers := append(append([]corev1.Container{}, actual.InitContainers...), actual.Containers...)
	for index, container := range append(append([]corev1.Container{}, expected.InitContainers...), expected.Containers...) {
		observed := containers[index]
		observed.TerminationMessagePath, observed.TerminationMessagePolicy = "", ""
		container.TerminationMessagePath, container.TerminationMessagePolicy = "", ""
		if !reflect.DeepEqual(container, observed) {
			return false
		}
	}
	return true
}

// JobExecutionMatches also tolerates Kubernetes defaulting, but never admission
// changes to the execution, credentials, mounts, or isolation policy.
func JobExecutionMatches(expected, actual *batchv1.Job) bool {
	return jobExecutionMatches(expected, actual)
}

func PodExecutionMatches(expected, actual corev1.PodSpec) bool {
	return podExecutionMatches(expected, actual)
}

func (s *Service) cleanupSubmission(ctx context.Context, submission *pv.KubernetesSubmission, reason string) (bool, error) {
	allGone := true
	var record *pv.Record
	if submission.RunID != "" {
		var err error
		record, err = s.store.GetPatchVerificationRun(ctx, submission.RunID)
		if err != nil && !errors.Is(err, pv.ErrIntegrity) {
			return false, err
		}
	}
	for _, side := range pv.ActionSides(submission.Manifest.Action) {
		name, uid := submission.OriginalTaskName, submission.OriginalTaskUID
		if side == pv.Patched {
			name, uid = submission.PatchedTaskName, submission.PatchedTaskUID
		}
		task := &corev1alpha1.Task{}
		err := s.reader.Get(ctx, client.ObjectKey{Namespace: submission.Namespace, Name: name}, task)
		present := !apierrors.IsNotFound(err)
		if apierrors.IsNotFound(err) {
			if uid == "" {
				continue
			}
			task = &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: submission.Namespace, UID: types.UID(uid)}}
			err = nil
		}
		if err != nil {
			return false, err
		}
		if uid == "" && task.UID != "" {
			expected := s.validationTask(submission, side, name)
			if reflect.DeepEqual(task.Spec, expected.Spec) && reflect.DeepEqual(task.OwnerReferences, expected.OwnerReferences) &&
				validationAnnotationsMatch(task, expected) {
				if err := s.store.RecordKubernetesValidationTaskUID(ctx, submission.Namespace, submission.RequestID, side, string(task.UID)); err != nil {
					return false, err
				}
				uid = string(task.UID)
			}
		}
		if uid == "" || string(task.UID) != uid {
			// Never remove another Task's finalizer or dependents after name reuse.
			if submission.Manifest.Environment.Dependencies[pv.PreparationUIDKey] != "" {
				return false, pv.ErrBinding
			}
			continue
		}
		gone, err := s.cleanupTaskObjects(ctx, submission, task)
		if err != nil {
			return false, err
		}
		if !gone {
			allGone = false
			continue
		}
		if !present {
			continue
		}
		phase := validationCleanupPhase(submission, record)
		if task.Status.Phase != phase {
			base := task.DeepCopy()
			task.Status.Phase = phase
			task.Status.Message = reason
			if record != nil {
				task.Status.Message = string(record.Assessment.Conclusion) + ": " + record.Assessment.Reason
			}
			task.Status.CompletionTime = &metav1.Time{Time: time.Now().UTC()}
			if err := s.client.Status().Patch(ctx, task, client.MergeFrom(base)); err != nil {
				return false, err
			}
		}
		if controllerutil.ContainsFinalizer(task, validationFinalizer) {
			base := task.DeepCopy()
			controllerutil.RemoveFinalizer(task, validationFinalizer)
			if err := s.client.Patch(ctx, task, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
				return false, err
			}
		}
	}
	return allGone, nil
}

func validationCleanupPhase(submission *pv.KubernetesSubmission, record *pv.Record) corev1alpha1.TaskPhase {
	if submission.State == pv.SubmissionCancelling || (record != nil && record.State == pv.RunCancelled) {
		return corev1alpha1.TaskPhaseCancelled
	}
	if record != nil && record.State == pv.RunFinalized {
		return corev1alpha1.TaskPhaseSucceeded
	}
	return corev1alpha1.TaskPhaseFailed
}

func (s *Service) cleanupTaskObjects(ctx context.Context, submission *pv.KubernetesSubmission, task *corev1alpha1.Task) (bool, error) {
	if submission.RunID == "" {
		return true, nil
	}
	side := pv.Original
	if task.Name == submission.PatchedTaskName {
		side = pv.Patched
	}
	gone := true
	jobUIDs := make(map[string]bool)
	jobNames := make([]string, 0, len(submission.Manifest.Checks))
	for _, check := range submission.Manifest.Checks {
		dispatch, err := s.store.GetKubernetesValidationDispatch(ctx, submission.RunID, side, check.ID)
		if errors.Is(err, pv.ErrRunNotFound) {
			continue
		}
		if err != nil {
			return false, err
		}
		if dispatch.JobUID != "" {
			jobUIDs[dispatch.JobUID] = true
		}
		jobNames = append(jobNames, dispatch.JobName)
		job := &batchv1.Job{}
		if err := s.reader.Get(ctx, client.ObjectKey{Namespace: task.Namespace, Name: dispatch.JobName}, job); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return false, err
		}
		if !metav1.IsControlledBy(job, task) {
			return false, fmt.Errorf("validation cleanup refused an unowned Job")
		}
		if dispatch.JobUID != "" && string(job.UID) != dispatch.JobUID {
			return false, fmt.Errorf("validation cleanup refused a replaced Job")
		}
		jobUIDs[string(job.UID)] = true
		gone = false
		if job.DeletionTimestamp.IsZero() {
			if err := s.client.Delete(ctx, job, client.Preconditions{UID: new(job.UID)}, client.PropagationPolicy(metav1.DeletePropagationForeground)); err != nil && !apierrors.IsNotFound(err) {
				return false, err
			}
		}
	}
	if !gone {
		return false, nil
	}
	hasPods, err := HasJobPods(ctx, s.reader, task.Namespace, jobUIDs, jobNames...)
	if err != nil {
		return false, err
	}
	if hasPods {
		return false, nil
	}
	for _, check := range submission.Manifest.Checks {
		jobName := validationJobName(submission.RunID, side, check.ID)
		for _, object := range []client.Object{
			&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: jobName + "-input", Namespace: task.Namespace}},
			validationNetworkPolicy(task, jobName),
		} {
			if err := s.reader.Get(ctx, client.ObjectKeyFromObject(object), object); err != nil {
				if apierrors.IsNotFound(err) {
					continue
				}
				return false, err
			}
			if !metav1.IsControlledBy(object, task) {
				return false, fmt.Errorf("validation cleanup refused an unowned resource")
			}
			if err := s.client.Delete(ctx, object, client.Preconditions{UID: new(object.GetUID())}); err != nil && !apierrors.IsNotFound(err) {
				return false, err
			}
		}
	}
	return true, nil
}
func validationContainerStatus(pod *corev1.Pod) (corev1.ContainerStatus, bool) {
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == validationContainerName && status.State.Terminated != nil {
			return status, true
		}
	}
	return corev1.ContainerStatus{}, false
}

func (s *Service) readPodReport(ctx context.Context, namespace, pod string) (pv.PodReport, error) {
	stream, err := s.kubeClient.CoreV1().Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{Container: validationContainerName}).Stream(ctx)
	if err != nil {
		return pv.PodReport{}, err
	}
	content, err := io.ReadAll(io.LimitReader(stream, pv.MaxPodReportBytes+1))
	closeErr := stream.Close()
	if err != nil || closeErr != nil {
		return pv.PodReport{}, errors.Join(err, closeErr)
	}
	if len(content) > pv.MaxPodReportBytes {
		return pv.PodReport{}, pv.ErrLimit
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	var report pv.PodReport
	if err := decoder.Decode(&report); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return report, io.ErrUnexpectedEOF
		}
		return report, err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) || strings.TrimSpace(string(content)) == "" {
		return report, pv.ErrEvidence
	}
	return report, nil
}
