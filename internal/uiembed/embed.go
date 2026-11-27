/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package uiembed

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var distFS embed.FS

func FS() (fs.FS, error) {
	return fs.Sub(distFS, "dist")
}
