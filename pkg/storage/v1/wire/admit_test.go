package wire

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/metadata"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

// describeOnly is an artifacts provider that only describes its dataset.
type describeOnly struct {
	storagev1.ArtifactProvider
	dataset string
}

func (p describeOnly) Describe(context.Context) (storagev1.ProviderInfo, error) {
	return storagev1.ProviderInfo{DatasetID: p.dataset, Contracts: []string{storagev1.ArtifactsContract},
		Limits: storagev1.ProviderLimits{MaxArtifactBytes: storagev1.MaxArtifactBytes}}, nil
}

// An admitted write answers for the dataset its admission checked. A
// description that clears the serving dataset after the admission, but
// before the response header is set, does not take the dataset out of a
// response whose write may already have committed.
func TestAdmittedCallsAnswerForTheDatasetTheyWereCheckedAgainst(t *testing.T) {
	server, err := NewServer(ServerConfig{Artifacts: describeOnly{dataset: "dataset-a"}})
	require.NoError(t, err)
	require.NoError(t, server.Refresh(t.Context()))
	ctx := metadata.NewIncomingContext(t.Context(), metadata.Pairs(HeaderContract, storagev1.ContractVersion,
		HeaderRevision, "2", HeaderDataset, "dataset-a"))

	_, release, served, err := server.admit(ctx, &methodDeleteArtifacts)
	require.NoError(t, err)
	defer release()
	server.publish(ctx, server.describeSeq.Add(1), "", 0, "the providers disagree")
	require.Empty(t, server.DatasetID())
	require.Equal(t, []string{"dataset-a"}, server.responseHeader(served).Get(HeaderDataset))
	require.Empty(t, server.responseHeader("").Get(HeaderDataset), "a refused call names the live dataset")
}
