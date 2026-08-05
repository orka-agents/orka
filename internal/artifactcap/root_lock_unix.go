//go:build darwin || linux

package artifactcap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const (
	artifactRootLockRetryInterval  = 10 * time.Millisecond
	artifactWriterIntentPrefix     = ".retention-writer-"
	artifactWriterIntentSuffix     = ".intent"
	artifactWriterSequenceName     = ".retention-writer-sequence"
	artifactWriterSequenceLockName = ".retention-writer-sequence.lock"
	artifactRootLockName           = ".retention.lock"
)

func acquireArtifactRootLock(ctx context.Context, root string, exclusive bool) (func() error, error) {
	releaseGate, err := acquireArtifactProcessGate(ctx, root, exclusive)
	if err != nil {
		return nil, err
	}
	var releaseIntent func() error
	if exclusive {
		var intentPath string
		intentPath, releaseIntent, err = acquireArtifactWriterIntent(ctx, root)
		if err == nil {
			err = waitForArtifactWriterTurn(ctx, root, intentPath)
		}
	} else {
		err = waitForNoArtifactWriterIntent(ctx, root)
	}
	if err != nil {
		_ = releaseGate()
		return nil, err
	}
	cleanupIntentAndGate := func() {
		if releaseIntent != nil {
			_ = releaseIntent()
		}
		_ = releaseGate()
	}

	file, err := openFileNoFollow(filepath.Join(root, artifactRootLockName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		cleanupIntentAndGate()
		return nil, fmt.Errorf("open artifact retention lock: %w", err)
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		cleanupIntentAndGate()
		return nil, ErrUnsafePath
	}
	if info.Mode().Perm()&0o077 != 0 {
		if err := file.Chmod(0o600); err != nil {
			_ = file.Close()
			cleanupIntentAndGate()
			return nil, fmt.Errorf("secure artifact retention lock: %w", err)
		}
	}
	operation := unix.LOCK_SH | unix.LOCK_NB
	if exclusive {
		operation = unix.LOCK_EX | unix.LOCK_NB
	}
	for {
		err = unix.Flock(int(file.Fd()), operation)
		if err == nil {
			return func() error {
				unlockErr := unix.Flock(int(file.Fd()), unix.LOCK_UN)
				closeErr := file.Close()
				var intentErr error
				if releaseIntent != nil {
					intentErr = releaseIntent()
				}
				gateErr := releaseGate()
				return errors.Join(unlockErr, closeErr, intentErr, gateErr)
			}, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EINTR) {
			_ = file.Close()
			cleanupIntentAndGate()
			return nil, fmt.Errorf("lock artifact storage: %w", err)
		}
		if err := waitArtifactLockRetry(ctx); err != nil {
			_ = file.Close()
			cleanupIntentAndGate()
			return nil, err
		}
	}
}

func acquireArtifactWriterIntent(ctx context.Context, root string) (string, func() error, error) {
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	pendingDir := filepath.Join(root, "tmp")
	if err := ensurePrivateDirectory(pendingDir); err != nil {
		return "", nil, err
	}
	pending, err := os.CreateTemp(pendingDir, ".retention-writer-pending-*.tmp")
	if err != nil {
		return "", nil, fmt.Errorf("create artifact writer intent: %w", err)
	}
	pendingPath := pending.Name()
	cleanupPending := func() {
		_ = pending.Close()
		_ = os.Remove(pendingPath)
	}
	if err := pending.Chmod(0o600); err != nil {
		cleanupPending()
		return "", nil, err
	}
	if err := unix.Flock(int(pending.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		cleanupPending()
		return "", nil, err
	}
	if _, err := fmt.Fprintf(pending, "%d\n", os.Getpid()); err != nil {
		_ = unix.Flock(int(pending.Fd()), unix.LOCK_UN)
		cleanupPending()
		return "", nil, err
	}
	if err := pending.Sync(); err != nil {
		_ = unix.Flock(int(pending.Fd()), unix.LOCK_UN)
		cleanupPending()
		return "", nil, err
	}
	base := filepath.Base(pendingPath)
	nonce := strings.TrimSuffix(strings.TrimPrefix(base, ".retention-writer-pending-"), ".tmp")
	intentPath, err := publishArtifactWriterIntent(ctx, root, pendingPath, nonce)
	if err != nil {
		_ = unix.Flock(int(pending.Fd()), unix.LOCK_UN)
		cleanupPending()
		return "", nil, err
	}
	return intentPath, func() error {
		unlockErr := unix.Flock(int(pending.Fd()), unix.LOCK_UN)
		closeErr := pending.Close()
		removed, removeErr := removeManagedRegularFile(intentPath)
		var syncErr error
		if removed {
			syncErr = syncDirectory(root)
		}
		return errors.Join(unlockErr, closeErr, removeErr, syncErr)
	}, nil
}

func waitForArtifactWriterTurn(ctx context.Context, root, intentPath string) error {
	for {
		queue, err := artifactWriterQueue(root)
		if err != nil {
			return err
		}
		if len(queue) == 0 {
			return ErrUnsafePath
		}
		if queue[0].path == intentPath {
			return nil
		}
		if err := waitArtifactLockRetry(ctx); err != nil {
			return err
		}
	}
}

type artifactWriterMarker struct {
	path   string
	name   string
	ticket uint64
}

func waitForNoArtifactWriterIntent(ctx context.Context, root string) error {
	for {
		active, err := activeArtifactWriterIntent(root)
		if err != nil {
			return err
		}
		if !active {
			return nil
		}
		if err := waitArtifactLockRetry(ctx); err != nil {
			return err
		}
	}
}

func activeArtifactWriterIntent(root string) (bool, error) {
	queue, err := artifactWriterQueue(root)
	return len(queue) > 0, err
}

func artifactWriterQueue(root string) ([]artifactWriterMarker, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	removed := false
	queue := make([]artifactWriterMarker, 0)
	for _, entry := range entries {
		name := entry.Name()
		isIntent := strings.HasPrefix(name, artifactWriterIntentPrefix) && strings.HasSuffix(name, artifactWriterIntentSuffix)
		if !isIntent {
			continue
		}
		path := filepath.Join(root, name)
		ticket, err := artifactWriterTicket(name)
		if err != nil {
			return nil, err
		}
		active, stale, err := inspectArtifactWriterMarker(path)
		if err != nil {
			return nil, err
		}
		if active {
			queue = append(queue, artifactWriterMarker{path: path, name: name, ticket: ticket})
			continue
		}
		if stale {
			deleted, err := removeManagedRegularFile(path)
			if err != nil {
				return nil, err
			}
			removed = removed || deleted
		}
	}
	if removed {
		if err := syncDirectory(root); err != nil {
			return nil, err
		}
	}
	sort.Slice(queue, func(i, j int) bool {
		if queue[i].ticket == queue[j].ticket {
			return queue[i].name < queue[j].name
		}
		return queue[i].ticket < queue[j].ticket
	})
	return queue, nil
}

func publishArtifactWriterIntent(ctx context.Context, root, pendingPath, nonce string) (string, error) {
	lockPath := filepath.Join(root, artifactWriterSequenceLockName)
	lockFile, err := openFileNoFollow(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return "", err
	}
	defer lockFile.Close() //nolint:errcheck
	if info, err := lockFile.Stat(); err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return "", ErrUnsafePath
	}
	for {
		err = unix.Flock(int(lockFile.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EINTR) {
			return "", err
		}
		if err := waitArtifactLockRetry(ctx); err != nil {
			return "", err
		}
	}
	defer unix.Flock(int(lockFile.Fd()), unix.LOCK_UN) //nolint:errcheck

	ticket, err := nextArtifactWriterTicket(root)
	if err != nil {
		return "", err
	}
	intentName := fmt.Sprintf("%s%020d-%s%s", artifactWriterIntentPrefix, ticket, nonce, artifactWriterIntentSuffix)
	intentPath := filepath.Join(root, intentName)
	if err := os.Rename(pendingPath, intentPath); err != nil {
		return "", err
	}
	if err := syncDirectory(root); err != nil {
		_ = os.Remove(intentPath)
		return "", err
	}
	return intentPath, nil
}

func nextArtifactWriterTicket(root string) (uint64, error) {
	sequencePath := filepath.Join(root, artifactWriterSequenceName)
	current := uint64(0)
	sequence, err := openFileNoFollow(sequencePath, os.O_RDONLY, 0)
	if err == nil {
		info, statErr := sequence.Stat()
		if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > 32 {
			_ = sequence.Close()
			return 0, ErrUnsafePath
		}
		data, readErr := io.ReadAll(io.LimitReader(sequence, 33))
		closeErr := sequence.Close()
		if readErr != nil || closeErr != nil || len(data) > 32 {
			return 0, ErrCorrupt
		}
		current, err = strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
		if err != nil {
			return 0, ErrCorrupt
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	if current == ^uint64(0) {
		return 0, ErrCorrupt
	}
	next := current + 1
	temp, err := os.CreateTemp(root, ".retention-writer-sequence-*.tmp")
	if err != nil {
		return 0, err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath) //nolint:errcheck
	if err = temp.Chmod(0o600); err == nil {
		_, err = fmt.Fprintf(temp, "%d\n", next)
	}
	if err == nil {
		err = temp.Sync()
	}
	closeErr := temp.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return 0, err
	}
	if err := os.Rename(tempPath, sequencePath); err != nil {
		return 0, err
	}
	if err := syncDirectory(root); err != nil {
		return 0, err
	}
	return next, nil
}

func artifactWriterTicket(name string) (uint64, error) {
	trimmed := strings.TrimSuffix(strings.TrimPrefix(name, artifactWriterIntentPrefix), artifactWriterIntentSuffix)
	parts := strings.SplitN(trimmed, "-", 2)
	if len(parts) != 2 || len(parts[0]) != 20 || parts[1] == "" {
		return 0, ErrCorrupt
	}
	ticket, err := strconv.ParseUint(parts[0], 10, 64)
	if err != nil || ticket == 0 {
		return 0, ErrCorrupt
	}
	return ticket, nil
}

func inspectArtifactWriterMarker(path string) (active, stale bool, err error) {
	file, err := openFileNoFollow(path, os.O_RDWR, 0)
	if errors.Is(err, os.ErrNotExist) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	defer file.Close() //nolint:errcheck
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return false, false, ErrUnsafePath
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
			return true, false, nil
		}
		return false, false, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_UN); err != nil {
		return false, false, err
	}
	return false, true, nil
}

func waitArtifactLockRetry(ctx context.Context) error {
	timer := time.NewTimer(artifactRootLockRetryInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
