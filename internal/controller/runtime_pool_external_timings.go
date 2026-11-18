/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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
	// externalRuntimeStopRequestedAnnotation records, in the same form and in
	// the retirement authorization patch, when this controller process first
	// saw the RuntimePool retire that sequence. It only measures how long the
	// runtime took to stop, and is absent when the process did not see the
	// retirement begin, for example after a restart.
	externalRuntimeStopRequestedAnnotation = "acp.workspace.orka.ai/stop-requested-at"

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
	if ready, ok := externalRuntimeReadyAt(w, published); ok && !accepted.Before(ready) {
		phases = append(phases,
			externalRuntimePhase{externalRuntimePhaseProviderMaterialization, ready.Sub(published)},
			externalRuntimePhase{externalRuntimePhaseBootstrapAndProbe, accepted.Sub(ready)},
		)
	}
	if !accepted.Before(published) {
		phases = append(phases, externalRuntimePhase{externalRuntimePhaseStartTotal, accepted.Sub(published)})
	}
	return phases
}

// externalRuntimeReadyAt returns when the provider reported DataPlaneReady
// for the published sequence. A condition time keeps whole seconds only, so a
// report in the publication's own second can read as slightly before it; that
// report counts as ready at publication. Readiness from an earlier second
// predates this sequence and is ignored.
func externalRuntimeReadyAt(w *workspacev1alpha1.ExecutionWorkspace, published time.Time) (time.Time, bool) {
	ready := meta.FindStatusCondition(w.Status.Conditions, string(workspacev1alpha1.ConditionWorkspaceDataPlaneReady))
	if ready == nil || ready.Status != metav1.ConditionTrue {
		return time.Time{}, false
	}
	at := ready.LastTransitionTime.Time
	if at.Before(published.Truncate(time.Second)) {
		return time.Time{}, false
	}
	if at.Before(published) {
		at = published
	}
	return at, true
}

// externalRuntimeStopPhases breaks a runtime stop into Core's drain, from the
// first observed retirement of the sequence to retirement authorization, and
// the provider's retirement up to Core's independent proof that the instance
// stopped. Detach and settlement times are not used: settlement can suspend
// the workspace after the pool already began, or even finished, its drain.
func externalRuntimeStopPhases(w *workspacev1alpha1.ExecutionWorkspace, stopped time.Time) []externalRuntimePhase {
	retirement := w.Spec.Retirement
	authorized, ok := parseExternalRuntimeMilestone(w.Annotations[externalRetirementAuthorizedAnnotation], retirement.Sequence)
	if !ok || stopped.Before(authorized) {
		return nil
	}
	var phases []externalRuntimePhase
	if requested, ok := parseExternalRuntimeMilestone(w.Annotations[externalRuntimeStopRequestedAnnotation], retirement.Sequence); ok && !authorized.Before(requested) {
		phases = append(phases,
			externalRuntimePhase{externalRuntimePhaseDrain, authorized.Sub(requested)},
			externalRuntimePhase{externalRuntimePhaseStopTotal, stopped.Sub(requested)},
		)
	}
	return append(phases, externalRuntimePhase{externalRuntimePhaseProviderRetirement, stopped.Sub(authorized)})
}

// stampExternalRuntimeStopRequested adds the observed start of the stop to the
// patch that first authorizes the sequence's retirement. A later
// authorization of the same sequence, such as deletion superseding a Suspend,
// keeps that start while the instance still runs, and drops it once the
// instance already stopped: its drain had ended.
func (r *RuntimePoolReconciler) stampExternalRuntimeStopRequested(w *workspacev1alpha1.ExecutionWorkspace, previous *workspacev1alpha1.WorkloadRetirement, pool types.UID, authorized time.Time, retired bool) {
	sequence := w.Spec.Retirement.Sequence
	if previous != nil && previous.Sequence == sequence {
		if retired {
			delete(w.Annotations, externalRuntimeStopRequestedAnnotation)
		}
		return
	}
	delete(w.Annotations, externalRuntimeStopRequestedAnnotation)
	if requested, ok := r.externalTimings.retirementRequested(pool, sequence); ok && !requested.After(authorized) {
		setExternalRuntimeMilestone(w, externalRuntimeStopRequestedAnnotation, sequence, requested)
	}
}

// noteExternalRuntimeStopProven keeps when Core first proved the authorized
// retirement's termination, so a Stopped write that fails and is retried
// does not count Core's retry delay as the provider's retirement.
func (r *RuntimePoolReconciler) noteExternalRuntimeStopProven(pool *corev1alpha1.RuntimePool, w *workspacev1alpha1.ExecutionWorkspace) {
	if w.Spec.Retirement != nil {
		r.externalTimings.noteStopProven(pool.UID, externalRuntimeStopKey(w.Spec.Retirement), r.now())
	}
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

// externalRuntimeTimingReports remembers, per RuntimePool, the runtime
// instance whose start and the retirement whose stop this controller process
// already reported, when it first saw the pool retire its current sequence,
// and when it first proved that retirement's termination. A readmission or a
// repeated Stopped write after a transient failure is then not reported
// again. It lives in memory only, so it costs no write: after a controller
// restart one already-reported transition can be reported again, and a
// retirement already under way is not given a start.
type externalRuntimeTimingReports struct {
	mu    sync.Mutex
	pools map[types.UID]*externalRuntimeTimingReport
}

type externalRuntimeTimingReport struct {
	startedInstance string
	stoppedKey      string
	provenKey       string
	provenAt        time.Time

	// retiringSequence is the sequence the pool is retiring, or zero.
	// retirementRequestedAt is when this process first saw that retirement,
	// or zero when the retirement was already under way at the process's
	// first look at the pool.
	retiringSequence      int64
	retirementRequestedAt time.Time
}

func (t *externalRuntimeTimingReports) poolLocked(uid types.UID) *externalRuntimeTimingReport {
	if t.pools == nil {
		t.pools = map[types.UID]*externalRuntimeTimingReport{}
	}
	report := t.pools[uid]
	if report == nil {
		report = &externalRuntimeTimingReport{}
		t.pools[uid] = report
	}
	return report
}

// observeRetirement records whether the pool is retiring the sequence, and
// when this process first saw it begin. A pool that leaves retirement before
// it was authorized starts a later retirement afresh.
func (t *externalRuntimeTimingReports) observeRetirement(uid types.UID, sequence int64, retiring bool, at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, seen := t.pools[uid]
	report := t.poolLocked(uid)
	switch {
	case !retiring:
		report.retiringSequence, report.retirementRequestedAt = 0, time.Time{}
	case report.retiringSequence == sequence:
	case !seen:
		// The retirement may have begun before this process started.
		report.retiringSequence, report.retirementRequestedAt = sequence, time.Time{}
	default:
		report.retiringSequence, report.retirementRequestedAt = sequence, at
	}
}

// retirementRequested returns when this process first saw the pool retire the
// sequence, if it saw the retirement begin.
func (t *externalRuntimeTimingReports) retirementRequested(uid types.UID, sequence int64) (time.Time, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	report := t.pools[uid]
	if report == nil || report.retiringSequence != sequence || report.retirementRequestedAt.IsZero() {
		return time.Time{}, false
	}
	return report.retirementRequestedAt, true
}

// claimStart reports whether the instance's start is not yet reported, and
// marks it reported.
func (t *externalRuntimeTimingReports) claimStart(uid types.UID, instance string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	report := t.poolLocked(uid)
	if report.startedInstance == instance {
		return false
	}
	report.startedInstance = instance
	return true
}

// claimStop reports whether the retirement's stop is not yet reported, marks
// it reported, and returns when its termination was first proven, if known.
func (t *externalRuntimeTimingReports) claimStop(uid types.UID, key string) (time.Time, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	report := t.poolLocked(uid)
	if report.stoppedKey == key {
		return time.Time{}, false
	}
	report.stoppedKey = key
	if report.provenKey == key {
		return report.provenAt, true
	}
	return time.Time{}, true
}

// noteStopProven keeps the first time Core proved the retirement's
// termination, so cleanup that follows the proof is not counted as the
// provider's retirement.
func (t *externalRuntimeTimingReports) noteStopProven(uid types.UID, key string, at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	report := t.poolLocked(uid)
	if report.provenKey != key {
		report.provenKey, report.provenAt = key, at
	}
}

func (t *externalRuntimeTimingReports) forget(uid types.UID) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.pools, uid)
}

func externalRuntimeStopKey(retirement *workspacev1alpha1.WorkloadRetirement) string {
	return strconv.FormatInt(retirement.Sequence, 10) + "/" + string(retirement.Action)
}

type externalRuntimeTimingWorkspaceKey struct{}

// withExternalRuntimeTimingWorkspace carries the workspace this reconcile
// already read and patches in place, so a persisted transition is reported
// from it rather than lost to a failed second read.
func withExternalRuntimeTimingWorkspace(ctx context.Context, w *workspacev1alpha1.ExecutionWorkspace) context.Context {
	return context.WithValue(ctx, externalRuntimeTimingWorkspaceKey{}, w)
}

func (r *RuntimePoolReconciler) externalRuntimeTimingWorkspace(ctx context.Context, pool *corev1alpha1.RuntimePool) *workspacev1alpha1.ExecutionWorkspace {
	if w, ok := ctx.Value(externalRuntimeTimingWorkspaceKey{}).(*workspacev1alpha1.ExecutionWorkspace); ok && w != nil {
		return w
	}
	w, err := r.externalPoolWorkspace(ctx, pool)
	if err != nil {
		return nil
	}
	return w
}

// observeExternalRuntimeTransition logs and records how long an
// execution-workspace runtime took to start or stop. It runs only after a
// status write persisted the transition. A start is reported the first time
// an instance admits work, even when an earlier probe already recorded it
// while it booted; a stop when the pool first records Stopped for a
// retirement.
func (r *RuntimePoolReconciler) observeExternalRuntimeTransition(ctx context.Context, pool *corev1alpha1.RuntimePool, previous, current corev1alpha1.RuntimePoolStatus) {
	if current.AdmissionState == corev1alpha1.RuntimePoolAdmissionAccepting && current.ActiveInstance != nil &&
		(previous.AdmissionState != corev1alpha1.RuntimePoolAdmissionAccepting || previous.ActiveInstance == nil ||
			previous.ActiveInstance.RuntimeInstanceID != current.ActiveInstance.RuntimeInstanceID) {
		r.observeExternalRuntimeStart(ctx, pool, current.ActiveInstance.RuntimeInstanceID)
		return
	}
	if current.Lifecycle == corev1alpha1.RuntimePoolLifecycleStopped && previous.Lifecycle != corev1alpha1.RuntimePoolLifecycleStopped {
		r.observeExternalRuntimeStop(ctx, pool)
	}
}

func (r *RuntimePoolReconciler) observeExternalRuntimeStart(ctx context.Context, pool *corev1alpha1.RuntimePool, instance string) {
	w := r.externalRuntimeTimingWorkspace(ctx, pool)
	if w == nil || w.Spec.Workload == nil || !r.externalTimings.claimStart(pool.UID, instance) {
		return
	}
	path := externalRuntimeStartPath(w.Spec.Workload)
	phases := externalRuntimeStartPhases(pool, w, r.now())
	for _, phase := range phases {
		orkametrics.RecordACPWorkspaceRuntimePhase(path, phase.name, phase.duration)
	}
	log.FromContext(ctx).Info(externalRuntimeStartedLogMessage, append([]any{
		"runtimePool", pool.Name, "path", path, "sequence", w.Spec.Workload.Sequence,
	}, externalRuntimePhaseKeysAndValues(phases)...)...)
}

// observeExternalRuntimeStop reports a stop whose termination Core proved,
// either through a Stopped status write or, for a deleted RuntimePool,
// through the release of its finalizer.
func (r *RuntimePoolReconciler) observeExternalRuntimeStop(ctx context.Context, pool *corev1alpha1.RuntimePool) {
	w := r.externalRuntimeTimingWorkspace(ctx, pool)
	if w == nil || w.Spec.Retirement == nil {
		return
	}
	proven, ok := r.externalTimings.claimStop(pool.UID, externalRuntimeStopKey(w.Spec.Retirement))
	if !ok {
		return
	}
	stopped := r.now()
	if !proven.IsZero() && proven.Before(stopped) {
		stopped = proven
	}
	action := strings.ToLower(string(w.Spec.Retirement.Action))
	phases := externalRuntimeStopPhases(w, stopped)
	for _, phase := range phases {
		orkametrics.RecordACPWorkspaceRuntimePhase(action, phase.name, phase.duration)
	}
	log.FromContext(ctx).Info(externalRuntimeStoppedLogMessage, append([]any{
		"runtimePool", pool.Name, "action", action, "sequence", w.Spec.Retirement.Sequence,
	}, externalRuntimePhaseKeysAndValues(phases)...)...)
}
