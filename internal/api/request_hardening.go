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
	maxDrainedRequestBodyBytes = 256 << 10
	requestBodyDrainTimeout    = time.Second
)

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

type redactedRequestError struct{ err error }

func (e redactedRequestError) Error() string {
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
