/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRuntimeJSONRejectsRemovedProtocolAndFields(t *testing.T) {
	tests := []struct {
		name   string
		data   string
		target any
		want   string
	}{
		{"v1 contract", `"orka.harness.v1"`, new(AgentRuntimeContractVersion), "only orka.harness.v2"},
		{"mode as contract", `"harness-v1"`, new(AgentRuntimeContractVersion), "only orka.harness.v2"},
		{"v1 backend", `"harness-wrapper"`, new(AgentExecutionBackend), "unsupported execution backend"},
		{"workspace", `{"workspace":{"gitRepo":"https://example.test/repo.git"}}`, new(AgentRuntimeSpec), "use spec.workspace"},
		{"null workspace", `{"workspace":null}`, new(AgentRuntimeSpec), "use spec.workspace"},
		{"case variant workspace", `{"Workspace":{}}`, new(AgentRuntimeSpec), "use spec.workspace"},
		{"case variant known field", `{"MaxTurns":1001}`, new(AgentRuntimeSpec), "spec.agentRuntime.MaxTurns is not supported"},
		{"unknown field", `{"unexpectedCredential":"ignored-value"}`, new(AgentRuntimeClientAuth), "spec.clientAuth.unexpectedCredential is not supported"},
		{"runtime credentials", `{"type":"codex","secretRef":null}`, new(AgentCLIRuntime), "spec.runtime.secretRef is no longer supported"},
		{"bearer auth", `{"bearerTokenSecretRef":{"name":"old-auth","key":"token"}}`, new(AgentRuntimeClientAuth), "bearerTokenSecretRef is no longer supported"},
		{"capabilities", `{"supportsArtifacts":false}`, new(AgentRuntimeCapabilitiesSpec), "supportsArtifacts is no longer supported"},
		{"observed contract", `{"protocolVersion":"orka.harness.v1"}`, new(AgentRuntimeObservedCapabilities), "only orka.harness.v2"},
		{"observed capabilities", `{"supportsSuspend":false}`, new(AgentRuntimeObservedCapabilities), "supportsSuspend is no longer supported"},
		{"task status", `{"harnessRuntime":{}}`, new(TaskStatus), "status.harnessRuntime is no longer supported"},
		{"duplicate contract", `{"contractVersion":"orka.harness.v1","contractVersion":"orka.harness.v2"}`, new(AgentRuntimeRegistrySpec), "only orka.harness.v2"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := json.Unmarshal([]byte(test.data), test.target)
			require.ErrorContains(t, err, test.want)
		})
	}
}

func TestRejectedRuntimeJSONLeavesExistingValueUnchanged(t *testing.T) {
	before := AgentRuntimeSpec{AllowedTools: []string{"read_file"}}
	value := before.DeepCopy()
	err := json.Unmarshal([]byte(`{"allowedTools":["write_file"],"workspace":{}}`), value)
	require.Error(t, err)
	require.Equal(t, &before, value)
}

func TestCurrentV2RuntimeStatusIgnoresRetiredSharedAuthAlias(t *testing.T) {
	var registered AgentRuntime
	err := json.Unmarshal([]byte(`{
		"spec":{"contractVersion":"orka.harness.v2"},
		"status":{
			"ready":true,
			"observedAuthRefResourceVersion":"previous-controller-version",
			"observedControllerAuthRefResourceVersion":"controller-version",
			"observedOperationCapabilityRefResourceVersion":"capability-version",
			"observedCapabilities":{"protocolVersion":"orka.harness.v2","runtimeInstanceID":"existing-instance"}
		}
	}`), &registered)
	require.NoError(t, err)
	require.Equal(t, AgentRuntimeContractHarnessV2, registered.RegisteredContractVersion())
	require.True(t, registered.Status.Ready)
	require.Equal(t, "controller-version", registered.Status.ObservedControllerAuthRefResourceVersion)
	require.Equal(t, "capability-version", registered.Status.ObservedOperationCapabilityRefResourceVersion)
	require.Equal(t, "existing-instance", registered.Status.ObservedCapabilities.RuntimeInstanceID)
	data, err := json.Marshal(registered)
	require.NoError(t, err)
	require.NotContains(t, string(data), "observedAuthRefResourceVersion")
}
