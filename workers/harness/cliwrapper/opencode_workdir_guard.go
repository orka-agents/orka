package cliwrapper

import (
	"fmt"
	"os"
	"path/filepath"
)

type opencodeWorkDirGuard struct {
	path     string
	dir      *os.File
	identity os.FileInfo
}

func openOpencodeWorkDirGuard(dir string) (*opencodeWorkDirGuard, error) {
	path, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve opencode work directory: %w", err)
	}
	path = filepath.Clean(path)
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open opencode work directory: %w", err)
	}
	guard := &opencodeWorkDirGuard{path: path, dir: file}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("stat opened opencode work directory: %w", err)
	}
	if !info.IsDir() {
		_ = file.Close()
		return nil, fmt.Errorf("opencode work directory %q is not a directory", path)
	}
	guard.identity = info
	if err := guard.verifyPathIdentity(); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := guard.ensureAccessible(); err != nil {
		_ = file.Close()
		return nil, err
	}
	return guard, nil
}

func (g *opencodeWorkDirGuard) ensureAccessible() error {
	if g == nil || g.dir == nil {
		return fmt.Errorf("opencode work directory guard is not open")
	}
	info, err := g.dir.Stat()
	if err != nil {
		return fmt.Errorf("stat guarded opencode work directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("guarded opencode work directory is not a directory")
	}
	if uid, _, ok := childCredentialIDs(); ok && os.Geteuid() == 0 {
		if err := g.dir.Chown(uid, 0); err != nil {
			return fmt.Errorf("chown guarded opencode work directory: %w", err)
		}
	}
	mode := info.Mode() & (os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky)
	if err := g.dir.Chmod(mode | 0o770); err != nil {
		return fmt.Errorf("chmod guarded opencode work directory: %w", err)
	}
	return nil
}

func (g *opencodeWorkDirGuard) restoreAndVerify() error {
	if err := g.ensureAccessible(); err != nil {
		return err
	}
	return g.verifyPathIdentity()
}

func (g *opencodeWorkDirGuard) verifyPathIdentity() error {
	if g == nil || g.dir == nil || g.identity == nil {
		return fmt.Errorf("opencode work directory guard is not open")
	}
	info, err := os.Lstat(g.path)
	if err != nil {
		return fmt.Errorf("inspect guarded opencode work directory path: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("guarded opencode work directory path %q was replaced by a symlink", g.path)
	}
	if !info.IsDir() {
		return fmt.Errorf("guarded opencode work directory path %q is not a directory", g.path)
	}
	if !os.SameFile(g.identity, info) {
		return fmt.Errorf("guarded opencode work directory path %q was replaced", g.path)
	}
	return nil
}

func (g *opencodeWorkDirGuard) Close() error {
	if g == nil || g.dir == nil {
		return nil
	}
	err := g.dir.Close()
	g.dir = nil
	return err
}
