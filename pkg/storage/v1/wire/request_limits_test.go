package wire_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/orka-agents/orka/pkg/storage/v1/wire"
	"github.com/orka-agents/orka/pkg/storage/v1/wire/storagepb"
	"github.com/orka-agents/orka/pkg/storage/v1/wire/wiretest"
)

// A download stream opened without its request message is ended at the
// bulk duration limit, before its handler runs, and frees its slot.
func TestServerEndsDownloadsThatNeverSendTheirRequest(t *testing.T) {
	provider := newRecordingProvider()
	config := serveBoth(provider)
	config.Limits = bulkLimits(300 * time.Millisecond)
	conn := wiretest.Dial(t, config)

	silent, err := conn.NewStream(contractContext(t.Context()), &grpc.StreamDesc{ServerStreams: true},
		storagepb.Results_GetResult_FullMethodName)
	require.NoError(t, err)
	ended := make(chan error, 1)
	go func() { ended <- silent.RecvMsg(&storagepb.GetResultResponse{}) }()
	select {
	case err := <-ended:
		_, reason := reasonOf(t, err)
		require.Equal(t, wire.ReasonUnavailable, reason)
	case <-time.After(10 * time.Second):
		t.Fatal("the download that never sent its request was never ended")
	}
	require.Empty(t, provider.callsTo("GetResult"))

	provider.set("GetArtifact", []any{[]byte("small"), "text/plain"}, nil)
	_, _, err = wire.NewClient(conn, boundConfig).GetArtifact(t.Context(), storagev1.ArtifactKey{Filename: "f"})
	require.NoError(t, err, "the bulk slot is free again")
}

// A server describes the artifact limit it enforces, which its own blob
// limit can lower below the provider's.
func TestServerDescribesTheArtifactLimitItEnforces(t *testing.T) {
	provider := newRecordingProvider()
	client := wiretest.Serve(t, wire.ServerConfig{Artifacts: provider, MaxBlobBytes: wire.MaxChunkBytes})
	info, err := client.Describe(t.Context())
	require.NoError(t, err)
	require.Equal(t, int64(wire.MaxChunkBytes), info.Limits.MaxArtifactBytes)

	err = client.SaveArtifact(t.Context(), storagev1.SaveArtifactRequest{Filename: "f", Data: make([]byte, wire.MaxChunkBytes+1)})
	var tooLarge storagev1.TooLargeError
	require.ErrorAs(t, err, &tooLarge, "the client refuses what the server describes it would refuse")
	require.Empty(t, provider.callsTo("SaveArtifact"))
}
