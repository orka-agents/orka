package api

import (
	"errors"
	"io"
	"net"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/valyala/fasthttp"
)

const (
	// maxDrainedRequestBodyBytes bounds how much of an unread request body is
	// discarded to keep the connection alive, as net/http does; a larger or
	// stalled remainder closes the connection instead.
	maxDrainedRequestBodyBytes = 256 << 10
	requestBodyDrainTimeout    = time.Second
)

// drainUnreadRequestBody discards whatever a handler left unread of a
// streamed request body. With StreamRequestBody, fasthttp keeps the unread
// remainder buffered on the keep-alive connection and parses it as the next
// request, so a request rejected before its body is read (authentication,
// authorization, validation) could otherwise carry a second request inside
// its body. Bodies read with Body() (including JSON binding) detach the
// stream and need nothing. Draining, rather than closing at once, delivers
// the response instead of resetting a connection that still holds unread
// bytes; the drain is bounded in size and time, so a client that has not
// finished sending only costs the connection.
func drainUnreadRequestBody(c fiber.Ctx) error {
	err := c.Next()
	request := c.Request()
	if !request.IsBodyStream() || request.Header.ContentLength() == 0 {
		return err
	}
	if !drainRequestBody(c.RequestCtx().Conn(), request.BodyStream()) {
		c.Response().Header.SetConnectionClose()
	}
	return err
}

func drainRequestBody(conn net.Conn, body io.Reader) bool {
	if conn != nil {
		_ = conn.SetReadDeadline(time.Now().Add(requestBodyDrainTimeout))
		defer func() { _ = conn.SetReadDeadline(time.Time{}) }()
	}
	n, err := io.CopyN(io.Discard, body, maxDrainedRequestBodyBytes+1)
	return n <= maxDrainedRequestBodyBytes && errors.Is(err, io.EOF)
}

// redactRequestParseErrors keeps a request the server cannot parse from being
// echoed in its error response: fasthttp's parse errors quote the raw request
// buffer, headers and credentials included.
func redactRequestParseErrors(app *fiber.App) {
	server := app.Server()
	handle := server.ErrorHandler
	server.ErrorHandler = func(ctx *fasthttp.RequestCtx, err error) {
		if err != nil {
			err = redactedRequestError{err: err}
		}
		handle(ctx, err)
	}
}

// redactedRequestError keeps the wrapped error for errors.Is/As, which Fiber
// uses to pick the status code, but replaces its text.
type redactedRequestError struct{ err error }

func (e redactedRequestError) Error() string {
	// Keep the phrases Fiber matches when it picks the status code.
	message := e.err.Error()
	switch {
	case strings.Contains(message, "unsupported http request method"):
		return "unsupported http request method"
	case strings.Contains(message, "timeout"):
		return "request timeout"
	}
	return "malformed HTTP request"
}

func (e redactedRequestError) Unwrap() error { return e.err }
