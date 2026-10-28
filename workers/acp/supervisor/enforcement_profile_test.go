package supervisor

import (
	"testing"

	"github.com/orka-agents/orka/internal/acp"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

func TestLoadConfigEnforcementRequirementDigest(t *testing.T) {
	setLoadConfigEnv(t)
	t.Setenv(EnvEnforcementRequirementDigest, "")
	ordinary, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	requirementDigest := testDigest("requirement")
	t.Setenv(EnvEnforcementRequirementDigest, requirementDigest)
	protected, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	profile := harnessv2.RuntimeProfile{
		ACPProfile: harnessv2.ACPProfileV1, AdapterDigests: acp.BuiltInRuntimeAdapterDigests(providerKindCodex),
		ProviderKind: providerKindCodex, Model: "gpt-test",
		AgentConfigurationDigest: testDigest("agent"), ToolPolicyDigest: testDigest("tool"),
		ApprovalPolicyDigest: testDigest("approval"), MCPConfigurationDigest: testDigest("mcp"),
		WorkspaceIntent: harnessv2.WorkspaceIntentRead, ProxyCredentialRole: "provider",
		ProxyCredentialScope: "model:gpt-test", ResourceClass: "standard",
		EnforcementRequirementDigest: requirementDigest,
	}
	digest, err := harnessv2.CanonicalProfileDigest(profile)
	if err != nil {
		t.Fatal(err)
	}
	if protected.Fence.RuntimeProfileDigest != digest || digest == ordinary.Fence.RuntimeProfileDigest {
		t.Fatal("supervisor did not preserve the controller's requirement-bound profile digest")
	}
	t.Setenv(EnvEnforcementRequirementDigest, "")
	restored, err := LoadConfigFromEnv()
	if err != nil || restored.Fence.RuntimeProfileDigest != ordinary.Fence.RuntimeProfileDigest {
		t.Fatalf("ordinary supervisor profile changed: %v", err)
	}
	t.Setenv(EnvEnforcementRequirementDigest, "invalid")
	if _, err := LoadConfigFromEnv(); err == nil {
		t.Fatal("supervisor accepted an invalid enforcement requirement digest")
	}
}
