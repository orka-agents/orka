package llm

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/log"
)

// FallbackEntry represents a fallback provider with an optional model override.
type FallbackEntry struct {
	Provider Provider
	Model    string
}

// FallbackProvider tries a primary provider and falls back to alternatives on failure.
type FallbackProvider struct {
	primary   Provider
	fallbacks []FallbackEntry
	tracker   *CooldownTracker
}

// NewFallbackProvider creates a FallbackProvider. If fallbacks is empty, it behaves
// like the primary provider.
func NewFallbackProvider(primary Provider, fallbacks []FallbackEntry) *FallbackProvider {
	return &FallbackProvider{
		primary:   primary,
		fallbacks: fallbacks,
	}
}

// SetCooldownTracker sets a cooldown tracker for rate-limit awareness.
func (f *FallbackProvider) SetCooldownTracker(tracker *CooldownTracker) {
	f.tracker = tracker
}

// Name returns the provider name.
func (f *FallbackProvider) Name() string {
	return fmt.Sprintf("fallback(%s)", f.primary.Name())
}

// TelemetryProviderName delegates to the primary provider so error telemetry
// emitted before a concrete fallback response is available uses the configured
// serving provider identity instead of the wrapper name.
func (f *FallbackProvider) TelemetryProviderName() string {
	return ProviderTelemetryName(f.primary)
}

// Complete tries the primary provider, then falls back on failure.
func (f *FallbackProvider) Complete(ctx context.Context, req *CompletionRequest) (*CompletionResponse, error) {
	logger := log.FromContext(ctx)

	type candidate struct {
		provider Provider
		model    string
	}
	candidates := make([]candidate, 0, 1+len(f.fallbacks))

	// Add primary if not cooling down
	if f.tracker == nil || !f.tracker.IsCoolingDown(f.primary.Name()) {
		candidates = append(candidates, candidate{provider: f.primary})
	}

	// Add fallbacks that aren't cooling down
	for _, fb := range f.fallbacks {
		if f.tracker == nil || !f.tracker.IsCoolingDown(fb.Provider.Name()) {
			candidates = append(candidates, candidate{provider: fb.Provider, model: fb.Model})
		}
	}

	// If all are cooling down, try the one with shortest remaining cooldown
	if len(candidates) == 0 {
		shortest := f.shortestCooldown()
		candidates = append(candidates, candidate{provider: shortest})
	}

	var lastErr error
	for _, c := range candidates {
		callReq := req
		if c.model != "" {
			clone := *req
			clone.Model = c.model
			callReq = &clone
		}

		resp, err := c.provider.Complete(ctx, callReq)
		if err == nil {
			if resp != nil && strings.TrimSpace(resp.Provider) == "" {
				resp.Provider = ProviderTelemetryName(c.provider)
			}
			if f.tracker != nil {
				f.tracker.Reset(c.provider.Name())
			}
			return resp, nil
		}

		lastErr = err
		logger.Info("provider failed, trying fallback",
			"provider", c.provider.Name(), "error", err)

		// Mark cooldown on rate limit
		if f.tracker != nil {
			var pe *ProviderError
			if errors.As(err, &pe) && pe.StatusCode == 429 {
				f.tracker.MarkCooldown(c.provider.Name())
			}
		}

		if !ShouldFallback(err) && !ShouldRetry(err) {
			return nil, err
		}
	}

	return nil, fmt.Errorf("all providers failed: %w", lastErr)
}

// Stream tries the primary provider's Stream, falling back on initial failure.
func (f *FallbackProvider) Stream(ctx context.Context, req *CompletionRequest) (<-chan StreamChunk, error) {
	logger := log.FromContext(ctx)

	type candidate struct {
		provider Provider
		model    string
	}
	candidates := make([]candidate, 0, 1+len(f.fallbacks))

	if f.tracker == nil || !f.tracker.IsCoolingDown(f.primary.Name()) {
		candidates = append(candidates, candidate{provider: f.primary})
	}
	for _, fb := range f.fallbacks {
		if f.tracker == nil || !f.tracker.IsCoolingDown(fb.Provider.Name()) {
			candidates = append(candidates, candidate{provider: fb.Provider, model: fb.Model})
		}
	}
	if len(candidates) == 0 {
		shortest := f.shortestCooldown()
		candidates = append(candidates, candidate{provider: shortest})
	}

	var lastErr error
	for _, c := range candidates {
		callReq := req
		if c.model != "" {
			clone := *req
			clone.Model = c.model
			callReq = &clone
		}

		innerCh, err := c.provider.Stream(ctx, callReq)
		if err != nil {
			lastErr = err
			logger.Info("provider stream failed, trying fallback",
				"provider", c.provider.Name(), "error", err)
			continue
		}

		// Peek at first chunk
		firstChunk, ok := <-innerCh
		if !ok {
			ch := make(chan StreamChunk)
			close(ch)
			return ch, nil
		}

		if firstChunk.Error != nil {
			lastErr = firstChunk.Error
			// Drain remaining
			for range innerCh {
			}

			if f.tracker != nil {
				var pe *ProviderError
				if errors.As(firstChunk.Error, &pe) && pe.StatusCode == 429 {
					f.tracker.MarkCooldown(c.provider.Name())
				}
			}

			logger.Info("provider stream error on first chunk, trying fallback",
				"provider", c.provider.Name(), "error", firstChunk.Error)
			continue
		}

		// Success — proxy first chunk and rest
		if f.tracker != nil {
			f.tracker.Reset(c.provider.Name())
		}
		outCh := make(chan StreamChunk)
		go func() {
			defer close(outCh)
			outCh <- firstChunk
			for chunk := range innerCh {
				outCh <- chunk
			}
		}()
		return outCh, nil
	}

	// All failed
	ch := make(chan StreamChunk, 1)
	if lastErr != nil {
		ch <- StreamChunk{Error: fmt.Errorf("all providers failed: %w", lastErr), Done: true}
	}
	close(ch)
	return ch, nil
}

// shortestCooldown returns the provider with the shortest remaining cooldown.
func (f *FallbackProvider) shortestCooldown() Provider {
	if f.tracker == nil {
		return f.primary
	}

	shortest := f.primary
	shortestDur := f.tracker.CooldownRemaining(f.primary.Name())

	for _, fb := range f.fallbacks {
		dur := f.tracker.CooldownRemaining(fb.Provider.Name())
		if dur < shortestDur {
			shortestDur = dur
			shortest = fb.Provider
		}
	}
	return shortest
}

// Ensure FallbackProvider implements Provider and TelemetryIdentity.
var _ Provider = (*FallbackProvider)(nil)
var _ TelemetryIdentity = (*FallbackProvider)(nil)
