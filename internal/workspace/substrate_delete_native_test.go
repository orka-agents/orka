package workspace

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	ateapipb "github.com/orka-agents/orka/internal/substratepb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestSubstrateNativeDeleteDoesNotRequireSnapshot(t *testing.T) {
	for _, state := range []ateapipb.ActorState{
		ateapipb.ActorState_ACTOR_STATE_RUNNING,
		ateapipb.ActorState_ACTOR_STATE_SUSPENDING,
		ateapipb.ActorState_ACTOR_STATE_CRASHED,
		ateapipb.ActorState_ACTOR_STATE_SUSPENDED,
	} {
		t.Run(state.String(), func(t *testing.T) {
			var suspends, deletes atomic.Int32
			cfg, _, _, _ := substrateTransportFixture(t, grpc.UnaryInterceptor(func(
				ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler,
			) (any, error) {
				switch info.FullMethod {
				case ateapipb.Control_GetActor_FullMethodName:
					return &ateapipb.Actor{
						Metadata: &ateapipb.ResourceMetadata{Atespace: "tenant", Name: "stateless", Uid: "actor-uid"},
						Status:   &ateapipb.ActorStatus{State: state},
					}, nil
				case ateapipb.Control_SuspendActor_FullMethodName:
					suspends.Add(1)
					// Upstream gVisor rejects Data capture without durable mounts.
					return nil, status.Error(codes.FailedPrecondition, "no durable-dir volumes found for DATA snapshot")
				case ateapipb.Control_DeleteActor_FullMethodName:
					deletes.Add(1)
					request := req.(*ateapipb.DeleteActorRequest)
					if !request.AnyState || request.Actor.Atespace != "tenant" || request.Actor.Name != "stateless" {
						return nil, status.Error(codes.InvalidArgument, "expected native any-state deletion of the exact Actor")
					}
					return &ateapipb.Actor{}, nil
				default:
					return handler(ctx, req)
				}
			}))
			cfg.RouterURL, cfg.ActorDNSSuffix = "http://router.test", "actors.test"
			executor, err := NewSubstrateExecutor(cfg)
			require.NoError(t, err)
			defer executor.Close() //nolint:errcheck
			result, err := executor.Delete(t.Context(), DeleteRequest{
				Ref: WorkspaceRef{ID: "stateless.tenant"}, SkipScrub: true, Timeout: time.Second,
			})
			require.NoError(t, err)
			require.NotNil(t, result)
			require.True(t, result.Deleted)
			require.Equal(t, PhaseDeleted, result.Phase)
			require.Zero(t, suspends.Load(), "deletion must not depend on creating a snapshot")
			require.EqualValues(t, 1, deletes.Load())
		})
	}
}
