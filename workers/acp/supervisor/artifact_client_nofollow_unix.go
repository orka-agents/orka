//go:build darwin || linux

package supervisor

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func openMaterializedFileNoFollow(path string, mode os.FileMode) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, uint32(mode.Perm()))
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("create materialized workspace file handle")
	}
	return file, nil
}
