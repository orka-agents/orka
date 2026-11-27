package wire

import (
	"context"
	"sync/atomic"

	"google.golang.org/grpc"
	"google.golang.org/grpc/stats"
)

type EvidenceHandler struct{}

func NewEvidenceHandler() *EvidenceHandler { return &EvidenceHandler{} }

func (*EvidenceHandler) TagRPC(ctx context.Context, _ *stats.RPCTagInfo) context.Context {
	return ctx
}

func (*EvidenceHandler) HandleRPC(ctx context.Context, event stats.RPCStats) {
	if header, ok := event.(*stats.OutHeader); ok && header.IsClient() {
		if evidence := sendEvidenceFrom(ctx); evidence != nil {
			evidence.sent.Store(true)
		}
	}
}

func (*EvidenceHandler) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context {
	return ctx
}

func (*EvidenceHandler) HandleConn(context.Context, stats.ConnStats) {}

type sendEvidence struct {
	conn    *grpc.ClientConn
	covered atomic.Bool
	sent    atomic.Bool
}

func (e *sendEvidence) possiblySent() bool {
	return e == nil || e.sent.Load() || !e.covered.Load()
}

func coverEvidence(own *atomic.Pointer[grpc.ClientConn]) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn,
		invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		if evidence := sendEvidenceFrom(ctx); evidence != nil && cc != nil && cc == own.Load() && cc == evidence.conn {
			evidence.covered.Store(true)
		}
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

func coverStreamEvidence(own *atomic.Pointer[grpc.ClientConn]) grpc.StreamClientInterceptor {
	return func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string,
		streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		if evidence := sendEvidenceFrom(ctx); evidence != nil && cc != nil && cc == own.Load() && cc == evidence.conn {
			evidence.covered.Store(true)
		}
		return streamer(ctx, desc, cc, method, opts...)
	}
}

type sendEvidenceKey struct{}

func withSendEvidence(ctx context.Context, evidence *sendEvidence) context.Context {
	return context.WithValue(ctx, sendEvidenceKey{}, evidence)
}

func sendEvidenceFrom(ctx context.Context) *sendEvidence {
	evidence, _ := ctx.Value(sendEvidenceKey{}).(*sendEvidence)
	return evidence
}
