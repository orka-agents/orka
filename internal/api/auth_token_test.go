/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"errors"
	"net/http"
	"testing"
)

func TestExtractAuthTokenFromHeaders(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		authorization string
		apiKey        string
		want          string
		wantErr       error
	}{
		{
			name:    "missing headers",
			wantErr: errMissingAuthToken,
		},
		{
			name:          "bearer token",
			authorization: BearerPrefix + "test-bearer",
			want:          "test-bearer",
		},
		{
			name:   "api key fallback",
			apiKey: "test-api-key",
			want:   "test-api-key",
		},
		{
			name:          "bearer takes precedence",
			authorization: BearerPrefix + "test-bearer",
			apiKey:        "test-api-key",
			want:          "test-bearer",
		},
		{
			name:          "invalid scheme prevents fallback",
			authorization: "Basic invalid",
			apiKey:        "test-api-key",
			wantErr:       errInvalidAuthHeaderFormat,
		},
		{
			name:          "lowercase scheme prevents fallback",
			authorization: "bearer test-bearer",
			apiKey:        "test-api-key",
			wantErr:       errInvalidAuthHeaderFormat,
		},
		{
			name:          "missing bearer separator prevents fallback",
			authorization: "Bearer",
			apiKey:        "test-api-key",
			wantErr:       errInvalidAuthHeaderFormat,
		},
		{
			name:          "empty bearer token uses fallback",
			authorization: BearerPrefix,
			apiKey:        "test-api-key",
			want:          "test-api-key",
		},
		{
			name:          "empty bearer token without fallback",
			authorization: BearerPrefix,
			wantErr:       errMissingAuthToken,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			headers := make(http.Header)
			headers.Set(AuthHeader, tt.authorization)
			headers.Set(XAPIKeyHeader, tt.apiKey)
			got, err := extractAuthTokenFromHeaders(headers.Get)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("authentication error = %v, want %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatal("authentication header selection did not match the expected precedence")
			}
		})
	}
}
