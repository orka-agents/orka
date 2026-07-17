//go:build !windows

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

type secureFileMetadata struct {
	Size    int64
	Mode    uint32
	ModTime time.Time
}

func secureWriteFile(
	requested string,
	data []byte,
	mode os.FileMode,
	setOwner bool,
	uid, gid uint32,
	modTime time.Time,
) (secureFileMetadata, error) {
	parentFD, name, err := secureParentFD(requested, true, setOwner, uid, gid)
	if err != nil {
		return secureFileMetadata{}, err
	}
	defer unix.Close(parentFD) //nolint:errcheck
	created := true
	fd, err := unix.Openat(
		parentFD,
		name,
		unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK,
		uint32(mode.Perm()),
	)
	if errors.Is(err, unix.EEXIST) {
		created = false
		fd, err = unix.Openat(
			parentFD,
			name,
			unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK,
			0,
		)
	}
	if err != nil {
		return secureFileMetadata{}, err
	}
	if err := validateSecureRegularFile(fd, true); err != nil {
		_ = unix.Close(fd)
		return secureFileMetadata{}, err
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		_ = unix.Close(fd)
		return secureFileMetadata{}, fmt.Errorf("create file handle")
	}
	defer file.Close() //nolint:errcheck

	contentMatches := false
	if !created {
		metadata, err := secureMetadata(fd)
		if err != nil {
			return secureFileMetadata{}, err
		}
		if metadata.Size == int64(len(data)) {
			hash := sha256.New()
			if _, err := io.Copy(hash, file); err != nil {
				return secureFileMetadata{}, err
			}
			expected := sha256.Sum256(data)
			contentMatches = subtleStringEqual(
				hex.EncodeToString(hash.Sum(nil)),
				hex.EncodeToString(expected[:]),
			)
		}
	}
	if !contentMatches {
		if err := unix.Ftruncate(fd, 0); err != nil {
			return secureFileMetadata{}, err
		}
		if _, err := unix.Seek(fd, 0, io.SeekStart); err != nil {
			return secureFileMetadata{}, err
		}
		if _, err := file.Write(data); err != nil {
			return secureFileMetadata{}, err
		}
	}
	if err := unix.Fchmod(fd, uint32(mode.Perm())); err != nil {
		return secureFileMetadata{}, err
	}
	if setOwner {
		if err := unix.Fchown(fd, int(uid), int(gid)); err != nil {
			return secureFileMetadata{}, err
		}
	}
	if !modTime.IsZero() {
		times := []unix.Timeval{
			unix.NsecToTimeval(modTime.UnixNano()),
			unix.NsecToTimeval(modTime.UnixNano()),
		}
		if err := unix.Futimes(fd, times); err != nil {
			return secureFileMetadata{}, err
		}
	}
	return secureMetadata(fd)
}

func secureReadFile(requested string, maxBytes int64) ([]byte, secureFileMetadata, error) {
	if maxBytes < 0 {
		return nil, secureFileMetadata{}, errWorkspaceFileTooLarge
	}
	fd, err := secureOpenFile(requested, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW)
	if err != nil {
		return nil, secureFileMetadata{}, err
	}
	file := os.NewFile(uintptr(fd), filepath.Base(requested))
	if file == nil {
		_ = unix.Close(fd)
		return nil, secureFileMetadata{}, fmt.Errorf("create file handle")
	}
	defer file.Close() //nolint:errcheck
	metadata, err := secureMetadata(fd)
	if err != nil {
		return nil, secureFileMetadata{}, err
	}
	if metadata.Size < 0 || metadata.Size > maxBytes {
		return nil, secureFileMetadata{}, errWorkspaceFileTooLarge
	}
	limit := maxBytes + 1
	if maxBytes == int64(^uint64(0)>>1) {
		limit = maxBytes
	}
	data, err := io.ReadAll(io.LimitReader(file, limit))
	if err == nil && int64(len(data)) > maxBytes {
		err = errWorkspaceFileTooLarge
	}
	return data, metadata, err
}

func secureRemoveAll(requested string) error {
	return secureRemoveAllContext(context.Background(), requested)
}

func secureRemoveAllContext(ctx context.Context, requested string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, parts, _, pathErr := securePathParts(requested)
	if pathErr != nil {
		return pathErr
	}
	if len(parts) == 0 {
		return secureResetDirectoryContext(ctx, requested, false, 0, 0, nil)
	}
	parentFD, name, err := secureParentFD(requested, false, false, 0, 0)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	defer unix.Close(parentFD) //nolint:errcheck
	entries := 0
	return secureRemoveAtDepth(ctx, parentFD, name, 0, &entries)
}

func secureResetDirectory(requested string, setOwner bool, uid, gid uint32, protected []string) error {
	return secureResetDirectoryContext(context.Background(), requested, setOwner, uid, gid, protected)
}

func secureResetDirectoryContext(
	ctx context.Context,
	requested string,
	setOwner bool,
	uid, gid uint32,
	protected []string,
) error {
	root, parts, clean, err := securePathParts(requested)
	if err != nil {
		return err
	}
	fd, err := openSecureDirectory(root, parts, true, setOwner, uid, gid)
	if err != nil {
		return err
	}
	exactRoot := ""
	if len(parts) == 0 {
		exactRoot, err = logicalAllowedRoot(requested)
		if err != nil {
			_ = unix.Close(fd)
			return err
		}
	}
	if err := resetRootMetadata(fd, exactRoot, setOwner, uid, gid); err != nil {
		_ = unix.Close(fd)
		return err
	}
	defer unix.Close(fd) //nolint:errcheck
	entries := 0
	return forEachDirectoryName(ctx, fd, requested, func(name string) error {
		path := filepath.Join(clean, name)
		return secureRemoveAtExceptDepth(ctx, fd, name, path, protected, 1, &entries)
	})
}

func resetRootMetadata(fd int, path string, setOwner bool, uid, gid uint32) error {
	mode := uint32(0o755)
	setCommandOwner := setOwner
	switch filepath.Clean(path) {
	case "/app":
		setCommandOwner = false
	case "/tmp", "/dev/shm":
		mode = 0o1777
		setCommandOwner = false
	}
	if err := unix.Fchmod(fd, mode); err != nil {
		return err
	}
	if setCommandOwner {
		return unix.Fchown(fd, int(uid), int(gid))
	}
	return nil
}

func secureListFiles(requested string) ([]string, error) {
	root, parts, clean, err := securePathParts(requested)
	if err != nil {
		return nil, err
	}
	fd, err := openSecureDirectory(root, parts, false, false, 0, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd) //nolint:errcheck
	entries := 0
	return secureListAt(fd, clean, 0, &entries)
}

func secureOpenFile(requested string, flags int) (int, error) {
	parentFD, name, err := secureParentFD(requested, false, false, 0, 0)
	if err != nil {
		return -1, err
	}
	defer unix.Close(parentFD) //nolint:errcheck
	fd, err := unix.Openat(parentFD, name, flags|unix.O_NONBLOCK, 0)
	if err != nil {
		return -1, err
	}
	if err := validateSecureRegularFile(fd, true); err != nil {
		_ = unix.Close(fd)
		return -1, err
	}
	return fd, nil
}

func secureParentFD(
	requested string,
	createParents bool,
	setOwner bool,
	uid, gid uint32,
) (int, string, error) {
	root, parts, _, err := securePathParts(requested)
	if err != nil {
		return -1, "", err
	}
	if len(parts) == 0 {
		return -1, "", fmt.Errorf("file path must not be an allowed root")
	}
	parentFD, err := openSecureDirectory(root, parts[:len(parts)-1], createParents, setOwner, uid, gid)
	if err != nil {
		return -1, "", err
	}
	return parentFD, parts[len(parts)-1], nil
}

func openSecureDirectory(
	root string,
	parts []string,
	create bool,
	setOwner bool,
	uid, gid uint32,
) (int, error) {
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, err
	}
	for _, part := range parts {
		created := false
		next, openErr := unix.Openat(
			fd,
			part,
			unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
			0,
		)
		if openErr != nil && create && errors.Is(openErr, unix.ENOENT) {
			if err := unix.Mkdirat(fd, part, 0o755); err != nil && !errors.Is(err, unix.EEXIST) {
				_ = unix.Close(fd)
				return -1, err
			}
			created = true
			next, openErr = unix.Openat(
				fd,
				part,
				unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
				0,
			)
		}
		if openErr != nil {
			_ = unix.Close(fd)
			return -1, openErr
		}
		if created && setOwner {
			if err := unix.Fchown(next, int(uid), int(gid)); err != nil {
				_ = unix.Close(next)
				_ = unix.Close(fd)
				return -1, err
			}
		}
		_ = unix.Close(fd)
		fd = next
	}
	return fd, nil
}

func secureRemoveAtExceptDepth(
	ctx context.Context,
	parentFD int,
	name string,
	path string,
	protected []string,
	depth int,
	entries *int,
) error {
	if err := consumeWorkspaceTreeEntry(ctx, depth, entries); err != nil {
		return err
	}
	if protectedPath(path, protected) {
		return nil
	}
	if !hasProtectedDescendant(path, protected) {
		return secureRemoveAtDepthCounted(ctx, parentFD, name, depth, entries)
	}
	var stat unix.Stat_t
	if err := unix.Fstatat(parentFD, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return secureRemoveAtDepthCounted(ctx, parentFD, name, depth, entries)
	}
	fd, err := unix.Openat(
		parentFD,
		name,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		return err
	}
	defer unix.Close(fd) //nolint:errcheck
	return forEachDirectoryName(ctx, fd, name, func(child string) error {
		return secureRemoveAtExceptDepth(
			ctx, fd, child, filepath.Join(path, child), protected, depth+1, entries,
		)
	})
}

func protectedPath(path string, protected []string) bool {
	path = filepath.Clean(path)
	for _, candidate := range protected {
		if path == filepath.Clean(candidate) {
			return true
		}
	}
	return false
}

func hasProtectedDescendant(path string, protected []string) bool {
	path = filepath.Clean(path) + string(os.PathSeparator)
	for _, candidate := range protected {
		if strings.HasPrefix(filepath.Clean(candidate), path) {
			return true
		}
	}
	return false
}

func secureRemoveAtDepth(
	ctx context.Context,
	parentFD int,
	name string,
	depth int,
	entries *int,
) error {
	if err := consumeWorkspaceTreeEntry(ctx, depth, entries); err != nil {
		return err
	}
	return secureRemoveAtDepthCounted(ctx, parentFD, name, depth, entries)
}

func secureRemoveAtDepthCounted(
	ctx context.Context,
	parentFD int,
	name string,
	depth int,
	entries *int,
) error {
	var stat unix.Stat_t
	if err := unix.Fstatat(parentFD, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return unix.Unlinkat(parentFD, name, 0)
	}
	fd, err := unix.Openat(
		parentFD,
		name,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		return err
	}
	if err := forEachDirectoryName(ctx, fd, name, func(child string) error {
		return secureRemoveAtDepth(ctx, fd, child, depth+1, entries)
	}); err != nil {
		_ = unix.Close(fd)
		return err
	}
	if err := unix.Close(fd); err != nil {
		return err
	}
	return unix.Unlinkat(parentFD, name, unix.AT_REMOVEDIR)
}

func secureListAt(
	dirFD int,
	prefix string,
	depth int,
	entries *int,
) ([]string, error) {
	if depth > maxWorkspaceTreeDepth {
		return nil, errWorkspaceTreeTooDeep
	}
	var paths []string
	err := forEachDirectoryName(context.Background(), dirFD, prefix, func(name string) error {
		(*entries)++
		if *entries > maxWorkspaceTreeEntries {
			return errWorkspaceTreeTooLarge
		}
		var stat unix.Stat_t
		if err := unix.Fstatat(dirFD, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return err
		}
		path := filepath.Join(prefix, name)
		if stat.Mode&unix.S_IFMT == unix.S_IFDIR {
			childFD, err := unix.Openat(
				dirFD,
				name,
				unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
				0,
			)
			if err != nil {
				return err
			}
			children, err := secureListAt(childFD, path, depth+1, entries)
			_ = unix.Close(childFD)
			if err != nil {
				return err
			}
			paths = append(paths, children...)
			return nil
		}
		if stat.Mode&unix.S_IFMT == unix.S_IFREG {
			paths = append(paths, path)
		}
		return nil
	})
	return paths, err
}

func consumeWorkspaceTreeEntry(ctx context.Context, depth int, entries *int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if depth > maxWorkspaceTreeDepth {
		return errWorkspaceTreeTooDeep
	}
	(*entries)++
	if *entries > maxWorkspaceTreeEntries {
		return errWorkspaceTreeTooLarge
	}
	return nil
}

func forEachDirectoryName(ctx context.Context, dirFD int, label string, visit func(string) error) error {
	dupFD, err := unix.Dup(dirFD)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(dupFD), label)
	if file == nil {
		_ = unix.Close(dupFD)
		return fmt.Errorf("create directory handle")
	}
	defer file.Close() //nolint:errcheck
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		names, readErr := file.Readdirnames(workspaceDirectoryBatchSize)
		for _, name := range names {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := visit(name); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

func validateSecureRegularFile(fd int, requireSingleLink bool) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("path is not a regular file")
	}
	if requireSingleLink && stat.Nlink != 1 {
		return fmt.Errorf("refusing to modify a multiply-linked file")
	}
	return nil
}

func secureMetadata(fd int) (secureFileMetadata, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return secureFileMetadata{}, err
	}
	return secureFileMetadata{
		Size:    stat.Size,
		Mode:    uint32(stat.Mode & 0o777),
		ModTime: time.Unix(stat.Mtim.Sec, stat.Mtim.Nsec),
	}, nil
}

func securePathParts(value string) (string, []string, string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil, "", fmt.Errorf("path is required")
	}
	if !filepath.IsAbs(value) {
		value = filepath.Join("/app", value)
	}
	clean := filepath.Clean(value)
	for _, candidate := range allowedRoots {
		originalRoot := filepath.Clean(candidate)
		resolvedRoot := originalRoot
		if resolved, err := filepath.EvalSymlinks(originalRoot); err == nil {
			resolvedRoot = filepath.Clean(resolved)
		}
		rootForRelative := ""
		switch {
		case pathWithinRoot(clean, originalRoot):
			rootForRelative = originalRoot
		case pathWithinRoot(clean, resolvedRoot):
			rootForRelative = resolvedRoot
		default:
			continue
		}
		relative, err := filepath.Rel(rootForRelative, clean)
		if err != nil {
			return "", nil, "", err
		}
		canonical := filepath.Join(resolvedRoot, relative)
		if relative == "." {
			return resolvedRoot, nil, resolvedRoot, nil
		}
		parts := strings.Split(relative, string(os.PathSeparator))
		for _, part := range parts {
			if part == "" || part == "." || part == ".." {
				return "", nil, "", fmt.Errorf("invalid path component")
			}
		}
		return resolvedRoot, parts, canonical, nil
	}
	return "", nil, "", fmt.Errorf("path escapes allowed roots")
}

func logicalAllowedRoot(value string) (string, error) {
	value = strings.TrimSpace(value)
	if !filepath.IsAbs(value) {
		value = filepath.Join("/app", value)
	}
	clean := filepath.Clean(value)
	for _, candidate := range allowedRoots {
		root := filepath.Clean(candidate)
		if pathWithinRoot(clean, root) {
			return root, nil
		}
	}
	return "", fmt.Errorf("path escapes allowed roots")
}

func securePathAllowed(path string) bool {
	_, _, _, err := securePathParts(path)
	return err == nil
}

func workspaceFileErrorStatus(err error) int {
	if errors.Is(err, errWorkspaceFileTooLarge) || errors.Is(err, errWorkspaceTreeTooLarge) ||
		errors.Is(err, errWorkspaceTreeTooDeep) {
		return http.StatusRequestEntityTooLarge
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return http.StatusRequestTimeout
	}
	for _, invalid := range []error{
		unix.ENOENT, unix.ENOTDIR, unix.EISDIR, unix.ELOOP, unix.EINVAL, unix.EXDEV, unix.EMLINK,
	} {
		if errors.Is(err, invalid) {
			return http.StatusBadRequest
		}
	}
	return http.StatusInternalServerError
}
