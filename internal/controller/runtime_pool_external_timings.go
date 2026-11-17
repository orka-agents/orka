/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"strconv"
	"strings"
	"time"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	orkametrics "github.com/orka-agents/orka/internal/metrics"
)

const (
	// externalWorkloadPublishedAnnotation records when Core published the
	// workload request of a sequence, as "<sequence> <RFC3339Nano>". It only
	// measures how long the runtime took to start.
	externalWorkloadPublishedAnnotation = "acp.workspace.orka.ai/workload-published-at"
	// externalRetirementAuthorizedAnnotation records when Core authorized the
	// provider to retire a sequence, in the same form. It only measures how
	// long the runtime took to stop.
	externalRetirementAuthorizedAnnotation = "acp.workspace.orka.ai/retirement-authorized-at"

	externalRuntimeStartedLogMessage = "ACP workspace runtime started"
	externalRuntimeStoppedLogMessage = "ACP workspace runtime stopped"
)

// Start paths of an execution-workspace runtime.
const (
	externalRuntimePathCold        = "cold"
	externalRuntimePathRestore     = "restore"
	externalRuntimePathResume      = "resume"
	externalRuntimePathReplacement = "replacement"
)

// Phases of an execution-workspace runtime start and stop.
const (
	externalRuntimePhaseProvision               = "provision"
	externalRuntimePhaseProviderMaterialization = "provider_materialization"
	externalRuntimePhaseBootstrapAndProbe       = "bootstrap_and_probe"
	externalRuntimePhaseStartTotal              = "start_total"
	externalRuntimePhaseDrain                   = "drain"
	externalRuntimePhaseProviderRetirement      = "provider_retirement"
	externalRuntimePhaseStopTotal               = "stop_total"
)

func formatExternalRuntimeMilestone(sequence int64, at time.Time) string {
	return strconv.FormatInt(sequence, 10) + " " + at.UTC().Format(time.RFC3339Nano)
}

// parseExternalRuntimeMilestone returns the recorded time only when it belongs
// to the given sequence.
func parseExternalRuntimeMilestone(raw string, sequence int64) (time.Time, bool) {
	rawSequence, rawTime, ok := strings.Cut(strings.TrimSpace(raw), " ")
	if !ok {
		return time.Time{}, false
	}
	recorded, err := strconv.ParseInt(rawSequence, 10, 64)
	if err != nil || recorded != sequence {
		return time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339Nano, rawTime)
	if err != nil {
		return time.Time{}, false
	}
	return at, true
}

// setExternalRuntimeMilestone stamps an annotation on a workspace about to be
// patched, so the milestone costs no extra write.
func setExternalRuntimeMilestone(w *workspacev1alpha1.ExecutionWorkspace, annotation string, sequence int64, at time.Time) {
	if w.Annotations == nil {
		w.Annotations = map[string]string{}
	}
	w.Annotations[annotation] = formatExternalRuntimeMilestone(sequence, at)
}

func externalRuntimeStartPath(request *workspacev1alpha1.WorkloadRequest) string {
	switch {
	case request.RetainedData != nil:
		return externalRuntimePathResume
	case request.Sequence == 1 && request.RestoreFrom != nil:
		return externalRuntimePathRestore
	case request.Sequence == 1:
		return externalRuntimePathCold
	default:
		return externalRuntimePathReplacement
	}
}

type externalRuntimePhase struct {
	name     string
	duration time.Duration
}

// externalRuntimeStartPhases breaks a runtime start into the time Core took to
// provision and publish it (first sequence only, from RuntimePool creation),
// the provider's materialization up to its DataPlaneReady report, and Core's
// credential bootstrap and authenticated probe up to admission.
func externalRuntimeStartPhases(pool *corev1alpha1.RuntimePool, w *workspacev1alpha1.ExecutionWorkspace, accepted time.Time) []externalRuntimePhase {
	request := w.Spec.Workload
	published, ok := parseExternalRuntimeMilestone(w.Annotations[externalWorkloadPublishedAnnotation], request.Sequence)
	if !ok {
		return nil
	}
	var phases []externalRuntimePhase
	if request.Sequence == 1 && !pool.CreationTimestamp.IsZero() && !published.Before(pool.CreationTimestamp.Time) {
		phases = append(phases, externalRuntimePhase{externalRuntimePhaseProvision, published.Sub(pool.CreationTimestamp.Time)})
	}
	ready := meta.FindStatusCondition(w.Status.Conditions, string(workspacev1alpha1.ConditionWorkspaceDataPlaneReady))
	if ready != nil && ready.Status == metav1.ConditionTrue && !ready.LastTransitionTime.Time.Before(published) && !accepted.Before(ready.LastTransitionTime.Time) {
		phases = append(phases,
			externalRuntimePhase{externalRuntimePhaseProviderMaterialization, ready.LastTransitionTime.Sub(published)},
			externalRuntimePhase{externalRuntimePhaseBootstrapAndProbe, accepted.Sub(ready.LastTransitionTime.Time)},
		)
	}
	if !accepted.Before(published) {
		phases = append(phases, externalRuntimePhase{externalRuntimePhaseStartTotal, accepted.Sub(published)})
	}
	return phases
}

// externalRuntimeStopPhases breaks a runtime stop into Core's drain, from the
// last detach to retirement authorization, and the provider's retirement up to
// Core's independent proof that the instance stopped.
func externalRuntimeStopPhases(w *workspacev1alpha1.ExecutionWorkspace, stopped time.Time) []externalRuntimePhase {
	retirement := w.Spec.Retirement
	authorized, ok := parseExternalRuntimeMilestone(w.Annotations[externalRetirementAuthorizedAnnotation], retirement.Sequence)
	if !ok || stopped.Before(authorized) {
		return nil
	}
	var phases []externalRuntimePhase
	if retirement.Action == workspacev1alpha1.WorkloadRetirementSuspend && w.Spec.Attachment == nil {
		// The last detach starts a Suspend only when nothing reattached since.
		if detached, err := time.Parse(time.RFC3339Nano, w.Annotations[acpWorkspaceLastDetachedAnnotation]); err == nil && !authorized.Before(detached) {
			phases = append(phases,
				externalRuntimePhase{externalRuntimePhaseDrain, authorized.Sub(detached)},
				externalRuntimePhase{externalRuntimePhaseStopTotal, stopped.Sub(detached)},
			)
		}
	}
	return append(phases, externalRuntimePhase{externalRuntimePhaseProviderRetirement, stopped.Sub(authorized)})
}

func externalRuntimePhaseKeysAndValues(phases []externalRuntimePhase) []any {
	values := make([]any, 0, 2*len(phases))
	for _, phase := range phases {
		values = append(values, phaseLogKey(phase.name), phase.duration.Milliseconds())
	}
	return values
}

// phaseLogKey turns provider_materialization into providerMaterializationMs.
func phaseLogKey(phase string) string {
	parts := strings.Split(phase, "_")
	for i := 1; i < len(parts); i++ {
		if parts[i] != "" {
			parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
		}
	}
	return strings.Join(parts, "") + "Ms"
}

// observeExternalRuntimeTransition logs and records how long an
// execution-workspace runtime took to start or stop. It runs only after a
// status write persisted the transition, so each start is reported once, when
// a new runtime instance first admits work, and each stop once, when the pool
// first records Stopped.
func (r *RuntimePoolReconciler) observeExternalRuntimeTransition(ctx context.Context, pool *corev1alpha1.RuntimePool, previous, current corev1alpha1.RuntimePoolStatus) {
	started := current.AdmissionState == corev1alpha1.RuntimePoolAdmissionAccepting && current.ActiveInstance != nil &&
		(previous.ActiveInstance == nil || previous.ActiveInstance.RuntimeInstanceID != current.ActiveInstance.RuntimeInstanceID)
	stopped := current.Lifecycle == corev1alpha1.RuntimePoolLifecycleStopped && previous.Lifecycle != corev1alpha1.RuntimePoolLifecycleStopped
	if !started && !stopped {
		return
	}
	w, err := r.externalPoolWorkspace(ctx, pool)
	if err != nil || w.Spec.Workload == nil {
		return
	}
	now := r.now()
	logger := log.FromContext(ctx)
	if started {
		path := externalRuntimeStartPath(w.Spec.Workload)
		phases := externalRuntimeStartPhases(pool, w, now)
		for _, phase := range phases {
			orkametrics.RecordACPWorkspaceRuntimePhase(path, phase.name, phase.duration)
		}
		logger.Info(externalRuntimeStartedLogMessage, append([]any{
			"runtimePool", pool.Name, "path", path, "sequence", w.Spec.Workload.Sequence,
		}, externalRuntimePhaseKeysAndValues(phases)...)...)
		return
	}
	if w.Spec.Retirement == nil {
		return
	}
	action := strings.ToLower(string(w.Spec.Retirement.Action))
	phases := externalRuntimeStopPhases(w, now)
	for _, phase := range phases {
		orkametrics.RecordACPWorkspaceRuntimePhase(action, phase.name, phase.duration)
	}
	logger.Info(externalRuntimeStoppedLogMessage, append([]any{
		"runtimePool", pool.Name, "action", action, "sequence", w.Spec.Retirement.Sequence,
	}, externalRuntimePhaseKeysAndValues(phases)...)...)
}
