/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package llm

import (
	"context"
	"errors"
	"testing"
)

func TestTracingProviderName(t *testing.T) {
	tp := NewTracingProvider(&mockProvider{name: "test-provider"})
	if got := tp.Name(); got != "test-provider" {
		t.Errorf("Name() = %q, want %q", got, "test-provider")
	}
}

func TestTracingProviderComplete(t *testing.T) {
	tests := []struct {
		name    string
		inner   Provider
		wantErr bool
	}{
		{
			name:    "success delegates to inner",
			inner:   &mockProvider{name: "test"},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tp := NewTracingProvider(tt.inner)
			resp, err := tp.Complete(context.Background(), &CompletionRequest{Model: "gpt-4"})
			if (err != nil) != tt.wantErr {
				t.Fatalf("Complete() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && resp == nil {
				t.Fatal("Complete() returned nil response")
			}
			if !tt.wantErr && resp.Content != "mock response" {
				t.Errorf("Content = %q, want %q", resp.Content, "mock response")
			}
		})
	}
}

// errorProvider always returns an error from Complete.
type errorProvider struct{ mockProvider }

func (e *errorProvider) Complete(_ context.Context, _ *CompletionRequest) (*CompletionResponse, error) {
	return nil, errors.New("api error")
}

func TestTracingProviderCompleteError(t *testing.T) {
	tp := NewTracingProvider(&errorProvider{mockProvider: mockProvider{name: "err"}})
	_, err := tp.Complete(context.Background(), &CompletionRequest{Model: "gpt-4"})
	if err == nil {
		t.Fatal("Complete() expected error")
	}
	if err.Error() != "api error" {
		t.Errorf("error = %q, want %q", err.Error(), "api error")
	}
}

func TestTracingProviderStream(t *testing.T) {
	tp := NewTracingProvider(&mockProvider{name: "test"})
	ch, err := tp.Stream(context.Background(), &CompletionRequest{Model: "gpt-4"})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	chunk := <-ch
	if chunk.Content != "mock" {
		t.Errorf("chunk.Content = %q, want %q", chunk.Content, "mock")
	}
}

