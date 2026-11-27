/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/requestid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/orka-agents/orka/internal/metrics"
	"github.com/orka-agents/orka/internal/tracing"
)

func NewLoggingMiddleware() fiber.Handler {
	return func(c fiber.Ctx) error {
		start := time.Now()

		reqID := requestid.FromContext(c)

		err := c.Next()

		duration := time.Since(start)
		status := effectiveStatusCode(c, err)

		log.Info("request completed",
			"requestId", reqID,
			"method", c.Method(),
			"path", c.Path(),
			"status", status,
			"duration", duration.String(),
			"ip", c.IP(),
		)

		return err
	}
}

func NewMetricsMiddleware() fiber.Handler {
	return func(c fiber.Ctx) error {
		start := time.Now()

		err := c.Next()

		duration := time.Since(start)
		status := effectiveStatusCode(c, err)
		method := c.Method()
		path := c.Route().Path

		metrics.RecordAPIRequest(path, method, status, duration.Seconds())

		return err
	}
}

func NewTracingMiddleware() fiber.Handler {
	return func(c fiber.Ctx) error {
		tracer := tracing.Tracer("orka.api")
		ctx := otel.GetTextMapPropagator().Extract(c.Context(), fiberHeaderCarrier{c: c})
		method := c.Method()
		ctx, span := tracer.Start(ctx, method,
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				attribute.String("http.method", method),
				attribute.String("http.url", c.BaseURL()+c.Path()),
			),
		)
		defer span.End()

		c.SetContext(ctx)

		if reqID := requestid.FromContext(c); reqID != "" {
			span.SetAttributes(attribute.String("http.request_id", strings.Clone(reqID)))
		}

		err := c.Next()

		if route := c.Route().Path; route != "" {
			span.SetName(fmt.Sprintf("%s %s", method, route))
			span.SetAttributes(attribute.String("http.route", route))
		}

		status := effectiveStatusCode(c, err)
		span.SetAttributes(attribute.Int("http.status_code", status))
		if status >= 400 {
			span.SetStatus(codes.Error, fmt.Sprintf("HTTP %d", status))
		}

		return err
	}
}

func effectiveStatusCode(c fiber.Ctx, err error) int {
	status := c.Response().StatusCode()
	if err != nil && status < fiber.StatusBadRequest {
		if fiberErr, ok := errors.AsType[*fiber.Error](err); ok {
			status = fiberErr.Code
		} else {
			status = fiber.StatusInternalServerError
		}
	}
	if status == fiber.StatusNotFound && dashboardServed(c) && spaFallbackEligible(c.Path()) {
		if _, ok := embeddedSPAIndexPage(); ok {
			status = fiber.StatusOK
		}
	}
	if status == fiber.StatusNotFound {
		if _, unsupported := unsupportedCompatRoutes[routeLookupPath(c.Path())]; unsupported {
			status = fiber.StatusNotImplemented
		}
	}
	return status
}

type fiberHeaderCarrier struct {
	c fiber.Ctx
}

func (c fiberHeaderCarrier) Get(key string) string {
	if c.c == nil {
		return ""
	}
	return strings.Clone(c.c.Get(key))
}

func (c fiberHeaderCarrier) Set(string, string) {}

func (c fiberHeaderCarrier) Keys() []string {
	if c.c == nil {
		return nil
	}
	headers := c.c.GetReqHeaders()
	keys := make([]string, 0, len(headers))
	for key := range headers {
		keys = append(keys, strings.Clone(key))
	}
	return keys
}
