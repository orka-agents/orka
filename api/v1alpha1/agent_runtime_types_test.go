/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

import (
	"encoding/json"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

const (
	testExecutionRuntimeClassGVisor = "gvisor"
	testExecutionRuntimeClassKata   = "kata-qemu"
	testExecutionNodeLabelKey       = "sandbox-runtime"
	testAgentRuntimeControllerKey   = "controller-token"
	testAgentRuntimeCapabilityKey   = "capability-secret"
	testAgentRuntimeInstanceID      = "runtime-instance-1"
	testAgentRuntimeAuthSecretName  = "runtime-auth"
)

func TestTaskTypeAgentConstant(t *testing.T) {
	if TaskTypeAgent != "agent" {
		t.Errorf("TaskTypeAgent = %q, want %q", TaskTypeAgent, "agent")
	}
}

func TestTaskTypeConstants(t *testing.T) {
	tests := []struct {
		name     string
		constant TaskType
		want     string
	}{
		{"container", TaskTypeContainer, "container"},
		{"ai", TaskTypeAI, "ai"},
		{"agent", TaskTypeAgent, "agent"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if string(tt.constant) != tt.want {
				t.Errorf("TaskType constant %s = %q, want %q", tt.name, tt.constant, tt.want)
			}
		})
	}
}

func TestAgentRuntimeTypeConstants(t *testing.T) {
	tests := []struct {
		name     string
		constant AgentRuntimeType
		want     string
	}{
		{"copilot", AgentRuntimeCopilot, "copilot"},
		{"claude", AgentRuntimeClaude, "claude"},
		{"codex", AgentRuntimeCodex, "codex"},
		{"opencode", AgentRuntimeOpencode, "opencode"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if string(tt.constant) != tt.want {
				t.Errorf("AgentRuntimeType %s = %q, want %q", tt.name, tt.constant, tt.want)
			}
		})
	}
}

func readTaskTypesSource(t *testing.T) []byte {
	t.Helper()
	_, testFile, _, ok := goruntime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	paths := []string{filepath.Join(filepath.Dir(testFile), "task_types.go"), "task_types.go"}
	var lastErr error
	for _, path := range paths {
		source, err := os.ReadFile(path)
		if err == nil {
			return source
		}
		lastErr = err
	}
	t.Fatalf("read task_types.go: %v", lastErr)
	return nil
}

func TestAgentRuntimeTypeKubebuilderEnumIncludesSupportedBuiltIns(t *testing.T) {
	source := readTaskTypesSource(t)
	const marker = "// +kubebuilder:validation:Enum=claude;codex;copilot"
	if !strings.Contains(string(source), marker) {
		t.Fatalf("AgentRuntimeType marker does not include all supported built-ins: want %q", marker)
	}
}

func TestAgentPromptImmutabilityMarkerHandlesOmittedPrompt(t *testing.T) {
	source := readTaskTypesSource(t)
	const marker = "has(self.prompt) == has(oldSelf.prompt)"
	if !strings.Contains(string(source), marker) {
		t.Fatalf("agent prompt immutability marker is not presence-aware: want %q", marker)
	}
}

func TestAgentRuntimeSpecFields(t *testing.T) {
	maxTurns := int32(10)
	allowBash := true
	spec := AgentRuntimeSpec{
		MaxTurns:        &maxTurns,
		AllowedTools:    []string{"read", "write"},
		DisallowedTools: []string{"delete"},
		AllowBash:       &allowBash,
	}

	if *spec.MaxTurns != 10 {
		t.Errorf("MaxTurns = %d, want 10", *spec.MaxTurns)
	}
	if len(spec.AllowedTools) != 2 {
		t.Errorf("AllowedTools len = %d, want 2", len(spec.AllowedTools))
	}
	if spec.AllowedTools[0] != "read" || spec.AllowedTools[1] != "write" {
		t.Errorf("AllowedTools = %v, want [read write]", spec.AllowedTools)
	}
	if len(spec.DisallowedTools) != 1 || spec.DisallowedTools[0] != "delete" {
		t.Errorf("DisallowedTools = %v, want [delete]", spec.DisallowedTools)
	}
	if *spec.AllowBash != true {
		t.Errorf("AllowBash = %v, want true", *spec.AllowBash)
	}
}

func TestAgentRuntimeSpecDefaults(t *testing.T) {
	spec := AgentRuntimeSpec{}

	if spec.MaxTurns != nil {
		t.Errorf("MaxTurns should be nil by default, got %v", spec.MaxTurns)
	}
	if spec.AllowedTools != nil {
		t.Errorf("AllowedTools should be nil by default, got %v", spec.AllowedTools)
	}
	if spec.DisallowedTools != nil {
		t.Errorf("DisallowedTools should be nil by default, got %v", spec.DisallowedTools)
	}
	if spec.AllowBash != nil {
		t.Errorf("AllowBash should be nil by default, got %v", spec.AllowBash)
	}
}

func TestWorkspaceConfigFields(t *testing.T) {
	readRef := &WorkspaceCredentialReference{Name: "git-read"}
	publicationRef := &WorkspaceCredentialReference{Name: "git-publish"}
	ws := WorkspaceConfig{
		GitRepo:                  "https://github.com/example/repo",
		Branch:                   "develop",
		Ref:                      "abc123",
		ReadCredentialRef:        readRef,
		PublicationGitRepo:       "https://github.com/example/repo-fork",
		PublicationCredentialRef: publicationRef,
		SubPath:                  "src/app",
	}

	if ws.GitRepo != "https://github.com/example/repo" {
		t.Errorf("GitRepo = %q, want %q", ws.GitRepo, "https://github.com/example/repo")
	}
	if ws.Branch != "develop" {
		t.Errorf("Branch = %q, want %q", ws.Branch, "develop")
	}
	if ws.Ref != "abc123" {
		t.Errorf("Ref = %q, want %q", ws.Ref, "abc123")
	}
	if ws.ReadCredentialRef == nil || ws.ReadCredentialRef.Name != "git-read" {
		t.Errorf("ReadCredentialRef.Name = %v, want %q", ws.ReadCredentialRef, "git-read")
	}
	if ws.PublicationCredentialRef == nil || ws.PublicationCredentialRef.Name != "git-publish" {
		t.Errorf("PublicationCredentialRef.Name = %v, want %q", ws.PublicationCredentialRef, "git-publish")
	}
	if ws.PublicationGitRepo != "https://github.com/example/repo-fork" {
		t.Errorf("PublicationGitRepo = %q, want %q", ws.PublicationGitRepo, "https://github.com/example/repo-fork")
	}
	if ws.SubPath != "src/app" {
		t.Errorf("SubPath = %q, want %q", ws.SubPath, "src/app")
	}
}

func TestWorkspaceConfigDefaults(t *testing.T) {
	ws := WorkspaceConfig{}

	if ws.GitRepo != "" {
		t.Errorf("GitRepo should be empty by default, got %q", ws.GitRepo)
	}
	if ws.Branch != "" {
		t.Errorf("Branch should be empty by default, got %q", ws.Branch)
	}
	if ws.Ref != "" {
		t.Errorf("Ref should be empty by default, got %q", ws.Ref)
	}
	if ws.ReadCredentialRef != nil {
		t.Errorf("ReadCredentialRef should be nil by default, got %v", ws.ReadCredentialRef)
	}
	if ws.PublicationCredentialRef != nil {
		t.Errorf("PublicationCredentialRef should be nil by default, got %v", ws.PublicationCredentialRef)
	}
	if ws.SubPath != "" {
		t.Errorf("SubPath should be empty by default, got %q", ws.SubPath)
	}
}

func TestAgentCLIRuntimeFields(t *testing.T) {
	maxTurns := int32(50)
	allowBash := true
	runtime := AgentCLIRuntime{
		Type:                   AgentRuntimeCodex,
		DefaultMaxTurns:        &maxTurns,
		DefaultAllowedTools:    []string{"bash", "edit"},
		DefaultAllowBash:       &allowBash,
		DefaultReasoningEffort: "high",
	}

	if runtime.Type != AgentRuntimeCodex {
		t.Errorf("Type = %q, want %q", runtime.Type, AgentRuntimeCodex)
	}
	if *runtime.DefaultMaxTurns != 50 {
		t.Errorf("DefaultMaxTurns = %d, want 50", *runtime.DefaultMaxTurns)
	}
	if len(runtime.DefaultAllowedTools) != 2 {
		t.Errorf("DefaultAllowedTools len = %d, want 2", len(runtime.DefaultAllowedTools))
	}
	if runtime.DefaultAllowBash == nil || !*runtime.DefaultAllowBash {
		t.Error("DefaultAllowBash should be true")
	}
	if runtime.DefaultReasoningEffort != "high" {
		t.Errorf("DefaultReasoningEffort = %q, want high", runtime.DefaultReasoningEffort)
	}
}

func TestAgentCLIRuntimeJSONPreservesExplicitEmptyAllowedTools(t *testing.T) {
	omitted, err := json.Marshal(AgentCLIRuntime{Type: AgentRuntimeOpencode})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(omitted), `"defaultAllowedTools"`) {
		t.Fatalf("omitted runtime JSON = %s, want no defaultAllowedTools field", omitted)
	}

	explicit, err := json.Marshal(AgentCLIRuntime{
		Type:                   AgentRuntimeOpencode,
		DefaultAllowedTools:    []string{},
		DefaultReasoningEffort: "high",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(explicit), `"defaultAllowedTools":[]`) {
		t.Fatalf("explicit empty runtime JSON = %s, want defaultAllowedTools:[]", explicit)
	}
	var roundTrip AgentCLIRuntime
	if err := json.Unmarshal(explicit, &roundTrip); err != nil {
		t.Fatal(err)
	}
	if roundTrip.DefaultAllowedTools == nil || len(roundTrip.DefaultAllowedTools) != 0 {
		t.Fatalf("round-trip defaultAllowedTools = %#v, want explicit empty slice", roundTrip.DefaultAllowedTools)
	}
	if roundTrip.DefaultReasoningEffort != "high" {
		t.Fatalf("round-trip defaultReasoningEffort = %q, want high", roundTrip.DefaultReasoningEffort)
	}
}

func TestAgentCLIRuntimeOnAgentSpec(t *testing.T) {
	maxTurns := int32(25)
	allowBash := false
	agent := AgentSpec{
		Runtime: &AgentCLIRuntime{
			Type:             AgentRuntimeClaude,
			DefaultMaxTurns:  &maxTurns,
			DefaultAllowBash: &allowBash,
		},
	}

	if agent.Runtime == nil {
		t.Fatal("Runtime should not be nil")
	}
	if agent.Runtime.Type != AgentRuntimeClaude {
		t.Errorf("Runtime.Type = %q, want %q", agent.Runtime.Type, AgentRuntimeClaude)
	}
	if *agent.Runtime.DefaultMaxTurns != 25 {
		t.Errorf("Runtime.DefaultMaxTurns = %d, want 25", *agent.Runtime.DefaultMaxTurns)
	}
	if agent.Runtime.DefaultAllowBash == nil || *agent.Runtime.DefaultAllowBash {
		t.Error("Runtime.DefaultAllowBash should be false")
	}
}

func TestTaskSpecAgentRuntimeField(t *testing.T) {
	maxTurns := int32(15)
	task := TaskSpec{
		Type:   TaskTypeAgent,
		Prompt: "Fix the bug",
		AgentRef: &AgentReference{
			Name: "my-agent",
		},
		AgentRuntime: &AgentRuntimeSpec{
			MaxTurns:     &maxTurns,
			AllowedTools: []string{"bash", "read"},
		},
		Workspace: &WorkspaceConfig{
			GitRepo: "https://github.com/example/repo",
			Branch:  "main",
		},
	}

	if task.Type != TaskTypeAgent {
		t.Errorf("Type = %q, want %q", task.Type, TaskTypeAgent)
	}
	if task.AgentRuntime == nil {
		t.Fatal("AgentRuntime should not be nil")
	}
	if *task.AgentRuntime.MaxTurns != 15 {
		t.Errorf("AgentRuntime.MaxTurns = %d, want 15", *task.AgentRuntime.MaxTurns)
	}
	if task.Workspace == nil {
		t.Fatal("Task.Workspace should not be nil")
	}
	if task.Workspace.Branch != "main" {
		t.Errorf("Workspace.Branch = %q, want %q", task.Workspace.Branch, "main")
	}
}

func TestAgentRuntimeRequiredForAgentType(t *testing.T) {
	// When type is "agent", AgentRuntime and AgentRef should be set for proper configuration.
	// This tests the structural expectation (not webhook validation).
	task := TaskSpec{
		Type: TaskTypeAgent,
	}

	if task.AgentRuntime != nil {
		t.Error("AgentRuntime should be nil when not explicitly set")
	}
	if task.AgentRef != nil {
		t.Error("AgentRef should be nil when not explicitly set")
	}

	// A well-formed agent task should have AgentRef
	task.AgentRef = &AgentReference{Name: "my-agent"}
	if task.AgentRef == nil {
		t.Error("AgentRef should not be nil after being set")
	}

	// And optionally AgentRuntime for overrides
	maxTurns := int32(20)
	task.AgentRuntime = &AgentRuntimeSpec{MaxTurns: &maxTurns}
	if task.AgentRuntime == nil {
		t.Error("AgentRuntime should not be nil after being set")
	}
}

func TestAgentRuntimeTypeAssignment(t *testing.T) {
	// Verify AgentRuntimeType can be used in AgentCLIRuntime
	tests := []struct {
		name        string
		runtimeType AgentRuntimeType
	}{
		{"copilot runtime", AgentRuntimeCopilot},
		{"claude runtime", AgentRuntimeClaude},
		{"codex runtime", AgentRuntimeCodex},
		{"opencode runtime", AgentRuntimeOpencode},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runtime := AgentCLIRuntime{Type: tt.runtimeType}
			if runtime.Type != tt.runtimeType {
				t.Errorf("Type = %q, want %q", runtime.Type, tt.runtimeType)
			}
		})
	}
}

func TestExecutionSpecFields(t *testing.T) {
	spec := ExecutionSpec{
		RuntimeClassName: testExecutionRuntimeClassGVisor,
		NodeSelector: map[string]string{
			testExecutionNodeLabelKey: testExecutionRuntimeClassGVisor,
		},
		Tolerations: []corev1.Toleration{
			{
				Key:      testExecutionNodeLabelKey,
				Operator: corev1.TolerationOpEqual,
				Value:    testExecutionRuntimeClassGVisor,
				Effect:   corev1.TaintEffectNoSchedule,
			},
		},
		Affinity: &corev1.Affinity{
			NodeAffinity: &corev1.NodeAffinity{},
		},
	}

	if spec.RuntimeClassName != testExecutionRuntimeClassGVisor {
		t.Errorf("RuntimeClassName = %q, want %q", spec.RuntimeClassName, testExecutionRuntimeClassGVisor)
	}
	if got := spec.NodeSelector[testExecutionNodeLabelKey]; got != testExecutionRuntimeClassGVisor {
		t.Errorf("NodeSelector[%s] = %q, want %q", testExecutionNodeLabelKey, got, testExecutionRuntimeClassGVisor)
	}
	if len(spec.Tolerations) != 1 {
		t.Fatalf("Tolerations len = %d, want 1", len(spec.Tolerations))
	}
	if spec.Tolerations[0].Effect != corev1.TaintEffectNoSchedule {
		t.Errorf("Tolerations[0].Effect = %q, want %q", spec.Tolerations[0].Effect, corev1.TaintEffectNoSchedule)
	}
	if spec.Affinity == nil || spec.Affinity.NodeAffinity == nil {
		t.Fatal("Affinity.NodeAffinity should not be nil")
	}
}

func TestExecutionSpecOnAgentAndTaskSpec(t *testing.T) {
	agent := AgentSpec{
		Execution: &ExecutionSpec{
			RuntimeClassName: testExecutionRuntimeClassKata,
		},
	}
	task := TaskSpec{
		Type: TaskTypeAgent,
		Execution: &ExecutionSpec{
			RuntimeClassName: testExecutionRuntimeClassGVisor,
		},
	}

	if agent.Execution == nil {
		t.Fatal("Agent.Execution should not be nil")
	}
	if agent.Execution.RuntimeClassName != testExecutionRuntimeClassKata {
		t.Errorf("Agent.Execution.RuntimeClassName = %q, want %q", agent.Execution.RuntimeClassName, testExecutionRuntimeClassKata)
	}
	if task.Execution == nil {
		t.Fatal("Task.Execution should not be nil")
	}
	if task.Execution.RuntimeClassName != testExecutionRuntimeClassGVisor {
		t.Errorf("Task.Execution.RuntimeClassName = %q, want %q", task.Execution.RuntimeClassName, testExecutionRuntimeClassGVisor)
	}
}

func TestAgentRuntimeReferenceOnAgentCLI(t *testing.T) {
	runtime := AgentCLIRuntime{RuntimeRef: &AgentRuntimeReference{Name: "fibey-agentkit"}}
	if runtime.RuntimeRef == nil || runtime.RuntimeRef.Name != "fibey-agentkit" {
		t.Fatalf("RuntimeRef = %#v, want fibey-agentkit", runtime.RuntimeRef)
	}
	if runtime.Type != "" {
		t.Fatalf("Type = %q, want empty for runtimeRef custom runtime", runtime.Type)
	}
}

func TestAgentRuntimeCRDSpecFields(t *testing.T) {
	strict := AgentRuntimeWorkspaceGovernanceCapabilities{
		Mode:                            AgentRuntimeWorkspaceGovernanceStrict,
		OrkaOwnedWorkspaceDeltas:        true,
		PromptScopedBrokerAuthorization: true,
		NoDirectSCMPublication:          true,
		OrkaOwnedCleanRoomPublication:   true,
		ExactInstanceFencing:            true,
		DuplicateSafeMutations:          true,
		CancellationSettlement:          true,
	}
	runtime := AgentRuntime{
		Spec: AgentRuntimeRegistrySpec{
			ContractVersion: AgentRuntimeContractHarnessV2,
			Deployment: AgentRuntimeDeploymentSpec{
				Mode:     AgentRuntimeDeploymentModeExternalEndpoint,
				Endpoint: "https://runtime.example.com",
			},
			ClientAuth: AgentRuntimeClientAuth{
				ControllerBearerTokenSecretRef: AgentRuntimeSecretKeyReference{Name: testAgentRuntimeAuthSecretName, Key: testAgentRuntimeControllerKey},
				OperationCapabilitySecretRef:   AgentRuntimeSecretKeyReference{Name: testAgentRuntimeAuthSecretName, Key: testAgentRuntimeCapabilityKey},
			},
			Capabilities: &AgentRuntimeCapabilitiesSpec{
				RuntimeInstanceID: testAgentRuntimeInstanceID,
				Profile: AgentRuntimeProfileSpec{
					Digest: "sha256:" + strings.Repeat("a", 64), DigestSchemaVersion: 1,
					ACPProfile: "acp.v1", AdapterName: "codex", AdapterDigest: "sha256:" + strings.Repeat("b", 64),
					ProviderKind: "codex", Model: "gpt-test",
					AgentConfigurationDigest: "sha256:" + strings.Repeat("c", 64),
					ToolPolicyDigest:         "sha256:" + strings.Repeat("d", 64), ApprovalPolicyDigest: "sha256:" + strings.Repeat("e", 64),
					MCPConfigurationDigest: "sha256:" + strings.Repeat("f", 64), WorkspaceIntent: WorkspaceIntentRead,
					ProxyCredentialRole: "provider-proxy", ProxyCredentialScope: "session-and-prompt", ResourceClass: "standard",
				},
				Limits: AgentRuntimeProtocolLimits{
					MaxResidentSessions: 10, MaxConcurrentPrompts: 4, MaxRequestBytes: 1 << 20,
					MaxEventLineBytes: 1 << 20, MaxTerminalResultBytes: 1 << 20, MaxBufferedEvents: 256,
					MaxUpdateEventsPerSecond: 100, MinPromptLeaseMillis: 5000, MaxPromptLeaseMillis: 120000,
					MaxPendingPermissions: 32, MaxWorkspaceDeltaBytes: 512 << 20,
				},
				SupportsDrain:       true,
				WorkspaceGovernance: strict,
			},
		},
	}
	if runtime.Spec.ContractVersion != AgentRuntimeContractHarnessV2 {
		t.Fatalf("ContractVersion = %q", runtime.Spec.ContractVersion)
	}
	if runtime.Spec.ClientAuth.ControllerBearerTokenSecretRef.Key != testAgentRuntimeControllerKey ||
		runtime.Spec.ClientAuth.OperationCapabilitySecretRef.Key != testAgentRuntimeCapabilityKey {
		t.Fatalf("ClientAuth = %#v", runtime.Spec.ClientAuth)
	}
	if !runtime.Spec.Capabilities.SupportsStrictWorkspaceIntent(WorkspaceIntentRead) {
		t.Fatal("strict read profile was not eligible for strict read intent")
	}
	if runtime.Spec.Capabilities.SupportsStrictWorkspaceIntent(WorkspaceIntentWrite) {
		t.Fatal("exact read profile was incorrectly eligible for strict write intent")
	}
}

func TestAgentRuntimeWriteIntentRequiresPublicationFinalization(t *testing.T) {
	capabilities := AgentRuntimeCapabilitiesSpec{
		Profile: AgentRuntimeProfileSpec{WorkspaceIntent: WorkspaceIntentWrite},
		WorkspaceGovernance: AgentRuntimeWorkspaceGovernanceCapabilities{
			Mode: AgentRuntimeWorkspaceGovernanceStrict, OrkaOwnedWorkspaceDeltas: true, PromptScopedBrokerAuthorization: true,
			NoDirectSCMPublication: true, OrkaOwnedCleanRoomPublication: true, ExactInstanceFencing: true,
			DuplicateSafeMutations: true, CancellationSettlement: true,
		},
	}
	if capabilities.SupportsStrictWorkspaceIntent(WorkspaceIntentWrite) {
		t.Fatal("write runtime without publication finalization was strict-write eligible")
	}
	if err := capabilities.ValidateStrictWorkspaceIntent(WorkspaceIntentWrite); err == nil || !strings.Contains(err.Error(), "publication finalization") {
		t.Fatalf("ValidateStrictWorkspaceIntent(write) = %v, want publication-finalization rejection", err)
	}
	capabilities.SupportsPublicationFinalization = true
	if !capabilities.SupportsStrictWorkspaceIntent(WorkspaceIntentWrite) {
		t.Fatal("write runtime with publication finalization was not strict-write eligible")
	}
	if err := capabilities.ValidateStrictWorkspaceIntent(WorkspaceIntentWrite); err != nil {
		t.Fatalf("ValidateStrictWorkspaceIntent(write) = %v", err)
	}
}

func TestAgentRuntimeTrustedNonGovernedIsNeverStrictEligible(t *testing.T) {
	capabilities := AgentRuntimeCapabilitiesSpec{
		Profile: AgentRuntimeProfileSpec{WorkspaceIntent: WorkspaceIntentRead},
		WorkspaceGovernance: AgentRuntimeWorkspaceGovernanceCapabilities{
			Mode:    AgentRuntimeWorkspaceGovernanceTrusted,
			Trusted: true,
		},
	}
	if capabilities.SupportsStrictWorkspaceIntent(WorkspaceIntentRead) || capabilities.SupportsStrictWorkspaceIntent(WorkspaceIntentWrite) {
		t.Fatal("trusted non-governed runtime was eligible for strict workspace intent")
	}
	for _, intent := range []WorkspaceIntent{WorkspaceIntentRead, WorkspaceIntentWrite} {
		if err := capabilities.ValidateStrictWorkspaceIntent(intent); err == nil || !strings.Contains(err.Error(), "trusted-non-governed") {
			t.Fatalf("ValidateStrictWorkspaceIntent(%q) = %v, want explicit trusted rejection", intent, err)
		}
	}
}

func TestAgentRuntimeV1CapabilityFieldsAreAbsentFromSerializedCRDSurface(t *testing.T) {
	digest := func(char string) string { return "sha256:" + strings.Repeat(char, 64) }
	profile := AgentRuntimeProfileSpec{
		Digest: digest("a"), DigestSchemaVersion: 1, ACPProfile: "acp.v1", AdapterName: "codex", AdapterDigest: digest("b"),
		ProviderKind: "codex", Model: "gpt-test", AgentConfigurationDigest: digest("c"), ToolPolicyDigest: digest("d"),
		ApprovalPolicyDigest: digest("e"), MCPConfigurationDigest: digest("f"), WorkspaceIntent: WorkspaceIntentRead,
		ProxyCredentialRole: "provider-proxy", ProxyCredentialScope: "model:gpt-test", ResourceClass: "standard",
	}
	claims := AgentRuntimeWorkspaceGovernanceCapabilities{
		Mode: AgentRuntimeWorkspaceGovernanceStrict, OrkaOwnedWorkspaceDeltas: true, PromptScopedBrokerAuthorization: true,
		NoDirectSCMPublication: true, OrkaOwnedCleanRoomPublication: true, ExactInstanceFencing: true,
		DuplicateSafeMutations: true, CancellationSettlement: true,
	}
	limits := AgentRuntimeProtocolLimits{
		MaxResidentSessions: 10, MaxConcurrentPrompts: 4, MaxRequestBytes: 1 << 20, MaxEventLineBytes: 1 << 20,
		MaxTerminalResultBytes: 1 << 20, MaxBufferedEvents: 256, MaxUpdateEventsPerSecond: 100,
		MinPromptLeaseMillis: 5000, MaxPromptLeaseMillis: 120000, MaxPendingPermissions: 32, MaxWorkspaceDeltaBytes: 100 << 20,
	}
	spec := AgentRuntimeRegistrySpec{
		ContractVersion: AgentRuntimeContractHarnessV2,
		ClientAuth: AgentRuntimeClientAuth{
			ControllerBearerTokenSecretRef: AgentRuntimeSecretKeyReference{Name: testAgentRuntimeAuthSecretName, Key: testAgentRuntimeControllerKey},
			OperationCapabilitySecretRef:   AgentRuntimeSecretKeyReference{Name: testAgentRuntimeAuthSecretName, Key: testAgentRuntimeCapabilityKey},
		},
		Capabilities: &AgentRuntimeCapabilitiesSpec{RuntimeInstanceID: testAgentRuntimeInstanceID, Profile: profile, Limits: limits, SupportsDrain: true, WorkspaceGovernance: claims},
	}
	encoded, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	serialized := string(encoded)
	for _, forbidden := range []string{"orka.harness.v1", "bearerTokenSecretRef", "toolExecutionModes", "brokeredToolClasses", "supportsContinuation", "supportsRuntimeSessions", "supportsArtifacts"} {
		if strings.Contains(serialized, forbidden) {
			t.Fatalf("serialized AgentRuntime still contains v1-only field %q: %s", forbidden, serialized)
		}
	}
	for _, required := range []string{"orka.harness.v2", "controllerBearerTokenSecretRef", "operationCapabilitySecretRef", "runtimeInstanceID", "workspaceGovernance"} {
		if !strings.Contains(serialized, required) {
			t.Fatalf("serialized AgentRuntime is missing v2 field %q: %s", required, serialized)
		}
	}
}

func TestAgentRuntimeSpecJSONPreservesExplicitEmptyAllowedTools(t *testing.T) {
	omitted, err := json.Marshal(AgentRuntimeSpec{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(omitted), `"allowedTools"`) {
		t.Fatalf("omitted runtime JSON = %s, want no allowedTools field", omitted)
	}

	explicit, err := json.Marshal(AgentRuntimeSpec{AllowedTools: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(explicit), `"allowedTools":[]`) {
		t.Fatalf("explicit empty runtime JSON = %s, want allowedTools:[]", explicit)
	}
	var roundTrip AgentRuntimeSpec
	if err := json.Unmarshal(explicit, &roundTrip); err != nil {
		t.Fatal(err)
	}
	if roundTrip.AllowedTools == nil || len(roundTrip.AllowedTools) != 0 {
		t.Fatalf("round-trip allowedTools = %#v, want explicit empty slice", roundTrip.AllowedTools)
	}
}

func TestModelConfigTokenLimitsJSONRoundTrip(t *testing.T) {
	contextWindow := int32(32768)
	maxTokens := int32(4096)
	original := ModelConfig{
		Name:          "openai/gpt-test",
		ContextWindow: &contextWindow,
		MaxTokens:     &maxTokens,
	}
	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var decoded ModelConfig
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.ContextWindow == nil || *decoded.ContextWindow != contextWindow ||
		decoded.MaxTokens == nil || *decoded.MaxTokens != maxTokens {
		t.Fatalf("round-trip model limits = %#v", decoded)
	}
	copy := original.DeepCopy()
	*copy.ContextWindow++
	if *original.ContextWindow != contextWindow {
		t.Fatal("ModelConfig.DeepCopy shared contextWindow storage")
	}
}
