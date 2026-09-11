package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type fallbackContextCaptureProvider struct {
	name              string
	err               error
	firstChunkFailure bool
	requests          []*CompletionRequest
}

func (p *fallbackContextCaptureProvider) Name() string { return p.name }

func (p *fallbackContextCaptureProvider) Complete(_ context.Context, req *CompletionRequest) (*CompletionResponse, error) {
	p.requests = append(p.requests, req)
	if p.err != nil {
		return nil, p.err
	}
	return &CompletionResponse{Content: "done", Model: req.Model}, nil
}

func (p *fallbackContextCaptureProvider) Stream(_ context.Context, req *CompletionRequest) (<-chan StreamChunk, error) {
	p.requests = append(p.requests, req)
	if p.err != nil && !p.firstChunkFailure {
		return nil, p.err
	}
	ch := make(chan StreamChunk, 1)
	ch <- StreamChunk{Content: "done", Error: p.err, Done: true}
	close(ch)
	return ch, nil
}

func callFallbackContextProvider(provider *FallbackProvider, req *CompletionRequest, mode string) error {
	if mode == "complete" {
		_, err := provider.Complete(context.Background(), req)
		return err
	}
	chunks, err := provider.Stream(context.Background(), req)
	if err != nil {
		return err
	}
	var streamErr error
	for chunk := range chunks {
		if chunk.Error != nil {
			streamErr = chunk.Error
		}
	}
	return streamErr
}

func assertFallbackContextRequestUnchanged(t *testing.T, req *CompletionRequest, original []byte, window int) {
	t.Helper()
	encoded, err := json.Marshal(req)
	require.NoError(t, err)
	require.Equal(t, original, encoded, "fallback admission changed the caller's request")
	require.Equal(t, window, req.ContextWindow)
	require.Equal(t, "current-request", req.Messages[0].ID)
}

func TestFallbackContextRejectsSmallerModelBeforeBackendCall(t *testing.T) {
	for _, mode := range []string{"complete", "stream"} {
		for _, firstChunk := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/first-chunk=%t", mode, firstChunk), func(t *testing.T) {
				primary := &fallbackContextCaptureProvider{name: "primary", err: &ProviderError{StatusCode: 503, Message: "unavailable"}, firstChunkFailure: firstChunk}
				fallback := &fallbackContextCaptureProvider{name: "fallback"}
				provider := NewFallbackProvider(primary, []FallbackEntry{{Provider: fallback, Model: "smaller-model", ContextWindow: 256}})
				req := contextBudgetTestRequest()
				req.ContextWindow = 4096
				original, err := json.Marshal(req)
				require.NoError(t, err)
				err = callFallbackContextProvider(provider, req, mode)
				require.ErrorIs(t, err, ErrContextLimit)
				var limit *ContextLimitError
				require.ErrorAs(t, err, &limit)
				require.Equal(t, "smaller-model", limit.Model)
				require.Equal(t, 256, limit.Window)
				require.Equal(t, 64, limit.OutputTokens)
				require.Greater(t, limit.InputTokens+limit.OutputTokens, limit.Window)
				require.Len(t, primary.requests, 1)
				require.Empty(t, fallback.requests)
				assertFallbackContextRequestUnchanged(t, req, original, 4096)
			})
		}
	}
}

func TestFallbackContextAdmissionKeepsSelectedModelAndRequest(t *testing.T) {
	for _, mode := range []string{"complete", "stream"} {
		t.Run(mode, func(t *testing.T) {
			primary := &fallbackContextCaptureProvider{name: "primary", err: &ProviderError{StatusCode: 503, Message: "unavailable"}}
			fallback := &fallbackContextCaptureProvider{name: "fallback"}
			provider := NewFallbackProvider(primary, []FallbackEntry{{Provider: fallback, Model: "fallback-model", ContextWindow: 2048}})
			req := contextBudgetTestRequest()
			req.ContextWindow = 4096
			original, err := json.Marshal(req)
			require.NoError(t, err)
			require.NoError(t, callFallbackContextProvider(provider, req, mode))
			require.Len(t, primary.requests, 1)
			require.Len(t, fallback.requests, 1)
			require.Equal(t, "primary-model", primary.requests[0].Model)
			require.Equal(t, 4096, primary.requests[0].ContextWindow)
			require.Equal(t, "fallback-model", fallback.requests[0].Model)
			require.Equal(t, 2048, fallback.requests[0].ContextWindow)
			require.Equal(t, req.Messages, fallback.requests[0].Messages)
			require.NotSame(t, req, primary.requests[0])
			require.NotSame(t, req, fallback.requests[0])
			assertFallbackContextRequestUnchanged(t, req, original, 4096)
		})
	}
}

func TestFallbackContextTriesLaterFittingModels(t *testing.T) {
	for _, mode := range []string{"complete", "stream"} {
		for _, firstChunk := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/first-chunk=%t", mode, firstChunk), func(t *testing.T) {
				primary := &fallbackContextCaptureProvider{name: "primary", err: &ProviderError{StatusCode: 503, Message: "unavailable"}, firstChunkFailure: firstChunk}
				small := &fallbackContextCaptureProvider{name: "small"}
				large := &fallbackContextCaptureProvider{name: "large"}
				provider := NewFallbackProvider(primary, []FallbackEntry{
					{Provider: small, Model: "small-model", ContextWindow: 1024},
					{Provider: large, Model: "large-model", ContextWindow: 8192},
				})
				req := contextBudgetTestRequest()
				req.ContextWindow = 8192
				req.Messages[0].Content = strings.Repeat("required request ", 500)
				original, err := json.Marshal(req)
				require.NoError(t, err)
				require.NoError(t, callFallbackContextProvider(provider, req, mode))
				require.Len(t, primary.requests, 1)
				require.Empty(t, small.requests)
				require.Len(t, large.requests, 1)
				require.Equal(t, "large-model", large.requests[0].Model)
				assertFallbackContextRequestUnchanged(t, req, original, 8192)
			})
		}
	}
}

func TestFallbackContextReturnsLargestRejectedAllowance(t *testing.T) {
	for _, mode := range []string{"complete", "stream"} {
		t.Run(mode, func(t *testing.T) {
			primary := &fallbackContextCaptureProvider{name: "primary", err: &ProviderError{StatusCode: 503, Message: "unavailable"}}
			small := &fallbackContextCaptureProvider{name: "small"}
			medium := &fallbackContextCaptureProvider{name: "medium"}
			failed := &fallbackContextCaptureProvider{name: "failed", err: &ProviderError{StatusCode: 503, Message: "unavailable"}}
			provider := NewFallbackProvider(primary, []FallbackEntry{
				{Provider: small, Model: "small-model", ContextWindow: 1024},
				{Provider: medium, Model: "medium-model", ContextWindow: 2048},
				{Provider: failed, Model: "failed-model", ContextWindow: 8192},
			})
			req := contextBudgetTestRequest()
			req.ContextWindow = 8192
			req.Messages[0].Content = strings.Repeat("required request ", 500)
			err := callFallbackContextProvider(provider, req, mode)
			var limit *ContextLimitError
			require.ErrorAs(t, err, &limit)
			require.Equal(t, "medium-model", limit.Model)
			require.Equal(t, 2048, limit.Window)
			require.Empty(t, small.requests)
			require.Empty(t, medium.requests)
			require.Len(t, failed.requests, 1)
		})
	}
}

func TestFallbackContextMissingAllowanceFailsClosedOnlyWhenEnabled(t *testing.T) {
	for _, mode := range []string{"complete", "stream"} {
		for _, enabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/enabled=%t", mode, enabled), func(t *testing.T) {
				primary := &fallbackContextCaptureProvider{name: "primary", err: &ProviderError{StatusCode: 503, Message: "unavailable"}}
				fallback := &fallbackContextCaptureProvider{name: "fallback"}
				provider := NewFallbackProvider(primary, []FallbackEntry{{Provider: fallback, Model: "fallback-model"}})
				req := contextBudgetTestRequest()
				if enabled {
					req.ContextWindow = 4096
				}
				err := callFallbackContextProvider(provider, req, mode)
				if enabled {
					require.ErrorContains(t, err, "context allowance is required")
					require.Empty(t, fallback.requests)
				} else {
					require.NoError(t, err)
					require.Len(t, fallback.requests, 1)
					require.Zero(t, fallback.requests[0].ContextWindow)
				}
				require.Len(t, primary.requests, 1)
			})
		}
	}
}

func TestFallbackContextDisabledIgnoresFallbackAllowance(t *testing.T) {
	for _, mode := range []string{"complete", "stream"} {
		t.Run(mode, func(t *testing.T) {
			primary := &fallbackContextCaptureProvider{name: "primary", err: &ProviderError{StatusCode: 503, Message: "unavailable"}}
			fallback := &fallbackContextCaptureProvider{name: "fallback"}
			provider := NewFallbackProvider(primary, []FallbackEntry{{Provider: fallback, Model: "fallback-model", ContextWindow: 1}})
			req := contextBudgetTestRequest()
			require.NoError(t, callFallbackContextProvider(provider, req, mode))
			require.Len(t, fallback.requests, 1)
			require.Zero(t, fallback.requests[0].ContextWindow)
		})
	}
}

func TestFallbackContextCooldownRetainsModelAndAllowance(t *testing.T) {
	for _, mode := range []string{"complete", "stream"} {
		for _, allCooling := range []bool{false, true} {
			for _, fallbackWindow := range []int{32, 2048} {
				t.Run(fmt.Sprintf("%s/all-cooling=%t/window=%d", mode, allCooling, fallbackWindow), func(t *testing.T) {
					primary := &fallbackContextCaptureProvider{name: "primary"}
					fallback := &fallbackContextCaptureProvider{name: "fallback"}
					provider := NewFallbackProvider(primary, []FallbackEntry{{Provider: fallback, Model: "cooldown-model", ContextWindow: fallbackWindow}})
					tracker := NewCooldownTracker()
					tracker.MarkCooldown(primary.Name())
					tracker.MarkCooldown(primary.Name())
					if allCooling {
						tracker.MarkCooldown(fallback.Name())
					}
					provider.SetCooldownTracker(tracker)
					req := contextBudgetTestRequest()
					req.ContextWindow = 4096
					original, err := json.Marshal(req)
					require.NoError(t, err)
					err = callFallbackContextProvider(provider, req, mode)
					if fallbackWindow == 32 {
						var limit *ContextLimitError
						require.ErrorAs(t, err, &limit)
						require.Equal(t, "cooldown-model", limit.Model)
						require.Equal(t, fallbackWindow, limit.Window)
						require.Empty(t, fallback.requests)
					} else {
						require.NoError(t, err)
						require.Len(t, fallback.requests, 1)
						require.Equal(t, "cooldown-model", fallback.requests[0].Model)
						require.Equal(t, fallbackWindow, fallback.requests[0].ContextWindow)
					}
					require.Empty(t, primary.requests)
					assertFallbackContextRequestUnchanged(t, req, original, 4096)
				})
			}
		}
	}
}

type nonComparableFallbackContextProvider struct {
	*fallbackContextCaptureProvider
	options []string
}

func TestFallbackContextCooldownSupportsNonComparableProviders(t *testing.T) {
	for _, mode := range []string{"complete", "stream"} {
		t.Run(mode, func(t *testing.T) {
			primary := nonComparableFallbackContextProvider{
				fallbackContextCaptureProvider: &fallbackContextCaptureProvider{name: "primary"},
				options:                        []string{"primary-option"},
			}
			fallback := nonComparableFallbackContextProvider{
				fallbackContextCaptureProvider: &fallbackContextCaptureProvider{name: "fallback"},
				options:                        []string{"fallback-option"},
			}
			provider := NewFallbackProvider(primary, []FallbackEntry{{Provider: fallback, Model: "fallback-model", ContextWindow: 2048}})
			tracker := NewCooldownTracker()
			tracker.MarkCooldown(primary.Name())
			tracker.MarkCooldown(primary.Name())
			tracker.MarkCooldown(fallback.Name())
			provider.SetCooldownTracker(tracker)
			req := contextBudgetTestRequest()
			req.ContextWindow = 4096
			var err error
			require.NotPanics(t, func() { err = callFallbackContextProvider(provider, req, mode) })
			require.NoError(t, err)
			require.Empty(t, primary.requests)
			require.Len(t, fallback.requests, 1)
			require.Equal(t, "fallback-model", fallback.requests[0].Model)
			require.Equal(t, 2048, fallback.requests[0].ContextWindow)
		})
	}
}
