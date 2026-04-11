package llm

import (
	"context"
	"strings"
	"testing"
	"time"
)

// retryMockProvider is a test helper that returns configured responses.
type retryMockProvider struct {
	name            string
	completeResults []completeResult
	streamResults   [][]StreamChunk
	callCount       int
	streamCallCount int
}

type completeResult struct {
	resp *CompletionResponse
	err  error
}

func (m *retryMockProvider) Name() string { return m.name }

func (m *retryMockProvider) Complete(_ context.Context, _ *CompletionRequest) (*CompletionResponse, error) {
	idx := m.callCount
	m.callCount++
	if idx < len(m.completeResults) {
		r := m.completeResults[idx]
		return r.resp, r.err
	}
	return &CompletionResponse{Content: "default"}, nil
}

func (m *retryMockProvider) Stream(_ context.Context, _ *CompletionRequest) (<-chan StreamChunk, error) {
	idx := m.streamCallCount
	m.streamCallCount++
	ch := make(chan StreamChunk, 10)
	go func() {
		defer close(ch)
		if idx < len(m.streamResults) {
			for _, chunk := range m.streamResults[idx] {
				ch <- chunk
			}
		}
	}()
	return ch, nil
}

func TestRetryProvider_Complete_RetriesOn429(t *testing.T) {
	mock := &retryMockProvider{
		name: "test",
		completeResults: []completeResult{
			{nil, &ProviderError{StatusCode: 429, Message: "rate limited"}},
			{nil, &ProviderError{StatusCode: 429, Message: "rate limited"}},
			{&CompletionResponse{Content: "success"}, nil},
		},
	}
	rp := NewRetryProvider(mock, 3)
	rp.baseDelay = time.Millisecond
	resp, err := rp.Complete(context.Background(), &CompletionRequest{})
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if resp.Content != "success" {
		t.Errorf("expected 'success', got %q", resp.Content)
	}
	if mock.callCount != 3 {
		t.Errorf("expected 3 calls, got %d", mock.callCount)
	}
}

func TestRetryProvider_Complete_RetriesOn503(t *testing.T) {
	mock := &retryMockProvider{
		name: "test",
		completeResults: []completeResult{
			{nil, &ProviderError{StatusCode: 503, Message: "unavailable"}},
			{&CompletionResponse{Content: "ok"}, nil},
		},
	}
	rp := NewRetryProvider(mock, 3)
	rp.baseDelay = time.Millisecond
	resp, err := rp.Complete(context.Background(), &CompletionRequest{})
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if resp.Content != "ok" {
		t.Errorf("expected 'ok', got %q", resp.Content)
	}
	if mock.callCount != 2 {
		t.Errorf("expected 2 calls, got %d", mock.callCount)
	}
}

func TestRetryProvider_Complete_NoRetryOn401(t *testing.T) {
	mock := &retryMockProvider{
		name: "test",
		completeResults: []completeResult{
			{nil, &ProviderError{StatusCode: 401, Message: "unauthorized"}},
		},
	}
	rp := NewRetryProvider(mock, 3)
	rp.baseDelay = time.Millisecond
	_, err := rp.Complete(context.Background(), &CompletionRequest{})
	if err == nil {
		t.Fatal("expected error")
	}
	if mock.callCount != 1 {
		t.Errorf("expected 1 call (no retry), got %d", mock.callCount)
	}
}

func TestRetryProvider_Complete_NoRetryOn400(t *testing.T) {
	mock := &retryMockProvider{
		name: "test",
		completeResults: []completeResult{
			{nil, &ProviderError{StatusCode: 400, Message: "bad request"}},
		},
	}
	rp := NewRetryProvider(mock, 3)
	rp.baseDelay = time.Millisecond
	_, err := rp.Complete(context.Background(), &CompletionRequest{})
	if err == nil {
		t.Fatal("expected error")
	}
	if mock.callCount != 1 {
		t.Errorf("expected 1 call (no retry), got %d", mock.callCount)
	}
}

func TestRetryProvider_Complete_ExceedsMaxRetries(t *testing.T) {
	mock := &retryMockProvider{
		name: "test",
		completeResults: []completeResult{
			{nil, &ProviderError{StatusCode: 429, Message: "rate limited"}},
			{nil, &ProviderError{StatusCode: 429, Message: "rate limited"}},
			{nil, &ProviderError{StatusCode: 429, Message: "rate limited"}},
			{nil, &ProviderError{StatusCode: 429, Message: "rate limited"}},
		},
	}
	rp := NewRetryProvider(mock, 3)
	rp.baseDelay = time.Millisecond
	_, err := rp.Complete(context.Background(), &CompletionRequest{})
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if mock.callCount != 4 { // 1 initial + 3 retries
		t.Errorf("expected 4 calls, got %d", mock.callCount)
	}
}

func TestRetryProvider_Complete_ContextCancelled(t *testing.T) {
	mock := &retryMockProvider{
		name: "test",
		completeResults: []completeResult{
			{nil, &ProviderError{StatusCode: 429, Message: "rate limited"}},
			{nil, &ProviderError{StatusCode: 429, Message: "rate limited"}},
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	rp := NewRetryProvider(mock, 3)
	rp.baseDelay = 5 * time.Second // long enough to cancel during sleep
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, err := rp.Complete(ctx, &CompletionRequest{})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestRetryProvider_Stream_PeekRetry(t *testing.T) {
	mock := &retryMockProvider{
		name: "test",
		streamResults: [][]StreamChunk{
			{{Error: &ProviderError{StatusCode: 429, Message: "rate limited"}, Done: true}},
			{{Content: "hello"}, {Content: " world"}, {Done: true}},
		},
	}
	rp := NewRetryProvider(mock, 3)
	rp.baseDelay = time.Millisecond
	ch, err := rp.Stream(context.Background(), &CompletionRequest{})
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	var content strings.Builder
	for chunk := range ch {
		if chunk.Error != nil {
			t.Fatalf("unexpected error chunk: %v", chunk.Error)
		}
		content.WriteString(chunk.Content)
	}
	if content.String() != "hello world" {
		t.Errorf("expected 'hello world', got %q", content.String())
	}
	if mock.streamCallCount != 2 {
		t.Errorf("expected 2 stream calls, got %d", mock.streamCallCount)
	}
}

func TestRetryProvider_Stream_PeekPassthrough(t *testing.T) {
	mock := &retryMockProvider{
		name: "test",
		streamResults: [][]StreamChunk{
			{{Content: "chunk1"}, {Content: "chunk2"}, {Done: true}},
		},
	}
	rp := NewRetryProvider(mock, 3)
	ch, err := rp.Stream(context.Background(), &CompletionRequest{})
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	var chunks []string
	for chunk := range ch {
		if chunk.Content != "" {
			chunks = append(chunks, chunk.Content)
		}
	}
	if len(chunks) != 2 || chunks[0] != "chunk1" || chunks[1] != "chunk2" {
		t.Errorf("unexpected chunks: %v", chunks)
	}
}

func TestRetryProvider_Stream_AllRetriesExhausted(t *testing.T) {
	mock := &retryMockProvider{
		name: "test",
		streamResults: [][]StreamChunk{
			{{Error: &ProviderError{StatusCode: 503, Message: "unavailable"}, Done: true}},
			{{Error: &ProviderError{StatusCode: 503, Message: "unavailable"}, Done: true}},
			{{Error: &ProviderError{StatusCode: 503, Message: "unavailable"}, Done: true}},
			{{Error: &ProviderError{StatusCode: 503, Message: "unavailable"}, Done: true}},
		},
	}
	rp := NewRetryProvider(mock, 3)
	rp.baseDelay = time.Millisecond
	ch, err := rp.Stream(context.Background(), &CompletionRequest{})
	if err != nil {
		t.Fatalf("expected no error from Stream(), got %v", err)
	}
	var gotError bool
	for chunk := range ch {
		if chunk.Error != nil {
			gotError = true
		}
	}
	if !gotError {
		t.Error("expected error chunk after exhausting retries")
	}
}

func TestRetryProvider_DefaultMaxRetries(t *testing.T) {
	rp := NewRetryProvider(&retryMockProvider{name: "test"}, 0)
	if rp.maxRetries != 3 {
		t.Errorf("expected default maxRetries=3, got %d", rp.maxRetries)
	}
}

func TestRetryProvider_Name(t *testing.T) {
	rp := NewRetryProvider(&retryMockProvider{name: "myname"}, 0)
	if rp.Name() != "myname" {
		t.Errorf("expected 'myname', got %q", rp.Name())
	}
}

func TestRetryProvider_Backoff(t *testing.T) {
	rp := NewRetryProvider(&retryMockProvider{name: "test"}, 3)

	// Attempt 0: ~1s base
	d0 := rp.backoff(0)
	if d0 < 900*time.Millisecond || d0 > 1100*time.Millisecond {
		t.Errorf("attempt 0 backoff expected ~1s, got %v", d0)
	}

	// Attempt 1: ~2s
	d1 := rp.backoff(1)
	if d1 < 1800*time.Millisecond || d1 > 2200*time.Millisecond {
		t.Errorf("attempt 1 backoff expected ~2s, got %v", d1)
	}

	// Attempt 2: ~4s
	d2 := rp.backoff(2)
	if d2 < 3600*time.Millisecond || d2 > 4400*time.Millisecond {
		t.Errorf("attempt 2 backoff expected ~4s, got %v", d2)
	}
}

func TestRetryProvider_Backoff_CappedAtMaxDelay(t *testing.T) {
	rp := NewRetryProvider(&retryMockProvider{name: "test"}, 3)
	rp.maxDelay = 5 * time.Second

	// High attempt should be capped
	d := rp.backoff(100)
	if d > 6*time.Second {
		t.Errorf("backoff should be capped at maxDelay, got %v", d)
	}
}

func TestRetryProvider_Stream_EmptyChannel(t *testing.T) {
	mock := &retryMockProvider{
		name:          "test",
		streamResults: [][]StreamChunk{{}}, // empty stream
	}
	rp := NewRetryProvider(mock, 1)
	ch, err := rp.Stream(context.Background(), &CompletionRequest{})
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	chunks := 0
	for range ch {
		chunks++
	}
	if chunks != 0 {
		t.Errorf("expected 0 chunks from empty stream, got %d", chunks)
	}
}

func TestRetryProvider_Stream_NonRetryableFirstChunk(t *testing.T) {
	mock := &retryMockProvider{
		name: "test",
		streamResults: [][]StreamChunk{
			{{Error: &ProviderError{StatusCode: 400, Message: "bad request"}, Done: true}},
		},
	}
	rp := NewRetryProvider(mock, 3)
	rp.baseDelay = time.Millisecond
	ch, err := rp.Stream(context.Background(), &CompletionRequest{})
	if err != nil {
		t.Fatalf("expected no error from Stream(), got %v", err)
	}
	var gotError bool
	for chunk := range ch {
		if chunk.Error != nil {
			gotError = true
		}
	}
	if !gotError {
		t.Error("expected error chunk for non-retryable error")
	}
	if mock.streamCallCount != 1 {
		t.Errorf("expected 1 stream call (no retry for 400), got %d", mock.streamCallCount)
	}
}
