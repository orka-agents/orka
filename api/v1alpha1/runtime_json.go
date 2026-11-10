/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

// UnmarshalJSON rejects unsupported contracts before a client or admission
// decoder can interpret an explicitly requested protocol as current work.
func (in *AgentRuntimeContractVersion) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	if value != string(AgentRuntimeContractHarnessV2) {
		return fmt.Errorf("unsupported contractVersion %q: only orka.harness.v2 is supported", value)
	}
	*in = AgentRuntimeContractVersion(value)
	return nil
}

func (in *AgentExecutionBackend) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	switch AgentExecutionBackend(value) {
	case AgentExecutionBackendRuntimePool, AgentExecutionBackendExternalEndpoint:
		*in = AgentExecutionBackend(value)
		return nil
	default:
		return fmt.Errorf("unsupported execution backend %q: use runtime-pool or external-endpoint", value)
	}
}

func (in *AgentRuntimeSpec) UnmarshalJSON(data []byte) error {
	type plain AgentRuntimeSpec
	var value plain
	if err := decodeRuntimeJSON(data, &value, "spec.agentRuntime", "workspace"); err != nil {
		return err
	}
	*in = AgentRuntimeSpec(value)
	return nil
}

func (in *AgentCLIRuntime) UnmarshalJSON(data []byte) error {
	type plain AgentCLIRuntime
	var value plain
	if err := decodeRuntimeJSON(data, &value, "spec.runtime", "secretRef"); err != nil {
		return err
	}
	*in = AgentCLIRuntime(value)
	return nil
}

func (in *TaskStatus) UnmarshalJSON(data []byte) error {
	type plain TaskStatus
	var value plain
	if err := decodeRuntimeJSON(data, &value, "status", "harnessRuntime"); err != nil {
		return err
	}
	*in = TaskStatus(value)
	return nil
}

func (in *AgentRuntimeClientAuth) UnmarshalJSON(data []byte) error {
	type plain AgentRuntimeClientAuth
	var value plain
	if err := decodeRuntimeJSON(data, &value, "spec.clientAuth", "bearerTokenSecretRef"); err != nil {
		return err
	}
	*in = AgentRuntimeClientAuth(value)
	return nil
}

func (in *AgentRuntimeCapabilitiesSpec) UnmarshalJSON(data []byte) error {
	type plain AgentRuntimeCapabilitiesSpec
	var value plain
	if err := decodeRuntimeJSON(data, &value, "spec.capabilities",
		"toolExecutionModes", "brokeredToolClasses", "supportsCancel",
		"supportsRuntimeSessions", "supportsContinuation", "supportsArtifacts"); err != nil {
		return err
	}
	*in = AgentRuntimeCapabilitiesSpec(value)
	return nil
}

func (in *AgentRuntimeObservedCapabilities) UnmarshalJSON(data []byte) error {
	type plain AgentRuntimeObservedCapabilities
	var value plain
	if err := decodeRuntimeJSON(data, &value, "status.observedCapabilities",
		"runtimeName", "runtimeVersion", "toolExecutionModes", "brokeredToolClasses",
		"supportsCancel", "supportsRuntimeSessions", "supportsContinuation", "supportsArtifacts",
		"supportsSuspend", "supportsWorkspaceSnapshot", "maxConcurrentTurns", "maxTurnSeconds", "maxOutputBytes"); err != nil {
		return err
	}
	if value.ProtocolVersion != "" && value.ProtocolVersion != string(AgentRuntimeContractHarnessV2) {
		return fmt.Errorf("unsupported status.observedCapabilities.protocolVersion %q: only orka.harness.v2 is supported", value.ProtocolVersion)
	}
	*in = AgentRuntimeObservedCapabilities(value)
	return nil
}

// The corresponding CRD objects preserve unknown fields until admission.
// Reject removed inputs before typed decoding can discard them. Only exact
// schema field names may pass: Go's case-insensitive decoding must not turn an
// unvalidated unknown key into a known field after schema validation.
func decodeRuntimeJSON(data []byte, target any, path string, removed ...string) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	structType := reflect.TypeOf(target).Elem()
	known := make(map[string]bool, structType.NumField())
	for field := range structType.Fields() {
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if field.IsExported() && name != "" && name != "-" {
			known[name] = true
		}
	}
	for field := range fields {
		for _, name := range removed {
			if strings.EqualFold(field, name) {
				if path == "spec.agentRuntime" && name == "workspace" {
					return fmt.Errorf("spec.agentRuntime.workspace is no longer supported; use spec.workspace")
				}
				return fmt.Errorf("%s.%s is no longer supported", path, name)
			}
		}
		if !known[field] {
			return fmt.Errorf("%s.%s is not supported", path, field)
		}
	}
	return json.Unmarshal(data, target)
}
