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
	"os/exec"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/redact"
)

type artifactDiagnosticCommand func(context.Context, ...string) ([]byte, error)

func artifactTaskFailureDiagnostics(taskNamespace, taskName string, secrets ...string) string {
	return collectArtifactTaskDiagnostics(func(ctx context.Context, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "kubectl", args...)
		cmd.WaitDelay = time.Second
		// Never include unrestricted kubectl stderr in a test failure.
		return cmd.Output()
	}, taskNamespace, taskName, secrets...)
}

// collectArtifactTaskDiagnostics reads only this fixture's resources before its
// cleanup deletes them. The output excludes specs, annotations, results and logs.
func collectArtifactTaskDiagnostics(run artifactDiagnosticCommand, taskNamespace, taskName string, secrets ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	report := map[string]any{"namespace": taskNamespace, "taskName": taskName}
	var unavailable []string
	read := func(resource string, args ...string) map[string]any {
		args = append([]string{"get", resource, "-n", taskNamespace, "-o", "json", "--request-timeout=5s"}, args...)
		data, err := run(ctx, args...)
		if err == nil {
			var object map[string]any
			if json.Unmarshal(data, &object) == nil && object != nil {
				return object
			}
		}
		// Errors and malformed responses may contain credentials. Report only
		// which resource could not be read, then continue with the other reads.
		unavailable = append(unavailable, resource)
		return nil
	}

	var objects []map[string]any
	if task := read("task", taskName); task != nil {
		report["task"] = artifactDiagnosticResource(task)
		objects = append(objects, task)
	}
	selector := labels.LabelTask + "=" + labels.SelectorValue(taskName)
	for _, resource := range []string{"jobs", "pods"} {
		list := read(resource, "-l", selector)
		var summaries []map[string]any
		for _, item := range artifactDiagnosticItems(list) {
			summaries = append(summaries, artifactDiagnosticResource(item))
			objects = append(objects, item)
		}
		report[resource] = summaries
	}

	var events []map[string]any
	for _, object := range objects {
		metadata, _ := object["metadata"].(map[string]any)
		uid, _ := metadata["uid"].(string)
		if uid == "" {
			continue
		}
		list := read("events", "--field-selector=involvedObject.uid="+uid, "--sort-by=.lastTimestamp")
		items := artifactDiagnosticItems(list)
		// Retain the latest events for each exact object identity.
		for _, event := range items[max(0, len(items)-15):] {
			summary := artifactDiagnosticFields(event, "type", "reason", "message", "count", "firstTimestamp", "lastTimestamp", "eventTime")
			involved, _ := event["involvedObject"].(map[string]any)
			summary["involvedObject"] = artifactDiagnosticFields(involved, "kind", "name", "uid")
			events = append(events, summary)
		}
	}
	report["events"] = events
	if len(unavailable) > 0 {
		report["unavailable"] = unavailable
	}
	data, _ := json.MarshalIndent(artifactDiagnosticRedact(report, secrets), "", "  ")
	return string(data)
}

func artifactDiagnosticItems(list map[string]any) []map[string]any {
	var objects []map[string]any
	items, _ := list["items"].([]any)
	for _, item := range items {
		if object, ok := item.(map[string]any); ok {
			objects = append(objects, object)
		}
	}
	return objects
}

func artifactDiagnosticResource(object map[string]any) map[string]any {
	metadata, _ := object["metadata"].(map[string]any)
	status, _ := object["status"].(map[string]any)
	summary := artifactDiagnosticFields(status, "phase", "attempts", "jobName", "message", "reason",
		"active", "succeeded", "failed", "startTime", "completionTime")
	conditions, _ := status["conditions"].([]any)
	var safeConditions []map[string]any
	for _, value := range conditions {
		condition, _ := value.(map[string]any)
		safeConditions = append(safeConditions, artifactDiagnosticFields(condition, "type", "status", "reason", "message", "lastTransitionTime"))
	}
	if len(safeConditions) > 0 {
		summary["conditions"] = safeConditions
	}
	for _, field := range []string{"initContainerStatuses", "containerStatuses"} {
		containers, _ := status[field].([]any)
		var safeContainers []map[string]any
		for _, value := range containers {
			container, _ := value.(map[string]any)
			safeContainer := artifactDiagnosticFields(container, "name", "ready", "restartCount")
			for _, stateField := range []string{"state", "lastState"} {
				state, _ := container[stateField].(map[string]any)
				safeState := map[string]any{}
				for _, phase := range []string{"running", "waiting", "terminated"} {
					if details, ok := state[phase].(map[string]any); ok {
						safeState[phase] = artifactDiagnosticFields(details, "reason", "message", "exitCode", "signal", "startedAt", "finishedAt")
					}
				}
				safeContainer[stateField] = safeState
			}
			safeContainers = append(safeContainers, safeContainer)
		}
		if len(safeContainers) > 0 {
			summary[field] = safeContainers
		}
	}
	return map[string]any{"metadata": artifactDiagnosticFields(metadata, "name", "uid"), "status": summary}
}

func artifactDiagnosticFields(object map[string]any, fields ...string) map[string]any {
	selected := map[string]any{}
	for _, field := range fields {
		if value, ok := object[field]; ok {
			// Every selected leaf is scalar. Do not accept a nested object in a
			// malformed response where one of those fields should have been.
			switch value.(type) {
			case string, float64, bool, nil:
				selected[field] = value
			}
		}
	}
	return selected
}

func artifactDiagnosticRedact(value any, secrets []string) any {
	switch typed := value.(type) {
	case string:
		for _, secret := range secrets {
			if secret != "" {
				typed = strings.ReplaceAll(typed, secret, "[REDACTED]")
			}
		}
		typed = redact.SensitiveText(typed)
		if len(typed) > 1024 {
			typed = typed[:1024] + "...[truncated]"
		}
		return typed
	case map[string]any:
		for key, item := range typed {
			typed[key] = artifactDiagnosticRedact(item, secrets)
		}
	case []map[string]any:
		for _, item := range typed {
			artifactDiagnosticRedact(item, secrets)
		}
	}
	return value
}
