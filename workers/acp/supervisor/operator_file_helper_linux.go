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
//
// Confinement does not depend on walking path components: the session owns
// the directories it would walk and can rename them concurrently. The
// workspace root is opened without following symbolic links and must be the
// exact directory the supervisor recorded before the session child started.
// Every other path is resolved by the kernel in one openat2 call beneath that
// root (RESOLVE_BENEATH, no symbolic links, magic links, or mount crossings),
// which fails a resolution whose result is not beneath the root, including
// one raced by a rename. Without openat2 the helper refuses.
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

// operatorFileHelperResolvedHook runs after the helper resolves the target's
// parent directory and before it opens or creates the final entry. Tests use
// it to move directories at the worst moment.
var operatorFileHelperResolvedHook func()

func operatorFileHelperResolved() {
	if hook := operatorFileHelperResolvedHook; hook != nil {
		hook()
	}
}

func writeOperatorFileStatus(stdout io.Writer, status operatorFileStatus) {
	raw, _ := json.Marshal(status)
	_, _ = stdout.Write(append(raw, '\n'))
}

func operatorFileFailure(code, message string) *operatorFileStatus {
	return &operatorFileStatus{Code: code, Error: message}
}

func runOperatorFileHelper(args []string, stdin io.Reader, stdout io.Writer) *operatorFileStatus {
	if len(args) != 5 {
		return operatorFileFailure(operatorFileCodeInvalid, "invalid operator file helper invocation")
	}
	mode, root, rawIdentity, rel, rawBound := args[0], args[1], args[2], args[3], args[4]
	bound, err := strconv.ParseInt(rawBound, 10, 64)
	if err != nil || bound < 0 || bound > operatoraccess.MaxFileBytes {
		return operatorFileFailure(operatorFileCodeInvalid, "invalid size bound")
	}
	identity, ok := parseOperatorWorkspaceIdentity(rawIdentity)
	if !ok {
		return operatorFileFailure(operatorFileCodeInvalid, "invalid workspace identity")
	}
	cleaned, err := operatoraccess.CleanWorkspacePath(rel)
	if err != nil || cleaned != rel || !path.IsAbs(root) {
		return operatorFileFailure(operatorFileCodeInvalid, "invalid workspace path")
	}
	rootFD, failure := openOperatorWorkspaceRoot(root, identity)
	if failure != nil {
		return failure
	}
	defer unix.Close(rootFD) //nolint:errcheck
	switch mode {
	case OperatorFileHelperRead:
		return readOperatorFile(rootFD, cleaned, bound, stdout)
	case OperatorFileHelperWrite:
		return writeOperatorFile(rootFD, cleaned, bound, stdin, stdout)
	default:
		return operatorFileFailure(operatorFileCodeInvalid, "invalid operator file mode")
	}
}

func mapOperatorOpenError(err error) *operatorFileStatus {
	switch {
	case errors.Is(err, unix.ENOENT):
		return operatorFileFailure(operatorFileCodeNotFound, "no such file or directory")
	case errors.Is(err, unix.ELOOP), errors.Is(err, unix.ENOTDIR), errors.Is(err, unix.EXDEV):
		return operatorFileFailure(operatorFileCodeNotRegular, "path crosses a symbolic link, a non-directory, or the workspace boundary")
	case errors.Is(err, unix.EACCES), errors.Is(err, unix.EPERM), errors.Is(err, unix.EROFS):
		return operatorFileFailure(operatorFileCodeDenied, "permission denied")
	case errors.Is(err, unix.ENXIO), errors.Is(err, unix.EISDIR):
		return operatorFileFailure(operatorFileCodeNotRegular, "not a regular file")
	case errors.Is(err, unix.EAGAIN):
		return operatorFileFailure(operatorFileCodeIO, "the workspace changed during the copy; retry")
	case errors.Is(err, unix.ENOSYS):
		return operatorFileFailure(operatorFileCodeIO, "operator file copies require openat2 (Linux 5.6 or later)")
	default:
		return operatorFileFailure(operatorFileCodeIO, "file operation failed")
	}
}

// operatorWorkspaceIdentity is the device and inode of the workspace
// directory, recorded by the supervisor before the session child started.
type operatorWorkspaceIdentity struct{ dev, ino uint64 }

func (i operatorWorkspaceIdentity) String() string {
	return strconv.FormatUint(i.dev, 10) + ":" + strconv.FormatUint(i.ino, 10)
}

func parseOperatorWorkspaceIdentity(raw string) (operatorWorkspaceIdentity, bool) {
	rawDev, rawIno, found := strings.Cut(raw, ":")
	if !found {
		return operatorWorkspaceIdentity{}, false
	}
	dev, devErr := strconv.ParseUint(rawDev, 10, 64)
	ino, inoErr := strconv.ParseUint(rawIno, 10, 64)
	if devErr != nil || inoErr != nil || ino == 0 {
		return operatorWorkspaceIdentity{}, false
	}
	return operatorWorkspaceIdentity{dev: dev, ino: ino}, true
}

// openOperatorWorkspaceRoot opens the workspace root without following a
// symbolic link and proves it is the recorded directory. The child owns its
// session root, so it could rename the workspace aside and put its private
// home, or any directory it can read, in its place. The comparison is sound
// only because the supervisor holds the recorded directory open for the
// session's lifetime (acp.RecordWorkspaceIdentity): otherwise the child could
// delete the workspace and be given its inode number for a new directory.
func openOperatorWorkspaceRoot(root string, identity operatorWorkspaceIdentity) (int, *operatorFileStatus) {
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, mapOperatorOpenError(err)
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = unix.Close(fd)
		return -1, mapOperatorOpenError(err)
	}
	if uint64(stat.Dev) != identity.dev || stat.Ino != identity.ino { //nolint:unconvert // Dev is uint32 on some Linux architectures.
		_ = unix.Close(fd)
		return -1, operatorFileFailure(operatorFileCodeDenied, "the workspace directory was replaced")
	}
	return fd, nil
}

const (
	operatorResolveBeneath = unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_XDEV
	// operatorResolveAttempts bounds retries of a resolution the kernel
	// abandoned because a rename or mount raced it.
	operatorResolveAttempts = 8
)

// openOperatorBeneath resolves rel beneath rootFD in one kernel call. The
// kernel refuses any component that is a symbolic link, a magic link, or a
// mount crossing, and fails the resolution if its result is not beneath the
// root when it completes, which is what a concurrent rename of an
// intermediate directory out of the workspace produces.
func openOperatorBeneath(rootFD int, rel string, flags int, mode uint32) (int, error) {
	how := unix.OpenHow{Flags: uint64(flags | unix.O_CLOEXEC | unix.O_NOFOLLOW), Mode: uint64(mode), Resolve: operatorResolveBeneath}
	for range operatorResolveAttempts {
		fd, err := unix.Openat2(rootFD, rel, &how)
		if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
			continue
		}
		return fd, err
	}
	return -1, unix.EAGAIN
}

func sameOperatorInode(a, b *unix.Stat_t) bool { return a.Dev == b.Dev && a.Ino == b.Ino }

// verifyOperatorParent proves that the parent directory held by dirFD is
// still what rel's parent resolves to beneath the root.
func verifyOperatorParent(rootFD int, parent string, dirFD int) error {
	current, err := openOperatorBeneath(rootFD, parent, unix.O_PATH|unix.O_DIRECTORY, 0)
	if err != nil {
		return err
	}
	defer unix.Close(current) //nolint:errcheck
	var held, resolved unix.Stat_t
	if err := unix.Fstat(dirFD, &held); err != nil {
		return err
	}
	if err := unix.Fstat(current, &resolved); err != nil {
		return err
	}
	if !sameOperatorInode(&held, &resolved) {
		return unix.EXDEV
	}
	return nil
}

func readOperatorFile(rootFD int, rel string, bound int64, stdout io.Writer) *operatorFileStatus {
	operatorFileHelperResolved()
	fd, err := openOperatorBeneath(rootFD, rel, unix.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return mapOperatorOpenError(err)
	}
	file := os.NewFile(uintptr(fd), path.Base(rel))
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

func writeOperatorFile(rootFD int, rel string, size int64, stdin io.Reader, stdout io.Writer) *operatorFileStatus {
	parent, name := path.Dir(rel), path.Base(rel)
	dirFD, err := openOperatorBeneath(rootFD, parent, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return mapOperatorOpenError(err)
	}
	defer unix.Close(dirFD) //nolint:errcheck
	operatorFileHelperResolved()
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
	// The helper inherits the supervisor's restrictive umask, which would
	// narrow the creation mode. Apply the intended mode explicitly so a
	// replaced file keeps its permissions and a new file gets 0644.
	if err := unix.Fchmod(fd, perm); err != nil {
		return mapOperatorOpenError(err)
	}
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
	// The session can move the parent directory out of the workspace while
	// the upload streams. Commit only into the directory the path still
	// resolves to beneath the root.
	if err := verifyOperatorParent(rootFD, parent, dirFD); err != nil {
		return mapOperatorOpenError(err)
	}
	if err := unix.Renameat(dirFD, temporary, dirFD, name); err != nil {
		return mapOperatorOpenError(err)
	}
	committed = true
	// A move racing the instant between the check and the rename is caught
	// here. The copy is then reported as failed; a newly created entry is
	// withdrawn. Copy in requires the class exec level, which already lets
	// the operator write anywhere the session child can, so this window
	// cannot exceed the operator's authority.
	if err := verifyOperatorParent(rootFD, parent, dirFD); err != nil {
		if existing.Ino == 0 {
			var placed unix.Stat_t
			var created unix.Stat_t
			if unix.Fstatat(dirFD, name, &placed, unix.AT_SYMLINK_NOFOLLOW) == nil &&
				unix.Fstat(fd, &created) == nil && sameOperatorInode(&placed, &created) {
				_ = unix.Unlinkat(dirFD, name, 0)
			}
		}
		return mapOperatorOpenError(err)
	}
	writeOperatorFileStatus(stdout, operatorFileStatus{OK: true, Size: written})
	return nil
}
