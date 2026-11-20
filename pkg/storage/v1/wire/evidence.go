package wire

import (
	"context"
	"sync/atomic"

	"google.golang.org/grpc"
	"google.golang.org/grpc/stats"
)

// EvidenceHandler is a client stats.Handler that records, for each call a
// Client makes, whether any attempt reached the point where its request
// headers could leave this process. grpc-go reports that point as
// stats.OutHeader once a stream exists on a live transport, so a call that
// never reached it sent nothing. A header queued but not yet flushed counts
// as possibly sent: the classification can turn a harmless failure into an
// unknown outcome, never the reverse.
//
// The absence of that point is evidence only on a connection Dial built,
// which admits no caller interceptors that could send a call where the
// handler cannot see it. On any other connection, every transport failure of
// a write is an unknown outcome.
type EvidenceHandler struct{}

// NewEvidenceHandler returns the send-evidence handler.
func NewEvidenceHandler() *EvidenceHandler { return &EvidenceHandler{} }

// TagRPC implements stats.Handler.
func (*EvidenceHandler) TagRPC(ctx context.Context, _ *stats.RPCTagInfo) context.Context {
	return ctx
}

// HandleRPC notes that an attempt's request headers may have left the
// process.
func (*EvidenceHandler) HandleRPC(ctx context.Context, event stats.RPCStats) {
	if header, ok := event.(*stats.OutHeader); ok && header.IsClient() {
		if evidence := sendEvidenceFrom(ctx); evidence != nil {
			evidence.sent.Store(true)
		}
	}
}

// TagConn implements stats.Handler.
func (*EvidenceHandler) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context {
	return ctx
}

// HandleConn implements stats.Handler.
func (*EvidenceHandler) HandleConn(context.Context, stats.ConnStats) {}

// sendEvidence is one call's record. conn is the connection the Client
// calls, when it is a *grpc.ClientConn. covered shows the call reached the
// only interceptor of a connection Dial built, and that this connection is
// the Client's own: nothing can sit between the client and grpc-go there,
// every attempt of the call (transparent retries included) carries this
// record, and the handler sees each one. So a covered call without sent
// evidence sent nothing, including one that never reached an attempt (a
// closed connection, or a deadline spent waiting for name resolution). An
// uncovered call, such as one made through a wrapper connection, is always
// possibly sent.
type sendEvidence struct {
	conn    *grpc.ClientConn
	covered atomic.Bool
	sent    atomic.Bool
}

// possiblySent reports whether the request may have left the process.
func (e *sendEvidence) possiblySent() bool {
	return e == nil || e.sent.Load() || !e.covered.Load()
}

// coverEvidence returns the only unary client interceptor of a connection
// Dial builds. It marks a call as covered just before grpc-go starts it, and
// only when the call runs on that connection and the Client called it
// directly.
func coverEvidence(own *atomic.Pointer[grpc.ClientConn]) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn,
		invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		if evidence := sendEvidenceFrom(ctx); evidence != nil && cc != nil && cc == own.Load() && cc == evidence.conn {
			evidence.covered.Store(true)
		}
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

// coverStreamEvidence is coverEvidence for streaming calls.
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
