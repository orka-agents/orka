package controller

import (
	"encoding/json"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/evals"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

func TestClassifyEvalTrialInfra(t *testing.T) {
	task := func(phase corev1alpha1.TaskPhase, outcome corev1alpha1.TaskExecutionOutcome, reason corev1alpha1.TaskExecutionReason) *corev1alpha1.Task {
		t := &corev1alpha1.Task{Status: corev1alpha1.TaskStatus{Phase: phase}}
		if outcome != "" || reason != "" {
			t.Status.Execution = &corev1alpha1.TaskExecutionStatus{Outcome: outcome, Reason: reason}
		}
		return t
	}
	for name, tc := range map[string]struct {
		task     *corev1alpha1.Task
		signals  evalTrialSignals
		result   evalTrialResult
		deadline bool
		want     evals.InfraClass
	}{
		"succeeded":                  {task: task(corev1alpha1.TaskPhaseSucceeded, "", ""), signals: evalTrialSignals{ModelOutput: true}},
		"succeeded without output":   {task: task(corev1alpha1.TaskPhaseSucceeded, "", ""), want: evals.InfraNoModelOutput},
		"succeeded with result only": {task: task(corev1alpha1.TaskPhaseSucceeded, "", ""), result: evalTrialResult{Raw: []byte(`{"verdict":"passed"}`), Found: true}},
		"succeeded with placeholder result": {
			task: task(corev1alpha1.TaskPhaseSucceeded, "", ""), signals: evalTrialSignals{ModelOutput: true},
			result: evalTrialResult{Raw: []byte(harnessv2.CompletedWithoutTextResult + "\n"), Found: true}, want: evals.InfraNoModelOutput,
		},
		"succeeded with empty result": {
			task: task(corev1alpha1.TaskPhaseSucceeded, "", ""), signals: evalTrialSignals{ModelOutput: true},
			result: evalTrialResult{Raw: []byte("  "), Found: true}, want: evals.InfraNoModelOutput,
		},
		"succeeded despite 5xx":     {task: task(corev1alpha1.TaskPhaseSucceeded, "", ""), signals: evalTrialSignals{ModelOutput: true, ProviderFailure: true}},
		"runtime lost":              {task: task(corev1alpha1.TaskPhaseFailed, corev1alpha1.TaskExecutionOutcomeOutcomeUnknown, corev1alpha1.TaskExecutionReasonRuntimeLost), signals: evalTrialSignals{ModelOutput: true}, want: evals.InfraRuntimeLost},
		"outcome unknown":           {task: task(corev1alpha1.TaskPhaseFailed, corev1alpha1.TaskExecutionOutcomeOutcomeUnknown, "SettlementUnproven"), want: evals.InfraOutcomeUnknown},
		"provider proxy error":      {task: task(corev1alpha1.TaskPhaseFailed, corev1alpha1.TaskExecutionOutcomeFailed, ""), signals: evalTrialSignals{ModelOutput: true, ProviderFailure: true}, want: evals.InfraProviderProxyError},
		"failed before output":      {task: task(corev1alpha1.TaskPhaseFailed, "", ""), want: evals.InfraNoModelOutput},
		"cancelled before output":   {task: task(corev1alpha1.TaskPhaseCancelled, "", ""), want: evals.InfraNoModelOutput},
		"failed after output":       {task: task(corev1alpha1.TaskPhaseFailed, "", ""), signals: evalTrialSignals{ModelOutput: true}},
		"provider rejected request": {task: task(corev1alpha1.TaskPhaseFailed, corev1alpha1.TaskExecutionOutcomeFailed, ""), signals: evalTrialSignals{RequestRejected: true}},
		"task timeout no output":    {task: task(corev1alpha1.TaskPhaseFailed, corev1alpha1.TaskExecutionOutcomeFailed, acpTaskTimeoutReason), want: evals.InfraTimeoutBeforeInference},
		"deadline before output":    {task: task(corev1alpha1.TaskPhaseRunning, "", ""), deadline: true, want: evals.InfraTimeoutBeforeInference},
		"deadline after output":     {task: task(corev1alpha1.TaskPhaseRunning, "", ""), deadline: true, signals: evalTrialSignals{ModelOutput: true}},
		"task timeout after output": {task: task(corev1alpha1.TaskPhaseFailed, corev1alpha1.TaskExecutionOutcomeFailed, acpTaskTimeoutReason), signals: evalTrialSignals{ModelOutput: true}},
	} {
		t.Run(name, func(t *testing.T) {
			if got := classifyEvalTrialInfra(tc.task, tc.signals, tc.result, tc.deadline); got != tc.want {
				t.Fatalf("classifyEvalTrialInfra() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEvalProviderProxyFailure(t *testing.T) {
	for message, want := range map[string]bool{
		"provider upstream returned HTTP 502 for the final inference request": true,
		"provider upstream returned HTTP 429 for the final inference request": true,
		"provider upstream returned HTTP 402 for the final inference request": true,
		"provider upstream returned HTTP 400 for the final inference request": false,
		"a provider inference request was still in flight":                    true,
	} {
		content, _ := json.Marshal(map[string]string{"code": evalProviderUpstreamErrorCode, "message": message})
		if got := evalProviderProxyFailure(content); got != want {
			t.Errorf("%q: got %v, want %v", message, got, want)
		}
	}
	for message, want := range map[string]bool{
		"provider upstream returned HTTP 400 for the final inference request": true,
		"provider upstream returned HTTP 413 for the final inference request": true,
		"provider upstream returned HTTP 402 for the final inference request": false,
		"provider upstream returned HTTP 403 for the final inference request": false,
		"provider upstream returned HTTP 429 for the final inference request": false,
		"provider upstream returned HTTP 502 for the final inference request": false,
		"a provider inference request was still in flight":                    false,
	} {
		content, _ := json.Marshal(map[string]string{"code": evalProviderUpstreamErrorCode, "message": message})
		if got := evalProviderRequestRejected(content); got != want {
			t.Errorf("rejected %q: got %v, want %v", message, got, want)
		}
	}
	other, _ := json.Marshal(map[string]string{"code": "max_turns", "message": "HTTP 503"})
	if evalProviderProxyFailure(other) {
		t.Error("a non-provider failure code counted as a provider proxy failure")
	}
	other, _ = json.Marshal(map[string]string{"code": "max_turns", "message": "HTTP 400"})
	if evalProviderRequestRejected(other) {
		t.Error("a non-provider failure code counted as a rejected request")
	}
}
