//go:build darwin || linux

package workspacedelta

import (
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

type fileIdentity struct {
	device uint64
	inode  uint64
}

func identityAndLinks(info os.FileInfo) (fileIdentity, uint64, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return fileIdentity{}, 0, ErrUnsupportedFilesystem
	}
	return fileIdentity{device: uint64(stat.Dev), inode: uint64(stat.Ino)}, uint64(stat.Nlink), nil //nolint:unconvert
}

func openRegularNoFollow(filePath string) (*os.File, error) {
	fd, err := unix.Open(filePath, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), filePath)
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("create file handle")
	}
	return file, nil
}
