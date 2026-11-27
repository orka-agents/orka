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

var streamIdleTimeout = 10 * time.Minute

var errStreamIdle = errors.New("anthropic stream went idle")

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

func (g *idleGuard) pause() {
	g.timer.Stop()
}

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
