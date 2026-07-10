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

const (
	inventoryContinueKey    = "continue"
	inventoryItemsKey       = "items"
	inventoryListSubcommand = "list"
)

func TestPagedInventoryListCommandsSurfaceOpaqueContinuation(t *testing.T) {
	const (
		requestedContinuation = "requested-cursor+/=? segment"
		nextContinuation      = "next-cursor+/=? segment"
	)

	for _, tt := range []struct {
		name string
		path string
	}{
		{name: toolResourceName, path: "/api/v1/tools"},
		{name: sessionResourceName, path: "/api/v1/sessions"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tmp := t.TempDir()
			t.Setenv("HOME", tmp)

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
					inventoryItemsKey: []map[string]any{
						{testNameKey: tt.name + "-1"},
					},
					testMetadataKey: map[string]any{inventoryContinueKey: nextContinuation},
				})
			}))
			defer srv.Close()

			var stderr bytes.Buffer
			root := newRootCmd()
			root.SetOut(io.Discard)
			root.SetErr(&stderr)
			root.SetArgs([]string{
				tt.name,
				inventoryListSubcommand,
				testServerFlag,
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
		})
	}
}
