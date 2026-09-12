//go:build darwin || linux

package artifactcap

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func openFileNoFollow(path string, flags int, perm os.FileMode) (*os.File, error) {
	unixFlags := unix.O_CLOEXEC | unix.O_NOFOLLOW
	switch {
	case flags&os.O_RDWR != 0:
		unixFlags |= unix.O_RDWR
	case flags&os.O_WRONLY != 0:
		unixFlags |= unix.O_WRONLY
	default:
		unixFlags |= unix.O_RDONLY
	}
	if flags&os.O_CREATE != 0 {
		unixFlags |= unix.O_CREAT
	}
	if flags&os.O_EXCL != 0 {
		unixFlags |= unix.O_EXCL
	}
	if flags&os.O_TRUNC != 0 {
		unixFlags |= unix.O_TRUNC
	}
	fd, err := unix.Open(path, unixFlags, uint32(perm.Perm()))
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("create artifact file handle")
	}
	return file, nil
}
