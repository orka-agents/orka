package wire_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/orka-agents/orka/pkg/storage/v1/wire"
	"github.com/orka-agents/orka/pkg/storage/v1/wire/wiretest"
)

// A provider that panics while the server first describes it does not keep
// the call's admission slot.
func TestServerReleasesAdmissionWhenTheDescriptionPanics(t *testing.T) {
	provider := newRecordingProvider()
	provider.mu.Lock()
	provider.panics["Describe"] = "describe bug"
	provider.mu.Unlock()
	client := wire.NewClient(wiretest.Dial(t, wire.ServerConfig{State: provider, Limits: wire.Limits{
		MaxConcurrent: map[wire.Class]int{wire.ClassControl: 1},
	}}), boundConfig)

	require.Error(t, client.SavePlan(t.Context(), "n", "t", &storagev1.PlanState{}))
	require.Empty(t, provider.callsTo("SavePlan"))

	provider.mu.Lock()
	delete(provider.panics, "Describe")
	provider.mu.Unlock()
	require.NoError(t, client.SavePlan(t.Context(), "n", "t", &storagev1.PlanState{}), "the slot is free after the panic")
}

// describedProvider reports a fixed description.
type describedProvider struct {
	*recordingProvider
	info storagev1.ProviderInfo
}

func (p describedProvider) Describe(context.Context) (storagev1.ProviderInfo, error) {
	return p.info, nil
}

// A server with separate state and artifacts providers describes itself as a
// whole: both contract parts, and the artifacts provider's artifact limit.
func TestServerDescribesSeparateProvidersAsOne(t *testing.T) {
	stateInfo := describeDefault()
	stateInfo.Contracts = []string{storagev1.StateContract}
	stateInfo.Limits = storagev1.ProviderLimits{}
	artifactsInfo := describeDefault()
	artifactsInfo.Contracts = []string{storagev1.ArtifactsContract}
	artifactsInfo.Limits = storagev1.ProviderLimits{MaxArtifactBytes: 2 * storagev1.MaxArtifactBytes}
	conn := wiretest.Dial(t, wire.ServerConfig{
		State:     describedProvider{recordingProvider: newRecordingProvider(), info: stateInfo},
		Artifacts: describedProvider{recordingProvider: newRecordingProvider(), info: artifactsInfo},
	})
	info, _, err := wire.NewClient(conn, wire.ClientConfig{}).DescribeServer(t.Context())
	require.NoError(t, err)
	require.ElementsMatch(t, []string{storagev1.StateContract, storagev1.ArtifactsContract}, info.Contracts)
	require.Equal(t, int64(2*storagev1.MaxArtifactBytes), info.Limits.MaxArtifactBytes)
}
