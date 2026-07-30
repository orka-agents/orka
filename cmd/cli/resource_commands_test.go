/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPagedGenericInventoryCommandsSurfaceOpaqueContinuation(t *testing.T) {
	const (
		requestedContinuation = "requested-cursor+/=? segment"
		nextContinuation      = "$(touch should-not-run) $HOME ' opaque"
	)

	for _, tt := range []struct {
		name string
		path string
	}{
		{name: toolResourceName, path: "/api/v1/tools"},
		{name: sessionResourceName, path: "/api/v1/sessions"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != tt.path {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				if got := r.URL.Query().Get("continue"); got != requestedContinuation {
					t.Errorf("continue query = %q, want %q", got, requestedContinuation)
				}
				if got := r.URL.Query().Get("cursor"); got != requestedContinuation {
					t.Errorf("cursor query = %q, want %q", got, requestedContinuation)
				}
				json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
					"items":    []map[string]any{{"name": tt.name + "-1"}},
					"metadata": map[string]any{"continue": nextContinuation},
				})
			}))
			defer srv.Close()

			var stderr bytes.Buffer
			root := newRootCmd()
			root.SetOut(io.Discard)
			root.SetErr(&stderr)
			root.SetArgs([]string{
				tt.name,
				"list",
				"--server",
				srv.URL,
				"--continue",
				requestedContinuation,
			})

			if err := root.Execute(); err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
			if !strings.Contains(stderr.String(), nextContinuation) {
				t.Fatalf("stderr = %q, want continuation %q", stderr.String(), nextContinuation)
			}
			if !strings.Contains(stderr.String(), "--continue") {
				t.Fatalf("stderr = %q, want --continue guidance", stderr.String())
			}
			if !strings.Contains(stderr.String(), "not shell syntax") {
				t.Fatalf("stderr = %q, want explicit non-shell warning", stderr.String())
			}
			if strings.Contains(stderr.String(), "Re-run with --continue") {
				t.Fatalf("stderr = %q, must not present opaque server data as pasteable shell syntax", stderr.String())
			}
		})
	}
}
