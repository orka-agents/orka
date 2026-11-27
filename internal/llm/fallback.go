package llm

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/log"
)

type FallbackEntry struct {
	Provider Provider
	Model    string
}

type FallbackProvider struct {
	primary   Provider
	fallbacks []FallbackEntry
	tracker   *CooldownTracker
}

func NewFallbackProvider(primary Provider, fallbacks []FallbackEntry) *FallbackProvider {
	return &FallbackProvider{
		primary:   primary,
		fallbacks: fallbacks,
	}
}

func (f *FallbackProvider) SetCooldownTracker(tracker *CooldownTracker) {
	f.tracker = tracker
}

func (f *FallbackProvider) Name() string {
	return fmt.Sprintf("fallback(%s)", f.primary.Name())
}

func (f *FallbackProvider) TelemetryProviderName() string {
	return ProviderTelemetryName(f.primary)
}

func (f *FallbackProvider) Complete(ctx context.Context, req *CompletionRequest) (*CompletionResponse, error) {
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
			if IsUsagePersistenceError(err) {
				return nil, err
			}
			lastErr = err
			logger.Info("provider stream failed, trying fallback",
				"provider", c.provider.Name(), "error", err)
			continue
		}

		firstChunk, ok := firstStreamResult(innerCh)
		if !ok {
			ch := make(chan StreamChunk)
			close(ch)
			return ch, nil
		}

		if firstChunk.Error != nil {
			lastErr = firstChunk.Error
			for range innerCh {
			}
			if IsUsagePersistenceError(firstChunk.Error) {
				ch := make(chan StreamChunk, 1)
				ch <- firstChunk
				close(ch)
				return ch, nil
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

		if f.tracker != nil {
			f.tracker.Reset(c.provider.Name())
		}
		providerName := ProviderTelemetryName(c.provider)
		modelName := requestModel(callReq)
		outCh := make(chan StreamChunk)
		go func() {
			defer close(outCh)
			outCh <- withStreamTelemetry(firstChunk, providerName, modelName)
			for chunk := range innerCh {
				outCh <- withStreamTelemetry(chunk, providerName, modelName)
			}
		}()
		return outCh, nil
	}

	ch := make(chan StreamChunk, 1)
	if lastErr != nil {
		ch <- StreamChunk{Error: fmt.Errorf("all providers failed: %w", lastErr), Done: true}
	}
	close(ch)
	return ch, nil
}

func firstStreamResult(ch <-chan StreamChunk) (StreamChunk, bool) {
	for chunk := range ch {
		if chunk.Content != "" || chunk.ToolCall != nil || chunk.Done || chunk.Error != nil {
			return chunk, true
		}
	}
	return StreamChunk{}, false
}

func withStreamTelemetry(chunk StreamChunk, providerName, modelName string) StreamChunk {
	if strings.TrimSpace(chunk.Provider) == "" {
		chunk.Provider = providerName
	}
	if strings.TrimSpace(chunk.Model) == "" {
		chunk.Model = modelName
	}
	return chunk
}

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

var _ Provider = (*FallbackProvider)(nil)
var _ TelemetryIdentity = (*FallbackProvider)(nil)
