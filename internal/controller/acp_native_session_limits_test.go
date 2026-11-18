package controller

import (
	"testing"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/stretchr/testify/require"
)

func TestExternalNativeSessionCapabilityNegotiatedAndFrozen(t *testing.T) {
	fixture := newExternalACPDispatchFixture(t)
	client, err := harnessv2.NewClient(fixture.runtime.Spec.Deployment.Endpoint)
	require.NoError(t, err)
	capabilities, err := client.Capabilities(fixture.ctx)
	require.NoError(t, err)
	capabilities.Limits.MaxNativeSessionBytes = 16 << 20
	_, limits, err := validateExternalRuntimeCapabilities(fixture.runtime, capabilities, false)
	require.NoError(t, err)
	require.Equal(t, 16<<20, limits.EffectiveMaxNativeSessionBytes())
	frozen, err := canonicalExternalRuntimeCapabilities(capabilities)
	require.NoError(t, err)
	require.NoError(t, validateFrozenExternalRuntimeCapabilities(frozen, capabilities))
	capabilities.Limits.MaxNativeSessionBytes = 32 << 20
	require.Error(t, validateFrozenExternalRuntimeCapabilities(frozen, capabilities), "native capability drift must invalidate frozen authority")
	capabilities.Limits.MaxRequestBytes++
	_, _, err = validateExternalRuntimeCapabilities(fixture.runtime, capabilities, false)
	require.Error(t, err, "ordinary registered limits must remain exact")
}
