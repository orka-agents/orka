//go:build linux

package skill

import (
	"errors"
	"io"
	"os"
	"path"
	"slices"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	openDirFlags  = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	openFileFlags = unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC | unix.O_NONBLOCK | unix.O_NOCTTY
	createFlags   = unix.O_WRONLY | unix.O_CREAT | unix.O_EXCL | unix.O_NOFOLLOW | unix.O_CLOEXEC
	dirMode       = 0o755
	fileMode      = 0o644
	execMode      = 0o755
)

type loadState struct {
	tree    *Tree
	limits  Limits
	entries int
	bytes   int64
}

func loadTree(root, name string, limits Limits) (*Tree, error) {
	if !path.IsAbs(root) || path.Clean(root) != root {
		return nil, failf(ReasonMissingMount, "skill %s folder %q must be a clean absolute path", name, root)
	}
	fd, err := unix.Open(root, openDirFlags, 0)
	if err != nil {
		return nil, failf(ReasonMissingMount, "skill %s folder %s is not a real folder: %v", name, root, err)
	}
	defer unix.Close(fd) //nolint:errcheck
	state := &loadState{tree: &Tree{Name: name}, limits: limits}
	if err := state.walk(fd, "", 0); err != nil {
		return nil, err
	}
	return state.tree, nil
}

func (s *loadState) walk(dirFd int, relative string, depth int) error {
	names, err := readDirectoryNames(dirFd, s.limits.MaxEntries-s.entries)
	if err != nil {
		return err
	}
	slices.Sort(names)
	for _, name := range names {
		display := joinRelative(relative, name)
		if !validFileName(name) {
			return failf(ReasonInvalidFileName, "skill %s entry %q has a name that is not allowed", s.tree.Name, display)
		}
		s.entries++
		if s.entries > s.limits.MaxEntries {
			return failf(ReasonTooManyEntries, "skill %s has more than %d entries", s.tree.Name, s.limits.MaxEntries)
		}
		var stat unix.Stat_t
		if err := unix.Fstatat(dirFd, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return failf(ReasonReadFailed, "skill %s inspect %s: %v", s.tree.Name, display, err)
		}
		switch stat.Mode & unix.S_IFMT {
		case unix.S_IFDIR:
			if depth+1 > s.limits.MaxDepth {
				return failf(ReasonTooDeep, "skill %s is more than %d folders deep at %s", s.tree.Name, s.limits.MaxDepth, display)
			}
			if err := s.walkDirectory(dirFd, name, display, depth+1, &stat); err != nil {
				return err
			}
		case unix.S_IFREG:
			if err := s.readRegular(dirFd, name, display, &stat); err != nil {
				return err
			}
		default:
			return failf(ReasonUnsupportedFileType, "skill %s entry %s is not a regular file or folder; symlinks, devices, FIFOs, and sockets are not allowed", s.tree.Name, display)
		}
	}
	return nil
}

func (s *loadState) walkDirectory(parentFd int, name, display string, depth int, expected *unix.Stat_t) error {
	fd, err := unix.Openat(parentFd, name, openDirFlags, 0)
	if err != nil {
		return failf(ReasonReadFailed, "skill %s open %s: %v", s.tree.Name, display, err)
	}
	defer unix.Close(fd) //nolint:errcheck
	if err := requireSameObject(fd, expected, unix.S_IFDIR); err != nil {
		return failf(ReasonReadFailed, "skill %s %s changed while it was read", s.tree.Name, display)
	}
	s.tree.Dirs = append(s.tree.Dirs, display)
	return s.walk(fd, display, depth)
}

func (s *loadState) readRegular(parentFd int, name, display string, expected *unix.Stat_t) error {
	if expected.Size > s.limits.MaxFileBytes {
		return failf(ReasonFileTooLarge, "skill %s file %s is larger than %d bytes", s.tree.Name, display, s.limits.MaxFileBytes)
	}
	if s.bytes+expected.Size > s.limits.MaxTotalBytes {
		return failf(ReasonTooLarge, "skill %s is larger than %d bytes", s.tree.Name, s.limits.MaxTotalBytes)
	}
	fd, err := unix.Openat(parentFd, name, openFileFlags, 0)
	if err != nil {
		return failf(ReasonReadFailed, "skill %s open %s: %v", s.tree.Name, display, err)
	}
	file := os.NewFile(uintptr(fd), display)
	defer file.Close() //nolint:errcheck
	if err := requireSameObject(fd, expected, unix.S_IFREG); err != nil {
		return failf(ReasonReadFailed, "skill %s %s changed while it was read", s.tree.Name, display)
	}
	data, err := io.ReadAll(io.LimitReader(file, s.limits.MaxFileBytes+1))
	if err != nil {
		return failf(ReasonReadFailed, "skill %s read %s: %v", s.tree.Name, display, err)
	}
	if int64(len(data)) > s.limits.MaxFileBytes {
		return failf(ReasonFileTooLarge, "skill %s file %s is larger than %d bytes", s.tree.Name, display, s.limits.MaxFileBytes)
	}
	s.bytes += int64(len(data))
	if s.bytes > s.limits.MaxTotalBytes {
		return failf(ReasonTooLarge, "skill %s is larger than %d bytes", s.tree.Name, s.limits.MaxTotalBytes)
	}
	s.tree.Files = append(s.tree.Files, File{Path: display, Data: data, Executable: expected.Mode&0o111 != 0})
	return nil
}

func requireSameObject(fd int, expected *unix.Stat_t, kind uint32) error {
	var actual unix.Stat_t
	if err := unix.Fstat(fd, &actual); err != nil {
		return err
	}
	if actual.Mode&unix.S_IFMT != kind || actual.Dev != expected.Dev || actual.Ino != expected.Ino {
		return errors.New("object changed")
	}
	return nil
}

func readDirectoryNames(dirFd int, remaining int) ([]string, error) {
	dup, err := unix.Dup(dirFd)
	if err != nil {
		return nil, failf(ReasonReadFailed, "read folder: %v", err)
	}
	dir := os.NewFile(uintptr(dup), "")
	defer dir.Close() //nolint:errcheck
	var names []string
	for {
		entries, err := dir.ReadDir(64)
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		if len(names) > remaining {
			return nil, failf(ReasonTooManyEntries, "skill folder has more entries than the remaining limit of %d", max(remaining, 0))
		}
		if errors.Is(err, io.EOF) {
			return names, nil
		}
		if err != nil {
			return nil, failf(ReasonReadFailed, "read folder: %v", err)
		}
	}
}

// WriteTo creates destination, which must not exist yet, under an existing
// parent folder and writes the skill into it with fixed modes: folders 0755,
// files 0644, and 0755 for files that were executable in the image. Ownership
// is left to the caller, which assigns the whole session tree to the session
// identity.
func (t *Tree) WriteTo(destination string) error {
	if !path.IsAbs(destination) || path.Clean(destination) != destination {
		return failf(ReasonWriteFailed, "skill %s destination %q must be a clean absolute path", t.Name, destination)
	}
	parentFd, err := unix.Open(path.Dir(destination), openDirFlags, 0)
	if err != nil {
		return failf(ReasonWriteFailed, "skill %s open destination parent: %v", t.Name, err)
	}
	defer unix.Close(parentFd) //nolint:errcheck
	base := path.Base(destination)
	if err := unix.Mkdirat(parentFd, base, dirMode); err != nil {
		return failf(ReasonWriteFailed, "skill %s create %s: %v", t.Name, destination, err)
	}
	rootFd, err := unix.Openat(parentFd, base, openDirFlags, 0)
	if err != nil {
		return failf(ReasonWriteFailed, "skill %s open %s: %v", t.Name, destination, err)
	}
	defer unix.Close(rootFd) //nolint:errcheck
	for _, dir := range t.Dirs {
		if err := withParent(rootFd, dir, func(fd int, name string) error {
			if err := unix.Mkdirat(fd, name, dirMode); err != nil {
				return err
			}
			return unix.Fchmodat(fd, name, dirMode, 0)
		}); err != nil {
			return failf(ReasonWriteFailed, "skill %s create folder %s: %v", t.Name, dir, err)
		}
	}
	if err := unix.Fchmod(rootFd, dirMode); err != nil {
		return failf(ReasonWriteFailed, "skill %s set mode on %s: %v", t.Name, destination, err)
	}
	for _, file := range t.Files {
		mode := uint32(fileMode)
		if file.Executable {
			mode = execMode
		}
		if err := withParent(rootFd, file.Path, func(fd int, name string) error {
			return writeFileAt(fd, name, mode, file.Data)
		}); err != nil {
			return failf(ReasonWriteFailed, "skill %s write %s: %v", t.Name, file.Path, err)
		}
	}
	return nil
}

// withParent opens the parent folder of a relative path component by
// component with O_NOFOLLOW and calls fn with it and the final name.
func withParent(rootFd int, relative string, fn func(fd int, name string) error) error {
	components := strings.Split(relative, "/")
	fd, err := unix.Dup(rootFd)
	if err != nil {
		return err
	}
	for _, component := range components[:len(components)-1] {
		next, openErr := unix.Openat(fd, component, openDirFlags, 0)
		_ = unix.Close(fd)
		if openErr != nil {
			return openErr
		}
		fd = next
	}
	defer unix.Close(fd) //nolint:errcheck
	return fn(fd, components[len(components)-1])
}

func writeFileAt(dirFd int, name string, mode uint32, data []byte) error {
	fd, err := unix.Openat(dirFd, name, createFlags, mode)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), name)
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Chmod(os.FileMode(mode)); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}
