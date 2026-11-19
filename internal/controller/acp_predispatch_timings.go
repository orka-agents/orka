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

// acpPreDispatchMilestone is one step an agent Task passes between its
// creation and the start of its ACP turn. The Task controller and the ACP
// dispatcher record them in this process; the dispatcher logs them once when
// the turn starts, so the wait before dispatch is attributed per step.
type acpPreDispatchMilestone int

const (
	// acpPreDispatchInformerAdded is the Task's Add event reaching this
	// controller's informer, before it waits in the reconcile queue. Its step
	// from creation includes up to one second of rounding: creation times
	// have whole-second precision.
	acpPreDispatchInformerAdded acpPreDispatchMilestone = iota
	// acpPreDispatchObserved is the first reconcile of an agent Task that has
	// no execution yet.
	acpPreDispatchObserved
	// acpPreDispatchStatusInitialized is the Pending status write.
	acpPreDispatchStatusInitialized
	// acpPreDispatchExecutionBound is the write-once execution binding.
	acpPreDispatchExecutionBound
	// acpPreDispatchSpeculativeStart is the recorded speculative start of
	// the Task's execution workspace runtime (ADR 0057).
	acpPreDispatchSpeculativeStart
	// acpPreDispatchWorkspaceAttachRequested is the exclusive attachment of
	// the Task's execution workspace.
	acpPreDispatchWorkspaceAttachRequested
	// acpPreDispatchWorkspaceReady is the first pass that finds the
	// attachment admitted and acknowledged.
	acpPreDispatchWorkspaceReady
	// acpPreDispatchRuntimePoolReady is the first queue pass whose RuntimePool
	// is ensured and, for an execution workspace, whose post-attach handshake
	// holds, including the provider observing the current workspace
	// generation.
	acpPreDispatchRuntimePoolReady
	// acpPreDispatchAttemptPersisted is the durable PromptAttempt write.
	acpPreDispatchAttemptPersisted
	// acpPreDispatchQueued is the Queued status write.
	acpPreDispatchQueued
	// acpPreDispatchScanned is the first dispatcher scan that offers the
	// queued Task for admission.
	acpPreDispatchScanned
	// acpPreDispatchReserved is the Reserved status write.
	acpPreDispatchReserved
	// acpPreDispatchStarted is the start of the reserved execution.
	acpPreDispatchStarted
	acpPreDispatchMilestoneCount
)

var acpPreDispatchMilestoneNames = [acpPreDispatchMilestoneCount]string{
	"informerAdded",
	"observed",
	"statusInitialized",
	"executionBound",
	"speculativeStart",
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
	// acpPreDispatchMaxRecords bounds the Tasks tracked at once; Tasks that
	// never dispatch are pruned after acpPreDispatchRetention.
	acpPreDispatchMaxRecords = 4096
	acpPreDispatchRetention  = 30 * time.Minute
)

type acpPreDispatchRecord struct {
	created time.Time
	touched time.Time
	marks   [acpPreDispatchMilestoneCount]time.Time
}

// acpPreDispatchTimings holds, per Task UID, when each pre-dispatch milestone
// was first reached in this process. It keeps only times, never Task content.
type acpPreDispatchTimings struct {
	now     func() time.Time
	mu      sync.Mutex
	records map[types.UID]*acpPreDispatchRecord
	// added holds informer Add times that arrived before the Task's first
	// reconcile created its record; the record takes it when created.
	added map[types.UID]time.Time
}

func newACPPreDispatchTimings(now func() time.Time) *acpPreDispatchTimings {
	return &acpPreDispatchTimings{
		now: now, records: make(map[types.UID]*acpPreDispatchRecord), added: make(map[types.UID]time.Time),
	}
}

// acpPreDispatch is shared by the Task controller and the ACP dispatcher,
// which run in the same process.
var acpPreDispatch = newACPPreDispatchTimings(time.Now)

// mark records the first time an agent Task reaches milestone. Later calls
// for the same milestone keep the first time.
func (t *acpPreDispatchTimings) mark(task *corev1alpha1.Task, milestone acpPreDispatchMilestone) {
	t.record(task, milestone, false)
}

// markStarted records the start of each reserved execution attempt,
// replacing an earlier attempt's start: an attempt that requeues before its
// turn begins leaves the record for the attempt that does.
func (t *acpPreDispatchTimings) markStarted(task *corev1alpha1.Task) {
	t.record(task, acpPreDispatchStarted, true)
}

// markQueueWriteIssued records the queued milestone immediately before the
// Queued status write is issued. The write is watch-visible before it
// returns, so the dispatcher can scan and even start the Task first; marking
// beforehand keeps queued ahead of every dispatcher milestone and leaves no
// late mark to recreate a record a start already reported. Each call
// replaces the earlier time: a write that fails cleanly leaves the Task
// unqueued, and the retried queue pass records its own write. A write that
// fails but committed queued the Task, so its mark is correct; a Task never
// retried leaves a record that retention prunes.
func (t *acpPreDispatchTimings) markQueueWriteIssued(task *corev1alpha1.Task) {
	t.record(task, acpPreDispatchQueued, true)
}

// acpPreDispatchCreatesRecord reports whether a milestone may start a Task's
// record. Only the passes before a Task has any execution do, so a mark from a
// later stage, which can run after the Task's start already reported and
// removed its record, never recreates one that nothing would report.
func acpPreDispatchCreatesRecord(milestone acpPreDispatchMilestone) bool {
	return milestone == acpPreDispatchObserved || milestone == acpPreDispatchStatusInitialized
}

// acpPreDispatchTaskAdded records the informer Add of a new agent Task with
// no execution. Tasks the informer lists at startup are not new.
func acpPreDispatchTaskAdded(object any, isInInitialList bool) {
	task, ok := object.(*corev1alpha1.Task)
	if !ok || isInInitialList || task.Status.Execution != nil || task.Spec.Schedule != "" ||
		!task.DeletionTimestamp.IsZero() || task.Status.Phase.IsTerminal() {
		return
	}
	acpPreDispatch.informerAdded(task)
}

// informerAdded stamps the Add at the time it is delivered. The controller's
// own handler enqueues the Task independently, so the first reconcile can run
// before or after this callback. An Add that arrives first is held until the
// first reconcile creates the record; one that arrives after the reconcile
// was observed, or after the record finished, is later than what it would
// measure and is dropped. It never creates a record itself.
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

// finish removes the Task's record and returns it, recording the started
// milestone if no attempt did. A Task with no record, such as one queued before this process
// started, returns nil.
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

// report logs the Task's milestones once, when its reserved execution starts.
// A Task this process did not see queued, such as one requeued after an
// earlier start, logs at debug verbosity.
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

// keysAndValues returns the time from Task creation to the start, then each
// recorded milestone as the time since the previous one (the first since
// creation). Creation time has one-second precision; the steps do not.
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

// acpDispatchScanTimer times the phases of one dispatcher scan. Scans that
// reserve a Task or run long are logged at debug verbosity.
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

// withACPSteps carries a step timer through calls that time their own steps,
// such as Session planning, without widening their signatures.
func withACPSteps(ctx context.Context, steps *acpDispatchScanTimer) context.Context {
	return context.WithValue(ctx, acpStepsKey{}, steps)
}

// markACPStep marks a step on the context's timer, if it carries one.
func markACPStep(ctx context.Context, step string) {
	if steps, ok := ctx.Value(acpStepsKey{}).(*acpDispatchScanTimer); ok {
		steps.mark(step)
	}
}
