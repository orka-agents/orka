package storagetest

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

func TestWriteCatalogsCoverEveryContractWrite(t *testing.T) {
	stateUncovered, err := uncoveredWrites(storagev1.StateContract, stateWriteCases)
	require.NoError(t, err)
	artifactUncovered, err := uncoveredWrites(storagev1.ArtifactsContract, artifactWriteCases)
	require.NoError(t, err)
	require.Empty(t, artifactUncovered, "every artifacts.v1 write needs a write case")
	deferred := slices.Sorted(maps.Keys(deferredStateWrites))
	require.Equal(t, deferred, stateUncovered,
		"every state.v1 write needs a write case in pkg/storage/v1/storagetest, or a reason in deferredStateWrites")
	for method, reason := range deferredStateWrites {
		info, ok := storagev1.LookupMethod(method)
		require.Truef(t, ok && info.Kind == storagev1.MethodWrite, "deferred %s is not a write", method)
		require.NotEmptyf(t, reason, "deferred %s needs a reason", method)
	}
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

func (r *recordingResults) SaveMessage(ctx context.Context, namespace string, message *storagev1.Message) (string, error) {
	envelope, ok := storagev1.WriteEnvelopeFrom(ctx)
	if !ok {
		return "", storagev1.ValidationErrorf("write envelope is required")
	}
	r.envelopes = append(r.envelopes, envelope)
	message.ID = int64(len(r.envelopes))
	r.results[namespace] = message.Content
	return "saved", nil
}

func TestCallWriteCarriesTheEnvelopeAndReportsWrittenArguments(t *testing.T) {
	results := &recordingResults{results: map[string]string{}}
	c := writeCase[*recordingResults]{
		method: "SaveMessage",
		args: func(storagev1.ControllerEpochFence) []any {
			return []any{"tenant", &storagev1.Message{Content: "hello"}}
		},
	}
	fence := storagev1.ControllerEpochFence{Name: storagev1.DefaultControllerEpochName, Epoch: 1, HolderID: "holder"}
	envelope := envelopeFor(t, "dataset-1", c, fence)
	digest, err := storagev1.RequestDigest("SaveMessage", "tenant", &storagev1.Message{Content: "hello"})
	require.NoError(t, err)
	require.Equal(t, digest, envelope.RequestDigest, "the envelope digests the arguments before the call")
	outcome := callWrite(t.Context(), t, results, c, envelope, fence)
	require.NoError(t, outcome.err)
	require.Equal(t, []any{"saved"}, outcome.returned)
	require.Equal(t, int64(1), outcome.args[1].(*storagev1.Message).ID, "the outcome keeps what the call wrote into its arguments")
	require.Equal(t, []storagev1.WriteEnvelope{envelope}, results.envelopes)

	replay := callWrite(t.Context(), t, results, c, envelope, fence)
	require.NoError(t, replay.err)
	require.Panics(t, func() { requireSameOutcome(&panickingT{t}, outcome, replay) }, "a different written ID is a different outcome")
}

// panickingT turns a failed assertion into a panic, so a test can check that
// an assertion helper fails.
type panickingT struct{ *testing.T }

func (p *panickingT) Errorf(format string, args ...any) { panic(fmt.Sprintf(format, args...)) }
func (p *panickingT) FailNow()                          { panic("FailNow") }

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
