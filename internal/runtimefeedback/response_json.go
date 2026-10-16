package runtimefeedback

import (
	"encoding/json"
	"slices"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

// validReportJSON checks the bounded response before encoding/json can merge
// repeated objects or case-fold schema field names. Loss-counter keys remain
// case-sensitive data, while duplicate keys are rejected throughout the report.
func validReportJSON(data []byte) bool {
	if _, err := harnessv2.CanonicalJSON(data); err != nil {
		return false
	}
	report, ok := exactFeedbackFields(data, "apiVersion", "kind", "runID", "workload", "status", "sampledAt",
		"capture", "source", "attributionScope", "completeness", "events", "droppedEvents", "losses", "limitations", "explanation")
	if !ok {
		return false
	}
	for _, object := range []struct {
		name   string
		fields []string
	}{
		{"workload", []string{"namespace", "podName", "podUID", "containerName", "containerID", "restartCount", "node"}},
		{"capture", []string{"startedAt", "endedAt", "expiresAt"}},
		{"source", []string{"name", "agentVersion", "gadget", "instanceID"}},
	} {
		if _, ok := exactFeedbackFields(report[object.name], object.fields...); !ok {
			return false
		}
	}
	var events []json.RawMessage
	if raw := report["events"]; len(raw) != 0 && json.Unmarshal(raw, &events) != nil {
		return false
	}
	for _, event := range events {
		if _, ok := exactFeedbackFields(event, "timestamp", "decision", "decisionReason", "kernelEnforced",
			"destinationAddress", "destinationPort", "protocol", "policyUID", "policyGeneration", "activeGeneration"); !ok {
			return false
		}
	}
	return true
}

func exactFeedbackFields(raw json.RawMessage, names ...string) (map[string]json.RawMessage, bool) {
	var fields map[string]json.RawMessage
	// Preserve decoding semantics for absent and null optional objects; typed
	// decoding and Report.Validate still enforce their values and bindings.
	if len(raw) != 0 && json.Unmarshal(raw, &fields) != nil {
		return nil, false
	}
	for name := range fields {
		if !slices.Contains(names, name) {
			return nil, false
		}
	}
	return fields, true
}
