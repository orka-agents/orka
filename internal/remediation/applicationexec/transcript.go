package applicationexec

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"slices"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation/contract"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	actionReceiptFinalizer = "applicationexec.orka.ai/action-receipts"
	jsonPatchPathKey       = "path"
	jsonPatchValueKey      = "value"
	jsonPatchTest          = "test"
	jsonPatchAdd           = "add"
)

var errReceiptsPending = errors.New("application action receipts remain pending")

func (b *Backend) receiptFinalizer(ctx context.Context, pod *corev1.Pod, add bool) error {
	current := &corev1.Pod{}
	if err := b.targetReader.Get(ctx, client.ObjectKeyFromObject(pod), current); err != nil {
		return safeError(contract.ErrUnavailable, err)
	}
	if current.UID != pod.UID || add && !current.DeletionTimestamp.IsZero() {
		return contract.ErrConflict
	}
	finalizers := slices.Clone(current.Finalizers)
	if add {
		if slices.Contains(finalizers, actionReceiptFinalizer) {
			return nil
		}
		finalizers = append(finalizers, actionReceiptFinalizer)
	} else {
		finalizers = slices.DeleteFunc(finalizers, func(value string) bool { return value == actionReceiptFinalizer })
		if reflect.DeepEqual(finalizers, current.Finalizers) {
			return nil
		}
	}
	content, _ := json.Marshal([]map[string]any{
		{"op": jsonPatchTest, jsonPatchPathKey: "/metadata/uid", jsonPatchValueKey: string(pod.UID)},
		{"op": jsonPatchTest, jsonPatchPathKey: "/metadata/resourceVersion", jsonPatchValueKey: current.ResourceVersion},
		{"op": jsonPatchAdd, jsonPatchPathKey: "/metadata/finalizers", jsonPatchValueKey: finalizers},
	})
	if err := b.target.Patch(ctx, current, client.RawPatch(types.JSONPatchType, content)); err != nil {
		return safeError(contract.ErrUnavailable, err)
	}
	return nil
}

func (b *Backend) observerTranscript(ctx context.Context, r *run, side, role string, pod *corev1.Pod,
	input pv.ApplicationWorkerInput) (*pv.ApplicationReport, bool, error) {
	if pod == nil || pod.UID == "" || len(pod.Status.ContainerStatuses) != 1 {
		return nil, false, contract.ErrConflict
	}
	status := pod.Status.ContainerStatuses[0]
	if status.ContainerID == "" || status.Name != workerModeObserve || status.RestartCount != 0 ||
		imageDigest(status.ImageID) != b.config.WorkerImageID {
		return nil, false, contract.ErrConflict
	}
	stream, err := b.targetKube.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
		Container: workerModeObserve, LimitBytes: new(int64(pv.MaxApplicationTranscriptBytes + 1))}).Stream(ctx)
	if err != nil {
		return nil, false, safeError(contract.ErrUnavailable, err)
	}
	frames, decodeErr := pv.DecodeApplicationTranscript(stream)
	closeErr := stream.Close()
	current := &corev1.Pod{}
	if err := b.targetReader.Get(ctx, client.ObjectKeyFromObject(pod), current); err != nil {
		return nil, false, safeError(contract.ErrUnavailable, err)
	}
	if current.UID != pod.UID || !reflect.DeepEqual(current.Spec, pod.Spec) ||
		!reflect.DeepEqual(current.OwnerReferences, pod.OwnerReferences) || len(current.Status.ContainerStatuses) != 1 ||
		current.Status.ContainerStatuses[0].ContainerID != status.ContainerID ||
		imageDigest(current.Status.ContainerStatuses[0].ImageID) != imageDigest(status.ImageID) ||
		current.Status.ContainerStatuses[0].RestartCount != 0 ||
		!reflect.DeepEqual(current.Status.InitContainerStatuses, pod.Status.InitContainerStatuses) {
		return nil, false, contract.ErrConflict
	}
	report, failed, err := b.recordTranscriptFrames(ctx, r, side, role, string(pod.UID), input, frames)
	if err != nil {
		return nil, false, err
	}
	// Complete prefix frames are cleanup receipts even when the worker or
	// transport failed later. Neither a prefix nor a failure is runtime proof.
	if closeErr != nil || decodeErr != nil && !errors.Is(decodeErr, io.ErrUnexpectedEOF) {
		return nil, false, safeError(contract.ErrUnavailable, errors.Join(closeErr, decodeErr))
	}
	terminated := current.Status.ContainerStatuses[0].State.Terminated
	if terminated == nil {
		return nil, false, nil
	}
	if failed || decodeErr != nil || report == nil || terminated.ExitCode != 0 || current.Status.Phase != corev1.PodSucceeded {
		return nil, false, contract.ErrUnavailable
	}
	if err := b.validateActionSet(r, side, role, input, *report); err != nil {
		return nil, false, err
	}
	return report, true, nil
}

func (b *Backend) recordTranscriptFrames(ctx context.Context, r *run, side, role, podUID string,
	input pv.ApplicationWorkerInput, frames []pv.ApplicationStreamFrame) (*pv.ApplicationReport, bool, error) {
	var report *pv.ApplicationReport
	actions, failed := 0, false
	for _, frame := range frames {
		if frame.PodUID != podUID || pv.ValidateApplicationStreamFrame(input, frame) != nil {
			return nil, false, contract.ErrConflict
		}
		switch frame.Kind {
		case "action":
			if frame.Action.Index != actions {
				return nil, false, contract.ErrConflict
			}
			if err := b.recordAction(ctx, r, side, role, input, *frame.Action); err != nil {
				return nil, false, err
			}
			actions++
		case "final":
			if actions != len(input.ActionTargets) {
				return nil, false, contract.ErrConflict
			}
			report = frame.Report
		case "failure":
			failed = true
		}
	}
	return report, failed, nil
}

func (b *Backend) retainedObserverInput(ctx context.Context, r *run, side, role string) (pv.ApplicationWorkerInput, error) {
	object, err := b.boundObject(ctx, r, resourceKey(observerSide(r, side), role+"-input"))
	if err != nil {
		return pv.ApplicationWorkerInput{}, err
	}
	config := object.(*corev1.ConfigMap)
	var input pv.ApplicationWorkerInput
	if json.Unmarshal(config.BinaryData["input.json"], &input) != nil || pv.ValidateApplicationWorkerInput(input) != nil ||
		input.Side != side || input.TaskUID != string(r.state.ArmTasks[side].UID) ||
		r.state.Binding == nil || input.Binding != *r.state.Binding {
		return pv.ApplicationWorkerInput{}, contract.ErrConflict
	}
	return input, nil
}

func (b *Backend) collectStoppingReceipts(ctx context.Context, r *run) error {
	groups := make(map[string]actionReceipt)
	for _, action := range r.state.Actions {
		groups[resourceKey(action.Side, action.Role)] = action
	}
	for _, group := range groups {
		if err := b.collectStoppingGroup(ctx, r, group); err != nil {
			return err
		}
	}
	return nil
}

func (b *Backend) collectStoppingGroup(ctx context.Context, r *run, group actionReceipt) error {
	key := resourceKey(observerSide(r, group.Side), group.Role)
	receipt, found := r.state.Pods[key]
	if r.effect(key) == nil || !found {
		return b.rejectUndispatchedActions(ctx, r, group.Side, group.Role)
	}
	pod := &corev1.Pod{}
	err := b.targetReader.Get(ctx, client.ObjectKey{Namespace: receipt.Namespace, Name: receipt.Identity.PodName}, pod)
	if apierrors.IsNotFound(err) {
		if stoppingActionsResolved(r, group.Side, group.Role) {
			return nil
		}
		return ErrUncertain
	}
	if err != nil {
		return pendingReceipts(err)
	}
	if string(pod.UID) != receipt.Identity.PodUID {
		return contract.ErrConflict
	}
	started := len(pod.Status.ContainerStatuses) == 1 && pod.Status.ContainerStatuses[0].ContainerID != ""
	var receiptErr error
	if started {
		input, err := b.retainedObserverInput(ctx, r, group.Side, group.Role)
		if err != nil {
			return err
		}
		_, _, receiptErr = b.observerTranscript(ctx, r, group.Side, group.Role, pod, input)
	} else {
		receiptErr = b.rejectUndispatchedActions(ctx, r, group.Side, group.Role)
	}
	// A failed optimistic save can leave an outcome only in the local struct.
	// Only the reread durable ledger can release the Pod's log-retention fence.
	if err := b.fresh(ctx, r); err != nil {
		return pendingReceipts(err)
	}
	if receiptErr != nil && !errors.Is(receiptErr, contract.ErrUnavailable) && !errors.Is(receiptErr, ErrUncertain) {
		return receiptErr
	}
	if !stoppingActionsResolved(r, group.Side, group.Role) {
		return pendingReceipts(receiptErr)
	}
	if !started || pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		return b.receiptFinalizer(ctx, pod, false)
	}
	return nil
}

func stoppingActionsResolved(r *run, side, role string) bool {
	for _, action := range r.state.Actions {
		if action.Side == side && action.Role == role && action.Outcome == nil && !action.NotStarted {
			return false
		}
	}
	return true
}

func pendingReceipts(cause error) error {
	if cause == nil {
		cause = ErrUncertain
	}
	return errors.Join(errReceiptsPending, safeError(contract.ErrUnavailable, cause))
}

// Completed observers are stopped before another receive window can select the
// same Service. This also promptly destroys their short-lived actor token files.
func (b *Backend) stopObserver(ctx context.Context, r *run, side, role string) (bool, error) {
	physicalSide := observerSide(r, side)
	key := resourceKey(physicalSide, role)
	entry := r.effect(key)
	if entry == nil || entry.UID == "" {
		return false, contract.ErrConflict
	}
	allGone := true
	pods := &corev1.PodList{}
	if err := b.targetReader.List(ctx, pods, client.InNamespace(entry.Namespace), client.Limit(64)); err != nil {
		return false, safeError(contract.ErrUnavailable, err)
	}
	if pods.Continue != "" {
		return false, ErrIsolation
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		owned := false
		for _, owner := range pod.OwnerReferences {
			owned = owned || owner.UID == entry.UID
		}
		if !owned {
			continue
		}
		job := &batchv1.Job{}
		job.Name, job.UID = entry.Name, entry.UID
		if !ownedByJob(pod, job) {
			return false, contract.ErrConflict
		}
		if err := b.receiptFinalizer(ctx, pod, false); err != nil {
			return false, err
		}
		allGone = false
		uid := pod.UID
		if pod.DeletionTimestamp.IsZero() {
			if err := b.target.Delete(ctx, pod, client.Preconditions{UID: &uid}); err != nil && !apierrors.IsNotFound(err) {
				return false, safeError(contract.ErrUnavailable, err)
			}
		}
	}
	routesGone, err := b.stopReceiverAccess(ctx, r, physicalSide, role)
	if err != nil {
		return false, err
	}
	allGone = allGone && routesGone
	for index := range r.state.Effects {
		current := r.state.Effects[index]
		if current.Key != key && current.Key != resourceKey(physicalSide, role+"-actors") {
			continue
		}
		done, err := b.deleteEffect(ctx, r, index)
		if err != nil {
			return false, err
		}
		allGone = allGone && done
	}
	return allGone, nil
}
