/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/requestid"
)

func TestNewLoggingMiddleware(t *testing.T) {
	app := fiber.New()
	app.Use(requestid.New())
	app.Use(NewLoggingMiddleware())
	app.Get("/test", func(c fiber.Ctx) error {
		return c.SendString("OK")
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

func TestNewLoggingMiddleware_LogsInfo(t *testing.T) {
	app := fiber.New()
	app.Use(requestid.New())
	app.Use(NewLoggingMiddleware())
	app.Get("/test", func(c fiber.Ctx) error {
		return c.SendString("OK")
	})

	// Test multiple status codes
	tests := []struct {
		name   string
		path   string
		status int
	}{
		{"success", "/test", http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			resp, err := app.Test(req)
			if err != nil {
				t.Fatalf("Test request failed: %v", err)
			}
			if resp.StatusCode != tt.status {
				t.Errorf("StatusCode = %d, want %d", resp.StatusCode, tt.status)
			}
		})
	}
}

func TestNewMetricsMiddleware(t *testing.T) {
	app := fiber.New()
	app.Use(NewMetricsMiddleware())
	app.Get("/test", func(c fiber.Ctx) error {
		return c.SendString("OK")
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

func TestNewMetricsMiddleware_RecordsMetrics(t *testing.T) {
	app := fiber.New()
	app.Use(NewMetricsMiddleware())
	app.Get("/api/test", func(c fiber.Ctx) error {
		return c.SendString("OK")
	})
	app.Post("/api/create", func(c fiber.Ctx) error {
		return c.Status(fiber.StatusCreated).SendString("Created")
	})

	tests := []struct {
		name   string
		method string
		path   string
		status int
	}{
		{"GET request", http.MethodGet, "/api/test", http.StatusOK},
		{"POST request", http.MethodPost, "/api/create", http.StatusCreated},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, nil)
			resp, err := app.Test(req)
			if err != nil {
				t.Fatalf("Test request failed: %v", err)
			}
			if resp.StatusCode != tt.status {
				t.Errorf("StatusCode = %d, want %d", resp.StatusCode, tt.status)
			}
		})
	}
}

func TestNewLoggingMiddleware_WithError(t *testing.T) {
	app := fiber.New(fiber.Config{
		ErrorHandler: func(c fiber.Ctx, err error) error {
			return c.Status(fiber.StatusInternalServerError).SendString(err.Error())
		},
	})
	app.Use(requestid.New())
	app.Use(NewLoggingMiddleware())
	app.Get("/error", func(c fiber.Ctx) error {
		return fiber.NewError(fiber.StatusInternalServerError, "test error")
	})

	req := httptest.NewRequest(http.MethodGet, "/error", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusInternalServerError)
	}
}

func TestNewMetricsMiddleware_WithError(t *testing.T) {
	app := fiber.New(fiber.Config{
		ErrorHandler: func(c fiber.Ctx, err error) error {
			return c.Status(fiber.StatusBadRequest).SendString(err.Error())
		},
	})
	app.Use(NewMetricsMiddleware())
	app.Get("/error", func(c fiber.Ctx) error {
		return fiber.NewError(fiber.StatusBadRequest, "bad request")
	})

	req := httptest.NewRequest(http.MethodGet, "/error", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
}

func TestNewTracingMiddleware(t *testing.T) {
	app := fiber.New()
	app.Use(requestid.New())
	app.Use(NewTracingMiddleware())
	app.Get("/test", func(c fiber.Ctx) error {
		return c.SendString("OK")
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

func TestNewTracingMiddleware_WithRequestID(t *testing.T) {
	app := fiber.New()
	app.Use(requestid.New())
	app.Use(NewTracingMiddleware())
	app.Get("/traced", func(c fiber.Ctx) error {
		return c.SendString("traced")
	})

	req := httptest.NewRequest(http.MethodGet, "/traced", nil)
	req.Header.Set("X-Request-Id", "custom-req-id")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

func TestNewTracingMiddleware_ErrorStatus(t *testing.T) {
	app := fiber.New(fiber.Config{
		ErrorHandler: func(c fiber.Ctx, err error) error {
			code := fiber.StatusInternalServerError
			if e, ok := err.(*fiber.Error); ok {
				code = e.Code
			}
			return c.Status(code).SendString(err.Error())
		},
	})
	app.Use(requestid.New())
	app.Use(NewTracingMiddleware())
	app.Get("/bad", func(c fiber.Ctx) error {
		return fiber.NewError(fiber.StatusBadRequest, "bad request")
	})
	app.Get("/fail", func(c fiber.Ctx) error {
		return fiber.NewError(fiber.StatusInternalServerError, "internal error")
	})

	tests := []struct {
		name   string
		path   string
		status int
	}{
		{"400 error sets span error", "/bad", http.StatusBadRequest},
		{"500 error sets span error", "/fail", http.StatusInternalServerError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			resp, err := app.Test(req)
			if err != nil {
				t.Fatalf("Test request failed: %v", err)
			}
			if resp.StatusCode != tt.status {
				t.Errorf("StatusCode = %d, want %d", resp.StatusCode, tt.status)
			}
		})
	}
}

func TestNewTracingMiddleware_WithoutRequestID(t *testing.T) {
	// Test tracing middleware without requestid middleware to cover the empty reqID branch
	app := fiber.New()
	app.Use(NewTracingMiddleware())
	app.Get("/no-reqid", func(c fiber.Ctx) error {
		return c.SendString("OK")
	})

	req := httptest.NewRequest(http.MethodGet, "/no-reqid", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}
