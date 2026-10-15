/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"io/fs"

	"github.com/gofiber/fiber/v3"

	"github.com/orka-agents/orka/internal/uiembed"
)

// dashboardStateKey marks an app that serves the embedded dashboard. The
// error handler and the telemetry middleware both read it, so the status a
// client receives for an unrouted path and the one recorded always agree.
const dashboardStateKey = "orka.dashboard"

// dashboardServed reports whether the app serving c answers unrouted
// non-API paths with the dashboard's index page.
func dashboardServed(c fiber.Ctx) bool {
	served, _ := c.App().State().GetBool(dashboardStateKey)
	return served
}

// setupStaticFiles serves the embedded SPA frontend when the dashboard is
// enabled. All non-API, non-health routes fall back to index.html for
// client-side routing. Without the dashboard, connector consent still needs
// somewhere to land, so its redirect target gets a plain completion page.
func (s *Server) setupStaticFiles() {
	if !s.config.DashboardEnabled {
		if s.config.Connectors.Enabled {
			s.app.Get(connectorSettingsPath, s.handlers.ConnectorCompletionPage)
		}
		return
	}

	distFS, err := uiembed.FS()
	if err != nil {
		log.Error(err, "failed to load embedded UI assets")
		return
	}
	s.app.State().Set(dashboardStateKey, true)

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
	s.app.Get("/runtimes", serveIndex)
	s.app.Get("/runtimes/*", serveIndex)
	s.app.Get("/tools", serveIndex)
	s.app.Get("/tools/*", serveIndex)
}
