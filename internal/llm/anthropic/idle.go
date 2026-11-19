/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package anthropic

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/anthropics/anthropic-sdk-go/option"
)

// streamIdleTimeout is how long Complete and Stream wait without receiving a
// byte before they abandon a stream. The API sends ping events while the model
// thinks, but a proxy may buffer a whole turn and send nothing until it ends.
// Ten minutes matches the SDK's timeout for the whole non-streaming request
// Complete used to send, so a turn that finished then still finishes now. A
// variable so tests can shorten it.
var streamIdleTimeout = 10 * time.Minute

// errStreamIdle cancels a stream that has gone quiet for streamIdleTimeout.
var errStreamIdle = errors.New("anthropic stream went idle")

// idleGuard cancels its context when the response body delivers no bytes for
// streamIdleTimeout. A non-streaming request has the SDK's request timeout,
// and a streaming request has none, so without it a stalled upstream would
// hold the request until the caller's context ends.
type idleGuard struct {
	timer  *time.Timer
	cancel context.CancelCauseFunc
}

func newIdleGuard(ctx context.Context) (context.Context, *idleGuard) {
	ctx, cancel := context.WithCancelCause(ctx)
	g := &idleGuard{cancel: cancel}
	g.timer = time.AfterFunc(streamIdleTimeout, func() { cancel(errStreamIdle) })
	return ctx, g
}

func (g *idleGuard) stop() {
	g.timer.Stop()
	g.cancel(nil)
}

// middleware resets the idle timer whenever the response body is read,
// including the ping events the SDK consumes without returning them.
func (g *idleGuard) middleware() option.RequestOption {
	return option.WithMiddleware(func(req *http.Request, next option.MiddlewareNext) (*http.Response, error) {
		resp, err := next(req)
		if resp != nil && resp.Body != nil {
			resp.Body = &activityBody{ReadCloser: resp.Body, touch: g.touch}
		}
		return resp, err
	})
}

func (g *idleGuard) touch() {
	g.timer.Reset(streamIdleTimeout)
}

// pause stops the idle timer until the next touch, for time spent waiting on
// something other than the upstream.
func (g *idleGuard) pause() {
	g.timer.Stop()
}

// idled reports whether the guard, not the caller, ended ctx.
func idled(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), errStreamIdle)
}

type activityBody struct {
	io.ReadCloser
	touch func()
}

func (b *activityBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.touch()
	}
	return n, err
}
