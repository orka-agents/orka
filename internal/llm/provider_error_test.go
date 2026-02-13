package llm

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
)

func TestProviderError_IsRetryable(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		want       bool
	}{
		{"429 rate limit", 429, true},
		{"500 server error", 500, true},
		{"502 bad gateway", 502, true},
		{"503 service unavailable", 503, true},
		{"529 overloaded", 529, true},
		{"400 bad request", 400, false},
		{"401 unauthorized", 401, false},
		{"403 forbidden", 403, false},
		{"404 not found", 404, false},
		{"0 unset", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pe := &ProviderError{StatusCode: tt.statusCode}
			if got := pe.IsRetryable(); got != tt.want {
				t.Errorf("IsRetryable() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestProviderError_IsProviderDown(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		want       bool
	}{
		{"401 unauthorized", 401, true},
		{"403 forbidden", 403, true},
		{"429 rate limit", 429, false},
		{"500 server error", 500, false},
		{"0 unset", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pe := &ProviderError{StatusCode: tt.statusCode}
			if got := pe.IsProviderDown(); got != tt.want {
				t.Errorf("IsProviderDown() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestProviderError_IsContextTooLong(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		message    string
		want       bool
	}{
		{"400 with context keyword", 400, "context length exceeded", true},
		{"400 with token keyword", 400, "maximum token limit", true},
		{"400 with too long", 400, "input too long", true},
		{"400 with maximum", 400, "maximum context length", true},
		{"400 without keywords", 400, "invalid request format", false},
		{"429 with context keyword", 429, "context length exceeded", false},
		{"0 with context keyword", 0, "context length exceeded", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pe := &ProviderError{StatusCode: tt.statusCode, Message: tt.message}
			if got := pe.IsContextTooLong(); got != tt.want {
				t.Errorf("IsContextTooLong() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestShouldRetry(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"ProviderError 429", &ProviderError{StatusCode: 429}, true},
		{"ProviderError 503", &ProviderError{StatusCode: 503}, true},
		{"ProviderError 401", &ProviderError{StatusCode: 401}, false},
		{"ProviderError 400", &ProviderError{StatusCode: 400}, false},
		{"context.DeadlineExceeded", context.DeadlineExceeded, false},
		{"context.Canceled", context.Canceled, false},
		{"wrapped deadline", fmt.Errorf("wrap: %w", context.DeadlineExceeded), false},
		{"network error", &net.OpError{Op: "dial", Err: errors.New("connection refused")}, true},
		{"generic error", errors.New("something went wrong"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ShouldRetry(tt.err); got != tt.want {
				t.Errorf("ShouldRetry() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestShouldFallback(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"ProviderError 401", &ProviderError{StatusCode: 401}, true},
		{"ProviderError 403", &ProviderError{StatusCode: 403}, true},
		{"ProviderError 429", &ProviderError{StatusCode: 429}, false},
		{"ProviderError 400", &ProviderError{StatusCode: 400}, false},
		{"context.DeadlineExceeded", context.DeadlineExceeded, false},
		{"context.Canceled", context.Canceled, false},
		{"network error", &net.OpError{Op: "dial", Err: errors.New("connection refused")}, true},
		{"generic error", errors.New("something went wrong"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ShouldFallback(tt.err); got != tt.want {
				t.Errorf("ShouldFallback() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsContextTooLongErr(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"ProviderError 400 context", &ProviderError{StatusCode: 400, Message: "context too long"}, true},
		{"ProviderError 400 other", &ProviderError{StatusCode: 400, Message: "bad format"}, false},
		{"ProviderError 429", &ProviderError{StatusCode: 429, Message: "context too long"}, false},
		{"non-ProviderError", errors.New("context too long"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsContextTooLongErr(tt.err); got != tt.want {
				t.Errorf("IsContextTooLongErr() = %v, want %v", got, tt.want)
			}
		})
	}
}
