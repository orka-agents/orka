/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/metrics"
)

// turnRequestedPark reports whether the settling turn recorded a wait for
// itself. It reads the authoritative Task: the wait was written by the
// brokered park_until call during this exact attempt and prompt.
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
	if err := reader.Get(ctx, client.ObjectKeyFromObject(task), latest); err != nil || latest.UID != task.UID {
		return false
	}
	wait := latest.Status.Wait
	return wait != nil && wait.State == corev1alpha1.TaskWaitStateRequested &&
		wait.Attempt == task.Status.Execution.Attempt && wait.PromptID == task.Status.Execution.PromptID
}

// releaseParkedRuntimeSession deletes the exact resident RuntimeSession of a
// parked read-only turn after its Session result is final. It is best effort:
// the binding already requires recreation, so a failed deletion only leaves
// the resident capacity that existed before parking.
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
		return
	}
	d.forgetRuntimeSessionBinding(sessionUID)
	metrics.RecordTaskWaitRuntimeRelease("released")
}
