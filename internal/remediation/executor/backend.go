package executor

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net"
	"reflect"
	"slices"
	"strconv"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	pv "github.com/orka-agents/orka/internal/patchverification"
	pvkube "github.com/orka-agents/orka/internal/patchverification/kube"
	"github.com/orka-agents/orka/internal/remediation/contract"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func (b *Backend) requestInput(request contract.ExecutionRequest) (pv.PreparationInput, string, error) {
	if b == nil || !b.config.Enabled {
		return pv.PreparationInput{}, "", contract.ErrDisabled
	}
	if err := contract.RequireEvidenceScope(request.EvidenceScope, b.EvidenceScope()); err != nil {
		return pv.PreparationInput{}, "", err
	}
	if request.Namespace != b.config.Namespace || len(validation.IsDNS1123Subdomain(request.ParentTaskName)) != 0 {
		return pv.PreparationInput{}, "", contract.ErrInvalid
	}
	reportDigest, err := pv.StableReportDigest(request.Problem, request.Checks.Scope)
	if err != nil {
		return pv.PreparationInput{}, "", contract.ErrInvalid
	}
	input := pv.PreparationInput{Version: pv.PreparationProtocolVersion, ID: request.ID, Namespace: request.Namespace,
		ParentTaskName: request.ParentTaskName, ParentTaskUID: request.ParentTaskUID, Patch: []byte(request.Patch),
		Manifest: pv.Manifest{Version: pv.SchemaVersion, Action: request.Action, Problem: request.Problem, ReportDigest: reportDigest,
			Scope: request.Checks.Scope, Gaps: request.Checks.Gaps, Files: request.Checks.Files, Checks: request.Checks.Checks,
			DeclaredChanges: request.DeclaredChanges,
			Sources: pv.Sources{Repository: request.Source.Repository,
				Original: pv.SourceIdentity{Commit: request.Source.Commit, Tree: request.Source.Tree},
				Paths:    slices.Clone(request.Source.Paths)},
			Environment: pv.Environment{Image: b.config.ToolImage, ImageID: b.config.ToolImageID,
				Platform: b.config.Platform, Profile: b.config.Profile, Services: request.Checks.Services,
				Requirements: request.Checks.Requirements, Dependencies: map[string]string{
					"orka.kubernetes.policy": pv.KubernetesPolicyVersion, "orka.kubernetes.helper-image": b.config.HelperImage,
					"orka.kubernetes.helper-image-id": b.config.HelperImageID,
					"orka.remediation.general-image":  b.config.GeneralWorkerImage, pv.PreparationPolicyKey: pv.PreparationPolicy,
					"orka.remediation.general-image-id": b.config.GeneralWorkerImageID,
					pv.PreparationConfigurationKey:      b.ConfigurationDigest(),
					"orka.kubernetes.node-selector":     b.config.NodeSelectorKey + "=" + b.config.NodeSelectorValue,
					"orka.kubernetes.network-canary":    net.JoinHostPort(b.config.CanaryHost, strconv.Itoa(b.config.CanaryPort)),
					"orka.remediation.run-timeout":      b.config.RunTimeout.String(),
				}},
		}}
	// Omitted scope is reserved for retained pre-contract executions. Do not
	// rewrite their input digest or promote their unrecorded scope on resume.
	if request.EvidenceScope != "" {
		input.Manifest.Environment.Dependencies[contract.EvidenceScopeKey] = string(b.EvidenceScope())
	}
	if len(pv.MissingRequirements(input.Manifest.Environment)) != 0 {
		return pv.PreparationInput{}, "", contract.ErrUnavailable
	}
	if err := pv.ValidatePreparationInput(input); err != nil {
		return pv.PreparationInput{}, "", safeError(contract.ErrInvalid, err)
	}
	digest, err := pv.PreparationInputDigest(input)
	if err != nil {
		return pv.PreparationInput{}, "", safeError(contract.ErrInvalid, err)
	}
	// Request slices and maps belong to the caller; freeze a private copy before
	// the first callback or Kubernetes effect can yield control.
	content, err := json.Marshal(input)
	var frozen pv.PreparationInput
	if err != nil || json.Unmarshal(content, &frozen) != nil {
		return pv.PreparationInput{}, "", contract.ErrInvalid
	}
	return frozen, digest, nil
}

// RequestDigest performs the same pure validation as Start, without reading or
// creating any workload. Coordinators can reject input before marking admission.
func (b *Backend) RequestDigest(request contract.ExecutionRequest) (string, error) {
	_, digest, err := b.requestInput(request)
	return digest, err
}

func (b *Backend) Start(ctx context.Context, request contract.ExecutionRequest, accepted func(contract.ExecutionReceipt) error) (*pv.Record, error) {
	input, digest, err := b.requestInput(request)
	if err != nil {
		return nil, noEffectsError{cause: err}
	}
	if accepted == nil {
		return nil, noEffectsError{cause: contract.ErrInvalid}
	}
	ctx, cancel := context.WithTimeout(ctx, b.config.RunTimeout)
	defer cancel()
	if err := b.validateParent(ctx, input); err != nil {
		return nil, noEffectsError{cause: err}
	}
	expected := b.preparationTask(input, digest)
	if err := b.rejectCancellationFence(ctx, expected.Namespace, expected.Name); err != nil {
		if errors.Is(err, contract.ErrUnavailable) {
			return nil, noEffectsError{cause: err}
		}
		return nil, err
	}
	task := &corev1alpha1.Task{}
	err = b.reader.Get(ctx, client.ObjectKeyFromObject(expected), task)
	created := false
	if apierrors.IsNotFound(err) {
		marker := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: expected.Name + "-intent", Namespace: expected.Namespace,
			OwnerReferences: expected.OwnerReferences}, Data: map[string]string{"inputDigest": digest}}
		if err := b.client.Create(ctx, marker); err != nil {
			return nil, safeError(contract.ErrConflict, err)
		}
		if err := b.client.Create(ctx, expected); err != nil {
			return nil, safeError(contract.ErrUnavailable, err)
		}
		if expected.UID == "" || marker.UID == "" {
			return nil, contract.ErrConflict
		}
		marker.Data["taskUID"] = string(expected.UID)
		marker.Immutable = new(true)
		if err := b.client.Update(ctx, marker); err != nil {
			return nil, safeError(contract.ErrUnavailable, err)
		}
		if err := b.reader.Get(ctx, client.ObjectKeyFromObject(expected), task); err != nil {
			return nil, safeError(contract.ErrUnavailable, err)
		}
		created = true
	} else if err != nil {
		return nil, noEffectsError{cause: safeError(contract.ErrUnavailable, err)}
	}
	receipt := contract.ExecutionReceipt{Namespace: task.Namespace, TaskName: task.Name, TaskUID: string(task.UID), InputDigest: digest}
	if _, err := b.boundTask(ctx, input, receipt); err != nil {
		return nil, err
	}
	if err := accepted(receipt); err != nil {
		return nil, safeError(contract.ErrUnavailable, err)
	}
	input.TaskUID = receipt.TaskUID
	if created {
		if err := b.createPreparation(ctx, input, receipt); err != nil {
			return nil, err
		}
	}
	return b.await(ctx, input, receipt)
}

func (b *Backend) Recover(ctx context.Context, request contract.ExecutionRequest) (contract.ExecutionRecovery, error) {
	input, digest, err := b.requestInput(request)
	if err != nil {
		return contract.ExecutionRecovery{}, err
	}
	expected := b.preparationTask(input, digest)
	marker := &corev1.ConfigMap{}
	if err := b.reader.Get(ctx, client.ObjectKey{Namespace: expected.Namespace, Name: expected.Name + "-intent"}, marker); err != nil {
		if apierrors.IsNotFound(err) {
			return contract.ExecutionRecovery{}, contract.ErrNotFound
		}
		return contract.ExecutionRecovery{}, safeError(contract.ErrUnavailable, err)
	}
	// Only the Create response's durably recorded UID is authority; observing
	// a matching Task name or spec cannot recover an unacknowledged create.
	uid := marker.Data["taskUID"]
	if marker.UID == "" || !marker.DeletionTimestamp.IsZero() || marker.Immutable == nil || !*marker.Immutable ||
		uid == "" || len(marker.Data) != 2 || len(marker.BinaryData) != 0 ||
		marker.Data["inputDigest"] != digest || !reflect.DeepEqual(marker.OwnerReferences, expected.OwnerReferences) {
		return contract.ExecutionRecovery{}, contract.ErrConflict
	}
	return contract.ExecutionRecovery{Receipt: &contract.ExecutionReceipt{
		Namespace: expected.Namespace, TaskName: expected.Name, TaskUID: uid, InputDigest: digest,
	}}, nil
}

func (b *Backend) Resume(ctx context.Context, request contract.ExecutionRequest, receipt contract.ExecutionReceipt) (*pv.Record, error) {
	input, digest, err := b.requestInput(request)
	if err != nil {
		return nil, err
	}
	if digest != receipt.InputDigest {
		return nil, contract.ErrConflict
	}
	ctx, cancel := context.WithTimeout(ctx, b.config.RunTimeout)
	defer cancel()
	if _, err := b.boundTask(ctx, input, receipt); err != nil {
		return nil, err
	}
	input.TaskUID = receipt.TaskUID
	return b.await(ctx, input, receipt)
}

func (b *Backend) validateParent(ctx context.Context, input pv.PreparationInput) error {
	parent := &corev1alpha1.Task{}
	if err := b.reader.Get(ctx, client.ObjectKey{Namespace: input.Namespace, Name: input.ParentTaskName}, parent); err != nil {
		return safeError(contract.ErrUnavailable, err)
	}
	if input.Namespace != b.config.Namespace || string(parent.UID) != input.ParentTaskUID || !parent.DeletionTimestamp.IsZero() {
		return contract.ErrConflict
	}
	return nil
}

func (b *Backend) boundTask(ctx context.Context, input pv.PreparationInput, receipt contract.ExecutionReceipt) (*corev1alpha1.Task, error) {
	digest, err := pv.PreparationInputDigest(input)
	expected := b.preparationTask(input, digest)
	if err != nil || receipt.Namespace != b.config.Namespace || receipt.Namespace != input.Namespace ||
		receipt.TaskName != expected.Name || receipt.TaskUID == "" || receipt.InputDigest != digest {
		return nil, contract.ErrConflict
	}
	if err := b.validateParent(ctx, input); err != nil {
		return nil, err
	}
	if err := b.rejectCancellationFence(ctx, receipt.Namespace, receipt.TaskName); err != nil {
		return nil, err
	}
	task := &corev1alpha1.Task{}
	if err := b.reader.Get(ctx, client.ObjectKeyFromObject(expected), task); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, contract.ErrNotFound
		}
		return nil, safeError(contract.ErrUnavailable, err)
	}
	if string(task.UID) != receipt.TaskUID || !reflect.DeepEqual(task.Spec, expected.Spec) ||
		!reflect.DeepEqual(task.OwnerReferences, expected.OwnerReferences) ||
		task.Annotations[pv.ValidationBackendKey] != preparationBackend ||
		task.Annotations[pv.PreparationInputAnnotation] != digest ||
		task.Annotations[pv.PreparationCancelAnnotation] != "" || !task.DeletionTimestamp.IsZero() ||
		task.Status.Phase == corev1alpha1.TaskPhaseCancelled {
		return nil, contract.ErrConflict
	}
	return task, nil
}

func (b *Backend) createPreparation(ctx context.Context, input pv.PreparationInput, receipt contract.ExecutionReceipt) error {
	if err := b.lockEffects(ctx); err != nil {
		return err
	}
	defer func() { <-b.effectGate }()
	task, err := b.boundTask(ctx, input, receipt)
	if err != nil {
		return err
	}
	content, err := json.Marshal(input)
	if err != nil || len(content) > pv.MaxPreparationInputBytes {
		return contract.ErrInvalid
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: task.Name + "-input", Namespace: task.Namespace},
		Immutable: new(true), BinaryData: map[string][]byte{"request.json": content}}
	if err := controllerutil.SetControllerReference(task, cm, b.scheme); err != nil {
		return safeError(contract.ErrInvalid, err)
	}
	if err := b.client.Create(ctx, cm); err != nil {
		return safeError(contract.ErrConflict, err)
	}
	job, err := b.buildPreparationJob(task)
	if err != nil {
		return err
	}
	if err := b.client.Create(ctx, job); err != nil {
		return safeError(contract.ErrUnavailable, err)
	}
	observed := &batchv1.Job{}
	if err := b.reader.Get(ctx, client.ObjectKeyFromObject(job), observed); err != nil {
		return safeError(contract.ErrUnavailable, err)
	}
	if observed.UID == "" || (job.UID != "" && observed.UID != job.UID) ||
		!metav1.IsControlledBy(observed, task) || !pvkube.JobExecutionMatches(job, observed) {
		return contract.ErrConflict
	}
	base := task.DeepCopy()
	task.Annotations[preparationJobUID] = string(observed.UID)
	if err := b.client.Patch(ctx, task, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return safeError(contract.ErrUnavailable, err)
	}
	return b.setTaskPhase(ctx, task, corev1alpha1.TaskPhaseRunning)
}

func (b *Backend) await(ctx context.Context, input pv.PreparationInput, receipt contract.ExecutionReceipt) (*pv.Record, error) {
	task, err := b.boundTask(ctx, input, receipt)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(b.config.RunTimeout)
	if !task.CreationTimestamp.IsZero() {
		deadline = task.CreationTimestamp.Add(b.config.RunTimeout)
	}
	operation, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	ticker := time.NewTicker(b.config.ReconcileInterval)
	defer ticker.Stop()
	requestID := pvkube.PreparedSubmissionID(receipt.TaskUID, receipt.InputDigest)
	for {
		if err := operation.Err(); err != nil {
			return nil, safeError(contract.ErrUnavailable, err)
		}
		task, err := b.boundTask(operation, input, receipt)
		if err != nil {
			return nil, err
		}
		submission, err := b.validation.GetSubmission(operation, receipt.Namespace, requestID)
		if errors.Is(err, pv.ErrRunNotFound) {
			report, done, err := b.observePreparation(operation, input, task)
			if err != nil {
				return nil, err
			}
			if done {
				submission, err = b.validation.SubmitPrepared(operation, task, input, *report)
				if err != nil {
					return nil, safeError(contract.ErrUnavailable, err)
				}
			}
		} else if err != nil {
			return nil, safeError(contract.ErrUnavailable, err)
		}
		if submission != nil {
			if err := b.validateSubmission(input, receipt, submission); err != nil {
				return nil, err
			}
			if submission.State == pv.SubmissionTerminal {
				return b.finish(operation, task, submission)
			}
			if err := b.validation.Advance(operation, receipt.Namespace, requestID); err != nil {
				return nil, safeError(contract.ErrUnavailable, err)
			}
		}
		select {
		case <-operation.Done():
			return nil, safeError(contract.ErrUnavailable, operation.Err())
		case <-ticker.C:
		}
	}
}

func (b *Backend) validateSubmission(input pv.PreparationInput, receipt contract.ExecutionReceipt, submission *pv.KubernetesSubmission) error {
	deps := submission.Manifest.Environment.Dependencies
	if submission.Namespace != receipt.Namespace || submission.SubmittedBy != receipt.TaskUID ||
		submission.RequestID != pvkube.PreparedSubmissionID(receipt.TaskUID, receipt.InputDigest) ||
		deps[pv.PreparationTaskKey] != receipt.TaskName || deps[pv.PreparationUIDKey] != receipt.TaskUID ||
		deps[pv.PreparationDigestKey] != receipt.InputDigest || deps[pv.PreparationParentTaskKey] != input.ParentTaskName ||
		deps[pv.PreparationParentUIDKey] != input.ParentTaskUID {
		return contract.ErrConflict
	}
	report := pv.PreparationReport{Version: pv.PreparationProtocolVersion, TaskUID: receipt.TaskUID,
		InputDigest: receipt.InputDigest, Manifest: submission.Manifest, Provenance: submission.Provenance}
	report.Manifest.Environment.Dependencies = maps.Clone(deps)
	for _, key := range []string{pv.PreparationTaskKey, pv.PreparationUIDKey, pv.PreparationDigestKey, pv.PreparationParentTaskKey, pv.PreparationParentUIDKey} {
		delete(report.Manifest.Environment.Dependencies, key)
	}
	if pv.ValidatePreparationReport(input, report) != nil {
		return contract.ErrConflict
	}
	if submission.Binding != nil && (pv.ValidateRunBinding(submission.Manifest, *submission.Binding) != nil ||
		submission.Binding.RunID != submission.RunID || submission.Binding.OriginalTaskID != submission.OriginalTaskUID ||
		submission.Binding.PatchedTaskID != submission.PatchedTaskUID) {
		return contract.ErrConflict
	}
	return nil
}

func (b *Backend) finish(ctx context.Context, task *corev1alpha1.Task, submission *pv.KubernetesSubmission) (*pv.Record, error) {
	if err := b.lockEffects(ctx); err != nil {
		return nil, err
	}
	defer func() { <-b.effectGate }()
	current := &corev1alpha1.Task{}
	if err := b.reader.Get(ctx, client.ObjectKeyFromObject(task), current); err != nil {
		return nil, safeError(contract.ErrUnavailable, err)
	}
	if current.UID != task.UID || current.Annotations[pv.PreparationCancelAnnotation] != "" || !current.DeletionTimestamp.IsZero() ||
		current.Status.Phase == corev1alpha1.TaskPhaseCancelled {
		return nil, contract.ErrConflict
	}
	if submission.Binding == nil || submission.RunID == "" {
		return nil, contract.ErrUnavailable
	}
	record, err := b.validation.Evidence(ctx, submission.Namespace, submission.RequestID)
	if err != nil {
		return nil, safeError(contract.ErrUnavailable, err)
	}
	if record == nil || record.Binding != *submission.Binding || !reflect.DeepEqual(record.Manifest, submission.Manifest) ||
		record.State != pv.RunFinalized || record.Seal == nil || record.Seal.Digest != pv.Digest(record.Seal.Content) ||
		!reflect.DeepEqual(record.Seal.Assessment, record.Assessment) || len(record.Incidents) != 0 {
		return nil, contract.ErrUnavailable
	}
	observations := make([]pv.Observation, 0, len(record.Evidence))
	for _, evidence := range record.Evidence {
		content, err := json.Marshal(evidence.Observation)
		if err != nil || evidence.Rejection != "" || evidence.Digest != pv.Digest(content) ||
			pv.ValidateObservation(record.Manifest, record.Binding, evidence.Observation) != nil {
			return nil, contract.ErrUnavailable
		}
		observations = append(observations, evidence.Observation)
	}
	if !reflect.DeepEqual(pv.Evaluate(record.Manifest, record.Binding, observations), record.Assessment) {
		return nil, contract.ErrUnavailable
	}
	if err := b.cleanupPreparation(ctx, task); err != nil {
		return nil, err
	}
	if err := b.setTaskPhase(ctx, task, corev1alpha1.TaskPhaseSucceeded); err != nil {
		return nil, err
	}
	return record, nil
}

func (b *Backend) setTaskPhase(ctx context.Context, task *corev1alpha1.Task, phase corev1alpha1.TaskPhase) error {
	current := &corev1alpha1.Task{}
	if err := b.reader.Get(ctx, client.ObjectKeyFromObject(task), current); err != nil {
		return safeError(contract.ErrUnavailable, err)
	}
	if current.UID != task.UID {
		return contract.ErrConflict
	}
	base := current.DeepCopy()
	current.Status.Phase = phase
	current.Status.JobName = task.Name
	current.Status.Message = "trusted preparation and validation"
	if phase != corev1alpha1.TaskPhaseRunning && current.Status.CompletionTime == nil {
		current.Status.CompletionTime = new(metav1.Now())
	}
	if !reflect.DeepEqual(base.Status, current.Status) {
		if err := b.client.Status().Patch(ctx, current, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return safeError(contract.ErrUnavailable, err)
		}
	}
	if phase != corev1alpha1.TaskPhaseRunning &&
		(controllerutil.ContainsFinalizer(current, preparationFinalizer) || current.Annotations[preparationCleaned] != preparationMarkerSet) {
		base := current.DeepCopy()
		if current.Annotations == nil {
			current.Annotations = make(map[string]string)
		}
		current.Annotations[preparationCleaned] = preparationMarkerSet
		controllerutil.RemoveFinalizer(current, preparationFinalizer)
		if err := b.client.Patch(ctx, current, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return safeError(contract.ErrUnavailable, err)
		}
	}
	return nil
}
