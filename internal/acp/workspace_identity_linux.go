//go:build linux

package acp

import (
	"os"

	"golang.org/x/sys/unix"
)

func openWorkspaceDirectory(path string) (*os.File, error) {
	return os.OpenFile(path, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
}
