package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMemoryBackendPurgeCommandSendsExplicitCheckpointTargets(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var requestBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/memory-backends/default/purge" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"payloadsPurged":3,"purgeDigest":"sha256:purge"}`))
	}))
	defer server.Close()

	root := newRootCmd()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{
		"memory", "backend", "purge", "--server", server.URL,
		"--checkpoint-id", "mcheckpoint-a", "--max-sequence", "42",
		"--before", "2026-08-01T08:00:00Z", "--payloads",
		"--reason", "reclaim retained payload capacity", "--yes",
	})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if requestBody["checkpointId"] != "mcheckpoint-a" || requestBody["maximumOperationSequence"] != float64(42) ||
		requestBody["purgePayloads"] != true || requestBody["reason"] != "reclaim retained payload capacity" {
		t.Fatalf("request body = %#v", requestBody)
	}
}
