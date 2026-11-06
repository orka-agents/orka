//go:build linux

package supervisor

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/orka-agents/orka/internal/operatoraccess"
)

// RunOperatorFileHelper implements the hidden file-copy subcommand. It runs
// as the RuntimeSession child identity (launched through the ACP exec
// helper), never as the supervisor, so ordinary file permissions, read-only
// workspace protection, and ownership of created files match the agent's own.
// Every path component is opened with O_NOFOLLOW relative to the workspace
// root, so neither symbolic links nor traversal can leave the workspace.
//
// Protocol: one JSON status line on stdout, followed for reads by exactly
// status.size bytes of file content.
func RunOperatorFileHelper(args []string, stdin io.Reader, stdout io.Writer) int {
	status := runOperatorFileHelper(args, stdin, stdout)
	if status != nil {
		writeOperatorFileStatus(stdout, *status)
		return 1
	}
	return 0
}

func writeOperatorFileStatus(stdout io.Writer, status operatorFileStatus) {
	raw, _ := json.Marshal(status)
	_, _ = stdout.Write(append(raw, '\n'))
}

func operatorFileFailure(code, message string) *operatorFileStatus {
	return &operatorFileStatus{Code: code, Error: message}
}

func runOperatorFileHelper(args []string, stdin io.Reader, stdout io.Writer) *operatorFileStatus {
	if len(args) != 4 {
		return operatorFileFailure(operatorFileCodeInvalid, "invalid operator file helper invocation")
	}
	mode, root, rel, rawBound := args[0], args[1], args[2], args[3]
	bound, err := strconv.ParseInt(rawBound, 10, 64)
	if err != nil || bound < 0 || bound > operatoraccess.MaxFileBytes {
		return operatorFileFailure(operatorFileCodeInvalid, "invalid size bound")
	}
	cleaned, err := operatoraccess.CleanWorkspacePath(rel)
	if err != nil || cleaned != rel || !path.IsAbs(root) {
		return operatorFileFailure(operatorFileCodeInvalid, "invalid workspace path")
	}
	dirFD, name, failure := openOperatorParent(root, cleaned)
	if failure != nil {
		return failure
	}
	defer unix.Close(dirFD) //nolint:errcheck
	switch mode {
	case OperatorFileHelperRead:
		return readOperatorFile(dirFD, name, bound, stdout)
	case OperatorFileHelperWrite:
		return writeOperatorFile(dirFD, name, bound, stdin, stdout)
	default:
		return operatorFileFailure(operatorFileCodeInvalid, "invalid operator file mode")
	}
}

func mapOperatorOpenError(err error) *operatorFileStatus {
	switch {
	case errors.Is(err, unix.ENOENT):
		return operatorFileFailure(operatorFileCodeNotFound, "no such file or directory")
	case errors.Is(err, unix.ELOOP), errors.Is(err, unix.ENOTDIR):
		return operatorFileFailure(operatorFileCodeNotRegular, "path crosses a symbolic link or non-directory")
	case errors.Is(err, unix.EACCES), errors.Is(err, unix.EPERM), errors.Is(err, unix.EROFS):
		return operatorFileFailure(operatorFileCodeDenied, "permission denied")
	case errors.Is(err, unix.ENXIO), errors.Is(err, unix.EISDIR):
		return operatorFileFailure(operatorFileCodeNotRegular, "not a regular file")
	default:
		return operatorFileFailure(operatorFileCodeIO, "file operation failed")
	}
}

// openOperatorParent walks every directory component without following
// symbolic links and returns the parent directory and the final name.
func openOperatorParent(root, rel string) (int, string, *operatorFileStatus) {
	dirFD, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, "", mapOperatorOpenError(err)
	}
	components := strings.Split(rel, "/")
	for _, component := range components[:len(components)-1] {
		if component == "" || component == "." || component == ".." {
			_ = unix.Close(dirFD)
			return -1, "", operatorFileFailure(operatorFileCodeInvalid, "invalid workspace path")
		}
		next, err := unix.Openat(dirFD, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		_ = unix.Close(dirFD)
		if err != nil {
			return -1, "", mapOperatorOpenError(err)
		}
		dirFD = next
	}
	name := components[len(components)-1]
	if name == "" || name == "." || name == ".." {
		_ = unix.Close(dirFD)
		return -1, "", operatorFileFailure(operatorFileCodeInvalid, "invalid workspace path")
	}
	return dirFD, name, nil
}

func readOperatorFile(dirFD int, name string, bound int64, stdout io.Writer) *operatorFileStatus {
	fd, err := unix.Openat(dirFD, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return mapOperatorOpenError(err)
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close() //nolint:errcheck
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return mapOperatorOpenError(err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return operatorFileFailure(operatorFileCodeNotRegular, "not a regular file")
	}
	if stat.Size > bound {
		return operatorFileFailure(operatorFileCodeTooLarge, fmt.Sprintf("file is %d bytes; the bound is %d", stat.Size, bound))
	}
	writeOperatorFileStatus(stdout, operatorFileStatus{OK: true, Size: stat.Size})
	if _, err := io.CopyN(stdout, file, stat.Size); err != nil {
		// The status line is already sent; the supervisor detects the short
		// copy and aborts the response.
		os.Exit(1)
	}
	return nil
}

func writeOperatorFile(dirFD int, name string, size int64, stdin io.Reader, stdout io.Writer) *operatorFileStatus {
	var existing unix.Stat_t
	perm := uint32(0o644)
	switch err := unix.Fstatat(dirFD, name, &existing, unix.AT_SYMLINK_NOFOLLOW); {
	case err == nil:
		if existing.Mode&unix.S_IFMT != unix.S_IFREG {
			return operatorFileFailure(operatorFileCodeNotRegular, "destination exists and is not a regular file")
		}
		perm = existing.Mode & 0o777
	case errors.Is(err, unix.ENOENT):
	default:
		return mapOperatorOpenError(err)
	}
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		return operatorFileFailure(operatorFileCodeIO, "generate temporary name")
	}
	temporary := ".orka-cp-" + hex.EncodeToString(suffix)
	fd, err := unix.Openat(dirFD, temporary, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, perm)
	if err != nil {
		return mapOperatorOpenError(err)
	}
	file := os.NewFile(uintptr(fd), temporary)
	committed := false
	defer func() {
		_ = file.Close()
		if !committed {
			_ = unix.Unlinkat(dirFD, temporary, 0)
		}
	}()
	written, err := io.Copy(file, io.LimitReader(stdin, size+1))
	if err != nil {
		return operatorFileFailure(operatorFileCodeIO, "write failed")
	}
	if written > size {
		return operatorFileFailure(operatorFileCodeTooLarge, "upload exceeds the declared size")
	}
	if written != size {
		return operatorFileFailure(operatorFileCodeInvalid, "upload ended before the declared size")
	}
	if err := file.Sync(); err != nil {
		return operatorFileFailure(operatorFileCodeIO, "sync failed")
	}
	if err := unix.Renameat(dirFD, temporary, dirFD, name); err != nil {
		return mapOperatorOpenError(err)
	}
	committed = true
	writeOperatorFileStatus(stdout, operatorFileStatus{OK: true, Size: written})
	return nil
}
