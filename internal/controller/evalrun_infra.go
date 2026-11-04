package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/evals"
	executionevents "github.com/orka-agents/orka/internal/events"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
)

// evalProviderUpstreamErrorCode is the failure code the ACP supervisor gives
// a prompt whose final provider inference failed upstream, or whose provider
// stream did not drain (workers/acp/supervisor).
const evalProviderUpstreamErrorCode = "provider_upstream_error"

// evalTrialJournalPages bounds the journal pages read for one trial.
const evalTrialJournalPages = 20

var evalProviderUpstreamStatusPattern = regexp.MustCompile(`HTTP (\d{3})`)

// evalTrialSignals is what a trial's execution journal shows. Transcript
// content is never kept, only these facts.
//
// ModelOutput is proven by usage with positive output tokens, a tool call, or
// a model message other than the placeholder a runtime substitutes for a
// completion without text (see evalModelMessageIsOutput). Context-window
// telemetry, zero-token usage and ModelRequestStarted only prove the runtime
// accepted the prompt, so they do not count.
type evalTrialSignals struct {
	ModelOutput bool
	// ProviderFailure is set when a prompt failed because the provider proxy
	// could not serve it: a 5xx or 429, or a stream that never drained.
	ProviderFailure bool
	// RequestRejected is set when the provider rejected the request for its
	// content, such as an oversized prompt: the subject's fault.
	RequestRejected bool
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
	err := forEachEvalTrialEvent(ctx, events, filter, executionevents.ExecutionEventTypeModelRequestFailed, func(event store.ExecutionEvent) {
		switch {
		case evalProviderProxyFailure(event.Content):
			signals.ProviderFailure = true
		case evalProviderRequestRejected(event.Content):
			signals.RequestRejected = true
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
		if usage.OutputTokens != nil && *usage.OutputTokens > 0 {
			signals.ModelOutput = true
		}
	})
	if err != nil {
		return signals, err
	}
	// The journal maps both pending and in-progress updates of one tool call
	// to ToolCallStarted, so calls are counted by ID.
	toolCalls := map[string]struct{}{}
	err = forEachEvalTrialEvent(ctx, events, filter, executionevents.ExecutionEventTypeToolCallStarted, func(event store.ExecutionEvent) {
		signals.ModelOutput = true
		if id := strings.TrimSpace(event.ToolCallID); id != "" {
			if _, seen := toolCalls[id]; seen {
				return
			}
			toolCalls[id] = struct{}{}
		}
		signals.ToolCalls++
	})
	if err != nil {
		return signals, err
	}
	if !signals.ModelOutput {
		err = forEachEvalTrialEvent(ctx, events, filter, executionevents.ExecutionEventTypeModelMessage, func(event store.ExecutionEvent) {
			if evalModelMessageIsOutput(event) {
				signals.ModelOutput = true
			}
		})
	}
	return signals, err
}

// evalModelMessageIsOutput reports a journaled model message that the model
// wrote. The dispatcher journals a completed prompt's result as its
// transcript when nothing streamed, so a completion without text leaves a
// message holding only the runtime's placeholder.
func evalModelMessageIsOutput(event store.ExecutionEvent) bool {
	return !evalResultIsPlaceholder(event.ContentText)
}

// evalResultIsPlaceholder reports result text a runtime substituted for a
// completion without model text.
func evalResultIsPlaceholder(text string) bool {
	return strings.TrimSpace(text) == harnessv2.CompletedWithoutTextResult
}

// evalTrialResult is a trial's stored result, read once for classification
// and grading.
type evalTrialResult struct {
	Raw   []byte
	Found bool
}

// modelText reports a stored result the model wrote, not an empty result or
// the placeholder for a completion without text.
func (r evalTrialResult) modelText() bool {
	text := string(r.Raw)
	return r.Found && strings.TrimSpace(text) != "" && !evalResultIsPlaceholder(text)
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
// provider the proxy could not serve: a 5xx, a 429 or a 402 for exhausted
// quota. Any other 4xx is not the provider's capacity failing, so it stays a
// trial outcome.
func evalProviderProxyFailure(content json.RawMessage) bool {
	status, ok := evalProviderUpstreamStatus(content)
	if !ok {
		return false
	}
	// A stream drain timeout carries no status.
	return status == 0 || status >= 500 || status == http.StatusTooManyRequests || status == http.StatusPaymentRequired
}

// evalProviderRequestRejected reports a ModelRequestFailed journal entry for
// a request the provider rejected for its content, such as an oversized or
// malformed prompt: the subject's fault.
func evalProviderRequestRejected(content json.RawMessage) bool {
	status, ok := evalProviderUpstreamStatus(content)
	if !ok {
		return false
	}
	switch status {
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
		return true
	}
	return false
}

// evalProviderUpstreamStatus reads a provider_upstream_error failure's HTTP
// status, zero when the message carries none.
func evalProviderUpstreamStatus(content json.RawMessage) (int, bool) {
	var failure struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(content, &failure) != nil || failure.Code != evalProviderUpstreamErrorCode {
		return 0, false
	}
	match := evalProviderUpstreamStatusPattern.FindStringSubmatch(failure.Message)
	if match == nil {
		return 0, true
	}
	status, err := strconv.Atoi(match[1])
	if err != nil {
		return 0, true
	}
	return status, true
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
//     with a 5xx, 429 or 402 status, or a provider stream that never drained.
//   - A provider_upstream_error with a 400, 413 or 422 status rejected the
//     request itself, so the trial is graded as a failure.
//   - TimeoutBeforeInference: the trial timed out (TaskTimeout or the trial
//     deadline) before any model output.
//   - NoModelOutput: the Task failed or was cancelled before any model
//     output, for example a workspace, runtime profile or credential failure,
//     or it succeeded with a stored result that holds no model text.
//
// A Task that succeeded with a stored result the model wrote, or with model
// output but no stored result, or failed after producing model output, is
// graded.
func classifyEvalTrialInfra(task *corev1alpha1.Task, signals evalTrialSignals, result evalTrialResult, deadlineExceeded bool) evals.InfraClass {
	execution := task.Status.Execution
	if execution != nil && execution.Outcome == corev1alpha1.TaskExecutionOutcomeOutcomeUnknown {
		if execution.Reason == corev1alpha1.TaskExecutionReasonRuntimeLost {
			return evals.InfraRuntimeLost
		}
		return evals.InfraOutcomeUnknown
	}
	if task.Status.Phase == corev1alpha1.TaskPhaseSucceeded && !deadlineExceeded {
		// A success is graded only with evidence the model answered. A stored
		// result decides: an empty one or the runtime's placeholder for a
		// completion without text is no answer, whatever else the journal
		// shows. Without a stored result, journal output is graded as a
		// review with a missing result.
		if result.Found {
			if result.modelText() {
				return ""
			}
			return evals.InfraNoModelOutput
		}
		if signals.ModelOutput {
			return ""
		}
		return evals.InfraNoModelOutput
	}
	if signals.ProviderFailure {
		return evals.InfraProviderProxyError
	}
	timedOut := deadlineExceeded || (execution != nil && execution.Reason == acpTaskTimeoutReason)
	switch {
	case signals.ModelOutput, signals.RequestRejected:
		return ""
	case timedOut:
		return evals.InfraTimeoutBeforeInference
	default:
		return evals.InfraNoModelOutput
	}
}
