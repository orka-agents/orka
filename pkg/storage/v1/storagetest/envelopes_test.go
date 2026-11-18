package storagetest

import (
	"context"
	"testing"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

// combinedEnvelopeProbe serves both contracts and records the artifact write
// metadata; all unused methods come from the embedded interfaces.
type combinedEnvelopeProbe struct {
	storagev1.StateProvider
	storagev1.ArtifactStore
	envelope    storagev1.WriteEnvelope
	hasEnvelope bool
	request     storagev1.SaveArtifactRequest
}

func (*combinedEnvelopeProbe) Describe(context.Context) (storagev1.ProviderInfo, error) {
	return storagev1.ProviderInfo{DatasetID: "combined-dataset"}, nil
}

func (*combinedEnvelopeProbe) GetControllerEpochMirror(context.Context, string) (storagev1.ControllerEpochMirrorState, error) {
	return storagev1.ControllerEpochMirrorState{Epoch: mirroredEpoch(suiteFence.Epoch, suiteFence.HolderID)}, nil
}

func (p *combinedEnvelopeProbe) SaveArtifact(ctx context.Context, request storagev1.SaveArtifactRequest) error {
	p.envelope, p.hasEnvelope = storagev1.WriteEnvelopeFrom(ctx)
	p.request = request
	return nil
}

func TestWrapStatePreservesArtifactWritesWithEnvelopes(t *testing.T) {
	provider := &combinedEnvelopeProbe{}
	wrapped := wrapState(provider)
	artifacts, ok := wrapped.(storagev1.ArtifactStore)
	require.True(t, ok, "a combined provider must run the shared artifact-fence case")
	request := storagev1.SaveArtifactRequest{
		Owner:    storagev1.ArtifactOwner{Namespace: "tenant", TaskName: "task"},
		Filename: "out.txt", Data: []byte("output"), ContentType: "text/plain",
	}
	require.NoError(t, artifacts.SaveArtifact(t.Context(), request))
	require.True(t, provider.hasEnvelope, "artifact forwarding must attach an envelope")
	require.Equal(t, request, provider.request)
	digest, err := storagev1.RequestDigest("SaveArtifact", request)
	require.NoError(t, err)
	require.Equal(t, digest, provider.envelope.RequestDigest)
	require.Equal(t, "combined-dataset", provider.envelope.DatasetID)
	require.NotEmpty(t, provider.envelope.OperationID)

	_, ok = wrapState(struct{ storagev1.StateProvider }{}).(storagev1.ArtifactStore)
	require.False(t, ok, "a state-only provider must not advertise artifacts")
}
