package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	types "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/taskterminal"
)

func standaloneTaskTerminalProjectionID(task *corev1alpha1.Task, attempt int32) string {
	if task == nil {
		return ""
	}
	return standaloneTaskTerminalProjectionIDForUID(task.Namespace, task.UID, attempt)
}

func standaloneTaskTerminalProjectionIDForUID(namespace string, taskUID types.UID, attempt int32) string {
	if strings.TrimSpace(namespace) == "" || taskUID == "" {
		return ""
	}
	return store.CanonicalControlID("task-terminal-projection", namespace, string(taskUID), fmt.Sprint(attempt))
}

func (d *ACPDispatcher) enqueueStandaloneTaskProjection(ctx context.Context, task *corev1alpha1.Task, payload taskTerminalProjection) error {
	return d.enqueueTaskTerminalProjection(ctx, task, payload, false)
}

func (d *ACPDispatcher) enqueueUnboundSessionTaskProjection(ctx context.Context, task *corev1alpha1.Task, payload taskTerminalProjection) error {
	return d.enqueueTaskTerminalProjection(ctx, task, payload, true)
}

func (d *ACPDispatcher) enqueueTaskTerminalProjection(
	ctx context.Context,
	task *corev1alpha1.Task,
	payload taskTerminalProjection,
	allowSessionRef bool,
) error {
	if task == nil || (task.Spec.SessionRef != nil && !allowSessionRef) {
		return nil
	}
	fence, err := d.Epochs.CurrentFence(ctx)
	if err != nil {
		return err
	}
	return enqueueDurableTaskTerminalProjection(ctx, d.Store, fence, task, payload)
}

func enqueueDurableTaskTerminalProjection(
	ctx context.Context,
	projectionStore store.OutboxProjectionStore,
	fence store.ControllerEpochFence,
	task *corev1alpha1.Task,
	payload taskTerminalProjection,
) error {
	if task == nil {
		return fmt.Errorf("durable Task terminal projection identity is incomplete")
	}
	return enqueueDurableTaskTerminalProjectionForUID(ctx, projectionStore, fence, task, task.UID, payload)
}

func enqueueDurableTaskTerminalProjectionForUID(
	ctx context.Context,
	projectionStore store.OutboxProjectionStore,
	fence store.ControllerEpochFence,
	task *corev1alpha1.Task,
	projectionTaskUID types.UID,
	payload taskTerminalProjection,
) error {
	if projectionStore == nil || task == nil || task.UID == "" || payload.Attempt < 1 {
		return fmt.Errorf("durable Task terminal projection identity is incomplete")
	}
	if projectionTaskUID == "" {
		return fmt.Errorf("durable Task terminal projection source identity is incomplete")
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(encoded)
	projectionTime := task.CreationTimestamp.UTC()
	if payload.Execution.LastTransitionTime != nil && !payload.Execution.LastTransitionTime.IsZero() {
		projectionTime = payload.Execution.LastTransitionTime.UTC()
	} else if task.Status.CompletionTime != nil && !task.Status.CompletionTime.IsZero() {
		projectionTime = task.Status.CompletionTime.UTC()
	}
	if projectionTime.IsZero() {
		projectionTime = time.Unix(0, 0).UTC()
	}
	projection := &store.OutboxProjection{
		ID:            standaloneTaskTerminalProjectionIDForUID(task.Namespace, projectionTaskUID, payload.Attempt),
		AggregateKind: "Task", AggregateID: string(projectionTaskUID), ProjectionKind: "TaskTerminalStatus",
		PayloadDigest: "sha256:" + hex.EncodeToString(sum[:]), Payload: encoded,
		AvailableAt: projectionTime, CreatedAt: time.Now().UTC(),
	}
	if existing, getErr := projectionStore.GetOutboxProjection(ctx, projection.ID); getErr == nil {
		return validateMatchingTaskTerminalProjection(existing, projection)
	} else if !errors.Is(getErr, store.ErrNotFound) {
		return getErr
	}
	if _, err = projectionStore.EnqueueOutboxProjection(ctx, projection, fence); err == nil {
		return nil
	} else if !errors.Is(err, store.ErrConflict) {
		return err
	}
	existing, getErr := projectionStore.GetOutboxProjection(ctx, projection.ID)
	if getErr != nil {
		return errors.Join(err, getErr)
	}
	return validateMatchingTaskTerminalProjection(existing, projection)
}

func validateMatchingTaskTerminalProjection(existing *store.OutboxProjection, expected *store.OutboxProjection) error {
	if existing == nil || expected == nil || existing.ID != expected.ID ||
		existing.AggregateKind != expected.AggregateKind || existing.AggregateID != expected.AggregateID ||
		existing.ProjectionKind != expected.ProjectionKind || existing.PayloadDigest != expected.PayloadDigest ||
		!bytes.Equal(existing.Payload, expected.Payload) {
		return fmt.Errorf("%w: Task terminal projection %q was reused with different payload or metadata", store.ErrConflict, expected.ID)
	}
	return nil
}

const (
	defaultACPOutboxInterval = time.Second
	defaultACPOutboxLease    = 30 * time.Second
	defaultACPOutboxLimit    = 32
	defaultACPOutboxAttempts = 10
)

type ACPOutboxProjector struct {
	Client      client.Client
	Store       store.OutboxProjectionStore
	Epochs      *ControllerEpochManager
	WorkerID    string
	Interval    time.Duration
	MaxAttempts int64
}

func (p *ACPOutboxProjector) NeedLeaderElection() bool { return true }

func (p *ACPOutboxProjector) Start(ctx context.Context) error {
	if p.Client == nil || p.Store == nil || p.Epochs == nil {
		return fmt.Errorf("ACP outbox projector requires Kubernetes client, store, and epoch manager")
	}
	p.WorkerID = strings.TrimSpace(p.WorkerID)
	if p.WorkerID == "" {
		p.WorkerID = "acp-outbox-projector"
	}
	if p.Interval <= 0 {
		p.Interval = defaultACPOutboxInterval
	}
	if p.MaxAttempts <= 0 {
		p.MaxAttempts = defaultACPOutboxAttempts
	}
	ticker := time.NewTicker(p.Interval)
	defer ticker.Stop()
	for {
		if err := p.projectOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
			logf.FromContext(ctx).Error(err, "ACP outbox projection pass failed")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (p *ACPOutboxProjector) projectOnce(ctx context.Context) error {
	fence, err := p.Epochs.CurrentFence(ctx)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	projections, err := p.Store.ClaimOutboxProjections(ctx, store.ClaimOutboxProjectionsRequest{
		Fence: fence, WorkerID: p.WorkerID, Limit: defaultACPOutboxLimit, LeaseDuration: defaultACPOutboxLease, Now: now,
	})
	if err != nil {
		return err
	}
	for i := range projections {
		projection := &projections[i]
		deliveryDigest, projectErr := p.deliver(ctx, *projection)
		state := store.OutboxProjectionDelivered
		lastError := ""
		availableAt := time.Time{}
		if projectErr != nil {
			lastError = sanitizeOutboxError(projectErr)
			if projection.Attempts >= p.MaxAttempts {
				state = store.OutboxProjectionDeadLetter
			} else {
				state = store.OutboxProjectionPending
				availableAt = now.Add(outboxBackoff(projection.Attempts))
			}
			deliveryDigest = ""
		}
		digest, err := acpDomainDigest("outbox-completion", map[string]any{
			"id": projection.ID, "version": projection.Version, "state": state,
			"deliveryDigest": deliveryDigest, "lastError": lastError, "availableAt": availableAt,
		})
		if err != nil {
			return err
		}
		_, err = p.Store.CompleteOutboxProjection(ctx, store.CompleteOutboxProjectionRequest{
			ID: projection.ID, Fence: fence, ExpectedVersion: projection.Version, LeaseOwner: p.WorkerID,
			OperationID: "complete-" + fmt.Sprint(projection.Version), OperationDigest: digest,
			NewState: state, DeliveryDigest: deliveryDigest, LastError: lastError, AvailableAt: availableAt, UpdatedAt: time.Now().UTC(),
		})
		if err != nil {
			return err
		}
	}
	return nil
}

type taskTerminalProjection = taskterminal.Projection

func mergeTerminalExecutionStatus(existing *corev1alpha1.TaskExecutionStatus, projected corev1alpha1.TaskExecutionStatus) corev1alpha1.TaskExecutionStatus {
	if existing == nil {
		return projected
	}
	merged := *existing
	merged.State = projected.State
	merged.Outcome = projected.Outcome
	if projected.Attempt > 0 {
		merged.Attempt = projected.Attempt
	}
	if projected.PromptID != "" {
		merged.PromptID = projected.PromptID
	}
	if projected.Reason != "" {
		merged.Reason = projected.Reason
	}
	if projected.Message != "" {
		merged.Message = projected.Message
	}
	return merged
}

func (p *ACPOutboxProjector) deliver(ctx context.Context, projection store.OutboxProjection) (string, error) {
	if projection.ProjectionKind != "TaskTerminalStatus" {
		return "", fmt.Errorf("unsupported projection kind %q", projection.ProjectionKind)
	}
	var payload taskTerminalProjection
	if err := json.Unmarshal(projection.Payload, &payload); err != nil {
		return "", fmt.Errorf("decode task terminal projection: %w", err)
	}
	if payload.Namespace == "" || payload.Task == "" || payload.TaskUID == "" || payload.Attempt < 1 {
		return "", fmt.Errorf("task terminal projection identity is incomplete")
	}
	key := types.NamespacedName{Namespace: payload.Namespace, Name: payload.Task}
	var deliveredResourceVersion string
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		task := &corev1alpha1.Task{}
		if err := p.Client.Get(ctx, key, task); err != nil {
			if apierrors.IsNotFound(err) {
				deliveredResourceVersion = "deleted"
				return nil
			}
			return err
		}
		if string(task.UID) != payload.TaskUID {
			binding := executionBinding(task, corev1alpha1.AgentRuntimeContractHarnessV2)
			if binding == nil || string(binding.Task.UID) != payload.TaskUID || !acpTaskUsesRestoredSourceIdentity(task) ||
				task.Status.Execution == nil || task.Status.Execution.Attempt != payload.Attempt {
				return fmt.Errorf("task UID does not match outbox projection")
			}
			// Clean-cluster restore settlement already patched the new Task
			// incarnation directly. This projection remains immutable source
			// history, so delivery is an exact no-op against the restored UID.
			deliveredResourceVersion = task.ResourceVersion
			return nil
		}
		if payload.HarnessRuntime != nil {
			binding := task.Status.AgentExecutionBinding
			if binding == nil || binding.ContractVersion != corev1alpha1.AgentRuntimeContractHarnessV1 ||
				payload.BindingDigest == "" || binding.BindingDigest != payload.BindingDigest {
				return fmt.Errorf("harness v1 task binding does not match outbox projection")
			}
			if task.Status.HarnessRuntime != nil && task.Status.HarnessRuntime.Attempt > payload.Attempt {
				deliveredResourceVersion = task.ResourceVersion
				return nil
			}
			base := task.DeepCopy()
			now := metav1.Now()
			task.Status.Phase = payload.Phase
			task.Status.Message = payload.Message
			task.Status.Attempts = payload.Attempt
			if payload.ResultRef != nil {
				task.Status.ResultRef = payload.ResultRef.DeepCopy()
			}
			harnessRuntime := payload.HarnessRuntime.DeepCopy()
			harnessRuntime.LastTransitionTime = &now
			task.Status.HarnessRuntime = harnessRuntime
			switch payload.Phase {
			case corev1alpha1.TaskPhaseSucceeded, corev1alpha1.TaskPhaseFailed, corev1alpha1.TaskPhaseCancelled:
				task.Status.CompletionTime = &now
			default:
				task.Status.CompletionTime = nil
			}
			if err := p.Client.Status().Patch(ctx, task, client.MergeFrom(base)); err != nil {
				return err
			}
			deliveredResourceVersion = task.ResourceVersion
			return nil
		}
		if task.Status.Execution != nil && task.Status.Execution.Attempt > payload.Attempt {
			return fmt.Errorf("task has advanced beyond projected attempt")
		}
		base := task.DeepCopy()
		now := metav1.Now()
		task.Status.Phase = payload.Phase
		task.Status.Message = payload.Message
		task.Status.CompletionTime = &now
		execution := mergeTerminalExecutionStatus(task.Status.Execution, payload.Execution)
		execution.LastTransitionTime = &now
		task.Status.Execution = &execution
		if payload.Delivery != nil {
			delivery := taskDeliveryStatusForKubernetes(task, *payload.Delivery)
			delivery.LastTransitionTime = &now
			task.Status.Delivery = &delivery
		}
		if err := p.Client.Status().Patch(ctx, task, client.MergeFrom(base)); err != nil {
			return err
		}
		deliveredResourceVersion = task.ResourceVersion
		return nil
	})
	if err != nil {
		return "", err
	}
	return acpDomainDigest("outbox-delivery", map[string]any{
		"projectionID": projection.ID, "payloadDigest": projection.PayloadDigest, "resourceVersion": deliveredResourceVersion,
	})
}

func outboxBackoff(attempt int64) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 8 {
		attempt = 8
	}
	return time.Duration(1<<uint(attempt-1)) * time.Second
}

func sanitizeOutboxError(err error) string {
	if err == nil {
		return ""
	}
	// The outbox store rejects invalid UTF-8, so repair invalid sequences and
	// truncate on a rune boundary or the projection could never settle.
	message := strings.ToValidUTF8(strings.TrimSpace(err.Error()), "�")
	return truncateUTF8(message, 1024)
}
