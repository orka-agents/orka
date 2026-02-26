package llm

import (
	"context"
	"testing"
)

const testFallbackContent = "from fb"

func TestFallbackProvider_PrimarySucceeds(t *testing.T) {
	primary := &retryMockProvider{
		name: "primary",
		completeResults: []completeResult{
			{&CompletionResponse{Content: "from primary"}, nil},
		},
	}
	fb := &retryMockProvider{name: "fallback1"}

	fp := NewFallbackProvider(primary, []FallbackEntry{
		{Provider: fb, Model: "fb-model"},
	})

	resp, err := fp.Complete(context.Background(), &CompletionRequest{Model: "test"})
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if resp.Content != "from primary" {
		t.Errorf("expected 'from primary', got %q", resp.Content)
	}
	if fb.callCount != 0 {
		t.Error("fallback should not have been called")
	}
}

func TestFallbackProvider_PrimaryFails401_FallbackSucceeds(t *testing.T) {
	primary := &retryMockProvider{
		name: "primary",
		completeResults: []completeResult{
			{nil, &ProviderError{StatusCode: 401, Message: "unauthorized"}},
		},
	}
	fb := &retryMockProvider{
		name: "fallback1",
		completeResults: []completeResult{
			{&CompletionResponse{Content: "from fallback"}, nil},
		},
	}

	fp := NewFallbackProvider(primary, []FallbackEntry{
		{Provider: fb, Model: "fb-model"},
	})

	resp, err := fp.Complete(context.Background(), &CompletionRequest{Model: "test"})
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if resp.Content != "from fallback" {
		t.Errorf("expected 'from fallback', got %q", resp.Content)
	}
}

func TestFallbackProvider_PrimaryFails503_FallbackSucceeds(t *testing.T) {
	primary := &retryMockProvider{
		name: "primary",
		completeResults: []completeResult{
			{nil, &ProviderError{StatusCode: 503, Message: "unavailable"}},
		},
	}
	fb := &retryMockProvider{
		name: "fallback1",
		completeResults: []completeResult{
			{&CompletionResponse{Content: testFallbackContent}, nil},
		},
	}

	fp := NewFallbackProvider(primary, []FallbackEntry{
		{Provider: fb},
	})

	resp, err := fp.Complete(context.Background(), &CompletionRequest{Model: "test"})
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if resp.Content != testFallbackContent {
		t.Errorf("expected 'from fb', got %q", resp.Content)
	}
}

func TestFallbackProvider_AllFail(t *testing.T) {
	primary := &retryMockProvider{
		name: "primary",
		completeResults: []completeResult{
			{nil, &ProviderError{StatusCode: 401, Message: "auth error"}},
		},
	}
	fb1 := &retryMockProvider{
		name: "fb1",
		completeResults: []completeResult{
			{nil, &ProviderError{StatusCode: 403, Message: "forbidden"}},
		},
	}
	fb2 := &retryMockProvider{
		name: "fb2",
		completeResults: []completeResult{
			{nil, &ProviderError{StatusCode: 500, Message: "server error"}},
		},
	}

	fp := NewFallbackProvider(primary, []FallbackEntry{
		{Provider: fb1},
		{Provider: fb2},
	})

	_, err := fp.Complete(context.Background(), &CompletionRequest{Model: "test"})
	if err == nil {
		t.Fatal("expected error when all providers fail")
	}
}

func TestFallbackProvider_ModelOverride(t *testing.T) {
	primary := &retryMockProvider{
		name: "primary",
		completeResults: []completeResult{
			{nil, &ProviderError{StatusCode: 401, Message: "unauthorized"}},
		},
	}
	fb := &retryMockProvider{
		name: "fb",
		completeResults: []completeResult{
			{&CompletionResponse{Content: "ok"}, nil},
		},
	}

	fp := NewFallbackProvider(primary, []FallbackEntry{
		{Provider: fb, Model: "override-model"},
	})

	_, err := fp.Complete(context.Background(), &CompletionRequest{Model: "original"})
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if fb.callCount != 1 {
		t.Error("fallback should have been called")
	}
}

func TestFallbackProvider_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	primary := &retryMockProvider{
		name: "primary",
		completeResults: []completeResult{
			{nil, context.Canceled},
		},
	}
	fb := &retryMockProvider{name: "fb"}

	fp := NewFallbackProvider(primary, []FallbackEntry{
		{Provider: fb},
	})

	_, err := fp.Complete(ctx, &CompletionRequest{})
	if err == nil {
		t.Fatal("expected error on cancelled context")
	}
	if fb.callCount != 0 {
		t.Error("fallback should not be called on context cancellation")
	}
}

func TestFallbackProvider_Stream_PrimarySucceeds(t *testing.T) {
	primary := &retryMockProvider{
		name: "primary",
		streamResults: [][]StreamChunk{
			{{Content: "chunk1"}, {Content: "chunk2"}, {Done: true}},
		},
	}
	fb := &retryMockProvider{name: "fb"}

	fp := NewFallbackProvider(primary, []FallbackEntry{
		{Provider: fb},
	})

	ch, err := fp.Stream(context.Background(), &CompletionRequest{})
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	var content string
	for chunk := range ch {
		content += chunk.Content
	}
	if content != "chunk1chunk2" {
		t.Errorf("expected 'chunk1chunk2', got %q", content)
	}
	if fb.streamCallCount != 0 {
		t.Error("fallback should not have been called")
	}
}

func TestFallbackProvider_Stream_PrimaryEmpty_ClosedChannel(t *testing.T) {
	primary := &retryMockProvider{
		name:          "primary",
		streamResults: [][]StreamChunk{{}}, // empty stream
	}
	fb := &retryMockProvider{name: "fb"}

	fp := NewFallbackProvider(primary, []FallbackEntry{
		{Provider: fb},
	})

	ch, err := fp.Stream(context.Background(), &CompletionRequest{})
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

func TestFallbackProvider_Stream_AllFail(t *testing.T) {
	primary := &retryMockProvider{
		name: "primary",
		streamResults: [][]StreamChunk{
			{{Error: &ProviderError{StatusCode: 401, Message: "unauthorized"}, Done: true}},
		},
	}
	fb := &retryMockProvider{
		name: "fb",
		streamResults: [][]StreamChunk{
			{{Error: &ProviderError{StatusCode: 403, Message: "forbidden"}, Done: true}},
		},
	}

	fp := NewFallbackProvider(primary, []FallbackEntry{
		{Provider: fb},
	})

	ch, err := fp.Stream(context.Background(), &CompletionRequest{})
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
		t.Error("expected error chunk when all providers fail")
	}
}

func TestFallbackProvider_Stream_WithCooldown(t *testing.T) {
	primary := &retryMockProvider{name: "primary"}
	fb := &retryMockProvider{
		name: "fb",
		streamResults: [][]StreamChunk{
			{{Content: testFallbackContent}, {Done: true}},
		},
	}

	tracker := NewCooldownTracker()
	tracker.MarkCooldown("primary")

	fp := NewFallbackProvider(primary, []FallbackEntry{
		{Provider: fb},
	})
	fp.SetCooldownTracker(tracker)

	ch, err := fp.Stream(context.Background(), &CompletionRequest{})
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	var content string
	for chunk := range ch {
		content += chunk.Content
	}
	if content != testFallbackContent {
		t.Errorf("expected 'from fb', got %q", content)
	}
	if primary.streamCallCount != 0 {
		t.Error("primary should have been skipped due to cooldown")
	}
}

func TestFallbackProvider_Stream_429MarksCooldown(t *testing.T) {
	primary := &retryMockProvider{
		name: "primary",
		streamResults: [][]StreamChunk{
			{{Error: &ProviderError{StatusCode: 429, Message: "rate limited"}, Done: true}},
		},
	}
	fb := &retryMockProvider{
		name: "fb",
		streamResults: [][]StreamChunk{
			{{Content: "ok"}, {Done: true}},
		},
	}

	tracker := NewCooldownTracker()
	fp := NewFallbackProvider(primary, []FallbackEntry{
		{Provider: fb},
	})
	fp.SetCooldownTracker(tracker)

	ch, err := fp.Stream(context.Background(), &CompletionRequest{})
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	for range ch {
	}

	if !tracker.IsCoolingDown("primary") {
		t.Error("primary should be in cooldown after 429")
	}
}

func TestFallbackProvider_Stream_ModelOverride(t *testing.T) {
	primary := &retryMockProvider{
		name: "primary",
		streamResults: [][]StreamChunk{
			{{Error: &ProviderError{StatusCode: 401, Message: "unauthorized"}, Done: true}},
		},
	}
	fb := &retryMockProvider{
		name: "fb",
		streamResults: [][]StreamChunk{
			{{Content: "ok"}, {Done: true}},
		},
	}

	fp := NewFallbackProvider(primary, []FallbackEntry{
		{Provider: fb, Model: "override-model"},
	})

	ch, err := fp.Stream(context.Background(), &CompletionRequest{Model: "original"})
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	for range ch {
	}
	if fb.streamCallCount != 1 {
		t.Error("fallback should have been called")
	}
}

func TestFallbackProvider_Stream_AllCooledDown_UsesShortestCooldown(t *testing.T) {
	primary := &retryMockProvider{
		name: "primary",
		streamResults: [][]StreamChunk{
			{{Content: "from primary"}, {Done: true}},
		},
	}
	fb := &retryMockProvider{
		name: "fb",
		streamResults: [][]StreamChunk{
			{{Content: testFallbackContent}, {Done: true}},
		},
	}

	tracker := NewCooldownTracker()
	tracker.MarkCooldown("primary")
	tracker.MarkCooldown("primary") // longer cooldown
	tracker.MarkCooldown("fb")      // shorter cooldown

	fp := NewFallbackProvider(primary, []FallbackEntry{
		{Provider: fb},
	})
	fp.SetCooldownTracker(tracker)

	ch, err := fp.Stream(context.Background(), &CompletionRequest{})
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	for range ch {
	}
	if fb.streamCallCount != 1 {
		t.Errorf("expected fb to be called (shortest cooldown), fb calls: %d", fb.streamCallCount)
	}
}

func TestFallbackProvider_Stream_FallbackOnFirstChunkError(t *testing.T) {
	primary := &retryMockProvider{
		name: "primary",
		streamResults: [][]StreamChunk{
			{{Error: &ProviderError{StatusCode: 401, Message: "unauthorized"}, Done: true}},
		},
	}
	fb := &retryMockProvider{
		name: "fb",
		streamResults: [][]StreamChunk{
			{{Content: "hello"}, {Done: true}},
		},
	}

	fp := NewFallbackProvider(primary, []FallbackEntry{
		{Provider: fb},
	})

	ch, err := fp.Stream(context.Background(), &CompletionRequest{})
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	var content string
	for chunk := range ch {
		content += chunk.Content
	}
	if content != "hello" {
		t.Errorf("expected 'hello', got %q", content)
	}
}

func TestFallbackProvider_Name(t *testing.T) {
	fp := NewFallbackProvider(&retryMockProvider{name: "openai"}, nil)
	want := "fallback(openai)"
	if fp.Name() != want {
		t.Errorf("expected %q, got %q", want, fp.Name())
	}
}

func TestFallbackProvider_WithCooldown_SkipsCooledProvider(t *testing.T) {
	primary := &retryMockProvider{
		name: "primary",
	}
	fb := &retryMockProvider{
		name: "fb",
		completeResults: []completeResult{
			{&CompletionResponse{Content: testFallbackContent}, nil},
		},
	}

	tracker := NewCooldownTracker()
	tracker.MarkCooldown("primary")

	fp := NewFallbackProvider(primary, []FallbackEntry{
		{Provider: fb},
	})
	fp.SetCooldownTracker(tracker)

	resp, err := fp.Complete(context.Background(), &CompletionRequest{Model: "test"})
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if resp.Content != testFallbackContent {
		t.Errorf("expected 'from fb', got %q", resp.Content)
	}
	if primary.callCount != 0 {
		t.Error("primary should have been skipped")
	}
}

func TestFallbackProvider_429MarksAndResets(t *testing.T) {
	primary := &retryMockProvider{
		name: "primary",
		completeResults: []completeResult{
			{nil, &ProviderError{StatusCode: 429, Message: "rate limited"}},
		},
	}
	fb := &retryMockProvider{
		name: "fb",
		completeResults: []completeResult{
			{&CompletionResponse{Content: "ok"}, nil},
		},
	}

	tracker := NewCooldownTracker()
	fp := NewFallbackProvider(primary, []FallbackEntry{
		{Provider: fb},
	})
	fp.SetCooldownTracker(tracker)

	_, err := fp.Complete(context.Background(), &CompletionRequest{Model: "test"})
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}

	if !tracker.IsCoolingDown("primary") {
		t.Error("primary should be in cooldown after 429")
	}
	if tracker.IsCoolingDown("fb") {
		t.Error("fb should not be in cooldown after success")
	}
}

func TestFallbackProvider_NoFallbacks(t *testing.T) {
	primary := &retryMockProvider{
		name: "primary",
		completeResults: []completeResult{
			{&CompletionResponse{Content: "ok"}, nil},
		},
	}

	fp := NewFallbackProvider(primary, nil)
	resp, err := fp.Complete(context.Background(), &CompletionRequest{})
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if resp.Content != "ok" {
		t.Errorf("expected 'ok', got %q", resp.Content)
	}
}

func TestFallbackProvider_AllCooledDown_UsesShortestCooldown(t *testing.T) {
	primary := &retryMockProvider{
		name: "primary",
		completeResults: []completeResult{
			{&CompletionResponse{Content: "from primary"}, nil},
		},
	}
	fb := &retryMockProvider{
		name:            "fb",
		completeResults: []completeResult{},
	}

	tracker := NewCooldownTracker()
	// Mark both as cooling down, but primary has more marks (longer cooldown)
	tracker.MarkCooldown("primary")
	tracker.MarkCooldown("primary") // 5m cooldown
	tracker.MarkCooldown("fb")      // 1m cooldown

	fp := NewFallbackProvider(primary, []FallbackEntry{
		{Provider: fb},
	})
	fp.SetCooldownTracker(tracker)

	// Should pick fb (shorter cooldown) but fb has no completeResults
	// Actually, fb will return default "default" from retryMockProvider
	resp, err := fp.Complete(context.Background(), &CompletionRequest{Model: "test"})
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	// fb has shorter cooldown so it should be tried
	if fb.callCount != 1 {
		t.Errorf("expected fb to be called (shortest cooldown), fb calls: %d, primary calls: %d", fb.callCount, primary.callCount)
	}
	_ = resp
}

func TestFallbackProvider_CooldownExpires_PrimaryTriedAgain(t *testing.T) {
	// This test verifies the concept but can't easily test time expiry without
	// a mock clock. Instead, verify that Reset works correctly.
	primary := &retryMockProvider{
		name: "primary",
		completeResults: []completeResult{
			{&CompletionResponse{Content: "primary works"}, nil},
		},
	}
	fb := &retryMockProvider{name: "fb"}

	tracker := NewCooldownTracker()
	tracker.MarkCooldown("primary")
	// Immediately reset (simulating expiry)
	tracker.Reset("primary")

	fp := NewFallbackProvider(primary, []FallbackEntry{
		{Provider: fb},
	})
	fp.SetCooldownTracker(tracker)

	resp, err := fp.Complete(context.Background(), &CompletionRequest{Model: "test"})
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if resp.Content != "primary works" {
		t.Errorf("expected primary to be tried after reset, got %q", resp.Content)
	}
	if primary.callCount != 1 {
		t.Error("primary should have been called after cooldown reset")
	}
	if fb.callCount != 0 {
		t.Error("fb should not have been called")
	}
}

func TestFallbackProvider_SuccessfulCallResetsCooldown(t *testing.T) {
	primary := &retryMockProvider{
		name: "primary",
		completeResults: []completeResult{
			{nil, &ProviderError{StatusCode: 429, Message: "rate limited"}},
		},
	}
	fb := &retryMockProvider{
		name: "fb",
		completeResults: []completeResult{
			{&CompletionResponse{Content: "ok"}, nil},
		},
	}

	tracker := NewCooldownTracker()
	fp := NewFallbackProvider(primary, []FallbackEntry{
		{Provider: fb},
	})
	fp.SetCooldownTracker(tracker)

	_, err := fp.Complete(context.Background(), &CompletionRequest{Model: "test"})
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}

	// fb succeeded → its cooldown should be reset
	if tracker.IsCoolingDown("fb") {
		t.Error("successful provider should have cooldown reset")
	}
	// primary got 429 → should be in cooldown
	if !tracker.IsCoolingDown("primary") {
		t.Error("429'd provider should be in cooldown")
	}
}
