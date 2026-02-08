/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/requestid"
	"github.com/sozercan/mercan/internal/metrics"
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
