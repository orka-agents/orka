/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"fmt"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/requestid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/sozercan/orka/internal/metrics"
	"github.com/sozercan/orka/internal/tracing"
)

// NewLoggingMiddleware creates a logging middleware
func NewLoggingMiddleware() fiber.Handler {
	return func(c fiber.Ctx) error {
		start := time.Now()

		// Get request ID from middleware
		reqID := requestid.FromContext(c)

		// Process request
		err := c.Next()

		// Log the request
		duration := time.Since(start)
		status := c.Response().StatusCode()

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

// NewMetricsMiddleware creates a metrics middleware
func NewMetricsMiddleware() fiber.Handler {
	return func(c fiber.Ctx) error {
		start := time.Now()

		// Process request
		err := c.Next()

		// Record metrics
		duration := time.Since(start)
		status := c.Response().StatusCode()
		method := c.Method()
		path := c.Route().Path

		// Record in Prometheus metrics
		metrics.RecordAPIRequest(path, method, status, duration.Seconds())

		return err
	}
}

// NewTracingMiddleware creates an OpenTelemetry tracing middleware.
func NewTracingMiddleware() fiber.Handler {
	return func(c fiber.Ctx) error {
		tracer := tracing.Tracer("orka.api")
		ctx, span := tracer.Start(c.Context(), fmt.Sprintf("%s %s", c.Method(), c.Route().Path),
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				attribute.String("http.method", c.Method()),
				attribute.String("http.route", c.Route().Path),
				attribute.String("http.url", c.OriginalURL()),
			),
		)
		defer span.End()

		c.SetContext(ctx)

		if reqID := requestid.FromContext(c); reqID != "" {
			span.SetAttributes(attribute.String("http.request_id", reqID))
		}

		err := c.Next()

		status := c.Response().StatusCode()
		span.SetAttributes(attribute.Int("http.status_code", status))
		if status >= 400 {
			span.SetStatus(codes.Error, fmt.Sprintf("HTTP %d", status))
		}

		return err
	}
}
