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
// DialOptions installs it. On a connection without it, every transport
// failure of a write is an unknown outcome; on one without Dial's coverage,
// so is a write that failed before any attempt.
type EvidenceHandler struct{}

// NewEvidenceHandler returns the send-evidence handler.
func NewEvidenceHandler() *EvidenceHandler { return &EvidenceHandler{} }

// TagRPC notes that an attempt started for a Client call.
func (*EvidenceHandler) TagRPC(ctx context.Context, _ *stats.RPCTagInfo) context.Context {
	if evidence := sendEvidenceFrom(ctx); evidence != nil {
		evidence.attempted.Store(true)
	}
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

// sendEvidence is one call's record. covered shows the call reached the
// innermost interceptor of a connection Dial built, with nothing left between
// it and grpc-go, so the handler sees every attempt; attempted shows the
// handler saw an attempt. Either makes the absence of sent evidence rather
// than a missing handler: grpc-go tags every attempt before it can create a
// stream, so a covered call that never reached an attempt (a closed
// connection, or a deadline spent waiting for name resolution) sent nothing.
// A caller interceptor that sends the call on a context without this record
// leaves it neither covered nor attempted, so the call stays possibly sent.
type sendEvidence struct {
	covered   atomic.Bool
	attempted atomic.Bool
	sent      atomic.Bool
}

// possiblySent reports whether the request may have left the process.
func (e *sendEvidence) possiblySent() bool {
	return e == nil || e.sent.Load() || (!e.covered.Load() && !e.attempted.Load())
}

// coverEvidence returns the unary client interceptor Dial installs last, so
// it is innermost. It marks a call as covered just before grpc-go starts it,
// and only when the call runs on the connection Dial built, whose handler
// sees every attempt; a call an outer interceptor moved to another connection
// stays uncovered.
func coverEvidence(own *atomic.Pointer[grpc.ClientConn]) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn,
		invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		if evidence := sendEvidenceFrom(ctx); evidence != nil && cc != nil && cc == own.Load() {
			evidence.covered.Store(true)
		}
		return invoker(ctx, method, req, reply, cc, opts...)
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
