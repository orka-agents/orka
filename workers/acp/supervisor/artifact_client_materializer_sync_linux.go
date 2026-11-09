//go:build linux

package supervisor

import (
	"os"

	"golang.org/x/sys/unix"
)

// syncMaterializedTree flushes the whole staged workspace with one syncfs(2)
// instead of one fsync per file, which costs minutes on network-attached node
// disks for repositories with tens of thousands of files.
func syncMaterializedTree(root string) error {
	directory, err := os.Open(root)
	if err != nil {
		return err
	}
	defer directory.Close() //nolint:errcheck
	return unix.Syncfs(int(directory.Fd()))
}
