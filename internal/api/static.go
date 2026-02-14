/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"io/fs"

	"github.com/gofiber/fiber/v3"

	"github.com/sozercan/orka/internal/uiembed"
)

// setupStaticFiles serves the embedded SPA frontend.
// All non-API, non-health routes fall back to index.html for client-side routing.
func (s *Server) setupStaticFiles() {
	distFS, err := uiembed.FS()
	if err != nil {
		log.Error(err, "failed to load embedded UI assets")
		return
	}

	serveFile := func(c fiber.Ctx, filePath, contentType string) error {
		data, readErr := fs.ReadFile(distFS, filePath)
		if readErr != nil {
			return fiber.NewError(fiber.StatusNotFound, "file not found")
		}
		c.Set("Content-Type", contentType)
		return c.Status(fiber.StatusOK).Send(data)
	}

	serveIndex := func(c fiber.Ctx) error {
		return serveFile(c, "index.html", "text/html; charset=utf-8")
	}

	// Serve Vite build assets (JS, CSS)
	s.app.Get("/assets/:file", func(c fiber.Ctx) error {
		file := c.Params("file")
		contentType := "application/octet-stream"
		switch {
		case len(file) > 3 && file[len(file)-3:] == ".js":
			contentType = "application/javascript"
		case len(file) > 4 && file[len(file)-4:] == ".css":
			contentType = "text/css"
		}
		return serveFile(c, "assets/"+file, contentType)
	})

	// Serve favicon
	s.app.Get("/favicon.svg", func(c fiber.Ctx) error {
		return serveFile(c, "favicon.svg", "image/svg+xml")
	})

	// SPA routes: serve index.html for all UI paths (client-side routing)
	s.app.Get("/", serveIndex)
	s.app.Get("/login", serveIndex)
	s.app.Get("/tasks", serveIndex)
	s.app.Get("/tasks/*", serveIndex)
	s.app.Get("/sessions", serveIndex)
	s.app.Get("/sessions/*", serveIndex)
	s.app.Get("/agents", serveIndex)
	s.app.Get("/agents/*", serveIndex)
	s.app.Get("/tools", serveIndex)
	s.app.Get("/tools/*", serveIndex)
}
