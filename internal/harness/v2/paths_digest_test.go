package v2

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestV2PathsAndHostileSegments(t *testing.T) {
	path, err := PromptPermissionPath("session-1", "prompt-1", "permission-1")
	if err != nil {
		t.Fatalf("PromptPermissionPath() error = %v", err)
	}
	if path != "/v2/runtime-sessions/session-1/prompts/prompt-1/permissions/permission-1" {
		t.Fatalf("PromptPermissionPath() = %q", path)
	}
	publicationFinalizationPath, err := RuntimeSessionPublicationFinalizationPath("session-1")
	if err != nil {
		t.Fatalf("RuntimeSessionPublicationFinalizationPath() error = %v", err)
	}
	if publicationFinalizationPath != "/v2/runtime-sessions/session-1/publication-finalization" {
		t.Fatalf("RuntimeSessionPublicationFinalizationPath() = %q", publicationFinalizationPath)
	}
	if RuntimeSessionPublicationFinalizationPathTemplate != "/v2/runtime-sessions/{sessionID}/publication-finalization" {
		t.Fatalf("RuntimeSessionPublicationFinalizationPathTemplate = %q", RuntimeSessionPublicationFinalizationPathTemplate)
	}

	for _, value := range []string{"../escape", "a/b", "..", "a%2fb", "line\nbreak", "snowman-☃"} {
		t.Run(value, func(t *testing.T) {
			if _, err := RuntimeSessionPath(RuntimeSessionID(value)); err == nil {
				t.Fatalf("RuntimeSessionPath(%q) error = nil, want rejection", value)
			}
		})
	}
}

func TestCanonicalJSONNormalizesOrderingWhitespaceAndNumbers(t *testing.T) {
	left, err := CanonicalJSON([]byte(` { "z": 1.0, "a": [1e1, -0] } `))
	if err != nil {
		t.Fatalf("CanonicalJSON(left) error = %v", err)
	}
	right, err := CanonicalJSON([]byte(`{"a":[10,0],"z":1}`))
	if err != nil {
		t.Fatalf("CanonicalJSON(right) error = %v", err)
	}
	if !bytes.Equal(left, right) {
		t.Fatalf("canonical forms differ:\n%s\n%s", left, right)
	}
	if string(left) != `{"a":[10,0],"z":1}` {
		t.Fatalf("canonical form = %s", left)
	}
}

func TestCanonicalRequestDigestIsDeterministicAndExcludesDigestField(t *testing.T) {
	left := []byte(`{
		"value": 1.0,
		"metadata": {"requestDigest":"first", "requestDigestSchemaVersion":1, "operationID":"op"}
	}`)
	right := []byte(`{"metadata":{"operationID":"op","requestDigestSchemaVersion":1,"requestDigest":"second"},"value":1}`)
	leftDigest, err := CanonicalRequestDigestJSON(left)
	if err != nil {
		t.Fatalf("CanonicalRequestDigestJSON(left) error = %v", err)
	}
	rightDigest, err := CanonicalRequestDigestJSON(right)
	if err != nil {
		t.Fatalf("CanonicalRequestDigestJSON(right) error = %v", err)
	}
	if leftDigest != rightDigest {
		t.Fatalf("equivalent request digests differ: %s != %s", leftDigest, rightDigest)
	}
	if err := ValidateRequestDigest(leftDigest); err != nil {
		t.Fatalf("ValidateRequestDigest() error = %v", err)
	}

	changed, err := CanonicalRequestDigestJSON([]byte(`{"metadata":{"operationID":"other","requestDigestSchemaVersion":1,"requestDigest":""},"value":1}`))
	if err != nil {
		t.Fatalf("CanonicalRequestDigestJSON(changed) error = %v", err)
	}
	if changed == leftDigest {
		t.Fatal("request digest did not change with operation identity")
	}
}

func TestCanonicalJSONRejectsAmbiguousOrHostileInput(t *testing.T) {
	tests := map[string][]byte{
		"duplicate key": []byte(`{"a":1,"a":2}`),
		"trailing data": []byte(`{} {}`),
		"invalid UTF-8": {'{', '"', 'x', '"', ':', '"', 0xff, '"', '}'},
		"huge exponent": []byte(`{"n":1e1000000}`),
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := CanonicalJSON(input); err == nil {
				t.Fatalf("CanonicalJSON(%q) error = nil, want rejection", input)
			}
		})
	}

	deep := strings.Repeat("[", maxCanonicalJSONDepth+2) + "0" + strings.Repeat("]", maxCanonicalJSONDepth+2)
	if _, err := CanonicalJSON([]byte(deep)); err == nil || !strings.Contains(err.Error(), "depth") {
		t.Fatalf("CanonicalJSON(deep) error = %v, want depth error", err)
	}
}

func TestCanonicalRequestDigestRequiresObjectMetadataAndStringDigest(t *testing.T) {
	for name, input := range map[string]string{
		"array":            `[]`,
		"missing metadata": `{}`,
		"missing digest":   `{"metadata":{}}`,
		"wrong type":       `{"metadata":{"requestDigest":{}}}`,
		"duplicate digest": `{"metadata":{"requestDigest":"a","requestDigest":"b"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := CanonicalRequestDigestJSON([]byte(input)); err == nil {
				t.Fatalf("CanonicalRequestDigestJSON(%s) error = nil, want rejection", input)
			}
		})
	}
}

func TestCanonicalProfileDigestChangesWithPolicy(t *testing.T) {
	profile := testRuntimeProfile()
	first, err := CanonicalProfileDigest(profile)
	if err != nil {
		t.Fatalf("CanonicalProfileDigest() error = %v", err)
	}
	profile.ToolPolicyDigest = testSHA256("different-policy")
	second, err := CanonicalProfileDigest(profile)
	if err != nil {
		t.Fatalf("CanonicalProfileDigest(changed) error = %v", err)
	}
	if first == second {
		t.Fatal("profile digest did not change with tool policy")
	}
}

func TestProfileDigestSchemaRemainsV1(t *testing.T) {
	if ProfileDigestDomain != "orka.harness.v2.profile.v1" {
		t.Fatalf("ProfileDigestDomain = %q, want v1 domain", ProfileDigestDomain)
	}
	if ProfileDigestSchemaVersion != 1 {
		t.Fatalf("ProfileDigestSchemaVersion = %d, want 1", ProfileDigestSchemaVersion)
	}
}

const alternateTestModel = "other-model"

func TestCanonicalAgentConfigurationDigestBindsAllFields(t *testing.T) {
	base := testAgentSessionConfiguration()
	baseDigest, err := CanonicalAgentConfigurationDigest(base)
	if err != nil {
		t.Fatalf("CanonicalAgentConfigurationDigest(base) error = %v", err)
	}
	repeated, err := CanonicalAgentConfigurationDigest(base)
	if err != nil {
		t.Fatalf("CanonicalAgentConfigurationDigest(repeated) error = %v", err)
	}
	if repeated != baseDigest {
		t.Fatalf("canonical Agent configuration digest is not deterministic: %q != %q", repeated, baseDigest)
	}

	for name, mutate := range map[string]func(*AgentSessionConfiguration){
		"agent UID":        func(configuration *AgentSessionConfiguration) { configuration.AgentUID = "agent-uid-2" },
		"agent generation": func(configuration *AgentSessionConfiguration) { configuration.AgentGeneration++ },
		"provider": func(configuration *AgentSessionConfiguration) {
			configuration.ProviderKind = providerKindClaude
		},
		"model":            func(configuration *AgentSessionConfiguration) { configuration.Model = alternateTestModel },
		"max turns":        func(configuration *AgentSessionConfiguration) { configuration.MaxTurns++ },
		"reasoning effort": func(configuration *AgentSessionConfiguration) { configuration.ReasoningEffort = reasoningEffortMedium },
		"system prompt":    func(configuration *AgentSessionConfiguration) { configuration.SystemPrompt += " Be exact." },
	} {
		t.Run(name, func(t *testing.T) {
			changed := base
			mutate(&changed)
			digest, err := CanonicalAgentConfigurationDigest(changed)
			if err != nil {
				t.Fatalf("CanonicalAgentConfigurationDigest(changed) error = %v", err)
			}
			if digest == baseDigest {
				t.Fatalf("Agent configuration digest did not change with %s", name)
			}
		})
	}
}

func TestCanonicalPromptSettlementDigest(t *testing.T) {
	settlement := PromptSettlement{TerminalEvent: EventCompleted, Outcome: PromptOutcomeSucceeded, StopReason: ACPStopReasonEndTurn, SettledAt: testNow}
	first, err := CanonicalPromptSettlementDigest(settlement)
	if err != nil {
		t.Fatal(err)
	}
	second, err := CanonicalPromptSettlementDigest(settlement)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || first == "" {
		t.Fatalf("settlement digest mismatch: %q %q", first, second)
	}
	settlement.SettledAt = settlement.SettledAt.Add(time.Second)
	changed, err := CanonicalPromptSettlementDigest(settlement)
	if err != nil {
		t.Fatal(err)
	}
	if changed == first {
		t.Fatal("settlement digest did not change")
	}
}

func TestCanonicalRequestDigestKeepsAgentConfigurationWireCompatible(t *testing.T) {
	withoutConfiguration := []byte(`{"protocol":"orka.harness.v2","metadata":{"requestDigest":"sha256:placeholder"},"runtimeSessionID":"session"}`)
	withConfiguration := []byte(`{"protocol":"orka.harness.v2","metadata":{"requestDigest":"sha256:placeholder"},"runtimeSessionID":"session","agentConfiguration":{"agentUID":"agent","agentGeneration":1,"providerKind":"codex","model":"model","maxTurns":5}}`)
	oldDigest, err := CanonicalRequestDigestJSON(withoutConfiguration)
	if err != nil {
		t.Fatal(err)
	}
	newDigest, err := CanonicalRequestDigestJSON(withConfiguration)
	if err != nil {
		t.Fatal(err)
	}
	if oldDigest != newDigest {
		t.Fatalf("agentConfiguration changed v1 request digest: old=%q new=%q", oldDigest, newDigest)
	}
}

func TestAgentSessionConfigurationAcceptsLegacyDigestOnlyForLegacyFields(t *testing.T) {
	configuration := AgentSessionConfiguration{
		AgentUID: "agent-uid", AgentGeneration: 3, ProviderKind: providerKindCodex,
		Model: "model", MaxTurns: 12,
	}
	legacyDigest, err := CanonicalLegacyAgentConfigurationDigest(configuration, false)
	if err != nil {
		t.Fatal(err)
	}
	profile := testRuntimeProfile()
	profile.ProviderKind = configuration.ProviderKind
	profile.Model = configuration.Model
	profile.AgentConfigurationDigest = legacyDigest
	if err := configuration.ValidateProfileOrLegacy(profile, false); err != nil {
		t.Fatalf("legacy configuration rejected: %v", err)
	}
	configuration.SystemPrompt = "new unbound instructions"
	if err := configuration.ValidateProfileOrLegacy(profile, false); err == nil {
		t.Fatal("legacy digest accepted an unbound system prompt")
	}
}

func TestLegacyAgentConfigurationCannotBypassProviderOrModelFence(t *testing.T) {
	configuration := AgentSessionConfiguration{
		AgentUID: "agent-uid", AgentGeneration: 1, ProviderKind: providerKindCodex,
		Model: "model", MaxTurns: 5,
	}
	legacyDigest, err := CanonicalLegacyAgentConfigurationDigest(configuration, true)
	if err != nil {
		t.Fatal(err)
	}
	profile := testRuntimeProfile()
	profile.AgentConfigurationDigest = legacyDigest
	profile.ProviderKind = providerKindClaude
	if err := configuration.ValidateProfileOrLegacy(profile, true); err == nil || !strings.Contains(err.Error(), "provider kind") {
		t.Fatalf("legacy provider mismatch error = %v", err)
	}
	profile.ProviderKind = configuration.ProviderKind
	profile.Model = alternateTestModel
	if err := configuration.ValidateProfileOrLegacy(profile, true); err == nil || !strings.Contains(err.Error(), "model") {
		t.Fatalf("legacy model mismatch error = %v", err)
	}
}
