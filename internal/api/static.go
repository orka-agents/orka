/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"strings"
	"sync"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/compress"

	"github.com/orka-agents/orka/internal/uiembed"
)

const (
	// Vite content-hashes every file under assets/, so a URL never changes
	// meaning and the browser may keep it for a year without revalidating.
	staticAssetCacheControl = "public, max-age=31536000, immutable"
	// index.html names the current hashed assets, so it must be revalidated
	// on every load or a browser keeps requesting chunks of an old build.
	spaIndexCacheControl = "no-cache"

	// spaContentSecurityPolicyFormat is the dashboard's document policy; the
	// %s slot receives a hash source for each inline script in index.html.
	spaContentSecurityPolicyFormat = "default-src 'self'; script-src 'self'%s; style-src 'self' 'unsafe-inline'; " +
		"img-src 'self' data:; font-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; " +
		"base-uri 'self'; form-action 'self'; object-src 'none'"
)

// inlineScriptPattern matches a script element and captures its attributes
// and text. Vite copies index.html's inline scripts into the build verbatim.
var inlineScriptPattern = regexp.MustCompile(`(?is)<script\b([^>]*)>(.*?)</script\s*>`)

var scriptSrcAttributePattern = regexp.MustCompile(`(?i)\bsrc\s*=`)

// spaContentSecurityPolicy returns the document policy for html, allowing
// exactly its inline scripts by hash so script-src needs no 'unsafe-inline'.
func spaContentSecurityPolicy(html []byte) string {
	var hashes strings.Builder
	for _, match := range inlineScriptPattern.FindAllSubmatch(html, -1) {
		if scriptSrcAttributePattern.Match(match[1]) {
			continue
		}
		// Browsers hash the script text after the HTML parser normalizes
		// line endings to LF.
		text := strings.ReplaceAll(strings.ReplaceAll(string(match[2]), "\r\n", "\n"), "\r", "\n")
		sum := sha256.Sum256([]byte(text))
		hashes.WriteString(" 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'")
	}
	return fmt.Sprintf(spaContentSecurityPolicyFormat, hashes.String())
}

// spaIndexPage is the SPA index document and the policy it is served with.
type spaIndexPage struct {
	html []byte
	csp  string
}

func newSPAIndexPage(distFS fs.FS) (spaIndexPage, bool) {
	html, err := fs.ReadFile(distFS, "index.html")
	if err != nil {
		return spaIndexPage{}, false
	}
	return spaIndexPage{html: html, csp: spaContentSecurityPolicy(html)}, true
}

// embeddedSPAIndexPage returns the embedded index page, or false when the UI
// assets are unavailable. The embedded files cannot change at runtime, so the
// page and its policy are built once.
var embeddedSPAIndexPage = sync.OnceValues(func() (spaIndexPage, bool) {
	distFS, err := uiembed.FS()
	if err != nil {
		return spaIndexPage{}, false
	}
	return newSPAIndexPage(distFS)
})

// send writes the index page for an SPA route or the 404 fallback.
func (p spaIndexPage) send(c fiber.Ctx) error {
	c.Set(fiber.HeaderContentType, "text/html; charset=utf-8")
	c.Set(fiber.HeaderCacheControl, spaIndexCacheControl)
	c.Set(fiber.HeaderXContentTypeOptions, "nosniff")
	c.Set(fiber.HeaderXFrameOptions, "DENY")
	c.Set(fiber.HeaderReferrerPolicy, "no-referrer")
	c.Set(fiber.HeaderContentSecurityPolicy, p.csp)
	return c.Status(fiber.StatusOK).Send(p.html)
}

// staticAssetContentType maps the build's text assets to their media types.
// Anything else (the fonts) is served as an opaque, already compressed file.
func staticAssetContentType(file string) (contentType string, compressible bool) {
	switch path.Ext(file) {
	case ".js":
		return "application/javascript", true
	case ".css":
		return "text/css", true
	}
	return "application/octet-stream", false
}

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
	index, _ := embeddedSPAIndexPage()
	s.registerStaticFiles(distFS, index)
}

func (s *Server) registerStaticFiles(distFS fs.FS, index spaIndexPage) {
	s.app.State().Set(dashboardStateKey, true)

	serveFile := func(c fiber.Ctx, filePath, contentType string) error {
		data, readErr := fs.ReadFile(distFS, filePath)
		if readErr != nil {
			return fiber.NewError(fiber.StatusNotFound, "file not found")
		}
		c.Set(fiber.HeaderContentType, contentType)
		c.Set(fiber.HeaderXContentTypeOptions, "nosniff")
		return c.Status(fiber.StatusOK).Send(data)
	}

	serveIndex := func(c fiber.Ctx) error {
		if index.html == nil {
			return fiber.NewError(fiber.StatusNotFound, "file not found")
		}
		return index.send(c)
	}

	// Compression is attached only to these routes: API responses, event
	// streams, and the compatibility endpoints never pass through it.
	compressStatic := compress.New()
	compressAsset := compress.New(compress.Config{
		Next: func(c fiber.Ctx) bool {
			_, compressible := staticAssetContentType(c.Params("file"))
			return !compressible
		},
	})

	// Serve Vite build assets (JS, CSS, fonts)
	s.app.Get("/assets/:file", compressAsset, func(c fiber.Ctx) error {
		file := c.Params("file")
		contentType, _ := staticAssetContentType(file)
		if err := serveFile(c, "assets/"+file, contentType); err != nil {
			return err
		}
		c.Set(fiber.HeaderCacheControl, staticAssetCacheControl)
		return nil
	})

	// Serve favicon
	s.app.Get("/favicon.svg", compressStatic, func(c fiber.Ctx) error {
		return serveFile(c, "favicon.svg", "image/svg+xml")
	})

	// SPA routes: serve index.html for every top-level route under
	// ui/src/routes (client-side routing), so each gets the compressed page.
	// Unknown paths still reach the error handler's uncompressed fallback.
	for _, route := range []string{
		"/", "/login", "/chat", "/kanban", "/live", "/usage", "/runtime-simulator",
		"/tasks", "/tasks/*",
		"/sessions", "/sessions/*",
		"/agents", "/agents/*",
		"/runtimes", "/runtimes/*",
		"/tools", "/tools/*",
		"/gateways", "/gateways/*",
		"/monitors", "/monitors/*",
		"/security", "/security/*",
		"/settings/*",
	} {
		s.app.Get(route, compressStatic, serveIndex)
	}
}
