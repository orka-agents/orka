/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

type acpPreDispatchMilestone int

const (
	acpPreDispatchInformerAdded acpPreDispatchMilestone = iota
	acpPreDispatchObserved
	acpPreDispatchStatusInitialized
	acpPreDispatchExecutionBound
	acpPreDispatchSpeculativeStart
	acpPreDispatchSpeculativePoolActivated
	acpPreDispatchWorkspaceAttachRequested
	acpPreDispatchWorkspaceReady
	acpPreDispatchRuntimePoolReady
	acpPreDispatchAttemptPersisted
	acpPreDispatchQueued
	acpPreDispatchScanned
	acpPreDispatchReserved
	acpPreDispatchStarted
	acpPreDispatchMilestoneCount
)

var acpPreDispatchMilestoneNames = [acpPreDispatchMilestoneCount]string{
	"informerAdded",
	"observed",
	"statusInitialized",
	"executionBound",
	"speculativeStart",
	"speculativePoolActivated",
	"workspaceAttachRequested",
	"workspaceReady",
	"runtimePoolReady",
	"promptAttemptPersisted",
	"dispatchQueued",
	"dispatchScanned",
	"reserved",
	"started",
}

const (
	acpPreDispatchTimingsLogMessage = "ACP pre-dispatch timings"
	acpPreDispatchMaxRecords        = 4096
	acpPreDispatchRetention         = 30 * time.Minute
)

type acpPreDispatchRecord struct {
	created time.Time
	touched time.Time
	marks   [acpPreDispatchMilestoneCount]time.Time
}

type acpPreDispatchTimings struct {
	now     func() time.Time
	mu      sync.Mutex
	records map[types.UID]*acpPreDispatchRecord
	added   map[types.UID]time.Time
}

func newACPPreDispatchTimings(now func() time.Time) *acpPreDispatchTimings {
	return &acpPreDispatchTimings{
		now: now, records: make(map[types.UID]*acpPreDispatchRecord), added: make(map[types.UID]time.Time),
	}
}

var acpPreDispatch = newACPPreDispatchTimings(time.Now)

func (t *acpPreDispatchTimings) mark(task *corev1alpha1.Task, milestone acpPreDispatchMilestone) {
	t.record(task, milestone, false)
}

func (t *acpPreDispatchTimings) markStarted(task *corev1alpha1.Task) {
	t.record(task, acpPreDispatchStarted, true)
}

func (t *acpPreDispatchTimings) markQueueWriteIssued(task *corev1alpha1.Task) {
	t.record(task, acpPreDispatchQueued, true)
}

func acpPreDispatchCreatesRecord(milestone acpPreDispatchMilestone) bool {
	return milestone == acpPreDispatchObserved || milestone == acpPreDispatchStatusInitialized
}

func acpPreDispatchTaskAdded(object any, isInInitialList bool) {
	task, ok := object.(*corev1alpha1.Task)
	if !ok || isInInitialList || task.Status.Execution != nil || task.Spec.Schedule != "" ||
		!task.DeletionTimestamp.IsZero() || task.Status.Phase.IsTerminal() {
		return
	}
	acpPreDispatch.informerAdded(task)
}

func (t *acpPreDispatchTimings) informerAdded(task *corev1alpha1.Task) {
	if t == nil || task == nil || task.UID == "" || task.Spec.Type != corev1alpha1.TaskTypeAgent {
		return
	}
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	if record := t.records[task.UID]; record != nil {
		if record.marks[acpPreDispatchObserved].IsZero() && record.marks[acpPreDispatchInformerAdded].IsZero() {
			record.marks[acpPreDispatchInformerAdded] = now
		}
		return
	}
	if len(t.added) >= acpPreDispatchMaxRecords {
		for uid, at := range t.added {
			if now.Sub(at) >= acpPreDispatchRetention {
				delete(t.added, uid)
			}
		}
		if len(t.added) >= acpPreDispatchMaxRecords {
			return
		}
	}
	t.added[task.UID] = now
}

func (t *acpPreDispatchTimings) record(task *corev1alpha1.Task, milestone acpPreDispatchMilestone, replace bool) {
	if t == nil || task == nil || task.UID == "" || task.Spec.Type != corev1alpha1.TaskTypeAgent ||
		milestone < 0 || milestone >= acpPreDispatchMilestoneCount {
		return
	}
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	record := t.records[task.UID]
	if record == nil {
		if !acpPreDispatchCreatesRecord(milestone) {
			return
		}
		if len(t.records) >= acpPreDispatchMaxRecords {
			t.pruneLocked(now)
			if len(t.records) >= acpPreDispatchMaxRecords {
				return
			}
		}
		record = &acpPreDispatchRecord{created: task.CreationTimestamp.Time}
		if added, ok := t.added[task.UID]; ok {
			delete(t.added, task.UID)
			if !added.After(now) {
				record.marks[acpPreDispatchInformerAdded] = added
			}
		}
		t.records[task.UID] = record
	}
	if replace || record.marks[milestone].IsZero() {
		record.marks[milestone] = now
	}
	record.touched = now
}

func (t *acpPreDispatchTimings) pruneLocked(now time.Time) {
	for uid, record := range t.records {
		if now.Sub(record.touched) >= acpPreDispatchRetention {
			delete(t.records, uid)
		}
	}
}

func (t *acpPreDispatchTimings) finish(task *corev1alpha1.Task) *acpPreDispatchRecord {
	if t == nil || task == nil || task.UID == "" {
		return nil
	}
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.added, task.UID)
	record := t.records[task.UID]
	if record == nil {
		return nil
	}
	delete(t.records, task.UID)
	if record.marks[acpPreDispatchStarted].IsZero() {
		record.marks[acpPreDispatchStarted] = now
	}
	return record
}

func (t *acpPreDispatchTimings) report(ctx context.Context, task *corev1alpha1.Task) {
	record := t.finish(task)
	if record == nil {
		return
	}
	logger := logf.FromContext(ctx)
	if record.marks[acpPreDispatchQueued].IsZero() {
		logger = logger.V(1)
	}
	values := append([]any{acpCancelLogKeyNamespace, task.Namespace, acpCancelLogKeyTask, task.Name}, record.keysAndValues()...)
	logger.Info(acpPreDispatchTimingsLogMessage, values...)
}

func (r *acpPreDispatchRecord) keysAndValues() []any {
	if r == nil {
		return nil
	}
	values := make([]any, 0, 2+2*int(acpPreDispatchMilestoneCount))
	started := r.marks[acpPreDispatchStarted]
	if !r.created.IsZero() && !started.Before(r.created) {
		values = append(values, "sinceTaskCreatedMs", acpPreDispatchMillis(started.Sub(r.created)))
	}
	previous := r.created
	for milestone, at := range r.marks {
		if at.IsZero() {
			continue
		}
		if !previous.IsZero() && !at.Before(previous) {
			values = append(values, acpPreDispatchMilestoneNames[milestone]+"Ms", acpPreDispatchMillis(at.Sub(previous)))
		}
		previous = at
	}
	return values
}

const acpPreDispatchMaxReportedMillis = int64(1) << 53

func acpPreDispatchMillis(duration time.Duration) int64 {
	return min(duration.Milliseconds(), acpPreDispatchMaxReportedMillis)
}

type acpDispatchScanTimer struct {
	now      func() time.Time
	started  time.Time
	last     time.Time
	values   []any
	queued   int
	reserved int
	retired  bool
}

const (
	acpSessionPlanTimingsLogMessage  = "ACP session plan timings"
	acpReservationTimingsLogMessage  = "ACP reservation timings"
	acpDispatchScanTimingsLogMessage = "ACP dispatch scan timings"
	acpDispatchScanSlowThreshold     = 500 * time.Millisecond
)

func newACPDispatchScanTimer(now func() time.Time, retire bool) *acpDispatchScanTimer {
	started := now()
	return &acpDispatchScanTimer{now: now, started: started, last: started, retired: retire}
}

func (s *acpDispatchScanTimer) mark(phase string) {
	if s == nil {
		return
	}
	now := s.now()
	s.values = append(s.values, phase+"Ms", acpPreDispatchMillis(now.Sub(s.last)))
	s.last = now
}

func (s *acpDispatchScanTimer) log(ctx context.Context) {
	if s == nil {
		return
	}
	total := s.now().Sub(s.started)
	if s.reserved == 0 && total < acpDispatchScanSlowThreshold {
		return
	}
	values := append([]any{
		"totalMs", acpPreDispatchMillis(total), "queued", s.queued, "reserved", s.reserved, "poolRetirement", s.retired,
	}, s.values...)
	logf.FromContext(ctx).V(1).Info(acpDispatchScanTimingsLogMessage, values...)
}

type acpStepsKey struct{}

func withACPSteps(ctx context.Context, steps *acpDispatchScanTimer) context.Context {
	return context.WithValue(ctx, acpStepsKey{}, steps)
}

func markACPStep(ctx context.Context, step string) {
	if steps, ok := ctx.Value(acpStepsKey{}).(*acpDispatchScanTimer); ok {
		steps.mark(step)
	}
}
