/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tracing

import (
	"context"
	"testing"
	"time"
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
			if tt.enabled {
				t.Setenv("OTEL_EXPORTER_OTLP_TIMEOUT", "100")
			}
			shutdown, err := Init("test-service", tt.enabled)
			if err != nil {
				t.Fatalf("Init() error = %v", err)
			}
			if shutdown == nil {
				t.Fatal("Init() returned nil shutdown")
			}
			// Shutdown should not error, even when no local collector is running.
			shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := shutdown(shutdownCtx); err != nil {
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
