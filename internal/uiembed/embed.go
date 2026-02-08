/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

// Package uiembed provides the embedded frontend assets.
// The dist/ directory is populated by "make ui-build" which copies ui/dist/ here.
package uiembed

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var distFS embed.FS

// FS returns the embedded frontend filesystem rooted at the dist/ directory.
func FS() (fs.FS, error) {
	return fs.Sub(distFS, "dist")
}
