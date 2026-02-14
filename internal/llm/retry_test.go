package llm

import (
	"context"
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
	var content string
	for chunk := range ch {
		if chunk.Error != nil {
			t.Fatalf("unexpected error chunk: %v", chunk.Error)
		}
		content += chunk.Content
	}
	if content != "hello world" {
		t.Errorf("expected 'hello world', got %q", content)
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
