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

type RetryProvider struct {
	inner      Provider
	maxRetries int
	baseDelay  time.Duration
	maxDelay   time.Duration
	jitter     float64
}

func NewRetryProvider(inner Provider) *RetryProvider {
	return &RetryProvider{
		inner:      inner,
		maxRetries: defaultMaxRetries,
		baseDelay:  defaultBaseDelay,
		maxDelay:   defaultMaxDelay,
		jitter:     defaultJitter,
	}
}

func (r *RetryProvider) Name() string {
	return r.inner.Name()
}

func (r *RetryProvider) TelemetryProviderName() string {
	return ProviderTelemetryName(r.inner)
}

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

		firstChunk, ok := firstStreamResult(innerCh)
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

	ch := make(chan StreamChunk, 1)
	if lastErr != nil {
		ch <- StreamChunk{Error: lastErr, Done: true}
	}
	close(ch)
	return ch, nil
}

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

func (r *RetryProvider) backoff(attempt int) time.Duration {
	delay := float64(r.baseDelay) * math.Pow(2, float64(attempt))
	if delay > float64(r.maxDelay) {
		delay = float64(r.maxDelay)
	}
	jitterAmount := delay * r.jitter
	delay += jitterAmount * (2*rand.Float64() - 1) //nolint:gosec
	return time.Duration(delay)
}

var _ Provider = (*RetryProvider)(nil)
var _ TelemetryIdentity = (*RetryProvider)(nil)
