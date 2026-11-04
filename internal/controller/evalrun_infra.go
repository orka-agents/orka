package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/evals"
	executionevents "github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/store"
)

// evalProviderUpstreamErrorCode is the failure code the ACP supervisor gives
// a prompt whose final provider inference failed upstream, or whose provider
// stream did not drain (workers/acp/supervisor).
const evalProviderUpstreamErrorCode = "provider_upstream_error"

// evalTrialJournalPages bounds the journal pages read for one trial.
const evalTrialJournalPages = 20

var evalProviderUpstreamStatusPattern = regexp.MustCompile(`HTTP (\d{3})`)

// evalModelOutputEventTypes prove the provider produced output: a message
// chunk, token usage, context usage or a tool call. ModelRequestStarted only
// proves the runtime accepted the prompt, so it does not count.
var evalModelOutputEventTypes = []string{
	executionevents.ExecutionEventTypeModelMessage,
	executionevents.ExecutionEventTypeModelUsageUpdated,
	executionevents.ExecutionEventTypeModelContextUpdated,
	executionevents.ExecutionEventTypeToolCallStarted,
}

// evalTrialSignals is what a trial's execution journal shows. Transcript
// content is never kept, only these facts.
type evalTrialSignals struct {
	ModelOutput bool
	// ProviderFailure is set when a prompt failed because the provider proxy
	// could not serve it: a 5xx or 429, or a stream that never drained.
	ProviderFailure bool
	InputTokens     *int64
	OutputTokens    *int64
	ToolCalls       int64
}

func readEvalTrialSignals(ctx context.Context, events store.ExecutionEventStore, task *corev1alpha1.Task) (evalTrialSignals, error) {
	var signals evalTrialSignals
	if events == nil {
		return signals, fmt.Errorf("execution event store is required to classify eval trials")
	}
	filter := store.ExecutionEventFilter{
		Namespace:  task.Namespace,
		StreamType: store.ExecutionEventStreamTypeTask,
		StreamID:   task.Name,
	}
	outputFilter := filter
	outputFilter.EventTypes = evalModelOutputEventTypes
	outputFilter.Limit = 1
	output, err := events.ListExecutionEvents(ctx, outputFilter)
	if err != nil {
		return signals, err
	}
	signals.ModelOutput = len(output) > 0

	err = forEachEvalTrialEvent(ctx, events, filter, executionevents.ExecutionEventTypeModelRequestFailed, func(event store.ExecutionEvent) {
		if evalProviderProxyFailure(event.Content) {
			signals.ProviderFailure = true
		}
	})
	if err != nil {
		return signals, err
	}
	err = forEachEvalTrialEvent(ctx, events, filter, executionevents.ExecutionEventTypeModelUsageUpdated, func(event store.ExecutionEvent) {
		// Counts are cumulative for the trial's single prompt, so the
		// largest observation is the total.
		var usage struct {
			InputTokens  *int64 `json:"inputTokens"`
			OutputTokens *int64 `json:"outputTokens"`
		}
		if json.Unmarshal(event.Content, &usage) != nil {
			return
		}
		signals.InputTokens = maxInt64Pointer(signals.InputTokens, usage.InputTokens)
		signals.OutputTokens = maxInt64Pointer(signals.OutputTokens, usage.OutputTokens)
	})
	if err != nil {
		return signals, err
	}
	err = forEachEvalTrialEvent(ctx, events, filter, executionevents.ExecutionEventTypeToolCallStarted, func(store.ExecutionEvent) {
		signals.ToolCalls++
	})
	return signals, err
}

// forEachEvalTrialEvent visits one event type of a trial journal, reading at
// most evalTrialJournalPages pages.
func forEachEvalTrialEvent(ctx context.Context, events store.ExecutionEventStore, filter store.ExecutionEventFilter, eventType string, visit func(store.ExecutionEvent)) error {
	filter.EventTypes = []string{eventType}
	filter.Limit = store.MaxExecutionEventLimit
	for range evalTrialJournalPages {
		page, err := events.ListExecutionEvents(ctx, filter)
		if err != nil {
			return err
		}
		for _, event := range page {
			visit(event)
			filter.AfterSeq = event.Seq
		}
		if len(page) < filter.Limit {
			return nil
		}
	}
	return nil
}

func maxInt64Pointer(current, candidate *int64) *int64 {
	if candidate == nil || *candidate < 0 {
		return current
	}
	if current == nil || *candidate > *current {
		value := *candidate
		return &value
	}
	return current
}

// evalProviderProxyFailure reports a ModelRequestFailed journal entry for a
// provider the proxy could not serve. A 4xx other than 429 is the request's
// fault, such as an oversized prompt, so it stays a trial failure.
func evalProviderProxyFailure(content json.RawMessage) bool {
	var failure struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(content, &failure) != nil || failure.Code != evalProviderUpstreamErrorCode {
		return false
	}
	match := evalProviderUpstreamStatusPattern.FindStringSubmatch(failure.Message)
	if match == nil {
		// A stream drain timeout carries no status.
		return true
	}
	status, err := strconv.Atoi(match[1])
	if err != nil {
		return true
	}
	return status >= 500 || status == 429
}

// classifyEvalTrialInfra decides whether a terminal trial produced no
// gradable outcome for reasons outside the subject. The classes follow the
// execution states and reasons the Task path actually records:
//
//   - RuntimeLost: execution outcome OutcomeUnknown with reason RuntimeLost
//     (an accepted prompt lost with its runtime).
//   - OutcomeUnknown: any other OutcomeUnknown outcome, such as an unproven
//     settlement or a failed execution-event persistence.
//   - ProviderProxyError: the journal holds a provider_upstream_error failure
//     with a 5xx or 429 status, or a provider stream that never drained.
//   - TimeoutBeforeInference: the trial timed out (TaskTimeout or the trial
//     deadline) before any model output.
//   - NoModelOutput: the Task failed or was cancelled before any model
//     output, for example a workspace, runtime profile or credential failure.
//
// A Task that succeeded, or failed after producing model output, is graded.
func classifyEvalTrialInfra(task *corev1alpha1.Task, signals evalTrialSignals, deadlineExceeded bool) evals.InfraClass {
	execution := task.Status.Execution
	if execution != nil && execution.Outcome == corev1alpha1.TaskExecutionOutcomeOutcomeUnknown {
		if execution.Reason == corev1alpha1.TaskExecutionReasonRuntimeLost {
			return evals.InfraRuntimeLost
		}
		return evals.InfraOutcomeUnknown
	}
	if task.Status.Phase == corev1alpha1.TaskPhaseSucceeded && !deadlineExceeded {
		return ""
	}
	if signals.ProviderFailure {
		return evals.InfraProviderProxyError
	}
	timedOut := deadlineExceeded || (execution != nil && execution.Reason == acpTaskTimeoutReason)
	switch {
	case signals.ModelOutput:
		return ""
	case timedOut:
		return evals.InfraTimeoutBeforeInference
	default:
		return evals.InfraNoModelOutput
	}
}
