package llm

import (
	"context"
	"math"
	"math/rand/v2"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	defaultMaxRetries = 3
	defaultBaseDelay  = 1 * time.Second
	defaultMaxDelay   = 30 * time.Second
	defaultJitter     = 0.1
)

// RetryProvider wraps a Provider with automatic retry logic for transient errors.
type RetryProvider struct {
	inner      Provider
	maxRetries int
	baseDelay  time.Duration
	maxDelay   time.Duration
	jitter     float64
}

// NewRetryProvider creates a RetryProvider wrapping inner. If maxRetries is 0, defaults to 3.
func NewRetryProvider(inner Provider, maxRetries int) *RetryProvider {
	if maxRetries <= 0 {
		maxRetries = defaultMaxRetries
	}
	return &RetryProvider{
		inner:      inner,
		maxRetries: maxRetries,
		baseDelay:  defaultBaseDelay,
		maxDelay:   defaultMaxDelay,
		jitter:     defaultJitter,
	}
}

// Name delegates to the inner provider.
func (r *RetryProvider) Name() string {
	return r.inner.Name()
}

// TelemetryProviderName delegates to the inner provider so tracing preserves
// concrete provider identity through retry wrapping.
func (r *RetryProvider) TelemetryProviderName() string {
	return ProviderTelemetryName(r.inner)
}

// Complete calls the inner provider's Complete with retry logic.
func (r *RetryProvider) Complete(ctx context.Context, req *CompletionRequest) (*CompletionResponse, error) {
	var lastErr error
	for attempt := 0; attempt <= r.maxRetries; attempt++ {
		resp, err := r.inner.Complete(ctx, req)
		if err == nil {
			return resp, nil
		}
		lastErr = err

		if !ShouldRetry(err) {
			return nil, err
		}

		if attempt < r.maxRetries {
			logger := log.FromContext(ctx)
			logger.Info("retrying LLM call", "provider", r.inner.Name(), "attempt", attempt+1, "maxRetries", r.maxRetries, "error", err)

			if err := r.sleep(ctx, attempt); err != nil {
				return nil, lastErr
			}
		}
	}
	return nil, lastErr
}

// Stream calls the inner provider's Stream with peek-at-first-chunk retry logic.
// Both providers' Stream() always return (ch, nil) — errors appear as the first chunk.
func (r *RetryProvider) Stream(ctx context.Context, req *CompletionRequest) (<-chan StreamChunk, error) {
	var lastErr error
	for attempt := 0; attempt <= r.maxRetries; attempt++ {
		innerCh, err := r.inner.Stream(ctx, req)
		if err != nil {
			lastErr = err
			if !ShouldRetry(err) {
				return nil, err
			}
			if attempt < r.maxRetries {
				if sleepErr := r.sleep(ctx, attempt); sleepErr != nil {
					return nil, lastErr
				}
			}
			continue
		}

		// Peek at the first chunk to check for errors
		firstChunk, ok := <-innerCh
		if !ok {
			ch := make(chan StreamChunk)
			close(ch)
			return ch, nil
		}

		if firstChunk.Error != nil {
			lastErr = firstChunk.Error
			if !ShouldRetry(firstChunk.Error) {
				ch := make(chan StreamChunk, 1)
				ch <- firstChunk
				close(ch)
				return ch, nil
			}
			// Drain remaining chunks
			for range innerCh {
			}
			if attempt < r.maxRetries {
				logger := log.FromContext(ctx)
				logger.Info("retrying LLM stream", "provider", r.inner.Name(), "attempt", attempt+1, "error", firstChunk.Error)
				if sleepErr := r.sleep(ctx, attempt); sleepErr != nil {
					ch := make(chan StreamChunk, 1)
					ch <- firstChunk
					close(ch)
					return ch, nil
				}
			}
			continue
		}

		// First chunk is content — proxy it through a new channel
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

	// All retries exhausted — return last error as a chunk
	ch := make(chan StreamChunk, 1)
	if lastErr != nil {
		ch <- StreamChunk{Error: lastErr, Done: true}
	}
	close(ch)
	return ch, nil
}

// sleep performs an interruptible sleep with exponential backoff and jitter.
func (r *RetryProvider) sleep(ctx context.Context, attempt int) error {
	delay := r.backoff(attempt)
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// backoff calculates the delay for the given attempt with exponential backoff and jitter.
func (r *RetryProvider) backoff(attempt int) time.Duration {
	delay := float64(r.baseDelay) * math.Pow(2, float64(attempt))
	if delay > float64(r.maxDelay) {
		delay = float64(r.maxDelay)
	}
	jitterAmount := delay * r.jitter
	delay += jitterAmount * (2*rand.Float64() - 1) //nolint:gosec
	return time.Duration(delay)
}

// Ensure RetryProvider implements Provider and TelemetryIdentity.
var _ Provider = (*RetryProvider)(nil)
var _ TelemetryIdentity = (*RetryProvider)(nil)
