package main

import (
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

type secureWriteLockEntry struct {
	mu   sync.Mutex
	refs int
}

type secureWriteLockRegistry struct {
	mu      sync.Mutex
	entries map[string]*secureWriteLockEntry
}

var workspaceSecureWriteLocks = secureWriteLockRegistry{
	entries: make(map[string]*secureWriteLockEntry),
}

func lockSecureWritePath(requested string) func() {
	return workspaceSecureWriteLocks.lock(secureWriteLockKey(requested))
}

func (r *secureWriteLockRegistry) lock(key string) func() {
	r.mu.Lock()
	entry := r.entries[key]
	if entry == nil {
		entry = &secureWriteLockEntry{}
		r.entries[key] = entry
	}
	entry.refs++
	r.mu.Unlock()

	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		r.mu.Lock()
		entry.refs--
		if entry.refs == 0 && r.entries[key] == entry {
			delete(r.entries, key)
		}
		r.mu.Unlock()
	}
}

func secureWriteLockKey(requested string) string {
	key := strings.TrimSpace(requested)
	if !filepath.IsAbs(key) {
		key = filepath.Join("/app", key)
	}
	key = filepath.Clean(key)
	if runtime.GOOS == "windows" {
		key = strings.ToLower(key)
	}
	return key
}
