/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package api

import (
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/requestid"
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
		RecordAPIRequest(method, path, status, duration)

		return err
	}
}

// RecordAPIRequest records an API request in Prometheus metrics
// This is a placeholder - actual implementation would use Prometheus client
func RecordAPIRequest(method, path string, status int, duration time.Duration) {
	// TODO: Implement Prometheus metrics recording
	// apiRequestsTotal.WithLabelValues(method, path, strconv.Itoa(status)).Inc()
	// apiRequestDuration.WithLabelValues(method, path).Observe(duration.Seconds())
}
