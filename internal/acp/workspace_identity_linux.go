//go:build linux

package acp

import (
	"os"

	"golang.org/x/sys/unix"
)

// openWorkspaceDirectory returns a descriptor that holds the directory at path
// without following a symbolic link. O_PATH needs no permission on the
// directory itself, only search permission on its parents, so the supervisor
// can hold a workspace it cannot read.
func openWorkspaceDirectory(path string) (*os.File, error) {
	return os.OpenFile(path, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
}
