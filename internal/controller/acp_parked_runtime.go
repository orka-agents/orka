/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/metrics"
)

// turnRequestedPark reports whether the settling turn recorded a wait for
// itself. It reads the authoritative Task: the wait was written by the
// brokered park_until call during this exact attempt and prompt. A failed
// read counts as parked, so the turn's runtime is released and its process
// memory can never be reused by a continuation; an unneeded release costs
// only a recreated RuntimeSession.
func (d *ACPDispatcher) turnRequestedPark(ctx context.Context, task *corev1alpha1.Task) bool {
	if task == nil || task.Status.Execution == nil {
		return false
	}
	var reader = d.APIReader
	if reader == nil {
		reader = d.Client
	}
	if reader == nil {
		return false
	}
	latest := &corev1alpha1.Task{}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(task), latest); err != nil {
		logf.FromContext(ctx).Info("Could not confirm whether the turn parked; releasing its runtime",
			"namespace", task.Namespace, "task", task.Name, "reason", err.Error())
		return true
	}
	if latest.UID != task.UID {
		return false
	}
	wait := latest.Status.Wait
	return wait != nil && wait.Attempt == task.Status.Execution.Attempt && wait.PromptID == task.Status.Execution.PromptID
}

// acpParkedRuntimeRelease is the exact RuntimeSession of a parked turn whose
// deletion failed. It still holds a resident slot on its runtime instance.
type acpParkedRuntimeRelease struct {
	task             *corev1alpha1.Task
	runtimeSessionID harnessv2.RuntimeSessionID
	fence            harnessv2.Fence
}

// releaseParkedRuntimeSession deletes the exact resident RuntimeSession of a
// parked read-only turn after its Session result is final. The binding
// already requires recreation, so a failed deletion is recorded and retried
// before the Session's next turn needs a resident slot of its own.
func (d *ACPDispatcher) releaseParkedRuntimeSession(
	ctx context.Context,
	runtimeClient *harnessv2.Client,
	runtimeSessionID harnessv2.RuntimeSessionID,
	task *corev1alpha1.Task,
	runtimeFence harnessv2.Fence,
	sessionUID string,
) {
	if err := d.deleteRuntimeSession(context.WithoutCancel(ctx), runtimeClient, runtimeSessionID, task, runtimeFence, "task_parked"); err != nil {
		logf.FromContext(ctx).Error(err, "release parked ACP RuntimeSession", "namespace", task.Namespace, "task", task.Name,
			"runtimeSessionID", runtimeSessionID)
		metrics.RecordTaskWaitRuntimeRelease("failed")
		d.setPendingParkedRuntimeRelease(parkedRuntimeReleaseKey(sessionUID, runtimeFence), acpParkedRuntimeRelease{
			task: task.DeepCopy(), runtimeSessionID: runtimeSessionID, fence: runtimeFence,
		})
		return
	}
	// The Session lease was released when the result was finalized, so a
	// later turn may already have installed a newer binding. Forget only the
	// binding of the RuntimeSession this deleted.
	d.forgetRetiredRuntimeSessionBinding(sessionUID, runtimeFence)
	d.clearPendingParkedRuntimeRelease(parkedRuntimeReleaseKey(sessionUID, runtimeFence))
	metrics.RecordTaskWaitRuntimeRelease("released")
}

// forgetRetiredRuntimeSessionBinding removes the cached binding only while it
// still names the exact retired RuntimeSession incarnation.
func (d *ACPDispatcher) forgetRetiredRuntimeSessionBinding(sessionUID string, retired harnessv2.Fence) {
	d.mu.Lock()
	defer d.mu.Unlock()
	key := strings.TrimSpace(sessionUID)
	binding, ok := d.runtimeSessions[key]
	if !ok || binding.Generation != retired.RuntimeSessionGeneration ||
		binding.RuntimeInstanceID != retired.RuntimeInstanceID || binding.SupervisorBootID != retired.SupervisorBootID {
		return
	}
	delete(d.runtimeSessions, key)
}

// retryParkedRuntimeRelease retries every failed release of a parked turn's
// RuntimeSession in this Session before the next turn claims a resident
// slot, so that cleanup never waits for spare capacity. Each release is its
// own exact incarnation and is forgotten only once that deletion succeeded
// or its runtime instance was replaced, which retired its sessions. It never
// deletes a RuntimeSession generation the current turn could use.
func (d *ACPDispatcher) retryParkedRuntimeRelease(
	ctx context.Context,
	runtimeClient *harnessv2.Client,
	sessionUID string,
	runtimeInstanceID harnessv2.RuntimeInstanceID,
	generation uint64,
) {
	for key, pending := range d.pendingParkedRuntimeReleases(sessionUID) {
		if pending.fence.RuntimeInstanceID != runtimeInstanceID {
			d.clearPendingParkedRuntimeRelease(key)
			continue
		}
		if pending.fence.RuntimeSessionGeneration >= generation {
			continue
		}
		if err := d.deleteRuntimeSession(context.WithoutCancel(ctx), runtimeClient, pending.runtimeSessionID, pending.task, pending.fence, "task_parked"); err != nil {
			logf.FromContext(ctx).Error(err, "retry release of parked ACP RuntimeSession", "namespace", pending.task.Namespace,
				"task", pending.task.Name, "runtimeSessionID", pending.runtimeSessionID)
			metrics.RecordTaskWaitRuntimeRelease("failed")
			continue
		}
		d.clearPendingParkedRuntimeRelease(key)
		metrics.RecordTaskWaitRuntimeRelease("released")
	}
}

// acpParkedRuntimeReleaseKey names one exact RuntimeSession incarnation, so
// releases of different generations or runtime boots never replace or clear
// each other.
type acpParkedRuntimeReleaseKey struct {
	sessionUID string
	generation uint64
	instanceID harnessv2.RuntimeInstanceID
	bootID     harnessv2.SupervisorBootID
}

func parkedRuntimeReleaseKey(sessionUID string, fence harnessv2.Fence) acpParkedRuntimeReleaseKey {
	return acpParkedRuntimeReleaseKey{
		sessionUID: strings.TrimSpace(sessionUID), generation: fence.RuntimeSessionGeneration,
		instanceID: fence.RuntimeInstanceID, bootID: fence.SupervisorBootID,
	}
}

func (d *ACPDispatcher) setPendingParkedRuntimeRelease(key acpParkedRuntimeReleaseKey, release acpParkedRuntimeRelease) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.parkedRuntimeReleases == nil {
		d.parkedRuntimeReleases = make(map[acpParkedRuntimeReleaseKey]acpParkedRuntimeRelease)
	}
	d.parkedRuntimeReleases[key] = release
}

// pendingParkedRuntimeReleases returns a snapshot of the Session's pending
// releases.
func (d *ACPDispatcher) pendingParkedRuntimeReleases(sessionUID string) map[acpParkedRuntimeReleaseKey]acpParkedRuntimeRelease {
	d.mu.Lock()
	defer d.mu.Unlock()
	sessionUID = strings.TrimSpace(sessionUID)
	pending := make(map[acpParkedRuntimeReleaseKey]acpParkedRuntimeRelease)
	for key, release := range d.parkedRuntimeReleases {
		if key.sessionUID == sessionUID {
			pending[key] = release
		}
	}
	return pending
}

func (d *ACPDispatcher) clearPendingParkedRuntimeRelease(key acpParkedRuntimeReleaseKey) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.parkedRuntimeReleases, key)
}
