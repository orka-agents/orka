/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

type acpTurnTimingsClock struct{ now time.Time }

func (c *acpTurnTimingsClock) Now() time.Time { return c.now }

type capturedTurnTimingsLine struct {
	level  int
	values map[string]any
}

// captureTurnTimingsLogger returns a context whose logger records each line's
// verbosity and key/value pairs.
func captureTurnTimingsLogger(t *testing.T) (context.Context, *[]capturedTurnTimingsLine) {
	t.Helper()
	lines := []capturedTurnTimingsLine{}
	levelKey := "level"
	logger := funcr.NewJSON(func(obj string) {
		values := map[string]any{}
		if err := json.Unmarshal([]byte(obj), &values); err != nil {
			t.Fatalf("decode log line %q: %v", obj, err)
		}
		level := 0
		if raw, ok := values[levelKey].(float64); ok {
			level = int(raw)
		}
		lines = append(lines, capturedTurnTimingsLine{level: level, values: values})
	}, funcr.Options{Verbosity: 1, LogInfoLevel: &levelKey})
	return logf.IntoContext(context.Background(), logger), &lines
}

func TestACPTurnTimingsLogStepsInOrderWithTaskAge(t *testing.T) {
	created := time.Unix(1000, 0)
	clock := &acpTurnTimingsClock{now: created.Add(4 * time.Second)}
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "turn", CreationTimestamp: metav1.NewTime(created)}}
	timings := newACPTurnTimings(task, clock.Now)
	for _, step := range []struct {
		name  string
		after time.Duration
	}{
		{acpTurnStepWorkspacePrepare, 3500 * time.Millisecond},
		{acpTurnStepSessionCreate, 19 * time.Second},
		{acpTurnStepPromptAccepted, 200 * time.Millisecond},
		{acpTurnStepPromptStream, 1400 * time.Millisecond},
		{acpTurnStepRuntimeRetirement, 2 * time.Second},
	} {
		clock.now = clock.now.Add(step.after)
		timings.mark(step.name)
	}
	clock.now = clock.now.Add(100 * time.Millisecond)

	ctx, lines := captureTurnTimingsLogger(t)
	timings.log(ctx, task, nil)
	if len(*lines) != 1 {
		t.Fatalf("lines = %v", *lines)
	}
	line := (*lines)[0]
	if line.level != 0 || line.values["msg"] != acpTurnTimingsLogMessage {
		t.Fatalf("line = %+v, want one info line", line)
	}
	want := map[string]float64{
		"sinceTaskCreatedMs": 4000, "totalMs": 26200,
		"workspacePrepareMs": 3500, "sessionCreateMs": 19000, "promptAcceptedMs": 200,
		"promptStreamMs": 1400, "runtimeSessionRetirementMs": 2000,
	}
	for key, value := range want {
		if line.values[key] != value {
			t.Fatalf("%s = %v, want %v (line %v)", key, line.values[key], value, line.values)
		}
	}
	if line.values["promptAccepted"] != true || line.values["returnedError"] != false || line.values["task"] != "turn" {
		t.Fatalf("line = %v", line.values)
	}
}

// An execution that stops before its prompt is accepted, such as a capacity
// requeue, logs only at debug verbosity, and reports whether it failed.
func TestACPTurnTimingsLogUnpromptedExecutionsAtDebug(t *testing.T) {
	clock := &acpTurnTimingsClock{now: time.Unix(1000, 0)}
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "queued"}}
	timings := newACPTurnTimings(task, clock.Now)
	timings.mark(acpTurnStepDispatchPreflight)

	ctx, lines := captureTurnTimingsLogger(t)
	timings.log(ctx, task, errors.New("requeued"))
	if len(*lines) != 1 || (*lines)[0].level != acpTurnTimingsDebugVerbosity {
		t.Fatalf("lines = %v, want one debug line", *lines)
	}
	values := (*lines)[0].values
	if values["returnedError"] != true || values["promptAccepted"] != false {
		t.Fatalf("line = %v", values)
	}
	if _, ok := values["sinceTaskCreatedMs"]; ok {
		t.Fatalf("line = %v, want no Task age without a creation time", values)
	}
}

// Acceptance is recorded once, when first observed, and an accepted prompt
// whose stream later fails still reports how long it streamed, at info
// verbosity.
func TestACPTurnTimingsRecordAcceptedPromptWhoseStreamFails(t *testing.T) {
	clock := &acpTurnTimingsClock{now: time.Unix(1000, 0)}
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "stream-failed"}}
	timings := newACPTurnTimings(task, clock.Now)
	clock.now = clock.now.Add(time.Second)
	timings.mark(acpTurnStepSessionCreate)
	clock.now = clock.now.Add(300 * time.Millisecond)
	timings.markPromptAccepted()
	// A repeated accepted event, or the failure path, does not re-record it.
	clock.now = clock.now.Add(50 * time.Millisecond)
	timings.markPromptAccepted()
	clock.now = clock.now.Add(30 * time.Second)
	timings.markFailedPromptStream(true)

	ctx, lines := captureTurnTimingsLogger(t)
	timings.log(ctx, task, errors.New("stream transport failed"))
	if len(*lines) != 1 || (*lines)[0].level != 0 {
		t.Fatalf("lines = %v, want one info line", *lines)
	}
	values := (*lines)[0].values
	if values["promptAccepted"] != true || values["returnedError"] != true ||
		values["promptAcceptedMs"] != float64(300) || values["promptStreamMs"] != float64(30050) {
		t.Fatalf("line = %v", values)
	}
	if len(timings.steps) != 3 {
		t.Fatalf("steps = %v, want sessionCreate, promptAccepted, promptStream", timings.steps)
	}
}

// A failed stream whose summary shows an acceptance the event handler never
// recorded still counts as prompted.
func TestACPTurnTimingsFailedStreamAcceptedOnlyBySummary(t *testing.T) {
	clock := &acpTurnTimingsClock{now: time.Unix(1000, 0)}
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "summary-accepted"}}
	timings := newACPTurnTimings(task, clock.Now)
	clock.now = clock.now.Add(2 * time.Second)
	timings.markFailedPromptStream(true)
	if !timings.prompted || len(timings.steps) != 2 ||
		timings.steps[0].name != acpTurnStepPromptAccepted || timings.steps[1].name != acpTurnStepPromptStream {
		t.Fatalf("steps = %v, prompted = %v", timings.steps, timings.prompted)
	}
}

// A rejected, unaccepted prompt records no stream step and stays at debug.
func TestACPTurnTimingsFailedUnacceptedStreamRecordsNothing(t *testing.T) {
	clock := &acpTurnTimingsClock{now: time.Unix(1000, 0)}
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "rejected"}}
	timings := newACPTurnTimings(task, clock.Now)
	timings.markFailedPromptStream(false)
	if timings.prompted || len(timings.steps) != 0 {
		t.Fatalf("steps = %v, prompted = %v", timings.steps, timings.prompted)
	}
}

func TestACPTurnTimingsNilReceiverIsSafe(t *testing.T) {
	var timings *acpTurnTimings
	timings.mark(acpTurnStepSessionCreate)
	timings.markPromptAccepted()
	timings.markFailedPromptStream(true)
	timings.log(context.Background(), &corev1alpha1.Task{}, nil)
	if timings.keysAndValues(time.Now()) != nil {
		t.Fatal("nil timings produced values")
	}
}
