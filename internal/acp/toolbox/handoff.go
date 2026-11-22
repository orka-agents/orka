package toolbox

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

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

const worldAccessBits = os.FileMode(0o005)

func VerifyMounted(mountPath string, pathEntries []string) error {
	if !filepath.IsAbs(mountPath) || filepath.Clean(mountPath) != mountPath || mountPath == "/" {
		return failf(ReasonInvalidArguments, "mount path must be a clean absolute path below /")
	}
	if err := requireRealDirectory(mountPath); err != nil {
		return failf(ReasonMissingMount, "toolbox %s is not mounted: %v", mountPath, err)
	}
	if err := requireWorldAccessible(mountPath); err != nil {
		return failf(ReasonPermissionDenied, "toolbox %s: %v", mountPath, err)
	}
	for _, entry := range pathEntries {
		if entry == "" || filepath.IsAbs(entry) || filepath.Clean(entry) != entry || entry == "." || entry == ".." || strings.HasPrefix(entry, "../") {
			return failf(ReasonInvalidArguments, "path entry %q must be a clean relative path", entry)
		}
		dir := mountPath
		for component := range strings.SplitSeq(entry, string(filepath.Separator)) {
			dir = filepath.Join(dir, component)
			if err := requireRealDirectory(dir); err != nil {
				return failf(ReasonMissingPathEntry, "toolbox %s path entry %s: %v", mountPath, entry, err)
			}
			if err := requireWorldAccessible(dir); err != nil {
				return failf(ReasonPermissionDenied, "toolbox %s path entry %s: %v", mountPath, entry, err)
			}
		}
	}
	return nil
}

func requireRealDirectory(dir string) error {
	current := string(filepath.Separator)
	for component := range strings.SplitSeq(strings.TrimPrefix(dir, current), current) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symlink", current)
		}
		if !info.IsDir() {
			return fmt.Errorf("%s is not a folder", current)
		}
	}
	return nil
}

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
