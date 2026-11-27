package wire

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/pkg/storage/v1/wire/storagepb"
)

func TestSplitListSharesTheItems(t *testing.T) {
	response := &storagepb.ListArtifactsResponse{}
	for index := range 20_000 {
		response.Artifacts = append(response.Artifacts, &storagepb.ArtifactMetadata{
			Filename: string(rune('a'+index%26)) + "-artifact", ContentType: "application/octet-stream", Size: int64(index),
		})
	}
	field := listFieldOf(response.ProtoReflect().Descriptor())

	parts := splitList(response, field)
	var joined []*storagepb.ArtifactMetadata
	for _, part := range parts {
		joined = append(joined, part.(*storagepb.ListArtifactsResponse).Artifacts...)
	}
	require.Len(t, joined, len(response.Artifacts))
	for index, item := range joined {
		require.Same(t, response.Artifacts[index], item, "item %d is the response's own", index)
	}

	allocs := testing.AllocsPerRun(5, func() { _ = splitList(response, field) })
	require.Less(t, allocs, 1.5*float64(len(response.Artifacts)),
		"splitting never copies the items")
}
