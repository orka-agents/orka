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
	staticAssetCacheControl = "public, max-age=31536000, immutable"
	spaIndexCacheControl    = "no-cache"

	spaContentSecurityPolicyFormat = "default-src 'self'; script-src 'self'%s; style-src 'self' 'unsafe-inline'; " +
		"img-src 'self' data:; font-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; " +
		"base-uri 'self'; form-action 'self'; object-src 'none'"
)

var inlineScriptPattern = regexp.MustCompile(`(?is)<script\b([^>]*)>(.*?)</script\s*>`)

var scriptSrcAttributePattern = regexp.MustCompile(`(?i)\bsrc\s*=`)

func spaContentSecurityPolicy(html []byte) string {
	var hashes strings.Builder
	for _, match := range inlineScriptPattern.FindAllSubmatch(html, -1) {
		if scriptSrcAttributePattern.Match(match[1]) {
			continue
		}
		text := strings.ReplaceAll(strings.ReplaceAll(string(match[2]), "\r\n", "\n"), "\r", "\n")
		sum := sha256.Sum256([]byte(text))
		hashes.WriteString(" 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'")
	}
	return fmt.Sprintf(spaContentSecurityPolicyFormat, hashes.String())
}

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

var embeddedSPAIndexPage = sync.OnceValues(func() (spaIndexPage, bool) {
	distFS, err := uiembed.FS()
	if err != nil {
		return spaIndexPage{}, false
	}
	return newSPAIndexPage(distFS)
})

func (p spaIndexPage) send(c fiber.Ctx) error {
	c.Set(fiber.HeaderContentType, "text/html; charset=utf-8")
	c.Set(fiber.HeaderCacheControl, spaIndexCacheControl)
	c.Set(fiber.HeaderXContentTypeOptions, "nosniff")
	c.Set(fiber.HeaderXFrameOptions, "DENY")
	c.Set(fiber.HeaderReferrerPolicy, "no-referrer")
	c.Set(fiber.HeaderContentSecurityPolicy, p.csp)
	return c.Status(fiber.StatusOK).Send(p.html)
}

func staticAssetContentType(file string) (contentType string, compressible bool) {
	switch path.Ext(file) {
	case ".js":
		return "application/javascript", true
	case ".css":
		return "text/css", true
	}
	return "application/octet-stream", false
}

const dashboardStateKey = "orka.dashboard"

func dashboardServed(c fiber.Ctx) bool {
	served, _ := c.App().State().GetBool(dashboardStateKey)
	return served
}

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

	compressStatic := compress.New()
	compressAsset := compress.New(compress.Config{
		Next: func(c fiber.Ctx) bool {
			_, compressible := staticAssetContentType(c.Params("file"))
			return !compressible
		},
	})

	s.app.Get("/assets/:file", compressAsset, func(c fiber.Ctx) error {
		file := c.Params("file")
		contentType, _ := staticAssetContentType(file)
		if err := serveFile(c, "assets/"+file, contentType); err != nil {
			return err
		}
		c.Set(fiber.HeaderCacheControl, staticAssetCacheControl)
		return nil
	})

	s.app.Get("/favicon.svg", compressStatic, func(c fiber.Ctx) error {
		return serveFile(c, "favicon.svg", "image/svg+xml")
	})
	s.app.Get("/favicon.png", func(c fiber.Ctx) error {
		return serveFile(c, "favicon.png", "image/png")
	})

	for _, route := range []string{
		"/", "/login", "/chat", "/kanban", "/approvals", "/live", "/usage", "/runtime-simulator",
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
