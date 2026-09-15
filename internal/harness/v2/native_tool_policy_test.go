package v2

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNativeToolPolicyDigestPreservesLegacyAndBindsMode(t *testing.T) {
	allowed := []string{"Read"}
	legacy, err := canonicalACPDomainDigest("tool-policy", map[string]any{
		"allowed": allowed, "disallowed": []string(nil), "allowBash": false,
	})
	require.NoError(t, err)
	omitted, err := CanonicalRuntimeToolPolicyDigest(allowed, nil, false)
	require.NoError(t, err)
	empty, err := CanonicalRuntimeToolPolicyDigest(allowed, nil, false, "")
	require.NoError(t, err)
	require.Equal(t, legacy, omitted)
	require.Equal(t, legacy, empty)
	restricted, err := CanonicalRuntimeToolPolicyDigest(allowed, nil, false, NativeToolPolicyRestricted)
	require.NoError(t, err)
	require.NotEqual(t, legacy, restricted)
	full, err := CanonicalRuntimeToolPolicyDigest(nil, nil, true, NativeToolPolicyFull)
	require.NoError(t, err)
	oldFull, err := CanonicalRuntimeToolPolicyDigest(nil, nil, true)
	require.NoError(t, err)
	require.NotEqual(t, full, oldFull)
	_, err = CanonicalRuntimeToolPolicyDigest(nil, nil, true, "invalid")
	require.Error(t, err)
}

func TestFullNativeToolPolicyStillRequiresPermissionSupport(t *testing.T) {
	require.True(t, MCPPolicyRequiresPermissionCapability(MCPToolPolicy{NativeToolPolicy: NativeToolPolicyFull}, MCPApprovalPolicy{}))
	policy := MCPToolPolicy{NativeToolPolicy: NativeToolPolicyFull, AllowBash: true}
	require.False(t, policy.Allows("Read"))
	require.False(t, policy.Allows("delegate_task"))
}
