package toolbox

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Handoff copies the running binary into dst as HandoffBinaryName with mode
// 0555, writing a temporary name first and renaming so a toolbox-copy
// container never sees a partial file. It needs no supervisor configuration
// and runs as an unprivileged user.
func Handoff(dst string) (string, error) {
	if !filepath.IsAbs(dst) || filepath.Clean(dst) != dst {
		return "", failf(ReasonInvalidArguments, "handoff destination %q must be a clean absolute path", dst)
	}
	self, err := os.Executable()
	if err != nil {
		return "", failf(ReasonCopyFailed, "locate running binary: %v", err)
	}
	source, err := os.Open(self)
	if err != nil {
		return "", failf(ReasonCopyFailed, "open running binary: %v", err)
	}
	defer source.Close() //nolint:errcheck
	final := filepath.Join(dst, HandoffBinaryName)
	temporary := filepath.Join(dst, "."+HandoffBinaryName+".tmp")
	_ = os.Remove(temporary)
	target, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o555)
	if err != nil {
		return "", failf(ReasonCopyFailed, "create %s: %v", temporary, err)
	}
	if _, err := io.Copy(target, source); err != nil {
		_ = target.Close()
		_ = os.Remove(temporary)
		return "", failf(ReasonCopyFailed, "copy running binary: %v", err)
	}
	if err := target.Sync(); err != nil {
		_ = target.Close()
		_ = os.Remove(temporary)
		return "", failf(ReasonCopyFailed, "sync %s: %v", temporary, err)
	}
	if err := target.Close(); err != nil {
		_ = os.Remove(temporary)
		return "", failf(ReasonCopyFailed, "close %s: %v", temporary, err)
	}
	if err := os.Chmod(temporary, 0o555); err != nil {
		_ = os.Remove(temporary)
		return "", failf(ReasonCopyFailed, "set mode on %s: %v", temporary, err)
	}
	if err := os.Rename(temporary, final); err != nil {
		_ = os.Remove(temporary)
		return "", failf(ReasonCopyFailed, "publish %s: %v", final, err)
	}
	return final, nil
}

// worldAccessBits are the permission bits every session identity needs:
// agent processes run as arbitrary UIDs that own nothing in a toolbox and
// belong to none of its groups, so only the "other" bits matter.
const worldAccessBits = os.FileMode(0o005)

// VerifyMounted checks, with lstat only, that each toolbox mount path and
// each of its path entry folders exists, is a real folder, and is readable
// and traversable by every session identity. It never opens, reads, or
// follows anything inside a toolbox, so the root supervisor can call it
// safely.
func VerifyMounted(mountPath string, pathEntries []string) error {
	if err := requireRealDirectory(mountPath); err != nil {
		return failf(ReasonMissingMount, "toolbox %s is not mounted: %v", mountPath, err)
	}
	if err := requireWorldAccessible(mountPath); err != nil {
		return failf(ReasonPermissionDenied, "toolbox %s: %v", mountPath, err)
	}
	for _, entry := range pathEntries {
		dir := filepath.Join(mountPath, entry)
		if err := requireRealDirectory(dir); err != nil {
			return failf(ReasonMissingPathEntry, "toolbox %s path entry %s: %v", mountPath, entry, err)
		}
		if err := requireWorldAccessible(dir); err != nil {
			return failf(ReasonPermissionDenied, "toolbox %s path entry %s: %v", mountPath, entry, err)
		}
	}
	return nil
}

func requireRealDirectory(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink", dir)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a folder", dir)
	}
	return nil
}

// requireWorldAccessible checks the mode bits of an already verified real
// folder, independently of who owns it.
func requireWorldAccessible(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&worldAccessBits != worldAccessBits {
		return fmt.Errorf("%s (mode %04o) is not readable and traversable by every agent identity; it needs o+rx", dir, info.Mode().Perm())
	}
	return nil
}
