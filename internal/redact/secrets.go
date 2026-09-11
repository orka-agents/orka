/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package redact

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
)

type trackedSecretsKey struct{}

type trackedSecrets struct {
	mu     sync.RWMutex
	values []string
}

// WithTrackedSecrets retains credentials loaded during this context's lifetime
// for later redaction. It does not read credentials or change execution authority.
func WithTrackedSecrets(ctx context.Context) context.Context {
	if _, ok := ctx.Value(trackedSecretsKey{}).(*trackedSecrets); ok {
		return ctx
	}
	return context.WithValue(ctx, trackedSecretsKey{}, &trackedSecrets{})
}

// TrackSecrets remembers exact loaded values only when tracking was enabled.
func TrackSecrets(ctx context.Context, values ...string) {
	if ctx == nil {
		return
	}
	tracked, _ := ctx.Value(trackedSecretsKey{}).(*trackedSecrets)
	if tracked == nil {
		return
	}
	tracked.mu.Lock()
	defer tracked.mu.Unlock()
	for _, value := range values {
		if value != "" && !slices.Contains(tracked.values, value) {
			tracked.values = append(tracked.values, value)
		}
	}
}

// TrackedSecrets returns a copy for one redaction operation. Values must never
// be logged or persisted.
func TrackedSecrets(ctx context.Context) []string {
	if ctx == nil {
		return nil
	}
	tracked, _ := ctx.Value(trackedSecretsKey{}).(*trackedSecrets)
	if tracked == nil {
		return nil
	}
	tracked.mu.RLock()
	values := slices.Clone(tracked.values)
	tracked.mu.RUnlock()
	slices.SortFunc(values, func(a, b string) int {
		if len(a) != len(b) {
			return len(b) - len(a)
		}
		return strings.Compare(a, b)
	})
	return values
}

// KnownValues removes only supplied credentials, including overlapping values
// and canonical JSON escaping in nested text. Use SensitiveTextWithValues when
// credential patterns must also be redacted.
func KnownValues(text string, values ...string) string {
	return redactSpans(text, appendKnownValueSpans(nil, text, values...))
}

func appendKnownValueSpans(spans []valueSpan, text string, values ...string) []valueSpan {
	seen := make(map[string]bool)
	for _, value := range values {
		for _, form := range SecretForms(value, len(text)) {
			if seen[form] {
				continue
			}
			seen[form] = true
			for offset := 0; offset < len(text); {
				index := strings.Index(text[offset:], form)
				if index < 0 {
					break
				}
				start := offset + index
				spans = append(spans, valueSpan{start: start, end: start + len(form)})
				// A later occurrence can overlap this one.
				offset = start + 1
			}
		}
	}
	return spans
}

// SecretForms returns the raw value and successive canonical JSON-escaped forms
// that could fit the supplied byte limit. These values must never be logged.
func SecretForms(value string, limit int) []string {
	if value == "" || len(value) > limit {
		return nil
	}
	forms := []string{value}
	for {
		encoded, _ := json.Marshal(value)
		escaped := string(encoded[1 : len(encoded)-1])
		if escaped == value || len(escaped) > limit {
			return forms
		}
		forms = append(forms, escaped)
		value = escaped
	}
}
