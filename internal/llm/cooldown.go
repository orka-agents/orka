package llm

import (
	"sync"
	"time"
)

type CooldownTracker struct {
	mu        sync.RWMutex
	cooldowns map[string]*cooldownEntry
}

type cooldownEntry struct {
	until      time.Time
	errorCount int
}

func NewCooldownTracker() *CooldownTracker {
	return &CooldownTracker{
		cooldowns: make(map[string]*cooldownEntry),
	}
}

func (t *CooldownTracker) MarkCooldown(provider string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	entry, ok := t.cooldowns[provider]
	if !ok {
		entry = &cooldownEntry{}
		t.cooldowns[provider] = entry
	}
	entry.errorCount++

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

func (t *CooldownTracker) IsCoolingDown(provider string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()

	entry, ok := t.cooldowns[provider]
	if !ok {
		return false
	}
	return time.Now().Before(entry.until)
}

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

func (t *CooldownTracker) Reset(provider string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.cooldowns, provider)
}
