package workspace

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	ateapipb "github.com/orka-agents/orka/internal/substratepb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestSubstrateNativeRPCDeadlinesReachProvider(t *testing.T) {
	observed := make(chan time.Duration, 1)
	cfg, _, _, _ := substrateTransportFixture(t, grpc.UnaryInterceptor(func(ctx context.Context, _ any, info *grpc.UnaryServerInfo, _ grpc.UnaryHandler) (any, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			return nil, status.Error(codes.FailedPrecondition, "provider requires a deadline")
		}
		observed <- time.Until(deadline)
		switch info.FullMethod {
		case ateapipb.Control_ResumeActor_FullMethodName:
			return &ateapipb.ResumeActorResponse{}, nil
		case ateapipb.Control_SuspendActor_FullMethodName:
			return &ateapipb.SuspendActorResponse{}, nil
		case ateapipb.Control_CreateTag_FullMethodName, ateapipb.Control_DeleteTag_FullMethodName, ateapipb.Control_GetTag_FullMethodName:
			return &ateapipb.Tag{}, nil
		default:
			return &ateapipb.Actor{}, nil
		}
	}))
	api, err := NewSubstrateNativeClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close() //nolint:errcheck
	for _, tc := range []struct {
		name   string
		method string
		parent time.Duration
		want   time.Duration
	}{
		{"cold boot respects readiness window", "resume", 3 * time.Minute, 3 * time.Minute},
		{"suspension respects operation window", "suspend", 2 * time.Minute, 2 * time.Minute},
		{"snapshot tag creation respects operation window", "createTag", 2 * time.Minute, 2 * time.Minute},
		{"cold boot has bounded fallback", "resume", 0, 5 * time.Minute},
		{"suspension has bounded fallback", "suspend", 0, 5 * time.Minute},
		{"snapshot tag creation has bounded fallback", "createTag", 0, 5 * time.Minute},
		{"snapshot tag deletion has bounded fallback", "deleteTag", 0, 5 * time.Minute},
		{"deletion has bounded fallback", "delete", 0, 5 * time.Minute},
		{"short caller deadline wins", "resume", 5 * time.Second, 5 * time.Second},
		{"snapshot tag creation respects short deadline", "createTag", 5 * time.Second, 5 * time.Second},
		{"snapshot tag deletion respects short deadline", "deleteTag", 5 * time.Second, 5 * time.Second},
		{"metadata reads stay bounded", "read", 3 * time.Minute, 30 * time.Second},
		{"snapshot tag reads stay bounded", "readTag", 3 * time.Minute, 30 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			if tc.parent != 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tc.parent)
				defer cancel()
			}
			ref := &ateapipb.ObjectRef{Atespace: "tenant", Name: "same"}
			switch tc.method {
			case "resume":
				_, err = api.Control.ResumeActor(ctx, &ateapipb.ResumeActorRequest{Actor: ref, Boot: true})
			case "suspend":
				_, err = api.Control.SuspendActor(ctx, &ateapipb.SuspendActorRequest{Actor: ref})
			case "delete":
				_, err = api.Control.DeleteActor(ctx, &ateapipb.DeleteActorRequest{Actor: ref, AnyState: true})
			case "createTag":
				_, err = api.Control.CreateTag(ctx, &ateapipb.CreateTagRequest{Tag: &ateapipb.Tag{
					Metadata:    &ateapipb.ResourceMetadata{Atespace: ref.Atespace, Name: "checkpoint"},
					SourceActor: ref,
					Scope:       ateapipb.TagScope_TAG_SCOPE_ATESPACE,
				}})
			case "deleteTag":
				_, err = api.Control.DeleteTag(ctx, &ateapipb.DeleteTagRequest{Tag: ref})
			case "readTag":
				_, err = api.Control.GetTag(ctx, &ateapipb.GetTagRequest{Tag: ref})
			default:
				_, err = api.Control.GetActor(ctx, &ateapipb.GetActorRequest{Actor: ref})
			}
			if err != nil {
				t.Fatal(err)
			}
			if remaining := <-observed; remaining <= tc.want-min(5*time.Second, tc.want/2) || remaining > tc.want {
				t.Fatalf("provider received %v remaining; want at most %v with no hidden shorter cap", remaining, tc.want)
			}
		})
	}
}

func TestSubstrateNativeLifecycleCancellationDoesNotReplay(t *testing.T) {
	started := make(chan struct{}, 1)
	var calls atomic.Int32
	cfg, _, _, _ := substrateTransportFixture(t, grpc.UnaryInterceptor(func(ctx context.Context, _ any, _ *grpc.UnaryServerInfo, _ grpc.UnaryHandler) (any, error) {
		calls.Add(1)
		started <- struct{}{}
		<-ctx.Done()
		return nil, status.FromContextError(ctx.Err()).Err()
	}))
	api, err := NewSubstrateNativeClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close() //nolint:errcheck
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, callErr := api.Control.ResumeActor(ctx, &ateapipb.ResumeActorRequest{Actor: &ateapipb.ObjectRef{Atespace: "tenant", Name: "same"}, Boot: true})
		result <- callErr
	}()
	select {
	case <-started:
		cancel()
	case <-ctx.Done():
		t.Fatal("provider did not receive the lifecycle request")
	}
	if err := <-result; status.Code(err) != codes.Canceled {
		t.Fatalf("canceled lifecycle returned %v", err)
	}
	if calls.Load() != 1 {
		t.Fatal("canceled native mutation was replayed")
	}
}
