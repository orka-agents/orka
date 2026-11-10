package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

const (
	deferredFinalizationLabel = "orka.ai/deferred-task-finalization"
	deferredResultChunkBytes  = 512 << 10 // Each Secret remains below Kubernetes' 1 MiB limit.
)

// A storage outage must not keep failed/cancelled work running, erase available
// output, or turn the missing Job into an execution retry. Publish this immutable
// intent only after all output chunks exist, then allow cascading Job deletion.
// Output lives only in Task-owned Secrets until the result provider accepts it.
// Content-addressed chunks make interrupted staging safe to retry as logs grow.
// Neither the output nor Secret data is included in errors or logs.
type deferredTaskFinalization struct {
	Phase         corev1alpha1.TaskPhase `json:"phase"`
	OutcomePhase  corev1alpha1.TaskPhase `json:"outcomePhase"`
	Message       string                 `json:"message"`
	RecordOutcome bool                   `json:"recordOutcome"`
	ResultChunks  int                    `json:"resultChunks"`
	ResultBytes   int                    `json:"resultBytes"`
	ResultDigest  string                 `json:"resultDigest,omitempty"`
}

// stopTaskAfterUsageRetentionFailure preserves stop decisions while durable
// usage bookkeeping is unavailable. The Task remains nonterminal for recovery.
func (r *TaskReconciler) stopTaskAfterUsageRetentionFailure(ctx context.Context, task *corev1alpha1.Task, err error) error {
	// Stop requested work even when its bookkeeping cannot be written.
	// Durable finalization remains pending until storage recovers.
	intent, lookupErr := r.loadDeferredTaskFinalization(ctx, task)
	err = errors.Join(err, lookupErr)
	if intent != nil && !task.Status.Phase.IsTerminal() {
		stopping := task.DeepCopy()
		stopping.Status.Phase = intent.Phase
		err = errors.Join(err, r.stopJobForDeferredFinalization(ctx, stopping, *intent))
	} else if message, requested := requestedTaskCancellation(task); requested && lookupErr == nil {
		cancelled := task.DeepCopy()
		cancelled.Status.Phase = corev1alpha1.TaskPhaseCancelled
		cleanupErr := r.stopJobForDeferredFinalization(ctx, cancelled, deferredTaskFinalization{
			Phase: corev1alpha1.TaskPhaseCancelled, OutcomePhase: corev1alpha1.TaskPhaseCancelled, Message: message,
		})
		err = errors.Join(err, cleanupErr)
	}
	return err
}

// Persist output and the original decision before removing a Job whose
// finalization is blocked on storage. Inactive failed Jobs retain their Pods.
func (r *TaskReconciler) stopJobForDeferredFinalization(ctx context.Context, task *corev1alpha1.Task, intent deferredTaskFinalization) error {
	if task.Status.JobName == "" {
		return nil
	}
	job := &batchv1.Job{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: task.Namespace, Name: task.Status.JobName}, job); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if taskJobIdentityRejected(task) {
		_, err := r.cleanupTerminalTaskJob(ctx, task)
		return err
	}
	if task.Status.JobUID != "" && task.Status.JobUID != string(job.UID) {
		return errors.New("cannot stop a replacement Task Job")
	}
	if intent.Phase != corev1alpha1.TaskPhaseCancelled && (intent.Phase != corev1alpha1.TaskPhaseFailed || job.Status.Active == 0) {
		return nil
	}
	if err := r.stageDeferredTaskFinalization(ctx, task, intent); err != nil {
		return err
	}
	// Use the observed UID and resource version even if a same-name Job is
	// recreated between staging and deletion.
	propagation := metav1.DeletePropagationBackground
	if r.repositoryMonitorValidationSafetyTask(ctx, task) {
		propagation = metav1.DeletePropagationForeground
	}
	err := r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &propagation,
		Preconditions: &metav1.Preconditions{UID: &job.UID, ResourceVersion: &job.ResourceVersion}})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func deferredFinalizationName(task *corev1alpha1.Task) string {
	identity, _ := json.Marshal([]any{task.UID, task.Status.JobName, task.Status.JobUID, task.Status.Attempts, task.Status.Iteration})
	digest := sha256.Sum256(identity)
	return "task-finalization-" + hex.EncodeToString(digest[:16])
}

func deferredResultChunkName(task *corev1alpha1.Task, intent *deferredTaskFinalization, index int) string {
	return fmt.Sprintf("%s-%s-%d", deferredFinalizationName(task), intent.ResultDigest[:16], index)
}

func (r *TaskReconciler) readDeferredFinalizationSecret(ctx context.Context, task *corev1alpha1.Task, name string) (*corev1.Secret, error) {
	secret := &corev1.Secret{}
	if err := uncachedReader(r.APIReader, r.Client).Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: name}, secret); err != nil {
		return nil, err
	}
	owner := metav1.GetControllerOf(secret)
	if task.UID == "" || owner == nil || owner.UID != task.UID || owner.Name != task.Name ||
		owner.Kind != taskResourceKind || owner.APIVersion != corev1alpha1.GroupVersion.String() || secret.Immutable == nil || !*secret.Immutable ||
		secret.Type != corev1.SecretTypeOpaque || secret.Labels[deferredFinalizationLabel] != deferredFinalizationName(task) ||
		!secret.DeletionTimestamp.IsZero() {
		return nil, errors.New("deferred Task finalization Secret identity is invalid")
	}
	return secret, nil
}

func (r *TaskReconciler) loadDeferredTaskFinalization(ctx context.Context, task *corev1alpha1.Task) (*deferredTaskFinalization, error) {
	if task.Status.JobName == "" || task.UID == "" {
		return nil, nil
	}
	secret, err := r.readDeferredFinalizationSecret(ctx, task, deferredFinalizationName(task))
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	intent := &deferredTaskFinalization{}
	if json.Unmarshal(secret.Data["intent.json"], intent) != nil ||
		(intent.Phase != corev1alpha1.TaskPhaseCancelled && intent.Phase != corev1alpha1.TaskPhaseFailed) ||
		intent.ResultChunks < 0 || intent.ResultChunks > int(stdoutResultLogLimitBytes)/deferredResultChunkBytes+1 ||
		intent.ResultBytes < 0 || intent.ResultBytes > int(stdoutResultLogLimitBytes) ||
		(intent.ResultChunks == 0 && (intent.ResultBytes != 0 || intent.ResultDigest != "")) ||
		(intent.ResultChunks > 0 && (len(intent.ResultDigest) != sha256.Size*2 || intent.ResultChunks != max(1, (intent.ResultBytes+deferredResultChunkBytes-1)/deferredResultChunkBytes))) {
		return nil, errors.New("deferred Task finalization intent is invalid")
	}
	return intent, nil
}

func (r *TaskReconciler) createDeferredFinalizationSecret(ctx context.Context, task *corev1alpha1.Task, name, key string, data []byte) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: task.Namespace, Name: name,
			Labels:          map[string]string{deferredFinalizationLabel: deferredFinalizationName(task)},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(task, corev1alpha1.GroupVersion.WithKind(taskResourceKind))},
		},
		Type: corev1.SecretTypeOpaque, Immutable: new(true), Data: map[string][]byte{key: data},
	}
	if err := r.Create(ctx, secret); !apierrors.IsAlreadyExists(err) {
		return err
	}
	existing, err := r.readDeferredFinalizationSecret(ctx, task, name)
	if err != nil {
		return err
	}
	if !bytes.Equal(existing.Data[key], data) {
		return errors.New("deferred Task finalization Secret content changed")
	}
	return nil
}

// stageDeferredTaskFinalization is used only before deleting an unsettled Job.
// A pending intent wins over subsequent observations of the missing Job.
func (r *TaskReconciler) stageDeferredTaskFinalization(ctx context.Context, task *corev1alpha1.Task, intent deferredTaskFinalization) error {
	if existing, err := r.loadDeferredTaskFinalization(ctx, task); err != nil || existing != nil {
		return err
	}
	if task.UID == "" {
		return errors.New("cannot defer Task finalization without a Task UID")
	}
	// An unavailable binding lookup must suppress possible validation output
	// without keeping cancelled or failed work running. Normal finalization
	// still retries the authoritative classification before recording status.
	validationTask := r.repositoryMonitorValidationSafetyTask(ctx, task)
	if !validationTask && !taskJobIdentityRejected(task) && r.ResultStore != nil &&
		(task.Status.ResultRef == nil || !task.Status.ResultRef.Available) {
		result, available, err := r.taskPodResult(ctx, task, true)
		if err != nil {
			return err
		}
		if available {
			digest := sha256.Sum256(result)
			intent.ResultDigest = hex.EncodeToString(digest[:])
			intent.ResultBytes = len(result)
			intent.ResultChunks = max(1, (len(result)+deferredResultChunkBytes-1)/deferredResultChunkBytes)
			for i := 0; i < intent.ResultChunks; i++ {
				chunk := result[i*deferredResultChunkBytes : min((i+1)*deferredResultChunkBytes, len(result))]
				if err := r.createDeferredFinalizationSecret(ctx, task, deferredResultChunkName(task, &intent, i), "result", chunk); err != nil {
					return err
				}
			}
		}
	}
	data, err := json.Marshal(intent)
	if err != nil {
		return err
	}
	return r.createDeferredFinalizationSecret(ctx, task, deferredFinalizationName(task), "intent.json", data)
}

func (r *TaskReconciler) deferredTaskResult(ctx context.Context, task *corev1alpha1.Task, intent *deferredTaskFinalization) ([]byte, bool, error) {
	if intent.ResultChunks == 0 {
		return nil, false, nil
	}
	result := make([]byte, 0, intent.ResultBytes)
	for i := 0; i < intent.ResultChunks; i++ {
		secret, err := r.readDeferredFinalizationSecret(ctx, task, deferredResultChunkName(task, intent, i))
		if err != nil {
			return nil, false, err
		}
		chunk := secret.Data["result"]
		if len(chunk) > deferredResultChunkBytes || len(result)+len(chunk) > intent.ResultBytes {
			return nil, false, errors.New("deferred Task result size is invalid")
		}
		result = append(result, chunk...)
	}
	digest := sha256.Sum256(result)
	if len(result) != intent.ResultBytes || hex.EncodeToString(digest[:]) != intent.ResultDigest {
		return nil, false, errors.New("deferred Task result digest is invalid")
	}
	return result, true, nil
}

// Keep the intent until Kubernetes records the terminal phase. Result acceptance
// alone is insufficient: session finalization or the status write can still fail.
func (r *TaskReconciler) cleanupDeferredTaskFinalization(ctx context.Context, task *corev1alpha1.Task) error {
	if task.Status.JobName == "" || task.UID == "" {
		return nil
	}
	secrets := &corev1.SecretList{}
	if err := uncachedReader(r.APIReader, r.Client).List(ctx, secrets, client.InNamespace(task.Namespace),
		client.MatchingLabels{deferredFinalizationLabel: deferredFinalizationName(task)}); err != nil {
		return err
	}
	for i := range secrets.Items {
		secret := &secrets.Items[i]
		if !metav1.IsControlledBy(secret, task) {
			continue
		}
		if err := r.Delete(ctx, secret, client.Preconditions{UID: &secret.UID}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// taskPodResult shares result decoding between normal collection and outage
// staging. A Pod that never started has no output to preserve. Read failures for
// a started Pod block destructive cleanup, so recovery cannot silently lose it.
func (r *TaskReconciler) taskPodResult(ctx context.Context, task *corev1alpha1.Task, allowUnstarted bool) ([]byte, bool, error) {
	stdoutResult := taskUsesStdoutResult(task)
	if (task.Spec.Type != corev1alpha1.TaskTypeContainer && !stdoutResult) || r.KubeClient == nil || task.Status.JobName == "" {
		return nil, false, nil
	}
	if allowUnstarted {
		pods := &corev1.PodList{}
		if err := r.List(ctx, pods, client.InNamespace(task.Namespace), client.MatchingLabels{"job-name": task.Status.JobName}); err != nil {
			return nil, false, err
		}
		if len(pods.Items) == 0 {
			return nil, false, nil
		}
		pod := &pods.Items[len(pods.Items)-1]
		// GetLogs leaves Container empty. The API defaults it only for a
		// single regular container; init containers never supply this output.
		// Preserve log errors for ambiguous multi-container Pods.
		if pod.Status.Phase == corev1.PodPending && len(pod.Spec.Containers) <= 1 {
			container := ""
			if len(pod.Spec.Containers) == 1 {
				container = pod.Spec.Containers[0].Name
			}
			started := false
			for _, status := range pod.Status.ContainerStatuses {
				if container == "" || status.Name == container {
					started = started || status.State.Running != nil || status.State.Terminated != nil || status.LastTerminationState.Terminated != nil || status.RestartCount > 0
				}
			}
			if !started {
				return nil, false, nil
			}
		}
	}
	if stdoutResult {
		logs, err := r.readStdoutResultPodLogs(ctx, task)
		if err != nil {
			return nil, false, fmt.Errorf("reading stdout result pod logs: %w", err)
		}
		return extractStdoutTaskResult(logs)
	}
	logs, err := r.readPodLogs(ctx, task)
	if err != nil {
		return nil, false, fmt.Errorf("reading pod logs: %w", err)
	}
	return []byte(logs), true, nil
}
