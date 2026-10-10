//go:build unix

package toolbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	openDirFlags  = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	openFileFlags = unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC | unix.O_NONBLOCK | unix.O_NOCTTY
	createFlags   = unix.O_WRONLY | unix.O_CREAT | unix.O_EXCL | unix.O_NOFOLLOW | unix.O_CLOEXEC
	maxLinkBytes  = 4096
	maxLinkHops   = 8
)

type copyState struct {
	limits  Limits
	entries int64
	bytes   int64
	buffer  []byte
}

// Copy sanitizes Source from the toolbox image into Destination/root. It is
// idempotent across init-container retries: a finished copy is skipped and a
// partial one is discarded and redone.
func Copy(opts Options) (Summary, error) {
	arch, err := validateCopyOptions(opts)
	if err != nil {
		return Summary{}, err
	}
	// The sanitized modes below are exact; never let an inherited umask
	// narrow them below what the agent identities need.
	unix.Umask(0o022)

	dstFd, err := unix.Open(opts.Destination, openDirFlags, 0)
	if err != nil {
		return Summary{}, failf(ReasonCopyFailed, "open destination %s: %v", opts.Destination, err)
	}
	defer unix.Close(dstFd) //nolint:errcheck

	if summary, done := completedSummary(dstFd, arch); done {
		return summary, nil
	}
	// Discard any partial state from an interrupted run: the staging tree,
	// and a root folder whose completion marker was never written.
	for _, stale := range []string{stagingName, OutputRootName} {
		if err := os.RemoveAll(filepath.Join(opts.Destination, stale)); err != nil {
			return Summary{}, failf(ReasonCopyFailed, "remove stale %s: %v", stale, err)
		}
	}

	srcFd, err := openSourceDirectory(opts.Source)
	if err != nil {
		return Summary{}, err
	}
	defer unix.Close(srcFd) //nolint:errcheck

	if err := unix.Mkdirat(dstFd, stagingName, 0o755); err != nil {
		return Summary{}, failf(ReasonCopyFailed, "create staging folder: %v", err)
	}
	stagingFd, err := unix.Openat(dstFd, stagingName, openDirFlags, 0)
	if err != nil {
		return Summary{}, failf(ReasonCopyFailed, "open staging folder: %v", err)
	}
	defer unix.Close(stagingFd) //nolint:errcheck

	state := &copyState{limits: opts.Limits, buffer: make([]byte, 1<<20)}
	if err := state.copyTree(srcFd, stagingFd, opts.Source, 0); err != nil {
		return Summary{}, err
	}
	for _, entry := range opts.PathEntries {
		if err := requireDirectoryAt(stagingFd, entry); err != nil {
			return Summary{}, failf(ReasonMissingPathEntry, "path entry %s is not a folder in %s: %v", entry, opts.Source, err)
		}
	}
	if err := checkArchInTree(stagingFd, opts.Source, opts.PathEntries, arch); err != nil {
		return Summary{}, err
	}
	summary := Summary{Entries: state.entries, Bytes: state.bytes, Arch: arch}
	if err := publishCopy(dstFd, summary); err != nil {
		return Summary{}, err
	}
	return summary, nil
}

func validateCopyOptions(opts Options) (string, error) {
	arch, err := NormalizeArch(opts.Arch)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(opts.Source) || filepath.Clean(opts.Source) != opts.Source || opts.Source == "/" {
		return "", failf(ReasonInvalidArguments, "source %q must be a clean absolute path below /", opts.Source)
	}
	if !filepath.IsAbs(opts.Destination) || filepath.Clean(opts.Destination) != opts.Destination {
		return "", failf(ReasonInvalidArguments, "destination %q must be a clean absolute path", opts.Destination)
	}
	for _, entry := range opts.PathEntries {
		if entry == "" || path.IsAbs(entry) || path.Clean(entry) != entry || entry == "." || hasDotDot(entry) {
			return "", failf(ReasonInvalidArguments, "path entry %q must be a clean relative path", entry)
		}
	}
	limits := opts.Limits
	if limits.MaxEntries <= 0 || limits.MaxTotalBytes <= 0 || limits.MaxFileBytes <= 0 || limits.MaxDepth <= 0 {
		return "", failf(ReasonInvalidArguments, "every copy limit must be positive")
	}
	return arch, nil
}

// completedSummary reports a finished earlier run from its completion marker.
func completedSummary(dstFd int, arch string) (Summary, bool) {
	var completeStat unix.Stat_t
	if err := unix.Fstatat(dstFd, completeName, &completeStat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return Summary{}, false
	}
	summary := Summary{Arch: arch}
	if data, readErr := readAt(dstFd, completeName, 4096); readErr == nil {
		_ = json.Unmarshal(data, &summary)
	}
	summary.AlreadyComplete = true
	return summary, true
}

// publishCopy renames the finished staging tree to root and then writes the
// completion marker, in that order, so a crash between the two is redone.
func publishCopy(dstFd int, summary Summary) error {
	if err := unix.Renameat(dstFd, stagingName, dstFd, OutputRootName); err != nil {
		return failf(ReasonCopyFailed, "publish copied toolbox: %v", err)
	}
	marker, err := json.Marshal(summary)
	if err != nil {
		return failf(ReasonCopyFailed, "encode completion marker: %v", err)
	}
	if err := writeFileAt(dstFd, completeName, 0o444, marker); err != nil {
		return failf(ReasonCopyFailed, "write completion marker: %v", err)
	}
	return nil
}

// openSourceDirectory opens every component of an absolute path with
// O_NOFOLLOW so a symlink anywhere in the chain fails instead of being
// followed.
func openSourceDirectory(source string) (int, error) {
	fd, err := unix.Open("/", openDirFlags, 0)
	if err != nil {
		return -1, failf(ReasonSourceOpen, "open /: %v", err)
	}
	for component := range strings.SplitSeq(strings.Trim(source, "/"), "/") {
		next, err := unix.Openat(fd, component, openDirFlags, 0)
		_ = unix.Close(fd)
		if err != nil {
			return -1, failf(ReasonSourceOpen, "open %s without following symlinks: %v", source, err)
		}
		fd = next
	}
	return fd, nil
}

func (s *copyState) account(entries, bytes int64) error {
	s.entries += entries
	s.bytes += bytes
	if s.entries > s.limits.MaxEntries {
		return failf(ReasonTooManyEntries, "toolbox has more than %d entries", s.limits.MaxEntries)
	}
	if s.bytes > s.limits.MaxTotalBytes {
		return failf(ReasonTooLarge, "toolbox exceeds %d bytes", s.limits.MaxTotalBytes)
	}
	return nil
}

// directoryBatch bounds how many untrusted directory entries are held in
// memory at once; limits are enforced while streaming, so a folder with
// millions of entries fails with TOOLBOX_TOO_MANY_ENTRIES instead of
// exhausting the copier's memory.
const directoryBatch = 1024

func (s *copyState) copyTree(srcFd, dstFd int, display string, depth int) error {
	if depth > s.limits.MaxDepth {
		return failf(ReasonTooDeep, "%s is deeper than %d folders", display, s.limits.MaxDepth)
	}
	dup, err := unix.Dup(srcFd)
	if err != nil {
		return failf(ReasonCopyFailed, "read %s: %v", display, err)
	}
	dir := os.NewFile(uintptr(dup), display)
	defer dir.Close() //nolint:errcheck
	for {
		entries, err := dir.ReadDir(directoryBatch)
		if len(entries) == 0 && errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return failf(ReasonCopyFailed, "read %s: %v", display, err)
		}
		for _, entry := range entries {
			if err := s.copyEntry(srcFd, dstFd, display, entry.Name(), depth); err != nil {
				return err
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
	}
}

func (s *copyState) copyEntry(srcFd, dstFd int, display, name string, depth int) error {
	{
		entryDisplay := path.Join(display, name)
		var st unix.Stat_t
		if err := unix.Fstatat(srcFd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return failf(ReasonCopyFailed, "stat %s: %v", entryDisplay, err)
		}
		if err := s.account(1, 0); err != nil {
			return err
		}
		switch statMode(&st) & unix.S_IFMT {
		case unix.S_IFDIR:
			if err := s.copyDirectory(srcFd, dstFd, name, entryDisplay, depth); err != nil {
				return err
			}
		case unix.S_IFREG:
			if err := s.copyRegular(srcFd, dstFd, name, entryDisplay, &st); err != nil {
				return err
			}
		case unix.S_IFLNK:
			target, err := readLinkAt(srcFd, name)
			if err != nil {
				return failf(ReasonCopyFailed, "read symlink %s: %v", entryDisplay, err)
			}
			// Link text occupies output space too; count it so the total
			// stays within the volume sized for the copier's limit.
			if err := s.account(0, int64(len(target))); err != nil {
				return err
			}
			if err := unix.Symlinkat(target, dstFd, name); err != nil {
				return failf(ReasonCopyFailed, "write symlink %s: %v", entryDisplay, err)
			}
		default:
			return failf(ReasonUnsupportedFileType, "%s is not a folder, regular file, or symlink", entryDisplay)
		}
	}
	return nil
}

func (s *copyState) copyDirectory(srcFd, dstFd int, name, display string, depth int) error {
	childSrc, err := unix.Openat(srcFd, name, openDirFlags, 0)
	if err != nil {
		return failf(ReasonCopyFailed, "open folder %s: %v", display, err)
	}
	defer unix.Close(childSrc) //nolint:errcheck
	if err := unix.Mkdirat(dstFd, name, 0o755); err != nil {
		return failf(ReasonCopyFailed, "create folder %s: %v", display, err)
	}
	childDst, err := unix.Openat(dstFd, name, openDirFlags, 0)
	if err != nil {
		return failf(ReasonCopyFailed, "open copied folder %s: %v", display, err)
	}
	defer unix.Close(childDst) //nolint:errcheck
	return s.copyTree(childSrc, childDst, display, depth+1)
}

func (s *copyState) copyRegular(srcFd, dstFd int, name, display string, expected *unix.Stat_t) error {
	if expected.Size > s.limits.MaxFileBytes {
		return failf(ReasonFileTooLarge, "%s is larger than %d bytes", display, s.limits.MaxFileBytes)
	}
	fd, err := unix.Openat(srcFd, name, openFileFlags, 0)
	if err != nil {
		return failf(ReasonCopyFailed, "open %s: %v", display, err)
	}
	src := os.NewFile(uintptr(fd), display)
	defer src.Close() //nolint:errcheck
	var opened unix.Stat_t
	if err := unix.Fstat(fd, &opened); err != nil {
		return failf(ReasonCopyFailed, "stat opened %s: %v", display, err)
	}
	if statMode(&opened)&unix.S_IFMT != unix.S_IFREG || opened.Ino != expected.Ino || statDev(&opened) != statDev(expected) {
		return failf(ReasonSourceChanged, "%s changed while it was being copied", display)
	}
	mode := uint32(0o444)
	if statMode(expected)&0o111 != 0 {
		mode = 0o555
	}
	out, err := unix.Openat(dstFd, name, createFlags, mode)
	if err != nil {
		return failf(ReasonCopyFailed, "create %s: %v", display, err)
	}
	dst := os.NewFile(uintptr(out), display)
	// Read one byte past the limit so a file that grew after stat still fails.
	written, copyErr := io.CopyBuffer(dst, io.LimitReader(src, s.limits.MaxFileBytes+1), s.buffer)
	if closeErr := dst.Close(); copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		return failf(ReasonCopyFailed, "copy %s: %v", display, copyErr)
	}
	if written > s.limits.MaxFileBytes {
		return failf(ReasonFileTooLarge, "%s is larger than %d bytes", display, s.limits.MaxFileBytes)
	}
	if err := unix.Fchmodat(dstFd, name, mode, 0); err != nil {
		return failf(ReasonCopyFailed, "set mode on %s: %v", display, err)
	}
	return s.account(0, written)
}

func readDirectoryNames(dirFd int) ([]string, error) {
	dup, err := unix.Dup(dirFd)
	if err != nil {
		return nil, err
	}
	dir := os.NewFile(uintptr(dup), "")
	defer dir.Close() //nolint:errcheck
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names, nil
}

func readLinkAt(dirFd int, name string) (string, error) {
	buf := make([]byte, maxLinkBytes)
	n, err := unix.Readlinkat(dirFd, name, buf)
	if err != nil {
		return "", err
	}
	if n >= len(buf) {
		return "", errors.New("symlink target is too long")
	}
	return string(buf[:n]), nil
}

func readAt(dirFd int, name string, limit int64) ([]byte, error) {
	fd, err := unix.Openat(dirFd, name, openFileFlags, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close() //nolint:errcheck
	return io.ReadAll(io.LimitReader(file, limit))
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
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

// requireDirectoryAt walks a clean relative path component by component with
// O_NOFOLLOW and reports whether it ends at a real folder.
func requireDirectoryAt(rootFd int, relative string) error {
	fd, err := openRelativeDirectory(rootFd, relative)
	if err != nil {
		return err
	}
	return unix.Close(fd)
}

func openRelativeDirectory(rootFd int, relative string) (int, error) {
	fd, err := unix.Dup(rootFd)
	if err != nil {
		return -1, err
	}
	if relative == "." || relative == "" {
		return fd, nil
	}
	for component := range strings.SplitSeq(relative, "/") {
		next, err := unix.Openat(fd, component, openDirFlags, 0)
		_ = unix.Close(fd)
		if err != nil {
			return -1, err
		}
		fd = next
	}
	return fd, nil
}

func hasDotDot(relative string) bool {
	return slices.Contains(strings.Split(relative, "/"), "..")
}

// checkArchInTree verifies that every ELF file directly inside each path
// entry folder matches arch. Symlinks are resolved only while they stay inside
// the toolbox tree (rootFd is the tree root and mountPath its runtime path);
// anything that leaves the tree is skipped because it can never be a toolbox
// tool. The walk never follows a symlink at the kernel level.
func checkArchInTree(rootFd int, mountPath string, pathEntries []string, arch string) error {
	for _, entry := range pathEntries {
		dirFd, err := openRelativeDirectory(rootFd, entry)
		if err != nil {
			return failf(ReasonMissingPathEntry, "path entry %s is not a folder in %s: %v", entry, mountPath, err)
		}
		names, err := readDirectoryNames(dirFd)
		_ = unix.Close(dirFd)
		if err != nil {
			return failf(ReasonCopyFailed, "read path entry %s: %v", entry, err)
		}
		for _, name := range names {
			relative := path.Join(entry, name)
			resolved, ok := resolveInsideTree(rootFd, mountPath, relative)
			if !ok {
				continue
			}
			fileArch, isELF, err := elfArchAt(rootFd, resolved)
			if err != nil {
				return failf(ReasonCopyFailed, "inspect %s: %v", path.Join(mountPath, relative), err)
			}
			if !isELF {
				continue
			}
			if fileArch != arch {
				if fileArch == "" {
					fileArch = "unknown"
				}
				return failf(ReasonArchMismatch, "%s is built for %s but this node runs %s", path.Join(mountPath, relative), fileArch, arch)
			}
		}
	}
	return nil
}

// resolveInsideTree follows symlink text relative to the toolbox tree. It
// returns the relative path of a regular file, or ok=false when the entry is
// not a regular file, escapes the tree, or cannot be inspected.
func resolveInsideTree(rootFd int, mountPath, relative string) (string, bool) {
	current := relative
	for hop := 0; hop <= maxLinkHops; hop++ {
		st, err := lstatRelative(rootFd, current)
		if err != nil {
			return "", false
		}
		switch statMode(&st) & unix.S_IFMT {
		case unix.S_IFREG:
			return current, true
		case unix.S_IFLNK:
			next, ok := nextLinkHop(rootFd, mountPath, current)
			if !ok {
				return "", false
			}
			current = next
		default:
			return "", false
		}
	}
	return "", false
}

// nextLinkHop reads one symlink and maps its text back into the tree.
func nextLinkHop(rootFd int, mountPath, current string) (string, bool) {
	dirFd, err := openRelativeDirectory(rootFd, path.Dir(current))
	if err != nil {
		return "", false
	}
	target, err := readLinkAt(dirFd, path.Base(current))
	_ = unix.Close(dirFd)
	if err != nil {
		return "", false
	}
	var next string
	if path.IsAbs(target) {
		if target != mountPath && !strings.HasPrefix(target, mountPath+"/") {
			return "", false
		}
		next = strings.TrimPrefix(strings.TrimPrefix(target, mountPath), "/")
	} else {
		next = path.Join(path.Dir(current), target)
	}
	next = path.Clean(next)
	if next == "" || next == "." || path.IsAbs(next) || hasDotDot(next) {
		return "", false
	}
	return next, true
}

func lstatRelative(rootFd int, relative string) (unix.Stat_t, error) {
	var st unix.Stat_t
	dirFd, err := openRelativeDirectory(rootFd, path.Dir(relative))
	if err != nil {
		return st, err
	}
	defer unix.Close(dirFd) //nolint:errcheck
	if err := unix.Fstatat(dirFd, path.Base(relative), &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return st, err
	}
	return st, nil
}

func elfArchAt(rootFd int, relative string) (string, bool, error) {
	dirFd, err := openRelativeDirectory(rootFd, path.Dir(relative))
	if err != nil {
		return "", false, err
	}
	defer unix.Close(dirFd) //nolint:errcheck
	fd, err := unix.Openat(dirFd, path.Base(relative), openFileFlags, 0)
	if err != nil {
		return "", false, err
	}
	file := os.NewFile(uintptr(fd), relative)
	defer file.Close() //nolint:errcheck
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return "", false, err
	}
	if statMode(&st)&unix.S_IFMT != unix.S_IFREG {
		return "", false, fmt.Errorf("%s is not a regular file", relative)
	}
	return elfArch(file)
}

// CheckMounted verifies a toolbox that is already mounted at mountPath: every
// ELF file in its path entry folders must match arch. It opens the mount path
// component by component with O_NOFOLLOW and never follows a symlink out of
// the toolbox tree. It runs as an unprivileged user.
func CheckMounted(mountPath string, pathEntries []string, arch string) error {
	arch, err := NormalizeArch(arch)
	if err != nil {
		return err
	}
	if !filepath.IsAbs(mountPath) || filepath.Clean(mountPath) != mountPath || mountPath == "/" {
		return failf(ReasonInvalidArguments, "mount path %q must be a clean absolute path below /", mountPath)
	}
	for _, entry := range pathEntries {
		if entry == "" || path.IsAbs(entry) || path.Clean(entry) != entry || entry == "." || hasDotDot(entry) {
			return failf(ReasonInvalidArguments, "path entry %q must be a clean relative path", entry)
		}
	}
	rootFd, err := openSourceDirectory(mountPath)
	if err != nil {
		return failf(ReasonMissingMount, "%v", err)
	}
	defer unix.Close(rootFd) //nolint:errcheck
	for _, entry := range pathEntries {
		if err := requireDirectoryAt(rootFd, entry); err != nil {
			return failf(ReasonMissingPathEntry, "path entry %s is not a folder in %s: %v", entry, mountPath, err)
		}
	}
	return checkArchInTree(rootFd, mountPath, pathEntries, arch)
}
