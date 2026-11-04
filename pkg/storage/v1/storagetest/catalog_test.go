package storagetest

import (
	"context"
	"testing"
	"time"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

func TestWriteCatalogsListOnlyContractWrites(t *testing.T) {
	stateUncovered, err := uncoveredWrites(storagev1.StateContract, stateWriteCases)
	require.NoError(t, err)
	artifactUncovered, err := uncoveredWrites(storagev1.ArtifactsContract, artifactWriteCases)
	require.NoError(t, err)
	require.Contains(t, artifactUncovered, "SaveArtifact")
	require.NotContains(t, stateUncovered, "GetResult", "reads need no write case")
	require.NotContains(t, stateUncovered, "SyncControllerEpochMirror", "administrative methods need no write case")
	t.Logf("write cases still to add: %d state, %d artifacts", len(stateUncovered), len(artifactUncovered))
}

func TestUncoveredWritesRefusesMisplacedCases(t *testing.T) {
	type provider struct{}
	for name, cases := range map[string][]writeCase[provider]{
		"unknown method":  {{method: "SaveResults"}},
		"read method":     {{method: "GetResult"}},
		"admin method":    {{method: "ResolveOperation"}},
		"other contract":  {{method: "SaveArtifact"}},
		"listed twice":    {{method: "SaveResult"}, {method: "SaveResult"}},
		"shared contract": {{method: "Describe"}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := uncoveredWrites(storagev1.StateContract, cases)
			require.Error(t, err)
		})
	}
	uncovered, err := uncoveredWrites(storagev1.StateContract, []writeCase[provider]{{method: "SaveResult"}})
	require.NoError(t, err)
	require.NotContains(t, uncovered, "SaveResult")
	require.Contains(t, uncovered, "DeleteResult")
}

type recordingResults struct {
	results   map[string]string
	envelopes []storagev1.WriteEnvelope
}

func (r *recordingResults) Describe(context.Context) (storagev1.ProviderInfo, error) {
	return storagev1.ProviderInfo{DatasetID: "dataset-1"}, nil
}

func TestRunWriteCaseCarriesTheEnvelope(t *testing.T) {
	results := &recordingResults{results: map[string]string{}}
	c := writeCase[*recordingResults]{
		method: "SaveResult",
		setup:  func(_ *testing.T, p *recordingResults) { p.results["before"] = "kept" },
		call: func(ctx context.Context, p *recordingResults) (any, error) {
			envelope, ok := storagev1.WriteEnvelopeFrom(ctx)
			if !ok {
				return nil, storagev1.ValidationErrorf("write envelope is required")
			}
			p.envelopes = append(p.envelopes, envelope)
			p.results["task"] = "result"
			return "saved", nil
		},
		observe: func(_ *testing.T, p *recordingResults) any { return len(p.results) },
	}
	fence := storagev1.ControllerEpochFence{Name: storagev1.DefaultControllerEpochName, Epoch: 1, HolderID: "holder"}
	envelope := NewWriteEnvelope(t, "dataset-1", fence, "SaveResult", "tenant", "task", []byte("result"))
	outcome := runWriteCase(t, results, c, envelope)
	require.NoError(t, outcome.err)
	require.Equal(t, "saved", outcome.result)
	require.Equal(t, 2, outcome.observed)
	require.Equal(t, []storagev1.WriteEnvelope{envelope}, results.envelopes)
}

func TestNewWriteEnvelopeIssuesDistinctOrderedOperations(t *testing.T) {
	fence := storagev1.ControllerEpochFence{Name: storagev1.DefaultControllerEpochName, Epoch: 2, HolderID: "holder"}
	before := time.Now().UTC()
	first := NewWriteEnvelope(t, "dataset-1", fence, "SaveResult", "tenant", "task", []byte("result"))
	second := NewWriteEnvelope(t, "dataset-1", fence, "SaveResult", "tenant", "task", []byte("result"))

	digest, err := storagev1.RequestDigest("SaveResult", "tenant", "task", []byte("result"))
	require.NoError(t, err)
	require.Equal(t, digest, first.RequestDigest)
	require.Equal(t, first.RequestDigest, second.RequestDigest, "the same request has the same digest")
	require.NotEqual(t, first.OperationID, second.OperationID, "each call is a new operation")
	require.Greater(t, second.Sequence, first.Sequence)
	require.Equal(t, fence, first.Fence)
	require.Equal(t, "dataset-1", first.DatasetID)
	require.False(t, first.IssuedAt.Before(before))
	require.Equal(t, time.UTC, first.IssuedAt.Location())

	ctx := EnvelopeContext(t.Context(), t, &recordingResults{}, fence, "DeleteResult", "tenant", "task")
	attached, ok := storagev1.WriteEnvelopeFrom(ctx)
	require.True(t, ok)
	require.Equal(t, "dataset-1", attached.DatasetID, "the dataset comes from Describe")
	deleteDigest, err := storagev1.RequestDigest("DeleteResult", "tenant", "task")
	require.NoError(t, err)
	require.Equal(t, deleteDigest, attached.RequestDigest)
}
