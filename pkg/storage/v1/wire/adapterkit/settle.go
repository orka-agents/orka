package adapterkit

import (
	"context"
	"time"

	"google.golang.org/grpc"
)

type settleTimeoutKey struct{}

// SettleContext returns a context for finishing a commit the provider has
// already sent when the caller's context may already be done.
//
// When a client cancels a call or its deadline passes, gRPC cancels the
// handler's context. A provider must not abandon a commit it has already sent
// because of that: the client then reports an unknown outcome and will retry
// the same operation or resolve it, which needs the commit, and the receipt
// it records, to settle one way or the other. SettleContext keeps ctx's
// values without its cancellation and bounds it by the adapter's settle
// timeout (Config.SettleTimeout, DefaultSettleTimeout outside Serve). Use it
// only for the commit itself and the work that settles it, never to start
// new work for a caller that has gone.
func SettleContext(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout, ok := ctx.Value(settleTimeoutKey{}).(time.Duration)
	if !ok || timeout <= 0 {
		timeout = DefaultSettleTimeout
	}
	return context.WithTimeout(context.WithoutCancel(ctx), timeout)
}

type settleInterceptorPair struct {
	unary  grpc.UnaryServerInterceptor
	stream grpc.StreamServerInterceptor
}

// settleInterceptors attach the settle timeout to every call's context.
func settleInterceptors(timeout time.Duration) settleInterceptorPair {
	return settleInterceptorPair{
		unary: func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
			return handler(context.WithValue(ctx, settleTimeoutKey{}, timeout), req)
		},
		stream: func(srv any, stream grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
			return handler(srv, &settleStream{ServerStream: stream, ctx: context.WithValue(stream.Context(), settleTimeoutKey{}, timeout)})
		},
	}
}

type settleStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *settleStream) Context() context.Context { return s.ctx }
