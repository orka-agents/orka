/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"time"

	logf "sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

// acpTurnStep is one completed step of an ACP Task execution.
type acpTurnStep struct {
	name     string
	duration time.Duration
}

// acpTurnTimings records how long each step of one ACP Task execution takes,
// from the reserved dispatch through prompt settlement, workspace validation,
// publication, Session finalization and runtime retirement. It holds step names
// and durations only, logged once when the execution returns.
type acpTurnTimings struct {
	now     func() time.Time
	created time.Time
	started time.Time
	last    time.Time
	steps   []acpTurnStep
	// prompted is set once the prompt is accepted; executions that stop
	// earlier, such as capacity requeues, log at debug verbosity.
	prompted bool
}

func newACPTurnTimings(task *corev1alpha1.Task, now func() time.Time) *acpTurnTimings {
	started := now()
	return &acpTurnTimings{now: now, created: task.CreationTimestamp.Time, started: started, last: started}
}

// mark records the step that just finished as the time since the previous
// mark. A nil receiver records nothing.
func (t *acpTurnTimings) mark(step string) {
	if t == nil {
		return
	}
	now := t.now()
	t.steps = append(t.steps, acpTurnStep{name: step, duration: now.Sub(t.last)})
	t.last = now
	if step == acpTurnStepPromptAccepted {
		t.prompted = true
	}
}

// markPromptAccepted records the promptAccepted step the first time the
// runtime is seen to accept the prompt, before any fallible persistence of that
// acceptance, so a later failure still logs the turn at info verbosity. Later
// calls record nothing. A nil receiver records nothing.
func (t *acpTurnTimings) markPromptAccepted() {
	if t == nil || t.prompted {
		return
	}
	t.mark(acpTurnStepPromptAccepted)
}

// markFailedPromptStream records the promptStream step for a prompt stream
// that ended in an error after the runtime accepted the prompt, as the
// successful path does, so a long stream that later fails still reports its
// duration. An unaccepted prompt, such as a rejected admission that is retried,
// records nothing. A nil receiver records nothing.
func (t *acpTurnTimings) markFailedPromptStream(accepted bool) {
	if t == nil || !accepted {
		return
	}
	t.markPromptAccepted()
	t.mark(acpTurnStepPromptStream)
}

const (
	acpTurnStepRuntimeCapabilities  = "runtimeCapabilities"
	acpTurnStepSessionPlan          = "sessionPlan"
	acpTurnStepWorkspacePrepare     = "workspacePrepare"
	acpTurnStepSessionCreate        = "sessionCreate"
	acpTurnStepPromptAccepted       = "promptAccepted"
	acpTurnStepPromptStream         = "promptStream"
	acpTurnStepTerminalPersistence  = "terminalPersistence"
	acpTurnStepWorkspaceDelta       = "workspaceDelta"
	acpTurnStepExecutionSettled     = "executionSettled"
	acpTurnStepPublication          = "publication"
	acpTurnStepPublicationFinalize  = "publicationFinalization"
	acpTurnStepRuntimeFinalize      = "runtimeSessionFinalization"
	acpTurnStepSessionResult        = "sessionResult"
	acpTurnStepRuntimeRetirement    = "runtimeSessionRetirement"
	acpTurnStepTaskCompletion       = "taskCompletion"
	acpTurnTimingsLogMessage        = "ACP turn timings"
	acpTurnTimingsDebugVerbosity    = 1
	acpTurnTimingsMaxReportedMillis = int64(1) << 53
)

// keysAndValues returns the timings as logr key/value pairs: time since the
// Task was created, the execution total up to end, then each step in order.
func (t *acpTurnTimings) keysAndValues(end time.Time) []any {
	if t == nil {
		return nil
	}
	values := make([]any, 0, 6+2*len(t.steps))
	if !t.created.IsZero() && !t.started.Before(t.created) {
		values = append(values, "sinceTaskCreatedMs", boundedMillis(t.started.Sub(t.created)))
	}
	values = append(values, "totalMs", boundedMillis(end.Sub(t.started)), "promptAccepted", t.prompted)
	for _, step := range t.steps {
		values = append(values, step.name+"Ms", boundedMillis(step.duration))
	}
	return values
}

func boundedMillis(duration time.Duration) int64 {
	return min(duration.Milliseconds(), acpTurnTimingsMaxReportedMillis)
}

// log writes the timings once. Executions that never reached prompt
// acceptance (requeues, admission waits) log at debug verbosity so a waiting
// queue does not flood the log.
func (t *acpTurnTimings) log(ctx context.Context, task *corev1alpha1.Task, err error) {
	if t == nil {
		return
	}
	logger := logf.FromContext(ctx)
	if !t.prompted {
		logger = logger.V(acpTurnTimingsDebugVerbosity)
	}
	values := append([]any{"namespace", task.Namespace, "task", task.Name, "failed", err != nil}, t.keysAndValues(t.now())...)
	logger.Info(acpTurnTimingsLogMessage, values...)
}
