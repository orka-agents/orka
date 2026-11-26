package adapterkit

import (
	"context"
	"time"

	"google.golang.org/grpc"
)

type settleTimeoutKey struct{}

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
