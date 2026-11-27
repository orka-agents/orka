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

type acpParkedRuntimeRelease struct {
	task             *corev1alpha1.Task
	runtimeSessionID harnessv2.RuntimeSessionID
	fence            harnessv2.Fence
}

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
	d.forgetRetiredRuntimeSessionBinding(sessionUID, runtimeFence)
	d.clearPendingParkedRuntimeRelease(parkedRuntimeReleaseKey(sessionUID, runtimeFence))
	metrics.RecordTaskWaitRuntimeRelease("released")
}

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
