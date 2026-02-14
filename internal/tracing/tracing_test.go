/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tracing

import (
	"context"
	"testing"
)

func TestInit(t *testing.T) {
	tests := []struct {
		name    string
		enabled bool
	}{
		{
			name:    "disabled returns noop shutdown",
			enabled: false,
		},
		{
			name:    "enabled creates provider",
			enabled: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			shutdown, err := Init("test-service", tt.enabled)
			if err != nil {
				t.Fatalf("Init() error = %v", err)
			}
			if shutdown == nil {
				t.Fatal("Init() returned nil shutdown")
			}
			// Shutdown should not error
			if err := shutdown(context.Background()); err != nil {
				t.Fatalf("shutdown() error = %v", err)
			}
		})
	}
}

func TestTracer(t *testing.T) {
	tracer := Tracer("test")
	if tracer == nil {
		t.Fatal("Tracer() returned nil")
	}
}
