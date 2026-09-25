//go:build e2e
// +build e2e

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestArtifactTaskDiagnosticsPreservesFailureEvidence(t *testing.T) {
	t.Parallel()
	const callerSecret = "fixture-caller-secret"
	responses := map[string]string{
		"task": `{"metadata":{"name":"artifact-task","uid":"task-uid","annotations":{"private":"annotation-value"}},
			"spec":{"env":[{"name":"PRIVATE","value":"spec-value"}]},
			"status":{"phase":"Failed","attempts":1,"jobName":"artifact-job",
				"message":"failed to create job: fixture-caller-secret",
				"resultRef":{"output":"result-value"},
				"conditions":[{"type":"Complete","status":"False","reason":"TaskFailed","message":"token=condition-value"}]}}`,
		"jobs": `{"items":[{"metadata":{"name":"artifact-job","uid":"job-uid"},
			"spec":{"template":{"spec":{"private":"job-spec-value"}}},
			"status":{"failed":1,"conditions":[{"type":"Failed","status":"True","reason":"BackoffLimitExceeded"}]}}]}`,
		"pods": `{"items":[{"metadata":{"name":"artifact-pod","uid":"pod-uid"},
			"status":{"phase":"Failed","reason":"UnexpectedAdmissionError",
				"containerStatuses":[{"name":"worker","ready":false,"restartCount":0,
					"image":"private-image-value","state":{"terminated":{"exitCode":1,"reason":"Error","message":"password=container-value"}}}],
				"initContainerStatuses":[{"name":"init","state":{"waiting":{"reason":"ImagePullBackOff"}}}]}}]}`,
	}
	var eventUIDs []string
	got := collectArtifactTaskDiagnostics(func(ctx context.Context, args ...string) ([]byte, error) {
		require.NoError(t, ctx.Err())
		require.Equal(t, []string{"get", args[1], "-n", "fixture-namespace", "-o", "json", "--request-timeout=5s"}, args[:7])
		if args[1] == "events" {
			uid := strings.TrimPrefix(args[7], "--field-selector=involvedObject.uid=")
			eventUIDs = append(eventUIDs, uid)
			return fmt.Appendf(nil, `{"items":[{"involvedObject":{"kind":"Pod","name":"artifact-pod","uid":%q},"type":"Warning","reason":"FailedCreate","message":"token=event-value","count":1}]}`, uid), nil
		}
		if args[1] == "task" {
			require.Equal(t, "artifact-task", args[7])
		} else {
			require.Equal(t, []string{"-l", "orka.ai/task=artifact-task"}, args[7:])
		}
		return []byte(responses[args[1]]), nil
	}, "fixture-namespace", "artifact-task", callerSecret)

	require.JSONEq(t, `{"name":"artifact-task","uid":"task-uid"}`, jsonObjectAt(t, got, "task", "metadata"))
	for _, expected := range []string{"failed to create job:", "TaskFailed", "BackoffLimitExceeded", "UnexpectedAdmissionError", "ImagePullBackOff", "FailedCreate", `"exitCode": 1`, `"attempts": 1`, "[REDACTED]"} {
		require.Contains(t, got, expected)
	}
	for _, secret := range []string{callerSecret, "annotation-value", "spec-value", "result-value", "condition-value", "job-spec-value", "container-value", "event-value", "private-image-value"} {
		require.NotContains(t, got, secret)
	}
	require.Equal(t, []string{"task-uid", "job-uid", "pod-uid"}, eventUIDs)
}

func TestArtifactTaskDiagnosticsOmitUnreadablePayloads(t *testing.T) {
	t.Parallel()
	got := collectArtifactTaskDiagnostics(func(_ context.Context, args ...string) ([]byte, error) {
		if args[1] == "task" {
			return []byte(`{"status":{"message":`), nil
		}
		return []byte("private-command-output"), errors.New("private-command-error")
	}, "fixture-namespace", "artifact-task")
	require.Contains(t, got, `"unavailable"`)
	require.NotContains(t, got, "private-command")
	require.NotContains(t, got, "message")
	var report map[string]any
	require.NoError(t, json.Unmarshal([]byte(got), &report))
	require.Equal(t, []any{"task", "jobs", "pods"}, report["unavailable"])
}

func TestArtifactTaskDiagnosticsBoundsEventsAndMessages(t *testing.T) {
	t.Parallel()
	got := collectArtifactTaskDiagnostics(func(_ context.Context, args ...string) ([]byte, error) {
		switch args[1] {
		case "task":
			return []byte(`{"metadata":{"uid":"task-uid"},"status":{"message":{"spec":"unexpected-object"}}}`), nil
		case "events":
			var events []map[string]any
			for i := range 20 {
				events = append(events, map[string]any{"reason": fmt.Sprintf("Event%d", i), "message": strings.Repeat("m", 1500)})
			}
			return json.Marshal(map[string]any{"items": events})
		default:
			return []byte(`{"items":[]}`), nil
		}
	}, "fixture-namespace", "artifact-task")
	var report map[string]any
	require.NoError(t, json.Unmarshal([]byte(got), &report))
	events := report["events"].([]any)
	require.Len(t, events, 15)
	require.Equal(t, "Event5", events[0].(map[string]any)["reason"])
	require.Equal(t, "Event19", events[14].(map[string]any)["reason"])
	require.Len(t, events[0].(map[string]any)["message"], 1024+len("...[truncated]"))
	require.NotContains(t, got, "unexpected-object")
}

func jsonObjectAt(t *testing.T, data string, path ...string) string {
	t.Helper()
	var object map[string]any
	require.NoError(t, json.Unmarshal([]byte(data), &object))
	for _, field := range path {
		object = object[field].(map[string]any)
	}
	encoded, err := json.Marshal(object)
	require.NoError(t, err)
	return string(encoded)
}
