/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package llm

import (
	"context"
	"errors"
	"testing"

	"github.com/orka-agents/orka/internal/tracing/testutil"
)

const privacyMarker = "SYNTHETIC_TOKEN_PRIVATE_BODY"

type privacyStreamProvider struct {
	telemetryProvider
	chunks    <-chan StreamChunk
	streamErr error
	cancel    context.CancelFunc
}

func (p *privacyStreamProvider) Stream(context.Context, *CompletionRequest) (<-chan StreamChunk, error) {
	if p.cancel != nil {
		p.cancel()
	}
	return p.chunks, p.streamErr
}

func TestTracingProviderErrorPrivacy(t *testing.T) {
	providerErr := &ProviderError{Provider: "openai", StatusCode: 401, Message: privacyMarker}
	for _, stream := range []bool{false, true} {
		for _, tc := range []struct {
			name string
			err  error
			want string
		}{
			{"provider", providerErr, "401"},
			{"generic", errors.New(privacyMarker), "*errors.errorString"},
		} {
			t.Run(tc.name+map[bool]string{false: "/complete", true: "/stream_start"}[stream], func(t *testing.T) {
				h := testutil.NewSpanHarness(t)
				p := &privacyStreamProvider{telemetryProvider: telemetryProvider{name: "openai", err: tc.err}, streamErr: tc.err}
				traced := NewTracingProvider(p)
				var got error
				if stream {
					_, got = traced.Stream(context.Background(), &CompletionRequest{Model: "model"})
				} else {
					_, got = traced.Complete(context.Background(), &CompletionRequest{Model: "model"})
				}
				if got != tc.err {
					t.Fatal("error return changed")
				}
				spans := h.Recorder.Ended()
				if len(spans) != 1 {
					t.Fatalf("spans = %d", len(spans))
				}
				testutil.AssertNoSpanText(t, spans[0], privacyMarker)
				testutil.AssertSpanError(t, spans[0], tc.want)
			})
		}
	}
}

func TestTracingProviderStreamErrorPrivacy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		chunks []StreamChunk
		cancel bool
		want   string
	}{
		{"chunk error", []StreamChunk{{Error: errors.New(privacyMarker)}, {Done: true}}, false, "*errors.errorString"},
		{"unknown stop reason", []StreamChunk{{Done: true, StopReason: privacyMarker}}, false, unknownStopReasonErrorType},
		{"canceled closed stream", nil, true, "context.canceled"},
		{"canceled delivery", []StreamChunk{{Content: privacyMarker}}, true, "context.canceled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := testutil.NewSpanHarness(t)
			chunks := make(chan StreamChunk, len(tc.chunks))
			for _, chunk := range tc.chunks {
				chunks <- chunk
			}
			close(chunks)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			p := &privacyStreamProvider{telemetryProvider: telemetryProvider{name: "openai"}, chunks: chunks}
			if tc.cancel {
				p.cancel = cancel
			}
			out, err := NewTracingProvider(p).Stream(ctx, &CompletionRequest{Model: "model"})
			if err != nil {
				t.Fatal(err)
			}
			for range out {
			}
			spans := h.Recorder.Ended()
			if len(spans) != 1 {
				t.Fatalf("spans = %d", len(spans))
			}
			testutil.AssertNoSpanText(t, spans[0], privacyMarker)
			testutil.AssertSpanError(t, spans[0], tc.want)
		})
	}
}

func TestTracingProviderResponsePrivacy(t *testing.T) {
	h := testutil.NewSpanHarness(t)
	response := &CompletionResponse{Content: privacyMarker, StopReason: privacyMarker}
	got, err := NewTracingProvider(&telemetryProvider{name: "openai", resp: response}).Complete(context.Background(), &CompletionRequest{Model: "model"})
	if err != nil || got != response {
		t.Fatal("response changed")
	}
	span := h.Recorder.Ended()[0]
	testutil.AssertNoSpanText(t, span, privacyMarker)
	testutil.AssertSpanError(t, span, unknownStopReasonErrorType)
}
