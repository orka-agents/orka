/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultTokenReloadInterval  = 5 * time.Second
	defaultPreviousTokenOverlap = 10 * time.Minute
	maxPreviousTokenOverlap     = 24 * time.Hour
	tokenFileReadAttempts       = 3
	tokenFileReadRetryDelay     = 2 * time.Millisecond
	maxBearerTokenBytes         = 4096
	maxTokenDeadlineBytes       = 128
)

var errTokenReload = errors.New("provider auth token reload failed")

type bearerTokenSnapshot struct {
	currentDigest      [sha256.Size]byte
	previousDigest     [sha256.Size]byte
	previousValidUntil time.Time
	hasPrevious        bool
	ready              bool
}

type bearerTokenStore struct {
	now      func() time.Time
	snapshot atomic.Pointer[bearerTokenSnapshot]
}

func newBearerTokenStore(now func() time.Time) *bearerTokenStore {
	if now == nil {
		now = time.Now
	}
	store := &bearerTokenStore{now: now}
	store.disable()
	return store
}

func (s *bearerTokenStore) activate(current, previous []byte, previousValidUntil time.Time) {
	next := &bearerTokenSnapshot{
		currentDigest: sha256.Sum256(current),
		ready:         true,
	}
	if len(previous) != 0 {
		next.previousDigest = sha256.Sum256(previous)
		next.previousValidUntil = previousValidUntil
		next.hasPrevious = true
	}
	s.snapshot.Store(next)
}

func (s *bearerTokenStore) disable() {
	s.snapshot.Store(&bearerTokenSnapshot{})
}

func (s *bearerTokenStore) isReady() bool {
	return s.snapshot.Load().ready
}

func (s *bearerTokenStore) authorized(values []string) bool {
	if len(values) != 1 {
		return false
	}
	scheme, credential, ok := strings.Cut(values[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || credential == "" || strings.ContainsAny(credential, " \t\r\n") {
		return false
	}
	active := s.snapshot.Load()
	if !active.ready {
		return false
	}
	provided := sha256.Sum256([]byte(credential))
	currentMatch := subtle.ConstantTimeCompare(provided[:], active.currentDigest[:])
	previousMatch := 0
	if active.hasPrevious && s.now().Before(active.previousValidUntil) {
		previousMatch = subtle.ConstantTimeCompare(provided[:], active.previousDigest[:])
	}
	return currentMatch|previousMatch == 1
}

type tokenFileReloaderConfig struct {
	CurrentTokenFile            string
	PreviousTokenFile           string
	PreviousTokenValidUntilFile string
	ReloadInterval              time.Duration
	PreviousTokenOverlap        time.Duration
}

type tokenFileReloader struct {
	config tokenFileReloaderConfig
	store  *bearerTokenStore
	now    func() time.Time
	mu     sync.Mutex
}

func newTokenFileReloader(config tokenFileReloaderConfig, store *bearerTokenStore) (*tokenFileReloader, error) {
	config.CurrentTokenFile = strings.TrimSpace(config.CurrentTokenFile)
	config.PreviousTokenFile = strings.TrimSpace(config.PreviousTokenFile)
	config.PreviousTokenValidUntilFile = strings.TrimSpace(config.PreviousTokenValidUntilFile)
	if config.CurrentTokenFile == "" {
		return nil, fmt.Errorf("current provider auth token file is required")
	}
	if (config.PreviousTokenFile == "") != (config.PreviousTokenValidUntilFile == "") {
		return nil, fmt.Errorf("previous provider auth token and validity files must be configured together")
	}
	if pathsOverlap(config.CurrentTokenFile, config.PreviousTokenFile, config.PreviousTokenValidUntilFile) {
		return nil, fmt.Errorf("provider auth token file paths must differ")
	}
	if config.ReloadInterval <= 0 {
		return nil, fmt.Errorf("provider auth token reload interval must be positive")
	}
	if config.PreviousTokenOverlap <= 0 || config.PreviousTokenOverlap > maxPreviousTokenOverlap {
		return nil, fmt.Errorf("provider auth previous token overlap must be positive and at most %s", maxPreviousTokenOverlap)
	}
	if store == nil {
		return nil, fmt.Errorf("provider auth token store is required")
	}
	return &tokenFileReloader{
		config: config,
		store:  store,
		now:    store.now,
	}, nil
}

func pathsOverlap(paths ...string) bool {
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		if path == "" {
			continue
		}
		if _, ok := seen[path]; ok {
			return true
		}
		seen[path] = struct{}{}
	}
	return false
}

func (r *tokenFileReloader) reload() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	current, previous, validUntilFile, err := readStableTokenFiles(
		r.config.CurrentTokenFile,
		r.config.PreviousTokenFile,
		r.config.PreviousTokenValidUntilFile,
	)
	if err != nil {
		r.store.disable()
		return errTokenReload
	}
	defer clear(current.contents)
	defer clear(previous.contents)
	defer clear(validUntilFile.contents)
	normalizeMountedToken(&current)
	normalizeMountedToken(&previous)

	previousValidUntil, err := r.validateTokenFiles(current, previous, validUntilFile)
	if err != nil {
		r.store.disable()
		return errTokenReload
	}
	r.store.activate(current.contents, previous.contents, previousValidUntil)
	return nil
}

func normalizeMountedToken(snapshot *tokenFileSnapshot) {
	if snapshot == nil || len(snapshot.contents) == 0 {
		return
	}
	trimmed := bytes.TrimSpace(snapshot.contents)
	copy(snapshot.contents, trimmed)
	clear(snapshot.contents[len(trimmed):])
	snapshot.contents = snapshot.contents[:len(trimmed)]
}

func (r *tokenFileReloader) validateTokenFiles(
	current tokenFileSnapshot,
	previous tokenFileSnapshot,
	validUntilFile tokenFileSnapshot,
) (time.Time, error) {
	if err := validateBearerToken(current.contents); err != nil {
		return time.Time{}, errTokenReload
	}
	if previous.present != validUntilFile.present {
		return time.Time{}, errTokenReload
	}
	if !previous.present {
		return time.Time{}, nil
	}
	if err := validateBearerToken(previous.contents); err != nil {
		return time.Time{}, errTokenReload
	}
	currentDigest := sha256.Sum256(current.contents)
	previousDigest := sha256.Sum256(previous.contents)
	if subtle.ConstantTimeCompare(currentDigest[:], previousDigest[:]) == 1 {
		return time.Time{}, errTokenReload
	}
	validUntil, err := parseTokenDeadline(validUntilFile.contents)
	if err != nil || validUntil.After(r.now().Add(r.config.PreviousTokenOverlap)) {
		return time.Time{}, errTokenReload
	}
	return validUntil, nil
}

func (r *tokenFileReloader) run(ctx context.Context, logMessage func(string)) {
	ticker := time.NewTicker(r.config.ReloadInterval)
	defer ticker.Stop()
	failed := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := r.reload(); err != nil {
				if !failed && logMessage != nil {
					logMessage("provider auth proxy token reload failed; authentication is disabled until a valid reload")
				}
				failed = true
				continue
			}
			if failed && logMessage != nil {
				logMessage("provider auth proxy token reload recovered")
			}
			failed = false
		}
	}
}

func parseTokenDeadline(contents []byte) (time.Time, error) {
	if len(contents) == 0 || len(contents) > maxTokenDeadlineBytes {
		return time.Time{}, errTokenReload
	}
	value := strings.TrimSpace(string(contents))
	if value == "" || strings.ContainsAny(value, "\r\n\t ") {
		return time.Time{}, errTokenReload
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, errTokenReload
	}
	return parsed, nil
}

type tokenFileSnapshot struct {
	contents []byte
	info     os.FileInfo
	present  bool
}

func readStableTokenFiles(
	currentPath string,
	previousPath string,
	validUntilPath string,
) (tokenFileSnapshot, tokenFileSnapshot, tokenFileSnapshot, error) {
	return readStableTokenFilesWithWait(currentPath, previousPath, validUntilPath, waitForNextTokenFileRead)
}

func readStableTokenFilesWithWait(
	currentPath string,
	previousPath string,
	validUntilPath string,
	wait func(int),
) (tokenFileSnapshot, tokenFileSnapshot, tokenFileSnapshot, error) {
	configuredPaths := [3]string{currentPath, previousPath, validUntilPath}
	for attempt := range tokenFileReadAttempts {
		readPaths, generation, projected, err := credentialReadPaths(configuredPaths)
		if err != nil {
			wait(attempt)
			continue
		}
		current, err := readTokenFile(readPaths[0], false, maxBearerTokenBytes)
		if err != nil {
			if projected {
				wait(attempt)
				continue
			}
			return tokenFileSnapshot{}, tokenFileSnapshot{}, tokenFileSnapshot{}, err
		}
		previous, err := readTokenFile(readPaths[1], true, maxBearerTokenBytes)
		if err != nil {
			clear(current.contents)
			if projected {
				wait(attempt)
				continue
			}
			return tokenFileSnapshot{}, tokenFileSnapshot{}, tokenFileSnapshot{}, err
		}
		validUntil, err := readTokenFile(readPaths[2], true, maxTokenDeadlineBytes)
		if err != nil {
			clear(current.contents)
			clear(previous.contents)
			if projected {
				wait(attempt)
				continue
			}
			return tokenFileSnapshot{}, tokenFileSnapshot{}, tokenFileSnapshot{}, err
		}
		if credentialReadUnchanged(configuredPaths, readPaths, generation, projected, current, previous, validUntil) {
			return current, previous, validUntil, nil
		}
		clear(current.contents)
		clear(previous.contents)
		clear(validUntil.contents)
		wait(attempt)
	}
	return tokenFileSnapshot{}, tokenFileSnapshot{}, tokenFileSnapshot{}, errTokenReload
}

func waitForNextTokenFileRead(attempt int) {
	if attempt+1 < tokenFileReadAttempts {
		time.Sleep(tokenFileReadRetryDelay)
	}
}

func credentialReadPaths(configured [3]string) ([3]string, string, bool, error) {
	if configured[1] == "" || configured[2] == "" {
		return configured, "", false, nil
	}
	directory := filepath.Dir(configured[0])
	if filepath.Dir(configured[1]) != directory || filepath.Dir(configured[2]) != directory {
		return configured, "", false, nil
	}
	generationLink := filepath.Join(directory, "..data")
	generation, err := os.Readlink(generationLink)
	if errors.Is(err, os.ErrNotExist) {
		return configured, "", false, nil
	}
	if err != nil {
		return [3]string{}, "", false, err
	}
	generationDirectory := generation
	if !filepath.IsAbs(generationDirectory) {
		generationDirectory = filepath.Join(directory, generationDirectory)
	}
	generationDirectory = filepath.Clean(generationDirectory)
	relative, err := filepath.Rel(directory, generationDirectory)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return [3]string{}, "", false, errTokenReload
	}
	info, err := os.Stat(generationDirectory)
	if err != nil || !info.IsDir() {
		return [3]string{}, "", false, errTokenReload
	}
	return [3]string{
		filepath.Join(generationDirectory, filepath.Base(configured[0])),
		filepath.Join(generationDirectory, filepath.Base(configured[1])),
		filepath.Join(generationDirectory, filepath.Base(configured[2])),
	}, generationDirectory, true, nil
}

func credentialReadUnchanged(
	configured [3]string,
	readPaths [3]string,
	generation string,
	projected bool,
	current tokenFileSnapshot,
	previous tokenFileSnapshot,
	validUntil tokenFileSnapshot,
) bool {
	if projected {
		nextPaths, nextGeneration, nextProjected, err := credentialReadPaths(configured)
		if err != nil || !nextProjected || nextGeneration != generation || nextPaths != readPaths {
			return false
		}
	}
	return tokenFileUnchanged(readPaths[0], current) && tokenFileUnchanged(readPaths[1], previous) &&
		tokenFileUnchanged(readPaths[2], validUntil)
}

func readTokenFile(path string, optional bool, maxBytes int64) (tokenFileSnapshot, error) {
	if path == "" {
		return tokenFileSnapshot{}, nil
	}
	file, err := os.Open(path)
	if err != nil {
		if optional && errors.Is(err, os.ErrNotExist) {
			return tokenFileSnapshot{}, nil
		}
		return tokenFileSnapshot{}, err
	}
	defer file.Close() //nolint:errcheck

	contents, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		clear(contents)
		return tokenFileSnapshot{}, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		clear(contents)
		return tokenFileSnapshot{}, errTokenReload
	}
	return tokenFileSnapshot{contents: contents, info: info, present: true}, nil
}

func tokenFileUnchanged(path string, snapshot tokenFileSnapshot) bool {
	if path == "" {
		return !snapshot.present
	}
	info, err := os.Stat(path)
	if !snapshot.present {
		return errors.Is(err, os.ErrNotExist)
	}
	return err == nil && os.SameFile(snapshot.info, info) && snapshot.info.Size() == info.Size() &&
		snapshot.info.ModTime().Equal(info.ModTime())
}
