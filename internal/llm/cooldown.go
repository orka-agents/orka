package llm

import (
	"sync"
	"time"
)

// CooldownTracker tracks rate-limit cooldowns for providers.
type CooldownTracker struct {
	mu        sync.RWMutex
	cooldowns map[string]*cooldownEntry
}

type cooldownEntry struct {
	until      time.Time
	errorCount int
}

// NewCooldownTracker creates a new CooldownTracker.
func NewCooldownTracker() *CooldownTracker {
	return &CooldownTracker{
		cooldowns: make(map[string]*cooldownEntry),
	}
}

// MarkCooldown marks a provider as cooling down with exponential backoff.
func (t *CooldownTracker) MarkCooldown(provider string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	entry, ok := t.cooldowns[provider]
	if !ok {
		entry = &cooldownEntry{}
		t.cooldowns[provider] = entry
	}
	entry.errorCount++

	// Exponential: 1m * 5^(errorCount-1), capped at 1h
	duration := time.Minute
	for i := 1; i < entry.errorCount; i++ {
		duration *= 5
		if duration > time.Hour {
			duration = time.Hour
			break
		}
	}
	entry.until = time.Now().Add(duration)
}

// IsCoolingDown returns true if the provider is currently in cooldown.
func (t *CooldownTracker) IsCoolingDown(provider string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()

	entry, ok := t.cooldowns[provider]
	if !ok {
		return false
	}
	return time.Now().Before(entry.until)
}

// CooldownRemaining returns the remaining cooldown duration for a provider.
func (t *CooldownTracker) CooldownRemaining(provider string) time.Duration {
	t.mu.RLock()
	defer t.mu.RUnlock()

	entry, ok := t.cooldowns[provider]
	if !ok {
		return 0
	}
	remaining := time.Until(entry.until)
	if remaining < 0 {
		return 0
	}
	return remaining
}

// Reset clears the cooldown for a provider.
func (t *CooldownTracker) Reset(provider string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.cooldowns, provider)
}
