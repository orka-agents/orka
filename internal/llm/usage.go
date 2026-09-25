package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/orka-agents/orka/internal/store"
)

type usageRecorderKey struct{}
type usageHTTPAttemptKey struct{}
type UsageRecorder func(context.Context, store.UsageObservation) error

type usagePersistenceError struct{ cause error }

func (e *usagePersistenceError) Error() string { return e.cause.Error() }
func (e *usagePersistenceError) Unwrap() error { return e.cause }

// IsUsagePersistenceError identifies accounting failures that must not trigger
// another model call, even if a joined provider error normally permits retry.
func IsUsagePersistenceError(err error) bool {
	_, ok := errors.AsType[*usagePersistenceError](err)
	return ok
}

type usageCall struct {
	mu          sync.Mutex
	record      UsageRecorder
	observation store.UsageObservation
	httpStarted bool
	err         error
}

// UsageHTTPMiddleware accounts for retries and API fallbacks inside the SDK.
// Each HTTP request can incur usage even when only the last one reports counts.
// It never reads request bodies, response bodies, headers or credentials.
func UsageHTTPMiddleware(req *http.Request, next func(*http.Request) (*http.Response, error)) (*http.Response, error) {
	call, _ := req.Context().Value(usageHTTPAttemptKey{}).(*usageCall)
	if call != nil {
		if err := call.beforeHTTP(req.Context()); err != nil {
			return nil, err
		}
	}
	return next(req)
}

func (c *usageCall) beforeHTTP(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !c.httpStarted {
		c.httpStarted = true
		return nil
	}
	previous := c.observation
	previous.ID = previous.CounterID + "/http-finish"
	previous.ObservedAt = time.Now().UTC()
	if previous.Status == store.UsageStatusStarted {
		previous.Status = store.UsageStatusFailed
	}
	if err := finishUsage(ctx, c.record, previous); err != nil {
		c.err = fmt.Errorf("persist retried model call: %w", err)
		return c.err
	}
	next := store.UsageObservation{CounterID: uuid.NewString(), Source: previous.Source, Scope: previous.Scope,
		Provider: previous.Provider, Model: previous.Model, Status: store.UsageStatusStarted}
	next.ID, next.ObservedAt = next.CounterID+"/start", time.Now().UTC()
	if err := c.record(ctx, next); err != nil {
		c.err = &usagePersistenceError{cause: fmt.Errorf("persist model retry start: %w", err)}
		return c.err
	}
	c.observation = next
	return nil
}

func (c *usageCall) snapshot() store.UsageObservation {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.observation
}

func (c *usageCall) persistenceError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// RecordIntermediateUsage preserves a successful SDK probe whose response is
// consumed inside a provider rather than returned to the calling worker.
func RecordIntermediateUsage(ctx context.Context, response *CompletionResponse) error {
	call, _ := ctx.Value(usageHTTPAttemptKey{}).(*usageCall)
	if call == nil || response == nil {
		return nil
	}
	call.mu.Lock()
	defer call.mu.Unlock()
	observation := call.observation
	observation.ID = observation.CounterID + "/response"
	observation.ObservedAt, observation.Status = time.Now().UTC(), store.UsageStatusCompleted
	setUsageCounts(&observation, response.InputTokens, response.OutputTokens, response.CachedInputTokens, response.CacheWriteInputTokens, response.InputExcludesCache, response.UsageReported)
	observation.Complete = response.UsageReported
	if response.Model != "" {
		observation.Model = response.Model
	}
	if response.Provider != "" {
		observation.Provider = response.Provider
	}
	if err := finishUsage(ctx, call.record, observation); err != nil {
		call.err = fmt.Errorf("persist model probe usage: %w", err)
		return call.err
	}
	call.observation = observation
	return nil
}

// ReportedTokenCount preserves missing cache breakdowns as nil.
func ReportedTokenCount(value int64, reported bool) *int64 {
	if !reported {
		return nil
	}
	return &value
}

// WithUsageRecorder enables durable accounting independently of tracing. The
// caller supplies verified ownership; no provider/user billing fields do so.
func WithUsageRecorder(ctx context.Context, recorder UsageRecorder) context.Context {
	return context.WithValue(ctx, usageRecorderKey{}, recorder)
}

// CopyUsageRecorder preserves accounting when a stream detaches from an HTTP
// request. It copies only the counts recorder, not the request or credentials.
func CopyUsageRecorder(dst, src context.Context) context.Context {
	if record, ok := src.Value(usageRecorderKey{}).(UsageRecorder); ok {
		return WithUsageRecorder(dst, record)
	}
	return dst
}

// usageProvider is installed inside retry/fallback decorators, so every call,
// including rejected and cancelled attempts, has its own durable identity.
type usageProvider struct{ Provider }

func (p *usageProvider) TelemetryProviderName() string { return ProviderTelemetryName(p.Provider) }

func (p *usageProvider) begin(ctx context.Context, req *CompletionRequest) (context.Context, *usageCall, error) {
	record, _ := ctx.Value(usageRecorderKey{}).(UsageRecorder)
	if record == nil {
		return ctx, nil, nil
	}
	id := uuid.NewString()
	observation := store.UsageObservation{ID: id + "/start", CounterID: id, Scope: store.UsageScopeCall, Source: store.UsageSourceProvider,
		Provider: ProviderTelemetryName(p.Provider), Model: req.Model, Status: store.UsageStatusStarted, ObservedAt: time.Now().UTC()}
	if err := record(ctx, observation); err != nil {
		return ctx, nil, &usagePersistenceError{cause: fmt.Errorf("persist model call start: %w", err)}
	}
	call := &usageCall{record: record, observation: observation}
	return context.WithValue(ctx, usageHTTPAttemptKey{}, call), call, nil
}

func finishUsage(ctx context.Context, record UsageRecorder, observation store.UsageObservation) error {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := record(writeCtx, observation); err != nil {
		return &usagePersistenceError{cause: err}
	}
	return nil
}

func (p *usageProvider) Complete(ctx context.Context, req *CompletionRequest) (*CompletionResponse, error) {
	ctx, call, err := p.begin(ctx, req)
	if err != nil {
		return nil, err
	}
	response, callErr := p.Provider.Complete(ctx, req)
	if call == nil {
		return response, callErr
	}
	// SDK error conversion can discard the middleware error's type.
	callErr = errors.Join(callErr, call.persistenceError())
	observation := call.snapshot()
	observation.ID = observation.CounterID + "/finish"
	observation.ObservedAt = time.Now().UTC()
	observation.Status = usageStatus(ctx, callErr)
	if response != nil {
		setUsageCounts(&observation, response.InputTokens, response.OutputTokens, response.CachedInputTokens, response.CacheWriteInputTokens, response.InputExcludesCache, response.UsageReported)
		if response.Model != "" {
			observation.Model = response.Model
		}
		if response.Provider != "" {
			observation.Provider = response.Provider
		}
		observation.Complete = response.UsageReported && callErr == nil
	}
	if err := finishUsage(ctx, call.record, observation); err != nil {
		return response, errors.Join(callErr, fmt.Errorf("persist model call usage: %w", err))
	}
	return response, callErr
}

func (p *usageProvider) Stream(ctx context.Context, req *CompletionRequest) (<-chan StreamChunk, error) {
	ctx, call, err := p.begin(ctx, req)
	if err != nil {
		return nil, err
	}
	upstream, err := p.Provider.Stream(ctx, req)
	if call == nil {
		return upstream, err
	}
	err = errors.Join(err, call.persistenceError())
	if err != nil {
		observation := call.snapshot()
		observation.ID = observation.CounterID + "/finish"
		observation.Status, observation.ObservedAt = usageStatus(ctx, err), time.Now().UTC()
		return nil, errors.Join(err, finishUsage(ctx, call.record, observation))
	}
	out := make(chan StreamChunk)
	go func() {
		defer close(out)
		observation := call.snapshot()
		sequence := 0
		finished := false
		defer func() {
			if finished {
				return
			}
			if latest := call.snapshot(); latest.CounterID != observation.CounterID {
				observation = latest
			}
			observation.ID, observation.Status = observation.CounterID+"/finish", usageStatus(ctx, errors.New("stream ended without final usage"))
			observation.ObservedAt, observation.Complete = time.Now().UTC(), false
			// The durable start still exposes a measurement gap if this write fails.
			_ = finishUsage(ctx, call.record, observation)
		}()
		for {
			var chunk StreamChunk
			var ok bool
			select {
			case <-ctx.Done():
				return
			case chunk, ok = <-upstream:
				if !ok {
					return
				}
			}
			if latest := call.snapshot(); latest.CounterID != observation.CounterID {
				observation = latest
				sequence = 0
			}
			chunk.Error = errors.Join(chunk.Error, call.persistenceError())
			if chunk.Model != "" {
				observation.Model = chunk.Model
			}
			if chunk.Provider != "" {
				observation.Provider = chunk.Provider
			}
			if chunk.UsageReported || chunk.InputTokens > 0 || chunk.OutputTokens > 0 {
				setUsageCounts(&observation, chunk.InputTokens, chunk.OutputTokens, chunk.CachedInputTokens, chunk.CacheWriteInputTokens, chunk.InputExcludesCache, chunk.UsageReported)
			}
			if chunk.UsageReported || chunk.Done || chunk.Error != nil {
				sequence++
				observation.ID = fmt.Sprintf("%s/%d", observation.CounterID, sequence)
				observation.ObservedAt = time.Now().UTC()
				observation.Status = "running"
				if chunk.Done || chunk.Error != nil {
					observation.ID = observation.CounterID + "/finish"
					observation.Status = usageStatus(ctx, chunk.Error)
					observation.Complete = chunk.Error == nil && chunk.UsageReported
					finished = true
				}
				if err := finishUsage(ctx, call.record, observation); err != nil {
					chunk.Error, chunk.Done = fmt.Errorf("persist model stream usage: %w", err), true
					finished = true
				}
			}
			select {
			case out <- chunk:
			case <-ctx.Done():
				return
			}
			if finished {
				return
			}
		}
	}()
	return out, nil
}

func usageStatus(ctx context.Context, err error) string {
	if ctx.Err() != nil {
		return store.UsageStatusCancelled
	}
	if err != nil {
		return store.UsageStatusFailed
	}
	return store.UsageStatusCompleted
}

func setUsageCounts(observation *store.UsageObservation, input, output int, cached, cacheWrite *int64, excludesCache, reported bool) {
	i, o := int64(input), int64(output)
	if excludesCache {
		if cached != nil {
			i += *cached
		}
		if cacheWrite != nil {
			i += *cacheWrite
		}
	}
	if reported || i > 0 {
		observation.InputTokens = &i
	}
	if reported || o > 0 {
		observation.OutputTokens = &o
	}
	if cached != nil {
		v := *cached
		observation.CachedInputTokens = &v
	}
	if cacheWrite != nil {
		v := *cacheWrite
		observation.CacheWriteInputTokens = &v
	}
}
