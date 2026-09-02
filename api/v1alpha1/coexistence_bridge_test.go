/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

import (
	"encoding/json"
	"strings"
	"testing"
)

// The bridge schema must decode and round-trip stored objects from BOTH
// pre-coexistence baselines without pruning: the harness v1 baseline
// (origin/main) and the v2-only ACP cutover baseline. These fixtures mirror
// the exact historical JSON shapes.

const storedV1AgentRuntimeJSON = `{
  "contractVersion": "orka.harness.v1",
  "deployment": {"mode": "external-endpoint", "endpoint": "https://harness.example.com"},
  "clientAuth": {"bearerTokenSecretRef": {"name": "harness-auth", "key": "token"}},
  "capabilities": {
    "toolExecutionModes": ["observed", "brokered"],
    "brokeredToolClasses": ["read", "coordination"],
    "supportsCancel": true,
    "supportsRuntimeSessions": true,
    "supportsContinuation": false,
    "supportsArtifacts": true
  }
}`

const storedV1AgentRuntimeStatusJSON = `{
  "ready": true,
  "observedGeneration": 3,
  "observedAuthRefResourceVersion": "12345",
  "observedCapabilities": {
    "protocolVersion": "orka.harness.v1",
    "transport": "http+sse",
    "runtimeName": "agentkit",
    "runtimeVersion": "1.4.2",
    "providerKind": "generic",
    "toolExecutionModes": ["observed"],
    "supportsCancel": true,
    "maxConcurrentTurns": 4,
    "maxTurnSeconds": 1800,
    "maxOutputBytes": 1048576
  }
}`

func TestBridgeStoredV1AgentRuntimeRoundTrips(t *testing.T) {
	var spec AgentRuntimeRegistrySpec
	if err := json.Unmarshal([]byte(storedV1AgentRuntimeJSON), &spec); err != nil {
		t.Fatalf("decode stored v1 AgentRuntime spec: %v", err)
	}
	if spec.ContractVersion == nil || *spec.ContractVersion != AgentRuntimeContractHarnessV1 {
		t.Fatalf("contractVersion = %v", spec.ContractVersion)
	}
	if spec.ClientAuth.BearerAuthRef == nil || spec.ClientAuth.BearerAuthRef.Name != "harness-auth" || spec.ClientAuth.BearerAuthRef.Key != "token" {
		t.Fatalf("bearer auth ref = %+v", spec.ClientAuth.BearerAuthRef)
	}
	if spec.ClientAuth.ControllerBearerTokenSecretRef != nil || spec.ClientAuth.OperationCapabilitySecretRef != nil {
		t.Fatal("v1 auth decode must not synthesize v2 auth fields")
	}
	if spec.Capabilities == nil || len(spec.Capabilities.ToolExecutionModes) != 2 ||
		spec.Capabilities.SupportsCancel == nil || !*spec.Capabilities.SupportsCancel ||
		spec.Capabilities.SupportsContinuation == nil || *spec.Capabilities.SupportsContinuation {
		t.Fatalf("v1 capabilities = %+v", spec.Capabilities)
	}
	if spec.Capabilities.Profile != nil || spec.Capabilities.Limits != nil || spec.Capabilities.WorkspaceGovernance != nil {
		t.Fatal("v1 capabilities decode must not synthesize v2 capability fields")
	}

	reencoded, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("re-encode stored v1 AgentRuntime spec: %v", err)
	}
	serialized := string(reencoded)
	for _, required := range []string{`"orka.harness.v1"`, `"bearerTokenSecretRef"`, `"toolExecutionModes"`, `"brokeredToolClasses"`, `"supportsCancel"`} {
		if !strings.Contains(serialized, required) {
			t.Fatalf("re-encoded v1 spec lost %s: %s", required, serialized)
		}
	}
	for _, forbidden := range []string{"controllerBearerTokenSecretRef", "operationCapabilitySecretRef", "workspaceGovernance", `"profile"`} {
		if strings.Contains(serialized, forbidden) {
			t.Fatalf("re-encoded v1 spec gained v2 field %s: %s", forbidden, serialized)
		}
	}

}

func TestBridgeStoredV1AgentRuntimeStatusRoundTrips(t *testing.T) {
	var status AgentRuntimeStatus
	if err := json.Unmarshal([]byte(storedV1AgentRuntimeStatusJSON), &status); err != nil {
		t.Fatalf("decode stored v1 AgentRuntime status: %v", err)
	}
	if status.ObservedAuthRefResourceVersion != "12345" {
		t.Fatalf("observedAuthRefResourceVersion = %q; the v1 status surface must serialize", status.ObservedAuthRefResourceVersion)
	}
	if status.ObservedCapabilities == nil || status.ObservedCapabilities.RuntimeName != "agentkit" ||
		status.ObservedCapabilities.MaxConcurrentTurns != 4 || status.ObservedCapabilities.MaxOutputBytes != 1048576 {
		t.Fatalf("v1 observed capabilities = %+v", status.ObservedCapabilities)
	}
	if status.ObservedCapabilities.Limits != nil || status.ObservedCapabilities.WorkspaceGovernance != nil {
		t.Fatalf("v1 observed capabilities gained v2-only fields: %+v", status.ObservedCapabilities)
	}
	statusJSON, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("re-encode v1 status: %v", err)
	}
	serialized := string(statusJSON)
	if !strings.Contains(serialized, `"observedAuthRefResourceVersion":"12345"`) {
		t.Fatalf("v1 status round trip lost observedAuthRefResourceVersion: %s", statusJSON)
	}
	for _, forbidden := range []string{`"limits"`, `"workspaceGovernance"`} {
		if strings.Contains(serialized, forbidden) {
			t.Fatalf("v1 status serialized schema-invalid v2 field %s: %s", forbidden, statusJSON)
		}
	}
}

const storedV1TaskSpecFragmentJSON = `{
  "type": "agent",
  "prompt": "fix the bug",
  "agentRef": {"name": "coder"},
  "agentRuntime": {
    "workspace": {
      "gitRepo": "https://github.com/org/repo.git",
      "branch": "main",
      "ref": "abc123",
      "gitSecretRef": {"name": "git-credentials"},
      "subPath": "services/api",
      "forkRepo": "https://github.com/bot/repo.git",
      "prBaseBranch": "main",
      "pushBranch": "agent/fix-1"
    },
    "maxTurns": 25,
    "allowedTools": []
  }
}`

const storedV1TaskStatusFragmentJSON = `{
  "phase": "Running",
  "harnessRuntime": {
    "runtimeRefName": "external-harness",
    "runtimeName": "agentkit",
    "contractVersion": "orka.harness.v1",
    "endpoint": "https://harness.example.com",
    "runtimeGeneration": 7,
    "authRefName": "harness-auth",
    "authRefField": "token",
    "authRefResourceVersion": "999"
  }
}`

func TestBridgeStoredV1TaskRoundTrips(t *testing.T) {
	var spec TaskSpec
	if err := json.Unmarshal([]byte(storedV1TaskSpecFragmentJSON), &spec); err != nil {
		t.Fatalf("decode stored v1 Task spec: %v", err)
	}
	workspace := spec.AgentRuntime.Workspace
	if workspace == nil || workspace.GitRepo != "https://github.com/org/repo.git" ||
		workspace.GitSecretRef == nil || workspace.GitSecretRef.Name != "git-credentials" ||
		workspace.ForkRepo != "https://github.com/bot/repo.git" || workspace.PushBranch != "agent/fix-1" {
		t.Fatalf("legacy workspace = %+v", workspace)
	}
	// The explicit-empty allowlist must survive alongside the legacy workspace.
	if spec.AgentRuntime.AllowedTools == nil || len(spec.AgentRuntime.AllowedTools) != 0 {
		t.Fatalf("explicit empty allowedTools was not preserved: %#v", spec.AgentRuntime.AllowedTools)
	}

	reencoded, err := json.Marshal(spec.AgentRuntime)
	if err != nil {
		t.Fatalf("re-encode legacy agentRuntime: %v", err)
	}
	serialized := string(reencoded)
	for _, required := range []string{`"gitSecretRef"`, `"forkRepo"`, `"pushBranch"`, `"allowedTools":[]`} {
		if !strings.Contains(serialized, required) {
			t.Fatalf("legacy agentRuntime round trip lost %s: %s", required, serialized)
		}
	}

	var status TaskStatus
	if err := json.Unmarshal([]byte(storedV1TaskStatusFragmentJSON), &status); err != nil {
		t.Fatalf("decode stored v1 Task status: %v", err)
	}
	if status.HarnessRuntime == nil || status.HarnessRuntime.ContractVersion != "orka.harness.v1" ||
		status.HarnessRuntime.RuntimeGeneration != 7 || status.HarnessRuntime.AuthRefResourceVersion != "999" {
		t.Fatalf("harnessRuntime status = %+v", status.HarnessRuntime)
	}
	statusJSON, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("re-encode v1 Task status: %v", err)
	}
	if !strings.Contains(string(statusJSON), `"harnessRuntime"`) {
		t.Fatalf("v1 Task status round trip lost harnessRuntime: %s", statusJSON)
	}
}

const storedV1OpenCodeAgentJSON = `{
  "runtime": {"type": "opencode", "contractVersion": "orka.harness.v1", "defaultMaxTurns": 50},
  "model": {"name": "gpt-5.2", "maxTokens": 8192},
  "systemPrompt": {"inline": "You are a careful engineer."},
  "secretRef": {"name": "opencode-credentials"}
}`

func TestBridgeStoredV1OpenCodeAgentPreserved(t *testing.T) {
	var spec AgentSpec
	if err := json.Unmarshal([]byte(storedV1OpenCodeAgentJSON), &spec); err != nil {
		t.Fatalf("decode stored v1 OpenCode Agent: %v", err)
	}
	agent := &Agent{Spec: spec}
	if agent.BuiltInContractVersion() != AgentRuntimeContractHarnessV1 {
		t.Fatalf("contract = %q", agent.BuiltInContractVersion())
	}
	// Historical v1 OpenCode shape: legacy model ID without provider prefix,
	// Agent-level system prompt, and provider Secret all round-trip.
	if spec.Model == nil || spec.Model.Name != "gpt-5.2" || spec.SystemPrompt == nil ||
		spec.SystemPrompt.Inline == "" || spec.SecretRef == nil || spec.SecretRef.Name != "opencode-credentials" {
		t.Fatalf("v1 OpenCode fields = %+v", spec)
	}
	reencoded, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("re-encode v1 OpenCode Agent: %v", err)
	}
	for _, required := range []string{`"orka.harness.v1"`, `"gpt-5.2"`, `"systemPrompt"`, `"opencode-credentials"`} {
		if !strings.Contains(string(reencoded), required) {
			t.Fatalf("v1 OpenCode Agent round trip lost %s: %s", required, reencoded)
		}
	}
}

func TestAgentCLIRuntimeContractVersionSerialization(t *testing.T) {
	v2 := AgentRuntimeContractHarnessV2
	runtime := AgentCLIRuntime{Type: AgentRuntimeCodex, ContractVersion: &v2}
	encoded, err := json.Marshal(runtime)
	if err != nil {
		t.Fatalf("marshal runtime: %v", err)
	}
	if !strings.Contains(string(encoded), `"contractVersion":"orka.harness.v2"`) {
		t.Fatalf("contractVersion missing from custom marshal output: %s", encoded)
	}

	// The omitted-versus-explicit-empty allowlist distinction survives the
	// selector addition.
	runtime.DefaultAllowedTools = []string{}
	encoded, err = json.Marshal(runtime)
	if err != nil {
		t.Fatalf("marshal runtime with empty allowlist: %v", err)
	}
	if !strings.Contains(string(encoded), `"defaultAllowedTools":[]`) {
		t.Fatalf("explicit empty allowlist was dropped: %s", encoded)
	}
	runtime.DefaultAllowedTools = nil
	encoded, err = json.Marshal(runtime)
	if err != nil {
		t.Fatalf("marshal runtime with omitted allowlist: %v", err)
	}
	if strings.Contains(string(encoded), "defaultAllowedTools") {
		t.Fatalf("omitted allowlist serialized: %s", encoded)
	}

	unclassified := AgentCLIRuntime{Type: AgentRuntimeOpencode}
	encoded, err = json.Marshal(unclassified)
	if err != nil {
		t.Fatalf("marshal unclassified runtime: %v", err)
	}
	if strings.Contains(string(encoded), "contractVersion") {
		t.Fatalf("unclassified runtime must omit contractVersion: %s", encoded)
	}
	if (&Agent{Spec: AgentSpec{Runtime: &unclassified}}).BuiltInContractVersion() != "" {
		t.Fatal("unclassified agent must report empty contract")
	}
}
