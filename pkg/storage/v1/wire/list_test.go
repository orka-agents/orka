package wire_test

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/orka-agents/orka/pkg/storage/v1/wire"
	"github.com/orka-agents/orka/pkg/storage/v1/wire/storagepb"
)

// artifactList returns n artifact metadata entries in filename order, each
// padded to encode to about 1 KiB.
func artifactList(n int) []storagev1.ArtifactMetadata {
	pad := strings.Repeat("x", 960)
	list := make([]storagev1.ArtifactMetadata, n)
	for index := range list {
		list[index] = storagev1.ArtifactMetadata{
			Filename:    fmt.Sprintf("file-%06d.bin", index),
			ContentType: "application/x-" + pad,
			Size:        int64(index),
			CreatedAt:   time.Unix(1_700_000_000+int64(index), 0).UTC(),
		}
	}
	return list
}

// A Task's artifacts are not bounded in number, so ListArtifacts streams its
// list in parts of at most MaxListPartBytes. A list larger than the unary
// message limit crosses whole and in order.
func TestListsStreamPastOneMessage(t *testing.T) {
	provider := newRecordingProvider()
	server, err := wire.NewServer(serveBoth(provider))
	require.NoError(t, err)
	counter := &chunkCounter{}
	client := wire.NewClient(dialInsecure(t, serveInsecureWith(t, server, grpc.StatsHandler(counter))), boundConfig)

	list := artifactList(20_000)
	provider.set("ListArtifacts", []any{list}, nil)
	got, err := client.ListArtifacts(t.Context(), storagev1.ArtifactOwner{Namespace: "n", TaskName: "t", TaskUID: "uid"})
	require.NoError(t, err)
	require.Equal(t, list, got, "the list crosses whole and in order")
	require.Greater(t, counter.messages.Load(), int64(wire.MaxMessageBytes/wire.MaxListPartBytes),
		"a list over the message limit crosses in several messages")
	require.LessOrEqual(t, counter.largest.Load(), int64(wire.MaxListPartBytes))

	provider.set("ListArtifacts", []any{[]storagev1.ArtifactMetadata(nil)}, nil)
	got, err = client.ListArtifacts(t.Context(), storagev1.ArtifactOwner{Namespace: "n", TaskName: "t", TaskUID: "uid"})
	require.NoError(t, err)
	require.Empty(t, got, "an empty list crosses as one message")
}

// rawArtifacts serves ListArtifacts with a test's own framing.
type rawArtifacts struct {
	storagepb.UnimplementedArtifactsServer
	serve func(grpc.ServerStreamingServer[storagepb.ListArtifactsResponse]) error
}

func (a rawArtifacts) ListArtifacts(_ *storagepb.ListArtifactsRequest, stream grpc.ServerStreamingServer[storagepb.ListArtifactsResponse]) error {
	return a.serve(stream)
}

func wireArtifacts(names ...string) []*storagepb.ArtifactMetadata {
	items := make([]*storagepb.ArtifactMetadata, len(names))
	for index, name := range names {
		items[index] = &storagepb.ArtifactMetadata{Filename: name, ContentType: "text/plain",
			CreatedAt: timestamppb.New(time.Unix(1_700_000_000, 0))}
	}
	return items
}

// A list stream that fails, ends short, carries more than it declared or
// breaks the framing fails the call; the client never returns a partial
// list.
func TestListStreamsFailClosed(t *testing.T) {
	part := func(names ...string) *storagepb.ListArtifactsResponse {
		return &storagepb.ListArtifactsResponse{Artifacts: wireArtifacts(names...)}
	}
	for _, tc := range []struct {
		name  string
		count string
		parts []*storagepb.ListArtifactsResponse
		end   error
		want  error
	}{
		{"fails mid-stream", "3", []*storagepb.ListArtifactsResponse{part("a")}, status.Error(codes.Internal, "provider gone"), nil},
		{"fails after its last part", "1", []*storagepb.ListArtifactsResponse{part("a")}, status.Error(codes.Internal, "provider gone"), nil},
		{"ends short", "3", []*storagepb.ListArtifactsResponse{part("a"), part("b")}, nil, storagev1.ErrIncompatible},
		{"more than declared", "1", []*storagepb.ListArtifactsResponse{part("a"), part("b")}, nil, storagev1.ErrIncompatible},
		{"an empty later part", "2", []*storagepb.ListArtifactsResponse{part("a"), part(), part("b")}, nil, storagev1.ErrIncompatible},
		{"no count", "", []*storagepb.ListArtifactsResponse{part("a")}, nil, storagev1.ErrIncompatible},
		{"invalid count", "-1", []*storagepb.ListArtifactsResponse{part()}, nil, storagev1.ErrIncompatible},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := rawListClient(t, func(stream grpc.ServerStreamingServer[storagepb.ListArtifactsResponse]) error {
				if err := stream.SetHeader(listHeader(tc.count)); err != nil {
					return err
				}
				for _, message := range tc.parts {
					if err := stream.Send(message); err != nil {
						return err
					}
				}
				return tc.end
			})
			got, err := client.ListArtifacts(t.Context(), storagev1.ArtifactOwner{Namespace: "n", TaskName: "t", TaskUID: "uid"})
			require.Error(t, err)
			require.Nil(t, got, "no partial list")
			if tc.want != nil {
				require.ErrorIs(t, err, tc.want)
			}
		})
	}

	client := rawListClient(t, func(stream grpc.ServerStreamingServer[storagepb.ListArtifactsResponse]) error {
		if err := stream.SetHeader(listHeader("3")); err != nil {
			return err
		}
		for _, message := range []*storagepb.ListArtifactsResponse{part("a"), part("b", "c")} {
			if err := stream.Send(message); err != nil {
				return err
			}
		}
		return nil
	})
	got, err := client.ListArtifacts(t.Context(), storagev1.ArtifactOwner{Namespace: "n", TaskName: "t", TaskUID: "uid"})
	require.NoError(t, err)
	names := make([]string, len(got))
	for index, item := range got {
		names[index] = item.Filename
	}
	require.Equal(t, []string{"a", "b", "c"}, names, "parts are joined in order")
}

// The client stops reading a list stream as soon as it carries more items
// than it declared, rather than buffering whatever else the server sends.
func TestListStreamsStopAtTheDeclaredCount(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	client := rawListClient(t, func(stream grpc.ServerStreamingServer[storagepb.ListArtifactsResponse]) error {
		if err := stream.SetHeader(listHeader("1")); err != nil {
			return err
		}
		for _, name := range []string{"a", "b"} {
			if err := stream.Send(&storagepb.ListArtifactsResponse{Artifacts: wireArtifacts(name)}); err != nil {
				return err
			}
		}
		select {
		case <-release:
		case <-stream.Context().Done():
		}
		return nil
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, err := client.ListArtifacts(ctx, storagev1.ArtifactOwner{Namespace: "n", TaskName: "t", TaskUID: "uid"})
	require.ErrorIs(t, err, storagev1.ErrIncompatible, "the excess item ends the call before the stream does")
	require.NoError(t, ctx.Err())
}

// listHeader names the contract and test dataset, and declares count unless
// it is empty.
func listHeader(count string) metadata.MD {
	header := metadata.Pairs(wire.HeaderContract, storagev1.ContractVersion, wire.HeaderRevision,
		strconv.Itoa(wire.ProtocolRevision), wire.HeaderDataset, testDataset)
	if count != "" {
		header.Set(wire.HeaderListCount, count)
	}
	return header
}

// rawListClient returns a client of a server whose ListArtifacts runs serve.
func rawListClient(t *testing.T, serve func(grpc.ServerStreamingServer[storagepb.ListArtifactsResponse]) error) *wire.Client {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := grpc.NewServer()
	storagepb.RegisterArtifactsServer(server, rawArtifacts{serve: serve})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	return wire.NewClient(dialInsecure(t, listener.Addr().String()), boundConfig)
}
