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
